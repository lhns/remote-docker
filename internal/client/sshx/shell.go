package sshx

import (
	"context"
	"errors"
	"fmt"
	"os"

	"golang.org/x/crypto/ssh"
	"golang.org/x/term"
)

// Shell runs an interactive session on the workspace, attached to this
// terminal.
//
// The local terminal goes into raw mode for the duration so the remote shell
// receives keystrokes as they are typed -- without it, the line discipline
// here would swallow control characters, and Ctrl-C would kill this process
// rather than the remote command.
func (c *Client) Shell(ctx context.Context, command string) error {
	sess, err := c.ssh.NewSession()
	if err != nil {
		return fmt.Errorf("sshx: opening session: %w", err)
	}
	defer func() { _ = sess.Close() }()

	sess.Stdin = os.Stdin
	sess.Stdout = os.Stdout
	sess.Stderr = os.Stderr

	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		state, err := term.MakeRaw(fd)
		if err != nil {
			return fmt.Errorf("sshx: putting the terminal into raw mode: %w", err)
		}
		defer func() { _ = term.Restore(fd, state) }()

		width, height, err := term.GetSize(fd)
		if err != nil || width <= 0 || height <= 0 {
			// A sane default beats refusing to open a shell because the size
			// could not be read.
			width, height = 80, 24
		}

		modes := ssh.TerminalModes{
			ssh.ECHO:          1,
			ssh.TTY_OP_ISPEED: 14400,
			ssh.TTY_OP_OSPEED: 14400,
		}
		termType := os.Getenv("TERM")
		if termType == "" {
			termType = "xterm-256color"
		}
		if err := sess.RequestPty(termType, height, width, modes); err != nil {
			return fmt.Errorf("sshx: requesting a pty: %w", err)
		}
	}

	if err := sess.Start(command); err != nil {
		return fmt.Errorf("sshx: starting the shell: %w", err)
	}

	done := make(chan error, 1)
	go func() { done <- sess.Wait() }()

	select {
	case <-ctx.Done():
		_ = sess.Signal(ssh.SIGHUP)
		return ctx.Err()
	case err := <-done:
		// A shell exiting non-zero is how shells work, not a failure of ours.
		var exit *ssh.ExitError
		if errors.As(err, &exit) {
			return nil
		}
		return err
	}
}
