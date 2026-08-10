package main

// Editing the user's PATH on Windows, which is the only platform where this
// program does that, and the only one where the alternative is a trip
// through the System Properties dialog.

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// userEnvKey is the per-user environment block. HKCU, never HKLM: this is one
// account's PATH, and writing the machine's would need administrator and would
// affect everybody on it.
const userEnvKey = `Environment`

// ensurePATH adds dir to the user's PATH, and reports whether it had to.
//
// NEVER `setx`. It is the obvious way to do this and it is destructive: setx
// truncates the value at 1024 characters, so a PATH longer than that, which
// is most developer machines -- is silently CUT SHORT, and everything past the
// cut is gone from the account for good. The registry is the interface;
// setx is a wrapper around it with a limit the registry does not have.
func ensurePATH(out io.Writer, dir string) (bool, error) {
	key, err := registry.OpenKey(registry.CURRENT_USER, userEnvKey, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return false, fmt.Errorf("opening HKCU\\%s: %w", userEnvKey, err)
	}
	defer func() { _ = key.Close() }()

	current, valueType, err := key.GetStringValue("Path")
	if err != nil && err != registry.ErrNotExist {
		return false, fmt.Errorf("reading your PATH: %w", err)
	}
	if err == registry.ErrNotExist {
		// An account with no user PATH at all is unusual but legal, and
		// REG_EXPAND_SZ is what Windows itself writes.
		valueType = registry.EXPAND_SZ
	}

	updated, added := appendPath(current, dir)
	if !added {
		_, _ = fmt.Fprintf(out, "%s is already on your user PATH.\n", dir)
		// The registry having it and THIS SHELL having it are different
		// facts, and the gap between them is the whole confusion: a terminal
		// keeps the environment it started with, so "it is on my PATH" and
		// "docker is not recognised" are both true at once.
		if !onPath(dir) {
			_, _ = fmt.Fprintln(out,
				"  note: this shell kept the PATH it started with, so open a new one")
		}
		return false, nil
	}

	// The type is PRESERVED rather than chosen. A PATH holding %USERPROFILE%
	// is REG_EXPAND_SZ, and rewriting it as REG_SZ would leave the percent
	// signs as literal characters -- every entry using one silently stops
	// resolving.
	if valueType == registry.EXPAND_SZ {
		err = key.SetExpandStringValue("Path", updated)
	} else {
		err = key.SetStringValue("Path", updated)
	}
	if err != nil {
		return false, fmt.Errorf("writing your PATH: %w", err)
	}

	broadcastEnvChange()
	_, _ = fmt.Fprintf(out,
		"added %s to your user PATH (HKCU\\%s), at the end, so a real Docker later still wins\n"+
			"  undo: remote-docker shim uninstall\n", dir, userEnvKey)
	return true, nil
}

// removePATH takes the entry out again.
func removePATH(out io.Writer, dir string) error {
	key, err := registry.OpenKey(registry.CURRENT_USER, userEnvKey, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("opening HKCU\\%s: %w", userEnvKey, err)
	}
	defer func() { _ = key.Close() }()

	current, valueType, err := key.GetStringValue("Path")
	if err != nil {
		// Nothing to remove from, which is not a failure of uninstalling.
		return nil
	}

	updated, removed := removeFromPath(current, dir)
	if !removed {
		return nil
	}
	if valueType == registry.EXPAND_SZ {
		err = key.SetExpandStringValue("Path", updated)
	} else {
		err = key.SetStringValue("Path", updated)
	}
	if err != nil {
		return fmt.Errorf("writing your PATH: %w", err)
	}

	broadcastEnvChange()
	_, _ = fmt.Fprintf(out, "removed %s from your user PATH\n", dir)
	return nil
}

// broadcastEnvChange tells running programs the environment moved.
//
// Explorer and anything else listening will pick the new PATH up for the
// programs they launch afterwards. A shell that is ALREADY open keeps the
// environment it was started with -- nothing can change that from outside --
// which is why the caller says so in as many words.
//
// SendMessageTimeout rather than SendMessage: a single hung top-level window
// would otherwise block this program for as long as it stayed hung.
func broadcastEnvChange() {
	const (
		hwndBroadcast   = 0xffff
		wmSettingChange = 0x001A
		smtoAbortIfHung = 0x0002
	)
	user32 := windows.NewLazySystemDLL("user32.dll")
	send := user32.NewProc("SendMessageTimeoutW")
	param, err := windows.UTF16PtrFromString("Environment")
	if err != nil {
		return
	}
	var result uintptr
	_, _, _ = send.Call(
		uintptr(hwndBroadcast), uintptr(wmSettingChange), 0,
		uintptr(unsafe.Pointer(param)),
		uintptr(smtoAbortIfHung), uintptr(5000),
		uintptr(unsafe.Pointer(&result)),
	)
}

// crossDeviceNote explains a hardlink that could not be made because the two
// paths are on different volumes.
//
// A hardlink is a second directory entry for ONE file, and a file lives on one
// volume, so this is a hard limit rather than a permission.
func crossDeviceNote(self, path string) (string, bool) {
	a, b := filepath.VolumeName(self), filepath.VolumeName(path)
	if a == "" || b == "" || strings.EqualFold(a, b) {
		return "", false
	}
	return fmt.Sprintf(
		"%s is on %s and the shim goes on %s, and a hardlink cannot span volumes\n"+
			"(it is a second name for one file, and a file lives on one volume).",
		self, a, b), true
}
