package sshd

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/lhns/remote-docker/core/enrol"
)

// workspace-enrol is answered by the agent itself, before any shell, on every
// platform. Raw JSON, so this compiles against an agent that has none of it.
func TestWorkspaceEnrolIsAnsweredByTheAgent(t *testing.T) {
	w := startRevokeWorkspace(t)
	key := newSigner(t)
	w.enrol(t, "bob", key)

	s, err := w.dial(t, "bob", key).NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	s.Stdin = strings.NewReader(`{"op":"whoami"}`)
	out, err := s.Output(enrol.EnrolCommand)
	if err != nil {
		t.Fatalf("%s: %v", enrol.EnrolCommand, err)
	}
	var reply struct {
		Whoami struct{ Account string }
	}
	if err := json.Unmarshal(out, &reply); err != nil || reply.Whoami.Account != "bob" {
		t.Fatalf("reply %q: %v", out, err)
	}
}
