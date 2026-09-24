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

// rewritePorts lets the daemon pick each published port and returns, keyed by
// container port, the numbers the user asked for, which the client opens
// locally (ADR 0008).
func (r *Rewriter) rewritePorts(hostConfig map[string]json.RawMessage, changed *bool) (workspace.RequestedPorts, error) {
	raw, ok := hostConfig["PortBindings"]
	if !ok || string(raw) == "null" {
		return nil, nil
	}

	var bindings map[string][]binding
	if err := json.Unmarshal(raw, &bindings); err != nil {
		// The daemon reports a malformed body better.
		return nil, nil
	}

	requested := workspace.RequestedPorts{}
	for containerPort, list := range bindings {
		fixed, keep := fixedPorts(list)
		if len(fixed) == 0 {
			continue
		}

		// TCP only: the check opens a TCP listener.
		if workspace.IsTCP(workspace.ProtoOf(containerPort)) && r.LocalPortFree != nil {
			for _, port := range fixed {
				if err := r.LocalPortFree(port); err != nil {
					return nil, fmt.Errorf("bind for 127.0.0.1:%d failed: port is already allocated: %w", port, err)
				}
			}
		}

		// ONE binding with an empty HostPort ("any"), however many numbers were
		// asked for: two identical "any" bindings get one port that the daemon
		// then fails to bind twice ("address already in use").
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

// fixedPorts is every port asked for by number under one container port, and
// the first such binding, kept so its HostIp survives.
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

// remappable reports the port one binding names, false if it names none. UDP
// is moved like TCP (ADR 0038).
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
