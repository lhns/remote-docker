package dockercli

// Reading a reverse-tunnel port back out of a volume's driver options.
//
// Pure string handling, and worth pinning on its own, because the option string
// is the one place a plausible shortcut is wrong: it contains BOTH "port=" and
// "mountport=", so anything searching for a substring finds whichever comes
// first in the text.

import "testing"

// The real shape, as workspace.NFSVolumeOptions writes it.
const realOptions = "addr=127.0.0.1,port=30001,mountport=30001,nfsvers=3,nolock," +
	"noacl,soft,timeo=30,retrans=2,actimeo=1,noatime,rsize=1048576,wsize=1048576"

func TestPortOf(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want int
	}{
		{"the real option string", realOptions, 30001},
		// mountport FIRST, which is what a substring search gets wrong: it
		// would read the 9 of a "mountport=9..." before reaching port=.
		{"mountport before port", "addr=127.0.0.1,mountport=39999,port=30002,soft", 30002},
		{"spaces around fields", "addr=127.0.0.1, port=30003 ,soft", 30003},
		{"no port at all", "addr=127.0.0.1,soft,nolock", 0},
		{"a port that is not a number", "addr=127.0.0.1,port=,soft", 0},
		{"empty", "", 0},
		// A local volume with no NFS options at all, which is what a volume
		// docker recreated behind our back looks like.
		{"not an nfs volume", "o=bind", 0},
	} {
		if got := portOf(tc.in); got != tc.want {
			t.Errorf("%s: portOf(%q) = %d, want %d", tc.name, tc.in, got, tc.want)
		}
	}
}

// A machine may hold volumes from more than one era. The majority is the set
// that would otherwise need rebuilding, so it is the one worth keeping.
func TestFirstPortTakesTheMajority(t *testing.T) {
	options := []string{
		"addr=127.0.0.1,port=30001,mountport=30001,soft",
		"addr=127.0.0.1,port=30001,mountport=30001,soft",
		"addr=127.0.0.1,port=39998,mountport=39998,soft",
	}
	if got := firstPort(options); got != 30001 {
		t.Errorf("firstPort = %d, want the majority 30001", got)
	}
}

// Deterministic on a tie, because ranging a map is not: a workspace that
// answered differently on identical input would hand one machine two ports
// across two connects, and each answer would strand the other's volumes.
func TestFirstPortIsDeterministicOnATie(t *testing.T) {
	options := []string{
		"addr=127.0.0.1,port=30009,soft",
		"addr=127.0.0.1,port=30002,soft",
	}
	for range 20 {
		if got := firstPort(options); got != 30002 {
			t.Fatalf("firstPort = %d on a tie, want the lower port 30002 every time", got)
		}
	}
}

// Nothing to say is 0, and every reason for it says the same thing: the caller
// chooses a port the way it always did.
func TestFirstPortWithNothingUsable(t *testing.T) {
	if got := firstPort(nil); got != 0 {
		t.Errorf("firstPort(nil) = %d, want 0", got)
	}
	if got := firstPort([]string{"", "o=bind"}); got != 0 {
		t.Errorf("firstPort with no nfs options = %d, want 0", got)
	}
}
