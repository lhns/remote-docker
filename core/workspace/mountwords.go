package workspace

import (
	"fmt"
	"io/fs"
	"strconv"
	"strings"
	"unicode/utf8"
)

// The words a mount may carry beyond read= and write= (ADR 0042). Where the
// mode decides how a mount is served, these decide what its share reports and
// does. owner=, fmask= and dmask= are identity: a directory mounted with
// another owner or mask is another share. watch= and prefetch= are effort spent
// on one share, which runs at the highest level any mount of it asked for (Max,
// ADR 0042).
//
//	owner=1000        the uid, and gid, every file in the share reports
//	owner=1000.2000   uid and gid; any one non-digit separates them
//	fmask=0111        permission bits cleared from every file's reported mode
//	dmask=022         permission bits cleared from every directory's
//	watch=partial     whether, and how much of, a local change is replayed
//	prefetch=tree     how a union's cache is filled ahead of reads

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

// Mask is permission bits cleared from the modes a share reports, as FAT's
// and ntfs-3g's fmask and dmask do (ADR 0046). Set distinguishes a named 0,
// which overrides a session default, from no word at all.
type Mask struct {
	Bits fs.FileMode
	Set  bool
}

// maskForms is the tail of every mask refusal.
const maskForms = "want 1 to 4 octal digits, at most 0777 (0111, 022)"

// ParseFileMask reads fmask=, an octal mask as umask spells one.
func ParseFileMask(s string) (Mask, error) { return parseMask("fmask", s) }

// ParseDirMask reads dmask=, refusing a mask that clears every execute bit: a
// directory nobody can enter is a share nobody can use.
func ParseDirMask(s string) (Mask, error) {
	m, err := parseMask("dmask", s)
	if err != nil {
		return Mask{}, err
	}
	if m.Bits&0o111 == 0o111 {
		return Mask{}, fmt.Errorf("workspace: dmask %s clears every execute bit, so no directory in the share could be entered; leave at least one", m)
	}
	return m, nil
}

func parseMask(name, s string) (Mask, error) {
	s = strings.TrimSpace(s)
	// Checked by hand because ParseUint also takes a sign and underscores.
	if len(s) < 1 || len(s) > 4 || strings.Trim(s, "01234567") != "" {
		return Mask{}, fmt.Errorf("workspace: %s %q is not valid; %s", name, s, maskForms)
	}
	bits, err := strconv.ParseUint(s, 8, 32)
	if err != nil || bits > 0o777 {
		return Mask{}, fmt.Errorf("workspace: %s %q is not valid; %s", name, s, maskForms)
	}
	return Mask{Bits: fs.FileMode(bits), Set: true}, nil
}

// String is the canonical spelling, four octal digits; unset is 0000.
func (m Mask) String() string { return fmt.Sprintf("%04o", uint32(m.Bits)) }

// Watch is how much of what happens to a share on this machine is replayed
// inside the workspace (ADR 0016): nothing, what can be replayed faithfully,
// or also deletions and renames as a poke at the parent directory. A mount
// names a canonical word; the session setting also takes the aliases
// ParseWatchSetting lists.
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

// ParseWatchSetting reads the session's own watch setting (REMOTE_DOCKER_WATCH
// and the config file), which also takes the spellings people have used: on,
// true, 1 and yes mean partial; best-effort means coarse; empty, false, 0 and
// no mean off. A mount's watch= word takes canonical words only (ParseWatch).
func ParseWatchSetting(s string) (Watch, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "off", "false", "0", "no":
		return WatchOff, nil
	case "partial", "on", "true", "1", "yes":
		return WatchPartial, nil
	case "coarse", "best-effort":
		return WatchCoarse, nil
	}
	return WatchUnset, fmt.Errorf("workspace: unknown watch mode %q; want %s", s, watchWords)
}

// watchRank orders the answers by how much they replay.
func watchRank(w Watch) int {
	switch w {
	case WatchPartial:
		return 1
	case WatchCoarse:
		return 2
	}
	return 0
}

