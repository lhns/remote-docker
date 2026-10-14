package daemons

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/lhns/remote-docker/core/workspace"
)

// imageDocker answers the one question warnUnion asks the parent: the image.
type imageDocker struct{}

func (imageDocker) Line(_ context.Context, args ...string) (string, error) {
	if args[0] == "inspect" {
		return "docker:28-dind", nil
	}
	return "", errors.New("unexpected " + strings.Join(args, " "))
}
func (imageDocker) Run(context.Context, string, ...string) error      { return nil }
func (imageDocker) Output(context.Context, ...string) ([]byte, error) { return nil, nil }

func unionWarning(reported string) string {
	var buf bytes.Buffer
	m := &Manager{
		Log:    slog.New(slog.NewTextHandler(&buf, nil)),
		docker: func(string) docker { return imageDocker{} },
	}
	m.warnUnion(&Daemon{Account: "alice"}, reported)
	return buf.String()
}

// The warning carries the same remedy the client's refusal and the union child
// print, and the image the daemon runs.
func TestWarnsWithTheReasonTheCapabilityGave(t *testing.T) {
	for reported, wants := range map[string][]string{
		workspace.UnionNoBinary: {"fuse-overlayfs", workspace.UnionImageRemedy, "docker:28-dind"},
		workspace.UnionNoDevice: {"/dev/fuse", workspace.UnionDeviceRemedy},
		"":                      {"cannot serve a union"},
	} {
		got := unionWarning(reported)
		for _, want := range wants {
			if !strings.Contains(got, want) {
				t.Errorf("%q: warning %q lacks %q", reported, got, want)
			}
		}
	}
}

func TestSaysNothingWhenTheDaemonCanServeAUnion(t *testing.T) {
	if got := unionWarning(workspace.UnionReady); got != "" {
		t.Errorf("logged %q", got)
	}
}
