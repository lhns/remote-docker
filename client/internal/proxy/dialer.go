package proxy

import (
	"context"
	"io"

	"github.com/lhns/remote-docker/core-client/tunnelclient"
	"github.com/lhns/remote-docker/core/workspace"
)

// SSHDialer opens Docker connections over an SSH client.
type SSHDialer struct {
	Client *tunnelclient.Client
}

// DialDocker opens one stream to the workspace daemon, through the command
// both ends agree on (ADR 0010 has why it is a command).
func (d *SSHDialer) DialDocker(context.Context) (io.ReadWriteCloser, error) {
	return d.Client.OpenStream(workspace.DialStdioCommand)
}
