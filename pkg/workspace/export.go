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
// handle state, while still supporting bind sources anywhere on the client:
// another drive, above the working directory, or unrelated to it.
const (
	// ExportRoot is the NFS export path. rclone served the working directory
	// directly; we serve a mux and mount subtrees of it.
	ExportRoot = "/"

	// ExportCWD is where the invoking working directory is always registered,
	// so the interactive shell has somewhere meaningful to land.
	ExportCWD = "/cwd"

	// ExportMountPrefix precedes every dynamically registered directory.
	ExportMountPrefix = "/m/"

	// VolumeNamePrefix precedes every Docker volume we create, so garbage
	// collection can tell ours from the user's own.
	VolumeNamePrefix = "rd-"
)

// idLen is how many hex characters of the path digest identify a share.
// 16 hex characters is 64 bits: collisions are not a practical concern, and
// the registry verifies its mapping anyway rather than trusting the digest.
const idLen = 16

// CanonicalKey folds a local path into the form used to decide whether two
// bind sources are the same directory.
//
// An identity key, NOT a path to open. Windows compares paths
// case-insensitively so the key is lowercased, and opening that form would be
// wrong on any case-sensitive filesystem.
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

// VolumeNameForID is the Docker volume name backing a share with this id, on
// the given client machine.
//
// The client is part of the NAME and not merely a label, because the daemon is
// shared between an account's machines while the files behind a share are on
// one of them. Without it both machines derive `rd-cwd` for their own working
// directory, the second create silently returns the first's volume, and a
// container comes up reading somebody else's project. That is the same failure
// ADR 0019 records across accounts, one level down.
//
// An empty client yields the old shape, which is what a volume created before
// this looks like. Those still parse and are still collectable; nothing creates
// one.
func VolumeNameForID(client, id string) string {
	if client == "" {
		return VolumeNamePrefix + id
	}
	return VolumeNamePrefix + client + "-" + id
}

// IsManagedVolume reports whether a volume name is one of ours. Used before
// removing anything: a volume we did not create is never ours to delete.
func IsManagedVolume(name string) bool {
	return strings.HasPrefix(name, VolumeNamePrefix)
}

// cwdSuffix names the volume backing the working-directory share.
const cwdSuffix = "cwd"

// VolumeNameForExport is the volume backing any export path.
//
// It handles /cwd as well as /m/<id>, which matters more than it looks:
// registering the working directory as a bind source returns the existing
// /cwd share rather than minting a second one for the same directory, so the
// commonest bind of all, `-v .:/app`, arrives here as "/cwd".
func VolumeNameForExport(client, exportPath string) (string, error) {
	if exportPath == ExportCWD {
		return VolumeNameForID(client, cwdSuffix), nil
	}
	id, err := ParseID(exportPath)
	if err != nil {
		return "", err
	}
	return VolumeNameForID(client, id), nil
}

// ParseVolumeName splits a managed volume name into the client that created it
// and the share it backs.
//
// A volume from before clients were named has no client, which is reported as
// the empty string rather than an error: it is still ours, still collectable,
// and still tells its share apart from the next one.
func ParseVolumeName(name string) (client, share string, ok bool) {
	if !IsManagedVolume(name) {
		return "", "", false
	}
	rest := strings.TrimPrefix(name, VolumeNamePrefix)

	client, share, found := strings.Cut(rest, "-")
	if !found {
		// rd-<id> or rd-cwd, from before this.
		return "", rest, validShare(rest)
	}
	if len(client) != clientIDLen || !isHex(client) {
		return "", "", false
	}
	return client, share, validShare(share)
}

// validShare reports whether a volume name suffix names a share this program
// could have created.
func validShare(share string) bool {
	if share == cwdSuffix {
		return true
	}
	return len(share) == idLen && isHex(share)
}

// isHex reports whether every character is a lowercase hex digit, which is
// what both ids are made of.
func isHex(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// ValidExport reports whether a path is one this program exports, which is
// exactly /cwd and /m/<id>.
func ValidExport(exportPath string) error {
	if exportPath == ExportCWD {
		return nil
	}
	_, err := ParseID(exportPath)
	return err
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
// this client's NFS export. Docker's built-in "local" driver does the mount;
// no volume plugin is involved.
//
// dockerd mounts it in its own namespace when the container starts, which is
// what makes per-bind volumes better than one host-side mount: nothing has to
// propagate, and replacing a share is a container restart.
//
// The options are not arbitrary. soft plus a short timeo makes a dead tunnel
// surface as EIO rather than parking container processes in uninterruptible
// sleep. nolock and noacl because the server implements neither NLM nor the
// NFS_ACL sideband, and without noacl the client probes for one on every
// mount and the server logs the refusal as an error. port == mountport skips
// rpcbind.
func NFSVolumeOptions(port int, exportPath string) map[string]string {
	return map[string]string{
		"type": "nfs",
		"o": fmt.Sprintf(
			"addr=127.0.0.1,port=%d,mountport=%d,nfsvers=3,nolock,noacl,soft,timeo=30,retrans=2,actimeo=1,noatime,rsize=1048576,wsize=1048576",
			port, port,
		),
		"device": ":" + exportPath,
	}
}
