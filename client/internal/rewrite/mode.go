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

// FixUpdateWorkspace is the remedy for a workspace that cannot serve a union at
// all.
const FixUpdateWorkspace = "\n  fix: update the workspace, or use write=through"

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
// Two mounts of one directory asking for another read, write or owner are two
// shares and nothing here compares them; mounts of one variant are one share,
// and their watch and prefetch rise to the strongest asked (ADR 0042).
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

	watch, err := r.resolveWatch(source, asked.Watch, got)
	if err != nil {
		return workspace.MountWords{}, err
	}
	resolved.Watch = watch

	prefetch, err := r.resolvePrefetch(source, asked.Prefetch, got)
	if err != nil {
		return workspace.MountWords{}, err
	}
	resolved.Prefetch = prefetch

	// Refusals that need nothing from the workspace come first, then the ones
	// that ask it, and only a mount that passed them all opens a channel or
	// starts the watcher: a refused mount must leave nothing behind.
	if got.Union() {
		if err := unionAvailable(r.unionCapability(ctx)); err != nil {
			return workspace.MountWords{}, fmt.Errorf("rewrite: %s asks for %s, and %w",
				source, writeAsked(got.Write, spelled), err)
		}
	}

	key := workspace.CanonicalKey(source) + "\x00" + got.String() + "\x00" + resolved.Owner.String()
	if seen, ok := mounts[key]; ok {
		// The union, its owner and the share are settled by the first mount of
		// the variant. This one may still ask for a watch the first did not.
		if watch.Watching() {
			if err := r.startWatcher(source); err != nil {
				return workspace.MountWords{}, err
			}
		}
		seen.Watch = seen.Watch.Max(resolved.Watch)
		seen.Prefetch = seen.Prefetch.Max(resolved.Prefetch)
		mounts[key] = seen
		return resolved, nil
	}

	if got.Union() {
		wants := writeAsked(got.Write, spelled)
		channel, err := r.openCache(ctx)
		if err != nil {
			return workspace.MountWords{}, fmt.Errorf("rewrite: %s asks for %s, and %w",
				source, wants, err)
		}
		// A union's root is created by the agent as root. A workspace that
		// cannot hand it to another owner would serve a share its container
		// cannot use, and git there would refuse it as somebody else's, so
		// the mount is refused before anything is created.
		if resolved.Owner != (workspace.Owner{Set: true}) && !channel.ServesOwner() {
			return workspace.MountWords{}, fmt.Errorf(
				"rewrite: %s asks for %s with owner=%s, and this workspace cannot give a union's root an owner"+FixUpdateWorkspace,
				source, wants, resolved.Owner)
		}
	}
	if watch.Watching() {
		if err := r.startWatcher(source); err != nil {
			return workspace.MountWords{}, err
		}
	}

	mounts[key] = resolved
	return resolved, nil
}

// resolveWatch settles one mount's watch. The word, else the session's
// default, and a mount that cannot stay coherent without the watcher gets at
// least partial: a long attribute cache, or a copy in a union, with nothing
// to invalidate it is stale for good, and that fails by succeeding (ADR 0042,
// ADR 0044). Asking for off on such a mount is a contradiction and is refused.
func (r *Rewriter) resolveWatch(source string, asked workspace.Watch, mode workspace.Mode) (workspace.Watch, error) {
	cached, union := mode.Read == workspace.ReadCached, mode.Union()
	needs := cached || union

	if asked == workspace.WatchOff && needs {
		var why, fix string
		switch {
		case cached && union:
			why, fix = "read=cached and write="+string(mode.Write)+", which need the watcher to stay coherent",
				"drop watch=off, or use read=direct,write=through"
		case cached:
			why, fix = "read=cached, which needs the watcher to stay coherent", "drop watch=off, or use read=direct"
		default:
			why, fix = "write="+string(mode.Write)+", whose cache is kept honest by the watcher", "drop watch=off, or use write=through"
		}
		return workspace.WatchUnset, fmt.Errorf("rewrite: %s asks for watch=off, and %s\n  fix: %s", source, why, fix)
	}

	watch := asked
	if watch == workspace.WatchUnset {
		watch = r.Watch
	}
	if watch == workspace.WatchUnset {
		watch = workspace.WatchOff
	}
	if watch == workspace.WatchOff && needs {
		watch = workspace.WatchPartial
	}
	return watch, nil
}

// resolvePrefetch settles one mount's prefetch. The word, else the session's
// default, and off wherever the mode does not prefetch: only read=cached with a
// union has a cache to fill and permission to trust it (ADR 0045). A word
// asking for a fill where there is none is refused rather than ignored, since a
// prefetch that silently does nothing is the setting somebody then tunes.
func (r *Rewriter) resolvePrefetch(source string, asked workspace.Prefetch, mode workspace.Mode) (workspace.Prefetch, error) {
	if asked.Prefetching() && !mode.Prefetch() {
		return workspace.PrefetchUnset, fmt.Errorf("rewrite: %s asks for prefetch=%s, and %s does not prefetch\n"+
			"  fix: drop prefetch=, or use read=cached with write=back or write=ephemeral", source, asked, mode)
	}
	prefetch := asked
	if prefetch == workspace.PrefetchUnset {
		prefetch = r.Prefetch
	}
	if prefetch == workspace.PrefetchUnset || !mode.Prefetch() {
		prefetch = workspace.PrefetchOff
	}
	return prefetch, nil
}

// startWatcher makes sure the file watcher is running for a mount that needs
// it. A session that watches nothing by default starts it here, on the first
// such mount, and one that cannot start it refuses the mount rather than
// serving it stale.
func (r *Rewriter) startWatcher(source string) error {
	if r.StartWatcher == nil {
		return fmt.Errorf("rewrite: %s needs the file watcher, which this session does not run\n"+
			"  fix: use a session that serves mounts (docker remote start)", source)
	}
	if err := r.StartWatcher(); err != nil {
		return fmt.Errorf("rewrite: %s needs the file watcher, and it would not start: %w", source, err)
	}
	return nil
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
// unionCapability is UnionReady, asked again when it is not ready.
func (r *Rewriter) unionCapability(ctx context.Context) string {
	if r.UnionReady == workspace.UnionReady || r.AskUnion == nil {
		return r.UnionReady
	}
	if got, err := r.AskUnion(ctx); err == nil {
		return got
	}
	return r.UnionReady
}

func unionAvailable(reported string) error {
	switch reported {
	case workspace.UnionReady:
		return nil
	case workspace.UnionNoBinary:
		return fmt.Errorf("the daemon serving it has no %s\n"+
			"  fix: use write=through, or %s",
			"fuse-overlayfs", workspace.UnionImageRemedy)
	case workspace.UnionNoDevice:
		return fmt.Errorf("the daemon serving it has no /dev/fuse\n"+
			"  fix: use write=through, or %s", workspace.UnionDeviceRemedy)
	default:
		return fmt.Errorf("this workspace does not serve it" + FixUpdateWorkspace)
	}
}
