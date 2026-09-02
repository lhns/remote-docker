package rewrite

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/lhns/remote-docker/core/workspace"
)

// fakeSharer records what was exported and hands back deterministic paths.
type fakeSharer struct {
	shared []string
	cwd    string            // a local path to report as the /cwd share
	files  map[string]string // local path -> base name, for single-file shares
	err    error
}

func (f *fakeSharer) Share(localPath string) (string, string, error) {
	if f.err != nil {
		return "", "", f.err
	}
	f.shared = append(f.shared, localPath)
	file := f.files[localPath]
	if localPath == f.cwd {
		return workspace.ExportCWD, file, nil
	}
	return workspace.ExportPathForID(workspace.ShareID(localPath)), file, nil
}

type fakeVolumes struct {
	created map[string]map[string]string
	labels  map[string]map[string]string
	err     error
}

func (f *fakeVolumes) EnsureVolume(_ context.Context, name string, opts, labels map[string]string) error {
	if f.err != nil {
		return f.err
	}
	if f.created == nil {
		f.created = map[string]map[string]string{}
		f.labels = map[string]map[string]string{}
	}
	f.created[name] = opts
	f.labels[name] = labels
	return nil
}

func newRewriter() (*Rewriter, *fakeSharer, *fakeVolumes) {
	s := &fakeSharer{}
	v := &fakeVolumes{}
	return &Rewriter{Shares: s, Volumes: v, NFSPort: 30000}, s, v
}

// decode pulls HostConfig back out for assertions.
func decodeHostConfig(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decoding result: %v", err)
	}
	hc, _ := payload["HostConfig"].(map[string]any)
	return hc
}

func TestRewriteBinds(t *testing.T) {
	r, sharer, volumes := newRewriter()

	body := []byte(`{"Image":"alpine","HostConfig":{"Binds":["/home/alice/project:/app:ro"]}}`)
	out, err := r.ContainerCreate(t.Context(), body)
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}

	binds := decodeHostConfig(t, out)["Binds"].([]any)
	got := binds[0].(string)

	wantVolume := workspace.VolumeNameForID("", workspace.ShareID("/home/alice/project"))
	want := wantVolume + ":/app:ro"
	if got != want {
		t.Errorf("bind = %q, want %q", got, want)
	}
	if len(sharer.shared) != 1 || sharer.shared[0] != "/home/alice/project" {
		t.Errorf("exported %v, want [/home/alice/project]", sharer.shared)
	}

	opts, ok := volumes.created[wantVolume]
	if !ok {
		t.Fatalf("volume %q was not created (created %v)", wantVolume, volumes.created)
	}
	if opts["type"] != "nfs" {
		t.Errorf("volume type = %q, want nfs", opts["type"])
	}
	if !strings.Contains(opts["o"], "port=30000") {
		t.Errorf("volume options %q do not carry the tunnel port", opts["o"])
	}
}

// Binding the working directory is the commonest case there is, and it
// resolves to the /cwd share rather than a /m/<id> one.
func TestRewriteBindOfTheWorkingDirectory(t *testing.T) {
	r, sharer, volumes := newRewriter()
	sharer.cwd = "/home/alice/project"

	body := []byte(`{"HostConfig":{"Binds":["/home/alice/project:/app"]}}`)
	out, err := r.ContainerCreate(t.Context(), body)
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}

	binds := decodeHostConfig(t, out)["Binds"].([]any)
	if got, want := binds[0].(string), "rd-cwd:/app"; got != want {
		t.Errorf("bind = %q, want %q", got, want)
	}
	if _, ok := volumes.created["rd-cwd"]; !ok {
		t.Errorf("volume rd-cwd was not created (created %v)", volumes.created)
	}
}

// The case the single-mount design could not express.
func TestRewriteBindOutsideTheWorkingDirectory(t *testing.T) {
	r, sharer, _ := newRewriter()
	sharer.cwd = "/home/alice/project"

	body := []byte(`{"HostConfig":{"Binds":["/home/alice/project:/app","/mnt/data:/data"]}}`)
	out, err := r.ContainerCreate(t.Context(), body)
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}

	binds := decodeHostConfig(t, out)["Binds"].([]any)
	if len(binds) != 2 {
		t.Fatalf("got %d binds, want 2", len(binds))
	}
	if first := binds[0].(string); !strings.HasPrefix(first, "rd-cwd:") {
		t.Errorf("first bind = %q, want the cwd volume", first)
	}
	second := binds[1].(string)
	wantVolume := workspace.VolumeNameForID("", workspace.ShareID("/mnt/data"))
	if second != wantVolume+":/data" {
		t.Errorf("second bind = %q, want %q", second, wantVolume+":/data")
	}
}

