package proxy

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// maxExplainBody bounds how much of a failed start's response is read to look
// for a share-volume mount failure. A daemon's error is a line; anything
// larger is not one and is relayed as it came.
const maxExplainBody = 64 << 10

var (
	// The path the local volume driver mounts. Our volumes are
	// rd-<client>-<share>, with an eight-hex-digit client id (core/workspace).
	mountVolumeRE = regexp.MustCompile(`volumes/(rd-([0-9a-f]{8})-[^/]+)/_data`)
	// The NFS options the driver hands to mount(2) carry the tunnel port.
	mountPortRE = regexp.MustCompile(`port=(\d+)`)
)

// MountFailure is a daemon error that names one of our share volumes and a
// refused connection to its NFS port.
type MountFailure struct {
	Volume string
	Client string
	Port   int
}

// parseMountFailure reads a daemon error body, {"message": "..."}, and reports
// whether it is a refused mount of a share volume. Anything else, including a
// body that is not JSON, is not ours to explain.
func parseMountFailure(body []byte) (MountFailure, bool) {
	var payload struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return MountFailure{}, false
	}
	msg := payload.Message

	vol := mountVolumeRE.FindStringSubmatch(msg)
	port := mountPortRE.FindStringSubmatch(msg)
	if vol == nil || port == nil || !strings.Contains(strings.ToLower(msg), "connection refused") {
		return MountFailure{}, false
	}
	n, err := strconv.Atoi(port[1])
	if err != nil || n <= 0 || n > 65535 {
		return MountFailure{}, false
	}
	return MountFailure{Volume: vol[1], Client: vol[2], Port: n}, true
}

// rewriteMessage returns body with only "message" replaced, so the other
// fields a daemon may add survive.
func rewriteMessage(body []byte, message string) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, err
	}
	m, err := json.Marshal(message)
	if err != nil {
		return nil, err
	}
	fields["message"] = m
	return json.Marshal(fields)
}
