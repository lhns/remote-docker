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

func TestDirList(t *testing.T) {
	if got := dirList(" /a/keys, /b/keys ,,", "/fallback"); !reflect.DeepEqual(got, []string{"/a/keys", "/b/keys"}) {
		t.Errorf("got %v", got)
	}
	if got := dirList(" , ", "/fallback"); !reflect.DeepEqual(got, []string{"/fallback"}) {
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
