package dockercli

import (
	"slices"
	"testing"
)

// Only a project of the run's own containers ending in -<client> is the run's:
// not a longer id with the same tail, not a project merely containing it, and
// not another account's project with the same suffix.
func TestProjectNetworks(t *testing.T) {
	out := "app-0123abcd_default|app-0123abcd\n" +
		"app-f0123abcd_default|app-f0123abcd\n" +
		"app-0123abcd-x_default|app-0123abcd-x\n" +
		"0123abcd_default|0123abcd\n" +
		"bridge|\n" +
		"theirs-0123abcd_default|theirs-0123abcd\n"
	projects := map[string]bool{"app-0123abcd": true, "app-f0123abcd": true, "app-0123abcd-x": true, "0123abcd": true}
	got := projectNetworks(out, "0123abcd", projects)
	if want := []string{"app-0123abcd_default"}; !slices.Equal(got, want) {
		t.Errorf("projectNetworks = %v, want %v", got, want)
	}
}
