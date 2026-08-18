package workspace

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// RequestedPorts is what the user asked for, keyed by container port as the
// daemon spells it: "80/tcp".
//
// The value is the port they wrote on the left of the colon, which the client
// opens on this machine while the workspace publishes wherever it likes.
type RequestedPorts map[string]int

// ContainerPort is the daemon's spelling of a container port, which is the key
// on both sides of this label.
func ContainerPort(port int, proto string) string {
	if proto == "" {
		proto = "tcp"
	}
	return strconv.Itoa(port) + "/" + strings.ToLower(proto)
}

// String renders the label value: 80/tcp=8080,443/tcp=8443.
//
// Sorted, so a container created twice from one command carries the same label
// both times and a diff of two containers means something.
func (r RequestedPorts) String() string {
	if len(r) == 0 {
		return ""
	}
	keys := make([]string, 0, len(r))
	for k := range r {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, r[k]))
	}
	return strings.Join(parts, ",")
}

// ParseRequestedPorts reads the label back.
//
// Anything unreadable is SKIPPED rather than failing the whole label: this is
// read while deciding which local port to open, and one malformed entry must
// not cost a container every forward it has. A label written by a newer client
// with a form this one does not know reads as the entries it does know.
func ParseRequestedPorts(label string) RequestedPorts {
	out := RequestedPorts{}
	for _, entry := range strings.Split(label, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		port, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || port < 1 || port > MaxPort {
			continue
		}
		out[strings.TrimSpace(key)] = port
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