// Max is the stronger of two answers: off < partial < coarse. Coarse is partial
// plus a poke at the parent for deletions and renames, so it does everything
// partial does.
func (w Watch) Max(other Watch) Watch {
	if watchRank(other) > watchRank(w) {
		return other
	}
	if w == WatchUnset {
		return other
	}
	return w
}

// Watching reports whether this answer replays anything.
func (w Watch) Watching() bool { return w == WatchPartial || w == WatchCoarse }

// Prefetch is how a union's cache is filled ahead of reads (ADR 0045): not at
// all, the whole tree up to the budget, or what the reads point at. Only a mount
// that is read=cached and a union prefetches; anywhere else the only answer is
// off.
type Prefetch string

const (
	PrefetchUnset Prefetch = ""
	PrefetchOff   Prefetch = "off"
	PrefetchEager Prefetch = "eager"
	PrefetchTree  Prefetch = "tree"
)

// prefetchWords is the tail of a prefetch refusal.
const prefetchWords = "off|eager|tree"

// ParsePrefetch reads one prefetch word.
func ParsePrefetch(s string) (Prefetch, error) {
	switch p := Prefetch(strings.TrimSpace(s)); p {
	case PrefetchOff, PrefetchEager, PrefetchTree:
		return p, nil
	default:
		return PrefetchUnset, fmt.Errorf("workspace: prefetch=%q is not a prefetch policy; want one of %s", s, prefetchWords)
	}
}

// prefetchRank orders the answers by how much they send.
func prefetchRank(p Prefetch) int {
	switch p {
	case PrefetchTree:
		return 1
	case PrefetchEager:
		return 2
	}
	return 0
}

// Max is the stronger of two answers: off < tree < eager. Eager sends
// everything up to the budget; tree sends what reads point at.
func (p Prefetch) Max(other Prefetch) Prefetch {
	if prefetchRank(other) > prefetchRank(p) {
		return other
	}
	if p == PrefetchUnset {
		return other
	}
	return p
}

// Prefetching reports whether this answer fills anything.
func (p Prefetch) Prefetching() bool { return p == PrefetchEager || p == PrefetchTree }

// String is the canonical spelling; unset is off.
func (p Prefetch) String() string {
	if p == PrefetchUnset {
		return string(PrefetchOff)
	}
	return string(p)
}

// ShareWords are the words that belong to a share rather than to one mount of
// it. Owner and the masks are part of the share's identity; Watch and Prefetch
// are raised.
type ShareWords struct {
	Owner    Owner
	FMask    Mask
	DMask    Mask
	Watch    Watch
	Prefetch Prefetch
}

// String is the canonical spelling; unset is off.
func (w Watch) String() string {
	if w == WatchUnset {
		return string(WatchOff)
	}
	return string(w)
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
var shareKeySetting = map[string]string{
	"owner":    "REMOTE_DOCKER_SHARE_OWNER",
	"fmask":    "REMOTE_DOCKER_SHARE_FMASK",
	"dmask":    "REMOTE_DOCKER_SHARE_DMASK",
	"watch":    "REMOTE_DOCKER_WATCH",
	"prefetch": "REMOTE_DOCKER_PREFETCH",
}

func isShareKey(key string) bool {
	_, ok := shareKeySetting[key]
	return ok
}

// ParseMount reads a comma-separated list of read=, write=, owner=, fmask=,
// dmask=, watch=, prefetch= and Docker's own consistency words. A key named
// twice is refused, and so is any other word, naming it.
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
		case "fmask":
			m, err := ParseFileMask(value)
			if err != nil {
				return MountWords{}, err
			}
			w.FMask = m
		case "dmask":
			m, err := ParseDirMask(value)
			if err != nil {
				return MountWords{}, err
			}
			w.DMask = m
		case "watch":
			got, err := ParseWatch(value)
			if err != nil {
				return MountWords{}, err
			}
			w.Watch = got
		case "prefetch":
			got, err := ParsePrefetch(value)
			if err != nil {
				return MountWords{}, err
			}
			w.Prefetch = got
		}
	}
	mode, err := ParseMode(strings.Join(modeWords, ","))
	if err != nil {
		return MountWords{}, err
	}
	w.Mode = mode
	return w, nil
}
