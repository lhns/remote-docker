package session

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/lhns/remote-docker/client/internal/config"
)

type closerFunc func() error

func (f closerFunc) Close() error { return f() }

// A machine that is held but cannot be located is released, and the failure
// is returned rather than panicking on the hold the return statement cleared.
func TestDialReleasesTheHoldWhenLocateFails(t *testing.T) {
	closed := 0
	hold, locate := machineHold, machineLocate
	t.Cleanup(func() { machineHold, machineLocate = hold, locate })
	machineHold = func(context.Context, string, string) (io.Closer, error) {
		return closerFunc(func() error { closed++; return nil }), nil
	}
	notRunning := errors.New("starting the wsl machine \"dev\": not running")
	machineLocate = func(context.Context, string, string, int) (string, error) { return "", notRunning }

	cfg := config.Config{Host: "127.0.0.1", Port: 2222, User: "alice",
		Machine: &config.Machine{Backend: "wsl", Name: "dev"}}
	client, held, err := dial(t.Context(), cfg, nil, nil)
	if !errors.Is(err, notRunning) || client != nil || held != nil {
		t.Fatalf("dial = %v, %v, %v", client, held, err)
	}
	if closed != 1 {
		t.Errorf("the hold was closed %d times, want 1", closed)
	}
}
