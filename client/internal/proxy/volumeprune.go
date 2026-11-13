package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/lhns/remote-docker/core/workspace"
)

// A volume prune is the one request whose query is decoded besides a container
// create (ADR 0005, 2026-11-13). The daemon calls a write=back union's cache
// volume unused, because a union is bound by path, so a plain prune deletes it
// under a running container. This is a convenience and not a boundary: a stock
// docker in an ssh shell, or an older client, does not pass through here.

// isVolumePrune matches POST /volumes/prune, versioned or not. `docker system
// prune --volumes` sends the same request.
func isVolumePrune(req *http.Request) bool {
	if req.Method != http.MethodPost {
		return false
	}
	_, path := splitVersion(req.URL.Path)
	return path == "/volumes/prune"
}

// splitVersion separates a leading /vMAJOR.MINOR from a path.
func splitVersion(path string) (version, rest string) {
	if len(path) > 2 && path[1] == 'v' && path[2] >= '0' && path[2] <= '9' {
		if i := strings.Index(path[1:], "/"); i > 0 {
			return path[2 : i+1], path[i+1:]
		}
	}
	return "", path
}

// versionBelow142 reports an API version older than 1.42, whose prune removes
// every local volume and not only anonymous ones, with no `all` filter.
func versionBelow142(v string) bool {
	major, minor, _ := strings.Cut(v, ".")
	maj, err := strconv.Atoi(major)
	if err != nil {
		return false
	}
	mnr, _ := strconv.Atoi(minor)
	return maj < 1 || (maj == 1 && mnr < 42)
}

// guardVolumePrune adds an exclusion of every managed volume to the request's
// filters, keeping the user's. collect reports whether this machine's collector
// should then run: only when the prune can reach non-anonymous volumes and the
// user filtered on no label, so it does no more than was asked.
func guardVolumePrune(req *http.Request) (collect bool, err error) {
	query := req.URL.Query()
	fields, err := parseFilters(query.Get("filters"))
	if err != nil {
		return false, fmt.Errorf("refusing volume prune: the filters could not be read (%v)", err)
	}

	// The daemon excludes a volume only when it has EVERY label! key listed, so
	// a second key would let managed volumes through.
	if len(fields["label!"]) > 0 {
		return false, errors.New("refusing volume prune: a label! filter cannot be combined with the exclusion of managed volumes")
	}
	userLabel := len(fields["label"]) > 0

	all := false
	for value, on := range fields["all"] {
		if ok, err := strconv.ParseBool(value); on && err == nil && ok {
			all = true
		}
	}
	version, _ := splitVersion(req.URL.Path)
	all = all || versionBelow142(version)

	fields["label!"] = map[string]bool{workspace.ManagedLabel: true}
	encoded, err := json.Marshal(fields)
	if err != nil {
		return false, fmt.Errorf("refusing volume prune: %v", err)
	}
	query.Set("filters", string(encoded))
	req.URL.RawQuery = query.Encode()
	return all && !userLabel, nil
}

// parseFilters reads moby's filters parameter: {"key":{"value":true}}, or the
// older {"key":["value"]}. Empty is no filters.
func parseFilters(raw string) (map[string]map[string]bool, error) {
	fields := map[string]map[string]bool{}
	if raw == "" {
		return fields, nil
	}
	if err := json.Unmarshal([]byte(raw), &fields); err == nil {
		if fields == nil {
			fields = map[string]map[string]bool{}
		}
		return fields, nil
	}
	legacy := map[string][]string{}
	if err := json.Unmarshal([]byte(raw), &legacy); err != nil {
		return nil, errors.New("not a filters object")
	}
	fields = make(map[string]map[string]bool, len(legacy))
	for k, values := range legacy {
		fields[k] = map[string]bool{}
		for _, v := range values {
			fields[k][v] = true
		}
	}
	return fields, nil
}
