package prune

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
	"time"
)

// Footprint is what one account owns on the workspace's own daemon: its daemon
// container and its graph volume (ADR 0019).
type Footprint struct {
	Account   string
	Container bool // the daemon container exists, running or not
	Running   bool
	Volume    bool
}

// Orphan is a footprint whose account is no longer enrolled.
type Orphan struct {
	Footprint
	// Since is when the agent first saw the account gone. The account's key
	// file leaves no date, so this is the only clock there is, and an upgrade
	// starts it at the upgrade.
	Since time.Time
	// Due: the grace is over and the daemon container may be removed. Only a
	// container of this workspace is ever due; a volume alone could belong to
	// another workspace on the same parent daemon, so it is reported only.
	Due bool
}

// OrphanRecord is when each orphaned account was first seen without a key.
type OrphanRecord struct {
	Since map[string]time.Time
}

const orphanVersion = 1

type orphanFile struct {
	Version int                  `json:"version"`
	Since   map[string]time.Time `json:"since"`
}

// LoadOrphans reads the record at path. A missing file is an empty record and
// no error; an unreadable one is an empty record AND the error, which only
// restarts every grace period.
func LoadOrphans(path string) (*OrphanRecord, error) {
	empty := &OrphanRecord{Since: map[string]time.Time{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return empty, nil
	}
	if err != nil {
		return empty, err
	}
	var f orphanFile
	if err := json.Unmarshal(data, &f); err != nil {
		return empty, fmt.Errorf("%s: %w", path, err)
	}
	if f.Version != orphanVersion {
		return empty, fmt.Errorf("%s: record version %d, want %d", path, f.Version, orphanVersion)
	}
	if f.Since != nil {
		empty.Since = f.Since
	}
	return empty, nil
}

// Save writes the record the way Record.Save does.
func (r *OrphanRecord) Save(path string) error {
	data, err := json.MarshalIndent(orphanFile{Version: orphanVersion, Since: r.Since}, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(path, data)
}

// Orphans names the footprints whose account is not enrolled, and notes in rec
// when each was first seen so. An account that is enrolled again drops out of
// the record, so a returning account starts a fresh grace if it leaves again.
// Account names are compared exactly: a footprint with an empty account is
// never an orphan, since nothing says whose it is.
func Orphans(rec *OrphanRecord, found []Footprint, enrolled map[string]bool, grace time.Duration, now time.Time) []Orphan {
	if rec.Since == nil {
		rec.Since = map[string]time.Time{}
	}
	var out []Orphan
	seen := map[string]bool{}
	for _, f := range found {
		if f.Account == "" || enrolled[f.Account] {
			continue
		}
		seen[f.Account] = true
		since, ok := rec.Since[f.Account]
		if !ok {
			since = now
			rec.Since[f.Account] = since
		}
		out = append(out, Orphan{Footprint: f, Since: since, Due: f.Container && now.Sub(since) >= grace})
	}
	for account := range rec.Since {
		if !seen[account] {
			delete(rec.Since, account)
		}
	}
	slices.SortFunc(out, func(a, b Orphan) int { return strings.Compare(a.Account, b.Account) })
	return out
}
