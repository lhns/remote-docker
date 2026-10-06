package main

import (
	"bytes"
	"cmp"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/lhns/remote-docker/client/internal/config"
	"github.com/lhns/remote-docker/core/enrol"
	"github.com/lhns/remote-docker/core/workspace"
)

// `remote token|user|key`, and `create --token`'s redeem, against a workspace
// that answers one canned reply, or that predates account management and has a
// shell answer 127. Raw JSON on
// purpose: these tests compile against a client that has none of it.

type manageServer struct {
	addr    string
	hostKey ssh.PublicKey
	reply   string // the JSON reply
	status  uint32 // the exit status; 127 is an agent that predates it
	got     chan map[string]any

	// Refuses the first refuse logins that are not a token's; negative is all.
	refuse int32
	logins atomic.Int32

	// info answers workspace-info; empty is a valid answer for alice.
	// Set before the first connection: serve reads it unlocked.
	info string
}

func startManageServer(t *testing.T, reply string, status uint32) *manageServer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	m := &manageServer{hostKey: signer.PublicKey(), reply: reply, status: status, got: make(chan map[string]any, 1)}
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(c ssh.ConnMetadata, _ ssh.PublicKey) (*ssh.Permissions, error) {
			if !strings.HasPrefix(c.User(), enrol.LoginPrefix) {
				if n := m.logins.Add(1); m.refuse < 0 || n <= m.refuse {
					return nil, errors.New("no such account")
				}
			}
			return &ssh.Permissions{}, nil
		},
	}
	cfg.AddHostKey(signer)

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	m.addr = l.Addr().String()
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go m.serve(conn, cfg)
		}
	}()
	return m
}

func (m *manageServer) serve(conn net.Conn, cfg *ssh.ServerConfig) {
	_, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	for newCh := range chans {
		ch, chReqs, err := newCh.Accept()
		if err != nil {
			continue
		}
		go func() {
			for req := range chReqs {
				var payload struct{ Command string }
				_ = ssh.Unmarshal(req.Payload, &payload)
				ok := req.Type == "exec" && (payload.Command == enrol.EnrolCommand || payload.Command == enrol.RedeemCommand ||
					payload.Command == workspace.InfoCommand)
				_ = req.Reply(ok, nil)
				if !ok {
					continue
				}
				if payload.Command == workspace.InfoCommand {
					_, _ = ch.Write([]byte(cmp.Or(m.info, "WORKSPACE_USER=alice\nWORKSPACE_NFS_PORT=20001\n")))
					_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
					_ = ch.Close()
					return
				}
				if m.status == 127 {
					_, _ = ch.Stderr().Write([]byte("bash: line 1: workspace-enrol: command not found\n"))
				} else {
					var got map[string]any
					_ = json.NewDecoder(ch).Decode(&got)
					m.got <- got
					_, _ = ch.Write([]byte(m.reply + "\n"))
				}
				_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{m.status}))
				_ = ch.Close()
				return
			}
		}()
	}
}

// withManageServer configures workspace "main" as bob at the server.
func withManageServer(t *testing.T, m *manageServer) {
	t.Helper()
	host, port, _ := net.SplitHostPort(m.addr)
	n, _ := strconv.Atoi(port)
	withConfig(t, &config.File{
		Default:    "main",
		Workspaces: map[string]config.Workspace{"main": {Host: host, Port: n, User: "bob"}},
	})
	t.Setenv("REMOTE_DOCKER_STATE_DIR", t.TempDir())
}

func TestTokenCreatePrintsAnInviteForThisWorkspace(t *testing.T) {
	m := startManageServer(t, `{"token":{"id":"abcdefgh","token":"abcdefgh.AAAAAAAAAAAAAAAAAAAAAA","account":"bob","expires":"2026-10-06T12:00:00Z"}}`, 0)
	withManageServer(t, m)

	out, err := runOut(t, "remote", "token", "create", "--note", "phone")
	if err != nil {
		t.Fatalf("token create: %v\n%s", err, out)
	}
	if got := <-m.got; got["op"] != "token.create" || got["note"] != "phone" || got["account"] != nil {
		t.Errorf("sent %v", got)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "docker remote create main --token rdt1.") ||
		lines[1] != "single use, for account bob, expires 2026-10-06 12:00 UTC" {
		t.Fatalf("printed %q", out)
	}
	invite, err := enrol.ParseInvite(strings.Fields(lines[0])[5])
	if err != nil {
		t.Fatal(err)
	}
	if invite.URL != "ssh://"+m.addr || invite.HostKey != ssh.FingerprintSHA256(m.hostKey) ||
		invite.Account != "bob" || invite.Token != "abcdefgh.AAAAAAAAAAAAAAAAAAAAAA" {
		t.Errorf("invite %+v", invite)
	}
}

