package session

import (
	"encoding/json"
	"testing"

	"github.com/lhns/remote-docker/core/cache"
)

// Whether a union's root can be given an owner is read from the workspace's
// greeting, never assumed: an agent before the field ignores it, and says
// nothing.
func TestTheGreetingSaysWhetherAUnionRootTakesAnOwner(t *testing.T) {
	for _, owner := range []bool{true, false} {
		line, err := json.Marshal(cache.Reply{Hello: &cache.Hello{Version: cache.Version, Owner: owner}})
		if err != nil {
			t.Fatal(err)
		}
		client := startWorkspace(t, func(string) (string, bool) { return string(line) + "\n", true })

		c, err := openCache(t.Context(), client)
		if err != nil {
			t.Fatalf("openCache: %v", err)
		}
		if got := c.ServesOwner(); got != owner {
			t.Errorf("greeting owner=%v read as %v", owner, got)
		}
		_ = c.Close()
	}
}
