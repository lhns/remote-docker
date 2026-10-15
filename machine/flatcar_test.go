package machine

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha512"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeFlatcar is a zip holding a stand-in disk, and the pin that describes it.
func fakeFlatcar(t *testing.T, url string) ([]byte, flatcarSource) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(flatcarImageInZip)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte("not really a disk"))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	sum := sha512.Sum512(buf.Bytes())
	return buf.Bytes(), flatcarSource{Version: "1.2.3", URL: url, SHA512: hex.EncodeToString(sum[:])}
}

func TestEnsureFlatcarDownloadsVerifiesAndReuses(t *testing.T) {
	var hits atomic.Int32
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	var src flatcarSource
	body, src = fakeFlatcar(t, srv.URL+"/disk.zip")

	dir := t.TempDir()
	path, err := ensureFlatcar(context.Background(), src, dir, "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != "not really a disk" {
		t.Errorf("unpacked disk = %q", got)
	}

	// A second create downloads nothing, and the zip is not kept.
	again, err := ensureFlatcar(context.Background(), src, dir, "", io.Discard)
	if err != nil || again != path || hits.Load() != 1 {
		t.Errorf("second call = %q, %v after %d downloads; want the cached %q after 1", again, err, hits.Load(), path)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("cache holds %d files, want only the disk", len(entries))
	}
}

func TestEnsureFlatcarRefusesABadHashAndCachesNothing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("something else entirely"))
	}))
	defer srv.Close()
	_, src := fakeFlatcar(t, srv.URL)

	dir := t.TempDir()
	_, err := ensureFlatcar(context.Background(), src, dir, "", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "SHA512") {
		t.Fatalf("err = %v, want a SHA512 refusal", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("a refused download left %d files in the cache", len(entries))
	}
}

func TestEnsureFlatcarGoesThroughTheProxyWhenSet(t *testing.T) {
	var body []byte
	var asked atomic.Value
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Store(r.Host)
		_, _ = w.Write(body)
	}))
	defer proxy.Close()
	var src flatcarSource
	// A host that exists nowhere: only the proxy can have answered.
	body, src = fakeFlatcar(t, "http://flatcar.invalid/disk.zip")

	if _, err := ensureFlatcar(context.Background(), src, t.TempDir(), proxy.URL, io.Discard); err != nil {
		t.Fatal(err)
	}
	if asked.Load() != "flatcar.invalid" {
		t.Errorf("the proxy was asked for %v", asked.Load())
	}
}

func TestParseProxy(t *testing.T) {
	for _, ok := range []string{"http://proxy.example:3128", "https://alice:pw@proxy.example"} {
		if _, err := ParseProxy(ok); err != nil {
			t.Errorf("ParseProxy(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"proxy.example:3128", "socks5://proxy.example", "http://", `http://a"b.example`, "http://a b.example"} {
		if _, err := ParseProxy(bad); err == nil {
			t.Errorf("ParseProxy(%q) accepted it", bad)
		}
	}
}

// The bug: Hyper-V copies the file it is given to the machine's disk, and was
// given the workspace image's tarball. The image reference here is not a
// reference at all, so the old behaviour fails the test.
func TestPrepareRootfsGivesHyperVFlatcarNeverTheTarball(t *testing.T) {
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	var src flatcarSource
	body, src = fakeFlatcar(t, srv.URL)

	saved := flatcarPinned
	flatcarPinned = src
	t.Cleanup(func() { flatcarPinned = saved })
	t.Setenv("LOCALAPPDATA", t.TempDir())

	path, err := PrepareRootfs(context.Background(), "hyperv", "not a reference::", "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Ext(path) != ".vhdx" || !IsFetched(path) {
		t.Errorf("hyperv got %q, want the cached Flatcar .vhdx", path)
	}

	// The other backend still asks for the workspace image.
	if _, err := PrepareRootfs(context.Background(), "wsl", "not a reference::", "", io.Discard); err == nil ||
		!strings.Contains(err.Error(), "not a reference") {
		t.Errorf("wsl err = %v, want the workspace image to be asked for", err)
	}
}