// A machine's entry holds a placeholder address, so its invite would send the
// device to its own loopback; refused before the workspace mints a token.
func TestTokenCreateRefusesAMachine(t *testing.T) {
	withConfig(t, &config.File{
		Default: "dev",
		Workspaces: map[string]config.Workspace{"dev": {
			Host: machinePlaceholderHost, Port: 2222, User: "bob",
			Machine: &config.Machine{Backend: "wsl", Name: "dev"},
		}},
	})
	t.Setenv("REMOTE_DOCKER_STATE_DIR", t.TempDir())

	err := run(t, "remote", "token", "create")
	if err == nil || !strings.Contains(err.Error(), "no other device can reach") {
		t.Fatalf("got %v", err)
	}
	requireFixLine(t, err, "other devices reach")
}

func TestAnOldAgentPredatesAccountManagement(t *testing.T) {
	withManageServer(t, startManageServer(t, "", 127))
	for _, args := range [][]string{{"token", "ls"}, {"user", "ls"}, {"key", "ls"}} {
		err := run(t, append([]string{"remote"}, args...)...)
		if err == nil || !strings.HasPrefix(err.Error(), "this workspace's agent predates account management") {
			t.Errorf("%v: %v", args, err)
			continue
		}
		requireFixLine(t, err, "upgrade")
	}
}

func TestARefusalIsOneLineAndAFix(t *testing.T) {
	withManageServer(t, startManageServer(t,
		`{"error":{"code":"denied","msg":"only an admin can remove an account (you are bob)","fix":"ask an admin"}}`, 0))
	err := run(t, "remote", "user", "rm", "carol")
	if err == nil || !strings.HasPrefix(err.Error(), "only an admin can remove an account (you are bob)") {
		t.Fatalf("got %v", err)
	}
	requireFixLine(t, err, "ask an admin")
}

func TestUserRemoveSaysWhatWasKeptAndWhatIsStillTrue(t *testing.T) {
	m := startManageServer(t,
		`{"notices":[{"code":"","msg":"carol is still named in WORKSPACE_ADMINS, so whoever is enrolled as carol next is an admin","fix":"remove carol from WORKSPACE_ADMINS"}]}`, 0)
	withManageServer(t, m)
	out, err := runOut(t, "remote", "user", "rm", "carol", "-f")
	if err != nil {
		t.Fatal(err)
	}
	if got := <-m.got; got["op"] != "user.rm" || got["account"] != "carol" || got["force"] != true {
		t.Errorf("sent %v", got)
	}
	for _, want := range []string{"removed carol, keeping their uid and storage", "  fix: remove carol from WORKSPACE_ADMINS"} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q lacks %q", out, want)
		}
	}
}

func TestUserRemovePurgeAsksForItAndSaysSo(t *testing.T) {
	m := startManageServer(t, `{}`, 0)
	withManageServer(t, m)
	out, err := runOut(t, "remote", "user", "rm", "carol", "--purge")
	if err != nil {
		t.Fatal(err)
	}
	if got := <-m.got; got["op"] != "user.rm" || got["account"] != "carol" || got["purge"] != true || got["force"] != nil {
		t.Errorf("sent %v", got)
	}
	if want := "removed carol and their storage, keeping only their uid"; !strings.Contains(out, want) {
		t.Errorf("output %q lacks %q", out, want)
	}
}

func TestUserListShowsKeysPerSource(t *testing.T) {
	withManageServer(t, startManageServer(t, `{"users":[
		{"name":"alice","uid":2000,"sources":[{"dir":"/etc/workspace/authorized_keys.d","operator":true,"keys":1},{"dir":"/etc/workspace/enrolled_keys.d","keys":2}],"admin":true,"state":"connected"},
		{"name":"carol","admin":true,"state":"not enrolled"}]}`, 0))
	out, err := runOut(t, "remote", "user", "ls")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	want := [][]string{
		{"ACCOUNT", "UID", "KEYS", "ADMIN", "STATE"},
		{"alice", "2000", "2", "enrolled,", "1", "operator", "yes", "connected"},
		{"carol", "-", "-", "yes", "not", "enrolled"},
	}
	for i, w := range want {
		if i >= len(lines) || strings.Join(strings.Fields(lines[i]), " ") != strings.Join(w, " ") {
			t.Fatalf("printed %q", out)
		}
	}
}

// Windows PowerShell 5.1 writes `remote enroll > key.pub` as UTF-16 with a
// byte order mark, and some editors put a UTF-8 one before the first line.
func TestKeyAddReadsAFileWithAByteOrderMark(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))) + " alice@laptop"

	utf16le := []byte{0xFF, 0xFE}
	for _, r := range line + "\r\n" {
		utf16le = append(utf16le, byte(r), 0)
	}
	for name, data := range map[string][]byte{
		"utf-8":    append([]byte{0xEF, 0xBB, 0xBF}, line+"\r\n"...),
		"utf-16le": utf16le,
	} {
		got, key, err := readPublicKey(bytes.NewReader(data), "-")
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if got != line || ssh.FingerprintSHA256(key) != ssh.FingerprintSHA256(signer.PublicKey()) {
			t.Errorf("%s: read %q", name, got)
		}
	}
}
