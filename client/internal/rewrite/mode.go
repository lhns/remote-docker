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
// Returns the mode, the word it was SPELLED as when that was one of Docker's
// (see asWritten), and the remaining options.
func splitMode(options string) (workspace.Mode, string, string, error) {
	if options == "" {
		return workspace.ModeUnset, "", "", nil
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
	asked := strings.Join(words, ",")
	mode, err := workspace.ParseMode(asked)
	if err != nil {
		return workspace.ModeUnset, "", "", fmt.Errorf("rewrite: %w", err)
	}
	return mode, dockerSpelling(asked), strings.Join(kept, ","), nil
}

// dockerSpelling is the word to quote back at somebody, and "" when they
// already wrote our own. Only one of Docker's whole-mode words qualifies:
// anything else is what they typed and is in the mode itself.
func dockerSpelling(asked string) string {
	if workspace.DockerWord(asked) {
		return strings.TrimSpace(asked)
	}
	return ""
}

// takeMode reads and removes a `--mount` entry's Consistency field: Docker's
// field, our values. The CLI splits `--mount` on commas, so both axes reach
// it as one csv-quoted field: `"consistency=read=cached,write=back"`.
func takeMode(mount map[string]json.RawMessage) (workspace.Mode, string, error) {
	raw, ok := mount["Consistency"]
	if !ok || string(raw) == "null" {
		return workspace.ModeUnset, "", nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return workspace.ModeUnset, "", fmt.Errorf("rewrite: decoding mount consistency: %w", err)
	}
	mode, err := workspace.ParseMode(value)
	if err != nil {
		return workspace.ModeUnset, "", fmt.Errorf("rewrite: %w", err)
	}
	delete(mount, "Consistency")
	return mode, dockerSpelling(value), nil
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
// spelled is the word the mount actually used when it was one of Docker's,
// which the refusals quote: `delegated` is write=back, and a message naming
// only write=back names something nobody typed.
func (r *Rewriter) resolveMode(modes map[string]workspace.Mode, source string, asked workspace.Mode, spelled string) (workspace.Mode, error) {
	// An axis nobody named is Docker's default for it.
	got := asked.Or(r.modeFor(source)).Or(workspace.DefaultMode)

	if got.Union() {
		wants := writeAsked(got.Write, spelled)
		if r.Cache == nil {
			return workspace.ModeUnset, fmt.Errorf(
				"rewrite: %s asks for %s, which needs a session that can reach the workspace's cache\n"+
					"  fix: use write=%s, which is served by the mount itself",
				source, wants, workspace.WriteThrough)
		}
		if !r.Watching {
			// A stronger requirement than read=cached's, and for a stronger
			// reason: that mode goes stale for at most actimeo, while a cached
			// COPY of a file that changed here is stale until something
			// removes it, and the watcher is what removes it (ADR 0044).
			return workspace.ModeUnset, fmt.Errorf(
				"rewrite: %s asks for %s, whose cache is kept honest by the watcher, and watching is off"+fixWatchOn,
				source, wants)
		}
		if err := unionAvailable(r.UnionReady); err != nil {
			return workspace.ModeUnset, fmt.Errorf("rewrite: %s asks for %s, and %w",
				source, wants, err)
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

// writeAsked names the write mode a refusal is about, and where a Docker word
// asked for it, that word too: somebody who wrote `delegated` is told
// `write=back, which delegated means`, because the union is the write axis and
// that word is the one way to reach it without naming it.
func writeAsked(write workspace.Write, spelled string) string {
	if spelled == "" {
		return "write=" + string(write)
	}
	return fmt.Sprintf("write=%s, which %s means", write, spelled)
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
			"  fix: use write=through, or run the workspace's image for per-account daemons, with WORKSPACE_DIND_IMAGE",
			"fuse-overlayfs")
	case workspace.UnionNoDevice:
		return fmt.Errorf("the daemon serving it has no /dev/fuse\n" +
			"  fix: use write=through, or load the fuse module on the host and run the daemon with the device")
	default:
		return fmt.Errorf("this workspace does not serve it" + FixUpdateWorkspace)
	}
}
