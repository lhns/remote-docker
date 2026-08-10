package main

import (
	"os"
	"testing"
)

// The variable is the whole reason `workspace create` can write a docker
// context now that the docker LookPath finds may be us.
func TestNoSessionEnvStopsIt(t *testing.T) {
	withArgs(t, []string{"docker", "ps"})
	if !invokingDocker() {
		t.Fatal("the case being suppressed does not hold, so this proves nothing")
	}

	t.Setenv(NoSessionEnv, "1")
	if invokingDocker() {
		t.Errorf("%s did not stop a session being made available", NoSessionEnv)
	}
}

func withArgs(t *testing.T, args []string) {
	t.Helper()
	old := os.Args
	os.Args = args
	t.Cleanup(func() { os.Args = old })
}
