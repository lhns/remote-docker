package machine

// Getting the filesystem a machine is built from.
//
// A machine IS the workspace image's filesystem (ADR 0026), and getting one out
// of an image needs docker -- which is the thing being installed. So the client
// fetches the published artifact instead, and `--rootfs` stays for the cases
// this cannot serve: an air-gapped machine, a build of your own, a version
// somebody wants to pin by hand.
//
// This is not a package manager and must not become one. What it downloads is
// ONE immutable file, named by a release tag, written whole or not at all. There
// is no upgrade path through it, because the way a machine changes version is
// that a new one is built from a different file (ADR 0026).

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// rootfsBase is where the releases live.
const rootfsBase = "https://github.com/lhns/remote-docker/releases"

// RootfsURL is where the filesystem for a version and architecture is
// published.
//
// A version that is not a release -- "dev", a commit sha, anything a local
// build reports -- takes the latest release. That is deliberate: somebody
// running a development build wants a machine to try it against, and refusing
// them one because their binary has no tag would be pedantry with no upside.
// A tagged build takes its own tag, so a machine matches the client that built
// it.
func RootfsURL(version, arch string) (string, error) {
	name, err := rootfsName(arch)
	if err != nil {
		return "", err
	}
	if isRelease(version) {
		return fmt.Sprintf("%s/download/%s/%s", rootfsBase, version, name), nil
	}
	return fmt.Sprintf("%s/latest/download/%s", rootfsBase, name), nil
}

// isRelease reports whether a version string names a release tag.
func isRelease(version string) bool {
	return strings.HasPrefix(version, "v") && strings.ContainsRune(version, '.')
}

// rootfsName is the published file for an architecture.
//
// Named rather than mapped loosely: a machine runs the same architecture as the
// computer hosting it, and quietly handing an amd64 filesystem to a Windows on
// ARM would produce a distribution that imports and then cannot execute
// anything in it.
func rootfsName(arch string) (string, error) {
	switch arch {
	case "amd64", "arm64":
		return "workspace-rootfs-" + arch + ".tar.gz", nil
	default:
		return "", fmt.Errorf("no workspace filesystem is published for %s\n"+
			"  fix: build one with `docker export` and pass it to --rootfs", arch)
	}
}

// EnsureRootfs returns a local path to the filesystem, downloading it once.
//
// Cached by name, because the file is immutable: a release tag names one build
// forever, so a file already there is the right file and re-fetching several
// hundred megabytes to learn that would be rude. `rebuild` therefore costs
// nothing after the first time.
func EnsureRootfs(ctx context.Context, version string, out io.Writer) (string, error) {
	arch := runtime.GOARCH

	url, err := RootfsURL(version, arch)
	if err != nil {
		return "", err
	}
	name, err := rootfsName(arch)
	if err != nil {
		return "", err
	}

	dir, err := rootfsCacheDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, rootfsCacheName(version, name))

	if info, err := os.Stat(path); err == nil && info.Size() > 0 {
		return path, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("preparing %s: %w", dir, err)
	}

	_, _ = fmt.Fprintf(out, "downloading the workspace filesystem for %s\n", version)
	if err := download(ctx, url, path); err != nil {
		return "", err
	}
	return path, nil
}

// rootfsCacheName keys the cache by version as well as architecture, so a
// second machine on a different version does not silently get the first one's
// filesystem.
//
// "latest" is spelled out for an untagged build, because that is what was
// actually fetched. It is the one entry that can go stale, and deleting it is
// how somebody asks for a newer one.
func rootfsCacheName(version, name string) string {
	if !isRelease(version) {
		version = "latest"
	}
	return version + "-" + name
}

// rootfsCacheDir is where downloaded filesystems are kept.
func rootfsCacheDir() (string, error) {
	dir, err := stateDir("")
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(dir), "rootfs"), nil
}

// download fetches a URL to a path, writing it whole or not at all.
//
// Through a temporary file in the same directory and a rename, which is the
// property the whole design rests on: an interrupted download leaves a partial
// file that is never mistaken for the real one, so there is no half-installed
// state to be in even though something was being installed.
func download(ctx context.Context, url, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}

	// No timeout on the whole request: this is several hundred megabytes over
	// whatever connection the user has, and a deadline generous enough to be
	// safe is one long enough to be useless as a check.
	resp, err := (&http.Client{Timeout: 0}).Do(req)
	if err != nil {
		return fmt.Errorf("downloading %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("downloading %s: %s\n"+
			"  fix: pass a filesystem with --rootfs, or check that a release for this version exists",
			url, resp.Status)
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".rootfs-*")
	if err != nil {
		return err
	}
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name()) // a no-op once the rename below succeeded
	}()

	if _, err := io.Copy(tmp, resp.Body); err != nil {
		return fmt.Errorf("downloading %s: %w", url, err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
