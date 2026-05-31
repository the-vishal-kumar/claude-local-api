// Package claude runs tasks against the Claude Code CLI in non-interactive
// ("headless") mode and returns their structured results.
//
// There is no official Go Agent SDK, so this package drives the `claude`
// command-line tool directly. The prompt and all flags are passed as discrete
// argv elements (never via a shell), so there is no command-injection surface.
package claude

import "encoding/json"

// Request describes a single task to execute.
type Request struct {
	// Prompt is the instruction for the agent. It is required.
	Prompt string `json:"prompt"`

	// CWD is the working directory the task runs in. When empty, the
	// runner's configured default directory is used.
	CWD string `json:"cwd,omitempty"`

	// AllowedTools optionally overrides the runner's default tool allow-list
	// (comma-separated, e.g. "Read,Edit,Bash").
	AllowedTools string `json:"allowed_tools,omitempty"`

	// PermissionMode optionally overrides the runner's default permission
	// mode (e.g. "acceptEdits").
	PermissionMode string `json:"permission_mode,omitempty" enum:"default,auto,acceptEdits,plan,dontAsk,bypassPermissions"`

	// SessionID, when set, resumes a previous session via --resume.
	SessionID string `json:"session_id,omitempty"`

	// --- per-request CLI knobs (all optional, passed through to the CLI) ---

	// Model selects the model for this task (--model), e.g. "opus", "sonnet".
	Model string `json:"model,omitempty"`

	// Effort sets the reasoning effort (--effort): low|medium|high|xhigh|max.
	Effort string `json:"effort,omitempty" enum:"low,medium,high,xhigh,max"`

	// MaxBudgetUSD caps spend on API calls for this task (--max-budget-usd).
	MaxBudgetUSD float64 `json:"max_budget_usd,omitempty"`

	// AddDirs grants the agent tool access to additional directories
	// (--add-dir, repeated).
	AddDirs []string `json:"add_dirs,omitempty"`

	// --- structured / formatted output ---

	// JSONSchema, when set, makes the CLI validate the final answer against this
	// JSON Schema (--json-schema). Sent as a raw JSON object, not a string.
	JSONSchema json.RawMessage `json:"json_schema,omitempty"`

	// ContentFormat is a soft hint for the textual answer's format, injected via
	// --append-system-prompt: markdown|html|csv|text|json.
	ContentFormat string `json:"content_format,omitempty" enum:"markdown,html,csv,text,json"`

	// --- file inputs / outputs ---

	// Inputs are files supplied inline (base64) to be staged into the working
	// directory before the task runs. Multipart file parts are merged here too.
	Inputs []InputFile `json:"inputs,omitempty"`

	// Output controls which generated files are collected and returned.
	Output OutputSpec `json:"output,omitempty"`

	// Delivery selects how the response is encoded: json|multipart|zip. Empty
	// means negotiate from the Accept header, defaulting to json.
	Delivery string `json:"delivery,omitempty" enum:"json,multipart,zip"`

	// StagedInputs is populated by the server (never the client) with the
	// relative paths of input files already written into the workspace, so the
	// runner can compose an input manifest for the agent.
	StagedInputs []string `json:"-"`
}

// InputFile is a file supplied inline in the JSON body. The bytes are standard
// base64; the server decodes and stages them into the workspace at Path.
type InputFile struct {
	// Path is the destination path, relative to the workspace root.
	Path string `json:"path"`
	// ContentBase64 is the standard-base64-encoded file content.
	ContentBase64 string `json:"content_base64"`
}

// OutputSpec controls which files the agent generated are returned to the
// caller. When zero-valued, the server defaults to returning everything under
// the workspace's outputs/ directory.
type OutputSpec struct {
	// ReturnAll returns every file under the outputs/ directory.
	ReturnAll bool `json:"return_all,omitempty"`
	// Globs are additional filepath.Match patterns, relative to the workspace
	// root, to collect (e.g. "report.pdf", "charts/*.png").
	Globs []string `json:"globs,omitempty"`
}

// Artifact is one file produced by the agent and returned to the caller. It is
// defined here so the workspace and server packages can share it.
type Artifact struct {
	// Path is the file's path relative to the workspace root (forward slashes),
	// e.g. "outputs/report.pdf".
	Path string `json:"path"`
	// MediaType is the sniffed/looked-up MIME type; never empty.
	MediaType string `json:"media_type"`
	// Size is the byte length.
	Size int64 `json:"size"`
	// Bytes is the raw content. encoding/json base64-encodes it under the
	// content_base64 key; it is omitted (stripped) for multipart/zip delivery.
	Bytes []byte `json:"content_base64,omitempty"`
}

// Execution is the full outcome of a run: the CLI Result, optional structured
// output (when a JSON schema was supplied), and any collected output files.
type Execution struct {
	// Result is the CLI envelope, verbatim.
	Result *Result
	// Structured is the parsed structured output when JSONSchema was used and
	// the result was valid JSON; nil otherwise.
	Structured json.RawMessage
	// Artifacts are the files collected from the workspace; nil when none.
	Artifacts []Artifact
}

// Result is the structured output produced by
// `claude -p --output-format json`. Field tags follow the CLI's JSON schema.
type Result struct {
	// Type is the result envelope type, normally "result".
	Type string `json:"type"`

	// Subtype distinguishes success from error variants
	// (e.g. "success", "error_max_turns").
	Subtype string `json:"subtype"`

	// Result is the agent's final textual answer.
	Result string `json:"result"`

	// SessionID identifies the session. Pass it back via [Request.SessionID]
	// to continue the conversation.
	SessionID string `json:"session_id"`

	// TotalCostUSD is the billed cost of the task in US dollars.
	TotalCostUSD float64 `json:"total_cost_usd"`

	// IsError reports whether the agent terminated abnormally even though the
	// process exited cleanly (for example, after hitting a turn limit).
	IsError bool `json:"is_error"`

	// DurationMS is the wall-clock duration of the task in milliseconds.
	DurationMS int64 `json:"duration_ms"`

	// DurationAPIMS is the time spent in API calls, in milliseconds.
	DurationAPIMS int64 `json:"duration_api_ms"`

	// NumTurns is the number of agent turns taken.
	NumTurns int `json:"num_turns"`
}
