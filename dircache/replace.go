package dircache

// Directories the consumer removed or replaced, links it made, and names the
// cache cannot hold (ADR 0044). Pure, like decide.go: given what changed and
// what the fill sent, they say what the changes mean, and nothing here touches
// a disk.

import (
	"path"
	"strings"
	"time"
)

// expandReplaced turns a removed or replaced directory into the deletion of
// every file under it that the fill sent.
//
// The cache layer records a deletion at the level it happened: removing a
// directory is one whiteout for it, and replacing one is a directory marked as
// hiding what the lower held. Neither names the files, and the files are what
// this machine has. Only paths the fill sent are touched; one it never sent
// (prefetch=tree leaves most of a large tree to the live export) is not the
// cache's to take out of somebody's tree, and says so by being absent from the
// manifest. A file recreated by the consumer under the same name is listed as
// its own change and is a write, not a deletion.
//
// Each synthesised deletion goes through decide like any other, so a file
// changed here since it was sent is kept and reported rather than removed.
func expandReplaced(manifest map[string]Baseline, changes []Change) []Change {
	hidden := map[string]bool{}
	listed := map[string]bool{}
	for _, ch := range changes {
		switch {
		case ch.Opaque:
			hidden[ch.Path] = true
		case ch.Deleted:
			hidden[ch.Path] = true
			listed[ch.Path] = true
		default:
			listed[ch.Path] = true
		}
	}
	if len(hidden) == 0 {
		return changes
	}

	out := changes
	for p := range manifest {
		if listed[p] {
			continue
		}
		for dir := path.Dir(p); dir != "/" && dir != "."; dir = path.Dir(dir) {
			if hidden[dir] {
				out = append(out, Change{Path: p, Deleted: true})
				break
			}
		}
	}
	return out
}

// linkState is what this machine has at a path, not following a link.
type linkState struct {
	Exists bool
	IsLink bool
	Target string

	// ModTime is the link's own, which is what a baseline records for it.
	ModTime time.Time
}

// linkAt answers what is at a share-relative path without following it.
type linkAt func(path string) linkState

// linkBaseline is the baseline a link carries: its target's length and its own
// modification time. Two links that point at the same thing agree on both, and
// one re-pointed here does not.
func linkBaseline(target string, mod time.Time) Baseline {
	return Baseline{Size: int64(len(target)), ModTime: mod}
}

// decideLinks says what to do about the links the consumer made.
//
// A link is created where nothing is. One already there with the same target is
// agreed. One there that this write-back made and nobody has touched since is
// replaced, which is how a link the consumer re-pointed arrives. Anything else
// is this machine's own and is kept, and said so: a link where there is a file
// or a different link is a choice only the person can make.
func decideLinks(manifest map[string]Baseline, changes []Change, local linkAt) []action {
	var actions []action
	for _, ch := range changes {
		if ch.Link == "" {
			continue
		}
		here := local(ch.Path)
		switch {
		case !here.Exists:
			actions = append(actions, action{Path: ch.Path, kind: kindLink, Link: ch.Link})
		case here.IsLink && here.Target == ch.Link:
			// Already agreed.
		case here.IsLink && manifest[ch.Path] == linkBaseline(here.Target, here.ModTime) && !here.ModTime.IsZero():
			actions = append(actions, action{Path: ch.Path, kind: kindLink, Link: ch.Link})
		case here.IsLink:
			actions = append(actions, action{
				Path: ch.Path, kind: kindConflict,
				Why: "the container made a link here that points elsewhere than yours; yours is kept",
			})
		default:
			actions = append(actions, action{
				Path: ch.Path, kind: kindConflict,
				Why: "the container made a link where you have a file; your file is kept",
			})
		}
	}
	return actions
}

// linkActions are the actions to make a link at.
func linkActions(actions []action) []action {
	var out []action
	for _, a := range actions {
		if a.kind == kindLink {
			out = append(out, a)
		}
	}
	return out
}

// overlayMarker reports whether a share path has a component the union reserves
// for its own bookkeeping: fuse-overlayfs reads a name starting ".wh." as the
// deletion of another, so no file can be named so inside a union. One arriving
// here would be a marker reported as the consumer's file, which written into
// somebody's tree is a file they never made.
func overlayMarker(p string) bool {
	for _, part := range strings.Split(strings.Trim(p, "/"), "/") {
		if strings.HasPrefix(part, ".wh.") {
			return true
		}
	}
	return false
}
