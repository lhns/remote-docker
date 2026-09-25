package main

// `status`: the verdict on the first line, then rows grouped by question (is
// it up, what is on the other end, which versions). Add rows to their group,
// never at the end.

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/docker/cli/cli"
	dockerconfig "github.com/docker/cli/cli/config"
	"github.com/spf13/cobra"

	"github.com/lhns/remote-docker/client/internal/config"
	"github.com/lhns/remote-docker/client/internal/proxy"
	"github.com/lhns/remote-docker/client/internal/session"
	"github.com/lhns/remote-docker/core/workspace"
)

// facts is everything status found, gathered first because the verdict,
// printed first, depends on all of it.
type facts struct {
	cfg      config.Config
	endpoint string

	// serving: something answers the endpoint. answering: it reports local.
	serving   bool
	answering bool
	local     proxy.Status

	// infoErr is reported, not fatal: an unreachable workspace is when
	// somebody runs this.
	info    workspace.Info
	infoErr error
}

// gather collects the local facts, which never fail as a whole.
func gather(cfg config.Config) facts {
	f := facts{cfg: cfg, endpoint: endpointOf(cfg)}

	f.serving = proxy.Reachable(f.endpoint)
	if f.serving {
		f.answering = control(f.endpoint, http.MethodGet, "status", &f.local) == nil
	}
	return f
}

// askWorkspace fills in the half that needs the network.
func (f *facts) askWorkspace() {
	f.infoErr = withQuerySession(f.cfg, func(ctx context.Context, s *session.Session) error {
		info, err := s.Info(ctx)
		if err != nil {
			return err
		}
		f.info = info
		return nil
	})
}

// verdict is the first problem, or "ready" with any non-fatal warning.
func (f facts) verdict() string {
	switch {
	case f.infoErr != nil:
		return "cannot reach the workspace: " + firstLine(f.infoErr.Error())
	case !f.serving:
		return "no session (run `" + ourCommand("start") + "`)"
	case !f.answering:
		return "a session is serving the endpoint but will not answer"
	case versionDiffers(f.local):
		return fmt.Sprintf("the running session is %s; run `%s`", differentBuild(f.local), ourCommand("restart"))
	}

	if f.info.Storage == "vfs" {
		return "ready, but " + slowStorage
	}
	return "ready"
}

// ready is whether the verdict is, which is what status exits on.
func (f facts) ready() bool { return strings.HasPrefix(f.verdict(), "ready") }

// reportStatus prints the verdict and the detail behind it.
func reportStatus(out io.Writer, f facts) {
	row(out, "status", f.verdict())
	rowf(out, "workspace", "%s (%s)", contextHint(f.cfg), where(f.cfg))

	// Is it up, and how does anything else reach it.
	_, _ = fmt.Fprintln(out)
	row(out, "session", f.sessionLine())
	row(out, "endpoint", proxy.DockerHost(f.endpoint))
	row(out, "docker", dockerReach(f.cfg))

	// What is on the other end; the verdict already says if unreachable.
	if f.infoErr == nil {
		_, _ = fmt.Fprintln(out)
		row(out, "daemon", daemonLine(f.info))
		rowf(out, "account", "%s (uid %d), tunnel port %d", f.info.User, f.info.UID, f.info.NFSPort)
	}

	// How much of each cached share is local (ADR 0044).
	for _, cache := range f.local.Caches {
		row(out, "cache", cache)
	}

	// Which builds are in play.
	_, _ = fmt.Fprintln(out)
	row(out, "versions", versionsLine(f))
}

// sessionLine is what the background session is doing, shared with
// `workspace inspect`.
func (f facts) sessionLine() string {
	switch {
	case !f.serving:
		return "not running"
	case !f.answering:
		return "running, but not answering"
	case f.local.Drops > 0 && !f.local.Connected:
		return fmt.Sprintf("running (pid %d, since %s), reconnecting on the next command",
			f.local.PID, f.local.Since)
	case f.local.Drops > 0:
		return fmt.Sprintf("running (pid %d, since %s), reconnected %s (last %s)",
			f.local.PID, f.local.Since, times(f.local.Drops), f.local.LastDrop)
	default:
		return fmt.Sprintf("running (pid %d, since %s)", f.local.PID, f.local.Since)
	}
}

