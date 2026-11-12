package rewrite

import (
	"strings"
	"testing"

	"github.com/lhns/remote-docker/core/workspace"
)

// readOnlyShareID is defaultShareID for a share served read-only (ADR 0058).
func readOnlyShareID(localPath string) string {
	return workspace.ShareIDFor(localPath, workspace.ShareVariant{
		Mode: workspace.DefaultMode, Owner: workspace.Owner{Set: true}, ReadOnly: true,
	})
}

// A read-only bind is a read-only share: `ro` in a `-v` list, beside other
// options or not, and ReadOnly on a `--mount` entry. Either way it also stays
// on the container's mount.
func TestReadOnlyBindIsAReadOnlyShare(t *testing.T) {
	const project = "/home/alice/project"
	for _, c := range []struct {
		name     string
		body     string
		readOnly bool
		kept     string // what the container's mount must still carry
	}{
		{"-v ro", `{"HostConfig":{"Binds":["` + project + `:/app:ro"]}}`, true, ":/app:ro"},
		{"-v ro,z", `{"HostConfig":{"Binds":["` + project + `:/app:ro,z"]}}`, true, ":/app:ro,z"},
		{"-v z,ro with a word", `{"HostConfig":{"Binds":["` + project + `:/app:z,ro,owner=1000"]}}`, true, ":/app:z,ro"},
		{"-v rw", `{"HostConfig":{"Binds":["` + project + `:/app:rw"]}}`, false, ""},
		{"-v nothing", `{"HostConfig":{"Binds":["` + project + `:/app"]}}`, false, ""},
		{"--mount readonly", `{"HostConfig":{"Mounts":[{"Type":"bind","Source":"` + project + `","Target":"/app","ReadOnly":true}]}}`, true, `"ReadOnly":true`},
		{"--mount rw", `{"HostConfig":{"Mounts":[{"Type":"bind","Source":"` + project + `","Target":"/app","ReadOnly":false}]}}`, false, ""},
		{"--mount unspecified", `{"HostConfig":{"Mounts":[{"Type":"bind","Source":"` + project + `","Target":"/app"}]}}`, false, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, sharer, _ := newRewriter()
			r.ReadOnlyExports = true
			out, err := r.ContainerCreate(t.Context(), []byte(c.body))
			if err != nil {
				t.Fatalf("ContainerCreate: %v", err)
			}
			if got := sharer.words[project].ReadOnly; got != c.readOnly {
				t.Errorf("shared ReadOnly = %v, want %v", got, c.readOnly)
			}
			if !strings.Contains(string(out), c.kept) {
				t.Errorf("the container's mount lost %q: %s", c.kept, out)
			}
		})
	}
}

// A read-only and a read-write mount of one directory are two shares side by
// side, and a repeat of either is the one share.
func TestReadOnlyAndReadWriteAreTwoShares(t *testing.T) {
	r, sharer, volumes := newRewriter()
	r.ReadOnlyExports = true
	if _, err := r.ContainerCreate(t.Context(), []byte(`{"HostConfig":{"Binds":[`+
		`"/home/alice/project:/a:ro","/home/alice/project:/b","/home/alice/project:/c:ro"]}}`)); err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	if len(volumes.created) != 2 {
		t.Errorf("volumes = %v, want two", volumes.created)
	}
	for _, id := range []string{readOnlyShareID("/home/alice/project"), defaultShareID("/home/alice/project")} {
		if _, ok := volumes.created[workspace.VolumeNameForID("", id)]; !ok {
			t.Errorf("no volume for share %s in %v", id, volumes.created)
		}
	}
	if sharer.exports[0] != sharer.exports[2] || sharer.exports[0] == sharer.exports[1] {
		t.Errorf("exports = %v, want the two ro mounts to share one and the rw mount another", sharer.exports)
	}
}

