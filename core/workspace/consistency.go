package workspace

import (
	"fmt"
	"strings"
)

// Consistency is Docker's own mount consistency axis, which this project gives
// a meaning to.
//
// Docker defines it (api/types/mount) and every client already parses it:
// `-v /project:/app:cached`, `--mount type=bind,...,consistency=cached`, and
// `consistency: cached` in a compose file. On a normal daemon it is inert. Here
// the mount is NFS over a tunnel, where an attribute revalidation is a round
// trip, so the axis describes something real.
//
// Inventing an option of our own was not open to us: the CLI and the daemon
// reject mount options they do not know, so a spelling nobody else understands
// fails before it reaches the rewriter. This one is understood everywhere and
// means nothing anywhere else.
type Consistency string

const (
	// Unset is a mount that named nothing, which is what makes the workspace
	// default apply. Distinct from Default, which is a value somebody asked
	// for and therefore outranks the setting.
	Unset Consistency = ""

	// Default and Consistent are Docker's words for the mount behaving as a
	// bind mount does: what this project has always done.
	Default    Consistency = "default"
	Consistent Consistency = "consistent"

	// Cached: the client's filesystem is authoritative, and the container may
	// cache read data and directory structure. The mount stays live; what goes
	// away is revalidating every attribute (ADR 0042).
	Cached Consistency = "cached"

	// Delegated: the CONTAINER's view is authoritative, so its reads and
	// writes may be cached. Docker's own word for a copy that is written back.
	Delegated Consistency = "delegated"
)

// ParseConsistency reads one of Docker's four words.
func ParseConsistency(s string) (Consistency, error) {
	switch c := Consistency(strings.TrimSpace(s)); c {
	case Unset, Default, Consistent, Cached, Delegated:
		return c, nil
	default:
		return Unset, fmt.Errorf("workspace: %q is not a mount consistency; want one of %s",
			s, strings.Join(Consistencies(), ", "))
	}
}

// Consistencies lists the words a person may write, for help text and for the
// error above. Unset is not one of them: it is the absence of an answer.
func Consistencies() []string {
	return []string{string(Default), string(Consistent), string(Cached), string(Delegated)}
}

// IsConsistency reports whether a word is one of Docker's, without deciding
// anything about it. Used where consistency sits in a list beside options this
// program does not interpret, such as the third field of a `-v`.
func IsConsistency(s string) bool {
	c, err := ParseConsistency(s)
	return err == nil && c != Unset
}

// Or returns c when it is set, and fallback otherwise. Precedence in one place:
// the mount outranks the rule, and the rule outranks the workspace default.
func (c Consistency) Or(fallback Consistency) Consistency {
	if c == Unset {
		return fallback
	}
	return c
}

// attributeOptions are the NFS mount options that differ per consistency, and
// they are the whole of what `cached` is.
//
// actimeo bounds how long the kernel trusts a cached attribute before asking
// again, and asking is a round trip. At the default 1 second a walk over a tree
// of small files pays that per file per second, which is what the latency rows
// in test/bench.sh measure.
//
// nocto drops close-to-open consistency, which is a revalidation on every open.
// Correct to drop only because the client's watcher pokes the workspace when a
// file here changes, and that SETATTR refreshes the kernel's cached attributes
// for the inode: long cache for what has not changed, immediate refresh for
// what has (ADR 0042). Without a watcher `cached` is a stale mount, which is
// why selecting it without one is refused rather than tuned down.
func attributeOptions(c Consistency) []string {
	if c == Cached {
		return []string{"actimeo=60", "nocto"}
	}
	return []string{"actimeo=1"}
}
