package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExposition(t *testing.T) {
	var r Registry
	refused := r.Counter("rd_refused_total", "Refusals, by reason.", "reason")
	refused.Add(0, "limit")
	refused.Inc("duplicate")
	refused.Inc("duplicate")
	r.Counter("rd_plain_total", "No labels.\nTwo lines, one \\ backslash.").Inc()
	r.Gauge("rd_runs", "Runs.", []string{"account", "state"}, func() []Sample {
		return []Sample{
			{Labels: []string{"bob", "live"}, Value: 1},
			{Labels: []string{`a"l\i` + "\nce", "grace"}, Value: 2.5},
		}
	})
	r.Gauge("rd_empty", "Nothing yet.", []string{"account"}, func() []Sample { return nil })

	var b strings.Builder
	if _, err := r.WriteTo(&b); err != nil {
		t.Fatal(err)
	}
	want := `# HELP rd_empty Nothing yet.
# TYPE rd_empty gauge
# HELP rd_plain_total No labels.\nTwo lines, one \\ backslash.
# TYPE rd_plain_total counter
rd_plain_total 1
# HELP rd_refused_total Refusals, by reason.
# TYPE rd_refused_total counter
rd_refused_total{reason="duplicate"} 2
rd_refused_total{reason="limit"} 0
# HELP rd_runs Runs.
# TYPE rd_runs gauge
rd_runs{account="a\"l\\i\nce",state="grace"} 2.5
rd_runs{account="bob",state="live"} 1
`
	if got := b.String(); got != want {
		t.Errorf("exposition:\n%s\nwant:\n%s", got, want)
	}
}

func TestNilCounterCountsNothing(t *testing.T) {
	var c *Counter
	c.Inc("x")
	if v := c.Value("x"); v != 0 {
		t.Errorf("nil counter = %v", v)
	}
}

func TestDuplicateNamePanics(t *testing.T) {
	var r Registry
	r.Counter("x_total", "x")
	defer func() {
		if recover() == nil {
			t.Error("registering x_total twice did not panic")
		}
	}()
	r.Counter("x_total", "x")
}

func TestHandler(t *testing.T) {
	var r Registry
	r.Counter("rd_x_total", "x").Inc()

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if got := rec.Header().Get("Content-Type"); got != "text/plain; version=0.0.4; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
	if !strings.Contains(rec.Body.String(), "rd_x_total 1\n") {
		t.Errorf("body = %q", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/metrics", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST = %d, want 405", rec.Code)
	}
}

func TestServe(t *testing.T) {
	var r Registry
	r.Counter("rd_x_total", "x").Inc()
	ln, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	stop := r.Serve(ln, func(err error) { t.Error(err) })
	defer stop()

	resp, err := http.Get("http://" + ln.Addr().String() + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "rd_x_total 1") {
		t.Errorf("GET /metrics = %d %q", resp.StatusCode, body)
	}

	other, err := http.Get("http://" + ln.Addr().String() + "/")
	if err != nil {
		t.Fatal(err)
	}
	_ = other.Body.Close()
	if other.StatusCode != http.StatusNotFound {
		t.Errorf("GET / = %d, want 404", other.StatusCode)
	}
}

func TestListenEmptyOpensNothing(t *testing.T) {
	ln, err := Listen("")
	if ln != nil || err != nil {
		t.Errorf("Listen(\"\") = %v, %v; want no listener", ln, err)
	}
}