// A single file bind goes to Mounts in the same walk, and its share is
// read-only too.
func TestAReadOnlySingleFileIsAReadOnlyShare(t *testing.T) {
	const local = "/home/alice/nginx.conf"
	r := newFileRewriter(local, "nginx.conf")
	r.ReadOnlyExports = true
	sharer := r.Shares.(*fakeSharer)
	if _, err := r.ContainerCreate(t.Context(),
		[]byte(`{"HostConfig":{"Binds":["`+local+`:/etc/nginx/nginx.conf:ro"]}}`)); err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	if !sharer.words[local].ReadOnly {
		t.Error("the single file was shared read-write")
	}
}

// Write-back is a second way home for what the workspace writes, so a
// read-only share never has it: write=back becomes write=ephemeral, and the
// read mode stays (ADR 0058).
func TestReadOnlyWriteBackIsEphemeral(t *testing.T) {
	const project = "/home/alice/project"
	cachedEphemeral := workspace.Mode{Read: workspace.ReadCached, Write: workspace.WriteEphemeral}
	for _, c := range []struct {
		name string
		bind string
		want workspace.Mode
	}{
		{"back", project + ":/app:ro,read=cached,write=back", cachedEphemeral},
		{"delegated", project + ":/app:ro,delegated", cachedEphemeral},
		{"ephemeral stays", project + ":/app:ro,write=ephemeral", workspace.Mode{Read: workspace.ReadDirect, Write: workspace.WriteEphemeral}},
		{"read-write back stays", project + ":/app:read=cached,write=back", cachedBack},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, _, cache := unionRewriter()
			r.ReadOnlyExports = true
			if _, err := r.ContainerCreate(t.Context(), []byte(`{"HostConfig":{"Binds":["`+c.bind+`"]}}`)); err != nil {
				t.Fatalf("ContainerCreate: %v", err)
			}
			if cache.attached == "" {
				t.Fatal("the share was not attached to the cache")
			}
			if cache.mode != c.want {
				t.Errorf("attached as %v, want %v", cache.mode, c.want)
			}
		})
	}

	// Through is not a union and stays what it was.
	r, sharer, _ := newRewriter()
	r.ReadOnlyExports = true
	r.StartWatcher = func() error { return nil }
	if _, err := r.ContainerCreate(t.Context(),
		[]byte(`{"HostConfig":{"Binds":["`+project+`:/app:ro,read=cached"]}}`)); err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	if !sharer.words[project].ReadOnly {
		t.Error("read=cached,write=through share was not read-only")
	}
}

// With the setting off, which is the default, a read-only bind is what it was
// before the setting existed: not a variant, the same id and volume as a
// read-write mount, and write=back stays write=back (ADR 0058).
func TestReadOnlyExportsOffChangesNothing(t *testing.T) {
	const project = "/home/alice/project"

	r, sharer, volumes := newRewriter()
	out, err := r.ContainerCreate(t.Context(), []byte(`{"HostConfig":{"Binds":["`+project+`:/a:ro","`+project+`:/b"]}}`))
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	if sharer.words[project].ReadOnly {
		t.Error("the share is read-only with the setting off")
	}
	if len(volumes.created) != 1 || sharer.exports[0] != sharer.exports[1] {
		t.Errorf("volumes = %v, exports = %v; want one share for both", volumes.created, sharer.exports)
	}
	want := workspace.VolumeNameForID("", defaultShareID(project)) + ":/a:ro"
	if !strings.Contains(string(out), want) {
		t.Errorf("want %q in %s", want, out)
	}

	r, _, cache := unionRewriter()
	if _, err := r.ContainerCreate(t.Context(),
		[]byte(`{"HostConfig":{"Binds":["`+project+`:/app:ro,read=cached,write=back"]}}`)); err != nil {
		t.Fatal(err)
	}
	if cache.mode != cachedBack || cache.words.ReadOnly {
		t.Errorf("attached as %v read-only=%v, want write=back and read-write", cache.mode, cache.words.ReadOnly)
	}

	r, sharer, _ = newRewriter()
	if _, err := r.ContainerCreate(t.Context(), []byte(`{"HostConfig":{"Mounts":[`+
		`{"Type":"bind","Source":"`+project+`","Target":"/app","ReadOnly":true}]}}`)); err != nil {
		t.Fatal(err)
	}
	if sharer.words[project].ReadOnly {
		t.Error("a --mount readonly share is read-only with the setting off")
	}
}
