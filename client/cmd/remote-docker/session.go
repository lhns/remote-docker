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

// consistencyOf parses this workspace's mount consistency settings.
//
// Here rather than in config, for the same reason the watch mode is: config is
// the lowest layer, and a value nobody can serve should be refused by name
// before anything connects.
func consistencyOf(cfg config.Config) (workspace.Consistency, map[string]workspace.Consistency, error) {
	def, err := workspace.ParseConsistency(cfg.Consistency)
	if err != nil {
		return workspace.Unset, nil, fmt.Errorf("consistency: %w", err)
	}
	if len(cfg.ConsistencyPaths) == 0 {
		return def, nil, nil
	}

	paths := make(map[string]workspace.Consistency, len(cfg.ConsistencyPaths))
	for path, value := range cfg.ConsistencyPaths {
		parsed, err := workspace.ParseConsistency(value)
		if err != nil {
			return workspace.Unset, nil, fmt.Errorf("consistencyPaths[%s]: %w", path, err)
		}
		paths[path] = parsed
	}
	return def, paths, nil
}

// runSession holds a session open until something ends it.
//
// This is the whole of what a background session does; `start` spawns
// `start --foreground`, which lands here. Keeping one implementation rather
// than a foreground command and a daemon body means there is nothing to keep
// in step, and watching a session by running it in a terminal shows exactly
// what the background one does.
func runSession(cmd *cobra.Command, cfg config.Config) error {
	ctx, cancel := signalContext()
	defer cancel()

	// Parsed here rather than in config, which is the lowest layer and depends
	// on nothing above it. A bad value is reported now, before anything
	// connects, rather than being silently treated as off.
	watch, err := fswatch.ParseMode(cfg.Watch)
	if err != nil {
		return err
	}
	consistency, consistencyPaths, err := consistencyOf(cfg)
	if err != nil {
		return err
	}

	s, err := session.Open(ctx, session.Options{
		Config:      cfg,
		WorkDir:     mustWorkDir(),
		Endpoint:    endpointOf(cfg),
		IdleTimeout: cfg.IdleTimeout,
		// The only session that hosts: it binds the endpoint, takes the
		// account's one export port, and narrates. Every other command either
		// talks to whoever is serving, or only asks the workspace a question.
		Role:             session.Host,
		Version:          version,
		PosixSource:      msysFrom(os.Getenv).posixSource,
		Consistency:      consistency,
		ConsistencyPaths: consistencyPaths,
		Watch:            watch,
		WatchBudget:      cfg.WatchBudget,
		WatchExclude:     cfg.WatchExclude,
		Log:              logger(),
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

	// Letting go of the workspace is not the same as ending: standby drops the
	// connection and the watches and keeps serving the endpoint, so the Docker
	// clients pointed at it never notice. Shutdown is the tier above and is off
	// unless asked for, because it takes the endpoint with it.
	go standbyWhenIdle(ctx, s, daemonStandby(cfg.DaemonStandby))

	// Three ways out: the terminal, `remote-docker stop`, and having nothing
	// left to do. A background session has no terminal to press Ctrl-C in, so
	// without the other two there would be no way to end one short of finding
	// its pid.
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

// logger prints session progress to stderr, so stdout stays usable for
// anything a command genuinely outputs.
//
// Two spaces and the message, which is what these lines have always looked
// like: they sit under a command's own output and are read by a person, not
// parsed. logx.Handler is what keeps that true through log/slog, whose own
// TextHandler would render them as time=... level=INFO msg="...".
func logger() *slog.Logger { return logx.Logger(os.Stderr, "  ", false) }

// `session.Query` is the load-bearing part, and the reason this is shared
// rather than written twice. A query session takes neither the local endpoint
// nor the account's one reverse-tunnel port (ADR 0003), so it still works while
// a real session holds both, which is precisely when somebody runs `status`
// or `gc`. See session.Role for what each half of that prevents.
func withQuerySession(fn func(ctx context.Context, s *session.Session) error) error {
	cfg, err := resolve()
	if err != nil {
		return err
	}
	if err := cfg.RequireHost(); err != nil {
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
