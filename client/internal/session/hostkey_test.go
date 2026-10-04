package session

import (
	"net"
	"os"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/lhns/remote-docker/client/internal/config"
	"github.com/lhns/remote-docker/core-client/keys"
)

// The address every WSL machine shares.
const wslAddr = "172.18.12.79:2222"

func hostKeyPair(t *testing.T) (keys.HostKey, ssh.PublicKey) {
	t.Helper()
	hk, err := keys.NewHostKey()
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.ParsePrivateKey(hk.Private)
	if err != nil {
		t.Fatal(err)
	}
	return hk, signer.PublicKey()
}

func tcpAddr(t *testing.T, addr string) net.Addr {
	t.Helper()
	a, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// A rebuilt machine at an address known_hosts already holds another key for:
// what `machine rebuild`, and `rm` then `create`, left behind.
func TestARebuiltMachineIgnoresKnownHosts(t *testing.T) {
	t.Setenv("REMOTE_DOCKER_STATE_DIR", t.TempDir())

	_, previous := hostKeyPair(t)
	stale := knownhosts.Line([]string{knownhosts.Normalize(wslAddr)}, previous) + "\n"
	if err := os.WriteFile(config.KnownHostsPath(), []byte(stale), 0o600); err != nil {
		t.Fatal(err)
	}

	built, current := hostKeyPair(t)
	cfg := config.Config{
		Name: "dev", Port: 2222,
		Machine: &config.Machine{Backend: "wsl", Name: "dev", HostKey: built.Public},
	}
	check, err := hostKeyRule(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, addr := range []string{wslAddr, "172.24.110.158:2222"} {
		if err := check(addr, tcpAddr(t, addr), current); err != nil {
			t.Errorf("the key the machine was built with was refused at %s: %v", addr, err)
		}
	}

	// Anything else at that address is not this machine, and the message
	// says which machine and what to do.
	_, other := hostKeyPair(t)
	err = check(wslAddr, tcpAddr(t, wslAddr), other)
	if err == nil {
		t.Fatal("a key the machine was not built with was accepted")
	}
	for _, want := range []string{`"dev"`, ssh.FingerprintSHA256(other), "machine rebuild dev", "port 2222"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q:\n%v", want, err)
		}
	}

	// Nothing learned from the network, either way.
	if got, _ := os.ReadFile(config.KnownHostsPath()); string(got) != stale {
		t.Errorf("known_hosts changed under a pinned machine:\n%s", got)
	}
}

// Everything without a pinned key keeps known_hosts, a machine built before
// keys were made here included: trusted on first use, refused when it changes.
func TestWithoutAPinnedKeyKnownHostsDecides(t *testing.T) {
	for name, cfg := range map[string]config.Config{
		"a workspace":               {Name: "box", Host: "box.example", Port: 2222},
		"an older machine's record": {Name: "dev", Port: 2222, Machine: &config.Machine{Backend: "wsl", Name: "dev"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("REMOTE_DOCKER_STATE_DIR", t.TempDir())

			check, err := hostKeyRule(cfg)
			if err != nil {
				t.Fatal(err)
			}
			_, first := hostKeyPair(t)
			if err := check(wslAddr, tcpAddr(t, wslAddr), first); err != nil {
				t.Fatalf("first contact was refused: %v", err)
			}
			if got, _ := os.ReadFile(config.KnownHostsPath()); !strings.Contains(string(got), "172.18.12.79") {
				t.Errorf("first contact was not recorded:\n%s", got)
			}
			_, changed := hostKeyPair(t)
			if err := check(wslAddr, tcpAddr(t, wslAddr), changed); err == nil || !strings.Contains(err.Error(), "CHANGED") {
				t.Errorf("a changed key = %v, want it refused", err)
			}
		})
	}
}

// A record that pins nothing readable must not fall back to trusting anyone.
func TestAnUnreadablePinIsRefused(t *testing.T) {
	cfg := config.Config{Name: "dev", Machine: &config.Machine{Backend: "wsl", Name: "dev", HostKey: "not a key"}}
	if _, err := hostKeyRule(cfg); err == nil {
		t.Error("an unreadable host key record was accepted")
	}
}
