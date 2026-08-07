// Command watchprobe answers one question: does a file watcher inside a
// container see a file created on the client machine?
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
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

func main() {
	dir := "/data"
	if len(os.Args) > 1 {
		dir = os.Args[1]
	}
	timeout := 20 * time.Second

	var (
		mu       sync.Mutex
		inotify  []string
		polled   []string
		pollSeen = map[string]bool{}
	)

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		fmt.Printf("RESULT inotify=unavailable error=%v\n", err)
		os.Exit(1)
	}
	defer func() { _ = watcher.Close() }()

	if err := watcher.Add(dir); err != nil {
		fmt.Printf("RESULT inotify=unavailable error=%v\n", err)
		os.Exit(1)
	}
	fmt.Printf("watching %s for %s\n", dir, timeout)

	done := make(chan struct{})

	// inotify: report anything the kernel tells us about.
	go func() {
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				mu.Lock()
				inotify = append(inotify, fmt.Sprintf("%s %s", event.Op, filepath.Base(event.Name)))
				mu.Unlock()
				fmt.Printf("INOTIFY %s %s\n", event.Op, filepath.Base(event.Name))
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				fmt.Printf("INOTIFY-ERROR %v\n", err)
			case <-done:
				return
			}
		}
	}()

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

	time.Sleep(timeout)
	close(done)

	mu.Lock()
	defer mu.Unlock()
	sort.Strings(polled)

	// A single machine-readable line, so the test asserts on this rather than
	// on the shape of the log.
	fmt.Printf("RESULT inotify_events=%d poll_entries=%d inotify=[%s] poll=[%s]\n",
		len(inotify), len(polled),
		strings.Join(inotify, "; "), strings.Join(polled, "; "))
}
