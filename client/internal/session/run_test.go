package session

import (
	"context"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/lhns/remote-docker/client/internal/config"
	"github.com/lhns/remote-docker/core/workspace"
)

// A client run (ADR 0050): the process names itself once per connection, with
// one id for its whole life.

func openQuery(t *testing.T) *Session {
	t.Helper()
	s, err := Open(context.Background(), Options{
		Config: config.Config{Host: "workspace.invalid", User: "alice", Port: 22},
		Role:   Query,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestTheRunIDIsOnePerProcess(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	ws := startWorkspaceWith(t, &silentWorkspace{global: func(req *ssh.Request) (bool, []byte) {
		if req.Type == workspace.RunRequest {
			mu.Lock()
			seen = append(seen, string(req.Payload))
			mu.Unlock()
		}
		return true, nil
	}})

	s := openQuery(t)
	// Two connections, as an idle release and a reconnect make.
	for range 2 {
		if err := s.announceRun(t.Context(), ws.dial(t)); err != nil {
			t.Fatalf("announceRun: %v", err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || seen[0] != seen[1] {
		t.Fatalf("the workspace was told %q, want one run id twice", seen)
	}
	if !workspace.ValidRunID(seen[0]) {
		t.Errorf("%q is not a run id", seen[0])
	}
	if other := openQuery(t); other.runID == seen[0] {
		t.Error("a second process is the same run")
	}
}

// An agent that predates runs refuses the request with no reason, and the
// client carries on as the machine its key names.
func TestAnOlderAgentLeavesAMachine(t *testing.T) {
	s := openQuery(t)
	client := startWorkspace(t, func(string) (string, bool) { return "", true })
	if err := s.announceRun(t.Context(), client); err != nil {
		t.Fatalf("an older agent's refusal failed the connection: %v", err)
	}

	key := []byte("ssh-ed25519 AAAA...alice-laptop")
	if got := clientIDFor(workspace.Info{User: "alice"}, key); got != workspace.ClientID(key) {
		t.Errorf("client = %q, want the key's %q", got, workspace.ClientID(key))
	}
	if got := clientIDFor(workspace.Info{User: "alice", Client: "0123abcd"}, key); got != "0123abcd" {
		t.Errorf("client = %q, want the one the workspace derived", got)
	}
}

// A refusal with a reason ends the connection and says why.
func TestARefusedRunSaysWhy(t *testing.T) {
	const why = "run 0123abcd of account alice already has a live connection\n  fix: close it"
	ws := startWorkspaceWith(t, &silentWorkspace{global: func(*ssh.Request) (bool, []byte) {
		return false, []byte(why)
	}})

	err := openQuery(t).announceRun(t.Context(), ws.dial(t))
	if err == nil || !strings.Contains(err.Error(), why) {
		t.Fatalf("err = %v, want the workspace's reason", err)
	}
}
