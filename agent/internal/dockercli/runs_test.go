package dockercli

import (
	"slices"
	"testing"
)

// Only a project ending in -<client> is the run's: not a longer id with the
// same tail, and not a project merely containing it.
func TestProjectNetworks(t *testing.T) {
	out := "app-0123abcd_default|app-0123abcd\n" +
		"app-f0123abcd_default|app-f0123abcd\n" +
		"app-0123abcd-x_default|app-0123abcd-x\n" +
		"0123abcd_default|0123abcd\n" +
		"bridge|\n"
	got := projectNetworks(out, "0123abcd")
	if want := []string{"app-0123abcd_default"}; !slices.Equal(got, want) {
		t.Errorf("projectNetworks = %v, want %v", got, want)
	}
}
