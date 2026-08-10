package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/lhns/remote-docker/client/internal/config"
	"github.com/lhns/remote-docker/client/internal/proxy"
)

// A session in the background.
//
// `start` spawns `start --foreground`, so the thing running in the background
// is exactly the thing a person would have run in a terminal, with its output
// going to a log. There is no second implementation of the session to keep in
// step with the first, and `start --help` describes both.

// How long to wait for an endpoint to start or stop answering.
//
// Together, and named, because they were three anonymous literals in three
// copies of the same loop -- and two of them waited for the SAME event with
// different patience (10s and 15s) for no stated reason.
//
// startTimeout is the generous one: the first thing a spawned daemon does is
// bind the endpoint, but on a cold start it may also be loading a key and
// reading known_hosts off a slow disk. stopTimeout can be shorter because the
// session has already acknowledged the request before we begin waiting.
const (
	startTimeout = 20 * time.Second
	stopTimeout  = 15 * time.Second
)

// waitForEndpoint blocks until the endpoint is reachable, or stops being, and
// reports whether it got there before the deadline.
func waitForEndpoint(endpoint string, want bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if proxy.Reachable(endpoint) == want {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

func newStartCommand() *cobra.Command {
	var foreground bool

	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start a session for this workspace",
		Long: `Starts a session in the background and returns, so no terminal has to stay
open. Idempotent: if one is already running, this says so and does nothing.

The session serves the local Docker endpoint, exports this directory over the
tunnel, and makes published container ports reachable here.

With --foreground it runs in this terminal instead and holds it until Ctrl-C.
That is what the background one runs, so it is also how to watch what a session
is doing.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := resolve()
			if err != nil {
				return err
			}
			if err := cfg.RequireHost(); err != nil {
				return err
			}
			endpoint := endpointOf(cfg)
			out := cmd.OutOrStdout()

			if foreground {
				return runSession(cmd, cfg)
			}

			if proxy.Reachable(endpoint) {
				_, _ = fmt.Fprintf(out, "already running: %s\n", proxy.DockerHost(endpoint))
				return nil
			}

			if err := startDaemon(cfg, endpoint); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(out, "started: %s\n", proxy.DockerHost(endpoint))
			_, _ = fmt.Fprintf(out, "log: %s\n", daemonLogPath(cfg))
			return nil
		},
	}
	cmd.Flags().BoolVar(&foreground, "foreground", false,
		"run in this terminal instead of the background")
	return cmd
}

func newStopCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the background session for this workspace",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := resolve()
			if err != nil {
				return err
			}
			endpoint := endpointOf(cfg)
			out := cmd.OutOrStdout()

			if !proxy.Reachable(endpoint) {
				_, _ = fmt.Fprintln(out, "not running")
				return nil
			}

			if err := control(endpoint, http.MethodPost, "shutdown", nil); err != nil {
				return fmt.Errorf("stopping the session: %w", err)
			}

			// Confirmed rather than assumed: the daemon acknowledges before it
			// acts, so a successful reply only means it agreed to stop.
			if waitForEndpoint(endpoint, false, stopTimeout) {
				_, _ = fmt.Fprintln(out, "stopped")
				return nil
			}
			return fmt.Errorf("the session acknowledged the stop but is still serving %s",
				proxy.DockerHost(endpoint))
		},
	}
}

// startDaemon spawns a foreground session, detached, and waits for it to answer.
func startDaemon(cfg config.Config, endpoint string) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("finding this binary: %w", err)
	}

	logPath := daemonLogPath(cfg)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return fmt.Errorf("creating the log directory: %w", err)
	}
	// Appended, not truncated: the log of the run that failed is exactly what
	// somebody needs when the next one will not start either.
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("opening %s: %w", logPath, err)
	}
	defer func() { _ = logFile.Close() }()

	// The workspace is passed explicitly rather than inherited from the
	// environment, so the daemon serves the workspace that was asked for even
	// if it is started from a shell whose variables say otherwise.
	// Itself, in the foreground. `start` used to spawn `up`, which was a
	// second command doing the same job; folding them left one code path and
	// one thing to describe.
	args := []string{"start", "--foreground"}
	if cfg.Name != "" {
		args = append(args, "--workspace", cfg.Name)
	}

	cmd := exec.Command(self, args...)
	cmd.Dir = mustWorkDir()
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	detach(cmd)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting the background session: %w", err)
	}
	// Released deliberately: this process is about to exit and must not be the
	// daemon's parent for any longer than it takes to launch it.
	_ = cmd.Process.Release()

	if waitForEndpoint(endpoint, true, startTimeout) {
		return nil
	}
	return fmt.Errorf("the background session did not start within %s; see %s",
		startTimeout, logPath)
}

// daemonLogPath is where a background session's output goes.
func daemonLogPath(cfg config.Config) string {
	name := cfg.ContextName()
	return filepath.Join(config.StateDir(), "logs", name+".log")
}

// control makes a request to the session's own endpoints.
func control(endpoint, method, path string, out any) error {
	client := &http.Client{
		Transport: &http.Transport{DialContext: proxy.DialEndpoint(endpoint)},
		Timeout:   10 * time.Second,
	}
	// The host is ignored -- the transport dials the endpoint -- but a URL
	// needs one.
	req, err := http.NewRequest(method, "http://remote-docker"+proxy.ControlPrefix+path, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		var msg struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(body, &msg) == nil && msg.Message != "" {
			return fmt.Errorf("%s", msg.Message)
		}
		return fmt.Errorf("the session answered %s", resp.Status)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(body, out)
}

// ensureDaemon makes a usable session available, restarting one built from a
// different commit when that costs nothing.
//
// A running daemon serves the endpoint, so a freshly updated client talks to
// the OLD build and appears not to have changed. That is not hypothetical: it
// cost real debugging time during development, once presenting as `stop`
// failing with Docker's own "page not found" because the request was being
// forwarded by a daemon that predated the control channel.
//
// Reports whether anything is usable, never fails: if this cannot sort it out,
// the command below says what went wrong with more context than a guess here.
func ensureDaemon(cfg config.Config, endpoint string) {
	if !proxy.Reachable(endpoint) {
		if cfg.Host != "" {
			_ = startDaemon(cfg, endpoint)
		}
		return
	}

	var st proxy.Status
	if err := control(endpoint, http.MethodGet, "status", &st); err != nil {
		// Something is serving the endpoint but will not answer for itself:
		// a daemon too old to have a control channel, or not ours at all.
		// Left alone either way -- taking over something we cannot identify
		// is worse than the mismatch.
		return
	}

	warnSlowStorage(os.Stderr, st)

	if st.Version == version {
		return
	}

	// Versions differ. Whether that is worth doing anything about depends on
	// what would be lost, and asking THAT costs a round trip to the workspace
	// -- which is why it is asked here and not on every command.
	var idle proxy.Idle
	if err := control(endpoint, http.MethodGet, "idle", &idle); err != nil || !idle.Safe {
		warnVersionMismatch(st)
		return
	}

	if err := restartDaemon(cfg, endpoint); err != nil {
		warnVersionMismatch(st)
	}
}

// warnVersionMismatch reports a difference without claiming an order.
//
// "different", never "outdated": a sha build names a commit and says nothing
// about when, so sha-a7634c0 and sha-95e42ac cannot be sequenced and neither
// can be compared with a release version. Saying which is newer would be
// inventing information.
func warnVersionMismatch(st proxy.Status) {
	fmt.Fprintf(os.Stderr,
		"\nwarning: the running session was built from a different version.\n"+
			"  session: %s (pid %d)\n"+
			"  this:    %s\n"+
			"It is in use, so it was left alone -- restarting drops the file server\n"+
			"and any container holding a directory from it. Run `remote-docker restart`\n"+
			"when nothing needs it, or `remote-docker restart --force` to do it anyway.\n",
		orUnknown(st.Version), st.PID, orUnknown(version))
}

func orUnknown(v string) string {
	if v == "" {
		return "unknown"
	}
	return v
}

// restartDaemon stops a running session and starts one from this binary.
func restartDaemon(cfg config.Config, endpoint string) error {
	if err := control(endpoint, http.MethodPost, "shutdown", nil); err != nil {
		return fmt.Errorf("stopping the running session: %w", err)
	}
	if !waitForEndpoint(endpoint, false, stopTimeout) {
		return fmt.Errorf("the running session did not stop")
	}
	return startDaemon(cfg, endpoint)
}

func newRestartCommand() *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:   "restart",
		Short: "Restart the background session for this workspace",
		Long: `Stops the running session and starts one from this binary.

Refused while anything depends on it: restarting drops the file server, and a
container holding a directory from it loses its filesystem. --force overrides.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := resolve()
			if err != nil {
				return err
			}
			if err := cfg.RequireHost(); err != nil {
				return err
			}
			endpoint := endpointOf(cfg)
			out := cmd.OutOrStdout()

			if !proxy.Reachable(endpoint) {
				if err := startDaemon(cfg, endpoint); err != nil {
					return err
				}
				_, _ = fmt.Fprintf(out, "started: %s\n", proxy.DockerHost(endpoint))
				return nil
			}

			if !force {
				var idle proxy.Idle
				// A session that will not answer cannot be judged, and
				// "cannot tell" is not a reason to break something.
				if err := control(endpoint, http.MethodGet, "idle", &idle); err != nil {
					return fmt.Errorf("cannot tell whether the running session is in use: %w "+
						"(use --force to restart anyway)", err)
				}
				if !idle.Safe {
					return fmt.Errorf("the running session is in use -- a container is running, " +
						"a stream is open, or a shell; restarting would take its file server away. " +
						"Use --force to restart anyway")
				}
			}

			if err := restartDaemon(cfg, endpoint); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(out, "restarted: %s\n", proxy.DockerHost(endpoint))
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "restart even if something depends on the session")
	return cmd
}

// reportLocalSession describes the background session, if there is one.
//
// The version is the reason this exists. A running session serves the
// endpoint, so an updated client talks to the OLD build and behaves like it --
// and until this line existed there was no way to see that from the outside,
// which is exactly how it went unnoticed during development.
func reportLocalSession(out io.Writer, cfg config.Config) {
	endpoint := endpointOf(cfg)
	if !proxy.Reachable(endpoint) {
		row(out, "session", "not running")
		return
	}

	var st proxy.Status
	if err := control(endpoint, http.MethodGet, "status", &st); err != nil {
		row(out, "session", "running, but not answering")
		return
	}

	rowf(out, "session", "running (pid %d, since %s)", st.PID, st.Since)

	// Reported as a difference, never as an ordering. A sha build names a
	// commit and says nothing about when.
	if st.Version == version {
		row(out, "session version", orUnknown(st.Version))
		return
	}
	rowf(out, "session version", "%s  (this binary: %s -- DIFFERENT)",
		orUnknown(st.Version), orUnknown(version))
}

// warnSlowStorage says so when the workspace's daemon is on vfs.
//
// vfs has no copy-on-write: it copies the entire image on every container
// create. Nothing fails -- `docker ps` stays instant, `docker run` takes
// minutes -- so it presents as a hang, and it stays that way until somebody
// changes the workspace's configuration. A real workspace lost a day to it.
//
// Printed here, on the path every `remote-docker docker ...` already takes,
// because that is where somebody is standing when they notice the slowness.
// The agent logs it too, and `status` shows it, but both require already
// suspecting storage -- and reaching the daemon's own host to look is exactly
// what an account may not do.
//
// To stderr, and only when it is true: a correctly configured workspace prints
// nothing, and nothing here ever touches stdout, which belongs to the command.
func warnSlowStorage(w io.Writer, st proxy.Status) {
	if st.Storage != "vfs" {
		return
	}
	_, _ = fmt.Fprintf(w,
		"\nwarning: this workspace's docker daemon is using the vfs storage driver.\n"+
			"It has no copy-on-write, so every `docker create` copies the whole image --\n"+
			"expect containers to take minutes to start. Nothing is broken; it is slow.\n"+
			"Whoever runs the workspace should set --storage-driver in WORKSPACE_DOCKERD_ARGS\n"+
			"(fuse-overlayfs for Ceph- or NFS-backed data) and rebuild the daemon.\n")
}
