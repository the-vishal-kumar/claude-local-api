// Package workspace provides per-request working directories for agent tasks:
// it stages caller-supplied input files in, lets the agent run there, and
// collects the files the agent generated back out — all behind strict
// path-traversal and resource-limit guards.
//
// It depends only on the standard library (plus the claude package for the
// shared Artifact/InputFile/OutputSpec types), keeping the project free of
// third-party dependencies.
package workspace

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/the-vishal-kumar/claude-local-api/src/internal/claude"
)

// Sentinel errors (mirroring the claude.ErrEmptyPrompt style).
var (
	ErrUnsafePath      = errors.New("workspace: unsafe file name rejected")
	ErrTooManyInputs   = errors.New("workspace: input file count exceeds limit")
	ErrInputTooLarge   = errors.New("workspace: input file exceeds size limit")
	ErrInputsTooLarge  = errors.New("workspace: total input size exceeds limit")
	ErrTooManyOutputs  = errors.New("workspace: output file count exceeds limit")
	ErrOutputTooLarge  = errors.New("workspace: output file exceeds size limit")
	ErrOutputsTooLarge = errors.New("workspace: total output size exceeds limit")
	ErrNotADir         = errors.New("workspace: path is not a directory")
)

const outputsSubdir = "outputs"

// Config controls workspace creation and resource limits. It is supplied once
// at startup from the service config.
type Config struct {
	Root           string        // root under which ephemeral run dirs are created
	Keep           bool          // when true, Cleanup never deletes (debugging)
	TTL            time.Duration // sweep orphan run-* dirs older than this (0 = off)
	MaxInputFile   int64         // per input file, bytes
	MaxInputTotal  int64         // aggregate input bytes per request
	MaxInputFiles  int           // input file count
	MaxOutputFile  int64         // per output file, bytes
	MaxOutputTotal int64         // aggregate output bytes per request
	MaxOutputFiles int           // output file count
}

// Manager owns the shared configuration and is created once at startup. It is
// safe for concurrent use: it holds only immutable config plus the absolute,
// cleaned root path.
type Manager struct {
	cfg     Config
	rootAbs string
}

// NewManager returns a Manager for cfg. It resolves the root to an absolute,
// cleaned path used for cleanup containment checks.
func NewManager(cfg Config) *Manager {
	root := cfg.Root
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	return &Manager{cfg: cfg, rootAbs: filepath.Clean(root)}
}

// Workspace is a single per-request working directory handle. It is NOT safe
// for concurrent use; one Workspace belongs to one in-flight request.
type Workspace struct {
	dir       string // absolute, cleaned path to the run dir (the agent's cwd)
	outputDir string // absolute path to dir/outputs
	ephemeral bool   // true => Cleanup may delete dir; false => in-place, never deleted
	mgr       *Manager
}

// Dir returns the absolute working directory the agent should run in.
func (w *Workspace) Dir() string { return w.dir }

// Create makes a fresh ephemeral workspace under the manager's Root:
//
//	<Root>/run-<random hex>/
//	<Root>/run-<random hex>/outputs/
func (m *Manager) Create() (*Workspace, error) {
	id, err := newID()
	if err != nil {
		return nil, fmt.Errorf("workspace: id: %w", err)
	}
	dir := filepath.Join(m.rootAbs, "run-"+id)
	out := filepath.Join(dir, outputsSubdir)
	if err := os.MkdirAll(out, 0o700); err != nil {
		return nil, fmt.Errorf("workspace: create: %w", err)
	}
	return &Workspace{dir: dir, outputDir: out, ephemeral: true, mgr: m}, nil
}

// Adopt wraps an existing caller-supplied directory as an in-place workspace.
// The directory is never deleted by Cleanup. Its outputs/ subdir is created if
// absent so Collect has a place to look.
func (m *Manager) Adopt(dir string) (*Workspace, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("workspace: adopt: %w", err)
	}
	abs = filepath.Clean(abs)
	fi, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("workspace: adopt: %w", err)
	}
	if !fi.IsDir() {
		return nil, ErrNotADir
	}
	out := filepath.Join(abs, outputsSubdir)
	if err := os.MkdirAll(out, 0o700); err != nil {
		return nil, fmt.Errorf("workspace: adopt outputs: %w", err)
	}
	return &Workspace{dir: abs, outputDir: out, ephemeral: false, mgr: m}, nil
}

// Stage decodes and writes the given inline input files into the workspace,
// enforcing count/size limits and rejecting unsafe paths. It returns the staged
// relative paths (forward-slash) in input order for the agent's manifest.
func (w *Workspace) Stage(inputs []claude.InputFile) ([]string, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	if len(inputs) > w.mgr.cfg.MaxInputFiles {
		return nil, ErrTooManyInputs
	}
	var total int64
	staged := make([]string, 0, len(inputs))
	for _, in := range inputs {
		// Reject by the decoded-size upper bound before allocating/decoding.
		if int64(base64.StdEncoding.DecodedLen(len(in.ContentBase64))) > w.mgr.cfg.MaxInputFile {
			return nil, ErrInputTooLarge
		}
		data, err := base64.StdEncoding.DecodeString(in.ContentBase64)
		if err != nil {
			return nil, fmt.Errorf("workspace: decode %q: %w", in.Path, err)
		}
		if int64(len(data)) > w.mgr.cfg.MaxInputFile {
			return nil, ErrInputTooLarge
		}
		total += int64(len(data))
		if total > w.mgr.cfg.MaxInputTotal {
			return nil, ErrInputsTooLarge
		}
		rel, err := w.writeInput(in.Path, data)
		if err != nil {
			return nil, err
		}
		staged = append(staged, rel)
	}
	return staged, nil
}

