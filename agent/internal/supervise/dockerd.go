// Package supervise keeps dockerd running inside the workspace.
package supervise

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/lhns/remote-docker/core/logx"
)

// Dockerd runs the workspace's Docker daemon and restarts it if it dies.
//
// Supervised rather than started and forgotten: a daemon that crashes leaves
// the container running and looking healthy, still accepting SSH, while every
// Docker call fails.
type Dockerd struct {
	// Args are passed through, typically from WORKSPACE_DOCKERD_ARGS.
	Args []string

	// Socket is where the daemon is expected to appear.
	Socket string

	// StartTimeout bounds how long to wait for the socket before reporting
	// that the daemon did not come up.
	StartTimeout time.Duration

	// Log receives progress. defaults() fills it with logx.Discard(), so it is
	// never nil by the time anything logs; nil is not silence, that is.
	Log *slog.Logger

	mu      sync.Mutex
	current *exec.Cmd
}

const (
	// command is the entrypoint to run. The dind image ships
	// dockerd-entrypoint.sh, which prepends `dockerd` when the first argument
	// starts with a dash and sets up the storage driver and iptables.
	command = "dockerd-entrypoint.sh"

	// restartDelay is how long to wait before restarting a daemon that died.
	restartDelay = 2 * time.Second

	DefaultSocket       = "/var/run/docker.sock"
	DefaultStartTimeout = 90 * time.Second
)

// Run starts the daemon and keeps it running until ctx is done.
//
// It returns only when ctx is cancelled: a daemon that keeps dying is a
// condition to report and retry, not one to give up on, because the container
// would then be restarted by the orchestrator anyway and lose every session
// with it.
func (d *Dockerd) Run(ctx context.Context) error {
	d.applyDefaults()

	for ctx.Err() == nil {
		if err := d.runOnce(ctx); err != nil && ctx.Err() == nil {
			d.Log.Warn("dockerd exited; restarting", "err", err, "in", restartDelay)
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(restartDelay):
		}
	}
	return nil
}

func (d *Dockerd) runOnce(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, command, d.Args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting %s: %w", command, err)
	}

	d.mu.Lock()
	d.current = cmd
	d.mu.Unlock()

	d.Log.Info("started " + command)
	return cmd.Wait()
}

// WaitReady blocks until the daemon's socket appears, or the timeout passes.
//
// Callers should not treat a timeout as fatal. The workspace is still worth
// serving without a daemon: a user can log in and see what went wrong, which
// is more useful than a container that exits and takes the evidence with it.
func (d *Dockerd) WaitReady(ctx context.Context) error {
	d.applyDefaults()

	deadline := time.Now().Add(d.StartTimeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(d.Socket); err == nil {
			d.Log.Info("dockerd is ready")
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return fmt.Errorf("supervise: %s did not appear within %s", d.Socket, d.StartTimeout)
}

// Stop asks the daemon to shut down.
func (d *Dockerd) Stop() error {
	d.mu.Lock()
	cmd := d.current
	d.mu.Unlock()

	if cmd == nil || cmd.Process == nil {
		return nil
	}
	// SIGTERM, not Kill: dockerd stops its containers and flushes its state,
	// and killing it risks leaving the graph driver inconsistent.
	if err := cmd.Process.Signal(os.Interrupt); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}

func (d *Dockerd) applyDefaults() {
	if d.Socket == "" {
		d.Socket = DefaultSocket
	}
	if d.StartTimeout == 0 {
		d.StartTimeout = DefaultStartTimeout
	}
	if d.Log == nil {
		d.Log = logx.Discard()
	}
}
