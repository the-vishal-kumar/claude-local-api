// Package server exposes the HTTP API that turns JSON (or multipart) requests
// into Claude tasks. It is transport-only: all task execution is delegated to a
// [claude.Runner], and per-request file staging/collection to a [Workspace].
package server

import (
	"archive/zip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"path/filepath"
	"strings"
	"time"

	"github.com/the-vishal-kumar/claude-local-api/src/internal/claude"
	"github.com/the-vishal-kumar/claude-local-api/src/internal/openapi"
)

// Delivery modes for the response.
const (
	deliveryJSON      = "json"
	deliveryMultipart = "multipart"
	deliveryZip       = "zip"
)

// Workspace creates per-request working directories. The server depends on this
// interface (not a concrete type) so it can be faked in tests; the production
// implementation lives in internal/workspace.
type Workspace interface {
	// Create allocates a fresh ephemeral working directory.
	Create() (Run, error)
	// Adopt wraps an existing caller-supplied directory in place.
	Adopt(dir string) (Run, error)
}

// Run is one per-request working directory: stage inputs, collect outputs, and
// clean up.
type Run interface {
	Dir() string
	Stage(inputs []claude.InputFile) (staged []string, err error)
	Collect(spec claude.OutputSpec) ([]claude.Artifact, error)
	Cleanup()
}

// Options configures a [Server].
type Options struct {
	// Runner executes tasks. Required.
	Runner claude.Runner

	// Workspace stages input files and collects outputs. When nil, requests
	// that use file inputs/outputs or non-json delivery return 501.
	Workspace Workspace

	// Logger receives structured logs. Defaults to [slog.Default] when nil.
	Logger *slog.Logger

	// TaskTimeout bounds how long a single task may run.
	TaskTimeout time.Duration

	// MaxBodyBytes caps the size of a JSON request body.
	MaxBodyBytes int64

	// MaxUploadBytes caps the size of an entire multipart request body.
	MaxUploadBytes int64

	// EphemeralWorkspace, when true, lets file requests without an explicit cwd
	// run in a fresh auto-managed workspace. When false, such requests must
	// supply a cwd.
	EphemeralWorkspace bool
}

// Server holds the dependencies needed to serve the API.
type Server struct {
	runner             claude.Runner
	workspace          Workspace
	logger             *slog.Logger
	taskTimeout        time.Duration
	maxBodyBytes       int64
	maxUploadBytes     int64
	ephemeralWorkspace bool
	openAPIJSON        []byte // cached OpenAPI document served at /openapi.json
}

// New returns a Server built from opts.
func New(opts Options) *Server {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{
		runner:             opts.Runner,
		workspace:          opts.Workspace,
		logger:             logger,
		taskTimeout:        opts.TaskTimeout,
		maxBodyBytes:       opts.MaxBodyBytes,
		maxUploadBytes:     opts.MaxUploadBytes,
		ephemeralWorkspace: opts.EphemeralWorkspace,
	}
	if doc, err := json.MarshalIndent(s.openAPIDoc(), "", "  "); err == nil {
		s.openAPIJSON = doc
	} else {
		logger.Error("failed to build OpenAPI document", "error", err)
	}
	return s
}

// Handler builds the HTTP handler with all routes and middleware applied.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("POST /v1/run", s.handleRun)
	mux.HandleFunc("GET /openapi.json", s.handleOpenAPI)
	mux.HandleFunc("GET /docs", s.handleDocs)
	return s.recoverPanic(s.logRequests(mux))
}

