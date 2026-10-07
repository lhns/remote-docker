package main

import (
	"net"
	"path/filepath"
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

// WORKSPACE_EPHEMERAL_ACCOUNTS and WORKSPACE_ADMINS name accounts as their key
// files do, and a name that is no account refuses the start, naming the
// variable.
func TestAccountSet(t *testing.T) {
	got, err := accountSet(envAdmins, " CI, bob ,,")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, map[string]bool{"ci": true, "bob": true}) {
		t.Errorf("got %v, want ci and bob", got)
	}

	if got, err := accountSet(envAdmins, ""); err != nil || len(got) != 0 {
		t.Errorf("unset gave %v, %v; want none", got, err)
	}

	if _, err := accountSet(envEphemeral, "ci,123"); err == nil || !strings.Contains(err.Error(), envEphemeral) {
		t.Errorf("err = %v, want one naming %s", err, envEphemeral)
	}
}

func TestCommaList(t *testing.T) {
	if got := commaList(" /a/keys, /b/keys ,,", []string{"/fallback"}); !reflect.DeepEqual(got, []string{"/a/keys", "/b/keys"}) {
		t.Errorf("got %v", got)
	}
	if got := commaList(" , ", []string{"/fallback"}); !reflect.DeepEqual(got, []string{"/fallback"}) {
		t.Errorf("an empty list gave %v, want the fallback", got)
	}
}

// An enrolled keys directory inside the operator's would have the agent write
// into the operator's keys, so the start is refused, naming both variables.
func TestServeRefusesAnEnrolledDirectoryInsideTheOperators(t *testing.T) {
	t.Setenv(envEnableDind, "false")
	t.Setenv(envPerUserDind, "false")
	state := t.TempDir()
	t.Setenv(envStateDir, state)
	keys := filepath.Join(state, "keys")
	t.Setenv(envKeysDir, keys)
	t.Setenv("WORKSPACE_ENROLLED_KEYS_DIR", filepath.Join(keys, "enrolled"))

	returned := make(chan error, 1)
	go func() { returned <- serve("127.0.0.1:0", "") }()

	select {
	case err := <-returned:
		if err == nil || !strings.Contains(err.Error(), "WORKSPACE_ENROLLED_KEYS_DIR") {
			t.Errorf("err = %v, want one naming WORKSPACE_ENROLLED_KEYS_DIR", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("serve started with the enrolled keys directory inside the operator's")
	}
}

// Removing a run's containers is off unless asked, and a value that is not a
// boolean refuses the start naming its variable.
func TestEphemeralCleanupContainers(t *testing.T) {
	for raw, want := range map[string]bool{"": false, "false": false, "true": true, "1": true} {
		if got, err := envBool(envEphemeralContainers, raw); err != nil || got != want {
			t.Errorf("%q gave %v, %v; want %v", raw, got, err, want)
		}
	}
	if _, err := envBool(envEphemeralContainers, "yes"); err == nil || !strings.Contains(err.Error(), envEphemeralContainers) {
		t.Errorf("err = %v, want one naming %s", err, envEphemeralContainers)
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

// Tokens for non-admins default on, and a value that is not a boolean refuses
// the start naming its variable.
func TestUserTokens(t *testing.T) {
	for raw, want := range map[string]bool{"": true, "true": true, "false": false, "0": false} {
		if got, err := envBoolDefault(envUserTokens, raw, true); err != nil || got != want {
			t.Errorf("%q gave %v, %v; want %v", raw, got, err, want)
		}
	}
	if _, err := envBoolDefault(envUserTokens, "nope", true); err == nil || !strings.Contains(err.Error(), envUserTokens) {
		t.Errorf("err = %v, want one naming %s", err, envUserTokens)
	}
}
