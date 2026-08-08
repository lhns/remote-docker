package daemons

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// WorkspaceIDFile is where the workspace's identity is kept, beside the uidmap
// and for the same reason: it must survive the container it describes.
const WorkspaceIDFile = "workspace-id"

// WorkspaceID reads the workspace's id from the state directory, creating one
// on first run.
//
// Deliberately NOT the container id, and this is the whole point of the file.
// A container id changes every time the workspace is redeployed -- every
// `docker compose up -d`, every Swarm task replacement -- so daemons labelled
// with it would stop being recognised as ours on the first upgrade. They would
// keep running, unadoptable, holding their volumes and their users' containers,
// while the agent started a second set beside them under names that were
// already taken.
//
// The state directory is a volume that outlives the container, which is what
// makes the id stable across exactly the events that change a container id.
func WorkspaceID(stateDir string) (string, error) {
	path := filepath.Join(stateDir, WorkspaceIDFile)

	data, err := os.ReadFile(path)
	if err == nil {
		if id := strings.TrimSpace(string(data)); id != "" {
			return id, nil
		}
		// An empty file is a half-finished first run. Fall through and write a
		// real one rather than labelling every daemon with "".
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("daemons: reading %s: %w", path, err)
	}

	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("daemons: generating a workspace id: %w", err)
	}
	id := "ws-" + hex.EncodeToString(buf)

	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return "", fmt.Errorf("daemons: preparing %s: %w", stateDir, err)
	}

	// Written via a temporary file and renamed, like the uidmap: a workspace
	// that crashed mid-write and came back with a truncated id would adopt
	// nothing and orphan everything.
	tmp, err := os.CreateTemp(stateDir, WorkspaceIDFile+".*")
	if err != nil {
		return "", fmt.Errorf("daemons: writing the workspace id: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()

	if _, err := tmp.WriteString(id + "\n"); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("daemons: writing the workspace id: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("daemons: writing the workspace id: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", fmt.Errorf("daemons: writing the workspace id: %w", err)
	}
	return id, nil
}
