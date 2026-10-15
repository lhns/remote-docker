//go:build windows

package machine

// What the installer runs as SYSTEM around the service (ADR 0055): the group
// that decides who may use it, the service's directories, and taking everything
// back. Both idempotent, and teardown tolerant of whatever is already gone.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// brokerRegKey records what setup did, so teardown removes only that: a group
// an administrator made, or a user they added, outlives an uninstall.
const brokerRegKey = `SOFTWARE\lhns\remote-docker\Service`

const (
	regCreatedGroup = "CreatedGroup" // DWORD 1 when setup meant to create BrokerGroup
	regAddedUsers   = "AddedUsers"   // SIDs setup meant to add to it
)

func openRegKey(create bool) (registry.Key, error) {
	access := uint32(registry.QUERY_VALUE | registry.SET_VALUE | registry.WOW64_64KEY)
	if create {
		k, _, err := registry.CreateKey(registry.LOCAL_MACHINE, brokerRegKey, access)
		return k, err
	}
	return registry.OpenKey(registry.LOCAL_MACHINE, brokerRegKey, access)
}

// BrokerSetup creates the service's protected directories and the group if it
// is missing, and adds the user to it, recording both. Running it again
// changes nothing.
//
// Intent is recorded BEFORE each step, so a setup that dies between the step
// and the record cannot leave a group or membership that teardown does not
// know it made. Teardown tolerates a record of something that never happened.
func BrokerSetup(userSID string) error {
	sid, err := windows.StringToSid(userSID)
	if err != nil {
		return fmt.Errorf("%q is not a SID: %w", userSID, err)
	}

	// Created here, as SYSTEM, at install: a user cannot then pre-create the
	// root before the service first runs.
	root, err := brokerRoot()
	if err != nil {
		return err
	}
	if err := ensureBrokerDirs(root); err != nil {
		return fmt.Errorf("preparing %s: %w", root, err)
	}

	k, err := openRegKey(true)
	if err != nil {
		return fmt.Errorf("opening the setup record: %w", err)
	}
	defer func() { _ = k.Close() }()

	members, exists, err := localGroupMembers(BrokerGroup)
	if err != nil {
		return err
	}
	if !exists {
		if err := k.SetDWordValue(regCreatedGroup, 1); err != nil {
			return err
		}
		if err := addLocalGroup(BrokerGroup); err != nil {
			return fmt.Errorf("creating the group %s: %w", BrokerGroup, err)
		}
	}

	if slices.ContainsFunc(members, func(m string) bool { return strings.EqualFold(m, sid.String()) }) {
		return nil
	}
	recorded, _, _ := k.GetStringsValue(regAddedUsers)
	was := slices.ContainsFunc(recorded, func(m string) bool { return strings.EqualFold(m, sid.String()) })
	if !was {
		if err := k.SetStringsValue(regAddedUsers, append(recorded, sid.String())); err != nil {
			return err
		}
	}
	err = changeLocalGroupMember(procNetLocalGroupAddMembers, BrokerGroup, sid)
	if errors.Is(err, errMemberInAlias) {
		// Somebody else added them first: not ours to take back.
		if !was {
			_ = k.SetStringsValue(regAddedUsers, recorded)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("adding %s to %s: %w", userSID, BrokerGroup, err)
	}
	return nil
}

// checkedRoot returns the service's root, or "" when there is none, and an
// error when there is one that must not be read or removed: a user's folder or
// junction at that path, or a service still running from it. The check the
// service makes at start, made again by whatever runs in another process.
func checkedRoot() (string, error) {
	root, err := brokerRoot()
	if err != nil {
		return "", err
	}
	if _, err := os.Lstat(root); errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err := verifyProtectedDir(root); err != nil {
		return "", fmt.Errorf("not touching it: %w", err)
	}
	// The service and this process would both write machines.json. The
	// installer stops the service before it tears down; a person running this
	// by hand is told to.
	if running, err := brokerServiceRunning(); err != nil {
		return "", err
	} else if running {
		return "", fmt.Errorf("the %s service is running\n  fix: `sc stop %s`, then run this again", BrokerServiceName, BrokerServiceName)
	}
	return root, nil
}

// BrokerPurgeLocal destroys every machine the service owns, in this process
// and without the pipe. Administrators run it, with the service stopped.
func BrokerPurgeLocal(ctx context.Context) ([]string, error) {
	root, err := checkedRoot()
	if err != nil || root == "" {
		return nil, err
	}
	srv := newBrokerServer(serviceHyperV(root), filepath.Join(root, "machines.json"), nil, nil, nil)
	done, errs := srv.purgeAll(ctx)
	return done, errors.Join(errs...)
}

// BrokerTeardown destroys the machines the service owns, takes back what
// BrokerSetup did and removes the service's directory. Whatever is already
// gone is skipped. If a machine cannot be destroyed, or the directory is not
// the service's, the directory stays and the error says so.
func BrokerTeardown(ctx context.Context) error {
	var errs []error

	root, err := checkedRoot()
	if err == nil && root != "" {
		srv := newBrokerServer(serviceHyperV(root), filepath.Join(root, "machines.json"), nil, nil, nil)
		_, perrs := srv.purgeAll(ctx)
		err = errors.Join(perrs...)
	}
	if err != nil {
		errs = append(errs, err)
	}

	if k, kerr := openRegKey(false); kerr == nil {
		users, _, _ := k.GetStringsValue(regAddedUsers)
		created, _, _ := k.GetIntegerValue(regCreatedGroup)
		_ = k.Close()

		for _, u := range users {
			sid, serr := windows.StringToSid(u)
			if serr != nil {
				continue
			}
			if merr := changeLocalGroupMember(procNetLocalGroupDelMembers, BrokerGroup, sid); merr != nil &&
				!errors.Is(merr, errMemberNoAlias) && !errors.Is(merr, errGroupMissing) && !errors.Is(merr, nerrGroupNotFound) {
				errs = append(errs, fmt.Errorf("removing %s from %s: %w", u, BrokerGroup, merr))
			}
		}
		if created == 1 {
			// Only an empty group: one somebody else filled, or made first, is
			// theirs.
			left, exists, lerr := localGroupMembers(BrokerGroup)
			switch {
			case lerr != nil:
				errs = append(errs, lerr)
			case exists && len(left) > 0:
				fmt.Fprintf(os.Stderr, "leaving the group %s: it still has %d member(s) this installer did not add\n", BrokerGroup, len(left))
			case exists:
				if derr := deleteLocalGroup(BrokerGroup); derr != nil {
					errs = append(errs, fmt.Errorf("deleting the group %s: %w", BrokerGroup, derr))
				}
			}
		}
	}

	if err == nil && root != "" {
		if rerr := os.RemoveAll(root); rerr != nil {
			errs = append(errs, rerr)
		}
	}
	_ = registry.DeleteKey(registry.LOCAL_MACHINE, brokerRegKey)
	return errors.Join(errs...)
}
