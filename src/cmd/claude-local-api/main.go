// Command claude-local-api runs a small HTTP service that executes tasks
// against the Claude Code CLI on behalf of local callers.
//
// It is intended for personal, single-user automation. See the README for
// prerequisites (an installed, logged-in Claude Code CLI) and the important
// note about subscription-versus-API-key billing.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/the-vishal-kumar/claude-local-api/src/internal/claude"
	"github.com/the-vishal-kumar/claude-local-api/src/internal/config"
	"github.com/the-vishal-kumar/claude-local-api/src/internal/server"
	"github.com/the-vishal-kumar/claude-local-api/src/internal/workspace"
)

// shutdownTimeout bounds how long graceful shutdown may take.
const shutdownTimeout = 10 * time.Second

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

// run wires the application together and blocks until the server stops,
// returning any startup or shutdown error. Keeping the body in run (rather
// than main) lets it return errors and makes the flow easy to follow.
func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger := newLogger(cfg.JSONLogs)
	slog.SetDefault(logger)

	// Warn early if the CLI is missing. The service still starts so the
	// problem surfaces as a clear per-request error rather than a crash.
	if _, err := exec.LookPath(cfg.Bin); err != nil {
		logger.Warn("Claude CLI not found on PATH; tasks will fail until it is installed and logged in",
			"bin", cfg.Bin)
	}

	runner := claude.NewCLIRunner(claude.Options{
		Bin:                   cfg.Bin,
		DefaultCWD:            cfg.DefaultCWD,
		DefaultAllowedTools:   cfg.AllowedTools,
		DefaultPermissionMode: cfg.PermissionMode,
		ExtraArgs:             cfg.ExtraArgs,
		ForceSubscription:     cfg.ForceSubscription,
	})

	wsMgr := workspace.NewManager(workspace.Config{
		Root:           cfg.WorkspaceRoot,
		Keep:           cfg.KeepWorkspace,
		TTL:            cfg.WorkspaceTTL,
		MaxInputFile:   cfg.MaxUploadBytes,
		MaxInputTotal:  cfg.MaxUploadTotalBytes,
		MaxInputFiles:  cfg.MaxInputFiles,
		MaxOutputFile:  cfg.MaxOutputBytes,
		MaxOutputTotal: cfg.MaxOutputTotalBytes,
		MaxOutputFiles: cfg.MaxOutputFiles,
	})
	if removed, err := wsMgr.SweepStale(); err != nil {
		logger.Warn("workspace sweep failed", "error", err)
	} else if removed > 0 {
		logger.Info("swept stale workspaces", "removed", removed)
	}

	srv := server.New(server.Options{
		Runner:             runner,
		Workspace:          workspaceAdapter{wsMgr},
		Logger:             logger,
		TaskTimeout:        cfg.TaskTimeout,
		MaxBodyBytes:       cfg.MaxBodyBytes,
		MaxUploadBytes:     cfg.MaxUploadTotalBytes,
		EphemeralWorkspace: cfg.EphemeralWorkspace,
	})

	httpServer := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// WriteTimeout is intentionally unset: agent tasks may run for
		// minutes, and per-task deadlines are enforced via context instead.
	}

	// Stop the context on SIGINT/SIGTERM to trigger graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		logger.Info("listening",
			"addr", cfg.Addr,
			"force_subscription", cfg.ForceSubscription,
			"permission_mode", cfg.PermissionMode,
			"task_timeout", cfg.TaskTimeout.String(),
			"workspace_root", cfg.WorkspaceRoot,
			"ephemeral_workspace", cfg.EphemeralWorkspace,
		)
		errCh <- httpServer.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		logger.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	}
}

// workspaceAdapter bridges *workspace.Manager to the server.Workspace
// interface: the manager returns concrete *workspace.Workspace handles, which
// already satisfy server.Run, so the adapter just wraps the return types.
type workspaceAdapter struct{ m *workspace.Manager }

func (a workspaceAdapter) Create() (server.Run, error) {
	w, err := a.m.Create()
	if err != nil {
		return nil, err
	}
	return w, nil
}

func (a workspaceAdapter) Adopt(dir string) (server.Run, error) {
	w, err := a.m.Adopt(dir)
	if err != nil {
		return nil, err
	}
	return w, nil
}

// newLogger returns a slog.Logger writing to stderr in either JSON or text.
func newLogger(jsonLogs bool) *slog.Logger {
	var handler slog.Handler
	if jsonLogs {
		handler = slog.NewJSONHandler(os.Stderr, nil)
	} else {
		handler = slog.NewTextHandler(os.Stderr, nil)
	}
	return slog.New(handler)
}
