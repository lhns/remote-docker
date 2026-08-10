package elevate

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"

	"github.com/lhns/remote-docker/internal/logx"
	"github.com/lhns/remote-docker/internal/server/dockercli"
)

// SelfEnv names the environment variable holding our own container id or name.
//
// Swarm expands service templates in environment values, so a deployment can
// simply say `WORKSPACE_SELF: "{{.Task.Name}}"` and no guessing is needed on
// the path that matters. Docker accepts a name wherever it accepts an id.
const SelfEnv = "WORKSPACE_SELF"

// HostSocketEnv overrides where the host's Docker socket is mounted.
const HostSocketEnv = "WORKSPACE_HOST_SOCKET"

// Runner performs the elevation.
type Runner struct {
	// HostSocket is the host daemon's socket inside this container.
	HostSocket string

	// Log receives progress. Nil means silence.
	Log *slog.Logger
}

// Run inspects this container, launches a privileged copy, and blocks until it
// exits, returning its exit code.
func (r *Runner) Run(ctx context.Context) (int, error) {
	self, err := r.inspect(ctx, r.selfRef())
	if err != nil {
		return 1, err
	}

	spec, err := Plan(self, Options{HostSocket: r.hostSocket()})
	if err != nil {
		return 1, err
	}

	// A previous task that died without cleanup leaves the name taken, and
	// `docker run --name` fails on a conflict rather than replacing it. This
	// is the difference between a node recovering by itself and needing a
	// human.
	if spec.Name != "" {
		r.log().Info("removing any stale container", "container", spec.Name)
		_ = r.docker(ctx, "rm", "-f", spec.Name).Run()
	}

	r.log().Info("launching a container privileged, sharing our network namespace", "container", spec.Name)
	return r.launch(ctx, spec)
}

// launch starts the child and forwards signals to it.
func (r *Runner) launch(ctx context.Context, spec RunSpec) (int, error) {
	cmd := r.docker(ctx, spec.Args()...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return 1, fmt.Errorf("elevate: starting the privileged container: %w", err)
	}

	// Signals have to reach the container, or stopping the Swarm task would
	// leave the privileged child running and holding the published port --
	// and the next task would then fail to bind it.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(signals)

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	for {
		select {
		case sig := <-signals:
			r.log().Info("forwarding a signal", "signal", sig, "container", spec.Name)
			if cmd.Process != nil {
				_ = cmd.Process.Signal(sig)
			}
			// `docker run` proxies the signal onward and then exits when the
			// container does, so keep waiting rather than exiting here.
		case err := <-done:
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				return exit.ExitCode(), nil
			}
			if err != nil {
				return 1, fmt.Errorf("elevate: running the privileged container: %w", err)
			}
			return 0, nil
		}
	}
}

// Inspect reads a container's configuration from the host daemon.
func (r *Runner) inspect(ctx context.Context, ref string) (ContainerInfo, error) {
	if ref == "" {
		return ContainerInfo{}, fmt.Errorf(
			"elevate: cannot identify this container; set %s (Swarm can template it as {{.Task.Name}})",
			SelfEnv)
	}

	out, err := r.docker(ctx, "container", "inspect", ref, "--format", "{{json .}}").Output()
	if err != nil {
		return ContainerInfo{}, fmt.Errorf("elevate: inspecting %s through the host daemon: %w", ref, err)
	}

	var raw struct {
		ID     string `json:"Id"`
		Name   string `json:"Name"`
		Image  string `json:"Image"`
		Config struct {
			Image string   `json:"Image"`
			Env   []string `json:"Env"`
		} `json:"Config"`
		Mounts []struct {
			Type        string `json:"Type"`
			Name        string `json:"Name"`
			Source      string `json:"Source"`
			Destination string `json:"Destination"`
			RW          bool   `json:"RW"`
		} `json:"Mounts"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return ContainerInfo{}, fmt.Errorf("elevate: decoding container inspect: %w", err)
	}

	// Config.Image is the reference as requested, which is what a Swarm
	// service resolves and what we want to relaunch. The top-level Image is a
	// digest, kept as a fallback so a missing Config.Image is not fatal.
	image := raw.Config.Image
	if image == "" {
		image = raw.Image
	}

	info := ContainerInfo{
		ID:    raw.ID,
		Name:  raw.Name,
		Image: image,
		Env:   raw.Config.Env,
	}
	for _, m := range raw.Mounts {
		info.Mounts = append(info.Mounts, Mount{
			Type:        m.Type,
			Name:        m.Name,
			Source:      m.Source,
			Destination: m.Destination,
			ReadOnly:    !m.RW,
		})
	}
	return info, nil
}

// selfRef works out how to refer to this container.
func (r *Runner) selfRef() string {
	if ref := os.Getenv(SelfEnv); ref != "" {
		return ref
	}
	if id := containerIDFromMountinfo("/proc/self/mountinfo"); id != "" {
		return id
	}
	// The hostname is the short container id unless something overrode it --
	// and a Swarm service usually does, which is why this is last.
	host, err := os.Hostname()
	if err != nil {
		return ""
	}
	return host
}

// containerIDFromMountinfo recovers our container id from the paths Docker
// bind-mounts into every container (/etc/hosts and friends live under
// /var/lib/docker/containers/<id>/).
//
// The fallback for plain Docker, where there is no Swarm template to tell us.
func containerIDFromMountinfo(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()

	const marker = "/containers/"
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		i := strings.Index(line, marker)
		if i < 0 {
			continue
		}
		rest := line[i+len(marker):]
		id, _, _ := strings.Cut(rest, "/")
		// Container ids are 64 hex characters; anything else is a coincidence
		// of path naming rather than the id we want.
		if len(id) == 64 && isHex(id) {
			return id
		}
	}
	return ""
}

func isHex(s string) bool {
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

func (r *Runner) docker(ctx context.Context, args ...string) *exec.Cmd {
	return dockercli.CLI{Host: "unix://" + r.hostSocket()}.Cmd(ctx, args...)
}

func (r *Runner) hostSocket() string {
	if r.HostSocket != "" {
		return r.HostSocket
	}
	if s := os.Getenv(HostSocketEnv); s != "" {
		return s
	}
	return DefaultHostSocket
}

// log is the runner's logger, or silence. A nil *slog.Logger panics on use
// rather than doing nothing, so the zero value needs an answer.
func (r *Runner) log() *slog.Logger {
	if r.Log == nil {
		return logx.Discard()
	}
	return r.Log
}
