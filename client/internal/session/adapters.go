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

func (s shareRegistrar) Share(localPath string, words workspace.ShareWords) (exportPath, file string, err error) {
	share, err := s.registry.Register(localPath, words)
	if err != nil {
		return "", "", err
	}
	// A share's words are fixed when it is registered: its filesystem is built
	// with them and never replaced (ADR 0046). A mount asking for others is
	// refused before anything is created for it.
	if err := refuseOtherWords(share, words); err != nil {
		return "", "", err
	}
	s.shares.remember(share.ExportPath, share.LocalPath, share.Words)
	if s.changed != nil {
		s.changed()
	}
	return share.ExportPath, share.File, nil
}

// refuseOtherWords refuses a mount whose words differ from those the share
// already has. Words a mount left unset are not compared.
func refuseOtherWords(share *nfsserve.Share, asked workspace.ShareWords) error {
	if !asked.Owner.Set {
		asked.Owner = share.Words.Owner
	}
	if asked.Watch == workspace.WatchUnset {
		asked.Watch = share.Words.Watch
	}
	diffs := share.Words.Differences(asked)
	if len(diffs) == 0 {
		return nil
	}
	var have, want []string
	for _, d := range diffs {
		have = append(have, d.Key+"="+d.A)
		want = append(want, d.Key+"="+d.B)
	}
	return fmt.Errorf("rewrite: %s is already shared with %s and this mount asks for %s; a share's words are fixed while the session runs\n"+
		"  fix: give every mount of %[1]s the same words, or remove the containers using it and run docker remote restart",
		share.LocalPath, strings.Join(have, ","), strings.Join(want, ","))
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
