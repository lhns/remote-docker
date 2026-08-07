// Command watchprobe answers one question: does a file watcher inside a
// container see a change made on the client machine?
//
// It watches a directory two ways at once and reports what each observes:
//
//	inotify  the kernel notification interface every hot-reload tool uses
//	polling  a directory listing on a timer, which is what tools fall back to
//
// The distinction decides how honest the project's central claim can be. A
// real filesystem rather than a sync is only better than a sync if changes are
// *noticed*; NFS carries no change notification, so a watcher may see nothing
// while the file is plainly there. See docs/adr/0014.
//
// It reports RAW inotify mask bits rather than going through fsnotify,
// because fsnotify's inotify mask omits IN_OPEN and IN_CLOSE_WRITE entirely --
// and IN_CLOSE_WRITE is the event the replay experiment is chiefly about. A
// probe that cannot see the thing under test would report "nothing happened"
// and be believed.
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

func main() {
	timeout := flag.Duration("timeout", 20*time.Second, "how long to watch")
	flag.Parse()

	dir := "/data"
	if flag.NArg() > 0 {
		dir = flag.Arg(0)
	}

	var (
		mu       sync.Mutex
		events   []string
		polled   []string
		pollSeen = map[string]bool{}
	)

	stop, err := watch(dir, func(mask, name string) {
		mu.Lock()
		events = append(events, strings.TrimSpace(mask+" "+name))
		mu.Unlock()
		fmt.Printf("INOTIFY %s %s\n", mask, name)
	})
	if err != nil {
		fmt.Printf("RESULT inotify=unavailable error=%v\n", err)
		os.Exit(1)
	}
	defer stop()

	done := make(chan struct{})

	// polling: the control. If this sees nothing either, the mount itself is
	// broken and the inotify result says nothing about notification.
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				entries, err := os.ReadDir(dir)
				if err != nil {
					continue
				}
				for _, e := range entries {
					mu.Lock()
					if !pollSeen[e.Name()] {
						pollSeen[e.Name()] = true
						polled = append(polled, e.Name())
						fmt.Printf("POLL %s\n", e.Name())
					}
					mu.Unlock()
				}
			case <-done:
				return
			}
		}
	}()

	// READY is printed only once the watch is established, so a caller can
	// wait for it instead of sleeping and hoping. A change made before the
	// watch lands proves nothing either way, and that is an easy way to
	// record a false negative.
	fmt.Printf("READY watching %s for %s\n", dir, *timeout)
	os.Stdout.Sync()

	time.Sleep(*timeout)
	close(done)

	mu.Lock()
	defer mu.Unlock()
	sort.Strings(polled)

	// A single machine-readable line, so the test asserts on this rather than
	// on the shape of the log.
	fmt.Printf("RESULT inotify_events=%d poll_entries=%d inotify=[%s] poll=[%s]\n",
		len(events), len(polled),
		strings.Join(events, "; "), strings.Join(polled, "; "))
}
