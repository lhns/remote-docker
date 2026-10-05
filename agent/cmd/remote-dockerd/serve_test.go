package main

import (
	"net"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The agent is pid 1 in its container, so an SSH port it cannot bind has to
// end it, where the container's restart policy and log can say why. With the
// WebSocket listener up it hung instead: nothing closed the SSH server that
// listener was serving, and waiting for it never returned.
func TestServeReturnsWhenTheSSHPortIsTaken(t *testing.T) {
	t.Setenv(envEnableDind, "false")
	t.Setenv(envPerUserDind, "false")
	t.Setenv(envStateDir, t.TempDir())

	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = taken.Close() }()

	returned := make(chan error, 1)
	go func() { returned <- serve(taken.Addr().String(), "127.0.0.1:0") }()

	select {
	case err := <-returned:
		if err == nil {
			t.Error("serve returned no error for an SSH port it could not bind")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("serve hung after failing to bind its SSH port")
	}
}

// WORKSPACE_EPHEMERAL_ACCOUNTS names accounts as their key files do, and a
// name that is no account refuses the start, naming the variable.
func TestEphemeralAccounts(t *testing.T) {
	got, err := ephemeralAccounts(" CI, bob ,,")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, map[string]bool{"ci": true, "bob": true}) {
		t.Errorf("got %v, want ci and bob", got)
	}

	if got, err := ephemeralAccounts(""); err != nil || len(got) != 0 {
		t.Errorf("unset gave %v, %v; want none", got, err)
	}

	if _, err := ephemeralAccounts("ci,123"); err == nil || !strings.Contains(err.Error(), envEphemeral) {
		t.Errorf("err = %v, want one naming %s", err, envEphemeral)
	}
}

// The run limit and grace period default when unset, and an unusable value
// refuses the start naming its variable.
func TestEphemeralLimits(t *testing.T) {
	if n, d, err := ephemeralLimits("", ""); err != nil || n != 8 || d != 2*time.Minute {
		t.Errorf("unset gave %d, %v, %v; want 8 and 2m", n, d, err)
	}
	if n, d, err := ephemeralLimits("3", "90s"); err != nil || n != 3 || d != 90*time.Second {
		t.Errorf("3 and 90s gave %d, %v, %v", n, d, err)
	}
	for _, c := range []struct{ max, grace, name string }{
		{"0", "", envEphemeralMax},
		{"many", "", envEphemeralMax},
		{"", "2", envEphemeralGrace},
		{"", "-1m", envEphemeralGrace},
	} {
		if _, _, err := ephemeralLimits(c.max, c.grace); err == nil || !strings.Contains(err.Error(), c.name) {
			t.Errorf("%q, %q: err = %v, want one naming %s", c.max, c.grace, err, c.name)
		}
	}
}
