package machine

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// brokerRecord says whose a machine is. Owner is a SID, read off the pipe when
// the machine was created, never from a request.
type brokerRecord struct {
	Owner   string `json:"owner"`
	Created string `json:"created"`
}

// brokerRecords is machines.json: machine name -> record. The file is the only
// authority on ownership, so a machine in Hyper-V that is not in it belongs to
// nobody the service knows and is never touched.
//
// Unlocked: the server's mutex serialises every use. Read afresh each time, so
// the teardown process sees what the running service wrote.
type brokerRecords struct{ path string }

type brokerRecordFile struct {
	Machines map[string]brokerRecord `json:"machines"`
}

// load reads the file. A file that does not parse is an error and is never
// overwritten: ownership we cannot read is not ownership we may rewrite.
func (r brokerRecords) load() (map[string]brokerRecord, error) {
	raw, err := os.ReadFile(r.path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]brokerRecord{}, nil
	}
	if err != nil {
		return nil, err
	}
	var f brokerRecordFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("%s does not parse: %w", r.path, err)
	}
	if f.Machines == nil {
		f.Machines = map[string]brokerRecord{}
	}
	return f.Machines, nil
}

// save writes atomically: a temp file in the same directory, renamed over. The
// directory is the service's own (broker_windows.go protects it).
func (r brokerRecords) save(m map[string]brokerRecord) error {
	raw, err := json.MarshalIndent(brokerRecordFile{Machines: m}, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(r.path), "machines-*.tmp")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(raw)
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), r.path); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return nil
}

func (r brokerRecords) put(name string, rec brokerRecord) error {
	m, err := r.load()
	if err != nil {
		return err
	}
	m[name] = rec
	return r.save(m)
}

func (r brokerRecords) remove(name string) error {
	m, err := r.load()
	if err != nil {
		return err
	}
	if _, ok := m[name]; !ok {
		return nil
	}
	delete(m, name)
	return r.save(m)
}