// Named volumes are the user's own persistent data. Rewriting one would swap
// it for an export of a directory that does not exist.
func TestRewriteLeavesNamedVolumesAlone(t *testing.T) {
	r, sharer, volumes := newRewriter()

	body := []byte(`{"HostConfig":{"Binds":["pgdata:/var/lib/postgresql/data","/src:/app"]}}`)
	out, err := r.ContainerCreate(t.Context(), body)
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}

	binds := decodeHostConfig(t, out)["Binds"].([]any)
	if got := binds[0].(string); got != "pgdata:/var/lib/postgresql/data" {
		t.Errorf("named volume was rewritten to %q", got)
	}
	for _, s := range sharer.shared {
		if s == "pgdata" {
			t.Error("a named volume was exported as a directory")
		}
	}
	if _, ok := volumes.created["pgdata"]; ok {
		t.Error("a volume was created for what was already a named volume")
	}
}

func TestRewriteMounts(t *testing.T) {
	r, _, volumes := newRewriter()

	body := []byte(`{"HostConfig":{"Mounts":[
		{"Type":"bind","Source":"/home/alice/src","Target":"/app","ReadOnly":true,"BindOptions":{"Propagation":"rprivate"}},
		{"Type":"volume","Source":"pgdata","Target":"/data"},
		{"Type":"tmpfs","Target":"/tmp"}
	]}}`)
	out, err := r.ContainerCreate(t.Context(), body)
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}

	mounts := decodeHostConfig(t, out)["Mounts"].([]any)
	first := mounts[0].(map[string]any)

	if first["Type"] != "volume" {
		t.Errorf("Type = %v, want volume", first["Type"])
	}
	wantVolume := workspace.VolumeNameForID("", workspace.ShareID("/home/alice/src"))
	if first["Source"] != wantVolume {
		t.Errorf("Source = %v, want %q", first["Source"], wantVolume)
	}
	// ReadOnly is the user's intent and must survive.
	if first["ReadOnly"] != true {
		t.Errorf("ReadOnly = %v, want true", first["ReadOnly"])
	}
	// BindOptions is rejected by the daemon on a volume mount, and the
	// propagation it requests is meaningless once the daemon mounts the
	// volume in its own namespace.
	if _, ok := first["BindOptions"]; ok {
		t.Error("BindOptions survived onto a volume mount; the daemon will reject it")
	}

	if second := mounts[1].(map[string]any); second["Type"] != "volume" || second["Source"] != "pgdata" {
		t.Errorf("named volume mount was altered: %v", second)
	}
	if third := mounts[2].(map[string]any); third["Type"] != "tmpfs" {
		t.Errorf("tmpfs mount was altered: %v", third)
	}
	if _, ok := volumes.created[wantVolume]; !ok {
		t.Errorf("volume %q was not created", wantVolume)
	}
}

// The central requirement of ADR 0005: we decode and re-encode this body, so
// anything we do not understand has to come back exactly as it arrived. A
// typed struct would drop all of it.
func TestRewritePreservesUnknownFields(t *testing.T) {
	r, _, _ := newRewriter()

	body := []byte(`{
		"Image":"alpine",
		"SomeFutureTopLevelField":{"nested":[1,2,3]},
		"Healthcheck":{"Test":["CMD","true"],"Interval":30000000000},
		"HostConfig":{
			"Binds":["/src:/app"],
			"Memory":536870912,
			"SomeFutureHostField":{"a":"b"},
			"RestartPolicy":{"Name":"unless-stopped","MaximumRetryCount":0}
		}
	}`)
	out, err := r.ContainerCreate(t.Context(), body)
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}

	var before, after map[string]any
	if err := json.Unmarshal(body, &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out, &after); err != nil {
		t.Fatal(err)
	}

	for _, key := range []string{"Image", "SomeFutureTopLevelField", "Healthcheck"} {
		if !reflect.DeepEqual(before[key], after[key]) {
			t.Errorf("%s changed:\n  before %v\n   after %v", key, before[key], after[key])
		}
	}
	beforeHC := before["HostConfig"].(map[string]any)
	afterHC := after["HostConfig"].(map[string]any)
	for _, key := range []string{"Memory", "SomeFutureHostField", "RestartPolicy"} {
		if !reflect.DeepEqual(beforeHC[key], afterHC[key]) {
			t.Errorf("HostConfig.%s changed:\n  before %v\n   after %v", key, beforeHC[key], afterHC[key])
		}
	}
}

