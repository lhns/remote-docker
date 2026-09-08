package rewrite

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/lhns/remote-docker/core/workspace"
)

// NConnectEnv asks the workspace's NFS client to open several TCP connections
// behind every share of this machine. OFF unless it is set, and it raises the
// ceiling on requests in flight (go-nfs Server.MaxConcurrentRequests bounds them
// per connection) rather than anything measured.
//
// It needs Linux 5.3 on the WORKSPACE and nothing checks that before mounting,
// so an older one fails every bind mount (workspace.nconnectOption). Being
// opt-in is what makes that acceptable, which is why this is an environment
// variable with no config-file key: a kernel gate, and a default other than off,
// are for whoever measures it first. It is not per share either, for the reason
// workspace.NFSVolumeOptions gives.
const NConnectEnv = "REMOTE_DOCKER_NFS_NCONNECT"

// NConnect is what NConnectEnv asks for: 0 when it is unset, empty, or names one
// connection, which is what the mount does anyway.
//
// The error is for a value that cannot be honoured, and the caller logs it and
// carries on with 0 rather than failing the command: a variable a person set on
// a whim must not stop them running a container.
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
