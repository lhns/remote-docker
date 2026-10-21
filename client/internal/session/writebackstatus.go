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
	known, carries := s.cacheCarries()
	return writeBackLines(s.cache.Reports(), known, carries)
}

// cacheCarries says whether the connection's cache channel is open and whether
// its workspace carries links and replaced directories back.
func (s *Session) cacheCarries() (known, carries bool) {
	live, ok := s.gate.currentLive()
	if !ok || live == nil {
		return false, false
	}
	// Status never waits on a channel being opened: that can take the whole
	// handshake timeout, and an open channel is what it asks about.
	if !live.cacheMu.TryLock() {
		return false, false
	}
	defer live.cacheMu.Unlock()
	if live.cacheChan == nil {
		return false, false
	}
	return true, live.cacheChan.carry
}

// writeBackLines is writeBackStatus for a set of reports.
func writeBackLines(reports []dircache.Report, carryKnown, carries bool) []string {
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
	if len(out) > 0 && carryKnown && !carries {
		out = append(out, "this workspace is too old to carry links and replaced directories back; "+
			"fix: update the workspace")
	}
	return out
}

// skippedLines names what a share's write-back will not retry, grouped by why,
// since a person needs to know what to do about each kind.
func skippedLines(local string, skipped []dircache.Skipped) []string {
	var names, links, clashes, markers []dircache.Skipped
	for _, sk := range skipped {
		switch {
		case sk.Link:
			links = append(links, sk)
		case sk.Clash:
			clashes = append(clashes, sk)
		case strings.Contains(sk.Why, "overlay marker"):
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
