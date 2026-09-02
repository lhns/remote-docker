package rewrite

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/lhns/remote-docker/core/workspace"
)

// The consistency a mount asks for is read out of the two places Docker puts
// it, and taken OUT of what is forwarded. Once the bind is a volume the word
// describes nothing the daemon can act on, and leaving it there asks a daemon
// that has never heard of it to accept an option it did not send us.

// splitConsistency separates Docker's consistency word from the rest of a `-v`
// option list, and returns the options without it.
//
// The third field of a `-v` is a comma-separated LIST, which is why
// `-v /a:/b:ro,cached` is the spelling and `/a:/b:cached:ro` is not a bind at
// all. Every other option is carried through untouched, `ro` above all: the
// export behind the volume is read-write, so that flag is the only thing
// between a container and the user's files.
func splitConsistency(options string) (workspace.Consistency, string) {
	if options == "" {
		return workspace.Unset, ""
	}

	consistency := workspace.Unset
	kept := make([]string, 0, 3)
	for _, opt := range strings.Split(options, ",") {
		if trimmed := strings.TrimSpace(opt); workspace.IsConsistency(trimmed) {
			// The last one wins, which is what the daemon does with a repeated
			// option and is not a case worth an error of its own.
			consistency, _ = workspace.ParseConsistency(trimmed)
			continue
		}
		kept = append(kept, opt)
	}
	return consistency, strings.Join(kept, ",")
}

// takeConsistency reads and removes a `--mount` entry's Consistency field.
func takeConsistency(mount map[string]json.RawMessage) (workspace.Consistency, error) {
	raw, ok := mount["Consistency"]
	if !ok || string(raw) == "null" {
		return workspace.Unset, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return workspace.Unset, fmt.Errorf("rewrite: decoding mount consistency: %w", err)
	}
	consistency, err := workspace.ParseConsistency(value)
	if err != nil {
		return workspace.Unset, fmt.Errorf("rewrite: %w", err)
	}
	delete(mount, "Consistency")
	return consistency, nil
}

// consistencyFor is what a share gets when the mount named nothing: the rule
// for the deepest configured path containing it, and the workspace default
// otherwise.
//
// Deepest rather than first, because rules nest: a workspace set to `cached`
// with one tree pinned back to `consistent` is the case these exist for, and
// which rule wins cannot depend on map order (CLAUDE.md: never range a map to
// decide something durable).
func (r *Rewriter) consistencyFor(localPath string) workspace.Consistency {
	key := workspace.CanonicalKey(localPath)

	best, bestLen := workspace.Unset, -1
	for prefix, value := range r.ConsistencyPaths {
		p := workspace.CanonicalKey(prefix)
		if key != p && !strings.HasPrefix(key, strings.TrimSuffix(p, "/")+"/") {
			continue
		}
		if len(p) > bestLen {
			best, bestLen = value, len(p)
		}
	}
	return best.Or(r.Consistency)
}

// resolveConsistency settles what one mount of one directory gets, and refuses
// what this client cannot serve.
//
// The volume is per SHARE, so two mounts of one directory asking for different
// things can only get one of them. Refused rather than silently resolved: the
// second EnsureVolume would recreate the volume the first just made, and both
// containers would quietly run under the second answer.
func (r *Rewriter) resolveConsistency(req *request, source string, asked workspace.Consistency) (workspace.Consistency, error) {
	got := asked.Or(r.consistencyFor(source))

	if got == workspace.Delegated {
		// Refused here, before anything is created, rather than half way
		// through a container start: the same shape as refusing a single-file
		// mount on a Docker too old for a volume subpath (ADR 0039).
		if r.Cache == nil {
			return workspace.Unset, fmt.Errorf(
				"rewrite: %s asks for the %s consistency, which needs a session that can reach the workspace's cache\n"+
					"\tfix: use %s, which is served by the mount itself",
				source, workspace.Delegated, workspace.Cached)
		}
		if !r.Watching {
			// A stronger requirement than `cached`'s, and for a stronger
			// reason: that mode goes stale for at most actimeo, while a cached
			// COPY of a file that changed here is stale until something
			// removes it, and the watcher is what removes it (ADR 0044).
			return workspace.Unset, fmt.Errorf(
				"rewrite: %s asks for the %s consistency, whose cache is kept honest by the watcher, and watching is off\n"+
					"\tfix: set watch to partial or coarse for this workspace",
				source, workspace.Delegated)
		}
		if err := unionAvailable(r.UnionReady); err != nil {
			return workspace.Unset, fmt.Errorf("rewrite: %s asks for the %s consistency, and %w",
				source, workspace.Delegated, err)
		}
	}
	if got == workspace.Cached && !r.Watching {
		return workspace.Unset, fmt.Errorf(
			"rewrite: %s asks for the %s consistency, which needs the watcher to stay coherent, and watching is off\n"+
				"\tfix: set watch to partial or coarse for this workspace",
			source, workspace.Cached)
	}

	key := workspace.CanonicalKey(source)
	if seen, ok := req.consistency[key]; ok {
		if seen != got {
			return workspace.Unset, fmt.Errorf(
				"rewrite: %s is mounted twice with different consistency, %s and %s, and one directory has one mount",
				source, seen, got)
		}
		return got, nil
	}
	if req.consistency == nil {
		req.consistency = map[string]workspace.Consistency{}
	}
	req.consistency[key] = got
	return got, nil
}

// request is what one /containers/create carries across its two mount lists.
type request struct {
	consistency map[string]workspace.Consistency
}

// unionAvailable turns the workspace's answer into a remedy.
//
// A reason rather than a boolean, because the remedies differ and "this
// workspace cannot" sends somebody to the source. An empty answer is an agent
// predating workspace.Info.Union, and reads as "cannot": no workspace served a
// union before that field existed.
func unionAvailable(reported string) error {
	switch reported {
	case workspace.UnionReady:
		return nil
	case workspace.UnionNoBinary:
		return fmt.Errorf("the daemon serving it has no %s\n"+
			"\tfix: run the workspace's own image for per-account daemons, with WORKSPACE_DIND_IMAGE",
			"fuse-overlayfs")
	case workspace.UnionNoDevice:
		return fmt.Errorf("the daemon serving it has no /dev/fuse\n" +
			"\tfix: load the fuse module on the host, and run the daemon with the device")
	default:
		return fmt.Errorf("this workspace does not serve it\n" +
			"\tfix: update the workspace, or use the cached consistency")
	}
}
