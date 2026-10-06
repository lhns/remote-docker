// Package metrics writes the Prometheus text exposition format, version
// 0.0.4, with the standard library (ADR 0054). Counters and histograms are
// updated where their events happen; gauges are read at scrape time from the
// structures that already hold the answer, so there is no second copy of any
// state.
package metrics

import (
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ContentType is what a 0.0.4 scrape is served as.
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

// Sample is one labelled value of a gauge, its label values in the order the
// gauge names its labels.
type Sample struct {
	Labels []string
	Value  float64
}

// Counter is a monotonically increasing count, per combination of label
// values. A nil *Counter counts nothing, so code that is not wired to a
// registry, tests included, needs no check.
type Counter struct {
	labels []string

	mu   sync.Mutex
	vals map[string]*Sample
}

// Inc adds one for these label values.
func (c *Counter) Inc(values ...string) { c.Add(1, values...) }

// Add adds n for these label values. Adding 0 declares a series, so it is
// exposed as 0 before its first event and a rate over it is defined.
func (c *Counter) Add(n float64, values ...string) {
	if c == nil {
		return
	}
	checkLabels(values, c.labels)
	k := seriesKey(values)
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.vals[k]
	if s == nil {
		s = &Sample{Labels: slices.Clone(values)}
		c.vals[k] = s
	}
	s.Value += n
}

// Value is the count for these label values, for tests.
func (c *Counter) Value(values ...string) float64 {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if s := c.vals[seriesKey(values)]; s != nil {
		return s.Value
	}
	return 0
}

func (c *Counter) samples() []Sample {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Sample, 0, len(c.vals))
	for _, s := range c.vals {
		out = append(out, *s)
	}
	return out
}

func seriesKey(values []string) string { return strings.Join(values, "\xff") }

func checkLabels(values, labels []string) {
	if len(values) != len(labels) {
		panic(fmt.Sprintf("metrics: %d label values for %d labels", len(values), len(labels)))
	}
}

// family is one metric: its header and what writes its samples.
type family struct {
	name, help, kind string
	write            func(b *strings.Builder, name string)
}

// Registry is the set of metrics one scrape returns, in name order.
type Registry struct {
	mu       sync.Mutex
	families []family
}

// Counter registers a counter. Its name ends in _total, by convention.
func (r *Registry) Counter(name, help string, labels ...string) *Counter {
	c := &Counter{labels: labels, vals: map[string]*Sample{}}
	r.add(family{name, help, "counter", samplesWriter(labels, c.samples)})
	return c
}

// Gauge registers a gauge whose samples collect returns at each scrape.
func (r *Registry) Gauge(name, help string, labels []string, collect func() []Sample) {
	r.add(family{name, help, "gauge", samplesWriter(labels, collect)})
}

// CounterFunc registers a counter whose samples collect returns at each
// scrape, for a count something else keeps, such as the kernel's CPU time.
func (r *Registry) CounterFunc(name, help string, labels []string, collect func() []Sample) {
	r.add(family{name, help, "counter", samplesWriter(labels, collect)})
}

func (r *Registry) add(f family) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, g := range r.families {
		if g.name == f.name {
			panic("metrics: " + f.name + " registered twice")
		}
	}
	r.families = append(r.families, f)
}

// WriteTo writes every metric in the text format: families by name, samples
// by label values.
func (r *Registry) WriteTo(w io.Writer) (int64, error) {
	r.mu.Lock()
	families := slices.Clone(r.families)
	r.mu.Unlock()
	slices.SortFunc(families, func(a, b family) int { return strings.Compare(a.name, b.name) })

	var b strings.Builder
	for _, f := range families {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", f.name, escapeHelp(f.help), f.name, f.kind)
		f.write(&b, f.name)
	}
	n, err := io.WriteString(w, b.String())
	return int64(n), err
}

// samplesWriter writes one line per sample, sorted by label values.
func samplesWriter(labels []string, collect func() []Sample) func(*strings.Builder, string) {
	return func(b *strings.Builder, name string) {
		samples := collect()
		slices.SortFunc(samples, func(x, y Sample) int { return slices.Compare(x.Labels, y.Labels) })
		for _, s := range samples {
			writeSample(b, name, labels, s.Labels, "", s.Value)
		}
	}
}

// writeSample writes one line. le, when not empty, is a histogram bucket's
// bound, written as the last label.
func writeSample(b *strings.Builder, name string, labels, values []string, le string, v float64) {
	b.WriteString(name)
	if len(labels) > 0 || le != "" {
		b.WriteByte('{')
		for i, l := range labels {
			if i > 0 {
				b.WriteByte(',')
			}
			val := ""
			if i < len(values) {
				val = values[i]
			}
			fmt.Fprintf(b, "%s=\"%s\"", l, escapeLabel(val))
		}
		if le != "" {
			if len(labels) > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(b, "le=\"%s\"", le)
		}
		b.WriteByte('}')
	}
	fmt.Fprintf(b, " %s\n", formatFloat(v))
}

func formatFloat(v float64) string {
	if math.IsInf(v, 1) {
		return "+Inf"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// ServeHTTP answers a scrape.
func (r *Registry) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", ContentType)
	_, _ = r.WriteTo(w)
}

var (
	helpEscaper  = strings.NewReplacer(`\`, `\\`, "\n", `\n`)
	labelEscaper = strings.NewReplacer(`\`, `\\`, "\n", `\n`, `"`, `\"`)
)

func escapeHelp(s string) string  { return helpEscaper.Replace(s) }
func escapeLabel(s string) string { return labelEscaper.Replace(s) }

// Listen opens the scrape listener, or none for an empty addr.
func Listen(addr string) (net.Listener, error) {
	if addr == "" {
		return nil, nil
	}
	return net.Listen("tcp", addr)
}

// Serve answers GET /metrics on ln in the background until stop. Plain HTTP,
// and nothing else on this listener.
func (r *Registry) Serve(ln net.Listener, onErr func(error)) (stop func()) {
	mux := http.NewServeMux()
	mux.Handle("/metrics", r)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) && onErr != nil {
			onErr(err)
		}
	}()
	return func() { _ = srv.Close() }
}
