// Package config loads and validates the service's runtime configuration.
//
// All configuration comes from environment variables and is read exactly once
// at startup via [Load]. Defaults are deliberately conservative so the service
// is safe to run without any configuration at all.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Default configuration values.
const (
	defaultAddr           = ":8787"
	defaultBin            = "claude"
	defaultCWD            = "."
	defaultAllowedTools   = "Read,Edit,Bash"
	defaultPermissionMode = "acceptEdits"
	defaultTaskTimeout    = 5 * time.Minute
	defaultMaxBodyBytes   = 1 << 20 // 1 MiB

	// Workspace / file-I/O defaults (used by the multimodal request path).
	defaultEphemeralWorkspace  = true
	defaultKeepWorkspace       = false
	defaultWorkspaceTTL        = time.Hour
	defaultMaxUploadBytes      = 25 << 20  // 25 MiB, per input file
	defaultMaxUploadTotalBytes = 100 << 20 // 100 MiB, whole multipart body
	defaultMaxInputFiles       = 32
	defaultMaxOutputBytes      = 50 << 20  // 50 MiB, per output file
	defaultMaxOutputTotalBytes = 200 << 20 // 200 MiB, all outputs
	defaultMaxOutputFiles      = 64
)

// validPermissionModes mirrors the permission modes accepted by the Claude
// Code CLI. See https://code.claude.com/docs/en/permission-modes.
var validPermissionModes = map[string]bool{
	"default":           true,
	"auto":              true,
	"acceptEdits":       true,
	"plan":              true,
	"dontAsk":           true,
	"bypassPermissions": true,
}

// Config holds every runtime setting for the service.
type Config struct {
	// Addr is the TCP address the HTTP server listens on (e.g. ":8787").
	Addr string

	// Bin is the name or path of the Claude Code executable.
	Bin string

	// DefaultCWD is the working directory a task runs in when the request
	// does not specify one.
	DefaultCWD string

	// AllowedTools is the default comma-separated tool allow-list passed to
	// the CLI via --allowedTools.
	AllowedTools string

	// PermissionMode is the default CLI permission mode passed via
	// --permission-mode.
	PermissionMode string

	// ExtraArgs are additional CLI flags appended verbatim to every
	// invocation. They are whitespace-separated, e.g. "--max-turns 30".
	ExtraArgs []string

	// TaskTimeout bounds how long a single task may run.
	TaskTimeout time.Duration

	// MaxBodyBytes caps the size of an incoming JSON request body.
	MaxBodyBytes int64

	// ForceSubscription strips ANTHROPIC_API_KEY and ANTHROPIC_AUTH_TOKEN
	// from the CLI's environment so usage is billed against the logged-in
	// Claude subscription rather than a metered API key.
	ForceSubscription bool

	// JSONLogs selects structured JSON logs over human-readable text logs.
	JSONLogs bool

	// --- Workspace / file-I/O (multimodal request path) ---

	// WorkspaceRoot is the directory under which per-request ephemeral working
	// directories are created.
	WorkspaceRoot string

	// EphemeralWorkspace, when true, makes requests that use file inputs/outputs
	// run in a fresh per-request directory (auto-created and auto-deleted) unless
	// the request supplies an explicit cwd.
	EphemeralWorkspace bool

	// KeepWorkspace, when true, prevents cleanup of ephemeral workspaces (for
	// debugging). It takes precedence over EphemeralWorkspace's auto-delete.
	KeepWorkspace bool

	// WorkspaceTTL bounds how long orphaned run directories may linger; they are
	// swept once at startup. Zero disables the sweep.
	WorkspaceTTL time.Duration

	// MaxUploadBytes caps the size of a single uploaded input file.
	MaxUploadBytes int64

	// MaxUploadTotalBytes caps the size of an entire multipart request body.
	MaxUploadTotalBytes int64

	// MaxInputFiles caps how many input files one request may stage.
	MaxInputFiles int

	// MaxOutputBytes caps the size of a single returned output file.
	MaxOutputBytes int64

	// MaxOutputTotalBytes caps the aggregate size of all returned output files.
	MaxOutputTotalBytes int64

	// MaxOutputFiles caps how many output files one request may return.
	MaxOutputFiles int
}

