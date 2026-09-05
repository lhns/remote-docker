package dircache

import (
	"fmt"
	"math/rand"
	"sort"
	"testing"
	"time"
)

// Comparing fetch policies without a daemon, a container or a network.
//
// The policy is a pure function of (files, access order), which is the whole
// reason it can be measured here at all: the development machine has no Docker
// and CI is minutes away (CLAUDE.md). What this does NOT measure is the union,
// the tar, the codec or the tunnel. test/bench.sh measures those against the
// same workloads, and the two tables are compared: where they disagree by more
// than 2x, the model is missing something and that is the finding.
//
// The cost model, applied identically to every policy so the comparison holds
// even where the absolute numbers are approximate:
//
//   - a read that reaches the network costs 2 round trips, WHATEVER THE FILE'S
//     SIZE. ADR 0042 measured 300 READ and 422 ACCESS for 300 files, so 2.4
//     per file; this says 2 and undercounts slightly. It does not scale with
//     size because the per-file LOOKUP/ACCESS chain is the serial part: the
//     READs within one file are issued by kernel readahead with several in
//     flight, and a concurrency-1 server takes them straight off the socket,
//     so their latency is amortised rather than paid per rsize.
//   - a cache fetch costs ONE round trip per batch, whatever it holds, and the
//     batch is not available until that round trip and its transfer are
//     over. Charging it as instantly available flattered every cache policy.
//     Optimistic even so: the real apply (test/bench.sh, ADR 0045) costs more
//     than one round trip per batch. Every policy pays the same optimism, so
//     the comparison between them holds where the absolute numbers do not.
//   - bytes cost bytes/bandwidth on either path.

// transfer is how long the link takes to carry bytes.
func (l Link) transfer(bytes int64) time.Duration {
	return seconds(float64(bytes) / float64(l.Bandwidth))
}

// nfsRoundTrips is what one read through the live mount costs, whatever the
// file's size: see the cost model above.
const nfsRoundTrips = 2

type result struct {
	fetched  int64 // bytes over the cache channel
	nfsBytes int64 // bytes over the live mount
	nfsRT    int   // round trips the consumer blocked on
	batches  int   // cache-channel requests
	useful   int64 // bytes the consumer actually read
	resent   int64 // bytes fetched that the consumer had already read
	largest  int64 // the biggest single batch
}

// readTime is what the consumer blocks on. Cache fetches are off this path:
// they are asynchronous and the container never waits for one, which is the
// same assumption ADR 0044's own table makes.
func (r result) readTime(l Link) time.Duration {
	return time.Duration(r.nfsRT)*l.RTT + l.transfer(r.nfsBytes)
}

// linkTime is everything the link carried, which is what a fill that never
// settles is actually spending.
func (r result) linkTime(l Link) time.Duration {
	return r.readTime(l) + time.Duration(r.batches)*l.RTT + l.transfer(r.fetched)
}

// amplification is bytes moved over bytes wanted.
func (r result) amplification() float64 {
	if r.useful == 0 {
		return 0
	}
	return float64(r.fetched+r.nfsBytes) / float64(r.useful)
}

func seconds(f float64) time.Duration { return time.Duration(f * float64(time.Second)) }

// access is one read the consumer performs.
type access struct {
	path string
	size int64
}

// --- the policies ---------------------------------------------------------

// runNoCache is the live mount on its own: what `cached` does, once its
// attribute revalidation is gone and only READ and ACCESS remain.
func runNoCache(trace []access) result {
	var r result
	for _, a := range trace {
		r.useful += a.size
		r.nfsBytes += a.size
		r.nfsRT += nfsRoundTrips
	}
	return r
}

// runEager is today's fill: the whole tree smallest-file-first, and every read
// afterwards is a hit.
//
// Given the benefit of the doubt on purpose. It is charged nothing for the fill
// having to FINISH first, which in the one measurement that exists it did not:
// ADR 0044's cache table did not settle in 300s at 40ms RTT. So its readTime
// here is the best case it can possibly have, and its linkTime is the honest
// one.
func runEager(entries []Entry, trace []access) result {
	var r result
	order := append([]Entry(nil), entries...)
	sort.SliceStable(order, func(i, j int) bool { return order[i].Size < order[j].Size })

	cached := make(map[string]bool, len(order))
	var batch int64
	for _, e := range order {
		cached[e.Path] = true
		r.fetched += e.Size
		batch += e.Size
		if batch >= batchBytes {
			r.batches++
			batch = 0
		}
	}
	if batch > 0 {
		r.batches++
	}

	for _, a := range trace {
		r.useful += a.size
		if cached[a.path] {
			continue
		}
		r.nfsBytes += a.size
		r.nfsRT += nfsRoundTrips
	}
	return r
}

