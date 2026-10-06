package metrics

import (
	"strings"
	"sync"
	"testing"
)

func TestHistogramExposition(t *testing.T) {
	var r Registry
	h := r.Histogram("rd_wait_seconds", "Waits, by outcome.", []float64{0.5, 1, 10}, "outcome")
	h.Declare("refused")
	h.Observe(0.5, "ok") // on a bound: counted in le="0.5"
	h.Observe(0.75, "ok")
	h.Observe(42, "ok") // above every bound: only +Inf
	h.Observe(0.1, `a"b`)
	r.Histogram("rd_plain_seconds", "No labels.", []float64{1}).Observe(2.5)

	var b strings.Builder
	if _, err := r.WriteTo(&b); err != nil {
		t.Fatal(err)
	}
	want := `# HELP rd_plain_seconds No labels.
# TYPE rd_plain_seconds histogram
rd_plain_seconds_bucket{le="1"} 0
rd_plain_seconds_bucket{le="+Inf"} 1
rd_plain_seconds_sum 2.5
rd_plain_seconds_count 1
# HELP rd_wait_seconds Waits, by outcome.
# TYPE rd_wait_seconds histogram
rd_wait_seconds_bucket{outcome="a\"b",le="0.5"} 1
rd_wait_seconds_bucket{outcome="a\"b",le="1"} 1
rd_wait_seconds_bucket{outcome="a\"b",le="10"} 1
rd_wait_seconds_bucket{outcome="a\"b",le="+Inf"} 1
rd_wait_seconds_sum{outcome="a\"b"} 0.1
rd_wait_seconds_count{outcome="a\"b"} 1
rd_wait_seconds_bucket{outcome="ok",le="0.5"} 1
rd_wait_seconds_bucket{outcome="ok",le="1"} 2
rd_wait_seconds_bucket{outcome="ok",le="10"} 2
rd_wait_seconds_bucket{outcome="ok",le="+Inf"} 3
rd_wait_seconds_sum{outcome="ok"} 43.25
rd_wait_seconds_count{outcome="ok"} 3
rd_wait_seconds_bucket{outcome="refused",le="0.5"} 0
rd_wait_seconds_bucket{outcome="refused",le="1"} 0
rd_wait_seconds_bucket{outcome="refused",le="10"} 0
rd_wait_seconds_bucket{outcome="refused",le="+Inf"} 0
rd_wait_seconds_sum{outcome="refused"} 0
rd_wait_seconds_count{outcome="refused"} 0
`
	if got := b.String(); got != want {
		t.Errorf("exposition:\n%s\nwant:\n%s", got, want)
	}
	if n := h.Count("ok"); n != 3 {
		t.Errorf("Count(ok) = %d, want 3", n)
	}
}

// Bounds sort numerically, not as strings: 2 before 10, +Inf last.
func TestHistogramBucketsKeepTheirOrder(t *testing.T) {
	var r Registry
	r.Histogram("rd_s", "s", []float64{0.01, 2, 10, 300}).Observe(5)
	var b strings.Builder
	_, _ = r.WriteTo(&b)
	want := `rd_s_bucket{le="0.01"} 0
rd_s_bucket{le="2"} 0
rd_s_bucket{le="10"} 1
rd_s_bucket{le="300"} 1
rd_s_bucket{le="+Inf"} 1
`
	if !strings.Contains(b.String(), want) {
		t.Errorf("exposition:\n%s\nwant it to contain:\n%s", b.String(), want)
	}
}

func TestNilHistogramObservesNothing(t *testing.T) {
	var h *Histogram
	h.Observe(1, "x")
	h.Declare("x")
	if n := h.Count("x"); n != 0 {
		t.Errorf("nil histogram count = %d", n)
	}
}

func TestHistogramRefusesBadBuckets(t *testing.T) {
	for _, buckets := range [][]float64{{1, 1}, {2, 1}} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("buckets %v did not panic", buckets)
				}
			}()
			var r Registry
			r.Histogram("rd_bad", "x", buckets)
		}()
	}
}

func TestHistogramIsSafeConcurrently(t *testing.T) {
	var r Registry
	h := r.Histogram("rd_c", "c", []float64{1}, "k")
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 1000 {
				h.Observe(0.5, "a")
				var b strings.Builder
				_, _ = r.WriteTo(&b)
			}
		})
	}
	wg.Wait()
	if n := h.Count("a"); n != 8000 {
		t.Errorf("Count = %d, want 8000", n)
	}
}