// handleRun executes a single task and returns its result, negotiating input
// (JSON or multipart) and output (json/multipart/zip) encodings.
func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	var (
		req          claude.Request
		hasFileParts bool
		err          error
	)
	switch mediaType(r.Header.Get("Content-Type")) {
	case "", "application/json":
		req, err = s.parseJSONBody(w, r)
	case "multipart/form-data":
		req, hasFileParts, err = s.parseMultipartBody(w, r)
	default:
		s.writeError(w, http.StatusUnsupportedMediaType, "unsupported Content-Type")
		return
	}
	if err != nil {
		s.writeParseError(w, err)
		return
	}

	if strings.TrimSpace(req.Prompt) == "" {
		s.writeError(w, http.StatusBadRequest, "field 'prompt' is required")
		return
	}
	if verr := validateRequest(req); verr != nil {
		s.writeError(w, http.StatusBadRequest, verr.Error())
		return
	}

	delivery, derr := resolveDelivery(req.Delivery, negotiateFromAccept(r.Header.Get("Accept")))
	if derr != nil {
		s.writeError(w, http.StatusBadRequest, derr.Error())
		return
	}

	wantsWorkspace := len(req.Inputs) > 0 || hasFileParts ||
		req.Output.ReturnAll || len(req.Output.Globs) > 0 || delivery != deliveryJSON
	if wantsWorkspace && s.workspace == nil {
		s.writeError(w, http.StatusNotImplemented, "file I/O features are not enabled on this server")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.taskTimeout)
	defer cancel()

	if !wantsWorkspace {
		// Legacy / text-only path: no workspace, json delivery only.
		res, runErr := s.runner.Run(ctx, req)
		if runErr != nil {
			s.writeRunError(w, runErr)
			return
		}
		s.writeExecution(w, delivery, claude.Execution{
			Result:     res,
			Structured: s.extractStructured(req, res),
		})
		return
	}

	// File path: allocate a workspace, stage inputs, run, collect outputs.
	var run Run
	switch {
	case req.CWD != "":
		run, err = s.workspace.Adopt(req.CWD)
	case s.ephemeralWorkspace:
		run, err = s.workspace.Create()
	default:
		s.writeError(w, http.StatusBadRequest, "cwd is required when ephemeral workspaces are disabled")
		return
	}
	if err != nil {
		s.logger.Error("workspace allocation failed", "error", err)
		s.writeError(w, http.StatusInternalServerError, "workspace error")
		return
	}
	defer run.Cleanup()

	staged, err := run.Stage(req.Inputs)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "staging inputs: "+err.Error())
		return
	}
	req.StagedInputs = staged
	req.Inputs = nil
	req.CWD = run.Dir()
	// Default to returning everything under outputs/, and make that visible to
	// buildArgs so the agent is told the outputs/ convention.
	if !req.Output.ReturnAll && len(req.Output.Globs) == 0 {
		req.Output.ReturnAll = true
	}

	res, runErr := s.runner.Run(ctx, req)
	if runErr != nil {
		s.writeRunError(w, runErr)
		return
	}
	artifacts, cerr := run.Collect(req.Output)
	if cerr != nil {
		s.logger.Error("collecting outputs failed", "error", cerr)
		s.writeError(w, http.StatusInternalServerError, "collecting outputs: "+cerr.Error())
		return
	}
	s.writeExecution(w, delivery, claude.Execution{
		Result:     res,
		Structured: s.extractStructured(req, res),
		Artifacts:  artifacts,
	})
}

// handleHealth reports that the service is running.
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleOpenAPI serves the generated OpenAPI document.
func (s *Server) handleOpenAPI(w http.ResponseWriter, _ *http.Request) {
	if s.openAPIJSON == nil {
		s.writeError(w, http.StatusInternalServerError, "OpenAPI document unavailable")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(s.openAPIJSON)
}

// handleDocs serves an interactive Swagger UI page backed by /openapi.json.
func (s *Server) handleDocs(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, swaggerUIHTML)
}

// swaggerUIHTML renders Swagger UI, loading its assets from a CDN (so nothing
// is vendored) and the spec from /openapi.json. Viewing /docs therefore needs
// internet access for the CDN assets.
const swaggerUIHTML = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>claude-local-api — API docs</title>
  <link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5/swagger-ui.css">
</head>
<body>
  <div id="swagger-ui"></div>
  <script src="https://unpkg.com/swagger-ui-dist@5/swagger-ui-bundle.js" crossorigin></script>
  <script>
    window.onload = function () {
      window.ui = SwaggerUIBundle({ url: '/openapi.json', dom_id: '#swagger-ui', deepLinking: true });
    };
  </script>
