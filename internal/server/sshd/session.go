//go:build linux

package sshd

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/creack/pty"
	gssh "github.com/gliderlabs/ssh"

	"github.com/lhns/remote-docker/internal/server/mount"
	"github.com/lhns/remote-docker/internal/server/notify"
	"github.com/lhns/remote-docker/pkg/workspace"
)

// Commands the agent answers itself rather than executing.
const (
	// InfoCommand is what the client runs to learn its parameters. Answered
	// from pkg/workspace, the same type the client parses with, so the two
	// cannot disagree about the format.
	InfoCommand = "workspace-info"

	// DialStdioCommand carries the Docker API. Spliced straight to the
	// daemon's socket, with no CLI in the path -- the client uses the docker
	// CLI's spelling of this because that is what stock sshd could offer, and
	// keeping the name means the client needs no change to talk to the agent.
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

// serveInfo answers the client's parameters from the shared contract.
func (s *Server) serveInfo(session gssh.Session, account sessionAccount) {
	port, err := s.cfg.Mapping.PortForUID(account.UID())
	if err != nil {
		_, _ = fmt.Fprintln(session.Stderr(), "workspace-info:", err)
		_ = session.Exit(1)
		return
	}

	stored, _ := s.cfg.Accounts.Lookup(account.Name())
	home := ""
	if stored != nil {
		home = stored.Home
	}

	info := workspace.Info{
		User:       account.Name(),
		UID:        account.UID(),
		GID:        account.UID(),
		NFSPort:    port,
		Mountpoint: home + "/workspace",
		Mounted:    mount.IsMounted(home + "/workspace"),
		Docker:     s.dockerVersion(),
	}

	if err := info.Encode(session); err != nil {
		_ = session.Exit(1)
		return
	}
	_ = session.Exit(0)
}

// serveDockerSocket connects the session to the daemon.
//
// This is what the workspace exists to provide, and it is the one place where
// every account's access to the shared daemon is granted. There is no
// per-account restriction here, because there is none to make: membership of
// the docker group is root on the daemon, which is the trade ADR 0012 records.
func (s *Server) serveDockerSocket(session gssh.Session, account sessionAccount) {
	conn, err := net.Dial("unix", s.cfg.DockerSocket)
	if err != nil {
		_, _ = fmt.Fprintf(session.Stderr(), "cannot reach the docker daemon: %v\n", err)
		_ = session.Exit(1)
		return
	}
	defer func() { _ = conn.Close() }()

	splice(session, conn)
	_ = session.Exit(0)
}

// serveNotify replays the client's filesystem changes inside the workspace.
//
// Runs as root, like serveDockerSocket and for the same reason: the paths it
// touches are volume mountpoints under /var/lib/docker, which the account
// cannot reach. Every path is re-validated here rather than trusted, because
// this is a root process being told which path to touch -- see
// workspace.FSEvent.Validate, called on both sides.
func (s *Server) serveNotify(session gssh.Session, account sessionAccount) {
	if s.cfg.Volumes == nil {
		// Nothing to resolve exports against. Exiting non-zero is what tells
		// the client to stop trying rather than reconnect forever.
		_, _ = fmt.Fprintln(session.Stderr(), "change notification is not available on this workspace")
		_ = session.Exit(1)
		return
	}

	// The interactive shell's ~/workspace is a second, separate mount of the
	// same export. Separate mounts do not share an inode the way dockerd's
	// bind mount does, so it has to be poked as well or a shell sees nothing.
	extra := map[string]string{}
	if stored, ok := s.cfg.Accounts.Lookup(account.Name()); ok && stored.Home != "" {
		extra[workspace.ExportCWD] = stored.Home + "/workspace"
	}

	replayer := &notify.Replayer{
		Volumes: s.cfg.Volumes,
		Poker:   notify.SyscallPoker{},
		Log:     s.cfg.Log,
		Extra:   extra,
	}
	if err := replayer.Serve(session.Context(), session); err != nil {
		s.logf("notify session for %s ended: %v", account.Name(), err)
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
		"USER="+stored.Name,
		"LOGNAME="+stored.Name,
		"SHELL="+shell,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{
			Uid: uint32(stored.UID),
			Gid: uint32(stored.GID),
		},
		Setsid: true,
	}

	if ptyReq, winCh, isPty := session.Pty(); isPty {
		cmd.Env = append(cmd.Env, "TERM="+ptyReq.Term)

		// Somewhere for the shell to land. Attempted here rather than when the
		// reverse forward was established, because the client begins serving
		// NFS a moment after asking for the forward -- mounting then would
		// race it. A failure is reported and the shell opens anyway: a shell
		// in the home directory is far better than no shell.
		if s.cfg.Mounts != nil {
			if port, err := s.cfg.Mapping.PortForUID(stored.UID); err == nil {
				if err := s.cfg.Mounts.Ensure(stored.Home, stored.UID, stored.GID, port); err != nil {
					_, _ = fmt.Fprintf(session.Stderr(), "workspace not mounted: %v\n", err)
				}
			}
		}

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

	// Window resizes have to reach the pty, or anything full-screen -- an
	// editor, a pager, top -- draws at the wrong size for the whole session.
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

// dockerVersion asks the daemon what it is, for the info reply.
func (s *Server) dockerVersion() string {
	out, err := exec.Command("docker", "version", "--format", "{{.Server.Version}}").Output()
	if err != nil {
		// A normal answer, not a failure: the client shows it rather than
		// refusing to start.
		return workspace.DockerUnavailable
	}
	return strings.TrimSpace(string(out))
}

func exitCode(err error) int {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	return 1
}