// times reads "once" or "3 times".
func times(n int) string {
	if n == 1 {
		return "once"
	}
	return fmt.Sprintf("%d times", n)
}

// dockerReach is what a tool that is not this binary will talk to. The context
// is read from docker's config file: spawning a CLI costs half a second.
func dockerReach(cfg config.Config) string {
	if host := os.Getenv("DOCKER_HOST"); host != "" {
		return "DOCKER_HOST=" + host + " (overrides any context)"
	}

	ours := cfg.ContextName()
	switch current := currentDockerContext(); current {
	case ours:
		return fmt.Sprintf("context %q is selected", ours)
	case "":
		return fmt.Sprintf("no context selected (run `%s`)", ourCommand("use "+contextHint(cfg)))
	default:
		return fmt.Sprintf("context %q is selected, not %q", current, ours)
	}
}

// contextHint is what to type after `workspace use`, which is the workspace's
// name rather than the context's when they differ.
func contextHint(cfg config.Config) string { return cmp.Or(cfg.Name, cfg.ContextName()) }

func currentDockerContext() string {
	cf, err := dockerconfig.Load(dockerconfig.Dir())
	if err != nil {
		return ""
	}
	return cf.CurrentContext
}

// daemonLine is the daemon mode, docker version and storage driver.
func daemonLine(info workspace.Info) string {
	parts := []string{}
	if info.Mode != "" {
		parts = append(parts, info.Mode)
	}
	if info.Docker != "" {
		parts = append(parts, "docker "+info.Docker)
	}
	switch info.Storage {
	case "":
	case "vfs":
		parts = append(parts, "vfs (slow: every container create copies the whole image)")
	default:
		parts = append(parts, info.Storage)
	}
	if len(parts) == 0 {
		return "not reported"
	}
	return strings.Join(parts, ", ")
}

// versionsLine puts client, agent and session builds side by side.
func versionsLine(f facts) string {
	parts := []string{"client " + orUnknown(version)}

	if f.infoErr == nil {
		parts = append(parts, "agent "+cmp.Or(f.info.Agent, "not reported"))
	}
	if f.answering && versionDiffers(f.local) {
		parts = append(parts, "session "+orUnknown(f.local.Version)+" (DIFFERENT)")
	}
	return strings.Join(parts, ", ")
}

// firstLine keeps a verdict to one line.
func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return strings.TrimSpace(line)
}

func newStatusCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "status [name]",
		Short: "Show whether the session works and what it is talking to",
		Long: `Prints a verdict first: ready, or the first thing that is wrong.

Then the detail behind it, grouped by question: whether a session is up and
how other tools reach it, what is on the other end, and which builds are in
play.

Reports what it can even when the workspace cannot be reached, which is when
somebody is most likely to be running it. Exits 1 when the verdict is not
ready.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolve(args)
			if err != nil {
				return err
			}
			if err := requireHost(cfg); err != nil {
				return err
			}

			f := gather(cfg)
			f.askWorkspace()
			reportStatus(cmd.OutOrStdout(), f)
			if !f.ready() {
				return errNotReady
			}
			return nil
		},
	}
}

// errNotReady exits 1 and prints nothing: the verdict above already said why.
// An empty cli.StatusError is how main is told to stay quiet (see exitCode).
var errNotReady = cli.StatusError{StatusCode: 1}

// row prints one aligned "key    value" line, shared by `status` and
// `workspace inspect`. An empty value prints nothing.
func row(out io.Writer, key, value string) {
	if value != "" {
		_, _ = fmt.Fprintf(out, "%-20s %s\n", key, value)
	}
}

// rowf is row with a formatted value.
func rowf(out io.Writer, key, format string, args ...any) {
	row(out, key, fmt.Sprintf(format, args...))
}
