package dircache

// What a consumer's changes mean for the files on this machine (ADR 0044).
//
// This is the only part of the cache that writes into a user's own directory,
// so the rules are here, alone, and pure: given what the fill sent, what
// changed in the cache, and what the local file looks like now, it says what to
// do and nothing else does it.
//
// # Baselines, not clocks
//
// The manifest is what the fill put in the cache: for each path, the size and
// modification time it had on THIS machine when it was sent. That makes both
// sides answerable separately, without comparing two clocks that were never set
// together:
//
//	local  != manifest  ->  you changed it
//	cached != manifest  ->  the consumer changed it
//	cached == manifest  ->  nobody did; this is what the fill wrote
//
// A file only YOU changed produces no action at all: nothing has to come back,
// and bringing the cache up to date is invalidate.go's job rather than this
// one's.
//
// Which leaves exactly one case where a clock is needed at all -- both sides
// changed -- and that is the only place the measured offset between the two
// machines is used.
import (
	"io/fs"
	"time"
)

// Baseline is what the fill sent, for one path: the size and modification time
// it had on THIS machine when it went.
type Baseline struct {
	Size    int64
	ModTime time.Time

	// Consumer is the modification time, in Unix nanoseconds on the consumer's
	// clock, of a version this machine carried back from it. What landed here
	// keeps a time to the second at best (a tar does), so the consumer's own
	// reading of that file never equals ModTime again; without this the same
	// version reads as a new change every round and is written back for ever.
	// Zero for what the fill sent.
	Consumer int64
}

// carries reports whether a change is exactly the version this baseline stands
// for: a copy of the fill's, or one already carried back.
func (b Baseline) carries(c Change) bool {
	if c.Size != b.Size {
		return false
	}
	if b.Consumer != 0 && c.ModTime == b.Consumer {
		return true
	}
	return time.Unix(0, c.ModTime).Equal(b.ModTime)
}

// kind is what to do about one path.
type kind int

const (
	// kindWrite copies the consumer's version onto this machine.
	kindWrite kind = iota

	// kindDelete removes the file here, because the consumer removed it and you
	// did not touch it.
	kindDelete

	// kindConflict means both sides changed it. Reported whichever way it
	// resolves, because silently choosing is the one thing this must not do.
	kindConflict

	// kindLink makes the symbolic link the consumer made (action.Link).
	kindLink
)

func (k kind) String() string {
	switch k {
	case kindWrite:
		return "write"
	case kindDelete:
		return "delete"
	case kindLink:
		return "link"
	default:
		return "conflict"
	}
}

// action is one decision.
type action struct {
	Path string
	kind kind

	// Wins says which side a conflict was resolved in favour of, and is only
	// set when kind is kindConflict. True means the consumer's version is written
	// back, which is what last-writer-wins decided.
	Wins bool

	// Why is the one line a person is shown for a conflict.
	Why string

	// Link is the target of a kindLink, verbatim.
	Link string

	// Consumer is the consumer's modification time of the version a write
	// carries back (Baseline.Consumer).
	Consumer int64

	// Seen is this machine's state of the path when it was decided, and
	// LinkSeen the same for a link. A pass can run for minutes, so each is
	// checked again immediately before the path is touched, and a path that
	// changed here since is left alone.
	Seen     seenState
	LinkSeen linkState
}

// seenState is what this machine had at a path: whether it existed, and its
// size and modification time.
type seenState struct {
	Here bool
	Size int64
	Mod  time.Time
}

func seenOf(info fs.FileInfo, here bool) seenState {
	if !here || info == nil {
		return seenState{}
	}
	return seenState{Here: true, Size: info.Size(), Mod: info.ModTime()}
}

// same reports whether now is what was seen.
func (s seenState) same(now seenState) bool {
	return s.Here == now.Here && s.Size == now.Size && s.Mod.Equal(now.Mod)
}

// localAt is what this machine knows about a path: its current state, or nothing
// if it is not there.
type localAt func(path string) (fs.FileInfo, bool)

