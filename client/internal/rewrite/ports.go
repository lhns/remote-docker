package rewrite

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/lhns/remote-docker/core/workspace"
)

// binding is one entry of HostConfig.PortBindings, kept generic so a field this
// does not know about survives the round trip.
type binding map[string]json.RawMessage

// rewritePorts hands the published port back to the daemon and records what
// the user asked for.
//
// A published port is bound on the WORKSPACE, so on a daemon shared between
// accounts (ADR 0012) two people running -p 8080:80 collide and the second is
// refused. Nothing needs that number to be 8080 there: the client opens the
// local listener, so the daemon can publish wherever it likes as long as the
// label says which local port belongs in front of it (ADR 0037).
//
// Returns what to record, keyed by container port.
func (r *Rewriter) rewritePorts(hostConfig map[string]json.RawMessage, changed *bool) (workspace.RequestedPorts, error) {
	raw, ok := hostConfig["PortBindings"]
	if !ok || string(raw) == "null" {
		return nil, nil
	}

	var bindings map[string][]binding
	if err := json.Unmarshal(raw, &bindings); err != nil {
		// Not ours to repair: the daemon reports a malformed body better than
		// a guess here would.
		return nil, nil
	}

	requested := workspace.RequestedPorts{}
	for containerPort, list := range bindings {
		fixed, keep := fixedPorts(list)
		if len(fixed) == 0 {
			continue
		}

		// TCP only, because only TCP gets a local listener. Refusing a
		// container on the strength of a TCP listener holding the number would
		// be refusing it for something unrelated.
		if workspace.IsTCP(workspace.ProtoOf(containerPort)) && r.LocalPortFree != nil {
			for _, port := range fixed {
				if err := r.LocalPortFree(port); err != nil {
					return nil, fmt.Errorf("bind for 127.0.0.1:%d failed: port is already allocated: %w", port, err)
				}
			}
		}

		// ONE binding, whatever was asked for, with an empty HostPort: that is
		// how the API says "any free port", and the daemon picks, so nobody can
		// be holding it.
		//
		// One rather than one per number, because two bindings asking for any
		// port are identical and the daemon allocates a single port for them
		// and then fails to bind it twice:
		//
		//	failed to bind host port 0.0.0.0:32778/tcp: address already in use
		//
		// Publishing once costs nothing here: every number the user asked for
		// fronts the same container port, so the client opens all of them in
		// front of the one publication.
		keep["HostPort"] = json.RawMessage(`""`)
		bindings[containerPort] = []binding{keep}

		for _, port := range fixed {
			requested.Add(containerPort, port)
		}
		*changed = true
	}

	if len(requested) == 0 {
		return nil, nil
	}
	encoded, err := json.Marshal(bindings)
	if err != nil {
		return nil, fmt.Errorf("rewrite: encoding PortBindings: %w", err)
	}
	hostConfig["PortBindings"] = encoded
	return requested, nil
}

// fixedPorts is every port asked for by name under one container port, and the
// binding to keep for them.
//
// The kept one is the first that named a port, so its HostIp survives: that is
// the interface of the workspace the user chose to publish on, and it is not
// ours to change.
func fixedPorts(list []binding) ([]int, binding) {
	var ports []int
	var keep binding

	for _, b := range list {
		port, ok := remappable(b)
		if !ok {
			continue
		}
		if keep == nil {
			keep = b
		}
		ports = append(ports, port)
	}
	return ports, keep
}

// remappable reports the port one binding asks for, and whether it may be
// moved.
//
// The only binding left where it is asks for an empty HostPort, which is the
// user asking for any port already.
//
// UDP is moved like TCP even though the tunnel cannot carry it: two accounts
// publishing 53/udp collided on the workspace, and moving it costs nothing the
// client could otherwise have had. It is unreachable from here either way
// (ADR 0038).
func remappable(b binding) (int, bool) {
	var hostPort string
	if err := json.Unmarshal(b["HostPort"], &hostPort); err != nil {
		return 0, false
	}
	if strings.TrimSpace(hostPort) == "" {
		return 0, false
	}

	port, err := strconv.Atoi(hostPort)
	if err != nil || port < 1 || port > workspace.MaxPort {
		return 0, false
	}
	return port, true
}
