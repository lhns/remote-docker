// The adapters between this package's neighbours.
//
// Each one exists because the packages on either side are deliberately not
// coupled: ports does not know about ssh, fswatch does not know about the NFS
// server, and rewrite does not know about either. The conversions are small and
// they belong together, away from the session's own logic.

package session

import (
	"context"
	"net"

	"github.com/lhns/remote-docker/internal/client/nfsserve"
	"github.com/lhns/remote-docker/internal/client/ports"
	"github.com/lhns/remote-docker/internal/client/proxy"
	"github.com/lhns/remote-docker/internal/client/sshx"
)

// shareRegistrar adapts the NFS registry to the rewriter's Sharer.
//
// It is also where a newly shared directory becomes a watched one: every bind
// rewrite funnels through here, so the watcher learns about a share the moment
// it exists rather than up to a reconcile interval later.
type shareRegistrar struct {
	registry *nfsserve.Registry
	changed  func()
}

func (s shareRegistrar) Share(localPath string) (string, error) {
	share, err := s.registry.Register(localPath)
	if err != nil {
		return "", err
	}
	if s.changed != nil {
		s.changed()
	}
	return share.ExportPath, nil
}

// sshForwarder adapts the SSH client to the port manager's Forwarder.
type sshForwarder struct{ client *sshx.Client }

func (f sshForwarder) Forward(local, remote string) (ports.Forward, error) {
	fwd, err := f.client.Forward(local, remote)
	if err != nil {
		return nil, err
	}
	return forwardAdapter{fwd}, nil
}

type forwardAdapter struct{ fwd *sshx.Forward }

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
