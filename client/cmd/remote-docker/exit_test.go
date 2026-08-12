package main

// What this program exits with, and what it says on the way out.
//
// Both are the Docker CLI's contract rather than ours, because the root command
// IS the Docker CLI (ADR 0024): a script running `docker run ...; echo $?` has
// to get what the container returned, and a container that exits non-zero must
// not put an extra line on the terminal.

import (
	"errors"
	"fmt"
	"testing"

	"github.com/docker/cli/cli"
)

// A container's exit status reaches here as cli.StatusError and has to survive.
//
// Collapsing it to 1 is invisible until something branches on the code, and
// then it is wrong everywhere at once.
func TestExitCodePassesAContainersStatusThrough(t *testing.T) {
	if got := exitCode(cli.StatusError{StatusCode: 3}); got != 3 {
		t.Errorf("exitCode(StatusError{3}) = %d, want 3", got)
	}
	// Wrapped, because anything between here and the command may add context.
	wrapped := fmt.Errorf("running the container: %w", cli.StatusError{StatusCode: 42})
	if got := exitCode(wrapped); got != 42 {
		t.Errorf("a wrapped status was flattened to %d, want 42", got)
	}
}

// Success is 0 and everything else is at least 1. A failure that exits 0 is the
// dangerous direction: it reports success to whatever ran us.
func TestExitCodeNeverReportsFailureAsSuccess(t *testing.T) {
	if got := exitCode(nil); got != 0 {
		t.Errorf("exitCode(nil) = %d, want 0", got)
	}
	if got := exitCode(errors.New("something went wrong")); got != 1 {
		t.Errorf("a plain error exited %d, want 1", got)
	}
	// A StatusError carrying a zero code is still a failure: docker sets the
	// code only when it means it, so zero here means "no code", not "success".
	if got := exitCode(cli.StatusError{Status: "boom"}); got != 1 {
		t.Errorf("a zero-coded StatusError exited %d, want 1", got)
	}
}

// The message decides whether anything is printed, and an empty one means
// silence.
//
// cli.StatusError.Error() is deliberately "" when it carries only an exit code,
// which is how `docker run` returns a container's status. Printing regardless
// puts a bare "remote-docker:" on the terminal after every non-zero container,
// which is what this pins.
func TestAStatusOnlyErrorHasNothingToSay(t *testing.T) {
	if msg := (cli.StatusError{StatusCode: 3}).Error(); msg != "" {
		t.Fatalf("a status-only error now has the message %q; main prints when "+
			"the message is non-empty, so check that assumption still holds", msg)
	}
	// And one with something to say still says it.
	if msg := (cli.StatusError{Status: "boom", StatusCode: 2}).Error(); msg != "boom" {
		t.Errorf("StatusError{Status: boom}.Error() = %q, want boom", msg)
	}
}
