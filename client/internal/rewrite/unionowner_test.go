package rewrite

import (
	"strings"
	"testing"

	"github.com/lhns/remote-docker/core/workspace"
)

// owner= works on a union as on a plain mount: the workspace is told it, so the
// merged root, which the agent creates as root, is the container's.
func TestOwnerReachesTheUnionRoot(t *testing.T) {
	r, _, cache := prefetchRewriter(workspace.PrefetchOff)
	if _, err := r.ContainerCreate(t.Context(), bindBody("write=back,owner=1000.2000")); err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	if cache.owner != (workspace.Owner{UID: 1000, GID: 2000, Set: true}) {
		t.Errorf("Prepare was told owner %v, want 1000:2000", cache.owner)
	}

	// The session's default counts, as it does for every mount.
	r, _, cache = prefetchRewriter(workspace.PrefetchOff)
	r.ShareOwner = workspace.Owner{UID: 7, GID: 7, Set: true}
	if _, err := r.ContainerCreate(t.Context(), bindBody("write=ephemeral")); err != nil {
		t.Fatal(err)
	}
	if cache.owner.UID != 7 {
		t.Errorf("Prepare was told owner %v, want the session's 7:7", cache.owner)
	}
}

// A workspace that cannot give the root an owner is refused by name before
// anything is created, for a non-root owner; root works against it as ever.
func TestAnOldWorkspaceRefusesANonRootUnionOwner(t *testing.T) {
	r, sharer, cache := prefetchRewriter(workspace.PrefetchOff)
	cache.noOwner = true

	_, err := r.ContainerCreate(t.Context(), bindBody("write=back,owner=1000"))
	if err == nil {
		t.Fatal("a union owned by 1000 was served by a workspace that cannot do it")
	}
	for _, want := range []string{"owner=1000:1000", "cannot give a union's root an owner", "\n  fix: update the workspace, or use write=through"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to contain %q", err, want)
		}
	}
	if len(sharer.shared) != 0 || cache.prepared != "" {
		t.Errorf("a refused mount shared %v and prepared %q", sharer.shared, cache.prepared)
	}

	if _, err := r.ContainerCreate(t.Context(), bindBody("write=back")); err != nil {
		t.Errorf("a root-owned union against an old workspace was refused: %v", err)
	}
	// A plain mount needs nothing from the cache channel.
	if _, err := r.ContainerCreate(t.Context(), bindBody("ro,owner=1000")); err != nil {
		t.Errorf("a plain mount with an owner was refused: %v", err)
	}
}
