package nfsserve

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
)

// These tests drive the server over a real NFSv3 conversation, using a real
// NFS client, on a real socket. They prove the wire protocol -- mount path
// resolution, file handles, attributes, reads and writes -- without needing a
// kernel mount, a container, or a Docker daemon, none of which exist on the
// machine this is developed on.
//
// What they do not cover is the Linux kernel client specifically. That is the
// integration suite's job.

// serve starts a server on loopback and returns its address.
func serve(t *testing.T, r *Registry) string {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { l.Close() })

	srv := New(r)
	go srv.Serve(l)
	return l.Addr().String()
}

func TestServeReadsAFile(t *testing.T) {
	dir := t.TempDir()
	const content = "the client's own files, seen from the workspace\n"
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	r := NewRegistry(DefaultAttrs)
	if _, err := r.RegisterCWD(dir); err != nil {
		t.Fatal(err)
	}
	target := mustMount(t, serve(t, r), "/cwd")

	f, err := target.Open("hello.txt")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()

	got, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != content {
		t.Errorf("read %q, want %q", got, content)
	}
}

// The share must be writable: containers build into their bind mounts.
func TestServeWritesAFile(t *testing.T) {
	dir := t.TempDir()
	r := NewRegistry(DefaultAttrs)
	if _, err := r.RegisterCWD(dir); err != nil {
		t.Fatal(err)
	}
	target := mustMount(t, serve(t, r), "/cwd")

	const content = "written from the workspace"
	f, err := target.OpenFile("out.txt", 0o644)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if _, err := f.Write([]byte(content)); err != nil {
		t.Fatalf("write: %v", err)
	}
	f.Close()

	// The whole point is that the bytes land on the client's real filesystem.
	onDisk, err := os.ReadFile(filepath.Join(dir, "out.txt"))
	if err != nil {
		t.Fatalf("reading what the workspace wrote: %v", err)
	}
	if string(onDisk) != content {
		t.Errorf("on disk %q, want %q", onDisk, content)
	}
}

// This is the case the single-mount design could not express at all: a bind
// source that is not under the working directory.
func TestServeAnUnrelatedDirectory(t *testing.T) {
	cwd, other := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "data.bin"), []byte("elsewhere"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := NewRegistry(DefaultAttrs)
	if _, err := r.RegisterCWD(cwd); err != nil {
		t.Fatal(err)
	}
	share, err := r.Register(other)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	// One server, one port, one tunnel -- two unrelated local directories.
	target := mustMount(t, serve(t, r), share.ExportPath)

	f, err := target.Open("data.bin")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()
	got, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "elsewhere" {
		t.Errorf("read %q, want %q", got, "elsewhere")
	}
}

func TestServeListsADirectory(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a.txt", "b.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}

	r := NewRegistry(DefaultAttrs)
	if _, err := r.RegisterCWD(dir); err != nil {
		t.Fatal(err)
	}
	target := mustMount(t, serve(t, r), "/cwd")

	entries, err := target.ReadDirPlus(".")
	if err != nil {
		t.Fatalf("ReadDirPlus: %v", err)
	}

	seen := map[string]bool{}
	for _, e := range entries {
		seen[e.Name()] = true
	}
	for _, want := range []string{"a.txt", "b.txt", "sub"} {
		if !seen[want] {
			t.Errorf("directory listing is missing %q (got %v)", want, seen)
		}
	}
}

// Mounting a subdirectory of a share has to work: a compose file may bind
// ./src rather than the project root.
func TestServeMountsASubdirectory(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "src")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "main.go"), []byte("package main"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := NewRegistry(DefaultAttrs)
	if _, err := r.RegisterCWD(dir); err != nil {
		t.Fatal(err)
	}
	target := mustMount(t, serve(t, r), "/cwd/src")

	f, err := target.Open("main.go")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()
	got, _ := io.ReadAll(f)
	if string(got) != "package main" {
		t.Errorf("read %q, want %q", got, "package main")
	}
}

// Ownership is synthesised, which is the documented fix for rclone's Windows
// limitation -- every file appearing as uid 1000 with chown failing.
func TestServeReportsSynthesisedOwnership(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	r := NewRegistry(Attrs{UID: 10042, GID: 10043, FileMode: 0o664, DirMode: 0o775})
	if _, err := r.RegisterCWD(dir); err != nil {
		t.Fatal(err)
	}
	target := mustMount(t, serve(t, r), "/cwd")

	attr, err := target.Getattr("f")
	if err != nil {
		t.Fatalf("Getattr: %v", err)
	}
	if attr.UID != 10042 {
		t.Errorf("UID = %d, want 10042", attr.UID)
	}
	if attr.GID != 10043 {
		t.Errorf("GID = %d, want 10043", attr.GID)
	}
	if perm := attr.Mode() & 0o777; perm != 0o664 {
		t.Errorf("mode = %04o, want 0664", perm)
	}
}

// An unregistered export must be refused. This is the boundary that keeps the
// workspace's view of this machine limited to what the user named.
func TestServeRefusesAnUnregisteredExport(t *testing.T) {
	r := NewRegistry(DefaultAttrs)
	if _, err := r.RegisterCWD(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	addr := serve(t, r)

	for _, export := range []string{"/", "/etc", "/m/0011223344556677", "/cwd/../..", "/cwdx"} {
		target, err := mountAt(t, addr, export)
		if err == nil {
			target.Close()
			t.Errorf("mounting %q succeeded, want refusal", export)
		}
	}

	// The refusals above must not have killed the server. go-nfs calls
	// ToHandle on the returned filesystem before checking the mount status,
	// so returning nil for a refusal panics it -- meaning any client could
	// crash this process by asking for a path that does not exist.
	if _, err := mountAt(t, addr, "/cwd"); err != nil {
		t.Fatalf("the server stopped serving after refusing a mount: %v", err)
	}
}
