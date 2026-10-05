package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/lhns/remote-docker/core-agent/tokens"
	"github.com/lhns/remote-docker/core/enrol"
)

func runAgent(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := newRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

func tokenEnv(t *testing.T) string {
	t.Helper()
	state := t.TempDir()
	t.Setenv(envStateDir, state)
	for _, env := range []string{envTokensDir, envEnrolledKeys, envHostKeys, envPublicURL} {
		t.Setenv(env, "")
	}
	if err := os.MkdirAll(filepath.Join(state, "enrolled_keys.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	return state
}

func TestTokenCreatePrintsTheCommandThatRedeemsIt(t *testing.T) {
	state := tokenEnv(t)
	t.Setenv(envPublicURL, "wss://ws.example/")

	out, err := runAgent(t, "token", "create", "--account", "Alice", "--note", "laptop")
	if err != nil {
		t.Fatalf("token create: %v\n%s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	fields := strings.Fields(lines[0])
	if len(lines) != 2 || len(fields) != 6 || strings.Join(fields[:5], " ") != "docker remote create ws --token" {
		t.Fatalf("output:\n%s", out)
	}
	if !strings.Contains(lines[1], "single use, for account alice, expires") {
		t.Errorf("second line: %q", lines[1])
	}

	invite, err := enrol.ParseInvite(fields[5])
	if err != nil {
		t.Fatal(err)
	}
	hostKeys, err := loadHostKeys(filepath.Join(state, "host_keys"))
	if err != nil {
		t.Fatal(err)
	}
	if invite.URL != "wss://ws.example/" || invite.Account != "alice" ||
		invite.HostKey != ssh.FingerprintSHA256(hostKeys[0].PublicKey()) {
		t.Errorf("invite %+v", invite)
	}

	id, secret, _ := enrol.ParseToken(invite.Token)
	store := &tokens.Store{Dir: filepath.Join(state, "tokens")}
	if tok, err := store.Check(id, secret); err != nil || tok.Account != "alice" || tok.Note != "laptop" {
		t.Errorf("the stored token: %+v, %v", tok, err)
	}

	out, err = runAgent(t, "token", "ls")
	if err != nil || !strings.Contains(out, id) || !strings.Contains(out, "laptop") {
		t.Errorf("token ls: %v\n%s", err, out)
	}
	if _, err := runAgent(t, "token", "rm", id); err != nil {
		t.Fatal(err)
	}
	if store.Live(id) {
		t.Error("token rm left the token")
	}
}

func TestTokenCreateRefusals(t *testing.T) {
	state := tokenEnv(t)
	for name, args := range map[string][]string{
		"neither": {"token", "create", "--url", "ws://x"},
		"both":    {"token", "create", "--url", "ws://x", "--account", "alice", "--unbound"},
		"no url":  {"token", "create", "--account", "alice"},
		"8 days":  {"token", "create", "--url", "ws://x", "--account", "alice", "--expires", "192h"},
	} {
		if out, err := runAgent(t, args...); err == nil {
			t.Errorf("%s: minted a token:\n%s", name, out)
		}
	}

	if err := os.Remove(filepath.Join(state, "enrolled_keys.d")); err != nil {
		t.Fatal(err)
	}
	_, err := runAgent(t, "token", "create", "--url", "ws://x", "--unbound")
	if err == nil || !strings.Contains(err.Error(), "cannot store keys") {
		t.Errorf("a workspace that cannot store keys minted a token: %v", err)
	}
	if list, _ := (&tokens.Store{Dir: filepath.Join(state, "tokens")}).List(); len(list) != 0 {
		t.Errorf("refusals left tokens: %+v", list)
	}
}

func TestRemoteName(t *testing.T) {
	for in, want := range map[string]string{
		"wss://ws.example/":  "ws",
		"ssh://dev.lan:2222": "dev",
		"build-box":          "build-box",
		"10.0.0.5":           "workspace",
		"ws://10.0.0.5:2280": "workspace",
		"[::1]:2222":         "workspace",
	} {
		if got := remoteName(in); got != want {
			t.Errorf("remoteName(%q) = %q, want %q", in, got, want)
		}
	}
}
