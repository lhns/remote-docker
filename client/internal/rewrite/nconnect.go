package rewrite

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/lhns/remote-docker/core/workspace"
)

// NConnectEnv asks the workspace's NFS client to open several TCP connections
// behind every share of this machine. OFF unless it is set.
//
// It REQUIRES Linux 5.3 or newer on the WORKSPACE, and nothing checks that
// before mounting: the NFS client refuses the whole option string over one word
// it does not know, so an older workspace fails every bind mount with `invalid
// argument` against a list whose every word is individually valid. That is
// acceptable only because it is opt-in, so this is read from the environment and
// has no config-file key: a workspace-side kernel gate, and a default other than
// off, are decisions for whoever measures it first.
//
// What it does is one thing and not per share: Linux keeps one RPC transport per
// server address and every share mounts from 127.0.0.1:<tunnel port>, so all of
// them share one transport and this multiplies the connections behind every
// share at once. With the fork's per-connection bound on requests in flight
// (go-nfs Server.MaxConcurrentRequests), more connections is a higher ceiling on
// what the client can have outstanding. Whether that is FASTER here is
// unmeasured.
const NConnectEnv = "REMOTE_DOCKER_NFS_NCONNECT"

// NConnect is what NConnectEnv asks for: 0 when it is unset, empty, or names one
// connection, which is what the mount does anyway.
//
// The error is for a value that cannot be honoured, and the caller logs it and
// carries on with 0 rather than failing the command: a variable a person set on
// a whim must not stop them running a container, and a mount silently carrying a
// number the kernel refuses would take every share down with it.
func NConnect() (int, error) {
	v := strings.TrimSpace(os.Getenv(NConnectEnv))
	if v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s=%q is not a number", NConnectEnv, v)
	}
	if n < 0 || n > workspace.NConnectMax {
		return 0, fmt.Errorf("%s=%d is not between 0 and %d, which is all the NFS client accepts",
			NConnectEnv, n, workspace.NConnectMax)
	}
	return n, nil
}