// runTree is the policy: nothing up front, and a node fetched once enough of
// it is in use. A batch is available only after its round trip and transfer,
// so a read that arrives before then still goes to the network.
func runTree(entries []Entry, trace []access, l Link, layout Layout) result {
	layout.Link = l
	tree, err := NewTree(entries, layout)
	if err != nil {
		panic(err)
	}
	return drive(tree, trace, l)
}

func drive(tree *Tree, trace []access, l Link) result {
	var (
		r         result
		now       time.Duration
		available = map[string]time.Duration{}
		read      = map[string]bool{}
	)
	for _, a := range trace {
		r.useful += a.size
		if at, ok := available[a.path]; ok && now >= at {
			continue
		}
		// Reaches the network, whether the file is unfetched or fetched but
		// not yet landed.
		cost := time.Duration(nfsRoundTrips)*l.RTT + l.transfer(a.size)
		r.nfsBytes += a.size
		r.nfsRT += nfsRoundTrips
		now += cost
		read[a.path] = true

		got := tree.Read(a.path, a.size)
		if len(got) == 0 {
			continue
		}
		// Anything in the batch the consumer had already read, including
		// the file that triggered it, crosses twice. That is the double-send
		// the design accepts and bounds.
		var bytes int64
		for _, e := range got {
			bytes += e.Size
			if read[e.Path] {
				r.resent += e.Size
			}
		}
		r.batches++
		r.fetched += bytes
		r.largest = max(r.largest, bytes)
		lands := now + l.RTT + l.transfer(bytes)
		for _, e := range got {
			available[e.Path] = lands
		}
	}
	return r
}

// --- trees and workloads --------------------------------------------------

// sourceTree is files in walk order: dirs in lexical order, files inside them
// likewise, which is what filepath.WalkDir yields.
//
// Sizes are drawn to look like a source tree rather than uniformly: most files
// are a few KB, some are tens to hundreds of KB, a few are multi-megabyte
// artifacts above LargeFile. A uniform distribution would hide the whole
// question, since large files are excluded precisely because real trees are
// not uniform.
func sourceTree(dirs, perDir int, seed int64) []Entry {
	rng := rand.New(rand.NewSource(seed))
	var out []Entry
	for d := range dirs {
		for f := range perDir {
			var size int64
			switch n := rng.Intn(100); {
			case n < 85:
				size = int64(500 + rng.Intn(12_000)) // ordinary source
			case n < 97:
				size = int64(20_000 + rng.Intn(180_000)) // generated, vendored
			default:
				size = int64(2 << 20)                      // an artifact
				size += int64(rng.Intn(30)) * int64(1<<20) // up to ~32 MiB
			}
			out = append(out, Entry{Path: fmt.Sprintf("pkg%03d/file%03d.go", d, f), Size: size})
		}
	}
	return out
}

func traceOf(entries []Entry, pick func(i int, e Entry) bool) []access {
	var out []access
	for i, e := range entries {
		if pick(i, e) {
			out = append(out, access{path: e.Path, size: e.Size})
		}
	}
	return out
}

// repeat is the same workload run again, which is what a session does: a
// container starts, does its work, and starts again.
func repeat(trace []access, n int) []access {
	out := make([]access, 0, len(trace)*n)
	for range n {
		out = append(out, trace...)
	}
	return out
}

func smallOnly(entries []Entry, threshold int64) []Entry {
	var out []Entry
	for _, e := range entries {
		if e.Size <= threshold {
			out = append(out, e)
		}
	}
	return out
}

