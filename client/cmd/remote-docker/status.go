package main

// What `status` answers, and in what order.
//
// It used to print eleven rows in the order the code learned them, with no
// line saying whether any of it was working. The facts were right and the
// question was unanswered, so the command somebody runs when they are unsure
// left them unsure.
//
// Now: the verdict first, then the rows grouped by the question they answer.
// Is it up and how do tools reach it. What is on the other end. What versions
// are in play.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	dockerconfig "github.com/docker/cli/cli/config"
	"github.com/spf13/cobra"

	"github.com/lhns/remote-docker/client/internal/config"
	"github.com/lhns/remote-docker/client/internal/proxy"
	"github.com/lhns/remote-docker/client/internal/session"
	"github.com/lhns/remote-docker/pkg/workspace"
)

// facts is everything status found, gathered before anything is printed.
//
// Collected rather than printed as it goes, because the verdict is a function
// of all of it and has to appear first.
type facts struct {
	cfg      config.Config
	endpoint string

	// serving is whether anything answers the endpoint, and local is what the
	// session says about itself. A session that serves but will not answer is
	// its own state, not a missing one.
	serving   bool
	answering bool
	local     proxy.Status

	// info is what the workspace reports, and infoErr is why it does not.
	// Unreachable is a state to report, not a reason to print nothing: this is
	// the command somebody runs when the workspace is unreachable.
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
	f.infoErr = withQuerySession(func(ctx context.Context, s *session.Session) error {
		info, err := s.Info(ctx)
		if err != nil {
			return err
		}
		f.info = info
		return nil
	})
}

// verdict is the one line somebody came for.
//
// The FIRST problem, not a summary of all of them: a reader acts on one thing
// at a time, and the rows below carry the detail. Warnings that do not stop
// anything working are appended to "ready" rather than replacing it.
func (f facts) verdict() string {
	switch {
	case f.infoErr != nil:
		return "cannot reach the workspace: " + firstLine(f.infoErr.Error())
	case !f.serving:
		return "no session (run `" + ourCommand("start") + "`)"
	case !f.answering:
		return "a session is serving the endpoint but will not answer"
	case f.local.Version != version:
		return fmt.Sprintf("the running session is a different build, %s (run `"+ourCommand("restart")+"`)",
			orUnknown(f.local.Version))
	}

	if f.info.Storage == "vfs" {
		return "ready, but the workspace daemon is on vfs, so containers start slowly"
	}
	return "ready"
}

// reportStatus prints the verdict and the detail behind it.
func reportStatus(out io.Writer, f facts) {
	row(out, "status", f.verdict())
	rowf(out, "workspace", "%s (%s@%s:%d)", contextHint(f.cfg), f.cfg.User, f.cfg.Host, f.cfg.Port)

	// Is it up, and how does anything else reach it.
	_, _ = fmt.Fprintln(out)
	row(out, "session", f.sessionLine())
	// The DOCKER_HOST spelling rather than the raw path, because that is the
	// form anything else has to be given.
	row(out, "endpoint", proxy.DockerHost(f.endpoint))
	row(out, "docker", dockerReach(f.cfg))

	// What is on the other end. Skipped entirely when there is no other end:
	// the verdict already carries the reason, and repeating it as a row says
	// the same thing twice to somebody who has read it once.
	if f.infoErr == nil {
		_, _ = fmt.Fprintln(out)
		row(out, "daemon", daemonLine(f.info))
		rowf(out, "account", "%s (uid %d), tunnel port %d", f.info.User, f.info.UID, f.info.NFSPort)
	}

	// What is in play, which is the question when something behaves oddly.
	_, _ = fmt.Fprintln(out)
	row(out, "versions", versionsLine(f))
}

// sessionLine is what the background session is doing, in one row's worth.
//
// Here rather than in daemon.go because `workspace inspect` asks the same
// question and used to answer it with its own copy of the reachable/answering
// dance.
func (f facts) sessionLine() string {
	switch {
	case !f.serving:
		return "not running"
	case !f.answering:
		return "running, but not answering"
	default:
		return fmt.Sprintf("running (pid %d, since %s)", f.local.PID, f.local.Since)
	}
}

// dockerReach is what a tool that is not this binary will talk to.
//
// The docker context is read from docker's own config rather than asked of a
// docker command: it is one file, and spawning a CLI to read it would cost
// half a second on a command that is otherwise instant.
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
func contextHint(cfg config.Config) string {
	if cfg.Name != "" {
		return cfg.Name
	}
	return cfg.ContextName()
}

func currentDockerContext() string {
	cf, err := dockerconfig.Load(dockerconfig.Dir())
	if err != nil {
		return ""
	}
	return cf.CurrentContext
}

// daemonLine folds the three facts about the far side into one row: which
// arrangement, which docker, and the storage driver that decides whether a
// container starts in a second or a minute.
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
		parts = append(parts, "vfs (SLOW: every container create copies the whole image)")
	default:
		parts = append(parts, info.Storage)
	}
	if len(parts) == 0 {
		return "not reported"
	}
	return strings.Join(parts, ", ")
}

// versionsLine puts the three builds side by side, because "it behaves like
// the old one" is answered by comparing them and by nothing else.
func versionsLine(f facts) string {
	parts := []string{"client " + orUnknown(version)}

	if f.infoErr == nil {
		agent := f.info.Agent
		if agent == "" {
			agent = "not reported"
		}
		parts = append(parts, "agent "+agent)
	}
	if f.answering && f.local.Version != version {
		parts = append(parts, "session "+orUnknown(f.local.Version)+" (DIFFERENT)")
	}
	return strings.Join(parts, ", ")
}

// firstLine keeps a verdict to one line. A session failure can be a paragraph,
// and the rows below have room for the rest.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

func newStatusCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Is this working, and what is it talking to?",
		Long: `Prints a verdict first: ready, or the first thing that is wrong.

Then the detail behind it, grouped by question: whether a session is up and
how other tools reach it, what is on the other end, and which builds are in
play.

Reports what it can even when the workspace cannot be reached, which is when
somebody is most likely to be running it.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := resolve()
			if err != nil {
				return err
			}
			// The one thing worth failing on: with no host there is no
			// workspace to have a status.
			if err := cfg.RequireHost(); err != nil {
				return err
			}

			f := gather(cfg)
			f.askWorkspace()
			reportStatus(cmd.OutOrStdout(), f)
			return nil
		},
	}
}

// row prints one aligned "key    value" line.
//
// `status` and `workspace inspect` print one table each and share this width,
// so a row added to one lines up in the other. It was a bare %-20s at thirteen
// call sites.
func row(out io.Writer, key, value string) {
	if value != "" {
		_, _ = fmt.Fprintf(out, "%-20s %s\n", key, value)
	}
}

// rowf is row with a formatted value.
func rowf(out io.Writer, key, format string, args ...any) {
	row(out, key, fmt.Sprintf(format, args...))
}
