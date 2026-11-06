package sshd

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/lhns/remote-docker/core/logx"
)

func TestFirstIPv4SkipsWhatAForwardCannotBindTo(t *testing.T) {
	for in, want := range map[string]string{
		"172.17.0.1\n":             "172.17.0.1",
		"fd00::1\n172.18.0.1\n":    "172.18.0.1",
		"\n  10.0.0.1 \n":          "10.0.0.1",
		"":                         "",
		"fd00::1\n":                "",
		"0.0.0.0\n":                "",
		"127.0.0.1\n":              "",
		"not an address\n":         "",
		"172.17.0.1/16\n":          "",
		"172.17.0.1\n172.18.0.1\n": "172.17.0.1",
	} {
		if got := firstIPv4(in); got != want {
			t.Errorf("firstIPv4(%q) = %q, want %q", in, got, want)
		}
	}
}

// answer is a docker CLI that reports the same gateway for every daemon, and
// records what it was asked.
func answer(gateway string, hosts *[]string, args *[]string) func(context.Context, string, ...string) (string, error) {
	return func(_ context.Context, host string, a ...string) (string, error) {
		if hosts != nil {
			*hosts = append(*hosts, host)
		}
		if args != nil {
			*args = a
		}
		if gateway == "" {
			return "", errors.New("no such network")
		}
		return gateway + "\n", nil
	}
}

func TestAHostForwardOnTheGatewayIsAllowedOnAnyPort(t *testing.T) {
	for _, port := range []uint32{1, 3000, 8931, 30000, 65535} {
		targets := twoAccounts()
		s := &Server{cfg: Config{Daemons: targets}, query: answer("172.17.0.1", nil, nil)}
		handled, ok := s.allowHostForward(context.Background(), sessionAccount{name: "alice", uid: 10001}, "172.17.0.1", port)
		if !handled || !ok {
			t.Errorf("port %d: handled=%v ok=%v, want both", port, handled, ok)
		}
		// Any connection may name a non-loopback host, and none may start a daemon.
		if len(targets.ensured) != 0 {
			t.Errorf("a host forward started a daemon: %v", targets.ensured)
		}
	}
}

// Port 0 would let the workspace pick a number the client never named, and a
// number past 65535 is no port.
func TestAHostForwardRefusesWhatIsNoPort(t *testing.T) {
	s := &Server{cfg: Config{Daemons: twoAccounts()}, query: answer("172.17.0.1", nil, nil)}
	for _, port := range []uint32{0, 65536} {
		handled, ok := s.allowHostForward(context.Background(), sessionAccount{name: "alice"}, "172.17.0.1", port)
		if !handled || ok {
			t.Errorf("port %d: handled=%v ok=%v, want refused by this branch", port, handled, ok)
		}
	}
}

// Every other host meets the loopback rule unchanged: handled is false, and for
// the loopback names the daemon is not even asked.
func TestOtherHostsAreLeftToTheLoopbackRule(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "::1", "localhost", "", "*", "0.0.0.0", "172.17.0.2", "192.168.1.5", "172.17.0.1.", "example.test"} {
		targets := twoAccounts()
		var args []string
		s := &Server{cfg: Config{Daemons: targets}, query: answer("172.17.0.1", nil, &args)}
		handled, ok := s.allowHostForward(context.Background(), sessionAccount{name: "alice"}, host, 3000)
		if handled || ok {
			t.Errorf("host %q: handled=%v ok=%v, want it left to the loopback rule", host, handled, ok)
		}
		switch host {
		case "127.0.0.1", "::1", "localhost", "", "*", "0.0.0.0", "example.test":
			if len(targets.asked) != 0 || args != nil {
				t.Errorf("host %q: the daemon was consulted (%v %v)", host, targets.asked, args)
			}
		}
	}
}

// A daemon with no bridge gateway has no host forwards to allow.
func TestNoGatewayAllowsNothing(t *testing.T) {
	s := &Server{cfg: Config{Daemons: twoAccounts()}, query: answer("", nil, nil)}
	if handled, ok := s.allowHostForward(context.Background(), sessionAccount{name: "alice"}, "172.17.0.1", 3000); handled || ok {
		t.Errorf("handled=%v ok=%v with no gateway", handled, ok)
	}
}

// The gateway is asked of the asking account's own daemon: bob's gateway is
// not alice's, and an address that is only bob's must not be allowed to her.
func TestTheGatewayIsAskedOfTheAccountsOwnDaemon(t *testing.T) {
	targets := twoAccounts()
	var hosts []string
	s := &Server{cfg: Config{Daemons: targets}}
	s.query = func(_ context.Context, host string, _ ...string) (string, error) {
		hosts = append(hosts, host)
		if strings.Contains(host, "bob") {
			return "172.18.0.1\n", nil
		}
		return "172.17.0.1\n", nil
	}
	ctx := context.Background()
	if handled, ok := s.allowHostForward(ctx, sessionAccount{name: "alice"}, "172.18.0.1", 3000); handled || ok {
		t.Errorf("alice was allowed bob's gateway: handled=%v ok=%v", handled, ok)
	}
	if handled, ok := s.allowHostForward(ctx, sessionAccount{name: "bob"}, "172.18.0.1", 3000); !handled || !ok {
		t.Errorf("bob was refused his own gateway: handled=%v ok=%v", handled, ok)
	}
	for i, want := range []string{targets.byAccount["alice"].Host, targets.byAccount["bob"].Host} {
		if hosts[i] != want {
			t.Errorf("query %d went to %q, want %q", i, hosts[i], want)
		}
	}
}

// Info asks without starting a daemon, like every other field in it, and
// reports the gateway only once the daemon answers.
func TestInfoReportsTheGatewayWithoutStartingADaemon(t *testing.T) {
	targets := twoAccounts()
	targets.missing = true
	targets.err = errors.New("this daemon must not be started here")
	s := &Server{cfg: Config{Daemons: targets}, query: answer("172.17.0.1", nil, nil)}
	if got := s.hostGatewayInfo(context.Background(), "alice"); got != "" {
		t.Errorf("gateway = %q for a daemon that is not up, want empty", got)
	}
	if len(targets.ensured) != 0 {
		t.Errorf("an info query started a daemon: %v", targets.ensured)
	}

	targets.missing = false
	if got := s.hostGatewayInfo(context.Background(), "alice"); got != "172.17.0.1" {
		t.Errorf("gateway = %q, want 172.17.0.1", got)
	}
}

// The client's warning points at the workspace's log, so a running daemon with
// no bridge gateway has to be named there, once however often info is asked.
func TestAMissingGatewayIsLoggedOncePerAccount(t *testing.T) {
	var log strings.Builder
	s := &Server{
		cfg:   Config{Daemons: twoAccounts(), Log: logx.Logger(&log, "", false)},
		query: answer("", nil, nil),
	}
	for range 3 {
		_ = s.hostGatewayInfo(context.Background(), "alice")
	}
	_ = s.hostGatewayInfo(context.Background(), "bob")

	if n := strings.Count(log.String(), "no IPv4 gateway"); n != 2 {
		t.Errorf("logged %d times for two accounts asked four times, want 2:\n%s", n, log.String())
	}
}