var links = []struct {
	name string
	link Link
}{
	{"160ms/100Mbit", Link{RTT: 160 * time.Millisecond, Bandwidth: 12_500_000}},
	{"40ms/100Mbit", Link{RTT: 40 * time.Millisecond, Bandwidth: 12_500_000}},
	{"20ms/100Mbit", Link{RTT: 20 * time.Millisecond, Bandwidth: 12_500_000}},
	{"0.1ms/1Gbit", Link{RTT: 100 * time.Microsecond, Bandwidth: 125_000_000}},
	{"0.3ms/10Mbit", Link{RTT: 300 * time.Microsecond, Bandwidth: 1_250_000}},
}

func workloads(entries []Entry, seed int64) []struct {
	name  string
	trace []access
} {
	rng := rand.New(rand.NewSource(seed))
	dense := traceOf(entries, func(int, Entry) bool { return true })
	return []struct {
		name  string
		trace []access
	}{
		{"dense", dense},
		{"subtree 10%", traceOf(entries, func(i int, _ Entry) bool { return i/25 < 4 })},
		{"sparse 2%", traceOf(entries, func(int, Entry) bool { return rng.Intn(100) < 2 })},
		{"dense x3", repeat(dense, 3)},
	}
}

// --- the comparison -------------------------------------------------------

// The table. Run with -v to read it:
//
//	go test ./dircache -run TestPolicyComparison -v
//
// The same rows and columns test/bench.sh produces on a real union, so the
// two can be laid side by side. The three assertions are ADR 0045's pass
// criteria at the 160ms link, on link time: dense within 2x of eager, subtree
// faster than eager, sparse within 1.2x of no cache at all.
func TestPolicyComparison(t *testing.T) {
	entries := sourceTree(40, 25, 1) // 1000 files, 40 directories
	var total, small int64
	for _, e := range entries {
		total += e.Size
		if e.Size <= defaultLargeFile {
			small += e.Size
		}
	}
	t.Logf("tree: %d files, %s, of which %s is at or under LargeFile (%s)",
		len(entries), bytesOf(total), bytesOf(small), bytesOf(defaultLargeFile))

	for _, w := range workloads(entries, 7) {
		var read int64
		for _, a := range w.trace {
			read += a.size
		}
		t.Logf("")
		t.Logf("workload %q: %d reads, %s", w.name, len(w.trace), bytesOf(read))
		t.Logf("  %-14s %-6s %10s %10s %6s %9s %9s %8s", "link", "policy", "fetched", "nfs", "amp", "readtime", "linktime", "resent")

		for _, l := range links {
			none := runNoCache(w.trace)
			eager := runEager(entries, w.trace)
			tree := runTree(entries, w.trace, l.link, Layout{})
			for _, run := range []struct {
				name string
				res  result
			}{{"none", none}, {"eager", eager}, {"tree", tree}} {
				t.Logf("  %-14s %-6s %10s %10s %5.1fx %9s %9s %8s",
					l.name, run.name,
					bytesOf(run.res.fetched), bytesOf(run.res.nfsBytes),
					run.res.amplification(),
					round(run.res.readTime(l.link)), round(run.res.linkTime(l.link)),
					bytesOf(run.res.resent))
			}
			if l.name != "160ms/100Mbit" {
				continue
			}
			treeTime, eagerTime, noneTime := tree.linkTime(l.link), eager.linkTime(l.link), none.linkTime(l.link)
			switch w.name {
			case "dense":
				if treeTime > 2*eagerTime {
					t.Errorf("dense: tree %s against eager %s, over 2x", round(treeTime), round(eagerTime))
				}
			case "subtree 10%":
				if treeTime >= eagerTime {
					t.Errorf("subtree: tree %s is not faster than eager %s", round(treeTime), round(eagerTime))
				}
			case "sparse 2%":
				if float64(treeTime) > 1.2*float64(noneTime) {
					t.Errorf("sparse: tree %s against no cache %s, over 1.2x", round(treeTime), round(noneTime))
				}
			}
		}
	}
}

// --- the properties -------------------------------------------------------

// A node promotion never moves more than the node already had fetched. This
// is the doubling rule as an inequality, and it holds for every internal node
// on every trace.
func TestNodePromotionIsAtMostTwofold(t *testing.T) {
	entries := sourceTree(40, 25, 1)
	for _, w := range workloads(entries, 7) {
		for _, l := range links {
			tree, _ := NewTree(entries, Layout{Link: l.link})
			tree.onFetch = func(n *node, sent int64) {
				if n.leaf() {
					return
				}
				// After fetch, fetched == bytes; what it had before is the
				// difference.
				had := n.fetched - sent
				if sent > had {
					t.Errorf("%s/%s: a node sent %s having fetched only %s before; the doubling rule is broken",
						l.name, w.name, bytesOf(sent), bytesOf(had))
				}
			}
			drive(tree, w.trace, l.link)
		}
	}
}