// A body with nothing to rewrite must be forwarded byte for byte, not
// re-serialised into an equivalent-but-different encoding.
func TestRewritePassesThroughUntouchedBodies(t *testing.T) {
	r, _, _ := newRewriter()

	for _, body := range []string{
		`{"Image":"alpine"}`,
		`{"Image":"alpine","HostConfig":{}}`,
		`{"Image":"alpine","HostConfig":{"Binds":null}}`,
		`{"Image":"alpine","HostConfig":{"Binds":["pgdata:/data"]}}`,
		`{"Image":"alpine","HostConfig":{"Mounts":[{"Type":"volume","Source":"v","Target":"/d"}]}}`,
	} {
		out, err := r.ContainerCreate(t.Context(), []byte(body))
		if err != nil {
			t.Fatalf("ContainerCreate(%s): %v", body, err)
		}
		if string(out) != body {
			t.Errorf("body was rewritten unnecessarily:\n  in  %s\n  out %s", body, out)
		}
	}
}

func TestRewriteRejectsMalformedJSON(t *testing.T) {
	r, _, _ := newRewriter()
	if _, err := r.ContainerCreate(t.Context(), []byte(`{"HostConfig":`)); err == nil {
		t.Error("malformed JSON was accepted")
	}
}

// A bind we cannot parse is forwarded so the daemon can produce its own error,
// which will be about the real problem rather than about our parser.
func TestRewriteForwardsUnparseableBinds(t *testing.T) {
	r, _, _ := newRewriter()

	body := []byte(`{"HostConfig":{"Binds":["/a:/b:ro:extra"]}}`)
	out, err := r.ContainerCreate(t.Context(), body)
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	if string(out) != string(body) {
		t.Errorf("an unparseable bind was altered:\n  in  %s\n  out %s", body, out)
	}
}

// Containers must be identifiable as ours. The workspace daemon is shared
// (ADR 0012), so without a mark of our own, port forwarding would open
// listeners on this machine because somebody else ran docker compose up.
func TestRewriteLabelsOurContainers(t *testing.T) {
	r, _, _ := newRewriter()
	r.Owner = "alice"

	out, err := r.ContainerCreate(t.Context(), []byte(`{"Image":"alpine","Labels":{"mine":"kept"}}`))
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatal(err)
	}
	labels, _ := payload["Labels"].(map[string]any)
	if labels[workspace.OwnerLabel] != "alice" {
		t.Errorf("labels = %v, want %s=alice", labels, workspace.OwnerLabel)
	}
	// The user's own labels are not ours to discard.
	if labels["mine"] != "kept" {
		t.Errorf("labels = %v, which lost the caller's own label", labels)
	}
}

func TestRewriteLabelsWithNoExistingLabels(t *testing.T) {
	r, _, _ := newRewriter()
	r.Owner = "alice"

	out, err := r.ContainerCreate(t.Context(), []byte(`{"Image":"alpine"}`))
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	if !strings.Contains(string(out), workspace.OwnerLabel) {
		t.Errorf("out = %s, want the owner label", out)
	}
}

// With no owner configured the body must still pass through untouched.
func TestRewriteWithoutOwnerLeavesBodyAlone(t *testing.T) {
	r, _, _ := newRewriter()

	body := []byte(`{"Image":"alpine"}`)
	out, err := r.ContainerCreate(t.Context(), body)
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	if string(out) != string(body) {
		t.Errorf("body changed with no owner set:\n  in  %s\n  out %s", body, out)
	}
}

