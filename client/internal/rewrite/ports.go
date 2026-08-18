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
		port, ok := remappable(containerPort, list)
		if !ok {
			continue
		}
		// The clash moves here with the port: the number the user typed is
		// bound on this machine now, so this is where it can be taken. In the
		// wording the daemon uses, because that is what it replaces.
		if r.LocalPortFree != nil {
			if err := r.LocalPortFree(port); err != nil {
				return nil, fmt.Errorf("Bind for 127.0.0.1:%d failed: port is already allocated: %w", port, err)
			}
		}

		// Empty is how the API says "any free port", which is the whole
		// mechanism: the daemon picks, so nobody can be holding it.
		list[0]["HostPort"] = json.RawMessage(`""`)
		requested[containerPort] = port
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

// remappable reports the port a binding asks for, and whether it may be moved.
//
// Three are deliberately left where they are:
//
//   - an empty HostPort, where the user already asked for any port;
//   - several bindings for one container port (-p 8080:80 -p 9090:80), because
//     the daemon reports the assigned ports in no defined order and they cannot
//     be paired back to what was asked for;
//   - UDP, because the tunnel carries TCP, so a moved UDP port would be neither
//     reachable nor predictable. Two accounts publishing one UDP port still
//     collide, exactly as they do now.
func remappable(containerPort string, list []binding) (int, bool) {
	if len(list) != 1 {
		return 0, false
	}
	if _, proto, found := strings.Cut(containerPort, "/"); found && !strings.EqualFold(proto, "tcp") {
		return 0, false
	}

	var hostPort string
	if err := json.Unmarshal(list[0]["HostPort"], &hostPort); err != nil {
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
