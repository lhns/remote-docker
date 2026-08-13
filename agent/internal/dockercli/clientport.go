package dockercli

// Which reverse-tunnel port a machine's existing volumes were built for.
//
// A managed volume carries the port in its driver options, and Docker volume
// options are IMMUTABLE: a volume cannot be re-pointed at a different port, so
// one that outlives the port it was made for can never be mounted again. The
// mount fails with "connection refused" against an address nothing explains.
//
// So the volumes, not the agent's own record, are the durable statement of what
// port a machine needs. This reads it back, and accounts.Ports asks before
// choosing a port for a machine it has forgotten.

import (
	"context"
	"strconv"
	"strings"

	"github.com/lhns/remote-docker/core/workspace"
)

// ClientPorts answers which port an account's machine expects, from the volumes
// that machine created.
type ClientPorts struct {
	// Host is the daemon to ask, as a -H value, resolved per call because a
	// per-account daemon may not exist yet when this type is built.
	Host func(account string) (string, error)
}

// For returns the port this machine's volumes were built for, or 0.
//
// Zero for every reason: no volumes, no daemon, a daemon that will not answer,
// options that do not parse. The caller treats all of them the same way and
// chooses a port as it always did, which is a working session whose volumes
// need rebuilding rather than no session at all.
func (c ClientPorts) For(ctx context.Context, account, client string) int {
	if client == "" || c.Host == nil {
		return 0
	}
	host, err := c.Host(account)
	if err != nil {
		return 0
	}
	cli := CLI{Host: host}

	// Both labels, because the owner alone is not enough once one account is
	// used from two machines: each stamps its own client, and taking the other
	// machine's port would move the failure rather than fix it.
	names, err := cli.Line(ctx, "volume", "ls", "--quiet",
		"--filter", "label="+workspace.ManagedLabel+"="+workspace.ManagedShare,
		"--filter", "label="+workspace.ClientLabel+"="+client)
	if err != nil || strings.TrimSpace(names) == "" {
		return 0
	}

	args := append([]string{"volume", "inspect", "--format", `{{index .Options "o"}}`},
		strings.Fields(names)...)
	out, err := cli.Line(ctx, args...)
	if err != nil {
		return 0
	}
	return firstPort(strings.Split(out, "\n"))
}

// firstPort reads the port out of the driver options, and reports the one the
// most volumes agree on.
//
// Agreement matters because a machine may hold volumes from more than one era.
// The majority is the set that would otherwise need rebuilding, so it is the
// one worth keeping.
func firstPort(options []string) int {
	counts := map[int]int{}
	for _, o := range options {
		if port := portOf(o); port != 0 {
			counts[port]++
		}
	}

	best, bestCount := 0, 0
	for port, n := range counts {
		// Ties break towards the lower port so the answer does not depend on
		// map order, which would make a workspace hand out different ports on
		// identical input.
		if n > bestCount || (n == bestCount && port < best) {
			best, bestCount = port, n
		}
	}
	return best
}

// portOf reads port= out of one volume's option string.
//
// Field by field, never a substring search: the option string contains BOTH
// "port=" and "mountport=", so anything looking for "port=" inside it finds
// whichever comes first in the text rather than the one that was asked for.
func portOf(options string) int {
	for _, field := range strings.Split(options, ",") {
		name, value, ok := strings.Cut(strings.TrimSpace(field), "=")
		if !ok || name != "port" {
			continue
		}
		port, err := strconv.Atoi(value)
		if err != nil {
			return 0
		}
		return port
	}
	return 0
}
