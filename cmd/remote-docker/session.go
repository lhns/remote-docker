package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/lhns/remote-docker/internal/client/config"
	"github.com/lhns/remote-docker/internal/client/fswatch"
	"github.com/lhns/remote-docker/internal/client/session"
)

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

	// This IS the file server. Failing to bind means another session is
	// running for this account, which is worth reporting rather than
	// half-working.
	files := session.FilesRequired

	// Parsed here rather than in config, which is the lowest layer and depends
	// on nothing above it. A bad value is reported now, before anything
	// connects, rather than being silently treated as off.
	watch, err := fswatch.ParseMode(cfg.Watch)
	if err != nil {
		return err
	}

	s, err := session.Open(ctx, session.Options{
		Config:      cfg,
		WorkDir:     mustWorkDir(),
		Endpoint:    endpointOf(cfg),
		Files:       files,
		IdleTimeout: cfg.IdleTimeout,
		// The only command that binds the endpoint. Everything else either
		// talks to whoever is serving it, or does not need it.
		Serve:   true,
		Version: version,
		// A session exists to be held open and to say what it is doing; every
		// other command has output of its own to protect.
		Progress:     true,
		Watch:        watch,
		WatchBudget:  cfg.WatchBudget,
		WatchExclude: cfg.WatchExclude,
		Log:          logger{},
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

	// Three ways out: the terminal, `remote-docker stop`, and having nothing
	// left to do. A background session has no terminal to press Ctrl-C in, so
	// without the other two there would be no way to end one short of finding
	// its pid.
	idle := cfg.DaemonIdle
	if idle == 0 {
		idle = config.DefaultDaemonIdle
	}
	select {
	case <-ctx.Done():
	case <-s.Stopped():
	case <-idleExpired(ctx, s, idle):
		_, _ = fmt.Fprintf(out, "\nnothing has needed this session for %s", idle)
	}
	_, _ = fmt.Fprintln(out, "\nclosing session")
	return nil
}
