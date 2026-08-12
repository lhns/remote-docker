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

// KeepAliveRequest probes a connection that is otherwise idle.
//
// OpenSSH's name, because a stock sshd answers it and the agent should not need
// a private one to look alive. A tunnel can be dead for a long time without
// either end noticing, and a dead tunnel means container I/O failing with EIO
// rather than anything that names a connection.
const KeepAliveRequest = "keepalive@openssh.com"
