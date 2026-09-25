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
// directory the client shares appears beneath it as /m/<id>, the id derived
// from its path. The working directory is no exception.
//
// One export means one server, one reverse-tunnel port and one round of NFS
// handle state, while still supporting bind sources anywhere on the client:
// another drive, above the working directory, or unrelated to it.
const (
	// ExportMountPrefix precedes every exported directory.
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

// VolumeNameForID is the Docker volume backing a share with this id on the
// given client machine. The client is in the NAME because an account's machines
// share a daemon: without it two machines with one path derive one name, the
// second create silently returns the first's volume, and a container reads
// somebody else's project.
// An empty client is the shape volumes had before, which nothing creates now.
func VolumeNameForID(client, id string) string {
	if client == "" {
		return VolumeNamePrefix + id
	}
	return VolumeNamePrefix + client + "-" + id
}

// cacheSuffix marks the volume holding a share's cache layer (ADR 0044). A
// suffix, so it is still a managed, per-client volume to everything else.
const cacheSuffix = "-cache"

// VolumeNameForCache is the volume holding a share's cache layer.
func VolumeNameForCache(client, id string) string {
	return CacheVolumeName(VolumeNameForID(client, id))
}

// CacheVolumeName is the cache layer belonging to a share's own volume, for a
// caller that already has that name and not the parts it was built from.
func CacheVolumeName(share string) string { return share + cacheSuffix }

// IsManagedVolume reports whether a volume name is one of ours. Used before
// removing anything: a volume we did not create is never ours to delete.
func IsManagedVolume(name string) bool {
	return strings.HasPrefix(name, VolumeNamePrefix)
}

// VolumeNameForExport is the volume backing an export path.
func VolumeNameForExport(client, exportPath string) (string, error) {
	id, err := parseID(exportPath)
	if err != nil {
		return "", err
	}
	return VolumeNameForID(client, id), nil
}

// CacheVolumeForExport is the cache volume a given machine's share must use,
// derived so the agent can check the name a client handed it: the client is
// the digest of the key that authenticated (ADR 0029).
func CacheVolumeForExport(client, exportPath string) (string, error) {
	name, err := VolumeNameForExport(client, exportPath)
	if err != nil {
		return "", err
	}
	return CacheVolumeName(name), nil
}

// ParseVolumeName splits a managed volume name into the client that created it
// and the share it backs. A volume from before clients were named reports an
// empty client, and is still ours.
func ParseVolumeName(name string) (client, share string, ok bool) {
	rest, ok := strings.CutPrefix(name, VolumeNamePrefix)
	if !ok {
		return "", "", false
	}

	// A cache volume is its share's, or nothing claims it and its disk is
	// never collected.
	rest = strings.TrimSuffix(rest, cacheSuffix)

	client, share, found := strings.Cut(rest, "-")
	if !found {
		// rd-<id>, from before this.
		return "", rest, validShare(rest)
	}
	if !validClient(client) {
		return "", "", false
	}
	return client, share, validShare(share)
}

// IsCacheVolume reports whether a name is a share's cache layer rather than the
// share itself.
func IsCacheVolume(name string) bool {
	return IsManagedVolume(name) && strings.HasSuffix(name, cacheSuffix)
}

// validShare reports whether a volume name suffix names a share this program
// could have created. Asked of parseID, so the rule for an id exists once.
func validShare(share string) bool {
	_, err := parseID(VolumeNamePrefix + share)
	return err == nil
}

// validClient reports whether a string could be a client id.
func validClient(s string) bool {
	if len(s) != clientIDLen {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// ValidExport reports whether a path is one this program exports, which is
// exactly /m/<id>.
func ValidExport(exportPath string) error {
	_, err := parseID(exportPath)
	return err
}

// parseID extracts the share id from an export path or a managed volume name.
func parseID(s string) (string, error) {
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
// this client's NFS export. Docker's built-in "local" driver does the mount, in
// dockerd's own namespace when the container starts, so nothing has to
// propagate and replacing a share is a container restart.
//
// soft makes a dead tunnel surface as EIO rather than parking container
// processes in uninterruptible sleep. nolock and noacl because the server
// implements neither NLM nor the NFS_ACL sideband, and without noacl the client
// probes for one on every mount and the server logs the refusal as an error.
// port == mountport skips rpcbind.
//
// timeo is DECISECONDS, so 600 is the kernel's own TCP default. It is not a
// per-request service budget: the deadline runs from transmit and includes time
// spent queued behind other requests, so a short timeo fails the whole queue at
// once instead of detecting a stall. Measured at timeo=30: 224 WRITEs in flight
// timing out together at 9.07s, with the transport never reconnecting.
//
// Every word here except nconnect is older than the supported workspace kernel;
// kernel_test.go is the table and the test. nconnect asks for that many
// connections and is emitted only when a caller names one (nconnectOption).
//
// The CAVEAT for anything transport-level: Linux keeps one RPC transport per
// server address and every share mounts from 127.0.0.1:<tunnel port>, so all of
// them SHARE one. nconnect, timeo and retrans therefore come from whichever
// share mounts first and are silently ignored for every share after it, so a
// change here can look like it did nothing until the workspace's daemon is
// restarted. (Checked 2026-09-07: a volume recording timeo=600 whose container
// had mounted timeo=30.)
//
// The attribute caching is the one part that varies, and it is what the read
// mode asks for (ADR 0042).
func NFSVolumeOptions(port int, exportPath string, read Read, nconnect int) map[string]string {
	options := append([]string{
		"addr=127.0.0.1",
		fmt.Sprintf("port=%d", port),
		fmt.Sprintf("mountport=%d", port),
		"nfsvers=3", "nolock", "noacl", "soft", "timeo=600", "retrans=2",
	}, attributeOptions(read)...)
	options = append(options, "noatime", "rsize=1048576", "wsize=1048576")
	options = append(options, nconnectOption(nconnect)...)

	return map[string]string{
		"type":   "nfs",
		"o":      strings.Join(options, ","),
		"device": ":" + exportPath,
	}
}

// NConnectMax is the most connections the NFS client accepts behind one mount.
//
// It refuses anything outside 1..16, and refusing means the whole option string
// (v6.6 fs/nfs/fs_context.c: NFS_MAX_CONNECTIONS is 16 and Opt_nconnect bounds
// the value by it; the same number in v5.3 fs/nfs/super.c, where the option
// arrived). Checked 2026-09-08, and re-checked with:
//
//	curl -s https://raw.githubusercontent.com/torvalds/linux/v6.6/fs/nfs/fs_context.c | grep -n NFS_MAX_CONNECTIONS
const NConnectMax = 16

// nconnectOption is `nconnect=n` for 2..NConnectMax and NOTHING otherwise, so
// the default list stays parseable by a kernel older than 5.3. A 1 opens no
// second transport anyway (v6.6 net/sunrpc/clnt.c, rpc_create), and an out of
// range value would make the parser refuse the whole string; the caller
// validates and reports. (Checked 2026-09-08, with the curl above and the same
// against net/sunrpc/clnt.c.)
func nconnectOption(n int) []string {
	if n < 2 || n > NConnectMax {
		return nil
	}
	return []string{fmt.Sprintf("nconnect=%d", n)}
}
