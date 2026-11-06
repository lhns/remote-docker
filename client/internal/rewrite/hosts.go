package rewrite

import (
	"encoding/json"
	"fmt"
	"strings"
)

// HostAlias is the name a container uses for this machine, Docker Desktop's.
const HostAlias = "host.docker.internal"

// SetHostGateway sets the address HostAlias points at; empty turns it off.
func (r *Rewriter) SetHostGateway(addr string) { r.hostGateway.Store(&addr) }

// HostGateway is the address HostAlias points at, empty when off.
func (r *Rewriter) HostGateway() string {
	if p := r.hostGateway.Load(); p != nil {
		return *p
	}
	return ""
}

// addHostAlias gives a container HostAlias, pointing at the gateway where the
// host forwards listen (ADR 0057). The address, not `host-gateway`: the daemon
// resolves that word from its own settings, which may name another address.
//
// Left alone when the feature is off, when the container already names the
// alias (a Docker Desktop compose file often does, and keeps what it asked
// for), and when its network is the workspace's own (`host`) or another
// container's (`container:<id>`), which the daemon refuses to combine with
// extra hosts.
func (r *Rewriter) addHostAlias(hostConfig map[string]json.RawMessage, changed *bool) error {
	gateway := r.HostGateway()
	if gateway == "" {
		return nil
	}

	var network string
	if raw, ok := hostConfig["NetworkMode"]; ok {
		// Not a string is the daemon's to report.
		_ = json.Unmarshal(raw, &network)
	}
	if network == "host" || strings.HasPrefix(network, "container:") {
		return nil
	}

	var hosts []string
	if raw, ok := hostConfig["ExtraHosts"]; ok && string(raw) != "null" {
		if err := json.Unmarshal(raw, &hosts); err != nil {
			return nil
		}
	}
	for _, h := range hosts {
		name, _, _ := strings.Cut(strings.ReplaceAll(h, "=", ":"), ":")
		if strings.EqualFold(name, HostAlias) {
			return nil
		}
	}

	encoded, err := json.Marshal(append(hosts, HostAlias+":"+gateway))
	if err != nil {
		return fmt.Errorf("rewrite: encoding ExtraHosts: %w", err)
	}
	hostConfig["ExtraHosts"] = encoded
	*changed = true
	return nil
}
