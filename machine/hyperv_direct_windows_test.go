//go:build windows

package machine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The direct executor is the PowerShell the backend ran before the executor
// existed: each typed operation produces exactly the script the ps* functions
// build, for the prefixed name.
func TestDirectHyperVRunsTheScriptsItAlwaysDid(t *testing.T) {
	local := t.TempDir()
	t.Setenv("LOCALAPPDATA", local)
	disk := filepath.Join(t.TempDir(), "flatcar.vhdx")
	if err := os.WriteFile(disk, []byte("disk"), 0o600); err != nil {
		t.Fatal(err)
	}

	type call struct{ script, stdin string }
	var calls []call
	d := directHyperV{run: func(_ context.Context, script, stdin string) (string, error) {
		calls = append(calls, call{script, stdin})
		return "", nil
	}}
	ctx := context.Background()
	dir := filepath.Join(local, "remote-docker", "machines", "dev")

	_ = d.Start(ctx, "dev")
	_ = d.Stop(ctx, "dev")
	_, _ = d.Address(ctx, "dev")
	_ = d.Retract(ctx, "dev")
	_ = d.Destroy(ctx, "dev")
	_, _, _ = d.Look(ctx, "dev")

	want := []string{
		psStart("rd-dev"), psStop("rd-dev"), psAddress("rd-dev"),
		psRemoveKVP("rd-dev"), psRemoveVM("rd-dev", dir), psGetVM("rd-dev"),
	}
	if len(calls) != len(want) {
		t.Fatalf("%d scripts, want %d", len(calls), len(want))
	}
	for i := range want {
		if calls[i].script != want[i] {
			t.Errorf("script %d =\n%s\nwant\n%s", i, calls[i].script, want[i])
		}
	}

	// Create: New-VM, the KVP items on stdin, the notes, then Start.
	calls = nil
	spec := Spec{Name: "dev", Account: "dev", Port: 2222, Image: "example.com/ws:1",
		Rootfs: disk, PublicKey: "ssh-ed25519 AAAA dev@host", CPUs: 2, MemoryMB: 2048}
	if err := d.Create(ctx, spec); err != nil {
		t.Fatal(err)
	}
	vhd := filepath.Join(dir, "disk.vhdx")
	notes := hyperVNotes{Generation: spec.Generation(), Key: keyFingerprint(spec.PublicKey)}
	wantCreate := []string{
		psNewVM("rd-dev", vhd, dir, spec), psAddKVP("rd-dev"),
		psSetNotes("rd-dev", notes), psStart("rd-dev"),
	}
	if len(calls) != len(wantCreate) {
		t.Fatalf("create ran %d scripts, want %d", len(calls), len(wantCreate))
	}
	for i := range wantCreate {
		if calls[i].script != wantCreate[i] {
			t.Errorf("create script %d =\n%s\nwant\n%s", i, calls[i].script, wantCreate[i])
		}
	}
	if !strings.Contains(calls[1].stdin, "ignition.config.0") || calls[0].stdin != "" {
		t.Errorf("the KVP items were not the only stdin: %q / %q", calls[0].stdin, calls[1].stdin)
	}
	if got, _ := os.ReadFile(vhd); string(got) != "disk" {
		t.Error("the disk was not copied beside the machine")
	}
}
