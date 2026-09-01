package tunnel

// What the two ends say to each other.
//
// Every name here is spoken by one binary and understood by the other, which is
// the whole test for belonging in this file: if only one side ever reads it, it
// is that side's business and does not come here.
//
// They are constants rather than an interface because SSH already decided the
// shape: a session is opened and a command is named. What the command MEANS is
// this project's invention, so spell each one here and nowhere else. A second
// spelling on either side does not fail to compile -- the agent falls through
// to running it as a shell command, which exits 127.

// InfoCommand asks the workspace for the parameters this client must agree
// with: its account, its reverse-tunnel port, the daemon it will reach.
//
// Answered from core/workspace, in the same type the client parses with, so the
// two cannot disagree about the format either.
const InfoCommand = "workspace-info"

// DialStdioCommand opens a stream to the workspace's Docker socket.
//
// `docker system dial-stdio` connects stdin and stdout to /var/run/docker.sock,
// so a session running it IS a connection to the daemon. That is what lets the
// client proxy the Docker API without a CLI in the path and without exposing a
// TCP port anywhere (ADR 0010).
//
// The agent does not run the docker CLI to serve this; it dials the socket
// itself. The command is the NAME of a request, not an instruction to execute
// something, and the spelling is docker's so that a stock `ssh` reaches the
// daemon the same way.
const DialStdioCommand = "docker system dial-stdio"

// NotifyCommand carries the client's filesystem changes to be replayed inside
// the workspace, so watchers in containers see edits made on the user's machine
// (ADR 0016).
//
// An agent too old to know it runs `sh -c "workspace-notify"`, which exits 127.
// That is the version check: the client offers, and an agent that cannot do it
// fails in a way the client recognises rather than one it has to ask about
// first.
const NotifyCommand = "workspace-notify"

// CacheCommand carries a delegated share's cache: preparing its union mount,
// filling it, and invalidating what changed on the client (ADR 0044).
//
// Deliberately not NotifyCommand, which promises to carry no content and to
// mutate nothing. This one is a sync, and a channel of its own is what keeps
// that promise true of the other.
//
// The same version check: an agent too old to know it runs
// `sh -c "workspace-cache"` and exits 127, so the client refuses the mode
// naming the workspace rather than discovering it half way through a mount.
const CacheCommand = "workspace-cache"

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