// No single batch exceeds the cap the link sets.
func TestNoBatchExceedsTheCap(t *testing.T) {
	entries := sourceTree(40, 25, 1)
	for _, w := range workloads(entries, 7) {
		for _, l := range links {
			layout, _ := Layout{Link: l.link}.withDefaults()
			tree, _ := NewTree(entries, layout)
			cap := int64(layout.MaxFetchBDP * float64(tree.bdp()))
			r := drive(tree, w.trace, l.link)
			// A single-file node up to LargeFile may be its own batch when
			// its parent promotes it; that is the one legitimate excess.
			if r.largest > max(cap, layout.LargeFile) {
				t.Errorf("%s/%s: a batch of %s exceeds the cap of %s",
					l.name, w.name, bytesOf(r.largest), bytesOf(cap))
			}
		}
	}
}

// A file above LargeFile is never in a batch, on any trace.
func TestLargeFilesAreNeverFetched(t *testing.T) {
	entries := sourceTree(40, 25, 1)
	for _, w := range workloads(entries, 7) {
		for _, l := range links {
			layout, _ := Layout{Link: l.link}.withDefaults()
			tree, _ := NewTree(entries, layout)
			tree.onFetch = func(n *node, _ int64) {
				for _, e := range n.entries {
					if e.Size > layout.LargeFile {
						t.Errorf("%s/%s: %s (%s) is above LargeFile and was fetched", l.name, w.name, e.Path, bytesOf(e.Size))
					}
				}
			}
			drive(tree, w.trace, l.link)
			for _, e := range entries {
				if e.Size > layout.LargeFile && tree.cached(e.Path) {
					t.Errorf("%s/%s: %s is above LargeFile and reports cached", l.name, w.name, e.Path)
				}
			}
		}
	}
}

// The branching factor decides which sizes are on offer and nothing else:
// total bytes fetched per workload agree within 2x across k. This is the
// matrix that found the uncapped climb, kept as the assertion that it stays
// capped.
func TestShapeIndependence(t *testing.T) {
	entries := sourceTree(40, 25, 1)
	l := Link{RTT: 160 * time.Millisecond, Bandwidth: 12_500_000}
	for _, w := range workloads(entries, 7) {
		var lo, hi int64 = -1, 0
		for _, k := range []int{2, 4, 8} {
			r := runTree(entries, w.trace, l, Layout{Branching: k})
			t.Logf("%-12s k=%d fetched=%s linktime=%s", w.name, k, bytesOf(r.fetched), round(r.linkTime(l)))
			if lo < 0 || r.fetched < lo {
				lo = r.fetched
			}
			hi = max(hi, r.fetched)
		}
		if lo > 0 && hi > 2*lo {
			t.Errorf("%s: fetched bytes vary %s to %s across k, more than 2x; the shape is leaking into policy",
				w.name, bytesOf(lo), bytesOf(hi))
		}
	}
}

// A link change mid-run loses nothing: no rebuild, cached answers unchanged,
// and the next batch is capped by the new BDP.
func TestLinkChangeMidRunKeepsEvidence(t *testing.T) {
	entries := sourceTree(40, 25, 1)
	fast := Link{RTT: 160 * time.Millisecond, Bandwidth: 12_500_000}
	slow := Link{RTT: 160 * time.Millisecond, Bandwidth: 1_250_000} // a tenth of the bandwidth
	trace := traceOf(entries, func(i int, _ Entry) bool { return i/25 < 4 })

	tree, _ := NewTree(entries, Layout{Link: fast})
	half := len(trace) / 2
	drive(tree, trace[:half], fast)

	var cachedBefore []string
	for _, e := range entries {
		if tree.cached(e.Path) {
			cachedBefore = append(cachedBefore, e.Path)
		}
	}
	tree.SetLink(slow)
	for _, p := range cachedBefore {
		if !tree.cached(p) {
			t.Fatalf("%s was cached before the link changed and is not after; the tree was rebuilt", p)
		}
	}

	layout, _ := Layout{Link: slow}.withDefaults()
	cap := int64(layout.MaxFetchBDP * float64(slow.BDP()))
	tree.onFetch = func(_ *node, sent int64) {
		if sent > max(cap, layout.LargeFile) {
			t.Errorf("after the link slowed, a batch of %s exceeds the new cap of %s", bytesOf(sent), bytesOf(cap))
		}
	}
	drive(tree, trace[half:], slow)
}

