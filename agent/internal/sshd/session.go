//go:build linux

package sshd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/creack/pty"
	gssh "github.com/gliderlabs/ssh"

	"github.com/lhns/remote-docker/agent/internal/daemons"
	"github.com/lhns/remote-docker/agent/internal/dockercli"
	"github.com/lhns/remote-docker/agent/internal/notify"
	"github.com/lhns/remote-docker/internal/iox"
	"github.com/lhns/remote-docker/pkg/workspace"
)

// Commands the agent answers itself rather than executing.
const (
	// InfoCommand is what the client runs to learn its parameters. Answered
	// from pkg/workspace, the same type the client parses with, so the two
	// cannot disagree about the format.
	InfoCommand = "workspace-info"

	// DialStdioCommand carries the Docker API, spliced straight to the
	// daemon's socket with no CLI in the path. It keeps the docker CLI's
	// spelling because that is what stock sshd could offer, so the client
	// needed no change when the agent replaced it.
	DialStdioCommand = "docker system dial-stdio"
)

// NotifyCommand carries the client's filesystem changes, which the agent
// replays as real syscalls so watchers in containers see them (ADR 0016). The
// name lives in pkg/workspace because both binaries must agree on it.
const NotifyCommand = workspace.NotifyCommand

// handleSession serves one session channel.
func (s *Server) handleSession(session gssh.Session) {
	account, ok := accountFor(session.Context())
	if !ok {
		_ = session.Exit(1)
		return
	}

	command := strings.Join(session.Command(), " ")

	switch command {
	case InfoCommand:
		s.serveInfo(session, account)
	case DialStdioCommand:
		s.serveDockerSocket(session, account)
	case NotifyCommand:
		s.serveNotify(session, account)
	default:
		s.serveExec(session, account, command)
	}
}

// infoQueryTimeout bounds the daemon queries that go into an info reply.
//
// workspace-info is the client's FIRST round trip, and two of its fields come
// from running the docker CLI. Neither is worth waiting on: the version and
// the storage driver are displayed, not acted upon, and a daemon slow enough
// to sit on `docker info` is exactly the daemon whose client should not be
// blocked introducing itself.
const infoQueryTimeout = 5 * time.Second

// serveInfo answers the client's parameters from the shared contract.
func (s *Server) serveInfo(session gssh.Session, account sessionAccount) {
	port, err := s.cfg.Mapping.PortForUID(account.UID())
	if err != nil {
		_, _ = fmt.Fprintln(session.Stderr(), "workspace-info:", err)
		_ = session.Exit(1)
		return
	}

	// Mountpoint and Mounted are left unset. They described a convenience
	// mount the agent made for the interactive shell, which is gone (ADR
	// 0018); the fields stay in the contract because removing a field from a
	// format both binaries parse is worth doing on purpose, not as a side
	// effect of deleting a command.
	info := workspace.Info{
		User:    account.Name(),
		UID:     account.UID(),
		GID:     account.UID(),
		NFSPort: port,
		Docker:  s.dockerVersion(session.Context(), account.Name()),
		Storage: s.storageDriver(session.Context(), account.Name()),
		Mode:    s.mode(),
		Agent:   s.cfg.Version,
	}

	if err := info.Encode(session); err != nil {
		_ = session.Exit(1)
		return
	}
	_ = session.Exit(0)
}

// serveDockerSocket connects the session to the daemon.
//
// This is what the workspace exists to provide, and it is where an account is
// bound to A daemon, which of the two depends on the mode.
//
// This used to say there was no per-account restriction here "because there is
// none to make": one daemon, and reaching it at all is root on it, which was
// the trade ADR 0012 recorded. With a daemon per account that is no longer
// true. The account is resolved to ITS OWN daemon here, and this resolution is
// the only thing between one user's session and another user's containers.
// Getting it wrong does not fail. It succeeds, against the wrong daemon.
//
// Which is why the resolver is asked rather than a mode being branched on: in
// shared mode it answers with the one socket, and there is no second path here
// that could disagree with the first.
func (s *Server) serveDockerSocket(session gssh.Session, account sessionAccount) {
	target, err := s.cfg.Daemons.Ensure(session.Context(), account.Name())
	if err != nil {
		_, _ = fmt.Fprintf(session.Stderr(), "cannot start your docker daemon: %v\n", err)
		_ = session.Exit(1)
		return
	}

	conn, err := net.Dial("unix", target.Socket)
	if err != nil {
		_, _ = fmt.Fprintf(session.Stderr(), "cannot reach the docker daemon: %v\n", err)
		_ = session.Exit(1)
		return
	}
	defer func() { _ = conn.Close() }()

	iox.Splice(session, conn)
	_ = session.Exit(0)
}

