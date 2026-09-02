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

	"github.com/lhns/remote-docker/core/cache"
)

// baseline is what the fill sent, for one path.
type baseline struct {
	Size    int64
	ModTime time.Time
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
)

func (k kind) String() string {
	switch k {
	case kindWrite:
		return "write"
	case kindDelete:
		return "delete"
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
func decide(
	manifest map[string]baseline,
	changes []cache.Change,
	local localAt,
	skew time.Duration,
	complete bool,
) []action {
	if !complete {
		return nil
	}

	var actions []action
	for _, change := range changes {
		base, sent := manifest[change.Path]
		info, here := local(change.Path)

		// What the fill itself put there. The cache is written THROUGH the
		// union (ADR 0044), so the filled copy of every file is in the layer
		// this reads, beside whatever the consumer wrote -- and without this
		// every round asks for the whole tree back, which is a stream large
		// enough to be refused rather than a small mistake.
		//
		// Being wrong here is cheap and self-correcting: if a timestamp did
		// not survive the round trip exactly, the file is written back with
		// the bytes it already has and the baseline moves to what both sides
		// then hold.
		if sent && !change.Deleted && change.Size == base.Size &&
			time.Unix(0, change.ModTime).Equal(base.ModTime) {
			continue
		}

		// A path the fill never sent is not the consumer's doing as far as
		// this can tell -- it may be a file it created, or one that was never
		// cached. Only the first should come back, and nothing here can tell
		// them apart, so neither does.
		if !sent {
			if change.Deleted || here {
				continue
			}
			actions = append(actions, action{Path: change.Path, kind: kindWrite})
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
			actions = append(actions, action{Path: change.Path, kind: kindWrite})
			continue
		}

		// Both sides changed, which is the only case a clock enters. Last
		// writer wins, as a plain mount would have behaved -- with the offset
		// applied rather than pretending the two clocks agree.
		containerAt := time.Unix(0, change.ModTime).Add(-skew)
		wins := containerAt.After(info.ModTime())
		actions = append(actions, action{
			Path: change.Path, kind: kindConflict, Wins: wins,
			Why: conflictReason(wins),
		})
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
