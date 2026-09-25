// Package ports opens a local forward for each published container port,
// which otherwise lives only in the workspace's namespace (ADR 0008).
package ports

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"slices"
	"sort"
	"strconv"
	"sync"

	"github.com/lhns/remote-docker/core/logx"
	"github.com/lhns/remote-docker/core/workspace"
)

// Forwarder opens a local listener carrying connections to an address inside
// the workspace.
type Forwarder interface {
	// Forward carries localAddr to remoteAddr inside the workspace. network is
	// "tcp" or "udp"; never add a UDP branch in this package (ADR 0038).
	Forward(network, localAddr, remoteAddr string) (Forward, error)
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
	// PublicPort is the port on the workspace container's network.
	PublicPort int

	// PrivatePort is the port inside the container.
	PrivatePort int

	Type string
}

// Docker is the subset of the daemon API the forwarder needs.
type Docker interface {
	ListContainers(ctx context.Context) ([]Container, error)
}

// bindAddr is loopback and not configurable: a container started elsewhere
// must not open this machine to the network.
const bindAddr = "127.0.0.1"

// Manager keeps local forwards in step with the containers that are running.
type Manager struct {
	Docker    Docker
	Forwarder Forwarder
	Log       *slog.Logger

	// Owned reports whether this client created a container; on a shared
	// daemon (ADR 0012) others' containers must not open listeners here.
	Owned func(Container) bool

	// LocalPorts are the client's numbers for a published port (ADR 0008),
	// several when one container port was published twice. Nil or empty means
	// the published port itself.
	LocalPorts func(Container, Published) []int

	mu     sync.Mutex
	active map[string]*containerForwards

	// closed stops a Reconcile already listing (outside the lock) from
	// reopening listeners after Close.
	closed bool
}

type containerForwards struct {
	name     string
	forwards map[listener]Forward
}

// listener is what this machine opened: the LOCAL port, and the protocol,
// since 53/tcp and 53/udp are two listeners on one number.
type listener struct {
	network string
	port    int
}

// Reconcile brings the forwards in line with what is running now. A full
// recompute rather than per-event, because the event stream can drop.
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
		if len(published(c)) == 0 {
			continue
		}
		wanted[c.ID] = c
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}
	if m.active == nil {
		m.active = map[string]*containerForwards{}
	}

	// Close forwards for gone containers and ports no longer published.
	for id, existing := range m.active {
		container, still := wanted[id]
		if !still {
			m.closeContainerLocked(id, existing)
			continue
		}
		keep := map[listener]bool{}
		for _, p := range published(container) {
			for _, local := range m.localPorts(container, p) {
				keep[listener{network(p), local}] = true
			}
		}
		for l, fwd := range existing.forwards {
			if !keep[l] {
				_ = fwd.Close()
				delete(existing.forwards, l)
				m.log().Info("closed a forward: the container no longer publishes it",
					"addr", bindAddr, "port", l.port, "network", l.network, "container", existing.name)
			}
		}
	}

	// Open new forwards in sorted order, so two containers wanting one local
	// port do not trade it between reconciles.
	for _, id := range slices.Sorted(maps.Keys(wanted)) {
		container := wanted[id]
		existing, ok := m.active[id]
		if !ok {
			existing = &containerForwards{name: container.Name, forwards: map[listener]Forward{}}
			m.active[id] = existing
		}
		for _, p := range published(container) {
			for _, local := range m.localPorts(container, p) {
				if _, already := existing.forwards[listener{network(p), local}]; already {
					continue
				}
				m.openLocked(existing, container, p, local)
			}
		}
		if len(existing.forwards) == 0 {
			delete(m.active, id)
		}
	}
	return nil
}

// openLocked starts one forward; a failure is logged and skipped.
func (m *Manager) openLocked(entry *containerForwards, container Container, p Published, localPort int) {
	local := net.JoinHostPort(bindAddr, fmt.Sprint(localPort))
	remote := net.JoinHostPort("127.0.0.1", fmt.Sprint(p.PublicPort))

	fwd, err := m.Forwarder.Forward(network(p), local, remote)
	if err != nil {
		// Never retried on another port: that would look like success.
		m.log().Warn("could not forward", "addr", local, "container", container.Name, "err", err)
		return
	}
	entry.forwards[listener{network(p), localPort}] = fwd
	m.log().Info("forwarding", "from", fwd.LocalAddr(), "container", container.Name, "port", p.PrivatePort)
}

// localPorts are the ports to open here for a published one, and never empty:
// with no answer the published port is its own.
func (m *Manager) localPorts(c Container, p Published) []int {
	if m.LocalPorts != nil {
		if local := m.LocalPorts(c, p); len(local) > 0 {
			return local
		}
	}
	return []int{p.PublicPort}
}

func (m *Manager) closeContainerLocked(id string, entry *containerForwards) {
	for l, fwd := range entry.forwards {
		_ = fwd.Close()
		m.log().Info("closed a forward: the container stopped",
			"addr", bindAddr, "port", l.port, "network", l.network, "container", entry.name)
	}
	delete(m.active, id)
}

// Close tears down every forward, and ends the manager: a Reconcile after it,
// or one already listing containers when it ran, opens nothing.
func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	for id, entry := range m.active {
		for _, fwd := range entry.forwards {
			_ = fwd.Close()
		}
		delete(m.active, id)
	}
	return nil
}

// Active lists the ports currently forwarded. Used by tests.
func (m *Manager) Active() []int {
	m.mu.Lock()
	defer m.mu.Unlock()

	var out []int
	for _, entry := range m.active {
		for l := range entry.forwards {
			out = append(out, l.port)
		}
	}
	sort.Ints(out)
	return out
}

// published returns a container's published ports, once per protocol and
// port: the daemon reports one entry per address family, and 53/tcp and
// 53/udp are different ports (ADR 0038).
func published(c Container) []Published {
	var out []Published
	seen := map[string]bool{}
	for _, p := range c.Ports {
		if p.PublicPort == 0 {
			continue
		}
		key := network(p) + "/" + strconv.Itoa(p.PublicPort)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].PublicPort != out[j].PublicPort {
			return out[i].PublicPort < out[j].PublicPort
		}
		return network(out[i]) < network(out[j])
	})
	return out
}

// network is what a published port is carried over; an empty type is tcp.
func network(p Published) string {
	if workspace.IsTCP(p.Type) {
		return "tcp"
	}
	return "udp"
}

// log is the manager's logger, or silence (logx.Or).
func (m *Manager) log() *slog.Logger {
	return logx.Or(m.Log)
}

// Forwarding reports whether a local TCP listener already holds a port. Asked
// before a container is created, since only this machine knows what is open.
func (m *Manager) Forwarding(local int) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, entry := range m.active {
		if _, ok := entry.forwards[listener{"tcp", local}]; ok {
			return true
		}
	}
	return false
}
