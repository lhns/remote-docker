package session

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/lhns/remote-docker/client/internal/config"
	"github.com/lhns/remote-docker/client/internal/proxy"
	"github.com/lhns/remote-docker/core/workspace"
)

// listingDaemon answers every request with a container list.
type listingDaemon struct{ containers []proxy.Container }

func (d listingDaemon) DialDocker(context.Context) (io.ReadWriteCloser, error) {
	ours, theirs := net.Pipe()
	go func() {
		defer func() { _ = theirs.Close() }()
		if _, err := http.ReadRequest(bufio.NewReader(theirs)); err != nil {
			return
		}
		body, _ := json.Marshal(d.containers)
		_, _ = fmt.Fprintf(theirs, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
	}()
	return ours, nil
}

// The idle sweep asks which containers are this client's while a reconnect
// works out which client it is. Only -race sees it go wrong, so this proves
// something only in CI's Linux test job, which runs `go test -race`.
func TestAReconnectDoesNotRaceTheSweep(t *testing.T) {
	t.Setenv("REMOTE_DOCKER_STATE_DIR", t.TempDir())
	ws := startWorkspaceWith(t, &silentWorkspace{answer: func(string) (string, bool) {
		var out strings.Builder
		if err := (workspace.Info{User: "alice", UID: 1000, GID: 1000, NFSPort: 2049}).Encode(&out); err != nil {
			return "", false
		}
		return out.String(), true
	}})
	host, portText, err := net.SplitHostPort(ws.addr.String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(t.Context(), Options{Config: config.Config{Host: host, Port: port, User: "alice"}, Role: Query})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// One of alice's containers on another machine, so the sweep compares
	// client ids and finds nothing of ours.
	held := &liveConn{
		info: workspace.Info{User: "alice"},
		api: &proxy.APIClient{Dialer: listingDaemon{containers: []proxy.Container{{
			ID:     "c1",
			Labels: map[string]string{workspace.OwnerLabel: "alice", workspace.ClientLabel: "another-machine"},
		}}}},
	}

	var wg sync.WaitGroup
	wg.Go(func() {
		for range 5 {
			live, err := s.connect(t.Context())
			if err != nil {
				t.Errorf("connect: %v", err)
				return
			}
			live.close()
		}
	})
	wg.Go(func() {
		for range 50 {
			if _, err := s.hasLiveDependents(t.Context(), held); err != nil {
				t.Errorf("hasLiveDependents: %v", err)
				return
			}
		}
	})
	wg.Wait()
}
