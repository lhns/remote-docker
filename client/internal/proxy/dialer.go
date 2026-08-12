package proxy

import (
	"context"
	"github.com/lhns/remote-docker/core/tunnel"
	"io"

	"github.com/lhns/remote-docker/core-client/tunnelclient"
)

// DialStdioCommand reaches the workspace daemon's socket through its own CLI.
//
// `docker system dial-stdio` connects stdin/stdout to /var/run/docker.sock,
// which is exactly what an SSH exec channel provides. It is also what
// DOCKER_HOST=ssh:// uses, so it needs nothing installed and no sshd
// configuration, because the dind image already ships the CLI.
//
// The Go workspace agent will eventually offer a direct channel to the socket
// with no CLI in the path (ADR 0010). Using dial-stdio is what lets the client
// be built and proven against stock sshd first.
const DialStdioCommand = tunnel.DialStdioCommand

// SSHDialer opens Docker connections over an SSH client.
type SSHDialer struct {
	Client *tunnelclient.Client
}

// DialDocker opens one stream to the workspace daemon.
func (d *SSHDialer) DialDocker(context.Context) (io.ReadWriteCloser, error) {
	return d.Client.OpenStream(DialStdioCommand)
}
