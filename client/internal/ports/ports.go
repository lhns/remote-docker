// Package ports makes published container ports reachable on the client.
//
// `docker run -p 8080:80` publishes on the daemon's network, which for
// Docker-in-Docker is the workspace container's own namespace, so nothing on
// this machine can reach it. Watching the daemon's event stream and opening a
// local forward per published port closes that gap without the user having to
// know the ports in advance (ADR 0008).
package ports

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strconv"
	"sync"

	"github.com/lhns/remote-docker/core/logx"
	"github.com/lhns/remote-docker/core/workspace"
)

// Forwarder opens a local listener carrying connections to an address inside
// the workspace.
type Forwarder interface {
	Forward(localAddr, remoteAddr string) (Forward, error)
}

// Forward is one live local listener.
type Forward interface {
	Close() error
	LocalAddr() net.Addr
}

// Container is the daemon's view of a running container.
type Container struct {
	ID     string
	Name   string
	Ports  []Published
	Labels map[string]string
}

// Published is one published port.
type Published struct {
	// PublicPort is the port on the workspace container's network, what the
	// user asked for on the left of the colon.
	PublicPort int

	// PrivatePort is the port inside the container.
	PrivatePort int

	Type string
}

// Docker is the subset of the daemon API the forwarder needs.
type Docker interface {
	ListContainers(ctx context.Context) ([]Container, error)
}

// bindAddr is the local interface a forward binds.
//
// Loopback, and not configurable. A published port becoming reachable from the
// network because a container started on somebody else's machine would be a
// surprise, and a nasty one. This was a field once; nothing ever set it, and
// two log lines hardcoded the value anyway, so a knob that appeared to work
// would have made them lie.
const bindAddr = "127.0.0.1"

// Manager keeps local forwards in step with the containers that are running.
type Manager struct {
	Docker    Docker
	Forwarder Forwarder
	Log       *slog.Logger

	// Owned reports whether a container is one this client created. With a
	// shared daemon (ADR 0012) the event stream carries other users'
	// containers, and forwarding those would open listeners on this machine
	// because somebody else ran docker compose up.
	Owned func(Container) bool

	// LocalPort is the port to open on this machine for a published one.
	//
	// They differ because the workspace daemon chooses what to publish, so
	// nobody collides on a shared daemon, and the client puts the number the
	// user actually typed in front of it (ADR 0037). Nil, and a container
	// whose ports were not remapped, both mean the published port itself.
	LocalPort func(Container, Published) int

	mu     sync.Mutex
	active map[string]*containerForwards
}

type containerForwards struct {
	name     string
	forwards map[int]Forward // keyed by public port
}

