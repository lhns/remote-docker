//go:build linux

package union

import "testing"

// Two lists have to agree: LowerMount decides which option words are kernel
// FLAGS rather than filesystem data, and mountFlagBits turns those words into
// MS_ bits. A word classed as a flag with no bit behind it is silently dropped
// -- the mount is made without it, which for `ro` would mean a read-only share
// mounted read-write.
//
// Two are deliberately bitless, because they ARE the absence of a bit.
func TestEveryMountFlagHasABit(t *testing.T) {
	absence := map[string]bool{"rw": true, "atime": true}

	for word := range mountFlags {
		if absence[word] {
			if got := mountFlagBits([]string{word}); got != 0 {
				t.Errorf("%q is the absence of a flag but sets %#x", word, got)
			}
			continue
		}
		if mountFlagBits([]string{word}) == 0 {
			t.Errorf("%q is classed as a mount flag and has no bit, so it is dropped", word)
		}
	}
}

// And the split itself: a word this build does not know must stay with the
// filesystem options rather than being silently classed as a flag.
func TestUnknownOptionIsNotAFlag(t *testing.T) {
	for _, opt := range []string{"nfsvers=3", "nolock", "actimeo=1", "addr=127.0.0.1"} {
		if isMountFlag(opt) {
			t.Errorf("%q was taken for a kernel flag; the NFS client needs it", opt)
		}
	}
}
