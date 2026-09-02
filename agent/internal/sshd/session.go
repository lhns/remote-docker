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
	"github.com/lhns/remote-docker/agent/internal/unions"
	"github.com/lhns/remote-docker/core-agent/notify"
	"github.com/lhns/remote-docker/core/tunnel"
	"github.com/lhns/remote-docker/core/workspace"
)

// handleSession serves one session channel.
//
// The commands this answers itself rather than executing are named in
// core/tunnel, because every one of them is spoken by the client and
// understood here: a second spelling on either side is a client asking for
// something this switch does not recognise, which arrives as a shell trying to
// run it and exiting 127. Read from there directly, so there is no local
// spelling that could become the second one.
func (s *Server) handleSession(session gssh.Session) {
	account, ok := accountFor(session.Context())
	if !ok {
		_ = session.Exit(1)
		return
	}

	command := strings.Join(session.Command(), " ")

	switch command {
	case workspace.InfoCommand:
		s.serveInfo(session, account)
	case workspace.DialStdioCommand:
		s.serveDockerSocket(session, account)
	case workspace.NotifyCommand:
		s.serveNotify(session, account)
	case workspace.CacheCommand:
		s.serveCache(session, account)
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
	// The port belongs to this MACHINE, not just to the account (ADR 0029).
	// The uid still decides the first one, so a workspace anybody reaches from
	// one computer is on exactly the port it always was; a second computer is
	// given one of its own rather than being refused the first one's.
	port, err := s.cfg.Ports.For(account.Name(), account.UID(), account.Client())
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

		// Which side of a dind mount this is was decided once, on the way in
		// (ADR 0041). The client matches sources against the list and never
		// asks which mode this workspace runs.
		DaemonPaths: s.cfg.DaemonPaths,

		// Whether a delegated share can be a cache here (ADR 0044). Answered
		// now so the client refuses the mode by name rather than half way
		// through a container start, which is the same reason Docker's
		// version is reported.
		Union: s.unionCapability(session.Context(), account.Name()),

		// This workspace's clock, so the client can measure the offset between
		// the two machines rather than assume they agree (ADR 0044).
		Now: time.Now().UnixNano(),
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
// Which daemon is decided here, and with a daemon per account (ADR 0019) that
// decision is the only thing between one user's session and another user's
// containers. It does not fail when it is wrong. It succeeds, against somebody
// else's daemon, with nothing logged.
//
// So ask the resolver rather than branching on the mode: in shared mode it
// answers with the one socket (ADR 0012), which leaves one code path that
// cannot disagree with itself.
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

	tunnel.Splice(session, conn)
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
	// One expression serves both arrangements: in shared mode the resolver
	// answers with an empty host and "/", which mean "no redirection", so
	// there is no mode to branch on here.
	name := account.Name()
	target := func() (daemons.Target, error) {
		return s.cfg.Daemons.Ensure(session.Context(), name)
	}
	volumes := dockercli.Volumes{
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

// unionCapability reports whether this workspace can serve a delegated share as
// a cache, for the account asking.
//
// Asked of the daemon that would serve it rather than of the agent, because
// that is where the answer differs: in per-account mode fuse-overlayfs has to
// be in the image THAT daemon runs, and the agent's own filesystem says nothing
// about it. Lookup rather than Ensure, for the same reason the version and the
// storage driver use it: this is the client's first round trip and must not
// wait for a daemon to boot.
func (s *Server) unionCapability(ctx context.Context, account string) string {
	ctx, cancel := context.WithTimeout(ctx, infoQueryTimeout)
	defer cancel()

	if s.cfg.Unions == nil {
		return ""
	}
	target, ok := s.cfg.Daemons.Lookup(ctx, account)
	if !ok {
		// A daemon that has not started yet cannot be asked, and guessing
		// would be worse than saying nothing: an empty answer reads as "not
		// available", which is what an older agent's answer reads as too.
		return ""
	}
	return unions.Capability(ctx, target.Root)
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