// An audit of every container-create field that can name a host path, prompted
// by Portainer's GHSA-7fw3-x4r2-g7wc, which exists precisely because a proxy
// inspected HostConfig.Binds and missed HostConfig.Mounts. Those two are the
// only fields that produce a bind mount, and both are handled.
//
// The rest are pinned here as fields we must NOT disturb. A rewriter that
// quietly drops a device or a tmpfs would be a subtler bug than one that
// misses a bind.
func TestRewriteLeavesOtherMountFieldsAlone(t *testing.T) {
	r, _, _ := newRewriter()

	body := []byte(`{
		"HostConfig": {
			"Binds": ["/src:/app"],
			"Tmpfs": {"/run": "rw,size=64m"},
			"Devices": [{"PathOnHost":"/dev/fuse","PathInContainer":"/dev/fuse","CgroupPermissions":"rwm"}],
			"VolumesFrom": ["other-container"],
			"DeviceRequests": [{"Driver":"nvidia","Count":-1}]
		}
	}`)
	out, err := r.ContainerCreate(t.Context(), body)
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}

	hc := decodeHostConfig(t, out)
	var before map[string]any
	if err := json.Unmarshal(body, &before); err != nil {
		t.Fatal(err)
	}
	beforeHC := before["HostConfig"].(map[string]any)

	for _, field := range []string{"Tmpfs", "Devices", "VolumesFrom", "DeviceRequests"} {
		if !reflect.DeepEqual(beforeHC[field], hc[field]) {
			t.Errorf("HostConfig.%s changed:\n  before %v\n   after %v", field, beforeHC[field], hc[field])
		}
	}
	// The bind itself must still have been rewritten.
	if got := hc["Binds"].([]any)[0].(string); strings.HasPrefix(got, "/src:") {
		t.Errorf("Binds = %q, which was not rewritten", got)
	}
}

// HostConfig.VolumeDriver names the driver used for volumes this container
// creates, and our rewrite produces an unqualified volume name, so a
// container setting it could in principle have our name resolved by the wrong
// driver.
//
// It cannot, because the rewriter creates the volume with the local driver
// BEFORE the container is created, and an existing volume is used as it
// stands. The field is preserved for the user's own volumes, which is what it
// was set for.
func TestRewritePreservesVolumeDriver(t *testing.T) {
	r, _, volumes := newRewriter()

	body := []byte(`{"HostConfig":{"VolumeDriver":"some-plugin","Binds":["/src:/app","theirs:/data"]}}`)
	out, err := r.ContainerCreate(t.Context(), body)
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}

	hc := decodeHostConfig(t, out)
	if hc["VolumeDriver"] != "some-plugin" {
		t.Errorf("VolumeDriver = %v, want it preserved", hc["VolumeDriver"])
	}

	// Ours is created explicitly with the local driver, so the name is already
	// taken by the time the container is created.
	wantVolume := workspace.VolumeNameForID("", workspace.ShareID("/src"))
	if _, ok := volumes.created[wantVolume]; !ok {
		t.Errorf("volume %q was not created ahead of the container", wantVolume)
	}
}

// npipe and cluster mounts name things that mean nothing on a Linux workspace.
// Forwarding them lets the daemon produce its own error, which will be about
// the real problem rather than about our rewriter.
func TestRewriteForwardsExoticMountTypes(t *testing.T) {
	r, _, _ := newRewriter()

	body := []byte(`{"HostConfig":{"Mounts":[
		{"Type":"npipe","Source":"\\\\.\\pipe\\docker_engine","Target":"/pipe"},
		{"Type":"cluster","Source":"csi-volume","Target":"/data"}
	]}}`)
	out, err := r.ContainerCreate(t.Context(), body)
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	if string(out) != string(body) {
		t.Errorf("exotic mount types were altered:\n  in  %s\n  out %s", body, out)
	}
}

// newFileRewriter reports localPath as a single-file share exporting base.
func newFileRewriter(localPath, base string) (*Rewriter, *fakeVolumes) {
	s := &fakeSharer{files: map[string]string{localPath: base}}
	v := &fakeVolumes{}
	return &Rewriter{Shares: s, Volumes: v, NFSPort: 30000}, v
}

