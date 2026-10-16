package machine

// Getting the disk a Hyper-V machine boots from.
//
// Flatcar Container Linux publishes a Hyper-V disk image as a zip. It is
// pinned below by version and SHA512, downloaded once, verified BEFORE it is
// unpacked, and kept. Like EnsureRootfs this fetches one artifact whole or not
// at all and has no upgrade path: a new Flatcar is a new pin in a new client
// release (ADR 0026).

import (
	"archive/zip"
	"cmp"
	"context"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// flatcarSource is one Flatcar release's Hyper-V image.
type flatcarSource struct {
	// Version names the cached disk, so a client pinning a newer Flatcar
	// fetches it instead of reusing the old one.
	Version string
	URL     string
	// SHA512 is the zip's, lower-case hex, from the release's .DIGESTS file.
	SHA512 string
	// MaxBytes caps the download: the zip's size plus a margin. Zero means
	// flatcarMaxDownload. The hash is checked only after the last byte, and
	// the proxy may be a caller's, so the size is bounded first.
	MaxBytes int64
}

// flatcarMaxDownload is the cap for the pinned zip (about 600 MB) with room.
const flatcarMaxDownload = 1 << 30

// flatcarDownloadTimeout bounds one download.
var flatcarDownloadTimeout = 30 * time.Minute

// flatcarPinned is the image a new Hyper-V machine boots.
//
// Checked 2026-10-15: stable 4757.2.1. The version is in the URL, not
// `current`, so the bytes cannot move under the hash. Re-check, and bump all
// three fields together, with:
//
//	curl -sL https://stable.release.flatcar-linux.net/amd64-usr/current/version.txt
//	curl -sL https://stable.release.flatcar-linux.net/amd64-usr/current/flatcar_production_hyperv_vhdx_image.vhdx.zip.DIGESTS
//
// (the SHA512 line). A stale pin keeps working, since old releases stay on the
// server; the cost is only an older Flatcar.
var flatcarPinned = flatcarSource{
	Version: "4757.2.1",
	URL:     "https://stable.release.flatcar-linux.net/amd64-usr/4757.2.1/flatcar_production_hyperv_vhdx_image.vhdx.zip",
	SHA512:  "6c1e9c1e323186a88cc15f1ab8921e2869dc8657294bcd0c422e14c93ce1c19c9cfbb20dadaf15dc00a755be4e3f425f3e43653497d77ee59953fb956d778a96",
}

// flatcarImageInZip is the one file the zip holds.
const flatcarImageInZip = "flatcar_production_hyperv_vhdx_image.vhdx"

// EnsureFlatcar returns a local path to the pinned Flatcar disk, downloading
// it into the rootfs cache once. proxy is an http(s) URL to download through,
// or "" for the environment's proxy settings.
func EnsureFlatcar(ctx context.Context, proxy string, out io.Writer) (string, error) {
	dir, err := rootfsCacheDir()
	if err != nil {
		return "", err
	}
	return ensureFlatcar(ctx, flatcarPinned, dir, proxy, out)
}

func ensureFlatcar(ctx context.Context, src flatcarSource, dir, proxy string, out io.Writer) (string, error) {
	path := filepath.Join(dir, "flatcar-"+src.Version+".vhdx")
	if info, err := os.Stat(path); err == nil && info.Size() > 0 {
		return path, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("preparing %s: %w", dir, err)
	}

	client, err := downloadClient(proxy)
	if err != nil {
		return "", err
	}

	_, _ = fmt.Fprintf(out, "downloading Flatcar %s for the machine's disk (about 600 MB)\n", src.Version)
	zipPath, err := download(ctx, client, src, dir)
	if err != nil {
		return "", fmt.Errorf("downloading Flatcar %s: %w\n"+
			"  fix: pass a downloaded disk with --rootfs, or --proxy if this machine needs one", src.Version, err)
	}
	defer func() { _ = os.Remove(zipPath) }()

	if err := unzipImage(zipPath, path); err != nil {
		return "", fmt.Errorf("unpacking Flatcar %s: %w", src.Version, err)
	}
	return path, nil
}

// downloadClient is an HTTP client for the proxy given, else for the
// environment's. No overall timeout: the file is hundreds of megabytes and the
// caller's context bounds it.
func downloadClient(proxy string) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone() // ProxyFromEnvironment
	if proxy != "" {
		u, err := ParseProxy(proxy)
		if err != nil {
			return nil, err
		}
		transport.Proxy = http.ProxyURL(u)
	}
	return &http.Client{Transport: transport}, nil
}

// ParseProxy checks a proxy setting: an http or https URL with a host.
func ParseProxy(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("the proxy %q is not an http(s) URL such as http://proxy.example:3128", raw)
	}
	// The value is written into a systemd unit between double quotes.
	if strings.ContainsAny(raw, " \t\r\n\"\\") {
		return nil, fmt.Errorf("the proxy %q holds a space, quote or backslash; percent-encode it", raw)
	}
	return u, nil
}

// download fetches the zip into dir and returns its path, deleting it unless
// its SHA512 is the pinned one. Hashed while it is written, so a bad file is
// never unpacked.
func download(ctx context.Context, client *http.Client, src flatcarSource, dir string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, flatcarDownloadTimeout)
	defer cancel()
	limit := cmp.Or(src.MaxBytes, flatcarMaxDownload)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src.URL, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s answered %s", src.URL, resp.Status)
	}

	if resp.ContentLength > limit {
		return "", fmt.Errorf("%s announces %d bytes, more than the %d allowed; refusing it", src.URL, resp.ContentLength, limit)
	}

	tmp, err := os.CreateTemp(dir, ".flatcar-*.zip")
	if err != nil {
		return "", err
	}
	fail := func(err error) (string, error) {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return "", err
	}

	h := sha512.New()
	// One byte more than the cap, so a body over it is told from one at it.
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return fail(err)
	}
	if n > limit {
		return fail(fmt.Errorf("%s sent more than the %d bytes allowed; refusing it", src.URL, limit))
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != src.SHA512 {
		return fail(fmt.Errorf("the download's SHA512 is %s, not the pinned %s; refusing it", got, src.SHA512))
	}
	if err := tmp.Close(); err != nil {
		return fail(err)
	}
	return tmp.Name(), nil
}

// unzipImage writes the zip's disk to path, whole or not at all. The entry is
// found by name and written to a path of ours: nothing in the archive decides
// where a file goes.
func unzipImage(zipPath, path string) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer func() { _ = zr.Close() }()

	var entry *zip.File
	for _, f := range zr.File {
		if f.Name == flatcarImageInZip {
			entry = f
			break
		}
	}
	if entry == nil {
		return errors.New("the archive holds no " + flatcarImageInZip)
	}
	rc, err := entry.Open()
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()

	tmp, err := os.CreateTemp(filepath.Dir(path), ".flatcar-*.vhdx")
	if err != nil {
		return err
	}
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name()) // a no-op once the rename below succeeded
	}()
	if _, err := io.Copy(tmp, rc); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// PrepareRootfs returns the local file a machine of this backend is built
// from, fetching it if nothing is cached: the workspace image's filesystem for
// WSL, Flatcar's disk for Hyper-V.
//
// Per backend because the two want different files. Hyper-V copies what it is
// given to the machine's disk, and handed the workspace tarball it built a VM
// that could not boot.
func PrepareRootfs(ctx context.Context, backend, image, proxy string, out io.Writer) (string, error) {
	if backend == "hyperv" {
		return EnsureFlatcar(ctx, proxy, out)
	}
	return EnsureRootfs(ctx, image, out)
}