// serveNotify replays the client's filesystem changes inside the workspace.
//
// Runs as root, like serveDockerSocket and for the same reason: the paths it
// touches are volume mountpoints under /var/lib/docker, which the account
// cannot reach. Every path is re-validated here rather than trusted, because
// this is a root process being told which path to touch. See
// workspace.FSEvent.Validate, which both sides call.
func (s *Server) serveNotify(session gssh.Session, account sessionAccount) {
	// The volume being replayed into belongs to THIS account's daemon, and the
	// mountpoint that daemon reports is a path in ITS filesystem. Both have to
	// be redirected, and both are resolved per call rather than captured: the
	// daemon restarts, and a stale root would silently name a path in nothing.
	//
	// In shared mode the resolver answers with an empty host and "/", which is
	// the same zero value this used to be configured with, so one expression
	// now serves both arrangements instead of a branch choosing between them.
	name := account.Name()
	target := func() (daemons.Target, error) {
		return s.cfg.Daemons.Ensure(session.Context(), name)
	}
	volumes := notify.DockerVolumes{
		Host: func() (string, error) { t, err := target(); return t.Host, err },
		Root: func() (string, error) { t, err := target(); return t.Root, err },
	}

	replayer := &notify.Replayer{
		Volumes: volumes,
		Poker:   notify.SyscallPoker{},
		// The volume an export lives in belongs to the machine that created
		// it, so a poke has to name the same one the client's rewriter did.
		Client: account.Client(),
		Log:    s.cfg.Log,
	}
	if err := replayer.Serve(session.Context(), session); err != nil {
		s.log().Info("a notify session ended", "account", account.Name(), "err", err)
	}
	_ = session.Exit(0)
}

// serveExec runs a command, or an interactive shell, as the authenticated
// account.
//
// Privilege is dropped to the account's uid. This is the sshd model and the
// reason the agent can run as root without handing root to its users.
func (s *Server) serveExec(session gssh.Session, account sessionAccount, command string) {
	stored, ok := s.cfg.Accounts.Lookup(account.Name())
	if !ok {
		_ = session.Exit(1)
		return
	}

	shell := "/bin/sh"
	if _, err := os.Stat("/bin/bash"); err == nil {
		shell = "/bin/bash"
	}

	args := []string{"-l"}
	if command != "" {
		args = []string{"-c", command}
	}

	cmd := exec.CommandContext(session.Context(), shell, args...)
	cmd.Dir = stored.Home
	cmd.Env = append(os.Environ(),
		"HOME="+stored.Home,
		"USER="+stored.Unix,
		"LOGNAME="+stored.Unix,
		"SHELL="+shell,
	)

	// Point a shell at the account's OWN daemon. Without this, `docker ps` in
	// an ssh session finds /var/run/docker.sock, the parent daemon holding
	// every account's dind, and the separation ends at the first shell
	// prompt.
	//
	// Ensure rather than Lookup: somebody who opens a shell to run docker
	// commands should wait for their daemon rather than be told it is not
	// there. A failure is reported and the shell opens anyway; a shell with no
	// DOCKER_HOST is far better than no shell.
	if target, err := s.cfg.Daemons.Ensure(session.Context(), account.Name()); err != nil {
		_, _ = fmt.Fprintf(session.Stderr(), "your docker daemon is not available: %v\n", err)
	} else if target.Host != "" {
		// Empty means the default socket is already the right one, the shared
		// daemon. Setting DOCKER_HOST to it would be noise in a login shell
		// rather than a redirection.
		cmd.Env = append(cmd.Env, "DOCKER_HOST="+target.Host)
	}
	// Supplementary groups have to be listed, or they are REMOVED.
	//
	// Go calls setgroups() with Credential.Groups whenever a Credential is
	// set, so leaving it nil clears every supplementary group rather than
	// inheriting one. An account correctly listed in `docker` in /etc/group
	// therefore got a shell that was not in it, and `docker ps` answered
	// "permission denied while trying to connect to the Docker daemon socket",
	// which reads exactly like a broken socket and is not one.
	//
	// It cost most of an evening: the group membership was checked, found
	// correct, and believed, because nothing suggested the shell might have a
	// different view of it than /etc/group did.
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{
			Uid:    uint32(stored.UID),
			Gid:    uint32(stored.GID),
			Groups: supplementaryGroups(stored.Unix, stored.GID),
		},
		Setsid: true,
	}

	if ptyReq, winCh, isPty := session.Pty(); isPty {
		cmd.Env = append(cmd.Env, "TERM="+ptyReq.Term)
		s.servePTY(session, cmd, winCh)
		return
	}

	cmd.Stdin = session
	cmd.Stdout = session
	cmd.Stderr = session.Stderr()

	if err := cmd.Run(); err != nil {
		_ = session.Exit(exitCode(err))
		return
	}
	_ = session.Exit(0)
}

