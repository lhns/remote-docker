package rewrite

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/lhns/remote-docker/core/workspace"
)

// NConnectEnv sets nconnect on every NFS mount of this machine. Off unless
// set: it needs Linux 5.3 on the workspace, unchecked, and an older kernel
// fails every bind mount (workspace.nconnectOption).
const NConnectEnv = "REMOTE_DOCKER_NFS_NCONNECT"

// NConnect is what NConnectEnv asks for, 0 when unset. On error the caller
// logs and carries on with 0.
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
