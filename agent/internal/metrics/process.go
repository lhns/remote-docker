package metrics

import (
	"errors"
	"os"
	"runtime"
	"runtime/metrics"
	"strconv"
	"strings"
)

// userHZ is the kernel's clock tick for /proc times. Fixed at 100 on every
// architecture Linux exposes to userspace; client_golang's procfs hardcodes
// the same (procfs proc_stat.go, read 2026-10-06).
const userHZ = 100

// Process registers the standard process_* and go_* metrics under the names
// client_golang uses, so dashboards recognise them. process_* is read from
// /proc and registered only where /proc/self/stat can be read: Linux.
func (r *Registry) Process() {
	r.Gauge("go_goroutines", "Number of goroutines that currently exist.", nil, func() []Sample {
		return []Sample{{Value: float64(runtime.NumGoroutine())}}
	})
	r.Gauge("go_memstats_heap_alloc_bytes", "Number of heap bytes allocated and currently in use.", nil, func() []Sample {
		s := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
		metrics.Read(s)
		if s[0].Value.Kind() != metrics.KindUint64 {
			return nil
		}
		return []Sample{{Value: float64(s[0].Value.Uint64())}}
	})

	if runtime.GOOS != "linux" {
		return
	}
	st, err := readStat()
	if err != nil {
		return
	}
	btime, err := readBtime()
	if err != nil {
		return
	}
	start := float64(btime) + float64(st.starttime)/userHZ
	pageSize := float64(os.Getpagesize())

	r.CounterFunc("process_cpu_seconds_total", "Total user and system CPU time spent in seconds.", nil, func() []Sample {
		st, err := readStat()
		if err != nil {
			return nil
		}
		return []Sample{{Value: float64(st.utime+st.stime) / userHZ}}
	})
	r.Gauge("process_resident_memory_bytes", "Resident memory size in bytes.", nil, func() []Sample {
		st, err := readStat()
		if err != nil {
			return nil
		}
		return []Sample{{Value: float64(st.rss) * pageSize}}
	})
	r.Gauge("process_open_fds", "Number of open file descriptors.", nil, func() []Sample {
		fds, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			return nil
		}
		return []Sample{{Value: float64(len(fds))}}
	})
	r.Gauge("process_start_time_seconds", "Start time of the process since unix epoch in seconds.", nil, func() []Sample {
		return []Sample{{Value: start}}
	})
}

// procStat is what Process reads from /proc/self/stat: times in clock ticks,
// rss in pages.
type procStat struct {
	utime, stime, starttime, rss uint64
}

func readStat() (procStat, error) {
	b, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return procStat{}, err
	}
	return parseStat(string(b))
}

func readBtime() (uint64, error) {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, err
	}
	return parseBtime(string(b))
}

// parseStat reads proc(5)'s /proc/<pid>/stat. The command name is in
// parentheses and may hold spaces or parentheses itself, so fields are
// counted from the LAST ')'.
func parseStat(s string) (procStat, error) {
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return procStat{}, errors.New("metrics: /proc/self/stat has no command name")
	}
	f := strings.Fields(s[i+1:])
	// f[0] is field 3, state; field n is f[n-3].
	if len(f) < 22 {
		return procStat{}, errors.New("metrics: /proc/self/stat is short")
	}
	var st procStat
	for _, p := range []struct {
		field int
		to    *uint64
	}{{14, &st.utime}, {15, &st.stime}, {22, &st.starttime}, {24, &st.rss}} {
		v, err := strconv.ParseUint(f[p.field-3], 10, 64)
		if err != nil {
			return procStat{}, err
		}
		*p.to = v
	}
	return st, nil
}

// parseBtime reads the boot time, in seconds since the epoch, from /proc/stat.
func parseBtime(s string) (uint64, error) {
	for line := range strings.Lines(s) {
		if v, ok := strings.CutPrefix(line, "btime "); ok {
			return strconv.ParseUint(strings.TrimSpace(v), 10, 64)
		}
	}
	return 0, errors.New("metrics: /proc/stat has no btime")
}
