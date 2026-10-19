package prune

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// recordVersion is bumped when the file's meaning changes; a record of another
// version is read as no record, which only delays removal.
const recordVersion = 1

// Record is when each of one daemon's images was last in use, and when the
// daemon was last looked at. An image's age is this and not its build date:
// a base image built a year ago and used this morning is not old.
type Record struct {
	// Swept is how far the create events have been read, where the next read
	// starts. Zero reads none: there is nothing earlier to catch up on.
	Swept time.Time
	Seen  map[string]time.Time
}

// NewRecord is a record that knows nothing.
func NewRecord() *Record { return &Record{Seen: map[string]time.Time{}} }

// LastSeen is when the image was last in use. An image the record has never
// met is as old as the record, which is now: an agent upgrade, or a lost file,
// makes nothing older than it was.
func (r *Record) LastSeen(id string, now time.Time) time.Time {
	if t, ok := r.Seen[id]; ok {
		return t
	}
	return now
}

// Observe notes what is in use at now: images a container references, and
// images named by the create events of containers since the last look (names as
// typed, resolved against images). Images met for the first time are seen now,
// and entries for images that no longer exist are dropped.
func (r *Record) Observe(now time.Time, images []Image, containers []Container, created []string) {
	seen := make(map[string]time.Time, len(images))
	for _, img := range images {
		if t, ok := r.Seen[img.ID]; ok {
			seen[img.ID] = t
		} else {
			seen[img.ID] = now
		}
	}
	for id := range Used(containers) {
		if _, ok := seen[id]; ok {
			seen[id] = now
		}
	}
	for _, name := range created {
		if id, ok := Resolve(images, name); ok {
			seen[id] = now
		}
	}
	r.Seen = seen
}

type file struct {
	Version int                  `json:"version"`
	Swept   time.Time            `json:"swept"`
	Seen    map[string]time.Time `json:"seen"`
}

// Marshal renders the record as the file holds it.
func (r *Record) Marshal() ([]byte, error) {
	return json.MarshalIndent(file{Version: recordVersion, Swept: r.Swept, Seen: r.Seen}, "", "  ")
}

// Parse reads what Marshal wrote.
func Parse(data []byte) (*Record, error) {
	var f file
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, err
	}
	if f.Version != recordVersion {
		return nil, fmt.Errorf("record version %d, want %d", f.Version, recordVersion)
	}
	if f.Seen == nil {
		f.Seen = map[string]time.Time{}
	}
	return &Record{Swept: f.Swept, Seen: f.Seen}, nil
}

// Load reads the record at path. A missing file is an empty record and no
// error; an unreadable one is an empty record AND the error, for a warning.
func Load(path string) (*Record, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return NewRecord(), nil
	}
	if err != nil {
		return NewRecord(), err
	}
	r, err := Parse(data)
	if err != nil {
		return NewRecord(), fmt.Errorf("%s: %w", path, err)
	}
	return r, nil
}

// Save writes the record by renaming a complete file into place, so a crash
// leaves the old record or the new and never half of one.
func (r *Record) Save(path string) error {
	data, err := r.Marshal()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
