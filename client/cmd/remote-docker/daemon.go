package main

import (
	"cmp"
	"encoding/json"
	"errors"
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

// A background session is `remote start --foreground`, spawned detached with
// its output in a log.

// How long to wait for an endpoint to start or stop answering.
const (
	startTimeout = 20 * time.Second
	stopTimeout  = 15 * time.Second
)

// waitForEndpoint blocks until the endpoint is reachable, or stops being, and
// reports whether it got there before the deadline.
func waitForEndpoint(endpoint string, want bool, timeout time.Duration) bool {
	return pollUntil(timeout, func() bool { return proxy.Reachable(endpoint) == want })
}

// pollUntil asks done every 100ms and reports whether it said yes before the
// deadline.
func pollUntil(timeout time.Duration, done func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if done() {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

func newStartCommand() *cobra.Command {
	var foreground bool

	cmd := &cobra.Command{
		Use:   "start [name]",
		Short: "Start the background session for this workspace",
		Long: `Starts a session in the background and returns, so no terminal has to stay
open. If one is already running, this says so and does nothing.

The session serves the local Docker endpoint, exports this directory over the
tunnel, and makes published container ports reachable here.

--foreground runs it in this terminal instead and holds it until Ctrl-C. That
is what the background one runs, so it is also how to watch one work.

The session forwards every request, so set REMOTE_DOCKER_WATCH and
REMOTE_DOCKER_TRACE here rather than on the docker command you run.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolve(args)
			if err != nil {
				return err
			}
			if err := requireHost(cfg); err != nil {
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
// there before the deadline. An unknown pid (0) counts as gone.
func waitForExit(pid int, timeout time.Duration) bool {
	if pid <= 0 {
		return true
	}
	return pollUntil(timeout, func() bool { return !processAlive(pid) })
}

func newStopCommand() *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:   "stop [name]",
		Short: "Stop the background session for this workspace",
		Long: `Stops the running session. If none is running, this says so and does nothing.

Refused while anything depends on it: stopping drops the file server, and a
container holding a directory from it loses its filesystem. -f overrides.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolve(args)
			if err != nil {
				return err
			}
			endpoint := endpointOf(cfg)
			out := cmd.OutOrStdout()

			if !proxy.Reachable(endpoint) {
				_, _ = fmt.Fprintf(out, "not running: %s\n", proxy.DockerHost(endpoint))
				return nil
			}
			if err := refuseInUse(cfg, force, "stopping it takes its file server away", "stop"); err != nil {
				return err
			}
			if err := stopSession(endpoint); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(out, "stopped: %s\n", proxy.DockerHost(endpoint))
			return nil
		},
	}
	forceFlag(cmd, &force, "stop even if something depends on the session")
	return cmd
}

// forceFlag is -f/--force, as docker spells it.
func forceFlag(cmd *cobra.Command, force *bool, usage string) {
	cmd.Flags().BoolVarP(force, "force", "f", false, usage)
}

// refuseInUse refuses to take a workspace's session away while anything
// depends on it, unless forced: docker's rule for removing something in use.
//
// "In use" is the session's own answer, the one its idle release acts on. A
// session that will not answer cannot be judged, and that is not a reason to
// break something either. Nothing serving is nothing in use. consequence ends
// the sentence "the session for X is in use, and ..."; command is ours without
// the name, and the remedy is it again with -f.
func refuseInUse(cfg config.Config, force bool, consequence, command string) error {
	endpoint := endpointOf(cfg)
	if force || !proxy.Reachable(endpoint) {
		return nil
	}
	if cfg.Name != "" {
		command += " " + cfg.Name
	}
	fix := fmt.Sprintf("  fix: `%s` to go ahead anyway", ourCommand(command+" -f"))

	var idle proxy.Idle
	if err := control(endpoint, http.MethodGet, "idle", &idle); err != nil {
		return fmt.Errorf("cannot tell whether %s is in use: %s\n%s", sessionOf(cfg), firstLine(err.Error()), fix)
	}
	if !idle.Safe {
		return fmt.Errorf("%s is in use, and %s\n%s", sessionOf(cfg), consequence, fix)
	}
	return nil
}

// sessionOf names a workspace's session in a message.
func sessionOf(cfg config.Config) string {
	if cfg.Name == "" {
		return "the session"
	}
	return fmt.Sprintf("the session for %q", cfg.Name)
}

// stopSession asks the session to stop and returns once its PROCESS has gone.
// The listener closes first in teardown, so returning when the endpoint goes
// quiet lets the next session start before the workspace has released its
// reverse-tunnel port, and that session then fails.
func stopSession(endpoint string) error {
	// Asked before the shutdown, while something can answer.
	var st proxy.Status
	if err := control(endpoint, http.MethodGet, "status", &st); err != nil {
		st.PID = proxy.Owner(endpoint)
	}

	if err := control(endpoint, http.MethodPost, "shutdown", nil); err != nil {
		return fmt.Errorf("stopping the session: %w", err)
	}

	// The reply comes before the daemon acts.
	if !waitForEndpoint(endpoint, false, stopTimeout) {
		return fmt.Errorf("the session acknowledged the stop but is still serving %s",
			proxy.DockerHost(endpoint))
	}
	if !waitForExit(st.PID, stopTimeout) {
		return &lingeringError{endpoint: endpoint, pid: st.PID}
	}
	return nil
}

// lingeringError is a session that stopped serving but whose process is still
// there. restartDaemon starts anyway, since the endpoint is free.
type lingeringError struct {
	endpoint string
	pid      int
}

func (e *lingeringError) Error() string {
	return fmt.Sprintf("the session stopped serving %s but process %d is still running, "+
		"so its workspace resources may not be free yet", proxy.DockerHost(e.endpoint), e.pid)
}

// startDaemon spawns a foreground session, detached, and waits for it to answer.
func startDaemon(cfg config.Config, endpoint string) error {
	logPath := daemonLogPath(cfg)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return fmt.Errorf("creating the log directory: %w", err)
	}
	// Appended, so the previous failed run's log survives.
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("opening %s: %w", logPath, err)
	}
	defer func() { _ = logFile.Close() }()

	// The workspace is passed explicitly, not inherited from the environment.
	args := []string{"remote", "start", "--foreground"}
	if cfg.Name != "" {
		args = append(args, "--workspace", cfg.Name)
	}

	cmd, err := selfCommand(args...)
	if err != nil {
		return err
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	detach(cmd)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting the background session: %w", err)
	}
	_ = cmd.Process.Release()

	if waitForEndpoint(endpoint, true, startTimeout) {
		return nil
	}

	// Killed, or a slow child binds later and serves a session reported as
	// failed, which the next `start` then calls "already running".
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
	// The host is ignored: the transport dials the endpoint.
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

// ensureDaemon makes a session available, restarting one built from a
// different commit when it is idle. Otherwise an updated client talks to the
// old build, and new endpoints answer "page not found". Something already
// serving is never an error, however old or foreign.
func ensureDaemon(cfg config.Config, endpoint string) error {
	if !proxy.Reachable(endpoint) {
		if err := requireHost(cfg); err != nil {
			return err
		}
		return startDaemon(cfg, endpoint)
	}

	var st proxy.Status
	if err := control(endpoint, http.MethodGet, "status", &st); err != nil {
		// Too old for a control channel, or not ours: left alone.
		return nil
	}

	warnSlowStorage(os.Stderr, st)
	warnTraceGoesNowhere(os.Stderr, st)

	if !versionDiffers(st) {
		return nil
	}

	// Asked only on a mismatch: it costs a round trip to the workspace.
	var idle proxy.Idle
	if err := control(endpoint, http.MethodGet, "idle", &idle); err != nil || !idle.Safe {
		warnVersionMismatch(st)
		return nil
	}

	// A failed restart is a warning: the serving session still works.
	if err := restartDaemon(cfg, endpoint); err != nil {
		warnVersionMismatch(st)
	}
	return nil
}

// versionDiffers reports whether a session was built from a different commit
// than this binary.
func versionDiffers(st proxy.Status) bool { return st.Version != version }

// differentBuild names both builds without claiming an order: sha builds
// cannot be sequenced, so never "outdated".
func differentBuild(st proxy.Status) string {
	return fmt.Sprintf("a different build (session %s, this binary %s)",
		orUnknown(st.Version), orUnknown(version))
}

// warnVersionMismatch reports a session left running because it is in use.
func warnVersionMismatch(st proxy.Status) {
	fmt.Fprintf(os.Stderr,
		"\nwarning: the running session (pid %d) is %s, and is in use, so it was left alone.\n"+
			"  fix: `%s` once nothing needs it, or `%s` now\n",
		st.PID, differentBuild(st), ourCommand("restart"), ourCommand("restart -f"))
}

func orUnknown(v string) string { return cmp.Or(v, "unknown") }

// restartDaemon stops a running session and starts one from this binary. A
// lingering process only warns; any other stop failure aborts.
func restartDaemon(cfg config.Config, endpoint string) error {
	if err := stopSession(endpoint); err != nil {
		if _, ok := errors.AsType[*lingeringError](err); !ok {
			return err
		}
		fmt.Fprintf(os.Stderr, "warning: %v\n", err)
	}
	return startDaemon(cfg, endpoint)
}

func newRestartCommand() *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:   "restart [name]",
		Short: "Restart the background session for this workspace",
		Long: `Stops the running session and starts one from this binary.

Refused while anything depends on it: restarting drops the file server, and a
container holding a directory from it loses its filesystem. -f overrides.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolve(args)
			if err != nil {
				return err
			}
			if err := requireHost(cfg); err != nil {
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

			if err := refuseInUse(cfg, force, "restarting it takes its file server away", "restart"); err != nil {
				return err
			}

			if err := restartDaemon(cfg, endpoint); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(out, "restarted: %s\n", proxy.DockerHost(endpoint))
			return nil
		},
	}
	forceFlag(cmd, &force, "restart even if something depends on the session")
	return cmd
}

// reportLocalSession describes the background session, if there is one,
// including its version, the only outside sign of a stale build.
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
	// versionsLine carries the same marker.
	rowf(out, "session version", "%s (DIFFERENT)", differentBuild(f.local))
}

// warnTraceGoesNowhere warns when REMOTE_DOCKER_TRACE is set here but not on
// the background session, which is the process that forwards and would trace.
func warnTraceGoesNowhere(w io.Writer, st proxy.Status) {
	if !proxy.Tracing() || st.Tracing {
		return
	}
	writeTraceWarning(w, st)
}

// writeTraceWarning is split out so a test need not set the environment.
func writeTraceWarning(w io.Writer, st proxy.Status) {
	_, _ = fmt.Fprintf(w,
		"\nwarning: %s is set here, but the session forwarding the requests (pid %d) was started without it.\n"+
			"  fix: %s=1 %s\n",
		proxy.TraceEnv, st.PID, proxy.TraceEnv, ourCommand("restart"))
}

// warnSlowStorage warns, on every docker command, when the workspace daemon is
// on vfs: it copies the whole image per container, so `docker run` takes
// minutes and presents as a hang. Stderr only.
func warnSlowStorage(w io.Writer, st proxy.Status) {
	if st.Storage != "vfs" {
		return
	}
	_, _ = fmt.Fprintf(w, "\nwarning: %s.\n  fix: %s\n", slowStorage, fixSlowStorage)
}

// `status` prints slowStorage; the warning adds the fix.
const (
	slowStorage    = "the workspace daemon is on vfs, so containers start slowly"
	fixSlowStorage = "set WORKSPACE_DOCKERD_ARGS=--storage-driver=fuse-overlayfs, then rebuild the daemon"
)
