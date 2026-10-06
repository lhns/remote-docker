package metrics

import (
	"slices"
	"strings"
	"sync"
	"time"
)

// Histogram counts observations into fixed buckets, per combination of label
// values. A nil *Histogram observes nothing.
type Histogram struct {
	labels  []string
	buckets []float64 // upper bounds, ascending; +Inf is implicit

	mu     sync.Mutex
	series map[string]*histSeries
}

type histSeries struct {
	labels []string
	counts []uint64 // per bucket, not cumulative; the last is +Inf
	sum    float64
	count  uint64
}

// Histogram registers a histogram with these bucket upper bounds, which must
// ascend. The +Inf bucket is added.
func (r *Registry) Histogram(name, help string, buckets []float64, labels ...string) *Histogram {
	for i := range buckets {
		if i > 0 && buckets[i] <= buckets[i-1] {
			panic("metrics: " + name + " needs strictly ascending buckets")
		}
	}
	h := &Histogram{labels: labels, buckets: slices.Clone(buckets), series: map[string]*histSeries{}}
	r.add(family{name, help, "histogram", h.write})
	return h
}

// Observe records v for these label values.
func (h *Histogram) Observe(v float64, values ...string) {
	if h == nil {
		return
	}
	checkLabels(values, h.labels)
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.get(values)
	i, _ := slices.BinarySearch(h.buckets, v) // first bound >= v, or len: +Inf
	s.counts[i]++
	s.sum += v
	s.count++
}

// Since records the seconds since start.
func (h *Histogram) Since(start time.Time, values ...string) {
	h.Observe(time.Since(start).Seconds(), values...)
}

// Declare exposes a series with no observations yet, so a rate over it is
// defined before its first event.
func (h *Histogram) Declare(values ...string) {
	if h == nil {
		return
	}
	checkLabels(values, h.labels)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.get(values)
}

// Count is the number of observations for these label values, for tests.
func (h *Histogram) Count(values ...string) uint64 {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if s := h.series[seriesKey(values)]; s != nil {
		return s.count
	}
	return 0
}

// get is the series for values, created if new. Under mu.
func (h *Histogram) get(values []string) *histSeries {
	k := seriesKey(values)
	s := h.series[k]
	if s == nil {
		s = &histSeries{labels: slices.Clone(values), counts: make([]uint64, len(h.buckets)+1)}
		h.series[k] = s
	}
	return s
}

// write writes each series as client_golang does: cumulative buckets
// ascending with le last among the labels, then _sum, then _count.
func (h *Histogram) write(b *strings.Builder, name string) {
	h.mu.Lock()
	all := make([]histSeries, 0, len(h.series))
	for _, s := range h.series {
		c := *s
		c.counts = slices.Clone(s.counts)
		all = append(all, c)
	}
	h.mu.Unlock()
	slices.SortFunc(all, func(x, y histSeries) int { return slices.Compare(x.labels, y.labels) })

	for _, s := range all {
		var cum uint64
		for i, n := range s.counts {
			cum += n
			le := "+Inf"
			if i < len(h.buckets) {
				le = formatFloat(h.buckets[i])
			}
			writeSample(b, name+"_bucket", h.labels, s.labels, le, float64(cum))
		}
		writeSample(b, name+"_sum", h.labels, s.labels, "", s.sum)
		writeSample(b, name+"_count", h.labels, s.labels, "", float64(s.count))
	}
}
