package main

import (
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/lhns/remote-docker/client/internal/config"
	"github.com/lhns/remote-docker/client/internal/session"
	"github.com/lhns/remote-docker/core/enrol"
)

// `remote create --token` saves nothing unless the key was enrolled.

func requireNoWorkspace(t *testing.T, name string) {
	t.Helper()
	file, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := file.Workspaces[name]; ok {
		t.Errorf("workspace %q was saved by a create that failed", name)
	}
}

// There is no stdin form: `-` is not an invite, and says so.
func TestATokenThatIsNotAnInviteIsRefused(t *testing.T) {
	withConfig(t, nil)
	for _, token := range []string{"-", "", "abcdefgh.secret", "rdt1.nope"} {
		err := run(t, "remote", "create", "ws", "--token", token)
		if err == nil || !strings.Contains(err.Error(), "not an enrolment invite") {
			t.Errorf("--token %q: %v", token, err)
			continue
		}
		requireFixLine(t, err, "invite")
	}
	requireNoWorkspace(t, "ws")
}

func TestAFailedRedemptionSavesNothing(t *testing.T) {
	withConfig(t, nil)
	t.Setenv("REMOTE_DOCKER_STATE_DIR", t.TempDir())

	// A port nothing listens on: the redeem fails at the dial.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	id, secret, err := enrol.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	invite := enrol.Invite{URL: "ssh://" + addr, HostKey: "SHA256:x", Account: "alice", Token: id + "." + secret}
	if err := run(t, "remote", "create", "ws", "--token", invite.String()); err == nil {
		t.Fatal("a redemption against nothing succeeded")
	}
	requireNoWorkspace(t, "ws")
}

func TestARedemptionRefusalSaysWhatToDo(t *testing.T) {
	err := redeemError(session.ErrPredatesTokens, "wss://ws.example/")
	if err == nil || !strings.Contains(err.Error(), "the workspace at wss://ws.example/ predates enrolment tokens") {
		t.Fatalf("an old workspace: %v", err)
	}
	requireFixLine(t, err, "remote enroll")

	err = redeemError(enrol.Refused, "wss://ws.example/")
	if !errors.Is(err, enrol.Refused) {
		t.Fatalf("a refused token: %v", err)
	}
	requireFixLine(t, err, "ask for a new one")
}
