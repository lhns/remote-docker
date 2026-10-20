package ports

import "github.com/lhns/remote-docker/core/workspace"

// Rule decides, for one session, which containers it forwards and on which
// local numbers. The forward manager and the response rewrite of docker ps,
// port and inspect both ask it, so what is opened here and what is shown
// cannot disagree (ADR 0008).
type Rule struct {
	// User is the account this session logs in as. On a shared daemon (ADR 0012)
	// a container labelled with another owner is not ours.
	User string

	// ClientID is this machine's id within the account (ADR 0029).
	ClientID string
}

// Owned reports whether this session forwards a container at all.
func (r Rule) Owned(c Container) bool {
	return c.Labels[workspace.OwnerLabel] == r.User
}

// LocalPorts is the numbers requested for one published port on this machine,
// or nil to use the published one. Only the machine that asked has them
// (ADR 0029); several when one container port was published more than once.
func (r Rule) LocalPorts(c Container, p Published) []int {
	if c.Labels[workspace.ClientLabel] != r.ClientID {
		return nil
	}
	return workspace.ParseRequestedPorts(c.Labels[workspace.PortsLabel])[workspace.ContainerPort(p.PrivatePort, p.Type)]
}

// Local is the ports opened here for a published port, never empty.
func (r Rule) Local(c Container, p Published) []int {
	return orPublished(r.LocalPorts(c, p), p)
}

// orPublished is asked, or with no request the published port, which is its
// own.
func orPublished(asked []int, p Published) []int {
	if len(asked) > 0 {
		return asked
	}
	return []int{p.PublicPort}
}
