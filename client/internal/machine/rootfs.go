package machine

// Getting the filesystem a machine is built from.
//
// A machine IS the workspace image's filesystem (ADR 0026), so there is nothing
// to publish for it: the image is already in a registry, already multi-platform,
// already built and tested on every push. This pulls that image and flattens it,
// which is what `docker export` does and what `wsl --import` wants.
//
// Doing it here rather than shipping a second artifact is what makes ADR 0026's
// claim literally true -- the unit of change is the image named in the machine's
// configuration, and nothing else names a version. A published tarball would be
// a second name for the same thing, able to disagree with it.
//
// This is not a package manager and must not become one. It fetches ONE image by
// digest, writes it whole or not at all, and offers no upgrade path: a machine
// changes version by being built again from a different reference.

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// DefaultImageRepo is the published workspace image.
const DefaultImageRepo = "ghcr.io/lhns/remote-docker-workspace"

// DefaultImage is the image a machine runs when nothing names one.
//
// Pinned to the client's own version when it has one, so a machine matches the
// binary that built it. An untagged build -- "dev", a commit sha, whatever a
// local build reports -- takes `latest`, because somebody running a development
// build wants a machine to try it against and refusing them one for having no
// tag would be pedantry.
func DefaultImage(version string) string {
	if tag, ok := releaseTag(version); ok {
		return DefaultImageRepo + ":" + tag
	}
	return DefaultImageRepo + ":latest"
}

// releaseTag turns a client version into an image tag.
//
// The image is tagged without the leading v, which is what the release workflow
// pushes.
func releaseTag(version string) (string, bool) {
	if !strings.HasPrefix(version, "v") || !strings.ContainsRune(version, '.') {
		return "", false
	}
	return strings.TrimPrefix(version, "v"), true
}

// EnsureRootfs returns a local path to the filesystem for an image, pulling it
// once.
//
// Cached by DIGEST rather than by reference, which is the whole reason this is
// short. A digest names one build forever, so a file already there is the right
// file and cannot be the wrong one -- where a tag can move, and `latest` does.
// Resolving the digest is one small request; the layers are only fetched when
// nothing has that digest already.
func EnsureRootfs(ctx context.Context, image string, out io.Writer) (string, error) {
	ref, err := name.ParseReference(image)
	if err != nil {
		return "", fmt.Errorf("the workspace image %q is not a reference: %w", image, err)
	}

	// The platform of the machine, which is the platform of this computer: a
	// machine runs under a hypervisor here, not somewhere else. Asking for it
	// explicitly is what stops a Windows on ARM quietly getting an amd64
	// filesystem that imports and then executes nothing.
	platform := v1.Platform{OS: "linux", Architecture: runtime.GOARCH}

	img, err := remote.Image(ref,
		remote.WithContext(ctx),
		remote.WithPlatform(platform),
		remote.WithAuthFromKeychain(authn.DefaultKeychain))
	if err != nil {
		return "", fmt.Errorf("reading %s for linux/%s: %w\n"+
			"  fix: pass a filesystem with --rootfs if this machine cannot reach the registry",
			image, runtime.GOARCH, err)
	}

	digest, err := img.Digest()
	if err != nil {
		return "", err
	}

	dir, err := rootfsCacheDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, strings.ReplaceAll(digest.String(), ":", "-")+".tar.gz")

	if info, err := os.Stat(path); err == nil && info.Size() > 0 {
		return path, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("preparing %s: %w", dir, err)
	}

	_, _ = fmt.Fprintf(out, "fetching the workspace filesystem from %s\n", ref.Context().RepositoryStr())
	if err := extract(img, path); err != nil {
		return "", fmt.Errorf("building a filesystem from %s: %w", image, err)
	}
	return path, nil
}

// extract flattens an image into a gzipped tar, whole or not at all.
//
// Through a temporary file in the same directory and a rename: an interrupted
// pull leaves a partial file that is never mistaken for the real one, so there
// is no half-installed state to be in even though something is being installed.
//
// Gzipped because that is what `wsl --import` reads and what the machine job
// proves on every run, and because this file is kept.
func extract(img v1.Image, path string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".rootfs-*")
	if err != nil {
		return err
	}
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name()) // a no-op once the rename below succeeded
	}()

	// mutate.Extract is what `docker export` is: the layers applied in order,
	// with whiteouts resolved, as one tar. No daemon and no graph driver.
	rc := mutate.Extract(img)
	defer func() { _ = rc.Close() }()

	zw := gzip.NewWriter(tmp)
	if _, err := io.Copy(zw, rc); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// rootfsCacheDir is where pulled filesystems are kept.
//
// Beside the machines rather than inside one: the file outlives any machine
// built from it, and removing a machine must not take it.
func rootfsCacheDir() (string, error) {
	dir, err := stateDir("")
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(dir), "rootfs"), nil
}
