package sshd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/lhns/remote-docker/agent/internal/daemons"
	"github.com/lhns/remote-docker/core-agent/accounts"
	"github.com/lhns/remote-docker/core/workspace"
)

// A run's identity (ADR 0050), over a real SSH conversation: the run request,
// workspace-info and the reverse forward are three requests whose ORDER is the
// subject, so they go through the server rather than around it.

const (
	runA = "00112233445566778899aabbccddeeff"
	runB = "ffeeddccbbaa99887766554433221100"
)

type runWorkspace struct {
	addr       string
	ports      *accounts.Ports
	alice, bob ssh.Signer
}

// startRunWorkspace enrols alice and bob with a key each, alice's clients
// ephemeral, on the shared daemon so a forward binds in this namespace.
func startRunWorkspace(t *testing.T) *runWorkspace {
	t.Helper()
	w := &runWorkspace{alice: newSigner(t), bob: newSigner(t)}

	keysDir := t.TempDir()
	for name, key := range map[string]ssh.Signer{"alice": w.alice, "bob": w.bob} {
		if err := os.WriteFile(filepath.Join(keysDir, name+".pub"), ssh.MarshalAuthorizedKey(key.PublicKey()), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	store := accounts.New(keysDir, t.TempDir(), workspace.DefaultMapping(), fakeProvisioner{}, nil)
	if err := store.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	w.ports = &accounts.Ports{Dir: t.TempDir(), Mapping: workspace.DefaultMapping()}

	s, err := New(Config{
		Accounts:  store,
		Mapping:   workspace.DefaultMapping(),
		Daemons:   daemons.Shared(""),
		Ports:     w.ports,
		Ephemeral: map[string]bool{"alice": true},
		HostKeys:  []ssh.Signer{newSigner(t)},
	})
	if err != nil {
		t.Fatal(err)
	}
	s.query = func(context.Context, string, ...string) (string, error) { return "", errors.New("no daemon here") }

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.ServeListener(l) }()
	t.Cleanup(func() { _ = s.Close(); _ = l.Close() })
	w.addr = l.Addr().String()
	return w
}

func newSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

// dial connects, and names run when it is not empty.
func (w *runWorkspace) dial(t *testing.T, user string, key ssh.Signer, run string) *ssh.Client {
	t.Helper()
	c, err := ssh.Dial("tcp", w.addr, &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(key)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if run != "" {
		if ok, why := sendRun(t, c, run); !ok {
			t.Fatalf("run %s refused: %s", run, why)
		}
	}
	return c
}

func sendRun(t *testing.T, c *ssh.Client, run string) (bool, string) {
	t.Helper()
	ok, reply, err := c.SendRequest(workspace.RunRequest, true, []byte(run))
	if err != nil {
		t.Fatal(err)
	}
	return ok, string(reply)
}

// needSessions skips where the agent refuses every session (session_other.go).
func needSessions(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("sessions are served on Linux only")
	}
}

// info runs workspace-info, failing with what the workspace said.
func info(t *testing.T, c *ssh.Client) workspace.Info {
	t.Helper()
	got, stderr, err := tryInfo(c)
	if err != nil {
		t.Fatalf("workspace-info: %v: %s", err, stderr)
	}
	return got
}

func tryInfo(c *ssh.Client) (workspace.Info, string, error) {
	session, err := c.NewSession()
	if err != nil {
		return workspace.Info{}, "", err
	}
	defer func() { _ = session.Close() }()
	var stdout, stderr bytes.Buffer
	session.Stdout, session.Stderr = &stdout, &stderr
	if err := session.Run(workspace.InfoCommand); err != nil {
		return workspace.Info{}, stderr.String(), err
	}
	got, err := workspace.ParseInfo(&stdout)
	return got, stderr.String(), err
}

// host binds the reverse forward as a hosting client does: on the port info
// reported, which is 0 for a run that has none yet.
func host(c *ssh.Client, port int) (net.Listener, error) {
	return c.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
}

// recorded reports whether clientports holds a port for this client.
func (w *runWorkspace) recorded(t *testing.T, account, client string) bool {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(w.ports.Dir, "clientports"))
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Contains(string(b), account+":"+client+":")
}

func (w *runWorkspace) run(run string) string {
	return workspace.EphemeralClientID(w.alice.PublicKey().Marshal(), run)
}