// A `-v` of a single file cannot stay in Binds: a bind string has no field for
// a subpath, and the subpath is what makes the container see a file rather than
// a directory (ADR 0039).
func TestASingleFileBindMovesToMounts(t *testing.T) {
	const local = "/home/alice/nginx.conf"
	r, _ := newFileRewriter(local, "nginx.conf")

	out, err := r.ContainerCreate(t.Context(),
		[]byte(`{"HostConfig":{"Binds":["`+local+`:/etc/nginx/nginx.conf:ro"]}}`))
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}

	hc := decodeHostConfig(t, out)
	if binds, ok := hc["Binds"].([]any); ok && len(binds) != 0 {
		t.Errorf("Binds still carries %v; the daemon refuses a target named twice", binds)
	}

	mounts, _ := hc["Mounts"].([]any)
	if len(mounts) != 1 {
		t.Fatalf("Mounts = %v, want the moved file mount", mounts)
	}
	mount := mounts[0].(map[string]any)

	wantVolume := workspace.VolumeNameForID("", workspace.ShareID(local))
	if mount["Type"] != "volume" || mount["Source"] != wantVolume {
		t.Errorf("mount = %v, want a volume mount of %q", mount, wantVolume)
	}
	if mount["Target"] != "/etc/nginx/nginx.conf" {
		t.Errorf("Target = %v, want the path the user asked for", mount["Target"])
	}
	if mount["ReadOnly"] != true {
		t.Error("ro was dropped, and the export behind it is read-write")
	}
	options, _ := mount["VolumeOptions"].(map[string]any)
	if options["Subpath"] != "nginx.conf" {
		t.Errorf("Subpath = %v, want nginx.conf", options["Subpath"])
	}
}

// A directory bind beside a file bind keeps its string form, so the two paths
// do not become one rewrite that has to know about both.
func TestADirectoryBindIsUnaffectedByAFileBeside(t *testing.T) {
	const file = "/home/alice/my.cnf"
	r, _ := newFileRewriter(file, "my.cnf")

	out, err := r.ContainerCreate(t.Context(),
		[]byte(`{"HostConfig":{"Binds":["/home/alice/src:/app","`+file+`:/etc/my.cnf"]}}`))
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}

	hc := decodeHostConfig(t, out)
	binds, _ := hc["Binds"].([]any)
	want := workspace.VolumeNameForID("", workspace.ShareID("/home/alice/src")) + ":/app"
	if len(binds) != 1 || binds[0] != want {
		t.Errorf("Binds = %v, want [%q]", binds, want)
	}
	if mounts, _ := hc["Mounts"].([]any); len(mounts) != 1 {
		t.Errorf("Mounts = %v, want only the file", mounts)
	}
}

// The --mount form already has somewhere to put the subpath, and whatever
// VolumeOptions the caller set survives beside it.
func TestASingleFileMountGainsASubpath(t *testing.T) {
	const local = "/home/alice/my.cnf"
	r, _ := newFileRewriter(local, "my.cnf")

	out, err := r.ContainerCreate(t.Context(), []byte(`{"HostConfig":{"Mounts":[
		{"Type":"bind","Source":"`+local+`","Target":"/etc/my.cnf","VolumeOptions":{"NoCopy":true}}
	]}}`))
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}

	mount := decodeHostConfig(t, out)["Mounts"].([]any)[0].(map[string]any)
	if mount["Type"] != "volume" {
		t.Errorf("Type = %v, want volume", mount["Type"])
	}
	options, _ := mount["VolumeOptions"].(map[string]any)
	if options["Subpath"] != "my.cnf" {
		t.Errorf("Subpath = %v, want my.cnf", options["Subpath"])
	}
	if options["NoCopy"] != true {
		t.Error("VolumeOptions was replaced rather than merged, losing NoCopy")
	}
}

// A bind option with no volume-mount equivalent is refused by name. Dropping it
// silently would break the rule that a rewritten mount keeps what it arrived
// with, and `ro` is the one that matters.
func TestASingleFileBindRefusesAnOptionItCannotCarry(t *testing.T) {
	const local = "/home/alice/app.conf"
	r, _ := newFileRewriter(local, "app.conf")

	_, err := r.ContainerCreate(t.Context(),
		[]byte(`{"HostConfig":{"Binds":["`+local+`:/etc/app.conf:ro,z"]}}`))
	if err == nil {
		t.Fatal("ContainerCreate = nil error, want a refusal naming the option")
	}
	if !strings.Contains(err.Error(), `"z"`) {
		t.Errorf("the refusal is %q, and does not name the option", err)
	}
}

