package workspace

import (
	"sort"
	"strconv"
	"strings"
)

// RequestedPorts is what the user asked for, keyed by container port as the
// daemon spells it: "80/tcp".
//
// A LIST, because one container port may be published more than once
// (`-p 8080:80 -p 9090:80`). Which requested number ends up in front of which
// port the daemon assigned does not matter: every one of them fronts the same
// container port, so any pairing gives the same thing.
type RequestedPorts map[string][]int

// ContainerPort is the daemon's spelling of a container port, which is the key
// on both sides of this label.
func ContainerPort(port int, proto string) string {
	if proto == "" {
		proto = "tcp"
	}
	return strconv.Itoa(port) + "/" + strings.ToLower(proto)
}

// IsTCP reports whether a protocol is TCP, and an ABSENT one is: both the
// Docker API and the CLI treat `-p 8080:80` as tcp.
//
// Here rather than beside each caller because three of them decide it: what may
// be moved to another port, what gets a local listener, and what a container
// port is called. A fourth would guess.
func IsTCP(proto string) bool {
	return proto == "" || strings.EqualFold(proto, "tcp")
}

// ProtoOf is the protocol half of a container port as the daemon spells it,
// "80/tcp", and empty when it carries none.
func ProtoOf(containerPort string) string {
	_, proto, _ := strings.Cut(containerPort, "/")
	return proto
}

// Add records another port asked for on one container port.
func (r RequestedPorts) Add(containerPort string, port int) {
	r[containerPort] = append(r[containerPort], port)
}

// At is the port requested for the nth publication of a container port, or 0
// when there was none.
//
// The caller supplies the index: the daemon reports the ports it assigned in no
// defined order, so both sides sort and count. See the type comment for why
// counting is enough.
func (r RequestedPorts) At(containerPort string, index int) int {
	list := r[containerPort]
	if index < 0 || index >= len(list) {
		return 0
	}
	return list[index]
}

// String renders the label value: 80/tcp=8080;9090,443/tcp=8443.
//
// Sorted throughout, so a container created twice from one command carries the
// same label both times and a diff of two containers means something.
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
		ports := append([]int(nil), r[k]...)
		sort.Ints(ports)

		numbers := make([]string, 0, len(ports))
		for _, p := range ports {
			numbers = append(numbers, strconv.Itoa(p))
		}
		parts = append(parts, k+"="+strings.Join(numbers, ";"))
	}
	return strings.Join(parts, ",")
}

// MaxRequestedPorts bounds how many local listeners one label can ask for.
//
// The label is read off a CONTAINER, and with one daemon for everybody
// (ADR 0012) any account can create a container carrying any label, so this is
// the number of sockets somebody else can ask this machine to open. Well above
// anything real: a published range is the largest legitimate case, and a
// container publishing more than a thousand ports to one machine is not a case
// this exists to serve.
const MaxRequestedPorts = 1024

// ParseRequestedPorts reads the label back.
//
// Anything unreadable is SKIPPED rather than failing the whole label: this is
// read while deciding which local port to open, and one malformed entry must
// not cost a container every forward it has. A label written by a newer client
// with a form this one does not know reads as the entries it does know. Ports
// past MaxRequestedPorts are dropped the same way.
func ParseRequestedPorts(label string) RequestedPorts {
	out := RequestedPorts{}
	count := 0
	for _, entry := range strings.Split(label, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)

		for _, number := range strings.Split(value, ";") {
			if count >= MaxRequestedPorts {
				break
			}
			port, err := strconv.Atoi(strings.TrimSpace(number))
			if err != nil || port < 1 || port > MaxPort {
				continue
			}
			out.Add(key, port)
			count++
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
