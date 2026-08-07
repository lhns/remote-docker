package workspace

import (
	"bufio"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// Info is what the workspace reports about the calling account.
//
// The wire format is the KEY=VALUE text emitted by the original
// image/bin/workspace-info shell script, and it stays that way on purpose:
// the Go client is built and tested against the existing sshd-based server
// before the Go agent replaces it, so both must speak the same thing. Once the
// agent is the only server this format can be revisited -- but not before, or
// the agent stops being a drop-in substitution.
type Info struct {
	User       string
	UID        int
	GID        int
	NFSPort    int
	Mountpoint string
	Mounted    bool
	Docker     string

	// Extra carries keys the client did not recognise. Preserving them keeps
	// an older client usable against a newer server instead of failing on a
	// field it has no opinion about.
	Extra map[string]string
}

// Wire keys, named once so the parser and the encoder cannot drift.
const (
	keyUser       = "WORKSPACE_USER"
	keyUID        = "WORKSPACE_UID"
	keyGID        = "WORKSPACE_GID"
	keyNFSPort    = "WORKSPACE_NFS_PORT"
	keyMountpoint = "WORKSPACE_MOUNTPOINT"
	keyMounted    = "WORKSPACE_MOUNTED"
	keyDocker     = "WORKSPACE_DOCKER"
)

// DockerUnavailable is what the workspace reports when it cannot reach its own
// dockerd. It is a normal answer, not a parse failure: the client wants to
// show it rather than refuse to start.
const DockerUnavailable = "unavailable"

// ParseInfo reads KEY=VALUE lines. Blank lines and # comments are skipped, and
// unrecognised keys are kept in Extra rather than rejected.
func ParseInfo(r io.Reader) (Info, error) {
	info := Info{}
	seen := map[string]bool{}

	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return Info{}, fmt.Errorf("workspace: malformed info line %q", line)
		}
		key = strings.TrimSpace(key)
		seen[key] = true

		var err error
		switch key {
		case keyUser:
			info.User = value
		case keyUID:
			info.UID, err = strconv.Atoi(value)
		case keyGID:
			info.GID, err = strconv.Atoi(value)
		case keyNFSPort:
			info.NFSPort, err = strconv.Atoi(value)
		case keyMountpoint:
			info.Mountpoint = value
		case keyMounted:
			info.Mounted, err = strconv.ParseBool(value)
		case keyDocker:
			info.Docker = value
		default:
			if info.Extra == nil {
				info.Extra = map[string]string{}
			}
			info.Extra[key] = value
		}
		if err != nil {
			return Info{}, fmt.Errorf("workspace: %s: %w", key, err)
		}
	}
	if err := sc.Err(); err != nil {
		return Info{}, fmt.Errorf("workspace: reading info: %w", err)
	}

	// Only these two are load-bearing. Without a user there is nothing to
	// address and without a port there is nothing to tunnel, and both failures
	// are far clearer here than three layers down in the mount.
	for _, k := range []string{keyUser, keyNFSPort} {
		if !seen[k] {
			return Info{}, fmt.Errorf("workspace: info is missing %s", k)
		}
	}
	if info.User == "" {
		return Info{}, fmt.Errorf("workspace: info reported an empty %s", keyUser)
	}
	if info.NFSPort < 1 || info.NFSPort > MaxPort {
		return Info{}, fmt.Errorf("workspace: info reported %s=%d, not a valid port", keyNFSPort, info.NFSPort)
	}
	return info, nil
}

// Encode writes the KEY=VALUE form ParseInfo reads. Extra keys are emitted in
// sorted order so the output is deterministic.
func (i Info) Encode(w io.Writer) error {
	docker := i.Docker
	if docker == "" {
		docker = DockerUnavailable
	}
	pairs := [][2]string{
		{keyUser, i.User},
		{keyUID, strconv.Itoa(i.UID)},
		{keyGID, strconv.Itoa(i.GID)},
		{keyNFSPort, strconv.Itoa(i.NFSPort)},
		{keyMountpoint, i.Mountpoint},
		{keyMounted, strconv.FormatBool(i.Mounted)},
		{keyDocker, docker},
	}

	extraKeys := make([]string, 0, len(i.Extra))
	for k := range i.Extra {
		extraKeys = append(extraKeys, k)
	}
	sort.Strings(extraKeys)
	for _, k := range extraKeys {
		pairs = append(pairs, [2]string{k, i.Extra[k]})
	}

	for _, p := range pairs {
		if _, err := fmt.Fprintf(w, "%s=%s\n", p[0], p[1]); err != nil {
			return err
		}
	}
	return nil
}
