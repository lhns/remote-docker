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

	"github.com/lhns/remote-docker/internal/client/config"
	"github.com/lhns/remote-docker/internal/client/proxy"
)

// `up` in the background.
//
// `up` itself is unchanged -- foreground, blocking, reporting -- which is what
// makes it the daemon body for free: `start` spawns exactly the command a
// person would have run, with its output going to a log instead of a terminal.
// There is no second implementation of the session to keep in step with the
// first, and `up --help` still describes what the daemon does.

// startTimeout is how long to wait for a spawned daemon to answer.
//
// Generous: the first thing it does is bind the endpoint, but on a cold start
// it may also be loading a key and reading known_hosts off a slow disk.
const startTimeout = 20 * time.Second

func newStartCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "Start a background session for this workspace",
		Long: `Starts a session in the background and returns, so no terminal has to stay
open. Idempotent: if one is already running, this says so and does nothing.

Use "remote-docker up" instead to run one in the foreground.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := resolve()
			if err != nil {
				return err
			}
			if err := cfg.RequireHost(); err != nil {
				return err
			}
			endpoint := cfg.EndpointFor(proxy.DefaultEndpoint)
			out := cmd.OutOrStdout()

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
			endpoint := cfg.EndpointFor(proxy.DefaultEndpoint)
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
			deadline := time.Now().Add(10 * time.Second)
			for time.Now().Before(deadline) {
				if !proxy.Reachable(endpoint) {
					_, _ = fmt.Fprintln(out, "stopped")
					return nil
				}
				time.Sleep(100 * time.Millisecond)
			}
			return fmt.Errorf("the session acknowledged the stop but is still serving %s",
				proxy.DockerHost(endpoint))
		},
	}
}

// startDaemon spawns `up` detached and waits for it to answer.
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
	args := []string{"up"}
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

	deadline := time.Now().Add(startTimeout)
	for time.Now().Before(deadline) {
		if proxy.Reachable(endpoint) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
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
