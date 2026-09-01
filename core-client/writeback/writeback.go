// Package writeback decides what a delegated share's container changes mean
// for the files on this machine (ADR 0044).
//
// This is the only part of the cache mode that writes into a user's own
// directory, so the rules are here, alone, and pure: given what the fill sent,
// what the container changed, and what the local file looks like now, it says
// what to do and nothing else does it.
//
// # Baselines, not clocks
//
// The manifest is what the fill put in the cache: for each path, the size and
// modification time it had on THIS machine when it was sent. That makes both
// sides answerable separately, without comparing two clocks that were never set
// together:
//
//	local  != manifest  ->  you changed it
//	cached != manifest  ->  the container changed it
//	cached == manifest  ->  nobody did; this is what the fill wrote
//
// A file only YOU changed produces no action at all: nothing has to come back,
// and bringing the cache up to date is the invalidator's job rather than this
// one's (client/internal/session/invalidate.go).
//
// Which leaves exactly one case where a clock is needed at all -- both sides
// changed -- and that is the only place the measured offset between the two
// machines is used.
package writeback

import (
	"io/fs"
	"time"

	"github.com/lhns/remote-docker/core/workspace"
)

// Baseline is what the fill sent, for one path.
type Baseline struct {
	Size    int64
	ModTime time.Time
}

// Kind is what to do about one path.
type Kind int

const (
	// Write copies the container's version onto this machine.
	Write Kind = iota

	// Delete removes the file here, because the container removed it and you
	// did not touch it.
	Delete

	// Conflict means both sides changed it. Reported whichever way it
	// resolves, because silently choosing is the one thing this must not do.
	Conflict
)

func (k Kind) String() string {
	switch k {
	case Write:
		return "write"
	case Delete:
		return "delete"
	default:
		return "conflict"
	}
}

// Action is one decision.
type Action struct {
	Path string
	Kind Kind

	// Wins says which side a conflict was resolved in favour of, and is only
	// set when Kind is Conflict. True means the container's version is written
	// back, which is what last-writer-wins decided.
	Wins bool

	// Why is the one line a person is shown for a conflict.
	Why string
}

// Local is what this machine knows about a path: its current state, or nothing
// if it is not there.
type Local func(path string) (fs.FileInfo, bool)

// Decide works out what to do about everything the container changed.
//
// skew is the workspace's clock minus this machine's, measured once per
// session. It is applied ONLY to a conflict, because every other case is
// decided by comparing each side against its own baseline.
//
// complete says whether the cache holds everything the fill chose. When it does
// not, nothing is written back at all: a file the fill never sent looks exactly
// like a file the container created, and the cost of that mistake is content
// appearing in somebody's source tree that they never wrote.
func Decide(
	manifest map[string]Baseline,
	changes []workspace.CacheChange,
	local Local,
	skew time.Duration,
	complete bool,
) []Action {
	if !complete {
		return nil
	}

	var actions []Action
	for _, change := range changes {
		base, sent := manifest[change.Path]
		info, here := local(change.Path)

		// What the fill itself put there. The cache is written THROUGH the
		// union (ADR 0044), so the filled copy of every file is in the layer
		// this reads, beside whatever the container wrote -- and without this
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

		// A path the fill never sent is not the container's doing as far as
		// this can tell -- it may be a file it created, or one that was never
		// cached. Only the first should come back, and nothing here can tell
		// them apart, so neither does.
		if !sent {
			if change.Deleted || here {
				continue
			}
			actions = append(actions, Action{Path: change.Path, Kind: Write})
			continue
		}

		youChanged := !here || info.Size() != base.Size || !info.ModTime().Equal(base.ModTime)

		if change.Deleted {
			switch {
			case !here:
				// Already gone on both sides.
			case youChanged:
				actions = append(actions, Action{
					Path: change.Path, Kind: Conflict,
					Why: "the container deleted it and you changed it; your file is kept",
				})
			default:
				actions = append(actions, Action{Path: change.Path, Kind: Delete})
			}
			continue
		}

		if !youChanged {
			actions = append(actions, Action{Path: change.Path, Kind: Write})
			continue
		}

		// Both sides changed, which is the only case a clock enters. Last
		// writer wins, as a plain mount would have behaved -- with the offset
		// applied rather than pretending the two clocks agree.
		containerAt := time.Unix(0, change.ModTime).Add(-skew)
		wins := containerAt.After(info.ModTime())
		actions = append(actions, Action{
			Path: change.Path, Kind: Conflict, Wins: wins,
			Why: conflictReason(wins),
		})
	}
	return actions
}

func conflictReason(containerWins bool) string {
	if containerWins {
		return "both changed it; the container wrote last, so its version is kept"
	}
	return "both changed it; you wrote last, so your version is kept"
}

// Writes are the paths whose contents have to be fetched.
func Writes(actions []Action) []string {
	var out []string
	for _, a := range actions {
		if a.Kind == Write || (a.Kind == Conflict && a.Wins) {
			out = append(out, a.Path)
		}
	}
	return out
}

// Deletes are the paths to remove from this machine.
func Deletes(actions []Action) []string {
	var out []string
	for _, a := range actions {
		if a.Kind == Delete {
			out = append(out, a.Path)
		}
	}
	return out
}

// Conflicts are what a person is told about, whichever way each resolved.
func Conflicts(actions []Action) []Action {
	var out []Action
	for _, a := range actions {
		if a.Kind == Conflict {
			out = append(out, a)
		}
	}
	return out
}
