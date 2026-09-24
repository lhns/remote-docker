package tunnel

// The channel types and requests this project adds to SSH. A channel whose
// payload this project defines belongs beside that payload, so the name and its
// version change together.

// KeepAliveRequest probes an idle connection. OpenSSH's name, so a stock sshd
// answers it. A dead tunnel otherwise surfaces as container I/O failing with
// EIO, naming no connection.
const KeepAliveRequest = "keepalive@openssh.com"

// UDPChannelType carries datagrams to a port published inside the workspace. A
// server that does not know it rejects the channel, which is the version check.
const UDPChannelType = "direct-udp@remote-docker.lhns.de"

// ForwardPayload opens a forwarding channel: RFC 4254's direct-tcpip payload,
// reused by UDPChannelType. Marshalled by whichever SSH library the caller has;
// this package imports neither.
type ForwardPayload struct {
	DestAddr string
	DestPort uint32

	OriginAddr string
	OriginPort uint32
}