func TestARunIsNamedOncePerConnection(t *testing.T) {
	w := startRunWorkspace(t)
	c := w.dial(t, "alice", w.alice, runA)

	if ok, why := sendRun(t, c, runB); ok || !strings.Contains(why, "already named its run") {
		t.Errorf("a second run id on one connection: ok=%v, %q", ok, why)
	}
	if ok, why := sendRun(t, w.dial(t, "alice", w.alice, ""), "not-a-run"); ok || why == "" {
		t.Errorf("a malformed run id: ok=%v, %q; want a refusal with a reason", ok, why)
	}

	needSessions(t)
	if got := info(t, c).Client; got != w.run(runA) {
		t.Errorf("client = %q, want %q", got, w.run(runA))
	}
}

// Before its run is named an ephemeral connection has no client, so nothing per
// client is served: Ports.For would answer the account's base port.
func TestRequestsNeedingAClientWaitForTheRun(t *testing.T) {
	w := startRunWorkspace(t)
	c := w.dial(t, "alice", w.alice, "")

	if l, err := host(c, 0); err == nil {
		_ = l.Close()
		t.Error("a reverse forward was allowed before the run was named")
	}

	needSessions(t)
	if _, stderr, err := tryInfo(c); err == nil || !strings.Contains(stderr, "named no run") {
		t.Errorf("workspace-info before the run: err=%v, stderr %q", err, stderr)
	}
}

// An account not listed is what it was: acknowledged, keyed on the key, and
// given its port by workspace-info.
func TestAMachineAccountIgnoresTheRun(t *testing.T) {
	needSessions(t)
	w := startRunWorkspace(t)
	c := w.dial(t, "bob", w.bob, runA)
	if ok, why := sendRun(t, c, runB); !ok {
		t.Fatalf("bob's second run request was refused: %s", why)
	}

	if got := info(t, c); got.Client != "" || got.NFSPort == 0 {
		t.Errorf("bob's info: client %q, port %d; want no client and a port", got.Client, got.NFSPort)
	}
	if !w.recorded(t, "bob", workspace.ClientID(w.bob.PublicKey().Marshal())) {
		t.Error("bob's port is not recorded against his key")
	}
}

// `remote status` with no session running is a run of its own each time.
// Asking for info allocates nothing; only binding the forward does.
func TestInfoAllocatesNoPortForARun(t *testing.T) {
	needSessions(t)
	w := startRunWorkspace(t)
	for _, run := range []string{runA, runB} {
		if port := info(t, w.dial(t, "alice", w.alice, run)).NFSPort; port != 0 {
			t.Errorf("run %s was told port %d before binding anything", run, port)
		}
		if w.recorded(t, "alice", w.run(run)) {
			t.Errorf("asking for info allocated run %s a port", run)
		}
	}
}

// The background session hosts; a one-off command presents the same run while
// it lives, and is the same client on the same port.
func TestAQueryJoinsALiveRun(t *testing.T) {
	needSessions(t)
	w := startRunWorkspace(t)
	hosting := w.dial(t, "alice", w.alice, runA)
	l, err := host(hosting, info(t, hosting).NFSPort)
	if err != nil {
		t.Fatalf("the hosting connection was refused its forward: %v", err)
	}
	defer func() { _ = l.Close() }()

	query := w.dial(t, "alice", w.alice, runA)
	if got, want := info(t, query).NFSPort, l.Addr().(*net.TCPAddr).Port; got != want {
		t.Errorf("a query of the live run was told port %d, want the hosting one's %d", got, want)
	}
}

// One port per run and one holder per port: a second hosting connection for
// the run is refused, and so is another run asking for this run's port.
func TestASecondHostingConnectionIsRefused(t *testing.T) {
	needSessions(t)
	w := startRunWorkspace(t)
	first := w.dial(t, "alice", w.alice, runA)
	l, err := host(first, info(t, first).NFSPort)
	if err != nil {
		t.Fatalf("the first hosting connection was refused: %v", err)
	}
	defer func() { _ = l.Close() }()
	port := l.Addr().(*net.TCPAddr).Port

	for _, c := range []struct {
		run   string
		asked int
	}{{runA, 0}, {runA, port}, {runB, port}} {
		second := w.dial(t, "alice", w.alice, "")
		if ok, _ := sendRun(t, second, c.run); !ok {
			continue // refused at the run, which also keeps it out
		}
		if l2, err := host(second, c.asked); err == nil {
			_ = l2.Close()
			t.Errorf("run %s bound port %d while the first held %d", c.run, c.asked, port)
		}
	}
}
