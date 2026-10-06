package dircache

import (
	"context"
	"slices"
	"strings"
	"sync"
)

// fakeStore is a Store that holds what it was given, so the policy can be
// driven end to end without a workspace, a connection or a tar.
type fakeStore struct {
	mu sync.Mutex

	applied  []Entry
	applyErr error
	dropped  []string
	pulled   []string

	changes    []Change
	changesErr error
	asked      map[string]int // Changes calls, per share
	onChanges  func()         // runs after Changes has answered

	// files is what Pull hands back, by share-relative path.
	files map[string]File
}

func (f *fakeStore) Apply(_ context.Context, _, _ string, entries []Entry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.applyErr != nil {
		return f.applyErr
	}
	f.applied = append(f.applied, entries...)
	return nil
}

func (f *fakeStore) Drop(_ context.Context, _ string, paths []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dropped = append(f.dropped, paths...)
	// Gone from the cache, so no longer a change in it.
	f.changes = slices.DeleteFunc(f.changes, func(c Change) bool {
		return slices.Contains(paths, c.Path) || slices.Contains(paths, strings.TrimPrefix(c.Path, "/"))
	})
	return nil
}

func (f *fakeStore) Changes(_ context.Context, share string) ([]Change, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.asked == nil {
		f.asked = map[string]int{}
	}
	f.asked[share]++
	if f.onChanges != nil {
		defer f.onChanges()
	}
	return slices.Clone(f.changes), f.changesErr
}

func (f *fakeStore) Pull(_ context.Context, _ string, paths []string, into func(File) error) error {
	f.mu.Lock()
	f.pulled = append(f.pulled, paths...)
	f.mu.Unlock()

	for _, p := range paths {
		file, ok := f.files[p]
		if !ok {
			continue
		}
		if err := into(file); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeStore) appliedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.applied)
}
