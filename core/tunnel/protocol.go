package tunnel

// What SSH itself carries for us: the channel types and requests this project
// adds to the protocol.
//
// A channel name whose PAYLOAD this project defines does not belong here -- it
// belongs beside that payload, so the name and the version it negotiates change
// together. UDPChannelType is the shape to copy: its name, its payload struct
// and its framing (datagram.go) are all in this package, because SSH is what
// defines them.

// KeepAliveRequest probes a connection that is otherwise idle.
//
// OpenSSH's name, because a stock sshd answers it and the agent should not need
// a private one to look alive. A tunnel can be dead for a long time without
// either end noticing, and a dead tunnel means container I/O failing with EIO
// rather than anything that names a connection.
const KeepAliveRequest = "keepalive@openssh.com"

// UDPChannelType carries datagrams to a port published inside the workspace.
//
// SSH forwards TCP and nothing else: direct-tcpip out, forwarded-tcpip back.
// This is the same idea for datagrams, in the namespace SSH keeps for
// extensions, so a server that does not know it REJECTS the channel and the
// client learns the workspace is too old without asking anything first.
//
// The payload is direct-tcpip's, because the question is the same one: which
// address and port inside the workspace, and where from.
const UDPChannelType = "direct-udp@remote-docker.lhns.de"

// ForwardPayload is what a forwarding channel is opened with: which address and
// port inside the workspace, and where from.
//
// RFC 4254 fixed the shape for direct-tcpip and UDPChannelType reuses it,
// because the question does not change with the protocol. Declared here rather
// than at each end for the reason everything else in this file is: two spellings
// of a wire format do not fail to compile, they fail to parse.
//
// Marshalled by whichever SSH library the caller already has. This package
// imports neither.
type ForwardPayload struct {
	DestAddr string
	DestPort uint32

	OriginAddr string
	OriginPort uint32
}
