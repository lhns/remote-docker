package workspace

import (
	"fmt"
	"strings"
	"testing"
)

// What a mount option COSTS when the workspace's kernel does not know it: the
// NFS client refuses the whole option string, so one word too new fails every
// mount on that workspace -- and a docker volume's driver options are
// immutable, so the volumes it has already made can never mount again either.
// The error names the list rather than the word:
//
//	failed to mount local volume: mount :/m/<id>:/var/lib/docker/volumes/...,
//	flags: 0x400, data: addr=127.0.0.1,...,nconnect=8,...: invalid argument
//
// Confirmed in the kernel's own source, in both parser eras: v3.10
// fs/nfs/super.c sets invalid_option on the default branch and returns 0 for
// the whole string, which the caller turns into -EINVAL; v6.6
// fs/nfs/fs_context.c returns -ENOPARAM, which vfs_parse_fs_param reports as
// `Unknown parameter` and -EINVAL. (Checked 2026-09-08.)
//
// The floor below is a decision, the table is a claim about the kernel, and
// this test is where the two meet. Nothing that RUNS can stand in for it: every
// CI runner is 6.x, and there is no way to ask an NFS client which options it
// parses, since it answers EINVAL and names nothing.

// kernel is a kernel version, compared component by component. Three of them,
// because the answers here are 2.6.23 and 3.10 and a two-field version cannot
// hold the first.
type kernel struct{ major, minor, patch int }

func (k kernel) String() string {
	if k.major == 2 {
		return fmt.Sprintf("%d.%d.%d", k.major, k.minor, k.patch)
	}
	return fmt.Sprintf("%d.%d", k.major, k.minor)
}

// atLeast reports whether k is min or newer. Numeric per component, because
// 3.10 is newer than 3.9 and no comparison on the text says so.
func (k kernel) atLeast(min kernel) bool {
	switch {
	case k.major != min.major:
		return k.major > min.major
	case k.minor != min.minor:
		return k.minor > min.minor
	default:
		return k.patch >= min.patch
	}
}

// minimumKernel is the oldest WORKSPACE kernel this project targets, and it is
// 3.10 because RHEL 7 is known to be in use. README says the same where
// deployment requirements live. Not the client's kernel, which mounts nothing.
var minimumKernel = kernel{major: 3, minor: 10}

// option is one word this code can emit, and the first upstream kernel that
// accepts it.
type option struct {
	needs kernel
	// where the version was read, so the next person checks rather than
	// trusting this line.
	source string
}

// minKernel is every word NFSVolumeOptions can emit. A word missing from here
// FAILS the test rather than passing quietly, so adding an option means
// answering the question first.
//
// Checked 2026-09-08 by reading the kernel at the tags named. The one command
// that re-checks a row, substituting the tag and the token:
//
//	curl -s https://raw.githubusercontent.com/torvalds/linux/v3.10/fs/nfs/super.c | grep -n Opt_actimeo
//
// On a workspace rather than a source tree: mount with the option, and an
// unknown word gives EINVAL for the whole mount with `NFS: unrecognized mount
// option` in the kernel log, which on 3.10 is a dfprintk and may not reach
// dmesg.
//
// One era rather than one release explains most of these: mount(2) with fstype
// nfs took a binary struct nfs_mount_data until the text parser arrived.
// nfs_parse_mount_options is absent from fs/nfs/super.c at v2.6.22 and present
// at v2.6.23, and every word below except nconnect is in its token table there.
var minKernel = map[string]option{
	"addr":      {kernel{2, 6, 23}, "v2.6.23 fs/nfs/super.c, Opt_addr"},
	"port":      {kernel{2, 6, 23}, "v2.6.23 fs/nfs/super.c, Opt_port"},
	"mountport": {kernel{2, 6, 23}, "v2.6.23 fs/nfs/super.c, Opt_mountport"},
	"nfsvers":   {kernel{2, 6, 23}, "v2.6.23 fs/nfs/super.c, Opt_nfsvers"},
	"nolock":    {kernel{2, 6, 23}, "v2.6.23 fs/nfs/super.c, Opt_nolock"},
	"noacl":     {kernel{2, 6, 23}, "v2.6.23 fs/nfs/super.c, Opt_noacl"},
	"soft":      {kernel{2, 6, 23}, "v2.6.23 fs/nfs/super.c, Opt_soft"},
	"timeo":     {kernel{2, 6, 23}, "v2.6.23 fs/nfs/super.c, Opt_timeo"},
	"retrans":   {kernel{2, 6, 23}, "v2.6.23 fs/nfs/super.c, Opt_retrans"},
	"actimeo":   {kernel{2, 6, 23}, "v2.6.23 fs/nfs/super.c, Opt_actimeo"},
	"nocto":     {kernel{2, 6, 23}, "v2.6.23 fs/nfs/super.c, Opt_nocto"},

	// The VALUE is the interesting half here and it is not a refusal: 1 MiB is
	// the client's own ceiling (NFS_MAX_FILE_IO_SIZE) since 2.6.16, commit
	// 40859d7ee64e. A figure above it, or above what the server advertised in
	// FSINFO, is negotiated DOWN in silence (v3.10 fs/nfs/client.c,
	// nfs_server_set_fsinfo), so a share on an old kernel mounts and reads,
	// it just may read in smaller pieces. Over UDP the ceiling is ~64 KiB,
	// which nothing here uses.
	"rsize": {kernel{2, 6, 23}, "v2.6.23 Opt_rsize; the 1 MiB ceiling is 2.6.16, commit 40859d7ee64e"},
	"wsize": {kernel{2, 6, 23}, "v2.6.23 Opt_wsize; the 1 MiB ceiling is 2.6.16, commit 40859d7ee64e"},

	// Not an NFS option at all: MS_NOATIME, which docker's local volume driver
	// splits out of this list before it calls mount(2) and which the union
	// splits out for itself (core-agent/union, Spec.LowerMount). Older than
	// anything else here, and in the table because this list is what is emitted
	// rather than what the NFS client sees.
	"noatime": {kernel{2, 6, 0}, "MS_NOATIME, a kernel mount flag rather than an NFS option"},

	// NOT EMITTED, and this row is why. Added in 5.3, commit 28cc5cd8c68f;
	// fs/nfs/super.c at v5.2 does not contain the string at all. `man 5 nfs`
	// carries no "since Linux 5.3" line for it, so the commit is the citation
	// and the man page is not. Red Hat documents it from RHEL 8.3
	// (4.18.0-240.el8); no RHEL 7 backport was found, which source cannot
	// prove either way. It cost a RHEL 7 workspace every one of its mounts.
	"nconnect": {kernel{5, 3, 0}, "5.3, commit 28cc5cd8c68f; absent from v5.2 fs/nfs/super.c"},
}