// writeInput safely writes one input file under the (sanitized) name and
// returns its workspace-relative path.
func (w *Workspace) writeInput(name string, content []byte) (string, error) {
	abs, err := safeJoin(w.dir, name)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return "", err
	}
	// Refuse to write through an existing symlink at the target.
	if fi, err := os.Lstat(abs); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		return "", ErrUnsafePath
	}
	f, err := os.OpenFile(abs, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return "", err
	}
	if _, err := f.Write(content); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	rel, _ := filepath.Rel(w.dir, abs)
	return filepath.ToSlash(rel), nil
}

// Collect gathers output files. When spec.ReturnAll (or spec is zero-valued and
// has no globs) it walks the outputs/ directory; spec.Globs add extra
// filepath.Match patterns relative to the workspace root. Symlinks and
// non-regular files are skipped; per-file/total/count caps are enforced.
func (w *Workspace) Collect(spec claude.OutputSpec) ([]claude.Artifact, error) {
	walkOutputs := spec.ReturnAll || len(spec.Globs) == 0

	// The fully symlink-resolved root, so containment checks compare resolved
	// path against resolved root (this also handles roots under symlinked
	// prefixes, e.g. macOS /var -> /private/var).
	realRoot, err := filepath.EvalSymlinks(w.dir)
	if err != nil {
		realRoot = filepath.Clean(w.dir)
	}

	c := &collector{w: w, seen: map[string]bool{}, realRoot: realRoot}
	if walkOutputs {
		if err := filepath.WalkDir(w.outputDir, c.visitWalk); err != nil {
			return nil, err
		}
	}
	for _, g := range spec.Globs {
		if g == "" {
			continue
		}
		if hasDotDot(g) { // defense-in-depth: no traversal patterns
			return nil, fmt.Errorf("workspace: glob %q must not contain '..'", g)
		}
		matches, err := filepath.Glob(filepath.Join(w.dir, filepath.FromSlash(g)))
		if err != nil {
			return nil, fmt.Errorf("workspace: bad glob %q: %w", g, err)
		}
		for _, m := range matches {
			if err := c.consider(m); err != nil {
				return nil, err
			}
		}
	}
	sort.Slice(c.arts, func(i, j int) bool { return c.arts[i].Path < c.arts[j].Path })
	return c.arts, nil
}

// collector accumulates artifacts while enforcing limits and dedup.
type collector struct {
	w        *Workspace
	realRoot string // symlink-resolved workspace root for containment checks
	arts     []claude.Artifact
	seen     map[string]bool // keyed on the resolved real path
	total    int64
}

func (c *collector) visitWalk(path string, d fs.DirEntry, err error) error {
	if err != nil {
		return err
	}
	if d.IsDir() {
		return nil
	}
	if d.Type()&fs.ModeSymlink != 0 || !d.Type().IsRegular() {
		return nil // skip symlinks, devices, sockets, pipes
	}
	return c.consider(path)
}

// consider validates a candidate file is contained, regular, within caps, then
// reads and appends it as an artifact. Containment is checked against the
// SYMLINK-RESOLVED path, so a glob that traverses an agent-created symlinked
// directory cannot escape the workspace.
func (c *collector) consider(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return ErrUnsafePath
	}
	abs = filepath.Clean(abs)

	// Lexical relative path — used only for the reported artifact path so it
	// stays workspace-relative and readable.
	rel, err := filepath.Rel(c.w.dir, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return ErrUnsafePath
	}
	relSlash := filepath.ToSlash(rel)

	// Resolve every symlink (including intermediate directories and the final
	// component) and require the result to remain under the resolved root.
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return ErrUnsafePath
	}
	rrel, err := filepath.Rel(c.realRoot, real)
	if err != nil || rrel == ".." || strings.HasPrefix(rrel, ".."+string(os.PathSeparator)) {
		return ErrUnsafePath
	}

	// Dedup on the resolved path so aliases (a file reachable via several
	// lexical paths/symlinks) are collected — and counted against caps — once.
	if c.seen[real] {
		return nil
	}

	fi, err := os.Lstat(real)
	if err != nil {
		return err
	}
	if fi.Mode()&fs.ModeSymlink != 0 || !fi.Mode().IsRegular() {
		return nil
	}
	if fi.Size() > c.w.mgr.cfg.MaxOutputFile {
		return ErrOutputTooLarge
	}
	if len(c.arts)+1 > c.w.mgr.cfg.MaxOutputFiles {
		return ErrTooManyOutputs
	}

	data, err := readCapped(real, c.w.mgr.cfg.MaxOutputFile)
	if err != nil {
		return err
	}
	c.total += int64(len(data))
	if c.total > c.w.mgr.cfg.MaxOutputTotal {
		return ErrOutputsTooLarge
	}

	c.seen[real] = true
	c.arts = append(c.arts, claude.Artifact{
		Path:      relSlash,
		MediaType: detectMedia(real, data),
		Size:      int64(len(data)),
		Bytes:     data,
	})
	return nil
}

