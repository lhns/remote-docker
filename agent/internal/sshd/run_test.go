package sshd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/lhns/remote-docker/core-agent/accounts"
	"github.com/lhns/remote-docker/core/cache"
	"github.com/lhns/remote-docker/core/notify"
	"github.com/lhns/remote-docker/core/workspace"
)

// A run's identity (ADR 0050): one key, many runs, each its own client.

const (
	runA = "00112233445566778899aabbccddeeff"
	runB = "ffeeddccbbaa99887766554433221100"
)

// runServer enrols alice and bob with one key each, alice's clients ephemeral.
func runServer(t *testing.T) (*Server, ssh.PublicKey, ssh.PublicKey) {
	t.Helper()
	keysDir := t.TempDir()
	aliceKey, bobKey := generateKey(t), generateKey(t)
	for name, key := range map[string]ssh.PublicKey{"alice": aliceKey, "bob": bobKey} {
		if err := os.WriteFile(filepath.Join(keysDir, name+".pub"), ssh.MarshalAuthorizedKey(key), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	store := accounts.New(keysDir, t.TempDir(), workspace.DefaultMapping(), fakeProvisioner{}, nil)
	if err := store.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	s, err := New(Config{
		Accounts:  store,
		Mapping:   workspace.DefaultMapping(),
		Daemons:   twoAccounts(),
		Ephemeral: map[string]bool{"alice": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	return s, aliceKey, bobKey
}

// connect authenticates one connection, which ends when the test does unless
// the returned cancel ends it first.
func connect(t *testing.T, s *Server, user string, key ssh.PublicKey) (*fakeContext, context.CancelFunc) {
	t.Helper()
	ctx := newFakeContext(user)
	var cancel context.CancelFunc
	ctx.Context, cancel = context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if !s.authenticate(ctx, key) {
		t.Fatalf("%s's own key was refused", user)
	}
	return ctx, cancel
}

func sendRun(s *Server, ctx *fakeContext, run string) (bool, string) {
	ok, reply := s.handleRun(ctx, nil, &ssh.Request{Type: workspace.RunRequest, Payload: []byte(run)})
	return ok, string(reply)
}

func TestARunIsNamedOncePerConnection(t *testing.T) {
	s, key, _ := runServer(t)
	ctx, _ := connect(t, s, "alice", key)

	if ok, why := sendRun(s, ctx, runA); !ok {
		t.Fatalf("the first run id was refused: %s", why)
	}
	account, _ := accountFor(ctx)
	if want := workspace.EphemeralClientID(key.Marshal(), runA); account.Client() != want {
		t.Errorf("client = %q, want %q", account.Client(), want)
	}

	ok, why := sendRun(s, ctx, runB)
	if ok {
		t.Fatal("a second run id on one connection was accepted")
	}
	if !strings.Contains(why, "already named its run") {
		t.Errorf("the refusal says %q", why)
	}
	if again, _ := accountFor(ctx); again.Client() != account.Client() {
		t.Errorf("the refused run changed the client to %q", again.Client())
	}

	if ok, _ := sendRun(s, connectOnly(t, s, key), "not-a-run"); ok {
		t.Error("a malformed run id was accepted")
	}
}

func connectOnly(t *testing.T, s *Server, key ssh.PublicKey) *fakeContext {
	t.Helper()
	ctx, _ := connect(t, s, "alice", key)
	return ctx
}

// Before its run is named, an ephemeral connection has no client, so nothing
// that is per client may be served: not the export's port, not notify, not the
// cache, not the reverse forward.
func TestRequestsNeedingAClientWaitForTheRun(t *testing.T) {
	s, key, _ := runServer(t)
	ctx, _ := connect(t, s, "alice", key)
	account, _ := accountFor(ctx)

	for _, command := range []string{workspace.InfoCommand, notify.Command, cache.Command} {
		if !needsClient(command) {
			t.Errorf("%s does not need a client", command)
		}
	}
	for _, command := range []string{workspace.DialStdioCommand, "", "ls"} {
		if needsClient(command) {
			t.Errorf("%q needs a client; a shell or the daemon does not", command)
		}
	}

	if account.Client() != "" {
		t.Fatalf("client = %q before the run was named", account.Client())
	}

	port, err := workspace.DefaultMapping().PortForUID(account.UID())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := (reversePolicy{s}).Allow(ctx, "127.0.0.1", uint32(port)); ok {
		t.Fatal("a reverse forward was allowed before the run was named")
	}

	if ok, why := sendRun(s, ctx, runA); !ok {
		t.Fatalf("run refused: %s", why)
	}
	if _, ok := (reversePolicy{s}).Allow(ctx, "127.0.0.1", uint32(port)); !ok {
		t.Error("the reverse forward was refused after the run was named")
	}
}

// An account not opted in is what it was: the request is acknowledged and the
// client stays the digest of the key.
func TestAMachineAccountIgnoresTheRun(t *testing.T) {
	s, _, key := runServer(t)
	ctx, _ := connect(t, s, "bob", key)

	if ok, why := sendRun(s, ctx, runA); !ok {
		t.Fatalf("bob's run request was refused: %s", why)
	}
	if ok, why := sendRun(s, ctx, runB); !ok {
		t.Fatalf("bob's second run request was refused: %s", why)
	}
	account, _ := accountFor(ctx)
	if account.Client() != workspace.ClientID(key.Marshal()) {
		t.Errorf("client = %q, want the digest of bob's key", account.Client())
	}
}

// A run is one process, so a second live connection naming it is refused; once
// the first ends, a reconnect for the same run is the same client.
func TestOneLiveConnectionPerRun(t *testing.T) {
	s, key, _ := runServer(t)
	first, closeFirst := connect(t, s, "alice", key)
	if ok, why := sendRun(s, first, runA); !ok {
		t.Fatalf("run refused: %s", why)
	}

	second, closeSecond := connect(t, s, "alice", key)
	ok, why := sendRun(s, second, runA)
	if ok {
		t.Fatal("a second live connection for one run was accepted")
	}
	if !strings.Contains(why, "already has a live connection") || !strings.Contains(why, "\n  fix: ") {
		t.Errorf("the refusal says %q", why)
	}
	// The refused connection ending must not release the first's claim.
	closeSecond()
	time.Sleep(50 * time.Millisecond)
	if ok, _ := sendRun(s, connectOnly(t, s, key), runA); ok {
		t.Fatal("a refused connection ending released the live one's run")
	}

	if ok, why := sendRun(s, connectOnly(t, s, key), runB); !ok {
		t.Errorf("another run of the same key was refused: %s", why)
	}

	closeFirst()
	waitFor(t, func() bool {
		ok, _ := sendRun(s, connectOnly(t, s, key), runA)
		return ok
	})
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for range 200 {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the run was never released after its connection ended")
}
