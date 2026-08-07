package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"runtime"
	"strings"
)

// The client serves a single NFS export whose root is synthetic: each local
// directory the client shares appears as one entry beneath it.
//
//	/cwd            -> the directory remote-docker was invoked from
//	/m/<id>         -> any other local directory named by a bind mount
//
// One export means one server, one reverse-tunnel port and one round of NFS
// handle state, while still supporting bind sources anywhere on the client --
// on another drive, above the working directory, or unrelated to it entirely.
// That is the case the previous single-mount design could not express at all.
const (
	// ExportRoot is the NFS export path. rclone served the working directory
	// directly; we serve a mux and mount subtrees of it.
	ExportRoot = "/"

	// ExportCWD is where the invoking working directory is always registered,
	// so the interactive shell has somewhere meaningful to land.
	ExportCWD = "/cwd"

	// ExportMountPrefix precedes every dynamically registered directory.
	ExportMountPrefix = "/m/"

	// VolumeNamePrefix precedes every Docker volume we create on the remote
	// daemon, so ours are distinguishable from the user's own volumes -- which
	// matters for garbage collection, and for never touching a volume we did
	// not create.
	VolumeNamePrefix = "rd-"
)

// idLen is how many hex characters of the path digest identify a share.
// 16 hex characters is 64 bits: collisions are not a practical concern, and
// the registry verifies its mapping anyway rather than trusting the digest.
const idLen = 16

// CanonicalKey folds a local path into the form used to decide whether two
// bind sources are the same directory.
//
// This is an identity key, NOT a path to open. Windows paths are compared
// case-insensitively, so the key is lowercased -- opening the lowercased form
// happens to work on Windows but would be wrong to rely on, and would be
// actively wrong on any case-sensitive filesystem.
func CanonicalKey(p string) string {
	return canonicalKeyFor(runtime.GOOS, p)
}

// canonicalKeyFor is CanonicalKey with the platform named explicitly, so the
// Windows rules can be tested from a Linux CI runner and vice versa.
func canonicalKeyFor(goos, p string) string {
	if goos != "windows" {
		return path.Clean(p)
	}

	p = strings.ReplaceAll(p, `\`, "/")

	// \\?\C:\x and \\?\UNC\server\share are extended-length forms of paths we
	// otherwise handle; strip the prefix so they key the same as the plain
	// spelling of the same directory.
	if rest, ok := strings.CutPrefix(p, "//?/"); ok {
		if unc, isUNC := strings.CutPrefix(rest, "UNC/"); isUNC {
			p = "//" + unc
		} else {
			p = rest
		}
	}

	// path.Clean collapses a leading "//" to "/", which would silently turn a
	// UNC share into a nonsense absolute path. Clean the remainder instead and
	// put the prefix back.
	if rest, ok := strings.CutPrefix(p, "//"); ok {
		return strings.ToLower("//" + strings.TrimPrefix(path.Clean("/"+rest), "/"))
	}

	// path.Clean has no notion of a drive letter and reads "C:/" as a relative
	// path, cleaning it to "C:" and losing the root. Split the drive off, clean
	// the rest as the absolute path it is, and rejoin.
	if len(p) >= 2 && p[1] == ':' && isDriveLetter(p[0]) {
		drive, rest := p[:2], p[2:]
		if rest == "" {
			return strings.ToLower(drive)
		}
		return strings.ToLower(drive + path.Clean(rest))
	}

	return strings.ToLower(path.Clean(p))
}

func isDriveLetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// ShareID is the stable identifier for a local directory, derived from its
// canonical key. The same directory always yields the same id, on this run and
// on the next one, which is what lets a reconnecting client keep its NFS
// handles and its remote volumes rather than orphaning them.
func ShareID(localPath string) string {
	sum := sha256.Sum256([]byte(CanonicalKey(localPath)))
	return hex.EncodeToString(sum[:])[:idLen]
}

// ExportPathForID is where a share with this id appears in the NFS export.
func ExportPathForID(id string) string {
	return ExportMountPrefix + id
}

// VolumeNameForID is the Docker volume name backing a share with this id.
func VolumeNameForID(id string) string {
	return VolumeNamePrefix + id
}

// IsManagedVolume reports whether a volume name is one of ours. Used before
// removing anything: a volume we did not create is never ours to delete.
func IsManagedVolume(name string) bool {
	return strings.HasPrefix(name, VolumeNamePrefix)
}

// ParseID extracts the share id from an export path or a managed volume name.
func ParseID(s string) (string, error) {
	var id string
	switch {
	case strings.HasPrefix(s, ExportMountPrefix):
		id = strings.TrimPrefix(s, ExportMountPrefix)
	case strings.HasPrefix(s, VolumeNamePrefix):
		id = strings.TrimPrefix(s, VolumeNamePrefix)
	default:
		return "", fmt.Errorf("workspace: %q is neither an export path nor a managed volume name", s)
	}
	if len(id) != idLen {
		return "", fmt.Errorf("workspace: %q does not contain a %d-character share id", s, idLen)
	}
	if _, err := hex.DecodeString(id); err != nil {
		return "", fmt.Errorf("workspace: %q does not contain a hex share id", s)
	}
	return id, nil
}

// NFSVolumeOptions returns the driver options for a Docker volume backed by
// this client's NFS export, using Docker's built-in "local" driver -- no
// volume plugin is involved.
//
// The mount happens inside dockerd's own namespace when the container starts,
// which is the property that makes per-bind volumes better than one host-side
// mount: nothing has to propagate anywhere, and replacing a share is a
// container restart rather than a remount that running containers cannot see.
//
// soft plus a short timeo means a dead tunnel surfaces as EIO instead of
// parking container processes in uninterruptible sleep. nolock because the NFS
// server implements no NLM. port == mountport skips rpcbind entirely.
func NFSVolumeOptions(port int, exportPath string) map[string]string {
	return map[string]string{
		"type": "nfs",
		"o": fmt.Sprintf(
			"addr=127.0.0.1,port=%d,mountport=%d,nfsvers=3,nolock,soft,timeo=30,retrans=2,actimeo=1,noatime,rsize=1048576,wsize=1048576",
			port, port,
		),
		"device": ":" + exportPath,
	}
}
