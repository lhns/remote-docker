package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"
)

// overtakenWrite holds the first record write until a second has been written
// or a moment has passed, and says when the first is being held. Unless saves
// are serialised, the second lands first and the first, holding the OLDER
// copy, lands over it.
func overtakenWrite(t *testing.T) <-chan struct{} {
	t.Helper()
	real := writeRecord
	t.Cleanup(func() { writeRecord = real })

	held, second := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	calls := 0
	writeRecord = func(path string, data []byte, mode os.FileMode) error {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		switch n {
		case 1:
			close(held)
			select {
			case <-second:
			case <-time.After(300 * time.Millisecond):
			}
		case 2:
			defer close(second)
		}
		return real(path, data, mode)
	}
	return held
}

func TestConcurrentCachedRecordsKeepTheNewerSet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "caches", "ws.json")
	store := newCachedStore(path, nil)
	held := overtakenWrite(t)

	done := make(chan struct{})
	go func() {
		defer close(done)
		store.Record("/m/aaaa", []string{"/a.go"})
	}()
	<-held
	store.Record("/m/bbbb", []string{"/b.go"})
	<-done

	again := newCachedStore(path, nil)
	for _, export := range []string{"/m/aaaa", "/m/bbbb"} {
		if _, ok := again.Filled(export); !ok {
			t.Errorf("%s is missing on disk after two concurrent records", export)
		}
	}
}

// The record survives the session that wrote it, which is the whole point:
// the deletion it has to explain happened while nothing was running.
func TestCachedStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "caches", "ws.json")
	store := newCachedStore(path, nil)

	if _, ok := store.Filled("/m/aaaa"); ok {
		t.Error("an empty store claimed to know a share")
	}

	store.Record("/m/aaaa", []string{"/b.go", "/a.go"})
	again := newCachedStore(path, nil)

	got, ok := again.Filled("/m/aaaa")
	if !ok {
		t.Fatal("the record did not survive being written and read")
	}
	// Sorted, so two runs of the same fill produce the same file rather than a
	// diff of nothing.
	if !slices.Equal(got, []string{"/a.go", "/b.go"}) {
		t.Errorf("filled = %v", got)
	}
}

// A configuration directory is a thing people sync between machines, and this
// record decides what to REMOVE from a cache. The same rule the share record
// already applies (ADR 0027).
func TestCachedStoreIgnoresARecordFromElsewhere(t *testing.T) {
	path := filepath.Join(t.TempDir(), "caches", "ws.json")
	data, err := json.Marshal(cachedFile{
		boundRecord: boundRecord{Version: cachedFileVersion, Machine: "some-other-laptop", User: "someone-else"},
		Shares:      map[string][]string{"/m/aaaa": {"/a.go"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, ok := newCachedStore(path, nil).Filled("/m/aaaa"); ok {
		t.Error("a record written on another machine was believed")
	}
}
