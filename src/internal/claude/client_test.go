package claude

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestBuildArgs(t *testing.T) {
	r := NewCLIRunner(Options{
		DefaultAllowedTools:   "Read,Edit,Bash",
		DefaultPermissionMode: "acceptEdits",
		ExtraArgs:             []string{"--max-turns", "30"},
	})

	tests := []struct {
		name string
		req  Request
		want []string
	}{
		{
			name: "defaults applied",
			req:  Request{Prompt: "hello"},
			want: []string{
				"-p", "hello",
				"--output-format", "json",
				"--allowedTools", "Read,Edit,Bash",
				"--permission-mode", "acceptEdits",
				"--max-turns", "30",
			},
		},
		{
			name: "request overrides and resume",
			req: Request{
				Prompt:         "fix bug",
				AllowedTools:   "Read",
				PermissionMode: "plan",
				SessionID:      "abc123",
			},
			want: []string{
				"-p", "fix bug",
				"--output-format", "json",
				"--allowedTools", "Read",
				"--permission-mode", "plan",
				"--resume", "abc123",
				"--max-turns", "30",
			},
		},
		{
			name: "per-request knobs and schema",
			req: Request{
				Prompt:       "x",
				Model:        "opus",
				Effort:       "high",
				MaxBudgetUSD: 1.5,
				AddDirs:      []string{"d1", "d2"},
				JSONSchema:   json.RawMessage(`{"type":"object"}`),
			},
			want: []string{
				"-p", "x",
				"--output-format", "json",
				"--allowedTools", "Read,Edit,Bash",
				"--permission-mode", "acceptEdits",
				"--model", "opus",
				"--effort", "high",
				"--max-budget-usd", "1.5",
				"--add-dir", "d1",
				"--add-dir", "d2",
				"--json-schema", `{"type":"object"}`,
				"--max-turns", "30",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := r.buildArgs(tt.req); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("buildArgs()\n got = %v\nwant = %v", got, tt.want)
			}
		})
	}
}

func TestEnvForcesSubscription(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "x")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "y")
	t.Setenv("CLAUDE_CODE_API_KEY", "z")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "keep-me")

	has := func(env []string, key string) bool {
		for _, kv := range env {
			if strings.HasPrefix(kv, key+"=") {
				return true
			}
		}
		return false
	}

	env := NewCLIRunner(Options{ForceSubscription: true}).env()
	for _, k := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_API_KEY"} {
		if has(env, k) {
			t.Errorf("%s should be stripped when ForceSubscription=true", k)
		}
	}
	if !has(env, "CLAUDE_CODE_OAUTH_TOKEN") {
		t.Error("CLAUDE_CODE_OAUTH_TOKEN (subscription token) must be preserved")
	}

	// ForceSubscription=false inherits the parent environment unchanged.
	if NewCLIRunner(Options{ForceSubscription: false}).env() != nil {
		t.Error("ForceSubscription=false should return nil (inherit all)")
	}
}

func TestRunRejectsEmptyPrompt(t *testing.T) {
	r := NewCLIRunner(Options{})
	if _, err := r.Run(context.Background(), Request{Prompt: "   "}); err != ErrEmptyPrompt {
		t.Fatalf("Run() error = %v, want ErrEmptyPrompt", err)
	}
}

func TestComposeSystemPrompt(t *testing.T) {
	// Empty request -> no appended system prompt (keeps legacy buildArgs clean).
	if got := composeSystemPrompt(Request{Prompt: "hi"}); got != "" {
		t.Errorf("composeSystemPrompt(empty) = %q, want \"\"", got)
	}

	full := composeSystemPrompt(Request{
		StagedInputs:  []string{"in/a.png", "in/b.csv"},
		ContentFormat: "markdown",
		Output:        OutputSpec{ReturnAll: true},
	})
	for _, want := range []string{"- in/a.png", "- in/b.csv", "Markdown", "outputs/"} {
		if !strings.Contains(full, want) {
			t.Errorf("composeSystemPrompt missing %q in:\n%s", want, full)
		}
	}

	// json format hint is suppressed when a schema is supplied.
	if instr := contentFormatInstruction("json", true); instr != "" {
		t.Errorf("contentFormatInstruction(json, hasSchema) = %q, want \"\"", instr)
	}
	if instr := contentFormatInstruction("json", false); instr == "" {
		t.Error("contentFormatInstruction(json, no schema) should be non-empty")
	}
}
