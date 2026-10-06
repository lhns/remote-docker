package main

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
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
	withConfig(t, nil)
	t.Setenv("REMOTE_DOCKER_STATE_DIR", t.TempDir())
	refusal, _ := json.Marshal(enrol.RedeemReply{Error: enrol.Refused})
	refused := startManageServer(t, string(refusal), 0)
	if err := run(t, "remote", "create", "ws", "--no-context", "--token", redeemInvite(t, refused)); err == nil || err.Error() != enrol.Refused.Error() {
		t.Fatalf("a refused redeem: %v", err)
	}
	requireNoWorkspace(t, "ws")

	m := startManageServer(t, `{"account":"alice","created":true}`, 0)
	out, err := runOut(t, "remote", "create", "ws", "--no-context", "--token", redeemInvite(t, m))
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

func redeemInvite(t *testing.T, m *manageServer) string {
	t.Helper()
	id, secret, err := enrol.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	return enrol.Invite{URL: "ssh://" + m.addr, HostKey: ssh.FingerprintSHA256(m.hostKey),
		Account: "alice", Token: id + "." + secret}.String()
}

// A config that cannot be written refuses before the token is spent.
func TestAnUnwritableConfigRefusesBeforeRedeeming(t *testing.T) {
	withConfig(t, nil)
	t.Setenv("REMOTE_DOCKER_STATE_DIR", t.TempDir())
	// A home below a regular file: no directory can be made there.
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(blocker, "home")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	m := startManageServer(t, `{"account":"alice","created":true}`, 0)
	err := run(t, "remote", "create", "ws", "--no-context", "--token", redeemInvite(t, m))
	if err == nil {
		t.Fatal("create --token succeeded with nowhere to save")
	}
	requireFixLine(t, err, "writable")
	select {
	case got := <-m.got:
		t.Errorf("the workspace saw a redeem: %v", got)
	default:
	}
}

// Every setting given survives, the account is the one enrolled, and a value
// with a space stays one argument.
func TestRecreateKeepsTheFlagsGiven(t *testing.T) {
	for _, c := range []struct {
		args []string
		want string
	}{{
		args: []string{"--host", "wss://ws.example/", "--ca-file", "ca.pem", "--insecure", "--port", "443"},
		want: "create ws --host wss://ws.example/ --ca-file ca.pem --insecure --port 443 --user bob",
	}, {
		args: []string{"--host", "ssh://ws.example:2222", "--user", "alice",
			"--consistency", "read=cached,write=back", "--watch", "partial",
			"--endpoint", `C:\Users\alice\My State\docker.sock`},
		want: `create ws --host ssh://ws.example:2222 --consistency read=cached,write=back ` +
			`--endpoint "C:\Users\alice\My State\docker.sock" --watch partial --user bob`,
	}} {
		var f workspaceFlags
		f.register(&cobra.Command{}, "")
		if err := f.set.Parse(c.args); err != nil {
			t.Fatal(err)
		}
		if got := recreate("ws", f, "bob"); got != c.want {
			t.Errorf("%q:\n got %q\nwant %q", c.args, got, c.want)
		}
	}
}

// A save that fails after the redeem says how to make the entry without the
// spent token.
func TestAFailedSaveAfterRedeemPrintsTheCreateToRun(t *testing.T) {
	withConfig(t, nil)
	t.Setenv("REMOTE_DOCKER_STATE_DIR", t.TempDir())
	saveConfig = func(config.File, string) error { return errors.New("config: disk full") }
	t.Cleanup(func() { saveConfig = config.Save })

	m := startManageServer(t, `{"account":"alice","created":true}`, 0)
	err := run(t, "remote", "create", "ws", "--no-context", "--token", redeemInvite(t, m))
	if err == nil {
		t.Fatal("create succeeded with a failing save")
	}
	if !strings.Contains(err.Error(), "enrolled as account alice, but workspace \"ws\" was not saved") {
		t.Errorf("message: %v", err)
	}
	requireFixLine(t, err, "create ws --host ssh://"+m.addr+" --user alice")
}