// Read one byte of one file a million times: exactly one leaf comes, once.
func TestAPolledFilePullsItsLeafOnce(t *testing.T) {
	entries := smallOnly(sourceTree(40, 25, 1), defaultLargeFile)
	l := Link{RTT: 160 * time.Millisecond, Bandwidth: 12_500_000}
	tree, _ := NewTree(entries, Layout{Link: l})

	var batches int
	var fetched int64
	for range 1_000_000 {
		got := tree.Read(entries[0].Path, 1)
		if len(got) > 0 {
			batches++
			for _, e := range got {
				fetched += e.Size
			}
		}
	}
	// The poll never reads the file whole, so the file itself is never
	// evidence enough for anything above its own leaf.
	if batches > 1 {
		t.Errorf("a poll triggered %d batches; a leaf pulls once", batches)
	}
	if fetched > 2*defaultLeafBytes {
		t.Errorf("a poll pulled %s; at most about one leaf", bytesOf(fetched))
	}
}

// A single-file node never promotes on its own read; it comes with its
// parent.
func TestASingleFileNodeNeverPromotesItself(t *testing.T) {
	// Enough small files to outweigh one 400 KiB file several times over,
	// all in one window so the file is a level-1 sibling of the composites.
	// The siblings have to dominate: byte fraction is the rule, and a
	// single large file beside a little evidence is refused BY DESIGN.
	var entries []Entry
	for i := range 40 {
		entries = append(entries, Entry{Path: fmt.Sprintf("a/f%02d", i), Size: 30_000})
	}
	entries = append(entries, Entry{Path: "a/big", Size: 400 << 10})
	l := Link{RTT: 160 * time.Millisecond, Bandwidth: 12_500_000}
	tree, _ := NewTree(entries, Layout{Link: l})

	if got := tree.Read("a/big", 400<<10); len(got) != 0 {
		t.Fatalf("reading the single-file node whole fetched %d files; nothing to gain", len(got))
	}
	if tree.cached("a/big") {
		t.Fatal("the single-file node reports cached after its own read")
	}

	// Now use its siblings: the parent should promote and bring it along.
	var brought bool
	for _, e := range entries[:40] {
		for _, got := range tree.Read(e.Path, e.Size) {
			if got.Path == "a/big" {
				brought = true
			}
		}
	}
	if !brought {
		t.Error("the single-file node never came with its parent; it can only arrive that way")
	}
}

// For every leaf promoted by its own reads, the bytes in the batch that were
// already read are at most f x the leaf. That is the double-send, bounded.
func TestDoubleSendIsBoundedPerLeaf(t *testing.T) {
	entries := smallOnly(sourceTree(40, 25, 1), defaultLargeFile)
	l := Link{RTT: 160 * time.Millisecond, Bandwidth: 12_500_000}
	layout, _ := Layout{Link: l}.withDefaults()
	tree, _ := NewTree(entries, layout)

	// Track reads per path to know what was already read at fetch time.
	read := map[string]bool{}
	tree.onFetch = func(n *node, _ int64) {
		if !n.leaf() {
			return
		}
		var resent, total int64
		for _, e := range n.entries {
			total += e.Size
			if read[e.Path] {
				resent += e.Size
			}
		}
		// f is at least FMin and at most 1, and the read that crosses the
		// bar can be as large as the leaf's largest file, so that is the
		// slack: the bound is f of the leaf plus one file.
		var largest int64
		for _, e := range n.entries {
			largest = max(largest, e.Size)
		}
		mean := float64(total) / float64(len(n.entries))
		f := max(mean/float64(tree.bdp()), layout.FMin)
		if float64(resent) > f*float64(total)+float64(largest) {
			t.Errorf("a leaf of %s in %d files re-sent %s, over f=%.2f of it plus one file of %s",
				bytesOf(total), len(n.entries), bytesOf(resent), f, bytesOf(largest))
		}
	}
	for _, e := range entries {
		read[e.Path] = true
		tree.Read(e.Path, e.Size)
	}
}

