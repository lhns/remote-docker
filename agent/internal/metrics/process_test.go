package metrics

import (
	"runtime"
	"strings"
	"testing"
)

func TestParseStat(t *testing.T) {
	// A command name with a space and a ')' in it, which is why fields are
	// counted from the last ')'.
	stat := "4242 (remote dockerd) x) S 1 4242 4242 0 -1 4194560 1234 0 0 0 " +
		"250 75 0 0 20 0 12 0 98765 734003200 4096 18446744073709551615 1 1 0 0 0 0 0 0 0 0 0 0 17 3 0 0 0 0 0\n"
	st, err := parseStat(stat)
	if err != nil {
		t.Fatal(err)
	}
	if want := (procStat{utime: 250, stime: 75, starttime: 98765, rss: 4096}); st != want {
		t.Errorf("parseStat = %+v, want %+v", st, want)
	}
	if _, err := parseStat("4242 (short) S 1 2 3"); err == nil {
		t.Error("a short stat line parsed")
	}
}

func TestParseBtime(t *testing.T) {
	got, err := parseBtime("cpu  1 2 3\nintr 5\nbtime 1790000000\nprocesses 9\n")
	if err != nil || got != 1790000000 {
		t.Errorf("parseBtime = %d, %v", got, err)
	}
	if _, err := parseBtime("cpu 1\n"); err == nil {
		t.Error("a /proc/stat without btime parsed")
	}
}

func TestProcessMetrics(t *testing.T) {
	var r Registry
	r.Process()
	var b strings.Builder
	if _, err := r.WriteTo(&b); err != nil {
		t.Fatal(err)
	}
	want := []string{"go_goroutines gauge", "go_memstats_heap_alloc_bytes gauge"}
	if runtime.GOOS == "linux" {
		want = append(want,
			"process_cpu_seconds_total counter",
			"process_open_fds gauge",
			"process_resident_memory_bytes gauge",
			"process_start_time_seconds gauge")
	}
	var got []string
	for line := range strings.SplitSeq(b.String(), "\n") {
		if name, ok := strings.CutPrefix(line, "# TYPE "); ok {
			got = append(got, name)
		}
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("families:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// Every family has its one sample.
	if n := strings.Count(b.String(), "\n") - 2*len(want); n != len(want) {
		t.Errorf("%d samples for %d families:\n%s", n, len(want), b.String())
	}
}
