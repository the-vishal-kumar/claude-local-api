package workspace

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/the-vishal-kumar/claude-local-api/src/internal/claude"
)

func testManager(t *testing.T) *Manager {
	t.Helper()
	return NewManager(Config{
		Root:           t.TempDir(),
		MaxInputFile:   1 << 20,
		MaxInputTotal:  4 << 20,
		MaxInputFiles:  8,
		MaxOutputFile:  1 << 20,
		MaxOutputTotal: 4 << 20,
		MaxOutputFiles: 8,
		TTL:            time.Hour,
	})
}

func TestSafeJoinRejectsUnsafe(t *testing.T) {
	base := t.TempDir()
	bad := []string{
		"", "   ", "..", "../x", "../../etc/passwd", "a/../../b",
		"/etc/passwd", "\\\\server\\share", "..\\..\\x", "C:\\x",
		"a\x00b", "sub/../../escape",
	}
	for _, name := range bad {
		if _, err := safeJoin(base, name); err == nil {
			t.Errorf("safeJoin(%q) = nil error, want rejection", name)
		}
	}
	good := []string{"a.png", "in/data.csv", "sub/dir/file.txt", "./ok.txt"}
	for _, name := range good {
		abs, err := safeJoin(base, name)
		if err != nil {
			t.Errorf("safeJoin(%q) unexpected error: %v", name, err)
			continue
		}
		if !strings.HasPrefix(abs, base+string(os.PathSeparator)) {
			t.Errorf("safeJoin(%q) = %q, escaped base %q", name, abs, base)
		}
	}
}

func TestStageWritesInputs(t *testing.T) {
	m := testManager(t)
	w, err := m.Create()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Cleanup()

	staged, err := w.Stage([]claude.InputFile{
		{Path: "in/data.csv", ContentBase64: base64.StdEncoding.EncodeToString([]byte("a,b\n1,2\n"))},
	})
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if len(staged) != 1 || staged[0] != "in/data.csv" {
		t.Fatalf("staged = %v", staged)
	}
	got, err := os.ReadFile(filepath.Join(w.Dir(), "in", "data.csv"))
	if err != nil || string(got) != "a,b\n1,2\n" {
		t.Fatalf("staged file content = %q, err %v", got, err)
	}
}

func TestStageRejectsTraversal(t *testing.T) {
	m := testManager(t)
	w, _ := m.Create()
	defer w.Cleanup()
	_, err := w.Stage([]claude.InputFile{
		{Path: "../escape.txt", ContentBase64: base64.StdEncoding.EncodeToString([]byte("x"))},
	})
	if err != ErrUnsafePath {
		t.Fatalf("Stage traversal err = %v, want ErrUnsafePath", err)
	}
}

func TestStageLimits(t *testing.T) {
	m := NewManager(Config{
		Root: t.TempDir(), MaxInputFile: 4, MaxInputTotal: 100, MaxInputFiles: 2,
		MaxOutputFile: 1 << 20, MaxOutputTotal: 1 << 20, MaxOutputFiles: 8,
	})
	w, _ := m.Create()
	defer w.Cleanup()

	big := base64.StdEncoding.EncodeToString([]byte("toolong"))
	if _, err := w.Stage([]claude.InputFile{{Path: "a", ContentBase64: big}}); err != ErrInputTooLarge {
		t.Fatalf("err = %v, want ErrInputTooLarge", err)
	}
	three := []claude.InputFile{{Path: "a"}, {Path: "b"}, {Path: "c"}}
	if _, err := w.Stage(three); err != ErrTooManyInputs {
		t.Fatalf("err = %v, want ErrTooManyInputs", err)
	}
}

func TestCollectOutputs(t *testing.T) {
	m := testManager(t)
	w, _ := m.Create()
	defer w.Cleanup()

	// A real PNG-ish file and a text file under outputs/.
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 16)...)
	if err := os.WriteFile(filepath.Join(w.outputDir, "chart.png"), png, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.outputDir, "notes.txt"), []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A symlink that must be skipped (points outside).
	_ = os.Symlink("/etc/passwd", filepath.Join(w.outputDir, "evil"))

	arts, err := w.Collect(claude.OutputSpec{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(arts) != 2 {
		t.Fatalf("got %d artifacts, want 2 (symlink skipped): %+v", len(arts), arts)
	}
	// Sorted by path: outputs/chart.png before outputs/notes.txt.
	if arts[0].Path != "outputs/chart.png" {
		t.Errorf("path[0] = %q", arts[0].Path)
	}
	if arts[0].MediaType != "image/png" {
		t.Errorf("media type = %q, want image/png", arts[0].MediaType)
	}
	if string(arts[1].Bytes) != "hi" {
		t.Errorf("notes bytes = %q", arts[1].Bytes)
	}
}

func TestCollectGlobSymlinkEscapeBlocked(t *testing.T) {
	m := testManager(t)
	w, _ := m.Create()
	defer w.Cleanup()

	// Simulate a malicious agent: a directory full of host secrets, reachable
	// only through a symlinked intermediate directory created inside the cwd.
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("EXFILTRATED"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(w.Dir(), "exfil")); err != nil {
		t.Fatal(err)
	}

	// A glob that traverses the symlinked intermediate dir must NOT leak it.
	arts, err := w.Collect(claude.OutputSpec{Globs: []string{"exfil/*.txt"}})
	if err != nil && err != ErrUnsafePath {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, a := range arts {
		if string(a.Bytes) == "EXFILTRATED" {
			t.Fatalf("sandbox escape: leaked outside file via glob (path=%q)", a.Path)
		}
	}
	if len(arts) != 0 {
		t.Fatalf("expected 0 artifacts from escaping glob, got %d: %+v", len(arts), arts)
	}

	// A "../" pattern is rejected outright.
	if _, err := w.Collect(claude.OutputSpec{Globs: []string{"../*"}}); err == nil {
		t.Fatal("expected error for '..' glob pattern")
	}
}

func TestCollectCountCap(t *testing.T) {
	m := NewManager(Config{
		Root: t.TempDir(), MaxInputFile: 1 << 20, MaxInputTotal: 1 << 20, MaxInputFiles: 8,
		MaxOutputFile: 1 << 20, MaxOutputTotal: 1 << 20, MaxOutputFiles: 1,
	})
	w, _ := m.Create()
	defer w.Cleanup()
	os.WriteFile(filepath.Join(w.outputDir, "a.txt"), []byte("a"), 0o600)
	os.WriteFile(filepath.Join(w.outputDir, "b.txt"), []byte("b"), 0o600)
	if _, err := w.Collect(claude.OutputSpec{}); err != ErrTooManyOutputs {
		t.Fatalf("err = %v, want ErrTooManyOutputs", err)
	}
}

func TestCleanup(t *testing.T) {
	m := testManager(t)
	w, _ := m.Create()
	dir := w.Dir()
	w.Cleanup()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("ephemeral workspace not removed: %v", err)
	}

	// Adopted dirs are never removed.
	keep := t.TempDir()
	a, err := m.Adopt(keep)
	if err != nil {
		t.Fatal(err)
	}
	a.Cleanup()
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("adopted dir was removed: %v", err)
	}
}

func TestCleanupKeep(t *testing.T) {
	m := NewManager(Config{
		Root: t.TempDir(), Keep: true, MaxInputFile: 1, MaxInputTotal: 1, MaxInputFiles: 1,
		MaxOutputFile: 1, MaxOutputTotal: 1, MaxOutputFiles: 1,
	})
	w, _ := m.Create()
	dir := w.Dir()
	w.Cleanup()
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("KeepWorkspace dir was removed: %v", err)
	}
}
