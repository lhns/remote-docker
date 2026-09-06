package nfsserve

import (
	"io/fs"
	"strings"
	"syscall"
)

// ntfsNameError is the rule Windows applies to a new name, spelled out here so
// it can be tested on every platform; checkNewName is what applies it, and
// does so only on Windows.
//
// Go opens files through a `\?\` path, which hands the name to NTFS with the
// Win32 rules switched off, so `star*`, `quote"` and `nul` are created without
// complaint and then refused by the Win32 layer of some later operation: a
// file that can be made and never removed (ENOENT, or EIO for `nul`). Docker
// on a Windows host refuses these names outright, and so does this.
//
// The rule is Microsoft's own list for a path component: none of `< > : " | ? *`
// and no control character; no trailing dot or space; and not a reserved
// device name, compared before the first dot and without regard to case.
func ntfsNameError(name string) error {
	if !ntfsNameOK(name) {
		return &fs.PathError{Op: "create", Path: name, Err: syscall.EINVAL}
	}
	return nil
}

func ntfsNameOK(name string) bool {
	if name == "" {
		return true
	}
	if strings.ContainsAny(name, `<>:"|?*`) {
		return false
	}
	for i := 0; i < len(name); i++ {
		if name[i] < 0x20 || name[i] == 0x7f {
			return false
		}
	}
	if strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") {
		return false
	}
	stem, _, _ := strings.Cut(name, ".")
	stem = strings.ToUpper(stem)
	switch stem {
	case "CON", "PRN", "AUX", "NUL":
		return false
	}
	if len(stem) == 4 && (strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT")) &&
		stem[3] >= '1' && stem[3] <= '9' {
		return false
	}
	return true
}
