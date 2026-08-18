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
		for i := range list {
			port, ok := remappable(list[i])
			if !ok {
				continue
			}

			// TCP only, because only TCP gets a local listener. Refusing a
			// container on the strength of a TCP listener holding the number
			// would be refusing it for something unrelated.
			if isTCP(containerPort) && r.LocalPortFree != nil {
				if err := r.LocalPortFree(port); err != nil {
					return nil, fmt.Errorf("bind for 127.0.0.1:%d failed: port is already allocated: %w", port, err)
				}
			}

			// Empty is how the API says "any free port", which is the whole
			// mechanism: the daemon picks, so nobody can be holding it.
			list[i]["HostPort"] = json.RawMessage(`""`)
			requested.Add(containerPort, port)
			*changed = true
		}
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

// isTCP reports whether a container port is TCP, which the daemon spells
// "80/tcp" and, when the protocol is left out, means.
func isTCP(containerPort string) bool {
	_, proto, found := strings.Cut(containerPort, "/")
	return !found || strings.EqualFold(proto, "tcp")
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
