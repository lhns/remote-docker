package rewrite

import (
	"io/fs"
	"strings"
	"testing"

	"github.com/lhns/remote-docker/core/workspace"
)

// fmask= and dmask= are words of the share, as owner= is: consumed in both
// spellings, the session's default where a mount names none, and a named 0
// beating that default.
func TestMaskWordsReachTheShare(t *testing.T) {
	const project = "/home/alice/project"
	set := func(bits fs.FileMode) workspace.Mask { return workspace.Mask{Bits: bits, Set: true} }
	for _, c := range []struct {
		name         string
		body         string
		fmask, dmask workspace.Mask
	}{
		{"-v", `{"HostConfig":{"Binds":["` + project + `:/app:ro,fmask=0111,dmask=022"]}}`, set(0o111), set(0o022)},
		{"--mount", `{"HostConfig":{"Mounts":[{"Type":"bind","Source":"` + project + `","Target":"/app","Consistency":"fmask=0111"}]}}`, set(0o111), set(0o002)},
		{"nothing named, session default", `{"HostConfig":{"Binds":["` + project + `:/app"]}}`, set(0o011), set(0o002)},
		{"a named 0 beats the default", `{"HostConfig":{"Binds":["` + project + `:/app:fmask=0"]}}`, set(0), set(0o002)},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, sharer, _ := newRewriter()
			r.ShareFMask, r.ShareDMask = set(0o011), set(0o002)

			out, err := r.ContainerCreate(t.Context(), []byte(c.body))
			if err != nil {
				t.Fatalf("ContainerCreate: %v", err)
			}
			if got := sharer.words[project]; got.FMask != c.fmask || got.DMask != c.dmask {
				t.Errorf("shared with fmask %v, dmask %v; want %v, %v", got.FMask, got.DMask, c.fmask, c.dmask)
			}
			if strings.Contains(string(out), "mask") {
				t.Errorf("a mask word reached the daemon: %s", out)
			}
			if strings.Contains(c.body, ":ro") && !strings.Contains(string(out), ":/app:ro") {
				t.Errorf("ro was lost: %s", out)
			}
		})
	}

	// No session default either: nothing cleared, which is the id every share
	// had before the masks.
	r, sharer, _ := newRewriter()
	if _, err := r.ContainerCreate(t.Context(), []byte(`{"HostConfig":{"Binds":["`+project+`:/app"]}}`)); err != nil {
		t.Fatal(err)
	}
	if got := sharer.words[project]; got.FMask != set(0) || got.DMask != set(0) {
		t.Errorf("default masks = %v, %v; want 0000", got.FMask, got.DMask)
	}
	if want := workspace.ExportPathForID(defaultShareID(project)); sharer.exports[0] != want {
		t.Errorf("export = %s, want the unmasked %s", sharer.exports[0], want)
	}
}

// Another mask on one directory is another share with its own volume; the
// same mask spelled twice is one.
func TestOneDirectoryWithTwoMasksGetsTwoShares(t *testing.T) {
	r, sharer, volumes := newRewriter()
	if _, err := r.ContainerCreate(t.Context(), []byte(`{"HostConfig":{"Binds":[`+
		`"/home/alice/project:/app:fmask=0111","/home/alice/project:/also"]}}`)); err != nil {
		t.Fatalf("two masks for one directory were refused: %v", err)
	}
	if len(volumes.created) != 2 || sharer.exports[0] == sharer.exports[1] {
		t.Errorf("volumes = %v, exports = %v; want two of each", volumes.created, sharer.exports)
	}

	r, sharer, volumes = newRewriter()
	if _, err := r.ContainerCreate(t.Context(), []byte(`{"HostConfig":{"Binds":[`+
		`"/home/alice/project:/app:fmask=0111","/home/alice/project:/also:fmask=111"]}}`)); err != nil {
		t.Errorf("the same mask in two spellings was refused: %v", err)
	}
	if len(volumes.created) != 1 || sharer.exports[0] != sharer.exports[1] {
		t.Errorf("volumes = %v, exports = %v; want one share", volumes.created, sharer.exports)
	}
}

// A bad mask is refused naming the word, before anything is created.
func TestABadMaskIsRefused(t *testing.T) {
	for _, bind := range []string{"fmask=0999", "dmask=0111"} {
		r, _, volumes := newRewriter()
		_, err := r.ContainerCreate(t.Context(), []byte(`{"HostConfig":{"Binds":["/home/alice/project:/app:`+bind+`"]}}`))
		if err == nil || !strings.Contains(err.Error(), strings.Split(bind, "=")[0]) {
			t.Errorf("%s = %v, want a refusal naming it", bind, err)
		}
		if len(volumes.created) != 0 {
			t.Errorf("%s created %v", bind, volumes.created)
		}
	}
}
