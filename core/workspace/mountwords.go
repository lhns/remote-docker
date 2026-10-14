package workspace

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// The words a mount may carry beyond read= and write= (ADR 0042). Where the
// mode decides how a mount is served, these decide what its share reports and
// does. They belong to the SHARE: one directory is one share, so every mount
// of it must agree.
//
//	owner=1000        the uid, and gid, every file in the share reports
//	owner=1000.2000   uid and gid; any one non-digit separates them
//	watch=partial     whether, and how much of, a local change is replayed

// Owner is the uid and gid a share reports for every file (ADR 0046). Set
// distinguishes a named owner from the zero value, because root is a
// legitimate answer.
type Owner struct {
	UID, GID uint32
	Set      bool
}

// ownerForms is the tail of every owner refusal.
const ownerForms = "want a uid, or a uid and a gid joined by one character that is not a digit, = or , (1000 or 1000.2000)"

// ParseOwner reads a uid, or a uid and a gid. The separator is any single
// character that is not a digit, `=` or `,`, because `-v` cannot carry a
// fourth colon (docker/cli refuses it before this program sees it) while
// `--mount` and compose can. One number is both ids.
func ParseOwner(s string) (Owner, error) {
	s = strings.TrimSpace(s)
	bad := func() (Owner, error) {
		return Owner{}, fmt.Errorf("workspace: owner %q is not valid; %s", s, ownerForms)
	}

	cut := strings.IndexFunc(s, func(r rune) bool { return r < '0' || r > '9' })
	if cut < 0 {
		id, err := strconv.ParseUint(s, 10, 32)
		if err != nil {
			return bad()
		}
		return Owner{UID: uint32(id), GID: uint32(id), Set: true}, nil
	}

	sep, size := utf8.DecodeRuneInString(s[cut:])
	if sep == '=' || sep == ',' || sep == utf8.RuneError {
		return bad()
	}
	// ParseUint takes digits only, so a sign, a second separator or trailing
	// text on either side is refused, and so is an empty side.
	uid, err := strconv.ParseUint(s[:cut], 10, 32)
	if err != nil {
		return bad()
	}
	gid, err := strconv.ParseUint(s[cut+size:], 10, 32)
	if err != nil {
		return bad()
	}
	return Owner{UID: uint32(uid), GID: uint32(gid), Set: true}, nil
}

// String is the canonical spelling, uid:gid.
func (o Owner) String() string {
	if !o.Set {
		return "unset"
	}
	return fmt.Sprintf("%d:%d", o.UID, o.GID)
}

// Watch is how much of what happens to a share on this machine is replayed
// inside the workspace (ADR 0016): nothing, what can be replayed faithfully,
// or also deletions and renames as a poke at the parent directory. Only the
// canonical words; the aliases the session setting accepts (on, true, 1) are
// not spellings of a mount's word.
type Watch string

const (
	WatchUnset   Watch = ""
	WatchOff     Watch = "off"
	WatchPartial Watch = "partial"
	WatchCoarse  Watch = "coarse"
)

// watchWords is the tail of a watch refusal.
const watchWords = "off|partial|coarse"

// ParseWatch reads one watch word.
func ParseWatch(s string) (Watch, error) {
	switch w := Watch(strings.TrimSpace(s)); w {
	case WatchOff, WatchPartial, WatchCoarse:
		return w, nil
	default:
		return WatchUnset, fmt.Errorf("workspace: watch=%q is not a watch mode; want one of %s", s, watchWords)
	}
}

// Watching reports whether this answer replays anything.
func (w Watch) Watching() bool { return w == WatchPartial || w == WatchCoarse }

// ShareWords are the words that belong to a share rather than to one mount of
// it. Comparable, so two mounts of one directory are checked with ==.
type ShareWords struct {
	Owner Owner
	Watch Watch
}

func (w Watch) display() string {
	if w == WatchUnset {
		return string(WatchOff)
	}
	return string(w)
}

// Difference is one key two sets of share words disagree about, each side in
// the spelling a person would write: Key "owner", A "1000:1000".
type Difference struct {
	Key, A, B string
}

// Differences lists each key on which w and other disagree, for refusals.
func (w ShareWords) Differences(other ShareWords) []Difference {
	var out []Difference
	if w.Owner != other.Owner {
		out = append(out, Difference{"owner", w.Owner.String(), other.Owner.String()})
	}
	if w.Watch != other.Watch {
		out = append(out, Difference{"watch", w.Watch.display(), other.Watch.display()})
	}
	return out
}

// MountWords is everything one mount can say: how it is served, and what its
// share is like.
type MountWords struct {
	Mode Mode
	ShareWords
}

// shareKeySetting maps each key beyond read and write that a mount may carry
// to the session setting that supplies its default. ParseMode, which reads the
// session's own settings, refuses these keys and names that setting.
var shareKeySetting = map[string]string{"owner": "shareOwner", "watch": "watch"}

func isShareKey(key string) bool {
	_, ok := shareKeySetting[key]
	return ok
}

// ParseMount reads a comma-separated list of read=, write=, owner=, watch= and
// Docker's own consistency words. A key named twice is refused, and so is any
// other word, naming it.
func ParseMount(s string) (MountWords, error) {
	var w MountWords
	var modeWords []string
	seen := map[string]bool{}
	for _, word := range strings.Split(s, ",") {
		word = strings.TrimSpace(word)
		if word == "" {
			continue
		}
		key, value, ok := strings.Cut(word, "=")
		if !ok || !isShareKey(key) {
			modeWords = append(modeWords, word)
			continue
		}
		if seen[key] {
			return MountWords{}, fmt.Errorf("workspace: %q names %s twice", s, key)
		}
		seen[key] = true
		switch key {
		case "owner":
			o, err := ParseOwner(value)
			if err != nil {
				return MountWords{}, err
			}
			w.Owner = o
		case "watch":
			got, err := ParseWatch(value)
			if err != nil {
				return MountWords{}, err
			}
			w.Watch = got
		}
	}
	mode, err := ParseMode(strings.Join(modeWords, ","))
	if err != nil {
		return MountWords{}, err
	}
	w.Mode = mode
	return w, nil
}
