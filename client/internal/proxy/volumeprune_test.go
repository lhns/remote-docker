package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

const managedFilter = `"label!":{"com.github.lhns.remote-docker":true}`

// prune sends POST path?filters=<filters> through a proxy and returns what the
// daemon received, the relayed response and its body.
func prune(t *testing.T, p *Proxy, path, filters string, daemonReply func()) (daemonQuery url.Values, resp *http.Response, body string) {
	t.Helper()
	var mu sync.Mutex
	d := startDaemon(t, func(_ *fakeDaemon, req *http.Request, conn net.Conn, _ *bufio.Reader) {
		mu.Lock()
		daemonQuery = req.URL.Query()
		mu.Unlock()
		if daemonReply != nil {
			daemonReply()
		}
		respondJSON(conn, 200, `{"VolumesDeleted":["x"],"SpaceReclaimed":0}`)
	})
	p.Dialer = &tcpDialer{d.listener.Addr().String()}
	addr := startProxy(t, p)

	target := "http://" + addr + path
	if filters != "" {
		target += "?filters=" + url.QueryEscape(filters)
	}
	resp, err := http.Post(target, "application/json", nil)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	mu.Lock()
	defer mu.Unlock()
	return daemonQuery, resp, string(b)
}

func sentFilters(t *testing.T, q url.Values) map[string]map[string]bool {
	t.Helper()
	var got map[string]map[string]bool
	if err := json.Unmarshal([]byte(q.Get("filters")), &got); err != nil {
		t.Fatalf("daemon got filters %q: %v", q.Get("filters"), err)
	}
	return got
}

func TestVolumePruneGetsTheManagedExclusion(t *testing.T) {
	for _, path := range []string{"/volumes/prune", "/v1.51/volumes/prune"} {
		q, resp, _ := prune(t, &Proxy{}, path, "", nil)
		if resp.StatusCode != 200 {
			t.Fatalf("%s: status %d", path, resp.StatusCode)
		}
		got := sentFilters(t, q)
		if len(got) != 1 || !got["label!"]["com.github.lhns.remote-docker"] {
			t.Errorf("%s: daemon got %q, want only %s", path, q.Get("filters"), managedFilter)
		}
	}
}

func TestVolumePruneKeepsTheUsersFilters(t *testing.T) {
	q, _, _ := prune(t, &Proxy{}, "/volumes/prune", `{"label":{"env=ci":true},"all":{"true":true}}`, nil)
	got := sentFilters(t, q)
	if !got["label"]["env=ci"] || !got["all"]["true"] || !got["label!"]["com.github.lhns.remote-docker"] {
		t.Errorf("daemon got %q", q.Get("filters"))
	}
	// The older list form is read too.
	q, _, _ = prune(t, &Proxy{}, "/volumes/prune", `{"all":["true"]}`, nil)
	if got := sentFilters(t, q); !got["all"]["true"] || !got["label!"]["com.github.lhns.remote-docker"] {
		t.Errorf("legacy form: daemon got %q", q.Get("filters"))
	}
}

func TestVolumePruneRefusesWhatItCannotGuard(t *testing.T) {
	for name, filters := range map[string]string{
		"malformed":     `{"label":`,
		"not an object": `[1,2]`,
		"user label!":   `{"label!":{"keep":true}}`,
		"wrong nesting": `{"all":"true"}`,
	} {
		daemonSaw := false
		p := &Proxy{}
		d := startDaemon(t, func(_ *fakeDaemon, _ *http.Request, conn net.Conn, _ *bufio.Reader) {
			daemonSaw = true
			respondJSON(conn, 200, `{}`)
		})
		p.Dialer = &tcpDialer{d.listener.Addr().String()}
		addr := startProxy(t, p)

		resp, err := http.Post("http://"+addr+"/volumes/prune?filters="+url.QueryEscape(filters), "application/json", nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var msg struct {
			Message string `json:"message"`
		}
		if resp.StatusCode != http.StatusBadRequest || json.Unmarshal(body, &msg) != nil || !strings.HasPrefix(msg.Message, "refusing volume prune") {
			t.Errorf("%s: status %d body %q", name, resp.StatusCode, body)
		}
		if daemonSaw {
			t.Errorf("%s: the request reached the daemon unguarded", name)
		}
	}
}

func TestVolumePruneCollectsOnlyForAllWithoutLabelFilter(t *testing.T) {
	tests := []struct {
		name, path, filters string
		want                bool
	}{
		{"all", "/volumes/prune", `{"all":{"true":true}}`, true},
		{"all, versioned", "/v1.51/volumes/prune", `{"all":{"true":true}}`, true},
		{"no filters", "/volumes/prune", ``, false},
		{"all=false", "/volumes/prune", `{"all":{"false":true}}`, false},
		{"all with a label", "/volumes/prune", `{"all":{"true":true},"label":{"a":true}}`, false},
		{"old API prunes everything", "/v1.41/volumes/prune", ``, true},
		{"new API without all", "/v1.42/volumes/prune", ``, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ran := false
			p := &Proxy{AfterVolumePrune: func(context.Context) { ran = true }}
			prune(t, p, tt.path, tt.filters, nil)
			if ran != tt.want {
				t.Errorf("collector ran = %v, want %v", ran, tt.want)
			}
		})
	}
}

// The collector must be done before the CLI sees the answer, or a one-shot
// command exits with it half-run.
func TestVolumePruneCollectsBeforeTheReply(t *testing.T) {
	var mu sync.Mutex
	var order []string
	note := func(s string) { mu.Lock(); order = append(order, s); mu.Unlock() }

	p := &Proxy{AfterVolumePrune: func(context.Context) {
		time.Sleep(200 * time.Millisecond)
		note("collected")
	}}
	_, resp, body := prune(t, p, "/volumes/prune", `{"all":{"true":true}}`, func() { note("pruned") })
	note("replied")

	mu.Lock()
	defer mu.Unlock()
	if strings.Join(order, ",") != "pruned,collected,replied" || resp.StatusCode != 200 {
		t.Errorf("order = %v, status %d", order, resp.StatusCode)
	}
	if body != `{"VolumesDeleted":["x"],"SpaceReclaimed":0}` {
		t.Errorf("response was not relayed untouched: %q", body)
	}
}

func TestVolumePruneSkipsTheCollectorWhenTheDaemonFails(t *testing.T) {
	ran := false
	p := &Proxy{AfterVolumePrune: func(context.Context) { ran = true }}
	d := startDaemon(t, func(_ *fakeDaemon, _ *http.Request, conn net.Conn, _ *bufio.Reader) {
		respondJSON(conn, 500, `{"message":"a prune operation is already running"}`)
	})
	p.Dialer = &tcpDialer{d.listener.Addr().String()}
	addr := startProxy(t, p)
	resp, err := http.Post("http://"+addr+"/volumes/prune?filters="+url.QueryEscape(`{"all":{"true":true}}`), "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if ran || resp.StatusCode != 500 {
		t.Errorf("ran = %v, status %d", ran, resp.StatusCode)
	}
}
