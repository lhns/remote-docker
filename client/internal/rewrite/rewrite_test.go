package rewrite

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/lhns/remote-docker/pkg/workspace"
)

// fakeSharer records what was exported and hands back deterministic paths.
type fakeSharer struct {
	shared []string
	cwd    string // a local path to report as the /cwd share
	err    error
}

func (f *fakeSharer) Share(localPath string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	f.shared = append(f.shared, localPath)
	if localPath == f.cwd {
		return workspace.ExportCWD, nil
	}
	return workspace.ExportPathForID(workspace.ShareID(localPath)), nil
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

	wantVolume := workspace.VolumeNameForID(workspace.ShareID("/home/alice/project"))
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
	wantVolume := workspace.VolumeNameForID(workspace.ShareID("/mnt/data"))
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
	wantVolume := workspace.VolumeNameForID(workspace.ShareID("/home/alice/src"))
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
	if labels[OwnerLabel] != "alice" {
		t.Errorf("labels = %v, want %s=alice", labels, OwnerLabel)
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
	if !strings.Contains(string(out), OwnerLabel) {
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
	wantVolume := workspace.VolumeNameForID(workspace.ShareID("/src"))
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