// Reconcile brings the set of forwards in line with what is running now.
//
// This is the whole of the logic, and it is deliberately a full reconciliation
// rather than an incremental apply of each event. The event stream can drop --
// a reconnect, a daemon restart, a tunnel blip, and an incremental design
// leaks forwards for containers that stopped during the gap while never
// forwarding those that started. Recomputing from the current state cannot
// drift.
func (m *Manager) Reconcile(ctx context.Context) error {
	containers, err := m.Docker.ListContainers(ctx)
	if err != nil {
		return fmt.Errorf("ports: listing containers: %w", err)
	}

	wanted := map[string]Container{}
	for _, c := range containers {
		if m.Owned != nil && !m.Owned(c) {
			continue
		}
		if len(publishedTCP(c)) == 0 {
			continue
		}
		wanted[c.ID] = c
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active == nil {
		m.active = map[string]*containerForwards{}
	}

	// Close forwards for containers that are gone, and for ports a surviving
	// container no longer publishes.
	for id, existing := range m.active {
		container, still := wanted[id]
		if !still {
			m.closeContainerLocked(id, existing)
			continue
		}
		keep := map[int]bool{}
		for _, p := range publishedTCP(container) {
			keep[p.PublicPort] = true
		}
		for port, fwd := range existing.forwards {
			if !keep[port] {
				_ = fwd.Close()
				delete(existing.forwards, port)
				m.log().Info("closed a forward: the container no longer publishes it",
					"addr", bindAddr, "port", port, "container", existing.name)
			}
		}
	}

	// Open forwards for anything newly published, in a stable order.
	//
	// Ranging the map left the outcome to Go randomising it, which only shows
	// when two containers want one local port: whichever was reached first got
	// it, and a reconcile a moment later could hand it to the other. Rare, and
	// the kind of rare that is reported as "sometimes it forwards the wrong
	// one".
	ids := make([]string, 0, len(wanted))
	for id := range wanted {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	for _, id := range ids {
		container := wanted[id]
		existing, ok := m.active[id]
		if !ok {
			existing = &containerForwards{name: container.Name, forwards: map[int]Forward{}}
			m.active[id] = existing
		}
		for _, p := range publishedTCP(container) {
			if _, already := existing.forwards[p.PublicPort]; already {
				continue
			}
			m.openLocked(existing, container, p)
		}
		if len(existing.forwards) == 0 {
			delete(m.active, id)
		}
	}
	return nil
}

// openLocked starts one forward. Failures are reported and skipped rather than
// aborting the reconciliation: one unavailable port must not stop every other
// container's ports from being forwarded.
func (m *Manager) openLocked(entry *containerForwards, container Container, p Published) {
	bind := bindAddr
	local := net.JoinHostPort(bind, fmt.Sprint(m.localPort(container, p)))
	remote := net.JoinHostPort("127.0.0.1", fmt.Sprint(p.PublicPort))

	fwd, err := m.Forwarder.Forward(local, remote)
	if err != nil {
		// Deliberately not retried on another port. A listener at an address
		// nobody asked for looks like success and breaks the next thing that
		// expects the real one, so the conflict is reported and left alone.
		m.log().Warn("could not forward", "addr", local, "container", container.Name, "err", err)
		return
	}
	entry.forwards[p.PublicPort] = fwd
	m.log().Info("forwarding", "from", fwd.LocalAddr(), "container", container.Name, "port", p.PrivatePort)
}

// localPort is the port to open here for a published one. Keyed on the
// published port everywhere else: it is what reconcile compares against the
// daemon, and the only one that survives a container being recreated.
func (m *Manager) localPort(c Container, p Published) int {
	if m.LocalPort == nil {
		return p.PublicPort
	}
	if local := m.LocalPort(c, p); local > 0 {
		return local
	}
	return p.PublicPort
}

func (m *Manager) closeContainerLocked(id string, entry *containerForwards) {
	for port, fwd := range entry.forwards {
		_ = fwd.Close()
		m.log().Info("closed a forward: the container stopped",
			"addr", bindAddr, "port", port, "container", entry.name)
	}
	delete(m.active, id)
}

// Close tears down every forward.
func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, entry := range m.active {
		for _, fwd := range entry.forwards {
			_ = fwd.Close()
		}
		delete(m.active, id)
	}
	return nil
}

// Active lists the ports currently forwarded, for `remote-docker status`.
func (m *Manager) Active() []int {
	m.mu.Lock()
	defer m.mu.Unlock()

	var out []int
	for _, entry := range m.active {
		for port := range entry.forwards {
			out = append(out, port)
		}
	}
	sort.Ints(out)
	return out
}

// publishedTCP returns the TCP ports a container publishes to the host.
//
// A port with no PublicPort is exposed but not published: it has no host
// side to forward. UDP is skipped because the SSH transport carries TCP only,
// and pretending otherwise would produce a listener that silently drops
// everything.
func publishedTCP(c Container) []Published {
	var out []Published
	seen := map[int]bool{}
	for _, p := range c.Ports {
		if p.PublicPort == 0 {
			continue
		}
		if !workspace.IsTCP(p.Type) {
			continue
		}
		if seen[p.PublicPort] {
			// The daemon reports one entry per address family, so a port
			// published on both IPv4 and IPv6 appears twice.
			continue
		}
		seen[p.PublicPort] = true
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PublicPort < out[j].PublicPort })
	return out
}

// log is the manager's logger, or silence (logx.Or). Nil is not an oversight
// here: it is how a command that must not narrate asks for quiet, which is why
// portsLogger hands one over deliberately.
func (m *Manager) log() *slog.Logger {
	return logx.Or(m.Log)
}

// Forwarding reports whether this manager already holds a local listener on a
// port.
//
// Asked before a container is created: the workspace daemon chooses the
// published port now, so the number the user typed is claimed on this machine
// instead, and a second container asking for it has to be refused somewhere.
// This is the only place that knows what is already open.
//
// Scanned rather than indexed, because the forwards are keyed on the published
// port and there are a handful of them.
func (m *Manager) Forwarding(local int) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, entry := range m.active {
		for _, fwd := range entry.forwards {
			_, port, err := net.SplitHostPort(fwd.LocalAddr().String())
			if err != nil {
				continue
			}
			if n, err := strconv.Atoi(port); err == nil && n == local {
				return true
			}
		}
	}
	return false
}