// servePTY runs a command attached to a pseudo-terminal.
func (s *Server) servePTY(session gssh.Session, cmd *exec.Cmd, winCh <-chan gssh.Window) {
	f, err := pty.Start(cmd)
	if err != nil {
		_, _ = fmt.Fprintf(session.Stderr(), "cannot allocate a terminal: %v\n", err)
		_ = session.Exit(1)
		return
	}
	defer func() { _ = f.Close() }()

	// Window resizes have to reach the pty, or anything full-screen (an
	// editor, a pager, top) draws at the wrong size for the whole session.
	go func() {
		for win := range winCh {
			_ = pty.Setsize(f, &pty.Winsize{Rows: uint16(win.Height), Cols: uint16(win.Width)})
		}
	}()

	go func() { _, _ = io.Copy(f, session) }()
	_, _ = io.Copy(session, f)

	if err := cmd.Wait(); err != nil {
		_ = session.Exit(exitCode(err))
		return
	}
	_ = session.Exit(0)
}

// dockerVersion asks the account's daemon what it is, for the info reply.
//
// Non-blocking on purpose. In per-account mode this runs while the daemon may
// still be booting: Warm fires at authentication and workspace-info is the
// very next thing the client asks. Waiting would turn every first connection
// into a hang, in exchange for a version string the client only
// displays. An unstarted daemon reports unavailable, exactly like a broken
// one, and the next command starts it.
func (s *Server) dockerVersion(ctx context.Context, account string) string {
	ctx, cancel := context.WithTimeout(ctx, infoQueryTimeout)
	defer cancel()

	// Lookup, never Ensure: see above. A daemon that is not up yet is reported
	// as unavailable rather than started and waited for.
	target, ok := s.cfg.Daemons.Lookup(ctx, account)
	if !ok {
		return workspace.DockerUnavailable
	}

	out, err := dockercli.CLI{Host: target.Host}.Line(ctx, "version", "--format", "{{.Server.Version}}")
	if err != nil {
		// A normal answer, not a failure: the client shows it rather than
		// refusing to start.
		return workspace.DockerUnavailable
	}
	return out
}

func exitCode(err error) int {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	return 1
}

// storageDriver reports the graph driver of the daemon serving this account.
//
// Non-blocking, exactly like dockerVersion and for the same reason: this
// answers the client's first round trip, and a cold daemon must not turn that
// into a wait.
//
// Worth carrying at all because the difference between overlay2 and vfs is the
// difference between `docker run` taking a second and taking minutes, nothing
// about it fails, and the account cannot look for itself: reaching the
// daemon's own host is precisely what it may not do.
func (s *Server) storageDriver(ctx context.Context, account string) string {
	ctx, cancel := context.WithTimeout(ctx, infoQueryTimeout)
	defer cancel()

	target, ok := s.cfg.Daemons.Lookup(ctx, account)
	if !ok {
		return ""
	}

	out, err := dockercli.CLI{Host: target.Host}.Line(ctx, "info", "--format", "{{.Driver}}")
	if err != nil {
		return ""
	}
	return out
}

// supplementaryGroups is the account's group membership as /etc/group has it.
//
// Nil on any failure, which restores the previous behaviour rather than
// refusing the shell: a login with fewer groups is worth having, a login that
// does not happen is not.
func supplementaryGroups(name string, gid int) []uint32 {
	u, err := user.Lookup(name)
	if err != nil {
		return nil
	}
	ids, err := u.GroupIds()
	if err != nil {
		return nil
	}

	out := make([]uint32, 0, len(ids))
	for _, id := range ids {
		n, err := strconv.Atoi(id)
		if err != nil || n == gid {
			// The primary group is already set and does not belong here twice.
			continue
		}
		out = append(out, uint32(n))
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// mode names how this workspace serves daemons, for the info reply. The
// resolver knows, because choosing it is what chose the mode.
func (s *Server) mode() string { return s.cfg.Daemons.Mode() }
