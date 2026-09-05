package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
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
open. If one is already running, this says so and does nothing.

The session serves the local Docker endpoint, exports this directory over the
tunnel, and makes published container ports reachable here.

--foreground runs it in this terminal instead and holds it until Ctrl-C. That
is what the background one runs, so it is also how to watch one work.

The session forwards every request, so set REMOTE_DOCKER_WATCH and
REMOTE_DOCKER_TRACE here rather than on the docker command you run.`,
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

// waitForExit blocks until the process is gone, and reports whether it got
// there before the deadline.
//
// A pid of 0 means nobody told us which process to watch: an older session
// that does not report one, or a status request that failed. Treated as done
// rather than as a failure: the endpoint has already gone quiet, which is what
// this command could check before, and refusing to return would turn a missing
// nicety into a broken `stop`.
func waitForExit(pid int, timeout time.Duration) bool {
	if pid <= 0 {
		return true
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
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
			if err := stopSession(endpoint); err != nil {
				return err
			}
			_, _ = fmt.Fprintln(out, "stopped")
			return nil
		},
	}
}

// stopSession asks the session serving an endpoint to stop, and returns once
// its process has gone.
//
// The PROCESS, not the endpoint. The endpoint going quiet is the START of the
// teardown: Session.Close shuts the listener first, so no request can arrive
// mid-teardown, and only then drops the SSH connection, the reverse tunnel and
// the NFS export. An account has exactly ONE reverse-tunnel port (ADR 0003) and
// a host session fails hard when it cannot take it, so returning at the
// listener lets the next session start against a port the workspace has not
// released, which kills its NFS server and takes the session down with it.
func stopSession(endpoint string) error {
	// Asked for first, because after the shutdown there is nothing left to
	// ask. Advisory: a pid we cannot read only costs the second wait below,
	// never correctness.
	var st proxy.Status
	if err := control(endpoint, http.MethodGet, "status", &st); err != nil {
		st.PID = proxy.Owner(endpoint)
	}

	if err := control(endpoint, http.MethodPost, "shutdown", nil); err != nil {
		return fmt.Errorf("stopping the session: %w", err)
	}

	// Confirmed rather than assumed: the daemon acknowledges before it acts,
	// so a successful reply only means it agreed to stop.
	if !waitForEndpoint(endpoint, false, stopTimeout) {
		return fmt.Errorf("the session acknowledged the stop but is still serving %s",
			proxy.DockerHost(endpoint))
	}
	if !waitForExit(st.PID, stopTimeout) {
		return fmt.Errorf(
			"the session stopped serving %s but process %d is still running, "+
				"so its workspace resources may not be free yet",
			proxy.DockerHost(endpoint), st.PID)
	}
	return nil
}

// startDaemon spawns a foreground session, detached, and waits for it to answer.
func startDaemon(cfg config.Config, endpoint string) error {
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
	// Itself, in the foreground: one command serves a workspace, and the
	// background case is that same command with something else holding it.
	//
	// Under `remote`, because that is where our commands are: the root is the
	// Docker CLI, so a bare "start" reaches nothing.
	args := []string{"remote", "start", "--foreground"}
	if cfg.Name != "" {
		args = append(args, "--workspace", cfg.Name)
	}

	cmd, err := selfCommand(args...)
	if err != nil {
		return err
	}
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

	// Killed, not left behind. Returning the error on its own leaves a child
	// that is merely SLOW rather than dead: it binds the endpoint a moment
	// later and serves a session the user was told had failed to start, with
	// no pid on screen and nothing owning it. Worse, the next `start` then
	// reports "already running" and points at the session its predecessor
	// disowned.
	//
	// By pid, because cmd.Process was released above: this process must not
	// be the daemon's parent, so it cannot wait on it either.
	stopped := "and was stopped"
	if err := killPID(cmd.Process.Pid); err != nil {
		stopped = fmt.Sprintf("and could not be stopped (pid %d: %v)", cmd.Process.Pid, err)
	}
	return fmt.Errorf("the background session did not start within %s %s; see %s",
		startTimeout, stopped, logPath)
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
	// The host is ignored (the transport dials the endpoint) but a URL
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
// A running daemon serves the endpoint, so without the restart a freshly
// updated client talks to the OLD build and appears not to have changed, which
// presents as commands the new build added answering Docker's own "page not
// found" -- forwarded by a daemon that predates them.
//
// The error is what stopped a session existing, and returning it is the whole
// point. The command below cannot say anything better: it reaches an endpoint
// nobody is serving and reports a missing daemon, which is true and explains
// nothing. Something already serving is NOT an error, however old or foreign
// it is, because the command about to run will work.
func ensureDaemon(cfg config.Config, endpoint string) error {
	if !proxy.Reachable(endpoint) {
		// Nothing is serving, so whatever goes wrong from here is the reason
		// the next docker command will fail. Report it: without it the CLI
		// reaches an endpoint nobody is on and blames a missing daemon, which
		// is true and useless -- the daemon is missing because of this.
		if err := cfg.RequireHost(); err != nil {
			return err
		}
		return startDaemon(cfg, endpoint)
	}

	var st proxy.Status
	if err := control(endpoint, http.MethodGet, "status", &st); err != nil {
		// Something is serving the endpoint but will not answer for itself:
		// a daemon too old to have a control channel, or not ours at all.
		// Left alone either way, because taking over something we cannot identify
		// is worse than the mismatch.
		return nil
	}

	warnSlowStorage(os.Stderr, st)
	warnTraceGoesNowhere(os.Stderr, st)

	if !versionDiffers(st) {
		return nil
	}

	// Versions differ. Whether that is worth doing anything about depends on
	// what would be lost, and asking THAT costs a round trip to the workspace,
	// which is why it is asked here and not on every command.
	var idle proxy.Idle
	if err := control(endpoint, http.MethodGet, "idle", &idle); err != nil || !idle.Safe {
		warnVersionMismatch(st)
		return nil
	}

	// A restart that fails is a WARNING, not this function's error: the
	// session that is already serving still works, and the only cost is
	// running a different build than this one.
	if err := restartDaemon(cfg, endpoint); err != nil {
		warnVersionMismatch(st)
	}
	return nil
}

// versionDiffers reports whether a session was built from a different commit
// than this binary.
func versionDiffers(st proxy.Status) bool { return st.Version != version }

// differentBuild names both builds without claiming an order.
//
// "different", never "outdated": a sha build names a commit and says nothing
// about when, so sha-a7634c0 and sha-95e42ac cannot be sequenced and neither
// can be compared with a release version. Saying which is newer would be
// inventing information.
func differentBuild(st proxy.Status) string {
	return fmt.Sprintf("a different build (session %s, this binary %s)",
		orUnknown(st.Version), orUnknown(version))
}

// warnVersionMismatch reports a session left running because it is in use.
func warnVersionMismatch(st proxy.Status) {
	fmt.Fprintf(os.Stderr,
		"\nwarning: the running session (pid %d) is %s, and is in use, so it was left alone.\n"+
			"  fix: `%s` once nothing needs it, or `restart --force` now\n",
		st.PID, differentBuild(st), ourCommand("restart"))
}

func orUnknown(v string) string {
	if v == "" {
		return "unknown"
	}
	return v
}

// restartDaemon stops a running session and starts one from this binary.
func restartDaemon(cfg config.Config, endpoint string) error {
	if err := stopSession(endpoint); err != nil {
		return err
	}
	return startDaemon(cfg, endpoint)
}

func newRestartCommand() *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:   "restart",
		Short: "Restart the background session for this workspace",
		Long: `Stops the running session and starts one from this binary.

