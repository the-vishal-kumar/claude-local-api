package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// Runner executes a [Request] and returns its [Result].
//
// It is an interface so that callers (such as the HTTP server) can be
// unit-tested against a fake implementation without invoking the real CLI.
type Runner interface {
	Run(ctx context.Context, req Request) (*Result, error)
}

// ErrEmptyPrompt is returned by [CLIRunner.Run] when a request has no prompt.
var ErrEmptyPrompt = errors.New("claude: prompt must not be empty")

// Options configures a [CLIRunner].
type Options struct {
	// Bin is the name or path of the Claude Code executable.
	Bin string

	// DefaultCWD is used when a request does not set CWD.
	DefaultCWD string

	// DefaultAllowedTools is used when a request does not set AllowedTools.
	DefaultAllowedTools string

	// DefaultPermissionMode is used when a request does not set
	// PermissionMode.
	DefaultPermissionMode string

	// ExtraArgs are appended verbatim to every invocation.
	ExtraArgs []string

	// ForceSubscription removes API-key environment variables from the child
	// process so usage is billed against the Claude subscription.
	ForceSubscription bool
}

// CLIRunner runs tasks by invoking the Claude Code CLI in headless mode.
type CLIRunner struct {
	opts Options
}

// NewCLIRunner returns a CLIRunner configured with opts.
func NewCLIRunner(opts Options) *CLIRunner {
	return &CLIRunner{opts: opts}
}

// Compile-time assertion that *CLIRunner satisfies Runner.
var _ Runner = (*CLIRunner)(nil)

// Run executes req and returns the parsed result.
//
// The supplied context bounds the lifetime of the CLI process: cancelling it
// (for example via a timeout) terminates the task. The CLI emits a JSON result
// on stdout even for some non-zero exits, so Run parses stdout before treating
// a non-zero exit as fatal.
func (r *CLIRunner) Run(ctx context.Context, req Request) (*Result, error) {
	if strings.TrimSpace(req.Prompt) == "" {
		return nil, ErrEmptyPrompt
	}

	cmd := exec.CommandContext(ctx, r.opts.Bin, r.buildArgs(req)...)
	cmd.Dir = r.cwd(req)
	cmd.Env = r.env()

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()

	// Report context errors first so timeouts and cancellations surface
	// clearly regardless of how the process happened to exit.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, fmt.Errorf("claude: task aborted: %w", ctxErr)
	}

	if stdout.Len() > 0 {
		var res Result
		if err := json.Unmarshal(stdout.Bytes(), &res); err == nil {
			return &res, nil
		}
	}

	if runErr != nil {
		return nil, fmt.Errorf("claude: cli failed: %w: %s", runErr, strings.TrimSpace(stderr.String()))
	}
	return nil, fmt.Errorf("claude: could not parse cli output: %s", strings.TrimSpace(stdout.String()))
}

// buildArgs assembles the CLI argument vector for req. It is separated from
// Run so that it can be unit-tested in isolation.
func (r *CLIRunner) buildArgs(req Request) []string {
	tools := req.AllowedTools
	if tools == "" {
		tools = r.opts.DefaultAllowedTools
	}
	mode := req.PermissionMode
	if mode == "" {
		mode = r.opts.DefaultPermissionMode
	}

	args := []string{
		"-p", req.Prompt,
		"--output-format", "json",
	}
	if tools != "" {
		args = append(args, "--allowedTools", tools)
	}
	if mode != "" {
		args = append(args, "--permission-mode", mode)
	}
	if req.SessionID != "" {
		args = append(args, "--resume", req.SessionID)
	}

	// Per-request CLI knobs. Each is gated on a non-empty value so requests
	// that set none of them produce the exact legacy argument vector.
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	if req.Effort != "" {
		args = append(args, "--effort", req.Effort)
	}
	if req.MaxBudgetUSD > 0 {
		args = append(args, "--max-budget-usd", strconv.FormatFloat(req.MaxBudgetUSD, 'f', -1, 64))
	}
	for _, d := range req.AddDirs {
		if d != "" {
			args = append(args, "--add-dir", d)
		}
	}
	if len(req.JSONSchema) > 0 {
		args = append(args, "--json-schema", string(req.JSONSchema))
	}
	if sys := composeSystemPrompt(req); sys != "" {
		args = append(args, "--append-system-prompt", sys)
	}

	return append(args, r.opts.ExtraArgs...)
}

// composeSystemPrompt builds the text appended via --append-system-prompt from
// (1) an input-file manifest, (2) a content-format instruction, and (3) the
// outputs/ deliverable convention. It returns "" when none apply, so legacy
// requests get no --append-system-prompt flag. The text is deterministic.
func composeSystemPrompt(req Request) string {
	var b strings.Builder
	if len(req.StagedInputs) > 0 {
		b.WriteString("The following input files have been placed in your working directory; read them as needed:\n")
		for _, p := range req.StagedInputs {
			b.WriteString("- ")
			b.WriteString(p)
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	if instr := contentFormatInstruction(req.ContentFormat, len(req.JSONSchema) > 0); instr != "" {
		b.WriteString(instr)
		b.WriteString("\n\n")
	}
	if req.Output.ReturnAll || len(req.Output.Globs) > 0 {
		b.WriteString("If you produce any files to return to the caller (images, PDFs, archives, data, etc.), " +
			"write them into the \"outputs/\" subdirectory of your working directory, creating it if needed. " +
			"Write deliverables only there.\n")
	}
	return strings.TrimSpace(b.String())
}

// contentFormatInstruction returns a directive for the requested textual format,
// or "" for an unknown/empty format. When a JSON schema is in play, the json
// format is left to the schema to avoid a conflicting instruction.
func contentFormatInstruction(format string, hasSchema bool) string {
	switch format {
	case "markdown":
		return "Format your final answer as GitHub-Flavored Markdown."
	case "html":
		return "Format your final answer as a single, self-contained, valid HTML document."
	case "csv":
		return "Format your final answer as CSV (comma-separated values) and nothing else."
	case "text":
		return "Format your final answer as plain text with no markup."
	case "json":
		if hasSchema {
			return ""
		}
		return "Your final answer must be a single valid JSON document and nothing else."
	default:
		return ""
	}
}

// cwd returns the working directory for req, falling back to the default.
func (r *CLIRunner) cwd(req Request) string {
	if req.CWD != "" {
		return req.CWD
	}
	return r.opts.DefaultCWD
}

// env returns the environment for the child process. A nil result means the
// child inherits the parent environment unchanged. When ForceSubscription is
// set, metered API-key variables are removed so the task cannot accidentally
// bill a metered API key. The subscription token (CLAUDE_CODE_OAUTH_TOKEN) is
// deliberately preserved so headless subscription auth still works.
func (r *CLIRunner) env() []string {
	if !r.opts.ForceSubscription {
		return nil
	}
	stripped := map[string]bool{
		"ANTHROPIC_API_KEY":    true,
		"ANTHROPIC_AUTH_TOKEN": true,
		"CLAUDE_CODE_API_KEY":  true,
	}
	parent := os.Environ()
	out := make([]string, 0, len(parent))
	for _, kv := range parent {
		key, _, _ := strings.Cut(kv, "=")
		if stripped[key] {
			continue
		}
		out = append(out, kv)
	}
	return out
}
