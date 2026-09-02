package session

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"archive/tar"
	"github.com/klauspost/compress/zstd"
	"github.com/lhns/remote-docker/core/workspace"
	"github.com/lhns/remote-docker/dircache"
	"os"
	"path/filepath"
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

// stalled is a stream whose peer never answers, which is what a wedged
// workspace looks like from here.
type stalled struct {
	closed chan struct{}
	once   sync.Once
}

func (s *stalled) Read([]byte) (int, error) {
	<-s.closed // never answers, until Close releases it
	return 0, io.EOF
}

func (s *stalled) Write(p []byte) (int, error) { return len(p), nil }

func (s *stalled) Close() error {
	s.once.Do(func() { close(s.closed) })
	return nil
}

// Every timeout on this channel was inert: `do` took no context and blocked in
// ReadString with no deadline, so every deadline dircache puts around a call
// bounded nothing, and a wedged agent hung the write-back poll for the life of
// the session.
func TestCacheChannelHonoursItsContext(t *testing.T) {
	stream := &stalled{closed: make(chan struct{})}
	c := &cacheChannel{stream: stream, r: bufio.NewReaderSize(stream, workspace.MaxCacheFrame)}

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := c.Changes(ctx, workspace.ExportCWD)
	if err == nil {
		t.Fatal("a request against a workspace that never answers returned no error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want it to carry the deadline", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("waited %s; the context was meant to bound it", elapsed)
	}

	// And the channel is CLOSED rather than left mid-exchange, because a reply
	// abandoned half read would put the next caller in the middle of a tar.
	select {
	case <-stream.closed:
	default:
		t.Error("the timed-out exchange left the channel open")
	}
}

// The batch a fill sends is a tar, optionally compressed, and the frame states
// the length of what is ACTUALLY sent -- so whatever builds the bytes has to
// encode them. A payload whose length describes the tar and whose contents are
// a zstd stream desynchronises the channel for everything after it.
func TestTarOfEncodesWhatItSays(t *testing.T) {
	root := t.TempDir()
	// Compressible on purpose: a tar of random bytes is larger compressed, and
	// the assertion below is about the encoding rather than the ratio.
	body := strings.Repeat("package main // and the same line again\n", 200)
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	entries := []dircache.Entry{{Path: "main.go", Size: int64(len(body))}}

	plain, err := tarOf(root, entries, workspace.CodecNone)
	if err != nil {
		t.Fatalf("tarOf: %v", err)
	}
	if names := tarNames(t, bytes.NewReader(plain)); len(names) != 1 || names[0] != "main.go" {
		t.Fatalf("the plain batch held %v", names)
	}

	zipped, err := tarOf(root, entries, workspace.CodecZstd)
	if err != nil {
		t.Fatalf("tarOf zstd: %v", err)
	}
	zr, err := zstd.NewReader(bytes.NewReader(zipped))
	if err != nil {
		t.Fatalf("the batch is not a zstd stream: %v", err)
	}
	if names := tarNames(t, zr); len(names) != 1 || names[0] != "main.go" {
		t.Fatalf("the compressed batch held %v", names)
	}
	zr.Close()

	// The point of doing it at all, on the kind of content a source tree is.
	if len(zipped) >= len(plain) {
		t.Errorf("compressed to %d bytes from %d, which is no saving", len(zipped), len(plain))
	}
}

// A tar cut short reads as a corrupt archive rather than as a short file, so
// the compressor's footer has to be written before the buffer is measured.
func TestTarOfClosesTheCompressor(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	zipped, err := tarOf(root, []dircache.Entry{{Path: "a.go", Size: 1}}, workspace.CodecZstd)
	if err != nil {
		t.Fatalf("tarOf: %v", err)
	}

	zr, err := zstd.NewReader(bytes.NewReader(zipped))
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	// Reading to EOF is what checks the footer: a stream missing it fails here
	// with ErrUnexpectedEOF rather than at the header.
	if _, err := io.Copy(io.Discard, zr); err != nil {
		t.Errorf("the compressed batch does not end cleanly: %v", err)
	}
}

func tarNames(t *testing.T, r io.Reader) []string {
	t.Helper()
	var names []string
	tr := tar.NewReader(r)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return names
		}
		if err != nil {
			t.Fatalf("reading the batch: %v", err)
		}
		names = append(names, header.Name)
	}
}
