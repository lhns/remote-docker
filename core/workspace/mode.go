package workspace

import (
	"fmt"
	"strings"
)

// A mount's mode on two axes (ADR 0042):
//
//	read=direct     every attribute revalidated every second
//	read=cached     attributes trusted for a minute; needs the watcher
//	write=through   a write lands on this machine as it happens
//	write=back      a write lands in a union on the workspace and is carried back
//	write=ephemeral a write lands in a union and is never carried back
//
// A union exists exactly when write != through, because overlayfs writes to
// its upper. Prefetch runs only when read=cached and there is a union to
// land in (ADR 0045). Docker's own values for the field
// (api/types/mount.Consistency) are aliases: consistent and default are
// read=direct,write=through; cached is read=cached,write=through; delegated
// is read=cached,write=back. Spelled in a `-v` option list beside `ro`, or in
// `--mount` as a csv-quoted field, `"consistency=read=cached,write=back"`.

// Read is how long the container may trust what it has read.
type Read string

// Write is where a container's write lands, and whether it comes back.
type Write string

const (
	ReadUnset  Read = ""
	ReadDirect Read = "direct"
	ReadCached Read = "cached"

	WriteUnset     Write = ""
	WriteThrough   Write = "through"
	WriteBack      Write = "back"
	WriteEphemeral Write = "ephemeral"
)

// Mode is one mount's answer on both axes. Either axis may be unset, so a
// mount can name one and take the rule's or the workspace's answer for the
// other.
type Mode struct {
	Read  Read
	Write Write
}

// ModeUnset is a mount that named nothing on either axis.
var ModeUnset = Mode{}

// DefaultMode is what a bind mount does: read=direct,write=through.
var DefaultMode = Mode{ReadDirect, WriteThrough}

// dockerWords are Docker's consistency values, each naming both axes.
var dockerWords = map[string]Mode{
	"default":    DefaultMode,
	"consistent": DefaultMode,
	"cached":     {ReadCached, WriteThrough},
	"delegated":  {ReadCached, WriteBack},
}

// The words a person may write on each axis, for error messages.
const (
	readWords  = "direct|cached"
	writeWords = "through|back|ephemeral"
)

// Or fills each unset axis from fallback: the mount outranks the rule, and
// the rule outranks the workspace default.
func (m Mode) Or(fallback Mode) Mode {
	if m.Read == ReadUnset {
		m.Read = fallback.Read
	}
	if m.Write == WriteUnset {
		m.Write = fallback.Write
	}
	return m
}

// Union reports whether this mode needs a union: exactly when writes are not
// synchronous.
func (m Mode) Union() bool { return m.Write == WriteBack || m.Write == WriteEphemeral }

// Prefetch reports whether the cache may be filled ahead of reads: a union to
// land in, and permission to cache what is read. A direct mount with a union
// captures writes and prefetches nothing.
func (m Mode) Prefetch() bool { return m.Read == ReadCached && m.Union() }

// String is the canonical spelling: `read=cached,write=back`.
func (m Mode) String() string {
	var parts []string
	if m.Read != ReadUnset {
		parts = append(parts, "read="+string(m.Read))
	}
	if m.Write != WriteUnset {
		parts = append(parts, "write="+string(m.Write))
	}
	if len(parts) == 0 {
		return "unset"
	}
	return strings.Join(parts, ",")
}

// ParseMode reads a comma-separated list of `read=X` and `write=Y` words, or
// one of Docker's, which names both axes. An axis not named is left unset;
// an axis named twice, or any other word, is refused naming it.
func ParseMode(s string) (Mode, error) {
	var m Mode
	for _, word := range strings.Split(s, ",") {
		word = strings.TrimSpace(word)
		if word == "" {
			continue
		}
		got, err := parseModeWord(word)
		if err != nil {
			return ModeUnset, err
		}
		if got.Read != ReadUnset {
			if m.Read != ReadUnset {
				return ModeUnset, fmt.Errorf("workspace: %q names read twice", s)
			}
			m.Read = got.Read
		}
		if got.Write != WriteUnset {
			if m.Write != WriteUnset {
				return ModeUnset, fmt.Errorf("workspace: %q names write twice", s)
			}
			m.Write = got.Write
		}
	}
	return m, nil
}

// parseModeWord reads one word.
func parseModeWord(word string) (Mode, error) {
	if m, ok := dockerWords[word]; ok {
		return m, nil
	}
	axis, value, ok := strings.Cut(word, "=")
	if !ok {
		return ModeUnset, fmt.Errorf("workspace: %q is not a mount mode; want read=<%s>, write=<%s>, or one of default, consistent, cached, delegated",
			word, readWords, writeWords)
	}
	switch axis {
	case "read":
		r, err := parseRead(value)
		return Mode{Read: r}, err
	case "write":
		w, err := parseWrite(value)
		return Mode{Write: w}, err
	default:
		return ModeUnset, fmt.Errorf("workspace: %q is not a mount mode axis; want read= or write=", axis)
	}
}

// IsModeWord reports whether one word of a `-v` option list is ours to
// consume. It is consumed whether or not the bind is rewritten: a bind left
// alone would carry the word to the daemon, which rejects it. It matches the
// AXIS and not the value, on purpose: a misspelt value is claimed and refused
// by ParseMode rather than forwarded to the daemon.
func IsModeWord(s string) bool {
	s = strings.TrimSpace(s)
	if _, ok := dockerWords[s]; ok {
		return true
	}
	axis, _, ok := strings.Cut(s, "=")
	return ok && (axis == "read" || axis == "write")
}

func parseRead(s string) (Read, error) {
	switch r := Read(strings.TrimSpace(s)); r {
	case ReadDirect, ReadCached:
		return r, nil
	default:
		return ReadUnset, fmt.Errorf("workspace: read=%q is not a read mode; want one of %s", s, readWords)
	}
}

func parseWrite(s string) (Write, error) {
	switch w := Write(strings.TrimSpace(s)); w {
	case WriteThrough, WriteBack, WriteEphemeral:
		return w, nil
	default:
		return WriteUnset, fmt.Errorf("workspace: write=%q is not a write mode; want one of %s", s, writeWords)
	}
}

// attributeOptions are the NFS mount options the read axis decides.
//
// actimeo bounds how long the kernel trusts a cached attribute; at 1s a walk
// over small files pays a round trip per file per second. nocto drops the
// revalidation on every open. Both are safe under read=cached only because
// the watcher's replayed edit is a SETATTR through the mount that refreshes
// the changed inode (ADR 0042), which is why a cached read without a watcher
// is refused.
func attributeOptions(r Read) []string {
	if r == ReadCached {
		return []string{"actimeo=60", "nocto"}
	}
	return []string{"actimeo=1"}
}