</body>
</html>
`

// openAPIDoc builds the OpenAPI 3.0 document. Component schemas are generated by
// reflection over the request/response types (so they track the code); the two
// paths are declared here since net/http routes carry no introspectable metadata.
func (s *Server) openAPIDoc() map[string]any {
	schemas := openapi.Schemas(claude.Request{}, runResponse{}, errorResponse{})
	return map[string]any{
		"openapi": "3.0.3",
		"info": map[string]any{
			"title":   "claude-local-api",
			"version": "0.2.0",
			"description": "Local HTTP gateway that drives the Claude Code CLI over your Claude " +
				"subscription. POST a task — optionally with input files and a requested output " +
				"format — and get text, schema-validated structured JSON, and/or generated files back.",
		},
		"paths":      s.openAPIPaths(),
		"components": map[string]any{"schemas": schemas},
	}
}

// schemaRef is an OpenAPI $ref to a named component schema.
func schemaRef(name string) map[string]any {
	return map[string]any{"$ref": "#/components/schemas/" + name}
}

// openAPIPaths declares the two routes. Kept small and stable; the verbose,
// change-prone schemas live in components (generated).
func (s *Server) openAPIPaths() map[string]any {
	errResp := map[string]any{
		"description": "Error",
		"content":     map[string]any{"application/json": map[string]any{"schema": schemaRef("errorResponse")}},
	}
	binary := map[string]any{"type": "string", "format": "binary"}
	return map[string]any{
		"/healthz": map[string]any{
			"get": map[string]any{
				"summary": "Liveness check",
				"responses": map[string]any{
					"200": map[string]any{
						"description": "Service is up",
						"content": map[string]any{"application/json": map[string]any{
							"schema": map[string]any{
								"type":       "object",
								"properties": map[string]any{"status": map[string]any{"type": "string"}},
							},
						}},
					},
				},
			},
		},
		"/v1/run": map[string]any{
			"post": map[string]any{
				"summary": "Execute a single Claude task",
				"description": "Send a prompt (optionally with files, a requested format, and per-request " +
					"knobs). Choose the response encoding via the `delivery` field or the Accept header.",
				"requestBody": map[string]any{
					"required": true,
					"content": map[string]any{
						"application/json": map[string]any{"schema": schemaRef("Request")},
						"multipart/form-data": map[string]any{"schema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"request": map[string]any{"type": "string", "description": "The JSON request object (same fields as the application/json body)."},
								"file":    map[string]any{"type": "array", "items": binary, "description": "One or more input files, staged under in/<filename>."},
							},
							"required": []any{"request"},
						}},
					},
				},
				"responses": map[string]any{
					"200": map[string]any{
						"description": "Task result. Shape depends on the negotiated delivery.",
						"content": map[string]any{
							"application/json": map[string]any{"schema": schemaRef("runResponse")},
							"multipart/mixed":  map[string]any{"schema": binary},
							"application/zip":  map[string]any{"schema": binary},
						},
					},
					"400": errResp,
					"405": errResp,
					"413": errResp,
					"415": errResp,
					"501": errResp,
					"502": errResp,
					"504": errResp,
				},
			},
		},
	}
}

// --- request parsing ---

// parseJSONBody decodes the JSON body into a Request, rejecting unknown fields.
func (s *Server) parseJSONBody(w http.ResponseWriter, r *http.Request) (claude.Request, error) {
	r.Body = http.MaxBytesReader(w, r.Body, s.maxBodyBytes)
	var req claude.Request
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return req, err
	}
	return req, nil
}

// parseMultipartBody reads a multipart/form-data body: a `request` part holding
// the JSON request, plus zero or more file parts merged into req.Inputs.
func (s *Server) parseMultipartBody(w http.ResponseWriter, r *http.Request) (claude.Request, bool, error) {
	r.Body = http.MaxBytesReader(w, r.Body, s.maxUploadBytes)
	mr, err := r.MultipartReader()
	if err != nil {
		return claude.Request{}, false, err
	}

	var req claude.Request
	var fileInputs []claude.InputFile
	usedNames := map[string]bool{}
	gotRequest := false
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return req, len(fileInputs) > 0, err
		}
		if part.FormName() == "request" {
			dec := json.NewDecoder(part)
			dec.DisallowUnknownFields()
			if err := dec.Decode(&req); err != nil {
				return req, len(fileInputs) > 0, fmt.Errorf("request part: %w", err)
			}
			gotRequest = true
			continue
		}
		fn := part.FileName()
		if fn == "" {
			continue // ignore stray non-file form fields
		}
		data, err := io.ReadAll(part)
		if err != nil {
			return req, len(fileInputs) > 0, err
		}
		fileInputs = append(fileInputs, claude.InputFile{
			// Strip any client directory and de-duplicate colliding basenames
			// so two uploads named "a.txt" don't silently overwrite each other.
			Path:          uniqueName("in/"+filepath.Base(fn), usedNames),
			ContentBase64: base64.StdEncoding.EncodeToString(data),
		})
	}
	if !gotRequest {
		return req, len(fileInputs) > 0, errors.New("multipart body missing required 'request' part")
	}
	req.Inputs = append(req.Inputs, fileInputs...)
	return req, len(fileInputs) > 0, nil
}

// validValues for per-request enums.
var (
	validEffort        = map[string]bool{"": true, "low": true, "medium": true, "high": true, "xhigh": true, "max": true}
	validContentFormat = map[string]bool{"": true, "markdown": true, "html": true, "csv": true, "text": true, "json": true}
)

// validateRequest checks the optional per-request fields.
func validateRequest(req claude.Request) error {
	if !validEffort[req.Effort] {
		return fmt.Errorf("invalid effort %q (want low|medium|high|xhigh|max)", req.Effort)
	}
	if !validContentFormat[req.ContentFormat] {
		return fmt.Errorf("invalid content_format %q (want markdown|html|csv|text|json)", req.ContentFormat)
	}
	if len(req.JSONSchema) > 0 {
		// Must be a JSON object, not null/string/number/array, so we never
		// forward a meaningless --json-schema value to the CLI.
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(req.JSONSchema, &obj); err != nil || obj == nil {
			// nil obj means the value was JSON `null` (decodes without error).
			return errors.New("json_schema must be a JSON object")
		}
	}
	return nil
}

// uniqueName returns p if unused, otherwise inserts a numeric suffix before the
// extension ("in/a.txt" -> "in/a-1.txt") until it finds an unused name. It
// records the chosen name in used.
func uniqueName(p string, used map[string]bool) string {
	if !used[p] {
		used[p] = true
		return p
	}
	ext := filepath.Ext(p)
	base := strings.TrimSuffix(p, ext)
	for i := 1; ; i++ {
		cand := fmt.Sprintf("%s-%d%s", base, i, ext)
		if !used[cand] {
			used[cand] = true
			return cand
		}
	}
}

// mediaType extracts the bare media type from a Content-Type header.
func mediaType(h string) string {
	if h == "" {
		return ""
	}
	mt, _, err := mime.ParseMediaType(h)
	if err != nil {
		return h
	}
	return mt
}

// resolveDelivery picks the delivery mode: explicit field wins, then the Accept
// header, then json. An unknown explicit value is an error.
func resolveDelivery(field, fromAccept string) (string, error) {
	switch field {
	case "":
		if fromAccept != "" {
			return fromAccept, nil
		}
		return deliveryJSON, nil
	case deliveryJSON, deliveryMultipart, deliveryZip:
		return field, nil
	default:
		return "", fmt.Errorf("unknown delivery %q (want json, multipart, or zip)", field)
	}
}

// negotiateFromAccept maps an Accept header to a delivery mode, or "" when none
// of the file-bearing types are requested (caller defaults to json).
func negotiateFromAccept(accept string) string {
	a := strings.ToLower(accept)
	switch {
	case strings.Contains(a, "application/zip"):
		return deliveryZip
	case strings.Contains(a, "multipart/mixed"):
		return deliveryMultipart
	default:
		return ""
	}
}

// extractStructured returns the parsed structured output when a JSON schema was
// supplied and the result is valid JSON; otherwise nil (logging a warning when
// a schema was requested but the result was not JSON).
func (s *Server) extractStructured(req claude.Request, res *claude.Result) json.RawMessage {
	if len(req.JSONSchema) == 0 || res == nil {
		return nil
	}
	trimmed := strings.TrimSpace(res.Result)
	// A bare "null" (or empty) answer is treated as "no structured output" so
	// the field is omitted rather than serialized as `"structured": null`.
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	if json.Valid([]byte(trimmed)) {
		return json.RawMessage(trimmed)
	}
	s.logger.Warn("json_schema requested but result is not valid JSON")
	return nil
}

// --- response writing ---

// runResponse is the JSON envelope. It embeds the CLI Result so the legacy
// top-level fields stay at the top level (backward compatibility), adding the
// structured output and artifacts alongside.
type runResponse struct {
	*claude.Result
	Structured json.RawMessage   `json:"structured,omitempty"`
	Artifacts  []claude.Artifact `json:"artifacts,omitempty"`
}

func envelopeFrom(e claude.Execution) runResponse {
	return runResponse{Result: e.Result, Structured: e.Structured, Artifacts: e.Artifacts}
}

// writeExecution dispatches on delivery mode.
func (s *Server) writeExecution(w http.ResponseWriter, delivery string, e claude.Execution) {
	switch delivery {
	case deliveryMultipart:
		s.writeMultipart(w, http.StatusOK, e)
	case deliveryZip:
		s.writeZip(w, http.StatusOK, e)
	default:
		s.writeJSON(w, http.StatusOK, envelopeFrom(e))
	}
}

// writeMultipart streams the envelope as the first part and each artifact as a
// raw file part.
func (s *Server) writeMultipart(w http.ResponseWriter, status int, e claude.Execution) {
	mw := multipart.NewWriter(w)
	w.Header().Set("Content-Type", "multipart/mixed; boundary="+mw.Boundary())
	w.WriteHeader(status)

	hdr := textproto.MIMEHeader{}
	hdr.Set("Content-Type", "application/json; charset=utf-8")
	hdr.Set("Content-Disposition", `form-data; name="result"`)
	if part, err := mw.CreatePart(hdr); err == nil {
		enc := json.NewEncoder(part)
		enc.SetIndent("", "  ")
		_ = enc.Encode(envelopeFrom(stripArtifactBytes(e)))
	}
	for _, a := range e.Artifacts {
		h := textproto.MIMEHeader{}
		h.Set("Content-Type", a.MediaType)
		h.Set("Content-Disposition", fmt.Sprintf("attachment; name=%q; filename=%q", "artifact", a.Path))
		if part, err := mw.CreatePart(h); err == nil {
			_, _ = part.Write(a.Bytes)
		}
	}
	if err := mw.Close(); err != nil {
		s.logger.Error("multipart close failed", "error", err)
	}
}

// writeZip streams result.json plus every artifact at its relative path.
func (s *Server) writeZip(w http.ResponseWriter, status int, e claude.Execution) {
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="result.zip"`)
	w.WriteHeader(status)

	zw := zip.NewWriter(w)
	if f, err := zw.Create("result.json"); err == nil {
		enc := json.NewEncoder(f)
		enc.SetIndent("", "  ")
		_ = enc.Encode(envelopeFrom(stripArtifactBytes(e)))
	}
	for _, a := range e.Artifacts {
		if f, err := zw.Create(a.Path); err == nil {
			_, _ = f.Write(a.Bytes)
		}
	}
	if err := zw.Close(); err != nil {
		s.logger.Error("zip close failed", "error", err)
	}
}

