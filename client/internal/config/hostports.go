package config

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/lhns/remote-docker/core/workspace"
)

// ParseHostPorts reads the host-ports setting: local TCP ports separated by
// commas, sorted and without repeats. Empty is none. A malformed value is an
// error rather than a shorter list, since this decides which of this machine's
// ports every container of the account can reach (ADR 0057).
func ParseHostPorts(s string) ([]int, error) {
	var out []int
	for _, field := range splitList(s) {
		port, err := strconv.Atoi(field)
		if err != nil || port < 1 || port > workspace.MaxPort {
			return nil, fmt.Errorf("host-ports: %q is not a port from 1 to %d", field, workspace.MaxPort)
		}
		out = append(out, port)
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// HostPortList is ParseHostPorts as written back into the setting.
func HostPortList(ports []int) string {
	out := make([]string, len(ports))
	for i, p := range ports {
		out[i] = strconv.Itoa(p)
	}
	return strings.Join(out, ",")
}