// decide works out what to do about everything the consumer changed.
//
// skew is the workspace's clock minus this machine's, measured once per
// session. It is applied ONLY to a conflict, because every other case is
// decided by comparing each side against its own baseline.
//
// complete says whether the cache holds everything the fill chose. When it does
// not, nothing is written back at all: a file the fill never sent looks exactly
// like a file the consumer created, and the cost of that mistake is content
// appearing in somebody's source tree that they never wrote.
//
// recorded is every path any batch put in the cache, in any session: one this
// run did not send and this machine no longer has was deleted here, and a
// cached copy of it is not the consumer's to bring back.
func decide(
	manifest map[string]Baseline,
	recorded map[string]bool,
	changes []Change,
	local localAt,
	skew time.Duration,
	complete bool,
) []action {
	if !complete {
		return nil
	}

	var actions []action
	seen := make(map[string]seenState, len(changes))
	for _, change := range changes {
		// A replaced directory and a link are not files to compare: the first
		// is expanded into deletions before this (expandReplaced) and the
		// second is decided by decideLinks.
		if change.Opaque || change.Link != "" {
			continue
		}
		base, sent := manifest[change.Path]
		info, here := local(change.Path)
		seen[change.Path] = seenOf(info, here)

		// What the fill itself put there, which sits in the same layer as the
		// consumer's writes (ADR 0044); without this every round asks for the
		// whole tree back. A timestamp that did not survive the round trip
		// exactly reads as the consumer's change, and the file comes back with
		// the bytes it already has.
		if sent && !change.Deleted && base.carries(change) {
			continue
		}

		// A path the fill never sent comes back only when it is new: one that
		// exists here, or was deleted, may be a file the cache never held, and
		// nothing here can tell.
		if !sent {
			if change.Deleted || here || recorded[change.Path] {
				continue
			}
			actions = append(actions, action{Path: change.Path, kind: kindWrite, Consumer: change.ModTime})
			continue
		}

		youChanged := !here || info.Size() != base.Size || !info.ModTime().Equal(base.ModTime)

		if change.Deleted {
			switch {
			case !here:
				// Already gone on both sides.
			case youChanged:
				actions = append(actions, action{
					Path: change.Path, kind: kindConflict,
					Why: "the container deleted it and you changed it; your file is kept",
				})
			default:
				actions = append(actions, action{Path: change.Path, kind: kindDelete})
			}
			continue
		}

		if !youChanged {
			actions = append(actions, action{Path: change.Path, kind: kindWrite, Consumer: change.ModTime})
			continue
		}

		if !here {
			actions = append(actions, action{
				Path: change.Path, kind: kindConflict, Wins: true, Consumer: change.ModTime,
				Why: "you deleted it and the container changed it; its version is kept",
			})
			continue
		}

		// Both sides changed, which is the only case a clock enters. Last
		// writer wins, as a plain mount would have behaved -- with the offset
		// applied rather than pretending the two clocks agree.
		containerAt := time.Unix(0, change.ModTime).Add(-skew)
		wins := containerAt.After(info.ModTime())
		actions = append(actions, action{
			Path: change.Path, kind: kindConflict, Wins: wins, Consumer: change.ModTime,
			Why: conflictReason(wins),
		})
	}
	for i := range actions {
		actions[i].Seen = seen[actions[i].Path]
	}
	return actions
}

// conflictReason is the one line a person reads, and it names a CONTAINER
// rather than a consumer: this module's word is the general one, and the thing
// on the other side of somebody's cache here is a container (ADR 0044). A
// different Store would want different wording.
func conflictReason(containerWins bool) string {
	if containerWins {
		return "both changed it; the container wrote last, so its version is kept"
	}
	return "both changed it; you wrote last, so your version is kept"
}

// writes are the paths whose contents have to be fetched.
func writes(actions []action) []string {
	var out []string
	for _, a := range actions {
		if a.kind == kindWrite || (a.kind == kindConflict && a.Wins) {
			out = append(out, a.Path)
		}
	}
	return out
}

// deletes are the paths to remove from this machine.
func deletes(actions []action) []string {
	var out []string
	for _, a := range actions {
		if a.kind == kindDelete {
			out = append(out, a.Path)
		}
	}
	return out
}

// conflicts are what a person is told about, whichever way each resolved.
func conflicts(actions []action) []action {
	var out []action
	for _, a := range actions {
		if a.kind == kindConflict {
			out = append(out, a)
		}
	}
	return out
}
