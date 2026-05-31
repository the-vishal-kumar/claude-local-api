package server

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/the-vishal-kumar/claude-local-api/src/internal/claude"
)

// fakeRunner is a test double for [claude.Runner].
type fakeRunner struct {
	result *claude.Result
	err    error
}

func (f fakeRunner) Run(context.Context, claude.Request) (*claude.Result, error) {
	return f.result, f.err
}

// newTestServer returns an HTTP handler backed by the given runner.
func newTestServer(r claude.Runner) http.Handler {
	return New(Options{
		Runner:       r,
		TaskTimeout:  time.Second,
		MaxBodyBytes: 1 << 20,
	}).Handler()
}

func do(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func TestRunOK(t *testing.T) {
	h := newTestServer(fakeRunner{result: &claude.Result{Result: "done", SessionID: "s1"}})

	rec := do(h, http.MethodPost, "/v1/run", `{"prompt":"hi"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	var got claude.Result
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.Result != "done" || got.SessionID != "s1" {
		t.Errorf("unexpected result: %+v", got)
	}
}

func TestRunMissingPrompt(t *testing.T) {
	h := newTestServer(fakeRunner{})
	if rec := do(h, http.MethodPost, "/v1/run", `{}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestRunUnknownField(t *testing.T) {
	h := newTestServer(fakeRunner{})
	if rec := do(h, http.MethodPost, "/v1/run", `{"prompt":"hi","nope":true}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for unknown field", rec.Code)
	}
}

func TestRunMethodNotAllowed(t *testing.T) {
	h := newTestServer(fakeRunner{})
	if rec := do(h, http.MethodGet, "/v1/run", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestHealth(t *testing.T) {
	h := newTestServer(fakeRunner{})
	if rec := do(h, http.MethodGet, "/healthz", ""); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestOpenAPISpec(t *testing.T) {
	h := newTestServer(fakeRunner{})
	rec := do(h, http.MethodGet, "/openapi.json", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("content-type = %q", ct)
	}
	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("invalid OpenAPI JSON: %v", err)
	}
	if doc["openapi"] != "3.0.3" {
		t.Errorf("openapi = %v", doc["openapi"])
	}
	paths := doc["paths"].(map[string]any)
	if _, ok := paths["/v1/run"]; !ok {
		t.Error("missing /v1/run path")
	}
	if _, ok := paths["/healthz"]; !ok {
		t.Error("missing /healthz path")
	}
	req := doc["components"].(map[string]any)["schemas"].(map[string]any)["Request"].(map[string]any)
	props := req["properties"].(map[string]any)
	if _, ok := props["prompt"]; !ok {
		t.Error("Request.prompt missing")
	}
	hasPrompt := false
	for _, r := range req["required"].([]any) {
		if r == "prompt" {
			hasPrompt = true
		}
	}
	if !hasPrompt {
		t.Error("prompt should be required")
	}
	if _, ok := props["delivery"].(map[string]any)["enum"]; !ok {
		t.Error("delivery enum missing from spec")
	}
}

func TestDocs(t *testing.T) {
	h := newTestServer(fakeRunner{})
	rec := do(h, http.MethodGet, "/docs", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("content-type = %q", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "swagger-ui") || !strings.Contains(body, "/openapi.json") {
		t.Error("docs page missing swagger-ui or /openapi.json reference")
	}
}

// --- helpers for the multimodal / file-I/O tests ---

// capturingRunner records the request it received so tests can assert the
// server populated workspace fields (CWD, StagedInputs) correctly.
type capturingRunner struct {
	result *claude.Result
	err    error
	got    claude.Request
}

func (c *capturingRunner) Run(_ context.Context, req claude.Request) (*claude.Result, error) {
	c.got = req
	return c.result, c.err
}

// fakeRun is a test double for the per-request Run.
type fakeRun struct {
	dir        string
	staged     []string
	artifacts  []claude.Artifact
	stageErr   error
	collectErr error
	gotInputs  []claude.InputFile
	gotSpec    claude.OutputSpec
	cleaned    bool
}

func (r *fakeRun) Dir() string { return r.dir }
func (r *fakeRun) Stage(inputs []claude.InputFile) ([]string, error) {
	r.gotInputs = inputs
	return r.staged, r.stageErr
}
func (r *fakeRun) Collect(spec claude.OutputSpec) ([]claude.Artifact, error) {
	r.gotSpec = spec
	return r.artifacts, r.collectErr
}
func (r *fakeRun) Cleanup() { r.cleaned = true }

// fakeWorkspace is a test double for the Workspace interface.
type fakeWorkspace struct {
	run       *fakeRun
	createErr error
}

func (f *fakeWorkspace) Create() (Run, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	return f.run, nil
}
func (f *fakeWorkspace) Adopt(dir string) (Run, error) {
	f.run.dir = dir
	return f.run, nil
}

func newTestServerWS(r claude.Runner, ws Workspace) http.Handler {
	return New(Options{
		Runner:             r,
		Workspace:          ws,
		TaskTimeout:        time.Second,
		MaxBodyBytes:       1 << 20,
		MaxUploadBytes:     10 << 20,
		EphemeralWorkspace: true,
	}).Handler()
}

func doReq(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func TestRunStructuredOutput(t *testing.T) {
	h := newTestServer(fakeRunner{result: &claude.Result{Result: `{"k":1}`}})
	rec := do(h, http.MethodPost, "/v1/run", `{"prompt":"x","json_schema":{"type":"object"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", rec.Code, rec.Body)
	}
	var env struct {
		Result     string `json:"result"`
		Structured struct {
			K int `json:"k"`
		} `json:"structured"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Structured.K != 1 {
		t.Fatalf("structured.k = %d, want 1; body = %s", env.Structured.K, rec.Body)
	}
}

func TestRunStructuredOutputNotJSON(t *testing.T) {
	h := newTestServer(fakeRunner{result: &claude.Result{Result: "plain text"}})
	rec := do(h, http.MethodPost, "/v1/run", `{"prompt":"x","json_schema":{"type":"object"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var env map[string]json.RawMessage
	json.Unmarshal(rec.Body.Bytes(), &env)
	if _, ok := env["structured"]; ok {
		t.Errorf("expected no structured field, got body = %s", rec.Body)
	}
}

func TestRunInputsBase64(t *testing.T) {
	run := &fakeRun{dir: "/ws/run-1", staged: []string{"in/a.txt"}}
	cr := &capturingRunner{result: &claude.Result{Result: "ok"}}
	h := newTestServerWS(cr, &fakeWorkspace{run: run})

	body := `{"prompt":"x","inputs":[{"path":"in/a.txt","content_base64":"aGk="}]}`
	rec := do(h, http.MethodPost, "/v1/run", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", rec.Code, rec.Body)
	}
	if cr.got.CWD != "/ws/run-1" {
		t.Errorf("runner CWD = %q, want /ws/run-1", cr.got.CWD)
	}
	if len(cr.got.StagedInputs) != 1 || cr.got.StagedInputs[0] != "in/a.txt" {
		t.Errorf("StagedInputs = %v", cr.got.StagedInputs)
	}
	if len(run.gotInputs) != 1 || run.gotInputs[0].Path != "in/a.txt" {
		t.Errorf("staged inputs = %v", run.gotInputs)
	}
	if !run.cleaned {
		t.Error("workspace was not cleaned up")
	}
}

func buildMultipart(t *testing.T, requestJSON string, files map[string]string) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if requestJSON != "" {
		fw, _ := mw.CreateFormField("request")
		fw.Write([]byte(requestJSON))
	}
	for name, content := range files {
		ff, _ := mw.CreateFormFile("file", name)
		ff.Write([]byte(content))
	}
	mw.Close()
	return &buf, mw.FormDataContentType()
}

func TestRunMultipart(t *testing.T) {
	run := &fakeRun{dir: "/ws/run-1", staged: []string{"in/photo.png"}}
	cr := &capturingRunner{result: &claude.Result{Result: "ok"}}
	h := newTestServerWS(cr, &fakeWorkspace{run: run})

	buf, ct := buildMultipart(t, `{"prompt":"describe it"}`, map[string]string{"photo.png": "PNGDATA"})
	r := httptest.NewRequest(http.MethodPost, "/v1/run", buf)
	r.Header.Set("Content-Type", ct)
	rec := doReq(h, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", rec.Code, rec.Body)
	}
	if len(run.gotInputs) != 1 || run.gotInputs[0].Path != "in/photo.png" {
		t.Fatalf("staged inputs = %+v", run.gotInputs)
	}
	data, _ := base64.StdEncoding.DecodeString(run.gotInputs[0].ContentBase64)
	if string(data) != "PNGDATA" {
		t.Errorf("file content = %q", data)
	}
}

func TestRunMultipartMissingRequest(t *testing.T) {
	run := &fakeRun{dir: "/ws/run-1"}
	h := newTestServerWS(fakeRunner{result: &claude.Result{}}, &fakeWorkspace{run: run})
	buf, ct := buildMultipart(t, "", map[string]string{"a.txt": "hi"})
	r := httptest.NewRequest(http.MethodPost, "/v1/run", buf)
	r.Header.Set("Content-Type", ct)
	if rec := doReq(h, r); rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body)
	}
}

func artifactRun() *fakeRun {
	return &fakeRun{
		dir: "/ws/run-1",
		artifacts: []claude.Artifact{
			{Path: "outputs/a.txt", MediaType: "text/plain", Size: 2, Bytes: []byte("hi")},
		},
	}
}

func TestRunDeliveryJSONArtifacts(t *testing.T) {
	h := newTestServerWS(fakeRunner{result: &claude.Result{Result: "ok"}}, &fakeWorkspace{run: artifactRun()})
	rec := do(h, http.MethodPost, "/v1/run", `{"prompt":"x","output":{"return_all":true}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", rec.Code, rec.Body)
	}
	var env struct {
		Artifacts []claude.Artifact `json:"artifacts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Artifacts) != 1 || string(env.Artifacts[0].Bytes) != "hi" {
		t.Fatalf("artifacts = %+v", env.Artifacts)
	}
}

func TestRunDeliveryZip(t *testing.T) {
	h := newTestServerWS(fakeRunner{result: &claude.Result{Result: "ok"}}, &fakeWorkspace{run: artifactRun()})
	r := httptest.NewRequest(http.MethodPost, "/v1/run", strings.NewReader(`{"prompt":"x","output":{"return_all":true}}`))
	r.Header.Set("Accept", "application/zip")
	rec := doReq(h, r)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/zip" {
		t.Fatalf("status = %d, ct = %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, f := range zr.File {
		names[f.Name] = true
	}
	if !names["result.json"] || !names["outputs/a.txt"] {
		t.Fatalf("zip entries = %v", names)
	}
}

func TestRunDeliveryMultipart(t *testing.T) {
	h := newTestServerWS(fakeRunner{result: &claude.Result{Result: "ok"}}, &fakeWorkspace{run: artifactRun()})
	rec := do(h, http.MethodPost, "/v1/run", `{"prompt":"x","delivery":"multipart","output":{"return_all":true}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", rec.Code, rec.Body)
	}
	mt, params, err := mime.ParseMediaType(rec.Header().Get("Content-Type"))
	if err != nil || mt != "multipart/mixed" {
		t.Fatalf("content-type = %q", rec.Header().Get("Content-Type"))
	}
	mr := multipart.NewReader(rec.Body, params["boundary"])
	parts := 0
	for {
		_, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		parts++
	}
	if parts != 2 { // result envelope + 1 artifact
		t.Fatalf("parts = %d, want 2", parts)
	}
}

func TestRunUnknownDelivery(t *testing.T) {
	h := newTestServer(fakeRunner{})
	if rec := do(h, http.MethodPost, "/v1/run", `{"prompt":"x","delivery":"pigeon"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestRunWorkspaceDisabled(t *testing.T) {
	h := newTestServer(fakeRunner{result: &claude.Result{}})
	body := `{"prompt":"x","inputs":[{"path":"a","content_base64":"aGk="}]}`
	if rec := do(h, http.MethodPost, "/v1/run", body); rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", rec.Code)
	}
}

func TestRunBadContentType(t *testing.T) {
	h := newTestServer(fakeRunner{})
	r := httptest.NewRequest(http.MethodPost, "/v1/run", strings.NewReader(`{}`))
	r.Header.Set("Content-Type", "text/xml")
	if rec := doReq(h, r); rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415", rec.Code)
	}
}

func TestRunUploadTooLarge(t *testing.T) {
	srv := New(Options{
		Runner:             fakeRunner{result: &claude.Result{}},
		Workspace:          &fakeWorkspace{run: &fakeRun{dir: "/ws"}},
		TaskTimeout:        time.Second,
		MaxBodyBytes:       1 << 20,
		MaxUploadBytes:     40, // tiny cap
		EphemeralWorkspace: true,
	}).Handler()

	buf, ct := buildMultipart(t, `{"prompt":"x"}`, map[string]string{"big.bin": strings.Repeat("A", 500)})
	r := httptest.NewRequest(http.MethodPost, "/v1/run", buf)
	r.Header.Set("Content-Type", ct)
	if rec := doReq(srv, r); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body = %s", rec.Code, rec.Body)
	}
}

func TestValidateRequest(t *testing.T) {
	if validateRequest(claude.Request{Effort: "ultra"}) == nil {
		t.Error("bad effort should fail")
	}
	if validateRequest(claude.Request{ContentFormat: "xml"}) == nil {
		t.Error("bad content_format should fail")
	}
	if validateRequest(claude.Request{JSONSchema: json.RawMessage("{")}) == nil {
		t.Error("invalid json_schema should fail")
	}
	for _, bad := range []string{"null", "true", "123", `"x"`, `[1,2]`} {
		if validateRequest(claude.Request{JSONSchema: json.RawMessage(bad)}) == nil {
			t.Errorf("json_schema %q should be rejected (not an object)", bad)
		}
	}
	if err := validateRequest(claude.Request{Effort: "high", ContentFormat: "markdown", JSONSchema: json.RawMessage(`{"a":1}`)}); err != nil {
		t.Errorf("valid request rejected: %v", err)
	}
}

func TestRunMultipartDedupesNames(t *testing.T) {
	run := &fakeRun{dir: "/ws/run-1"}
	cr := &capturingRunner{result: &claude.Result{Result: "ok"}}
	h := newTestServerWS(cr, &fakeWorkspace{run: run})

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormField("request")
	fw.Write([]byte(`{"prompt":"x"}`))
	f1, _ := mw.CreateFormFile("file", "a.txt")
	f1.Write([]byte("one"))
	f2, _ := mw.CreateFormFile("file", "a.txt") // same basename
	f2.Write([]byte("two"))
	mw.Close()

	r := httptest.NewRequest(http.MethodPost, "/v1/run", &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	if rec := doReq(h, r); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if len(run.gotInputs) != 2 {
		t.Fatalf("got %d inputs, want 2", len(run.gotInputs))
	}
	if run.gotInputs[0].Path == run.gotInputs[1].Path {
		t.Fatalf("colliding basenames not de-duplicated: both %q", run.gotInputs[0].Path)
	}
}
