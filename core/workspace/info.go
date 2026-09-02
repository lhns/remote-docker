package workspace

import (
	"bufio"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// InfoCommand asks the workspace for the parameters this client must agree
// with: its account, its reverse-tunnel port, the daemon it will reach.
//
// Answered from core/workspace, in the same type the client parses with, so the
// two cannot disagree about the format either.
const InfoCommand = "workspace-info"

// DialStdioCommand opens a stream to the workspace's Docker socket.
//
// `docker system dial-stdio` connects stdin and stdout to /var/run/docker.sock,
// so a session running it IS a connection to the daemon. That is what lets the
// client proxy the Docker API without a CLI in the path and without exposing a
// TCP port anywhere (ADR 0010).
//
// The agent does not run the docker CLI to serve this; it dials the socket
// itself. The command is the NAME of a request, not an instruction to execute
// something, and the spelling is docker's so that a stock `ssh` reaches the
// daemon the same way.
const DialStdioCommand = "docker system dial-stdio"

// Info is what the workspace reports about the calling account.
//
// The wire format is the KEY=VALUE text the original shell implementation
// emitted, kept so the Go agent could be a drop-in substitution for it. It
// can be revisited now that the agent is the only server.
type Info struct {
	User    string
	UID     int
	GID     int
	NFSPort int
	Docker  string

	// Agent is the version of remote-dockerd answering. Added after the
	// format was in use, which is safe: ParseInfo keeps unrecognised keys in
	// Extra, so an old client reading a new agent's reply ignores this and a
	// new client reading an old agent's sees it empty.
	Agent string

	// DaemonPaths are the paths the daemon serving this account resolves for
	// itself, so a bind naming one is left alone rather than exported from the
	// client (ADR 0041). Derived by the agent from WORKSPACE_DIND_MOUNTS.
	//
	// Empty from an agent that predates the key, which reads as "none" and is
	// exactly the old behaviour.
	DaemonPaths []string

	// Mode is how this workspace serves daemons: "shared" (ADR 0012) or
	// "per-account" (ADR 0019).
	//
	// Reported because everything else here means something different
	// depending on it: whose daemon the version and storage driver describe,
	// whether another account can see your containers, and what an operator
	// changes to alter a setting. It is set on the workspace and otherwise
	// invisible from the client.
	Mode string

	// Storage is the graph driver of the daemon serving this account.
	//
	// Reported because vfs is the difference between `docker run` taking a
	// second and taking minutes, and because nothing about it fails: it looks
	// like a hang. The answer is otherwise only visible from a daemon the
	// account deliberately cannot reach.
	//
	// Added the same way as Agent, and safe for the same reason.
	Storage string

	// Union says whether this workspace can serve a delegated share as a
	// cache: a union mount of the live export and a local cache, which needs
	// fuse-overlayfs and /dev/fuse where the account's daemon runs (ADR 0044).
	//
	// Reported rather than discovered, for the same reason as Docker: the
	// client refuses the mode by name, before it creates anything, instead of
	// failing in the middle of a container start. Empty from an agent that
	// predates the key, which reads as "cannot" and is the old behaviour.
	Union string

	// Now is the workspace's clock when it answered, in Unix nanoseconds.
	//
	// For exactly one question: when a file was changed BOTH here and in a
	// container, which side wrote last (ADR 0044). The two machines were never
	// set together, so the difference is measured rather than assumed away.
	// Zero from an agent that predates the key, which reads as "no offset" and
	// is the old behaviour.
	Now int64

	// Extra carries keys the client did not recognise. Preserving them keeps
	// an older client usable against a newer server instead of failing on a
	// field it has no opinion about.
	Extra map[string]string
}

// Wire keys, named once so the parser and the encoder cannot drift.
const (
	keyUser        = "WORKSPACE_USER"
	keyUID         = "WORKSPACE_UID"
	keyGID         = "WORKSPACE_GID"
	keyNFSPort     = "WORKSPACE_NFS_PORT"
	keyDocker      = "WORKSPACE_DOCKER"
	keyAgent       = "WORKSPACE_AGENT"
	keyStorage     = "WORKSPACE_STORAGE"
	keyMode        = "WORKSPACE_MODE"
	keyDaemonPaths = "WORKSPACE_DAEMON_PATHS"
	keyUnion       = "WORKSPACE_UNION"
	keyNow         = "WORKSPACE_NOW"
)

// What Union says. A reason rather than a boolean: "no" with nothing after it
// sends somebody to the source, and the remedy differs per cause.
const (
	// UnionReady means a delegated share can be served as a cache here.
	UnionReady = "ready"

	// UnionNoBinary means fuse-overlayfs is missing where the account's daemon
	// runs. In per-account mode that is the dind's image, whose remedy is
	// WORKSPACE_DIND_IMAGE (agent/internal/daemons/plan.go:38).
	UnionNoBinary = "no-fuse-overlayfs"

	// UnionNoDevice means /dev/fuse is not usable there, which is a kernel
	// module or a container without the device rather than an image.
	UnionNoDevice = "no-dev-fuse"
)

// What Mode says: how this workspace serves daemons. Here rather than beside
// the implementation in agent/internal/daemons, for the reason UnionReady above
// is here -- the client compares against these, so a value spelled in only one
// module is a string the other can never recognise. Nothing fails to compile
// when such a pair drifts.
const (
	// ModeShared is one dockerd for every account (ADR 0012).
	ModeShared = "shared"

	// ModePerAccount is a dockerd per enrolled account (ADR 0019), the default.
	ModePerAccount = "per-account"
)

// DockerUnavailable is what the workspace reports when it cannot reach its own
// dockerd. It is a normal answer, not a parse failure: the client wants to
// show it rather than refuse to start.
const DockerUnavailable = "unavailable"

// splitPaths reads a comma-separated list, dropping blanks so a trailing comma
// or an empty value is "none" rather than a path called "".
func splitPaths(value string) []string {
	var out []string
	for _, p := range strings.Split(value, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

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
		case keyAgent:
			info.Agent = value
		case keyStorage:
			info.Storage = value
		case keyMode:
			info.Mode = value
		case keyDaemonPaths:
			info.DaemonPaths = splitPaths(value)
		case keyUnion:
			info.Union = value
		case keyNow:
			info.Now, err = strconv.ParseInt(value, 10, 64)
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
		{keyDocker, docker},
		{keyAgent, i.Agent},
		{keyStorage, i.Storage},
		{keyMode, i.Mode},
		{keyDaemonPaths, strings.Join(i.DaemonPaths, ",")},
		{keyUnion, i.Union},
		{keyNow, strconv.FormatInt(i.Now, 10)},
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
