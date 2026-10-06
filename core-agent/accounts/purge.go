package accounts

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Purge removes what a removed account leaves on the workspace: its home
// directory, then its unix user. Its uid stays in the uidmap, so the name and
// the uid are never given to anybody else (ADR 0053).
//
// It also forgets that this process provisioned the account, so a re-enrolled
// one is provisioned again rather than published with a user that is gone.
// provMu keeps a re-enrolment's Ensure from running until Remove has.
func (s *Store) Purge(name string) error {
	s.provMu.Lock()
	defer s.provMu.Unlock()

	s.syncMu.Lock()
	a, ok := s.Lookup(name)
	if ok && len(a.Keys) == 0 {
		delete(s.provisioned, name)
	}
	s.syncMu.Unlock()
	if !ok {
		return fmt.Errorf("accounts: no account %s", name)
	}
	if len(a.Keys) > 0 {
		return fmt.Errorf("accounts: %s is still enrolled, so nothing was purged", name)
	}
	var errs []error
	if a.Home != "" {
		if err := removeHome(a); err != nil {
			errs = append(errs, err)
		}
	}
	if err := s.Provisioner.Remove(name, a.UID); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// removeHome deletes a's home only if it is an absolute, clean path, not the
// root, not a link, a directory owned by a's uid, and with nothing mounted
// inside it.
func removeHome(a *Account) error {
	path := a.Home
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || filepath.Dir(path) == path {
		return fmt.Errorf("accounts: %s's home %q is not a clean absolute path below the root, so it was not removed", a.Name, path)
	}
	fi, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("accounts: %s's home: %w", a.Name, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("accounts: %s's home %s is not a directory, so it was not removed", a.Name, path)
	}
	if uid, ok := ownerOf(fi); ok && uid != a.UID {
		return fmt.Errorf("accounts: %s is owned by uid %d, not %s's %d, so it was not removed", path, uid, a.Name, a.UID)
	}
	m, err := mountedUnder(path)
	if err == nil && m != "" {
		err = fmt.Errorf("%s is mounted inside it", m)
	}
	if err != nil {
		return fmt.Errorf("accounts: %s's home %s was not removed: %w", a.Name, path, err)
	}
	return os.RemoveAll(path)
}
