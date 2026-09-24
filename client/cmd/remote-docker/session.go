package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/lhns/remote-docker/client/internal/config"
	"github.com/lhns/remote-docker/client/internal/session"
	"github.com/lhns/remote-docker/core-client/fswatch"
	"github.com/lhns/remote-docker/core/logx"
	"github.com/lhns/remote-docker/core/workspace"
)

// modeOf parses this workspace's mount mode settings, so a bad value is
// refused before anything connects. Parsed here because config is the lowest
// layer; the same holds for the watch mode.
func modeOf(cfg config.Config) (workspace.Mode, map[string]workspace.Mode, error) {
	def, err := workspace.ParseMode(cfg.Consistency)
	if err != nil {
		return workspace.ModeUnset, nil, fmt.Errorf("consistency: %w", err)
	}
	if len(cfg.ConsistencyPaths) == 0 {
		return def, nil, nil
	}

	paths := make(map[string]workspace.Mode, len(cfg.ConsistencyPaths))
	for path, value := range cfg.ConsistencyPaths {
		parsed, err := workspace.ParseMode(value)
		if err != nil {
			return workspace.ModeUnset, nil, fmt.Errorf("consistencyPaths[%s]: %w", path, err)
		}
		paths[path] = parsed
	}
	return def, paths, nil
}

// runSession holds a session open until something ends it. It is also what a
// background session runs.
func runSession(cmd *cobra.Command, cfg config.Config) error {
	ctx, cancel := signalContext()
	defer cancel()

	watch, err := fswatch.ParseMode(cfg.Watch)
	if err != nil {
		return err
	}
	mode, modePaths, err := modeOf(cfg)
	if err != nil {
		return err
	}

	s, err := session.Open(ctx, session.Options{
		Config:      cfg,
		WorkDir:     mustWorkDir(),
		Endpoint:    endpointOf(cfg),
		IdleTimeout: cfg.IdleTimeout,
		// The only role that binds the endpoint and the export port.
		Role:         session.Host,
		Version:      version,
		PosixSource:  msysFrom(os.Getenv).posixSource,
		Mode:         mode,
		ModePaths:    modePaths,
		Watch:        watch,
		WatchBudget:  cfg.WatchBudget,
		WatchExclude: cfg.WatchExclude,
		Log:          logger(),
	})
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()

	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintf(out, "Docker endpoint ready. In another terminal:\n\n")
	_, _ = fmt.Fprintf(out, "    %s\n\n", exportLine(s.Endpoint))
	_, _ = fmt.Fprintln(out, "Then use docker normally. Ctrl-C here closes the session.")
	if watch != fswatch.ModeOff {
		_, _ = fmt.Fprintf(out,
			"\nWatching this directory so file watchers in containers see your edits (%s).\n", watch)
	}

	// Standby drops the connection and watches but keeps the endpoint;
	// shutdown, below, also takes the endpoint.
	go standbyWhenIdle(ctx, s, daemonStandby(cfg.DaemonStandby))

	idle := daemonIdle(cfg.DaemonIdle)
	select {
	case <-ctx.Done():
	case <-s.Stopped():
	case <-idleExpired(ctx, s, idle):
		_, _ = fmt.Fprintf(out, "\nnothing has needed this session for %s", idle)
	}
	_, _ = fmt.Fprintln(out, "\nclosing session")
	return nil
}

// logger prints session progress to stderr as "  message", for a person.
func logger() *slog.Logger { return logx.Logger(os.Stderr, "  ", false) }

// withQuerySession opens a session.Query session, which takes neither the
// endpoint nor the reverse-tunnel port, so it works beside a running session.
func withQuerySession(fn func(ctx context.Context, s *session.Session) error) error {
	cfg, err := resolve()
	if err != nil {
		return err
	}
	if err := requireHost(cfg); err != nil {
		return err
	}

	ctx, cancel := signalContext()
	defer cancel()

	s, err := session.Open(ctx, session.Options{
		Config:   cfg,
		WorkDir:  mustWorkDir(),
		Endpoint: endpointOf(cfg),
		Role:     session.Query,
		Log:      logger(),
	})
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()

	return fn(ctx, s)
}

func mustWorkDir() string {
	dir, err := os.Getwd()
	if err != nil {
		return "."
	}
	return dir
}

// signalContext cancels on Ctrl-C so a session is torn down rather than
// leaving its reverse forward bound on the workspace.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}