// Load reads configuration from the environment, applies defaults, and
// validates the result. It returns an error describing the first problem it
// encounters.
func Load() (*Config, error) {
	cfg := &Config{
		Addr:               env("CLAUDE_API_ADDR", defaultAddr),
		Bin:                env("CLAUDE_BIN", defaultBin),
		DefaultCWD:         env("CLAUDE_DEFAULT_CWD", defaultCWD),
		AllowedTools:       env("CLAUDE_ALLOWED_TOOLS", defaultAllowedTools),
		PermissionMode:     env("CLAUDE_PERMISSION_MODE", defaultPermissionMode),
		ExtraArgs:          strings.Fields(os.Getenv("CLAUDE_EXTRA_ARGS")),
		ForceSubscription:  envBool("CLAUDE_FORCE_SUBSCRIPTION", true),
		JSONLogs:           envBool("CLAUDE_JSON_LOGS", false),
		WorkspaceRoot:      env("CLAUDE_WORKSPACE_ROOT", defaultWorkspaceRoot()),
		EphemeralWorkspace: envBool("CLAUDE_WORKSPACE_EPHEMERAL", defaultEphemeralWorkspace),
		KeepWorkspace:      envBool("CLAUDE_KEEP_WORKSPACE", defaultKeepWorkspace),
	}

	var err error
	if cfg.TaskTimeout, err = durationEnv("CLAUDE_TASK_TIMEOUT", defaultTaskTimeout); err != nil {
		return nil, err
	}
	if cfg.WorkspaceTTL, err = durationEnv("CLAUDE_WORKSPACE_TTL", defaultWorkspaceTTL); err != nil {
		return nil, err
	}
	if cfg.MaxBodyBytes, err = int64Env("CLAUDE_MAX_BODY_BYTES", defaultMaxBodyBytes); err != nil {
		return nil, err
	}
	if cfg.MaxUploadBytes, err = int64Env("CLAUDE_MAX_UPLOAD_BYTES", defaultMaxUploadBytes); err != nil {
		return nil, err
	}
	if cfg.MaxUploadTotalBytes, err = int64Env("CLAUDE_MAX_UPLOAD_TOTAL_BYTES", defaultMaxUploadTotalBytes); err != nil {
		return nil, err
	}
	if cfg.MaxInputFiles, err = intEnv("CLAUDE_MAX_INPUT_FILES", defaultMaxInputFiles); err != nil {
		return nil, err
	}
	if cfg.MaxOutputBytes, err = int64Env("CLAUDE_MAX_OUTPUT_BYTES", defaultMaxOutputBytes); err != nil {
		return nil, err
	}
	if cfg.MaxOutputTotalBytes, err = int64Env("CLAUDE_MAX_OUTPUT_TOTAL_BYTES", defaultMaxOutputTotalBytes); err != nil {
		return nil, err
	}
	if cfg.MaxOutputFiles, err = intEnv("CLAUDE_MAX_OUTPUT_FILES", defaultMaxOutputFiles); err != nil {
		return nil, err
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// validate checks that the configuration is internally consistent.
func (c *Config) validate() error {
	switch {
	case c.Addr == "":
		return fmt.Errorf("addr must not be empty")
	case c.Bin == "":
		return fmt.Errorf("bin must not be empty")
	case !validPermissionModes[c.PermissionMode]:
		return fmt.Errorf("invalid permission mode %q", c.PermissionMode)
	case c.TaskTimeout <= 0:
		return fmt.Errorf("task timeout must be positive")
	case c.MaxBodyBytes <= 0:
		return fmt.Errorf("max body bytes must be positive")
	case c.WorkspaceRoot == "":
		return fmt.Errorf("workspace root must not be empty")
	case c.WorkspaceTTL < 0:
		return fmt.Errorf("workspace TTL must not be negative")
	case c.MaxUploadBytes <= 0:
		return fmt.Errorf("max upload bytes must be positive")
	case c.MaxUploadTotalBytes < c.MaxUploadBytes:
		return fmt.Errorf("max upload total bytes must be >= max upload bytes")
	case c.MaxInputFiles <= 0:
		return fmt.Errorf("max input files must be positive")
	case c.MaxOutputBytes <= 0:
		return fmt.Errorf("max output bytes must be positive")
	case c.MaxOutputTotalBytes < c.MaxOutputBytes:
		return fmt.Errorf("max output total bytes must be >= max output bytes")
	case c.MaxOutputFiles <= 0:
		return fmt.Errorf("max output files must be positive")
	}
	return nil
}

// defaultWorkspaceRoot returns a per-OS temp location for ephemeral run dirs.
// The Docker image overrides this via CLAUDE_WORKSPACE_ROOT (e.g. /runs).
func defaultWorkspaceRoot() string {
	return filepath.Join(os.TempDir(), "claude-local-api-runs")
}

// env returns the value of environment variable key, or def when unset/empty.
func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envBool parses a boolean environment variable, returning def when the
// variable is unset or cannot be parsed.
func envBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

// int64Env parses an int64 env var, returning def when unset and an error
// (keyed by name) when present but malformed.
func int64Env(key string, def int64) (int64, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}

// intEnv parses an int env var, returning def when unset and an error when
// present but malformed.
func intEnv(key string, def int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}

// durationEnv parses a Go duration env var, returning def when unset and an
// error when present but malformed.
func durationEnv(key string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return d, nil
}