// hasDotDot reports whether a slash- or backslash-separated pattern contains a
// ".." path segment.
func hasDotDot(p string) bool {
	for _, seg := range strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '\\' }) {
		if seg == ".." {
			return true
		}
	}
	return false
}

// Cleanup removes the workspace tree when it is ephemeral and Keep is false.
// In-place (adopted) workspaces are never deleted. It refuses to delete any
// path that is not under the manager root with the "run-" prefix, so a bug
// cannot remove an unrelated directory. Safe to call from a defer.
func (w *Workspace) Cleanup() {
	if !w.ephemeral || w.mgr.cfg.Keep {
		return
	}
	if !strings.HasPrefix(w.dir, w.mgr.rootAbs+string(os.PathSeparator)) {
		return
	}
	if !strings.HasPrefix(filepath.Base(w.dir), "run-") {
		return
	}
	_ = os.RemoveAll(w.dir)
}

// SweepStale removes orphaned run-* directories older than the configured TTL.
// It is meant to run once at startup as a safety net for processes killed
// mid-request. Returns the number of directories removed.
func (m *Manager) SweepStale() (int, error) {
	if m.cfg.TTL <= 0 {
		return 0, nil
	}
	entries, err := os.ReadDir(m.rootAbs)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	cutoff := time.Now().Add(-m.cfg.TTL)
	removed := 0
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "run-") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			if os.RemoveAll(filepath.Join(m.rootAbs, e.Name())) == nil {
				removed++
			}
		}
	}
	return removed, nil
}

// --- helpers ---

// newID returns 128 bits of randomness, hex-encoded (filesystem-safe).
func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// safeJoin resolves name against baseAbs, rejecting any path that escapes the
// base via traversal, absolute paths, drive letters, backslashes, NUL bytes, or
// symlinked ancestors. baseAbs must already be absolute and cleaned.
func safeJoin(baseAbs, name string) (string, error) {
	if strings.TrimSpace(name) == "" || strings.ContainsRune(name, 0) {
		return "", ErrUnsafePath
	}
	// Treat backslash as a separator so Windows-style traversal can't slip
	// through on a Linux runtime where '\' is a legal filename byte.
	n := strings.ReplaceAll(name, "\\", "/")
	if strings.HasPrefix(n, "/") || filepath.IsAbs(name) || filepath.IsAbs(n) {
		return "", ErrUnsafePath
	}
	if len(n) >= 2 && n[1] == ':' { // C: style
		return "", ErrUnsafePath
	}
	clean := filepath.Clean(n)
	sep := string(os.PathSeparator)
	if clean == "." || clean == ".." ||
		strings.HasPrefix(clean, ".."+sep) ||
		strings.Contains(clean, sep+".."+sep) ||
		strings.HasSuffix(clean, sep+"..") {
		return "", ErrUnsafePath
	}
	abs := filepath.Join(baseAbs, clean)
	rel, err := filepath.Rel(baseAbs, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+sep) {
		return "", ErrUnsafePath
	}
	if err := assertNoSymlinkAncestors(baseAbs, abs); err != nil {
		return "", err
	}
	return abs, nil
}

// assertNoSymlinkAncestors verifies no existing directory between baseAbs and
// abs's parent is a symlink (which could redirect a write outside the base).
func assertNoSymlinkAncestors(baseAbs, abs string) error {
	rel, err := filepath.Rel(baseAbs, filepath.Dir(abs))
	if err != nil {
		return ErrUnsafePath
	}
	if rel == "." {
		return nil
	}
	cur := baseAbs
	for _, seg := range strings.Split(rel, string(os.PathSeparator)) {
		cur = filepath.Join(cur, seg)
		fi, err := os.Lstat(cur)
		if errors.Is(err, fs.ErrNotExist) {
			return nil // will be created by MkdirAll; nothing to follow
		}
		if err != nil {
			return err
		}
		if fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir() {
			return ErrUnsafePath
		}
	}
	return nil
}

// readCapped reads up to max bytes from path (not following a final symlink),
// returning ErrOutputTooLarge if the file is larger (closing the TOCTOU window
// between stat and read).
func readCapped(path string, max int64) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, ErrOutputTooLarge
	}
	return data, nil
}

// detectMedia returns a MIME type for the file: by extension first, then by
// sniffing the content. It never returns an empty string.
func detectMedia(path string, content []byte) string {
	if ext := filepath.Ext(path); ext != "" {
		if mt := mime.TypeByExtension(ext); mt != "" {
			return mt
		}
	}
	n := len(content)
	if n > 512 {
		n = 512
	}
	return http.DetectContentType(content[:n])
}
