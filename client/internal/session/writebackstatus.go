package session

// What `remote status` says about carrying a container's writes back (ADR 0044):
// every share is either fine or says why not, and what was left behind is named
// rather than lost.

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/lhns/remote-docker/dircache"
)

// writeBackStatus is one line per share that carries writes back, and under it
// the paths it has given up on.
func (s *Session) writeBackStatus() []string {
	if s.cache == nil {
		return nil
	}
	return writeBackLines(s.cache.Reports(), s.cacheSays())
}

// workspaceSays is what the open cache channel's workspace announced, and
// whether one is open at all.
type workspaceSays struct {
	known   bool
	carries bool // links and replaced directories (cache.Hello.Carry)
	times   bool // change times, modes and empty directories (cache.Hello.Times)
}

// cacheSays says what the connection's cache channel announced.
func (s *Session) cacheSays() workspaceSays {
	live, ok := s.gate.currentLive()
	if !ok || live == nil {
		return workspaceSays{}
	}
	// Status never waits on a channel being opened: that can take the whole
	// handshake timeout, and an open channel is what it asks about.
	if !live.cacheMu.TryLock() {
		return workspaceSays{}
	}
	defer live.cacheMu.Unlock()
	if live.cacheChan == nil {
		return workspaceSays{}
	}
	return workspaceSays{known: true, carries: live.cacheChan.carry, times: live.cacheChan.times}
}

// writeBackLines is writeBackStatus for a set of reports.
func writeBackLines(reports []dircache.Report, says workspaceSays) []string {
	sort.Slice(reports, func(i, j int) bool { return reports[i].Local < reports[j].Local })

	var out []string
	for _, r := range reports {
		if r.Ephemeral {
			continue
		}
		wb := r.WriteBack
		if wb.Stalled {
			line := fmt.Sprintf("%s: stalled since %s, %s", r.Local, wb.Since.Format(time.TimeOnly), wb.Reason)
			if wb.Pending >= 0 {
				line += fmt.Sprintf("; %d change(s) pending", wb.Pending)
			}
			if wb.FirstPath != "" {
				line += "; first: " + strings.TrimPrefix(wb.FirstPath, "/")
			}
			out = append(out, line+"; next try "+wb.Next.Format(time.TimeOnly))
		} else {
			out = append(out, r.Local+": ok")
		}
		out = append(out, skippedLines(r.Local, wb.Skipped)...)
	}
	if len(out) > 0 && says.known && !says.carries {
		out = append(out, "this workspace is too old to carry links and replaced directories back; "+
			"fix: update the workspace")
	}
	if len(out) > 0 && says.known && !says.times {
		out = append(out, "this workspace is too old to say when a container deleted a file, so those deletions are kept; "+
			"fix: update the workspace")
	}
	return out
}

// skippedLines names what a share's write-back will not retry, grouped by why,
// since a person needs to know what to do about each kind.
func skippedLines(local string, skipped []dircache.Skipped) []string {
	var names, links, clashes, markers []dircache.Skipped
	kept := map[string][]dircache.Skipped{}
	for _, sk := range skipped {
		switch sk.Kind {
		case dircache.SkipKept:
			kept[sk.Why] = append(kept[sk.Why], sk)
		case dircache.SkipLink:
			links = append(links, sk)
		case dircache.SkipClash:
			clashes = append(clashes, sk)
		case dircache.SkipMarker:
			markers = append(markers, sk)
		default:
			names = append(names, sk)
		}
	}

	var out []string
	if len(names) > 0 {
		out = append(out, fmt.Sprintf("  %s: %d file(s) this machine cannot hold are left until they change: %s",
			local, len(names), listed(names)))
	}
	if len(links) > 0 {
		out = append(out, fmt.Sprintf("  %s: %d link(s) not carried back, which this account may not create here: %s",
			local, len(links), listed(links)))
	}
	if len(clashes) > 0 {
		out = append(out, fmt.Sprintf("  %s: %d file(s) differ only in case from one already here and are left: %s",
			local, len(clashes), listed(clashes)))
	}
	if len(markers) > 0 {
		out = append(out, fmt.Sprintf("  %s: %d overlay marker(s) in the tree, safe to delete: %s",
			local, len(markers), listed(markers)))
	}
	// One line per reason: a version kept because you wrote later asks nothing
	// of you, one kept because the workspace is too old asks for an update.
	whys := make([]string, 0, len(kept))
	for why := range kept {
		whys = append(whys, why)
	}
	sort.Strings(whys)
	for _, why := range whys {
		out = append(out, fmt.Sprintf("  %s: %d container change(s) not carried back, as %s: %s",
			local, len(kept[why]), why, listed(kept[why])))
	}
	return out
}

// listed is the first few paths, so a line stays one line.
func listed(skipped []dircache.Skipped) string {
	const shown = 3
	var names []string
	for i, sk := range skipped {
		if i == shown {
			names = append(names, fmt.Sprintf("and %d more", len(skipped)-shown))
			break
		}
		names = append(names, strings.TrimPrefix(sk.Path, "/"))
	}
	return strings.Join(names, ", ")
}