// A workspace too old to carry a subpath is refused before anything is
// created, naming the version it reported.
func TestASingleFileNeedsADaemonThatCanMountIt(t *testing.T) {
	const local = "/home/alice/app.conf"
	s := &fakeSharer{files: map[string]string{local: "app.conf"}}
	v := &fakeVolumes{}
	r := &Rewriter{Shares: s, Volumes: v, NFSPort: 30000, DockerVersion: "24.0.7"}

	_, err := r.ContainerCreate(t.Context(),
		[]byte(`{"HostConfig":{"Binds":["`+local+`:/etc/app.conf"]}}`))
	if err == nil {
		t.Fatal("ContainerCreate = nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), "24.0.7") {
		t.Errorf("the refusal is %q, and does not say what the workspace reported", err)
	}
	if len(v.created) != 0 {
		t.Errorf("a volume was created for a mount that cannot work: %v", v.created)
	}
}

func TestSupportsSubpath(t *testing.T) {
	for _, c := range []struct {
		version string
		want    bool
	}{
		{"29.0.1", true},
		{"26.0.0", true},
		{"25.0.5", false},
		{"24.0.7", false},
		// Unknown is treated as capable: refusing a working setup because a
		// version string was an unexpected shape is worse than letting the
		// daemon answer.
		{"unavailable", true},
		{"", true},
	} {
		if got := supportsSubpath(c.version); got != c.want {
			t.Errorf("supportsSubpath(%q) = %v, want %v", c.version, got, c.want)
		}
	}
}

// newDaemonRewriter reports paths the workspace's daemon owns, and which of
// them this machine also has.
func newDaemonRewriter(owned []string, here ...string) (*Rewriter, *fakeSharer, *fakeVolumes) {
	s := &fakeSharer{}
	v := &fakeVolumes{}
	return &Rewriter{
		Shares: s, Volumes: v, NFSPort: 30000,
		DaemonPaths: owned,
		LocalExists: func(p string) bool { return slices.Contains(here, p) },
	}, s, v
}

// kind builds `-v /lib/modules:/lib/modules:ro` itself and its flags are not
// ours to edit, so the client has to leave the source alone (ADR 0041).
func TestADaemonOwnedSourceIsLeftAlone(t *testing.T) {
	r, sharer, volumes := newDaemonRewriter([]string{"/lib/modules"})

	out, err := r.ContainerCreate(t.Context(),
		[]byte(`{"HostConfig":{"Binds":["/lib/modules:/lib/modules:ro","/home/me/src:/app"]}}`))
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}

	binds := decodeHostConfig(t, out)["Binds"].([]any)
	if binds[0] != "/lib/modules:/lib/modules:ro" {
		t.Errorf("the daemon's own path was rewritten to %v", binds[0])
	}
	// The bind beside it is untouched by any of this.
	want := workspace.VolumeNameForID("", workspace.ShareID("/home/me/src")) + ":/app"
	if binds[1] != want {
		t.Errorf("the ordinary bind = %v, want %q", binds[1], want)
	}
	if slices.Contains(sharer.shared, "/lib/modules") {
		t.Error("the daemon's own path was exported from this machine")
	}
	if len(volumes.created) != 1 {
		t.Errorf("created %d volumes, want one for the ordinary bind", len(volumes.created))
	}
}

// A subdirectory of an owned path is owned; a name that merely starts with the
// same letters is not.
func TestDaemonOwnershipMatchesOnPathBoundaries(t *testing.T) {
	r, _, _ := newDaemonRewriter([]string{"/lib/modules"})

	for _, c := range []struct {
		source string
		owned  bool
	}{
		{"/lib/modules", true},
		{"/lib/modules/5.15", true},
		{"/lib/modules-backup", false},
		{"/lib", false},
	} {
		if got := r.ownedByDaemon(c.source); got != c.owned {
			t.Errorf("ownedByDaemon(%q) = %v, want %v", c.source, got, c.owned)
		}
	}
}

// This machine wins when both could claim the path, so a Linux client's own
// /etc is still its own.
func TestThisMachineWinsWhenItHasThePathToo(t *testing.T) {
	r, sharer, _ := newDaemonRewriter([]string{"/etc"}, "/etc/hostname")

	if _, err := r.ContainerCreate(t.Context(),
		[]byte(`{"HostConfig":{"Binds":["/etc/hostname:/x"]}}`)); err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	if !slices.Contains(sharer.shared, "/etc/hostname") {
		t.Error("a path this machine has was handed to the workspace")
	}
}

