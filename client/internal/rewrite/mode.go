package rewrite

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/lhns/remote-docker/core/workspace"
)

// The mode a mount asks for is read out of the two places Docker puts it, and
// taken OUT of what is forwarded. Once the bind is a volume the words describe
// nothing the daemon can act on, and leaving them there asks a daemon that has
// never heard of them to accept an option it did not send us.
//
// Consumed UNCONDITIONALLY, rewrite or not. Docker's own words are inert on a
// daemon; ours are not, and a bind this program leaves alone would carry them
// to a daemon that rejects `read=cached` by name (ADR 0042).

// splitMode separates every mode word from the rest of a `-v` option list, and
// returns the options without them.
//
// The third field of a `-v` is a comma-separated LIST, which is why
// `-v /a:/b:ro,read=cached` is the spelling and `/a:/b:read=cached:ro` is not a bind at
// all. Every other option is carried through untouched, `ro` above all: the
// export behind the volume is read-write, so that flag is the only thing
// between a container and the user's files.
func splitMode(options string) (workspace.Mode, string, error) {
	if options == "" {
		return workspace.ModeUnset, "", nil
	}

	var words []string
	kept := make([]string, 0, 3)
	for _, opt := range strings.Split(options, ",") {
		if workspace.IsModeWord(opt) {
			words = append(words, opt)
		} else {
			kept = append(kept, opt)
		}
	}
	mode, err := workspace.ParseMode(strings.Join(words, ","))
	if err != nil {
		return workspace.ModeUnset, "", fmt.Errorf("rewrite: %w", err)
	}
	return mode, strings.Join(kept, ","), nil
}

// takeMode reads and removes a `--mount` entry's Consistency field: Docker's
// field, our values. The CLI splits `--mount` on commas, so both axes reach
// it as one csv-quoted field: `"consistency=read=cached,write=back"`.
func takeMode(mount map[string]json.RawMessage) (workspace.Mode, error) {
	raw, ok := mount["Consistency"]
	if !ok || string(raw) == "null" {
		return workspace.ModeUnset, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return workspace.ModeUnset, fmt.Errorf("rewrite: decoding mount consistency: %w", err)
	}
	mode, err := workspace.ParseMode(value)
	if err != nil {
		return workspace.ModeUnset, fmt.Errorf("rewrite: %w", err)
	}
	delete(mount, "Consistency")
	return mode, nil
}

// The remedies named more than once, so two spellings cannot drift.
const (
	fixWatchOn = "\n  fix: set watch to partial or coarse for this workspace"

	// FixUpdateWorkspace is the remedy for a workspace that cannot serve a
	// union at all, shared with the session that opens the cache channel.
	FixUpdateWorkspace = "\n  fix: update the workspace, or use write=through"
)

// modeFor is what a share gets when the mount named nothing on an axis: the
// rule for the deepest configured path containing it, and the workspace
// default otherwise.
//
// Deepest rather than first, because rules nest: a workspace set to `cached`
// with one tree pinned back to `consistent` is the case these exist for, and
// which rule wins cannot depend on map order (CLAUDE.md: never range a map to
// decide something durable).
func (r *Rewriter) modeFor(localPath string) workspace.Mode {
	key := workspace.CanonicalKey(localPath)

	best, bestLen := workspace.ModeUnset, -1
	for prefix, value := range r.ModePaths {
		p := workspace.CanonicalKey(prefix)
		if key != p && !strings.HasPrefix(key, strings.TrimSuffix(p, "/")+"/") {
			continue
		}
		if len(p) > bestLen {
			best, bestLen = value, len(p)
		}
	}
	return best.Or(r.Mode)
}

// resolveMode settles what one mount of one directory gets, and refuses what
// this client cannot serve.
//
// The volume is per SHARE, so two mounts of one directory asking for different
// things can only get one of them. Refused rather than silently resolved: the
// second EnsureVolume would recreate the volume the first just made, and both
// containers would quietly run under the second answer.
func (r *Rewriter) resolveMode(modes map[string]workspace.Mode, source string, asked workspace.Mode) (workspace.Mode, error) {
	// An axis nobody named is Docker's default for it.
	got := asked.Or(r.modeFor(source)).Or(workspace.DefaultMode)

	if got.Union() {
		if r.Cache == nil {
			return workspace.ModeUnset, fmt.Errorf(
				"rewrite: %s asks for write=%s, which needs a session that can reach the workspace's cache\n"+
					"  fix: use write=%s, which is served by the mount itself",
				source, got.Write, workspace.WriteThrough)
		}
		if !r.Watching {
			// A stronger requirement than read=cached's, and for a stronger
			// reason: that mode goes stale for at most actimeo, while a cached
			// COPY of a file that changed here is stale until something
			// removes it, and the watcher is what removes it (ADR 0044).
			return workspace.ModeUnset, fmt.Errorf(
				"rewrite: %s asks for write=%s, whose cache is kept honest by the watcher, and watching is off"+fixWatchOn,
				source, got.Write)
		}
		if err := unionAvailable(r.UnionReady); err != nil {
			return workspace.ModeUnset, fmt.Errorf("rewrite: %s asks for write=%s, and %w",
				source, got.Write, err)
		}
	}
	if got.Read == workspace.ReadCached && !r.Watching {
		return workspace.ModeUnset, fmt.Errorf(
			"rewrite: %s asks for read=%s, which needs the watcher to stay coherent, and watching is off"+fixWatchOn,
			source, got.Read)
	}

	key := workspace.CanonicalKey(source)
	if seen, ok := modes[key]; ok {
		if seen != got {
			return workspace.ModeUnset, fmt.Errorf(
				"rewrite: %s is mounted twice with different modes, %v and %v, and one directory has one mount",
				source, seen, got)
		}
		return got, nil
	}
	modes[key] = got
	return got, nil
}

// unionAvailable turns the workspace's answer into a remedy. An empty answer is
// an agent predating workspace.Info.Union, and reads as "cannot": no workspace
// served a union before that field existed.
func unionAvailable(reported string) error {
	switch reported {
	case workspace.UnionReady:
		return nil
	case workspace.UnionNoBinary:
		return fmt.Errorf("the daemon serving it has no %s\n"+
			"  fix: run the workspace's own image for per-account daemons, with WORKSPACE_DIND_IMAGE",
			"fuse-overlayfs")
	case workspace.UnionNoDevice:
		return fmt.Errorf("the daemon serving it has no /dev/fuse\n" +
			"  fix: load the fuse module on the host, and run the daemon with the device")
	default:
		return fmt.Errorf("this workspace does not serve it" + FixUpdateWorkspace)
	}
}
