package dircache

// What a consumer's changes mean for the files on this machine (ADR 0044).
//
// This is the only part of the cache that writes into a user's own directory,
// so the rules are here, alone, and pure: given what the fill sent, what
// changed in the cache, and what the local file looks like now, it says what to
// do and nothing else does it.
//
// # Baselines first, change times for the rest
//
// The manifest is what the fill put in the cache: for each path, the size and
// modification time it had on THIS machine when it was sent. For a file the
// cache holds, that makes both sides answerable separately, without comparing
// two clocks that were never set together:
//
//	local  != manifest  ->  you changed it
//	cached != manifest  ->  the consumer changed it
//	cached == manifest  ->  nobody did; this is what the fill wrote
//
// A file the cache never held (every file, while prefetch is off) has no
// baseline, and is decided as a bind mount would have it: the side that changed
// it last wins, by the change time of the cache layer's entry against this
// machine's. A change time moves on every write and no tool can set it back,
// which a modification time does not promise on either side.
import (
	"cmp"
	"io/fs"
	"path"
	"slices"
	"strings"
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

// sameFile reports whether two baselines describe the same file on this
// machine: same size, same modification time.
func (b Baseline) sameFile(o Baseline) bool {
	return b.Size == o.Size && b.ModTime.Equal(o.ModTime)
}

// carries reports whether a change is the version this baseline stands for: the
// copy this machine sent, or one already carried back.
func (b Baseline) carries(c Change) bool {
	if c.Size != b.Size {
		return false
	}
	if b.Consumer != 0 && c.ModTime == b.Consumer {
		return true
	}
	return copiedTime(c.ModTime, b.ModTime)
}

// sameContent reports whether a change has the size and modification time of
// the file this machine has, which is how a COPY looks: a chown -R copies every
// file into the cache layer with its time kept (test/union-probe.sh section
// 14), and the fill's own copies are this machine's files.
func sameContent(c Change, info fs.FileInfo) bool {
	return c.Size == info.Size() && copiedTime(c.ModTime, info.ModTime())
}

// copiedTime reports whether the consumer's modification time is a copy of
// ours. Either side may hold it rounded to the second, because a tar carries
// it so: every file this machine sends lands that way.
func copiedTime(theirs int64, ours time.Time) bool {
	t := time.Unix(0, theirs)
	return t.Equal(ours) || t.Equal(ours.Round(time.Second)) || t.Round(time.Second).Equal(ours)
}

// kind is what to do about one path.
type kind int

const (
	// kindWrite copies the consumer's version onto this machine.
	kindWrite kind = iota

	// kindDelete removes the file here, because the consumer removed it and you
	// did not touch it since. A directory is removed only while empty.
	kindDelete

	// kindConflict means both sides changed it, or which side changed it last
	// cannot be told. Reported whichever way it resolves, because silently
	// choosing is the one thing this must not do.
	kindConflict

	// kindLink makes the symbolic link the consumer made (action.Link).
	kindLink

	// kindMkdir makes a directory the consumer made.
	kindMkdir

	// kindChmod gives the file here the consumer's execute bit (action.Exec).
	kindChmod

	// kindAdopt records a consumer's copy of a file this machine has as a
	// baseline: nothing changes here, and the next round does not ask again.
	kindAdopt
)

func (k kind) String() string {
	switch k {
	case kindWrite:
		return "write"
	case kindDelete:
		return "delete"
	case kindLink:
		return "link"
	case kindMkdir:
		return "mkdir"
	case kindChmod:
		return "chmod"
	case kindAdopt:
		return "adopt"
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

	// Exec is the execute bit to leave the file here with: +1 set, -1 clear, 0
	// as it is. Set on a kindChmod, and on a write whose consumer changed it.
	Exec int8

	// Dir is a kindDelete of a directory, which is removed only while empty.
	Dir bool

	// Seen is this machine's state of the path when it was decided, and
	// LinkSeen the same for a link. A pass can run for minutes, so each is
	// checked again immediately before the path is touched, and a path that
	// changed here since is left alone.
	Seen     seenState
	LinkSeen linkState
}

// seenState is what this machine had at a path: whether it existed, its size
// and modification time, and its change time once a decision read it.
type seenState struct {
	Here  bool
	Size  int64
	Mod   time.Time
	Ctime time.Time
}

func seenOf(info fs.FileInfo, here bool) seenState {
	if !here || info == nil {
		return seenState{}
	}
	return seenState{Here: true, Size: info.Size(), Mod: info.ModTime()}
}

// same reports whether now is what was seen. A change time is compared when the
// decision read one, so a chmod made here since is seen too.
func (s seenState) same(now seenState) bool {
	return s.Here == now.Here && s.Size == now.Size && s.Mod.Equal(now.Mod) &&
		(s.Ctime.IsZero() || s.Ctime.Equal(now.Ctime))
}

// localAt is what this machine knows about a path: its current state, or nothing
// if it is not there.
type localAt func(path string) (fs.FileInfo, bool)

// ctimeAt is when a path last changed on this machine, by any write, rename or
// change of mode, which no tool can set back. False is not known; nil knows
// nothing.
type ctimeAt func(path string) (time.Time, bool)

// The reasons a person reads for a version kept here. Each names a CONTAINER
// rather than a consumer: this module's word is the general one, and the thing
// on the other side of somebody's cache here is a container (ADR 0044). A
// different Store would want different wording.
const (
	whyYouLater        = "you changed it here after the container did; your version is kept"
	whyYouLaterDeleted = "you changed it here after the container deleted it; your file is kept"
	whyNoChangeTime    = "when it last changed here cannot be read; your version is kept"
	whyOldWorkspace    = "the container deleted it, and this workspace is too old to say when; update the workspace"
	whyDirHere         = "the container has a file where you have a directory; your directory is kept"
	whyFileHere        = "the container made a directory where you have a file; your file is kept"
)

// decide works out what to do about everything the consumer changed.
//
// Last writer wins, as a bind mount would have it (ADR 0044):
//
//   - A copy is not a change: a version with the size and time of the file
//     here does nothing but carry the consumer's execute bit, if it changed.
//   - A file the cache holds is compared against its baseline, each side
//     against its own, so a clock is needed only when both changed it.
//   - A file it never held that this machine has is the consumer's if the
//     consumer changed it LAST, by the change times of both sides; otherwise
//     this machine's file is kept and said. A new file is the consumer's,
//     unless the record says it was deleted here.
//
// A time that is not known never writes or deletes anything: the file here is
// kept and said. An older consumer that sends no change time still carries a
// changed file back by its modification time, because a file untouched here
// for days is the common case; its deletions are kept.
//
// skew is the workspace's clock minus this machine's, measured once per
// session, applied to every comparison of the two clocks. margin is how much
// later the consumer must be before a file the cache never held is deleted or
// overwritten here: the skew is known to within the link's latency, and in
// doubt this machine wins.
//
// exec is the execute bits the consumer sees as they are here, after the
// share's mask (ADR 0046); zero where this machine has none, which is Windows,
// and then modes are neither compared nor carried.
//
// recorded is every path any batch put in the cache, in any session: one this
// run did not send and this machine no longer has was deleted here, and a
// cached copy of it is not the consumer's to bring back.
func decide(
	manifest map[string]Baseline,
	recorded map[string]bool,
	changes []Change,
	local localAt,
	ctime ctimeAt,
	skew, margin time.Duration,
	exec fs.FileMode,
) []action {
	var actions []action
	seen := make(map[string]seenState, len(changes))
	// held says whether the record has a path at p or under it: a directory
	// that held files this machine sent, and that is gone here, was removed
	// here, and an empty one the cache still has is its skeleton, not one the
	// consumer made. Built on first use.
	var holding map[string]bool
	held := func(p string) bool {
		if holding == nil {
			holding = map[string]bool{}
			for r := range recorded {
				for d := r; d != "/" && d != "."; d = path.Dir(d) {
					holding[d] = true
				}
			}
		}
		return holding[p]
	}
	for _, change := range changes {
		// A replaced directory and a link are not files to compare: the first
		// is expanded into deletions before this (replacedDeletions) and the
		// second is decided by decideLinks.
		if change.Link != "" {
			continue
		}
		if change.Opaque {
			// fuse-overlayfs may mark a directory the container MADE as one
			// that hides the lower (test/union-probe.sh section 14), so it
			// comes as Opaque rather than as an empty Dir. Either way the
			// container has a directory there, and one this machine lacks is
			// made, unless it is the skeleton of one removed here.
			if _, here := local(change.Path); !here && !held(change.Path) {
				actions = append(actions, action{Path: change.Path, kind: kindMkdir})
			}
			continue
		}
		p := change.Path
		base, sent := manifest[p]
		info, here := local(p)
		st := seenOf(info, here)

		// lastWriter compares the consumer's time with this machine's change
		// time, reading the latter only when a decision needs it, and calls
		// the consumer last only when it is later by more than by. ok is false
		// when either is not known.
		lastWriter := func(theirs int64, by time.Duration) (containerLast, ok bool) {
			if theirs == 0 || ctime == nil {
				return false, false
			}
			ours, known := ctime(p)
			if !known {
				return false, false
			}
			st.Ctime = ours
			return consumerLater(theirs, ours, skew, by), true
		}
		add := func(a action) {
			a.Path = p
			actions = append(actions, a)
		}
		keep := func(why string) { add(action{kind: kindConflict, Why: why}) }

		switch {
		case change.Dir:
			switch {
			case !here:
				if !held(p) {
					add(action{kind: kindMkdir})
				}
			case !info.IsDir():
				keep(whyFileHere)
			}

		case here && info.IsDir():
			if !change.Deleted {
				keep(whyDirHere)
				break
			}
			// A directory the consumer removed, emptied first by the deletions
			// its removal implies; removing it never takes a file.
			if last, ok := lastWriter(change.Changed, margin); ok && last {
				add(action{kind: kindDelete, Dir: true})
			}

		case here && !change.Deleted && sameContent(change, info):
			if flip := execFlip(change, info, exec); flip != 0 {
				last, ok := lastWriter(change.Changed, margin)
				switch {
				case !ok:
					keep(whyNoChangeTime)
				case last:
					add(action{kind: kindChmod, Exec: flip})
				default:
					keep(whyYouLater)
				}
			} else if !sent {
				add(action{kind: kindAdopt, Consumer: change.ModTime})
			}

		case sent && !change.Deleted && base.carries(change):
			// What the fill itself put there, which sits in the same layer as
			// the consumer's writes (ADR 0044); without this every round asks
			// for the whole tree back.

		case !sent && !here:
			if !change.Deleted && !recorded[p] {
				add(action{kind: kindWrite, Consumer: change.ModTime})
			}

		case !sent && change.Deleted:
			last, ok := lastWriter(change.Changed, margin)
			switch {
			case change.Changed == 0:
				keep(whyOldWorkspace)
			case !ok:
				keep(whyNoChangeTime)
			case last:
				add(action{kind: kindDelete})
			default:
				keep(whyYouLaterDeleted)
			}

		case !sent:
			// Changed by the consumer, a file the cache never held: its change
			// time, or from an older consumer its modification time, against
			// this machine's change time.
			theirs := cmp.Or(change.Changed, change.ModTime)
			last, ok := lastWriter(theirs, margin)
			switch {
			case !ok:
				keep(whyNoChangeTime)
			case last:
				add(action{kind: kindWrite, Consumer: change.ModTime, Exec: execFlip(change, info, exec)})
			default:
				keep(whyYouLater)
			}

		default:
			decideHeld(change, base, info, here, lastWriter, skew, exec, add)
		}
		seen[p] = st
	}
	for i := range actions {
		actions[i].Seen = seen[actions[i].Path]
	}
	return actions
}

// decideHeld is the table for a file the cache holds, each side compared
// against the baseline the fill sent.
func decideHeld(change Change, base Baseline, info fs.FileInfo, here bool,
	lastWriter func(int64, time.Duration) (bool, bool), skew time.Duration, exec fs.FileMode, add func(action)) {
	youChanged := !here || info.Size() != base.Size || !info.ModTime().Equal(base.ModTime)

	if change.Deleted {
		switch {
		case !here:
			// Already gone on both sides.
		case youChanged:
			add(action{kind: kindConflict, Why: "the container deleted it and you changed it; your file is kept"})
		default:
			add(action{kind: kindDelete})
		}
		return
	}

	if !youChanged {
		add(action{kind: kindWrite, Consumer: change.ModTime, Exec: execFlip(change, info, exec)})
		return
	}

	if !here {
		add(action{
			kind: kindConflict, Wins: true, Consumer: change.ModTime,
			Why: "you deleted it and the container changed it; its version is kept",
		})
		return
	}

	// Both sides changed, which is the only case a clock enters. Last writer
	// wins, as a plain mount would have behaved, by the change times where both
	// are known and otherwise by the modification times, with the offset
	// applied rather than pretending the two clocks agree. The change times are
	// what invalidation decided by too (Cache.invalidate), so the two never
	// disagree about one file.
	wins, ok := lastWriter(change.Changed, 0)
	if !ok {
		wins = time.Unix(0, change.ModTime).Add(-skew).After(info.ModTime())
	}
	a := action{kind: kindConflict, Wins: wins, Consumer: change.ModTime, Why: conflictReason(wins)}
	if wins {
		a.Exec = execFlip(change, info, exec)
	}
	add(a)
}

// consumerLater reports whether the consumer's time, on its own clock, is later
// than this machine's by more than by, once the measured skew is taken off.
func consumerLater(theirs int64, ours time.Time, skew, by time.Duration) bool {
	return time.Unix(0, theirs).Add(-skew).After(ours.Add(by))
}

// execFlip is the execute bit to give the file here so it matches the
// consumer's: +1 to set it, -1 to clear it, 0 when they already agree or modes
// are not known. The share reports the execute bit as the file has it and every
// other bit wide (ADR 0046), so the execute bit is the one a consumer can
// really change.
func execFlip(c Change, info fs.FileInfo, exec fs.FileMode) int8 {
	if exec == 0 || c.Changed == 0 || info == nil {
		return 0
	}
	theirs, ours := c.Mode&exec != 0, info.Mode()&0o111 != 0
	switch {
	case theirs == ours:
		return 0
	case theirs:
		return 1
	default:
		return -1
	}
}

// withExec is perm with its execute bits set where it can be read, or cleared:
// what chmod +x and chmod -x leave a file with.
func withExec(perm fs.FileMode, exec int8) fs.FileMode {
	switch {
	case exec > 0:
		return perm | (perm&0o444)>>2
	case exec < 0:
		return perm &^ 0o111
	}
	return perm
}

// conflictReason is the one line a person reads for a file both sides changed.
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

// deletes are the paths to remove from this machine, deepest first, so a
// directory is emptied by the deletions under it before its own.
func deletes(actions []action) []string {
	var out []string
	for _, a := range actions {
		if a.kind == kindDelete {
			out = append(out, a.Path)
		}
	}
	slices.SortStableFunc(out, func(a, b string) int {
		return strings.Count(b, "/") - strings.Count(a, "/")
	})
	return out
}

// ofKind are the actions of one kind: the conflicts a person is told about,
// whichever way each resolved, or the links to make.
func ofKind(actions []action, k kind) []action {
	var out []action
	for _, a := range actions {
		if a.kind == k {
			out = append(out, a)
		}
	}
	return out
}