// stripArtifactBytes returns a copy of e whose artifacts carry metadata only
// (no inline bytes), for the JSON parts of multipart/zip responses.
func stripArtifactBytes(e claude.Execution) claude.Execution {
	if len(e.Artifacts) == 0 {
		return e
	}
	cp := make([]claude.Artifact, len(e.Artifacts))
	for i, a := range e.Artifacts {
		a.Bytes = nil
		cp[i] = a
	}
	e.Artifacts = cp
	return e
}

// --- error helpers ---

// writeRunError maps a runner error to the appropriate status.
func (s *Server) writeRunError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		s.writeError(w, http.StatusGatewayTimeout, "task timed out")
	case errors.Is(err, claude.ErrEmptyPrompt):
		s.writeError(w, http.StatusBadRequest, err.Error())
	default:
		s.logger.Error("task failed", "error", err)
		s.writeError(w, http.StatusBadGateway, "task failed: "+err.Error())
	}
}

// writeParseError maps a body-parse error to 413 (too large) or 400.
func (s *Server) writeParseError(w http.ResponseWriter, err error) {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		s.writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	s.writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
}

// errorResponse is the body returned for any non-2xx outcome.
type errorResponse struct {
	Error string `json:"error"`
}

// writeError writes a JSON error response with the given status.
func (s *Server) writeError(w http.ResponseWriter, status int, msg string) {
	s.writeJSON(w, status, errorResponse{Error: msg})
}

// writeJSON writes v as an indented JSON response with the given status.
func (s *Server) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		s.logger.Error("failed to encode response", "error", err)
	}
}