// Sparse random access is the case an eager fill is worst at, and the one the
// tree exists for.
func TestSparseAccessFetchesFarLessThanEager(t *testing.T) {
	entries := sourceTree(40, 25, 1)
	rng := rand.New(rand.NewSource(7))
	trace := traceOf(entries, func(int, Entry) bool { return rng.Intn(100) < 2 })
	l := Link{RTT: 160 * time.Millisecond, Bandwidth: 12_500_000}

	eager := runEager(entries, trace)
	tree := runTree(entries, trace, l, Layout{})
	if tree.fetched >= eager.fetched/4 {
		t.Errorf("sparse access fetched %s under the tree against %s under eager",
			bytesOf(tree.fetched), bytesOf(eager.fetched))
	}
	t.Logf("sparse: tree moved %s, eager moved %s", bytesOf(tree.fetched), bytesOf(eager.fetched))
}

// Repeated passes converge on the small files being fetched, and the repeat
// costs far less than the first pass.
func TestRepeatedPassesConverge(t *testing.T) {
	// Small files only: large ones are read from NFS every pass by design,
	// and would swamp the measurement of what the cache buys.
	entries := smallOnly(sourceTree(20, 25, 3), defaultLargeFile)
	var smallBytes int64
	for _, e := range entries {
		smallBytes += e.Size
	}
	once := traceOf(entries, func(int, Entry) bool { return true })
	l := Link{RTT: 160 * time.Millisecond, Bandwidth: 12_500_000}

	first := runTree(entries, once, l, Layout{})
	third := runTree(entries, repeat(once, 3), l, Layout{})
	if third.fetched < smallBytes*9/10 {
		t.Errorf("three passes fetched %s of %s small; dense access should converge on the small files",
			bytesOf(third.fetched), bytesOf(smallBytes))
	}
	if third.readTime(l) >= first.readTime(l)*2 {
		t.Errorf("three passes cost %s against %s for one; the cache bought nothing on the repeats",
			round(third.readTime(l)), round(first.readTime(l)))
	}
	t.Logf("one pass: fetched %s, read %s; three passes: fetched %s, read %s",
		bytesOf(first.fetched), round(first.readTime(l)), bytesOf(third.fetched), round(third.readTime(l)))
}

// A file added mid-run keeps everything else's evidence.
func TestAddingFilesKeepsEvidence(t *testing.T) {
	entries := smallOnly(sourceTree(40, 25, 1), defaultLargeFile)
	l := Link{RTT: 160 * time.Millisecond, Bandwidth: 12_500_000}
	half := len(entries) / 2

	tree, _ := NewTree(entries[:half], Layout{Link: l})
	trace := traceOf(entries[:half], func(i int, _ Entry) bool { return i/25 < 4 })
	drive(tree, trace, l)

	var cached []string
	for _, e := range entries[:half] {
		if tree.cached(e.Path) {
			cached = append(cached, e.Path)
		}
	}
	if len(cached) == 0 {
		t.Fatal("nothing was cached before the add; the test proves nothing")
	}

	tree.Add(entries[half:])
	for _, p := range cached {
		if !tree.cached(p) {
			t.Errorf("%s was cached before Add and is not after", p)
		}
	}
	for _, e := range entries[half:] {
		if _, ok := tree.where[e.Path]; !ok {
			t.Errorf("%s was added and the tree does not hold it", e.Path)
		}
	}
}

// Branching below 2 is refused by name, not coerced.
func TestBranchingBelowTwoIsRefused(t *testing.T) {
	if _, err := NewTree(nil, Layout{Branching: 1}); err == nil {
		t.Error("Branching 1 was accepted")
	}
	if _, err := NewTree(nil, Layout{Branching: 0}); err != nil {
		t.Errorf("Branching 0 (the default) was refused: %v", err)
	}
}

// --- rendering ------------------------------------------------------------

func bytesOf(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1fG", float64(n)/float64(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1fM", float64(n)/float64(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fK", float64(n)/float64(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}

func round(d time.Duration) string {
	if d >= time.Second {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return fmt.Sprintf("%.0fms", float64(d)/float64(time.Millisecond))
}
