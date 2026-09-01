package session

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/lhns/remote-docker/core/workspace"
)

// The paths of a pull or a drop travel in the JSON header line, which the
// protocol caps. A `git checkout` across a large branch is not a big request
// but a refused one, so the list is split before it is sent.
func TestChunkPathsFitsAFrame(t *testing.T) {
	var paths []string
	for i := 0; i < 40000; i++ {
		paths = append(paths, fmt.Sprintf("/src/a/very/long/directory/name/file-%06d.go", i))
	}

	batches := chunkPaths(paths)
	if len(batches) < 2 {
		t.Fatalf("40,000 paths went out in %d request(s); the frame cannot hold that", len(batches))
	}

	var seen int
	for _, b := range batches {
		encoded, err := json.Marshal(workspace.CacheRequest{
			Op: workspace.OpPull, Export: workspace.ExportCWD, Paths: b,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(encoded)+1 > workspace.MaxCacheFrame {
			t.Errorf("a batch of %d paths encodes to %d bytes, over the %d frame",
				len(b), len(encoded), workspace.MaxCacheFrame)
		}
		seen += len(b)
	}
	if seen != len(paths) {
		t.Errorf("chunked %d of %d paths", seen, len(paths))
	}
}

// Nothing to send is no requests at all, rather than one empty one -- which the
// protocol refuses by name ("no paths").
func TestChunkPathsOfNothing(t *testing.T) {
	if got := chunkPaths(nil); len(got) != 0 {
		t.Errorf("chunkPaths(nil) = %v, want no requests", got)
	}
}

// A single path longer than the budget still goes, alone: refusing it would
// drop a file from the cache for being deeply nested.
func TestChunkPathsKeepsAnOversizedPath(t *testing.T) {
	long := "/" + strings.Repeat("a", pathsPerFrame+100)
	batches := chunkPaths([]string{long, "/b.go"})
	if len(batches) != 2 || len(batches[0]) != 1 || batches[0][0] != long {
		t.Errorf("chunkPaths split an oversized path wrongly: %v", len(batches))
	}
}
