package workspace

import "fmt"

// Default bases for the uid -> reverse-tunnel-port mapping. They are
// overridable per deployment (WORKSPACE_UID_BASE / WORKSPACE_PORT_BASE) but
// both sides must agree, which is why they travel together in a Mapping.
const (
	DefaultUIDBase  = 10000
	DefaultPortBase = 30000
)

// MaxPort is the highest port number a reverse tunnel can bind.
const MaxPort = 65535

// Mapping converts between a workspace account's uid and the loopback port its
// reverse tunnel occupies inside the workspace container.
//
// The mapping is deliberately a pure function of the uid rather than an
// allocation: it needs no coordination between users, cannot collide, and --
// the property that turned out to matter -- is stable, so a dropped tunnel
// reconnects to the same endpoint instead of forcing a remount.
type Mapping struct {
	UIDBase  int
	PortBase int
}

// DefaultMapping is the mapping used when a deployment overrides nothing.
func DefaultMapping() Mapping {
	return Mapping{UIDBase: DefaultUIDBase, PortBase: DefaultPortBase}
}

// Validate reports whether the mapping itself is usable, independent of any
// particular uid.
func (m Mapping) Validate() error {
	if m.UIDBase < 0 {
		return fmt.Errorf("workspace: uid base %d is negative", m.UIDBase)
	}
	if m.PortBase < 1 || m.PortBase > MaxPort {
		return fmt.Errorf("workspace: port base %d is not a valid port", m.PortBase)
	}
	return nil
}

// PortForUID returns the reverse-tunnel port belonging to uid.
//
// A uid below the base has no port: it belongs to a base-image account, not to
// an enrolled workspace user, and mapping it would hand out a port below
// PortBase that another deployment may be using for something else.
func (m Mapping) PortForUID(uid int) (int, error) {
	if err := m.Validate(); err != nil {
		return 0, err
	}
	if uid < m.UIDBase {
		return 0, fmt.Errorf("workspace: uid %d is below the workspace uid base %d", uid, m.UIDBase)
	}
	port := m.PortBase + uid - m.UIDBase
	if port > MaxPort {
		return 0, fmt.Errorf("workspace: uid %d maps to port %d, above the maximum %d", uid, port, MaxPort)
	}
	return port, nil
}

// UIDForPort is the inverse of PortForUID. The server agent uses it to answer
// "whose port is this?" when deciding whether to honour a tcpip-forward
// request, so it must agree with PortForUID exactly.
func (m Mapping) UIDForPort(port int) (int, error) {
	if err := m.Validate(); err != nil {
		return 0, err
	}
	if port < m.PortBase || port > MaxPort {
		return 0, fmt.Errorf("workspace: port %d is outside the workspace range %d-%d", port, m.PortBase, MaxPort)
	}
	return m.UIDBase + port - m.PortBase, nil
}

// OwnsPort reports whether the account with this uid is entitled to bind port.
//
// This is the whole of the port-ownership policy. Under the old sshd-based
// server the equivalent rule was a permitlisten="..." string generated into
// each user's authorized_keys and enforced by sshd; expressing it as a
// predicate means it can be tested directly and cannot be mis-generated.
func (m Mapping) OwnsPort(uid, port int) bool {
	want, err := m.PortForUID(uid)
	return err == nil && want == port
}
