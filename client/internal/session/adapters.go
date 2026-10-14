package session

import (
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/lhns/remote-docker/client/internal/ports"
	"github.com/lhns/remote-docker/client/internal/proxy"
	"github.com/lhns/remote-docker/core-client/nfsserve"
	"github.com/lhns/remote-docker/core-client/tunnelclient"
	"github.com/lhns/remote-docker/core/workspace"
)

// shareRegistrar adapts the NFS registry to the rewriter's Sharer. Every bind
// rewrite passes through it, so it records the share for later sessions and
// tells the watcher at once.
type shareRegistrar struct {
	registry *nfsserve.Registry
	shares   *shareStore
	changed  func()
}

func (s shareRegistrar) Share(localPath string, mount workspace.MountWords) (exportPath, file string, err error) {
	share, err := s.registry.Register(localPath, mount.ShareWords)
	if err != nil {
		return "", "", err
	}
	// A share's mode and words are fixed when it is registered: its volume and
	// filesystem are built with them and never replaced (ADR 0046). A mount
	// asking for others is refused before anything is created for it.
	if err := refuseOtherWords(share, s.registry.ClaimMode(share, mount.Mode), mount); err != nil {
		return "", "", err
	}
	s.shares.remember(share.ExportPath, share.LocalPath, share.Words)
	if s.changed != nil {
		s.changed()
	}
	return share.ExportPath, share.File, nil
}

// refuseOtherWords refuses a mount that resolves to a mode or words other than
// the share already has. Both sides are concrete: the rewriter settles every
// word before it asks. The mode is named first, because the watch it implies
// differs with it and would otherwise take the blame.
func refuseOtherWords(share *nfsserve.Share, haveMode workspace.Mode, asked workspace.MountWords) error {
	var have, want []string
	if haveMode != asked.Mode {
		have, want = append(have, haveMode.String()), append(want, asked.Mode.String())
	}
	for _, d := range share.Words.Differences(asked.ShareWords) {
		have, want = append(have, d.Key+"="+d.A), append(want, d.Key+"="+d.B)
	}
	if len(have) == 0 {
		return nil
	}
	return fmt.Errorf("rewrite: %[1]s is already shared with %[2]s and this mount resolves to %[3]s; a share's mode and words are fixed while the session runs\n"+
		"  fix: give every mount of %[1]s the same words, or remove the containers using it and run `%[4]s`",
		share.LocalPath, strings.Join(have, ","), strings.Join(want, ","), OurCommand("restart"))
}

// sshForwarder adapts the SSH client to the port manager's Forwarder.
type sshForwarder struct{ client *tunnelclient.Client }

func (f sshForwarder) Forward(network, local, remote string) (ports.Forward, error) {
	// The manager has one path for both protocols (ADR 0038).
	if network == "udp" {
		return newUDPForward(local, remote, f.client.DialRemoteUDP)
	}

	fwd, err := f.client.Forward(local, remote)
	if err != nil {
		return nil, err
	}
	return forwardAdapter{fwd}, nil
}

type forwardAdapter struct{ fwd *tunnelclient.Forward }

func (f forwardAdapter) Close() error        { return f.fwd.Close() }
func (f forwardAdapter) LocalAddr() net.Addr { return f.fwd.Local }

// dockerPorts adapts the API client to the port manager's Docker.
type dockerPorts struct{ api *proxy.APIClient }

func (d dockerPorts) ListContainers(ctx context.Context) ([]ports.Container, error) {
	raw, err := d.api.ListContainers(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ports.Container, 0, len(raw))
	for _, c := range raw {
		container := ports.Container{ID: c.ID, Labels: c.Labels}
		if len(c.Names) > 0 {
			container.Name = c.Names[0]
		}
		for _, p := range c.Ports {
			container.Ports = append(container.Ports, ports.Published{
				PublicPort:  p.PublicPort,
				PrivatePort: p.PrivatePort,
				Type:        p.Type,
			})
		}
		out = append(out, container)
	}
	return out, nil
}
