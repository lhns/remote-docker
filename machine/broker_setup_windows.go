//go:build windows

package machine

// What the installer runs as SYSTEM around the service (ADR 0055): the group
// that decides who may use it, and taking everything back. Both idempotent,
// and teardown tolerant of whatever is already gone.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// brokerRegKey records what setup did, so teardown removes only that: a group
// an administrator made, or a user they added, outlives an uninstall.
const brokerRegKey = `SOFTWARE\lhns\remote-docker\Service`

const (
	regCreatedGroup = "CreatedGroup" // DWORD 1 when setup created BrokerGroup
	regAddedUsers   = "AddedUsers"   // SIDs setup added to it
)

func openRegKey(create bool) (registry.Key, error) {
	access := uint32(registry.QUERY_VALUE | registry.SET_VALUE | registry.WOW64_64KEY)
	if create {
		k, _, err := registry.CreateKey(registry.LOCAL_MACHINE, brokerRegKey, access)
		return k, err
	}
	return registry.OpenKey(registry.LOCAL_MACHINE, brokerRegKey, access)
}

// BrokerSetup creates the group if it is missing and adds the user to it,
// recording both. Running it again changes nothing.
func BrokerSetup(userSID string) error {
	sid, err := windows.StringToSid(userSID)
	if err != nil {
		return fmt.Errorf("%q is not a SID: %w", userSID, err)
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
		if err := addLocalGroup(BrokerGroup); err != nil {
			return fmt.Errorf("creating the group %s: %w", BrokerGroup, err)
		}
		if err := k.SetDWordValue(regCreatedGroup, 1); err != nil {
			return err
		}
	}

	if slices.ContainsFunc(members, func(m string) bool { return strings.EqualFold(m, sid.String()) }) {
		return nil
	}
	if err := changeLocalGroupMember(procNetLocalGroupAddMembers, BrokerGroup, sid, errMemberInAlias); err != nil {
		return fmt.Errorf("adding %s to %s: %w", userSID, BrokerGroup, err)
	}
	added, _, _ := k.GetStringsValue(regAddedUsers)
	return k.SetStringsValue(regAddedUsers, append(added, sid.String()))
}

// BrokerPurgeLocal destroys every machine the service owns, in this process
// and without the pipe, so it works with the service stopped. Administrators
// run it.
func BrokerPurgeLocal(ctx context.Context) ([]string, error) {
	root, err := brokerRoot()
	if err != nil {
		return nil, err
	}
	srv := newBrokerServer(serviceHyperV(root), root+`\machines.json`, nil, nil, nil)
	done, errs := srv.purgeAll(ctx)
	return done, errors.Join(errs...)
}

// BrokerTeardown destroys the machines the service owns, takes back what
// BrokerSetup did and removes the service's directory. Whatever is already
// gone is skipped. If a machine cannot be destroyed the directory stays, since
// it holds the record a retry needs, and the error says so.
func BrokerTeardown(ctx context.Context) error {
	var errs []error

	_, perr := BrokerPurgeLocal(ctx)
	if perr != nil {
		errs = append(errs, perr)
	}

	if k, err := openRegKey(false); err == nil {
		users, _, _ := k.GetStringsValue(regAddedUsers)
		created, _, _ := k.GetIntegerValue(regCreatedGroup)
		_ = k.Close()

		for _, u := range users {
			sid, err := windows.StringToSid(u)
			if err != nil {
				continue
			}
			if err := changeLocalGroupMember(procNetLocalGroupDelMembers, BrokerGroup, sid, errMemberNoAlias, errGroupMissing, nerrGroupNotFound); err != nil {
				errs = append(errs, fmt.Errorf("removing %s from %s: %w", u, BrokerGroup, err))
			}
		}
		if created == 1 {
			if err := deleteLocalGroup(BrokerGroup); err != nil {
				errs = append(errs, fmt.Errorf("deleting the group %s: %w", BrokerGroup, err))
			}
		}
	}

	if perr == nil {
		if root, err := brokerRoot(); err == nil {
			if err := os.RemoveAll(root); err != nil {
				errs = append(errs, err)
			}
		}
	}
	_ = registry.DeleteKey(registry.LOCAL_MACHINE, brokerRegKey)
	return errors.Join(errs...)
}
