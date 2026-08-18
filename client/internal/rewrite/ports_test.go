package rewrite

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/lhns/remote-docker/core/workspace"
)

var errTaken = errors.New("something on this machine is listening there")

// create runs a body through the rewriter and returns the HostConfig and the
// labels it produced.
func create(t *testing.T, r *Rewriter, body string) (map[string]any, map[string]string) {
	t.Helper()

	out, err := r.ContainerCreate(context.Background(), []byte(body))
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	var payload struct {
		HostConfig map[string]any    `json:"HostConfig"`
		Labels     map[string]string `json:"Labels"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("decoding the rewritten body: %v", err)
	}
	return payload.HostConfig, payload.Labels
}

// hostPorts reads back what the daemon is being asked to publish.
func hostPorts(t *testing.T, hostConfig map[string]any, containerPort string) []string {
	t.Helper()

	bindings, ok := hostConfig["PortBindings"].(map[string]any)
	if !ok {
		t.Fatalf("no PortBindings in %+v", hostConfig)
	}
	list, ok := bindings[containerPort].([]any)
	if !ok {
		t.Fatalf("no binding for %s in %+v", containerPort, bindings)
	}
	var out []string
	for _, entry := range list {
		m, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("a binding is not an object: %+v", entry)
		}
		port, _ := m["HostPort"].(string)
		out = append(out, port)
	}
	return out
}

// The point of the whole thing: the daemon is asked for any port, and the
// number the user typed is recorded for the client to open locally.
func TestAPublishedPortIsLeftToTheDaemon(t *testing.T) {
	r, _, _ := newRewriter()

	hostConfig, labels := create(t, r, `{"HostConfig":{"PortBindings":{"80/tcp":[{"HostIp":"","HostPort":"8080"}]}}}`)

	if got := hostPorts(t, hostConfig, "80/tcp"); len(got) != 1 || got[0] != "" {
		t.Errorf("HostPort = %+v, want it left to the daemon", got)
	}
	if got := labels[PortsLabel]; got != "80/tcp=8080" {
		t.Errorf("the ports label is %q, so nothing knows the user asked for 8080", got)
	}
}

// HostIp is the user's and is not ours to change: it says which interface of
// the workspace to publish on.
func TestRemappingKeepsTheAddressTheUserAskedFor(t *testing.T) {
	r, _, _ := newRewriter()

	hostConfig, _ := create(t, r, `{"HostConfig":{"PortBindings":{"80/tcp":[{"HostIp":"127.0.0.1","HostPort":"8080"}]}}}`)

	bindings := hostConfig["PortBindings"].(map[string]any)
	entry := bindings["80/tcp"].([]any)[0].(map[string]any)
	if entry["HostIp"] != "127.0.0.1" {
		t.Errorf("HostIp = %v, want the one the user gave", entry["HostIp"])
	}
}

// Three cases are deliberately left alone. Each is listed here because a
// silent skip and a bug look the same from outside.
func TestWhatIsNotRemapped(t *testing.T) {
	cases := map[string]struct {
		body          string
		containerPort string
		want          string
	}{
		"the user already asked for any port": {
			body:          `{"HostConfig":{"PortBindings":{"80/tcp":[{"HostPort":""}]}}}`,
			containerPort: "80/tcp",
			want:          "",
		},
		"udp, which the tunnel cannot carry": {
			body:          `{"HostConfig":{"PortBindings":{"53/udp":[{"HostPort":"5353"}]}}}`,
			containerPort: "53/udp",
			want:          "5353",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r, _, _ := newRewriter()
			hostConfig, labels := create(t, r, tc.body)

			if got := hostPorts(t, hostConfig, tc.containerPort); len(got) != 1 || got[0] != tc.want {
				t.Errorf("HostPort = %+v, want %q untouched", got, tc.want)
			}
			if labels[PortsLabel] != "" {
				t.Errorf("it was recorded as remapped: %q", labels[PortsLabel])
			}
		})
	}
}

// Several bindings for one container port cannot be paired back: the daemon
// reports the assigned ports in no defined order, so which one was 8080 and
// which was 9090 is unanswerable. Left where they are, and they collide as
// they did before.
func TestSeveralBindingsForOnePortAreLeftAlone(t *testing.T) {
	r, _, _ := newRewriter()

	hostConfig, labels := create(t, r,
		`{"HostConfig":{"PortBindings":{"80/tcp":[{"HostPort":"8080"},{"HostPort":"9090"}]}}}`)

	got := hostPorts(t, hostConfig, "80/tcp")
	if len(got) != 2 || got[0] != "8080" || got[1] != "9090" {
		t.Errorf("HostPort = %+v, want both untouched", got)
	}
	if labels[PortsLabel] != "" {
		t.Errorf("they were recorded as remapped: %q", labels[PortsLabel])
	}
}

// The clash moves to this machine, so this is where it is reported, in the
// daemon's own words because that is what it replaces.
func TestATakenLocalPortIsRefused(t *testing.T) {
	r, _, _ := newRewriter()
	r.LocalPortFree = func(int) error { return errTaken }

	_, err := r.ContainerCreate(context.Background(),
		[]byte(`{"HostConfig":{"PortBindings":{"80/tcp":[{"HostPort":"8080"}]}}}`))
	if err == nil {
		t.Fatal("the container was created with a local port that cannot be opened")
	}
	if !strings.Contains(err.Error(), "Bind for 127.0.0.1:8080 failed: port is already allocated") {
		t.Errorf("the error does not read like the daemon refusing: %v", err)
	}
}

// The label the ports manager reads back has to survive the round trip, since
// it is the only record of what was asked for.
func TestTheLabelSurvivesTheRoundTrip(t *testing.T) {
	r, _, _ := newRewriter()

	_, labels := create(t, r,
		`{"HostConfig":{"PortBindings":{"80/tcp":[{"HostPort":"8080"}],"443/tcp":[{"HostPort":"8443"}]}}}`)

	got := workspace.ParseRequestedPorts(labels[PortsLabel])
	if got[workspace.ContainerPort(80, "tcp")] != 8080 || got[workspace.ContainerPort(443, "tcp")] != 8443 {
		t.Errorf("the label reads back as %+v", got)
	}
}

// Labels the caller set are preserved: a compose project relies on its own.
func TestRemappingKeepsTheCallersLabels(t *testing.T) {
	r, _, _ := newRewriter()

	_, labels := create(t, r,
		`{"Labels":{"com.docker.compose.project":"demo"},"HostConfig":{"PortBindings":{"80/tcp":[{"HostPort":"8080"}]}}}`)

	if labels["com.docker.compose.project"] != "demo" {
		t.Errorf("a label the caller set was lost: %+v", labels)
	}
	if labels[PortsLabel] == "" {
		t.Error("the ports label was not added")
	}
}
