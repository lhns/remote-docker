package main

import (
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

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

// The account saved is the one the workspace enrolled the key into, and a
// refusal in the reply saves nothing.
func TestARedemptionSavesTheAccountItJoined(t *testing.T) {
	invite := func(m *manageServer) string {
		id, secret, err := enrol.NewToken()
		if err != nil {
			t.Fatal(err)
		}
		return enrol.Invite{URL: "ssh://" + m.addr, HostKey: ssh.FingerprintSHA256(m.hostKey),
			Account: "alice", Token: id + "." + secret}.String()
	}

	withConfig(t, nil)
	t.Setenv("REMOTE_DOCKER_STATE_DIR", t.TempDir())
	refusal, _ := json.Marshal(enrol.RedeemReply{Error: enrol.Refused})
	refused := startManageServer(t, string(refusal), 0)
	if err := run(t, "remote", "create", "ws", "--no-context", "--token", invite(refused)); err == nil || err.Error() != enrol.Refused.Error() {
		t.Fatalf("a refused redeem: %v", err)
	}
	requireNoWorkspace(t, "ws")

	m := startManageServer(t, `{"account":"alice","created":true}`, 0)
	out, err := runOut(t, "remote", "create", "ws", "--no-context", "--token", invite(m))
	if err != nil {
		t.Fatalf("create --token: %v\n%s", err, out)
	}
	if got := <-m.got; got["account"] != nil {
		t.Errorf("a bound token's redeem named an account: %v", got)
	}
	if ws := savedWorkspace(t, "ws"); ws.User != "alice" || ws.Host != "ssh://"+m.addr {
		t.Errorf("saved %+v", ws)
	}
	if !strings.Contains(out, "this machine's key created the account alice") {
		t.Errorf("printed:\n%s", out)
	}
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
