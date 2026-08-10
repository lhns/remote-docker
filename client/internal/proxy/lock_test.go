package proxy

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Binding an endpoint twice must be refused, not silently taken over.
//
// This is the whole point of the lock. On Unix, Listen removed any existing
// socket before binding, which is necessary to recover from a process that died
// without cleaning up, and indiscriminate: a second process unlinked a RUNNING
// one's socket and took its place. The first kept accepting on an inode nobody
// could reach, and when the second exited the path was bound to nothing while
// the first still looked healthy. Nothing reported anything.
func TestListenRefusesASecondOwner(t *testing.T) {
	endpoint := testEndpoint(t)

	first, err := Listen(endpoint)
	if err != nil {
		t.Fatalf("first Listen: %v", err)
	}
	t.Cleanup(func() { _ = first.Close() })

	second, err := Listen(endpoint)
	if err == nil {
		_ = second.Close()
		t.Fatal("a second Listen succeeded; it has taken the endpoint from the first")
	}

	var locked *ErrLocked
	if !errors.As(err, &locked) {
		t.Errorf("second Listen failed with %v, want an ErrLocked naming the owner", err)
	}
}

// Releasing must hand the endpoint on, or a restart could never rebind.
func TestListenAgainAfterClose(t *testing.T) {
	endpoint := testEndpoint(t)

	first, err := Listen(endpoint)
	if err != nil {
		t.Fatalf("first Listen: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second, err := Listen(endpoint)
	if err != nil {
		t.Fatalf("Listen after Close: %v -- the claim outlived the listener", err)
	}
	_ = second.Close()
}

// The pid is what lets `start` and `stop` name the process holding a
// workspace, rather than reporting a bare "Access is denied".
func TestOwnerReportsTheHoldingProcess(t *testing.T) {
	endpoint := testEndpoint(t)

	l, err := Listen(endpoint)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })

	if pid := Owner(endpoint); pid <= 0 {
		t.Errorf("Owner() = %d, want this process's pid", pid)
	}
}

func TestLockPathIsAFilename(t *testing.T) {
	for _, endpoint := range []string{
		`\.\pipe\docker_remote_dev`,
		"/run/user/1000/remote-docker/docker.sock",
		"",
	} {
		got := LockPath(endpoint)
		if base := filepath.Base(got); base == "" || base == "." {
			t.Errorf("LockPath(%q) = %q, which has no filename", endpoint, got)
		}
		if filepath.Ext(got) != ".lock" {
			t.Errorf("LockPath(%q) = %q, want a .lock suffix", endpoint, got)
		}
	}
}

// A refused Listen must not stamp its own pid over the owner's.
//
// This was real, on Windows, and it turned the one message that tells a user
// what to do into a wrong one: the pid was written when the lock file was
// OPENED, before the pipe bind decided anything, so a second process reported
// "already serving (pid ...)" naming ITSELF, a pid that no longer existed by
// the time anyone looked. The owner's identity is the entire content of that
// message.
//
// Both listeners here share a pid, so the assertion is on the record being
// left alone rather than on which pid it holds: a sentinel stands in for the
// owner, and a failed attempt must not disturb it.
func TestARefusedListenDoesNotClaimTheRecord(t *testing.T) {
	endpoint := testEndpoint(t)

	first, err := Listen(endpoint)
	if err != nil {
		t.Fatalf("first Listen: %v", err)
	}
	t.Cleanup(func() { _ = first.Close() })

	const sentinel = "424242\n"
	if err := os.WriteFile(LockPath(endpoint), []byte(sentinel), 0o600); err != nil {
		t.Fatalf("seeding the lock record: %v", err)
	}

	if second, err := Listen(endpoint); err == nil {
		_ = second.Close()
		t.Fatal("a second Listen succeeded; it has taken the endpoint from the first")
	}

	got, err := os.ReadFile(LockPath(endpoint))
	if err != nil {
		t.Fatalf("reading the lock record back: %v", err)
	}
	if string(got) != sentinel {
		t.Errorf("a refused Listen rewrote the owner record to %q; "+
			"the refusal would name the process that just failed", strings.TrimSpace(string(got)))
	}
}