// A typo matches no owned path, so it is exported and fails exactly as before.
// That is what makes this safe where a blanket passthrough is not.
func TestATypoStillFails(t *testing.T) {
	r, sharer, _ := newDaemonRewriter([]string{"/lib/modules"})

	if _, err := r.ContainerCreate(t.Context(),
		[]byte(`{"HostConfig":{"Binds":["/hme/me/project:/app"]}}`)); err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	if !slices.Contains(sharer.shared, "/hme/me/project") {
		t.Error("a mistyped source was passed to the workspace instead of failing")
	}
}

// The --mount spelling takes the other path through the rewriter.
func TestADaemonOwnedMountIsLeftAlone(t *testing.T) {
	r, _, volumes := newDaemonRewriter([]string{"/lib/modules"})

	out, err := r.ContainerCreate(t.Context(), []byte(`{"HostConfig":{"Mounts":[
		{"Type":"bind","Source":"/lib/modules","Target":"/lib/modules","ReadOnly":true}
	]}}`))
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	mount := decodeHostConfig(t, out)["Mounts"].([]any)[0].(map[string]any)
	if mount["Type"] != "bind" || mount["Source"] != "/lib/modules" {
		t.Errorf("mount = %v, want the bind untouched", mount)
	}
	if len(volumes.created) != 0 {
		t.Errorf("a volume was created for the daemon's own path: %v", volumes.created)
	}
}

// An agent that predates the key sends nothing, which must read as "none".
func TestNoDaemonPathsIsTheOldBehaviour(t *testing.T) {
	r, sharer, _ := newDaemonRewriter(nil)

	if _, err := r.ContainerCreate(t.Context(),
		[]byte(`{"HostConfig":{"Binds":["/lib/modules:/lib/modules:ro"]}}`)); err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	if !slices.Contains(sharer.shared, "/lib/modules") {
		t.Error("without a list, a source was still not exported")
	}
}

// Git Bash rewrites BOTH halves of a `-v`, and only the container side can be
// restored blind (ADR 0040) -- so a workspace path typed there arrives as a
// Windows path under the Git installation and would match nothing. The two
// features have to compose, or `-v /lib/modules:/lib/modules:ro` works for kind
// and fails for the person testing the same command by hand.
func TestAMangledSourceStillMatchesADaemonPath(t *testing.T) {
	const mangled = `C:\Program Files\Git\lib\modules`

	r, sharer, _ := newDaemonRewriter([]string{"/lib/modules"})
	r.PosixSource = func(source string) string {
		if strings.HasPrefix(source, `C:\Program Files\Git`) {
			return strings.ReplaceAll(strings.TrimPrefix(source, `C:\Program Files\Git`), `\`, "/")
		}
		return ""
	}

	// Encoded rather than pasted: a Windows path is full of backslashes, which
	// are escapes in JSON.
	spec, err := json.Marshal(mangled + ":/lib/modules:ro")
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.ContainerCreate(t.Context(),
		[]byte(`{"HostConfig":{"Binds":[`+string(spec)+`]}}`))
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	if binds := decodeHostConfig(t, out)["Binds"].([]any); binds[0] != mangled+":/lib/modules:ro" {
		t.Errorf("the bind was rewritten to %v", binds[0])
	}
	if len(sharer.shared) != 0 {
		t.Errorf("a workspace path was exported from this machine: %v", sharer.shared)
	}
}

// The second reading is a CANDIDATE, and the workspace declaring the path is
// what makes it credible. Without that, the Windows path is just a Windows path.
func TestASecondReadingIsOnlyTakenWhenTheWorkspaceDeclaredIt(t *testing.T) {
	posix := func(string) string { return "/lib/modules" }

	// Nothing declared: the source is exported as it always was.
	r, sharer, _ := newDaemonRewriter(nil)
	r.PosixSource = posix
	if _, err := r.ContainerCreate(t.Context(),
		[]byte(`{"HostConfig":{"Binds":["C:/whatever:/x"]}}`)); err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	if len(sharer.shared) != 1 {
		t.Errorf("with nothing declared, the source was not exported: %v", sharer.shared)
	}

	// Declared, but the Windows path is really here: this machine still wins.
	r, sharer, _ = newDaemonRewriter([]string{"/lib/modules"}, "C:/whatever")
	r.PosixSource = posix
	if _, err := r.ContainerCreate(t.Context(),
		[]byte(`{"HostConfig":{"Binds":["C:/whatever:/x"]}}`)); err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	if len(sharer.shared) != 1 {
		t.Error("a path this machine has was handed to the workspace")
	}
}
