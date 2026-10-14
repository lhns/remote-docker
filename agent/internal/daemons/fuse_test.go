package daemons

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

// fuseDocker answers the two questions warnIfNoFuseOverlayfs asks.
type fuseDocker struct {
	execOut string
	execErr error
}

func (f fuseDocker) Line(_ context.Context, args ...string) (string, error) {
	switch args[0] {
	case "exec":
		return f.execOut, f.execErr
	case "inspect":
		return "docker:28-dind", nil
	}
	return "", errors.New("unexpected " + strings.Join(args, " "))
}
func (fuseDocker) Run(context.Context, string, ...string) error      { return nil }
func (fuseDocker) Output(context.Context, ...string) ([]byte, error) { return nil, nil }

func fuseWarning(f fuseDocker) string {
	var buf bytes.Buffer
	m := &Manager{
		Log:    slog.New(slog.NewTextHandler(&buf, nil)),
		docker: func(string) docker { return f },
	}
	m.warnIfNoFuseOverlayfs(&Daemon{Account: "alice"})
	return buf.String()
}

func TestWarnsWhenTheDaemonImageHasNoFuseOverlayfs(t *testing.T) {
	got := fuseWarning(fuseDocker{execOut: "no"})
	for _, want := range []string{"fuse-overlayfs", "WORKSPACE_DIND_IMAGE", "docker:28-dind"} {
		if !strings.Contains(got, want) {
			t.Errorf("warning %q lacks %q", got, want)
		}
	}
}

func TestSaysNothingWhenFuseOverlayfsIsThereOrCannotBeAsked(t *testing.T) {
	for name, f := range map[string]fuseDocker{
		"present":  {execOut: "yes"},
		"cannot":   {execErr: errors.New("container is not running")},
		"nonsense": {execOut: "???"},
	} {
		if got := fuseWarning(f); got != "" {
			t.Errorf("%s: logged %q", name, got)
		}
	}
}