Refused while anything depends on it. Restarting drops the file server, and a
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
					return fmt.Errorf("cannot tell whether the running session is in use: %w\n"+
						"  fix: `%s` to restart anyway", err, ourCommand("restart --force"))
				}
				if !idle.Safe {
					return fmt.Errorf("the running session is in use, and restarting takes its "+
						"file server away from whatever is using it\n"+
						"  fix: `%s` to restart anyway", ourCommand("restart --force"))
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
	f := gather(cfg)
	row(out, "session", f.sessionLine())
	if !f.answering {
		return
	}

	if !versionDiffers(f.local) {
		row(out, "session version", orUnknown(f.local.Version))
		return
	}
	row(out, "session version", differentBuild(f.local))
}

// warnTraceGoesNowhere says so when this process is tracing and the process
// that would print the traces is not.
//
// REMOTE_DOCKER_TRACE is read once, at start, by whichever process forwards
// the requests, and that is the background session, not this command. So
// `REMOTE_DOCKER_TRACE=1 remote-docker docker ps` against a running session
// prints nothing and explains nothing, which reads as "tracing does not work"
// rather than "you set it on the wrong process".
//
// Only when the session is genuinely not tracing: somebody who started the
// session with the variable set is already getting what they asked for and
// must not be told otherwise.
func warnTraceGoesNowhere(w io.Writer, st proxy.Status) {
	if !proxy.Tracing() || st.Tracing {
		return
	}
	writeTraceWarning(w, st)
}

// writeTraceWarning is the message, separated from the decision to print it so
// a test can read it without owning the environment this process started with.
func writeTraceWarning(w io.Writer, st proxy.Status) {
	_, _ = fmt.Fprintf(w,
		"\nwarning: %s is set here, but the session forwarding the requests (pid %d) was started without it.\n"+
			"  fix: %s=1 %s\n",
		proxy.TraceEnv, st.PID, proxy.TraceEnv, ourCommand("restart"))
}

// warnSlowStorage says so when the workspace's daemon is on vfs.
//
// vfs has no copy-on-write: it copies the entire image on every container
// create. Nothing fails: `docker ps` stays instant, `docker run` takes
// minutes, so it presents as a hang, and it stays that way until somebody
// changes the workspace's configuration. A real workspace lost a day to it.
//
// Printed here, on the path every `remote-docker docker ...` already takes,
// because that is where somebody is standing when they notice the slowness.
// The agent logs it too, and `status` shows it, but both require already
// suspecting storage, and reaching the daemon's own host to look is exactly
// what an account may not do.
//
// To stderr, and only when it is true: a correctly configured workspace prints
// nothing, and nothing here ever touches stdout, which belongs to the command.
func warnSlowStorage(w io.Writer, st proxy.Status) {
	if st.Storage != "vfs" {
		return
	}
	_, _ = fmt.Fprintf(w, "\nwarning: %s.\n  fix: %s\n", slowStorage, fixSlowStorage)
}

// slowStorage is the one verdict on a daemon using vfs, and fixSlowStorage
// its remedy. `status` prints the verdict; the warning adds the fix.
const (
	slowStorage    = "the workspace daemon is on vfs, so containers start slowly"
	fixSlowStorage = "set WORKSPACE_DOCKERD_ARGS=--storage-driver=fuse-overlayfs, then rebuild the daemon"
)
