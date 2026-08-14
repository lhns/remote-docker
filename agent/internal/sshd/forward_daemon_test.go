package sshd

import (
	"context"
	"errors"
	"testing"

	"github.com/lhns/remote-docker/agent/internal/daemons"
)

// A reverse forward is bound inside the account's own daemon namespace, so a
// daemon that will not start refuses the forward, and the reason has to survive
// to the caller.
//
// From a real workspace: rd-dind-<account> was crash-looping while every
// `docker run` failed with "tcpip-forward request denied by peer (another
// session for this account may still be open)". No session held that port. The
// agent's log is the only place the actual reason appears, so an error that
// arrived here empty would leave nothing anywhere.
func TestADaemonThatWillNotStartRefusesTheForward(t *testing.T) {
	wontStart := errors.New("dind: container exited with code 1")
	s := &Server{cfg: Config{Daemons: &fakeTargets{
		byAccount: map[string]daemons.Target{"lhns": {NetNSPath: "/proc/1/ns/net"}},
		err:       wontStart,
	}}}

	_, err := s.listenFor(context.Background(), sessionAccount{name: "lhns", uid: 10000}, "127.0.0.1:30000")
	if err == nil {
		t.Fatal("the forward was allowed with no daemon to bind it in")
	}
	if !errors.Is(err, wontStart) {
		t.Errorf("the daemon's failure did not reach the caller: %v", err)
	}
}