// Every option this code can emit, under every read mode, is one the supported
// floor parses.
//
// THE TEST THAT WAS MISSING. `nconnect=8` was pinned as a literal string in
// export_test.go, and that test passed while every mount on a 3.10 workspace
// failed: asserting that we wrote what we meant to write says nothing about
// whether the kernel reading it has heard of the word.
//
// It covers the union's lower mount too, which is this same list split in two
// (core-agent/union asks NFSVolumeOptions rather than copying it). It does NOT
// cover fuse-overlayfs, which is a binary in the workspace rather than a mount
// option, so the client asks the workspace whether it has one (Info.Union)
// rather than deriving it from a version.
func TestEmittedOptionsFitTheSupportedKernel(t *testing.T) {
	for _, read := range []Read{ReadUnset, ReadDirect, ReadCached} {
		opts := NFSVolumeOptions(30000, "/m/00112233445566ff", read)
		for _, word := range strings.Split(opts["o"], ",") {
			name, _, _ := strings.Cut(word, "=")
			needs, known := minKernel[name]
			if !known {
				t.Errorf("read=%s emits %q, which is in no row of minKernel: add one saying "+
					"which kernel first parsed it and where that was read", read, word)
				continue
			}
			if !minimumKernel.atLeast(needs.needs) {
				t.Errorf("read=%s emits %q, which needs Linux %s (%s), and the supported floor is %s. "+
					"The kernel refuses the WHOLE option string over one word it does not know, so this "+
					"is every mount on such a workspace failing, on volumes that can never be repaired.",
					read, word, needs.needs, needs.source, minimumKernel)
			}
		}
	}
}

// README and CLAUDE.md are written against this number, so changing it changes
// what the project promises and fails here rather than being edited in passing.
func TestTheFloorIsRHEL7(t *testing.T) {
	if minimumKernel != (kernel{major: 3, minor: 10}) {
		t.Errorf("the supported floor is now %s, where README and CLAUDE.md say 3.10 "+
			"and CHANGELOG names the RHEL 7 workspace it came from", minimumKernel)
	}
}

func TestKernelOrdering(t *testing.T) {
	for _, c := range []struct {
		k, min kernel
		want   bool
	}{
		{kernel{3, 10, 0}, kernel{2, 6, 23}, true},
		{kernel{3, 10, 0}, kernel{3, 10, 0}, true},
		{kernel{3, 10, 0}, kernel{3, 11, 0}, false},
		{kernel{3, 10, 0}, kernel{5, 3, 0}, false},
		{kernel{5, 3, 0}, kernel{5, 3, 0}, true},
		{kernel{6, 1, 0}, kernel{5, 3, 0}, true},
		{kernel{2, 6, 22}, kernel{2, 6, 23}, false},
		// 3.10 is newer than 3.9, which no comparison on the text says.
		{kernel{3, 10, 0}, kernel{3, 9, 0}, true},
	} {
		if got := c.k.atLeast(c.min); got != c.want {
			t.Errorf("%s.atLeast(%s) = %v, want %v", c.k, c.min, got, c.want)
		}
	}
}
