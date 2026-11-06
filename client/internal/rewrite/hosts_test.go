package rewrite

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
)

func extraHosts(t *testing.T, r *Rewriter, body string) []string {
	t.Helper()
	out, err := r.ContainerCreate(context.Background(), []byte(body))
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	var payload struct {
		HostConfig struct{ ExtraHosts []string }
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatal(err)
	}
	return payload.HostConfig.ExtraHosts
}

func TestTheHostAliasIsAddedOnlyWhereItCanWork(t *testing.T) {
	const alias = "host.docker.internal:172.17.0.1"
	for _, tc := range []struct {
		name, gateway, body string
		want                []string
	}{
		{"added", "172.17.0.1", `{"HostConfig":{}}`, []string{alias}},
		{"beside the user's own", "172.17.0.1", `{"HostConfig":{"ExtraHosts":["db:10.0.0.9"]}}`, []string{"db:10.0.0.9", alias}},
		{"a null list", "172.17.0.1", `{"HostConfig":{"ExtraHosts":null}}`, []string{alias}},
		{"no HostConfig at all", "172.17.0.1", `{"Image":"alpine"}`, []string{alias}},
		{"a user-defined network", "172.17.0.1", `{"HostConfig":{"NetworkMode":"appnet"}}`, []string{alias}},
		{"bridge", "172.17.0.1", `{"HostConfig":{"NetworkMode":"bridge"}}`, []string{alias}},
		{"already present as host-gateway", "172.17.0.1", `{"HostConfig":{"ExtraHosts":["host.docker.internal:host-gateway"]}}`, []string{"host.docker.internal:host-gateway"}},
		{"already present with its own address", "172.17.0.1", `{"HostConfig":{"ExtraHosts":["host.docker.internal:10.1.2.3"]}}`, []string{"host.docker.internal:10.1.2.3"}},
		{"already present, upper case", "172.17.0.1", `{"HostConfig":{"ExtraHosts":["Host.Docker.Internal=10.1.2.3"]}}`, []string{"Host.Docker.Internal=10.1.2.3"}},
		{"host network", "172.17.0.1", `{"HostConfig":{"NetworkMode":"host"}}`, nil},
		{"another container's network", "172.17.0.1", `{"HostConfig":{"NetworkMode":"container:abc123"}}`, nil},
		{"the setting is off", "", `{"HostConfig":{}}`, nil},
		{"off, and the user's own are untouched", "", `{"HostConfig":{"ExtraHosts":["db:10.0.0.9"]}}`, []string{"db:10.0.0.9"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := extraHosts(t, &Rewriter{HostGateway: tc.gateway}, tc.body)
			if !slices.Equal(got, tc.want) {
				t.Errorf("ExtraHosts = %v, want %v", got, tc.want)
			}
		})
	}
}

// With the feature off the body is returned exactly as it arrived: a client
// that does not use it changes nothing in any container.
func TestTheBodyIsUntouchedWithTheFeatureOff(t *testing.T) {
	body := `{"HostConfig":{"NetworkMode":"host"},"Image":"alpine"}`
	out, err := (&Rewriter{}).ContainerCreate(context.Background(), []byte(body))
	if err != nil || string(out) != body {
		t.Errorf("got %q, %v", out, err)
	}
}
