package rewrite

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/lhns/remote-docker/core/workspace"
)

// A mount's words (read, write, owner, and Docker's own) are read from `-v`
// options or `--mount` Consistency and always consumed: the daemon rejects
// ours by name (ADR 0042).

// splitMode takes the words out of a `-v` option list, leaving every other
// option, `ro` above all, untouched. It returns the words, the Docker word
// used if any (for writeAsked), and the remaining options.
func splitMode(options string) (workspace.MountWords, string, string, error) {
	if options == "" {
		return workspace.MountWords{}, "", "", nil
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
	got, err := workspace.ParseMount(asked)
	if err != nil {
		return workspace.MountWords{}, "", "", fmt.Errorf("rewrite: %w", err)
	}
	return got, dockerSpelling(asked), strings.Join(kept, ","), nil
}

// dockerSpelling is the Docker word to quote back, or "" for our own words.
func dockerSpelling(asked string) string {
	if workspace.DockerWord(asked) {
		return strings.TrimSpace(asked)
	}
	return ""
}

// takeMode reads and removes a `--mount` entry's Consistency field. Both axes
// arrive csv-quoted: `"consistency=read=cached,write=back"`.
func takeMode(mount map[string]json.RawMessage) (workspace.MountWords, string, error) {
	raw, ok := mount["Consistency"]
	if !ok || string(raw) == "null" {
		return workspace.MountWords{}, "", nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return workspace.MountWords{}, "", fmt.Errorf("rewrite: decoding mount consistency: %w", err)
	}
	got, err := workspace.ParseMount(value)
	if err != nil {
		return workspace.MountWords{}, "", fmt.Errorf("rewrite: %w", err)
	}
	delete(mount, "Consistency")
	return got, dockerSpelling(value), nil
}

// withoutOurWords takes our words (read=, write=, owner=) out of an option list
// the daemon will see as it is, keeping Docker's own. A misspelt one of ours is
// refused here as it would be on a rewritten bind.
func withoutOurWords(options string) (string, error) {
	var ours, kept []string
	for _, opt := range strings.Split(options, ",") {
		if workspace.IsModeWord(opt) && !workspace.DockerWord(opt) {
			ours = append(ours, opt)
		} else {
			kept = append(kept, opt)
		}
	}
	if len(ours) == 0 {
		return options, nil
	}
	if _, err := workspace.ParseMount(strings.Join(ours, ",")); err != nil {
		return "", fmt.Errorf("rewrite: %w", err)
	}
	return strings.Join(kept, ","), nil
}

// dropOurConsistency is withoutOurWords for a `--mount` entry that is not
// rewritten, reporting whether it changed the entry.
func dropOurConsistency(mount map[string]json.RawMessage) (bool, error) {
	raw, ok := mount["Consistency"]
	if !ok || string(raw) == "null" {
		return false, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return false, nil
	}
	rest, err := withoutOurWords(value)
	if err != nil || rest == value {
		return false, err
	}
	if rest == "" {
		delete(mount, "Consistency")
		return true, nil
	}
	encoded, err := json.Marshal(rest)
	if err != nil {
		return false, err
	}
	mount["Consistency"] = encoded
	return true, nil
}

// Remedies named more than once.
const (
	fixSameWords = "\n  fix: give both mounts the same words"

	fixWatchOn = "\n  fix: set watch to partial or coarse for this workspace"

	// FixUpdateWorkspace is for a workspace that cannot serve a union at all.
	FixUpdateWorkspace = "\n  fix: update the workspace, or use write=through"
)

// modeFor is the mode for axes a mount left unset: the DEEPEST configured path
// containing it, so nested rules never depend on map order, else the default.
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

// resolveMount settles one mount's words and refuses what cannot be served.
// Two mounts of one directory disagreeing are refused: the volume is per
// share, and the second EnsureVolume would silently recreate the first's.
func (r *Rewriter) resolveMount(ctx context.Context, mounts map[string]workspace.MountWords, source string, asked workspace.MountWords, spelled string) (workspace.MountWords, error) {
	resolved := workspace.MountWords{
		Mode:       asked.Mode.Or(r.modeFor(source)).Or(workspace.DefaultMode),
		ShareWords: asked.ShareWords,
	}
	// A concrete owner always: root unless the session names another, so a
	// share's words compare without knowing where a default came from.
	if !resolved.Owner.Set {
		resolved.Owner = r.ShareOwner
	}
	if !resolved.Owner.Set {
		resolved.Owner = workspace.Owner{Set: true}
	}
	got := resolved.Mode

	if got.Union() {
		wants := writeAsked(got.Write, spelled)
		if _, err := r.openCache(ctx); err != nil {
			return workspace.MountWords{}, fmt.Errorf("rewrite: %s asks for %s, and %w",
				source, wants, err)
		}
		if !r.Watching {
			// Without the watcher a cached copy stays stale forever (ADR 0044).
			return workspace.MountWords{}, fmt.Errorf(
				"rewrite: %s asks for %s, whose cache is kept honest by the watcher, and watching is off"+fixWatchOn,
				source, wants)
		}
		if err := unionAvailable(r.UnionReady); err != nil {
			return workspace.MountWords{}, fmt.Errorf("rewrite: %s asks for %s, and %w",
				source, wants, err)
		}
	}
	if got.Read == workspace.ReadCached && !r.Watching {
		return workspace.MountWords{}, fmt.Errorf(
			"rewrite: %s asks for read=%s, which needs the watcher to stay coherent, and watching is off"+fixWatchOn,
			source, got.Read)
	}

	key := workspace.CanonicalKey(source)
	if seen, ok := mounts[key]; ok {
		if seen != resolved {
			return workspace.MountWords{}, fmt.Errorf(
				"rewrite: %s is mounted twice with different words, %s, and one directory has one share"+fixSameWords,
				source, describeDifference(seen, resolved))
		}
		return resolved, nil
	}
	mounts[key] = resolved
	return resolved, nil
}

// describeDifference names what two mounts of one directory disagree about.
func describeDifference(a, b workspace.MountWords) string {
	var parts []string
	if a.Mode != b.Mode {
		parts = append(parts, fmt.Sprintf("%v and %v", a.Mode, b.Mode))
	}
	for _, d := range a.Differences(b.ShareWords) {
		parts = append(parts, fmt.Sprintf("%s=%s and %s=%s", d.Key, d.A, d.Key, d.B))
	}
	return strings.Join(parts, ", ")
}

// openCache reaches the session's cache channel, refusing if there is none.
func (r *Rewriter) openCache(ctx context.Context) (Cache, error) {
	if r.OpenCache == nil {
		return nil, fmt.Errorf("this session cannot reach the workspace's cache\n"+
			"  fix: use write=%s, which is served by the mount itself", workspace.WriteThrough)
	}
	return r.OpenCache(ctx)
}

// writeAsked names the write mode a refusal is about, plus the Docker word that
// asked for it: `write=back, which delegated means`.
func writeAsked(write workspace.Write, spelled string) string {
	if spelled == "" {
		return "write=" + string(write)
	}
	return fmt.Sprintf("write=%s, which %s means", write, spelled)
}

// unionAvailable turns the workspace's answer into a remedy. Empty is an agent
// predating the field, which cannot serve one.
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
