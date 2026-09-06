package dircache

// What to prefetch, and when: small files in walk order, cut into fixed leaves
// under a binary tree, and a node fetched whole once enough of it is in use.
// ADR 0045 is the decision and docs/caching.md the reasoning; this file is the
// mechanism, kept pure so sim_test.go can measure it.

import (
	"errors"
	"fmt"
	"sort"
	"time"
)

// Link is what the cache sits on the other side of. It shapes decisions and
// never the tree, so a link that changes mid-session changes what is fetched
// next and nothing else.
type Link struct {
	// RTT is the round trip time.
	RTT time.Duration

	// Bandwidth is bytes per second. Zero means unmeasured.
	Bandwidth int64
}

// BDP is the bandwidth-delay product: how many bytes travel in one round trip.
// It is the size at which one more request and more bytes cost the same.
func (l Link) BDP() int64 {
	if l.Bandwidth <= 0 || l.RTT <= 0 {
		return 0
	}
	return int64(float64(l.Bandwidth) * l.RTT.Seconds())
}

// Layout is how a share's small files are arranged and when a node is fetched.
// The zero value is usable; every field has a default.
type Layout struct {
	Link Link

	// LargeFile is the size above which a file is left to NFS and never
	// prefetched. Files at or below it are read whole by anything that reads
	// them, which is what makes whole-file caching the right granularity for
	// them and bytes-read an exact measure.
	LargeFile int64

	// LeafBytes is the smallest unit the tree resolves. FIXED, and not
	// derived from the link: bandwidth varies during a session, and a tree
	// rebuilt to match would discard every bit of accumulated evidence, which
	// is the one thing the policy cannot recreate.
	LeafBytes int64

	// WindowLeaves is how many leaves' worth of consecutive files are sorted
	// by size together before being cut into leaves. It is the locality axis
	// of a two-dimensional problem: too small and there is nothing to sort,
	// too large and files from distant parts of the tree are grouped by size,
	// which in the limit is a global size sort that destroys locality.
	WindowLeaves int

	// Branching is how many children an internal node has. Two, because the
	// tree's job is to offer a range of fetch sizes and the link picks one by
	// climbing: binary offers 64K, 128K, 256K and so on, the finest steps.
	// Below 2 is refused by name.
	Branching int

	// MaxFetchBDP caps one fetch at this many round trips' worth of bytes.
	// This is what makes the fetch unit follow the link, and what makes the
	// branching factor stop mattering: uncapped, a fetched child leaves its
	// parent 1/k full and k becomes a threshold multiplier.
	MaxFetchBDP float64

	// FMin is the least evidence a leaf ever needs, so a leaf of very cheap
	// files still needs somebody to have touched it.
	FMin float64

	// NodePromote is the fraction of an internal node that must already be
	// fetched before the rest is. Above a half, so that a binary parent does
	// not promote on one fetched child alone.
	NodePromote float64
}

// defaultLeafBytes and defaultNodePromote are measured, not chosen; ADR 0045
// has the table.
const (
	defaultLargeFile    = 1 << 20
	defaultLeafBytes    = 256 << 10
	defaultWindowLeaves = 32
	defaultBranching    = 2
	defaultMaxFetchBDP  = 4.0
	defaultFMin         = 0.05
	defaultNodePromote  = 0.6
)

// ErrBranching is returned for a Layout whose Branching is below 2.
var ErrBranching = errors.New("dircache: tree branching must be at least 2")

func (l Layout) withDefaults() (Layout, error) {
	if l.Branching == 0 {
		l.Branching = defaultBranching
	}
	if l.Branching < 2 {
		return l, fmt.Errorf("%w: got %d", ErrBranching, l.Branching)
	}
	if l.LargeFile <= 0 {
		l.LargeFile = defaultLargeFile
	}
	if l.LeafBytes <= 0 {
		l.LeafBytes = defaultLeafBytes
	}
	if l.WindowLeaves <= 0 {
		l.WindowLeaves = defaultWindowLeaves
	}
	if l.MaxFetchBDP <= 0 {
		l.MaxFetchBDP = defaultMaxFetchBDP
	}
	if l.FMin <= 0 {
		l.FMin = defaultFMin
	}
	if l.NodePromote <= 0 {
		l.NodePromote = defaultNodePromote
	}
	return l, nil
}

// fileState is what has happened to one file: sent into the cache, or how
// much of it the consumer read over the network, capped at its size.
type fileState struct {
	stored bool
	read   int64
}

// node is a leaf or an internal node of the tree.
type node struct {
	parent *node
	kids   []*node

	// entries and state are the leaf's files, empty for an internal node.
	// A "leaf" here is any node with no children, which includes a file too
	// large for a level-0 leaf placed on its own at the level that fits it.
	entries []Entry
	state   []fileState

	// bytes is everything under this node; fetched is what is in the upper;
	// read is what has crossed the network for files that are not.
	bytes, fetched, read int64

	// files and fetchedFiles give the mean size of what a fetch would move.
	files, fetchedFiles int
}

func (n *node) leaf() bool { return len(n.kids) == 0 }

func (n *node) unfetched() int64    { return n.bytes - n.fetched }
func (n *node) unfetchedFiles() int { return n.files - n.fetchedFiles }

// unread reports whether a leaf still holds a file that is neither stored
// nor read whole -- something a promotion would actually bring. A single-file
// node read whole has nothing, and promoting it would send a file the
// consumer just finished for nobody's benefit.
func (n *node) unread() bool {
	for i, st := range n.state {
		if !st.stored && st.read < n.entries[i].Size {
			return true
		}
	}
	return false
}

// Tree is one share's layout and what is in its cache.
//
// Not safe for concurrent use; the caller serialises, as the fill already does.
type Tree struct {
	layout  Layout
	root    *node
	entries []Entry

	// where finds the leaf and index for a path. A path the walk never
	// yielded, or one above LargeFile, is absent and is served live.
	where map[string]location

	// onFetch is the simulator's hook (sim_test.go), called with each node
	// promoted and the bytes that sent. Nil in production.
	onFetch func(n *node, sent int64)
}

type location struct {
	leaf *node
	idx  int
}

// NewTree arranges entries and returns a tree with nothing fetched.
//
// entries must be in WALK ORDER (lexical depth-first), which is what makes
// siblings adjacent and cousins near. Files above LargeFile are dropped here
// rather than by the caller, so nothing can offer the tree a file it must not
// hold.
func NewTree(entries []Entry, layout Layout) (*Tree, error) {
	layout, err := layout.withDefaults()
	if err != nil {
		return nil, err
	}
	t := &Tree{layout: layout}
	t.rebuild(entries, nil)
	return t, nil
}

// SetLink replaces the link the policy decides against. The tree is not
// rebuilt and nothing is forgotten; only what is fetched next changes.
func (t *Tree) SetLink(l Link) { t.layout.Link = l }

// Add offers files the walk has found since the tree was built, and keeps
// every file's state. Promotion only ever considers files the tree holds, so
// a tree built from a partial walk is correct and merely knows less.
//
// A rebuild rather than an in-place insertion: the walk yields in batches,
// and a sort per batch is cheaper to be right about than re-parenting.
func (t *Tree) Add(entries []Entry) {
	all := make([]Entry, 0, len(t.entries)+len(entries))
	all = append(all, t.entries...)
	all = append(all, entries...)
	t.rebuild(all, t.where)
}

// rebuild lays out entries from scratch, carrying state over by path.
func (t *Tree) rebuild(entries []Entry, prev map[string]location) {
	t.entries = entries
	t.where = make(map[string]location, len(entries))
	small := make([]Entry, 0, len(entries))
	for _, e := range entries {
		if e.Size <= t.layout.LargeFile {
			small = append(small, e)
		}
	}
	items := t.place(small)
	if prev != nil {
		for path, loc := range t.where {
			if old, ok := prev[path]; ok {
				st := old.leaf.state[old.idx]
				loc.leaf.state[loc.idx] = st
				loc.leaf.account(st, loc.leaf.entries[loc.idx].Size)
			}
		}
	}
	t.root = t.build(items)
}

// account adds one file's carried-over state to a fresh leaf's counters.
func (n *node) account(st fileState, size int64) {
	if st.stored {
		n.fetched += size
		n.fetchedFiles++
		return
	}
	n.read += min(st.read, size)
}

// placed is a node with the level it enters the build at. Level 0 is a leaf
// of small files; a file too large for one is its own node at the level that
// fits it, the buddy allocator's rule, so every node at a level is within 2x
// of every other and "depth = size" holds.
type placed struct {
	n     *node
	level int
}

// place cuts the walk order into windows, sorts each window by size, packs
// what fits into leaves, and places what does not at its own level.
func (t *Tree) place(entries []Entry) []placed {
	windowBytes := t.layout.LeafBytes * int64(t.layout.WindowLeaves)

	var (
		out    []placed
		window []Entry
		acc    int64
	)
	flush := func() {
		if len(window) == 0 {
			return
		}
		// Smallest first, path breaking ties so both ends and successive
		// runs derive the same tree from the same input.
		sort.SliceStable(window, func(i, j int) bool {
			if window[i].Size != window[j].Size {
				return window[i].Size < window[j].Size
			}
			return window[i].Path < window[j].Path
		})
		out = append(out, t.cut(window)...)
		window, acc = nil, 0
	}

	for _, e := range entries {
		window = append(window, e)
		acc += e.Size
		if acc >= windowBytes {
			flush()
		}
	}
	flush()
	return out
}

// cut turns one sorted window into level-0 leaves of at most LeafBytes, and
// single-file nodes at the level that fits each larger file.
func (t *Tree) cut(window []Entry) []placed {
	var (
		out  []placed
		cur  []Entry
		size int64
	)
	emit := func(entries []Entry, level int) {
		n := &node{entries: entries, state: make([]fileState, len(entries)), files: len(entries)}
		for i, e := range entries {
			n.bytes += e.Size
			t.where[e.Path] = location{leaf: n, idx: i}
		}
		out = append(out, placed{n: n, level: level})
	}

	for _, e := range window {
		if e.Size > t.layout.LeafBytes {
			if len(cur) > 0 {
				emit(cur, 0)
				cur, size = nil, 0
			}
			emit([]Entry{e}, t.levelFor(e.Size))
			continue
		}
		if size+e.Size > t.layout.LeafBytes && len(cur) > 0 {
			emit(cur, 0)
			cur, size = nil, 0
		}
		cur = append(cur, e)
		size += e.Size
	}
	if len(cur) > 0 {
		emit(cur, 0)
	}
	return out
}

// levelFor is the smallest level whose node size holds a file.
func (t *Tree) levelFor(size int64) int {
	level := 0
	for span := t.layout.LeafBytes; span < size; span <<= 1 {
		level++
	}
	return level
}

// build pairs items level by level until one remains.
//
// At level L, consecutive items at or below L are grouped k at a time into a
// parent at L+1; an item that finds no partner simply moves up a level on its
// own, so a lone leaf beside a large single-file node is not wrapped in
// pointless parents. Items above L pass through untouched until the build
// reaches them.
func (t *Tree) build(items []placed) *node {
	if len(items) == 0 {
		return &node{}
	}
	for level := 0; len(items) > 1; level++ {
		var next []placed
		for i := 0; i < len(items); {
			if items[i].level > level {
				next = append(next, items[i])
				i++
				continue
			}
			end := i
			for end < len(items) && end-i < t.layout.Branching && items[end].level <= level {
				end++
			}
			if end-i == 1 {
				next = append(next, placed{n: items[i].n, level: level + 1})
				i = end
				continue
			}
			parent := &node{}
			for _, p := range items[i:end] {
				p.n.parent = parent
				parent.kids = append(parent.kids, p.n)
				parent.bytes += p.n.bytes
				parent.fetched += p.n.fetched
				parent.read += p.n.read
				parent.files += p.n.files
				parent.fetchedFiles += p.n.fetchedFiles
			}
			next = append(next, placed{n: parent, level: level + 1})
			i = end
		}
		items = next
	}
	return items[0].n
}

// cached reports whether a read of path would be served from the upper.
func (t *Tree) cached(path string) bool {
	loc, ok := t.where[path]
	if !ok {
		return false
	}
	return loc.leaf.state[loc.idx].stored
}

// Read records that the consumer read n bytes of path through the LIVE mount,
// and returns whatever that makes worth fetching.
//
// Called only for a read that actually reached the network, which on a share
// with a cache is exactly a miss. So this needs no separate hit accounting.
//
// The returned entries are the caller's to send as one batch, in one request.
// They are already marked stored: the batch is not allowed to fail silently
// and be forgotten, and a failed send is a reason to rebuild rather than to
// unpick this.
func (t *Tree) Read(path string, n int64) []Entry {
	loc, ok := t.where[path]
	if !ok {
		// A large file, an excluded path, or one the walk has not reached.
		// Served live, and nothing to learn from it.
		return nil
	}
	leaf, i := loc.leaf, loc.idx
	st := &leaf.state[i]
	if st.stored {
		// A stored file's reads never reach the network. The caller should
		// not have reported this; harmless, and not evidence.
		return nil
	}

	size := leaf.entries[i].Size
	before := min(st.read, size)
	st.read = min(st.read+n, size)
	if st.read > before {
		leaf.addRead(st.read - before)
	}

	// Every ancestor is asked, not just those up to the first refusal. A
	// node can be under its bar because of one unfetched child while its
	// parent is over it thanks to the other subtree.
	//
	// The cap bounds what ONE miss triggers in total. A leaf, its parent and
	// its grandparent can each be under it alone and over it together, so
	// the climb stops once the batch so far plus the next node's remainder
	// would exceed it; the rest waits for the next miss.
	var (
		out   []Entry
		total int64
		limit = t.cap()
	)
	for at := leaf; at != nil; at = at.parent {
		if at.unfetched() == 0 {
			continue
		}
		if total+at.unfetched() > limit {
			break
		}
		if !t.promote(at) {
			continue
		}
		sent := t.fetch(at)
		var bytes int64
		for _, e := range sent {
			bytes += e.Size
		}
		total += bytes
		if t.onFetch != nil {
			t.onFetch(at, bytes)
		}
		out = append(out, sent...)
	}
	return out
}

// promote reports whether a node has earned being fetched whole.
func (t *Tree) promote(n *node) bool {
	bdp := t.bdp()

	if !n.leaf() {
		// Fetched bytes are the only evidence that can exist above a leaf.
		// The bar is NodePromote of the node, and it is above half so that
		// with two children the second one has to have earned something
		// itself. Byte fraction also refuses a node whose remainder is a
		// few large files beside many small fetched ones, with no separate
		// test needed.
		return float64(n.fetched) >= t.layout.NodePromote*float64(n.bytes)
	}

	// The leaf rule. Evidence is what the consumer read, and the bar scales
	// with the cost of being wrong: fetching is worth it when the round trips
	// saved outweigh the bytes added,
	//
	//	unfetched_files * RTT >= unfetched_bytes / bandwidth
	//
	// which rearranges to mean <= BDP, so mean/BDP is the fraction required.
	// Every file here is under LargeFile, so it is at most 1 and never
	// "never". And there has to be something left to bring.
	if !n.unread() {
		return false
	}
	mean := float64(n.unfetched()) / float64(n.unfetchedFiles())
	f := mean / float64(bdp)
	if f < t.layout.FMin {
		f = t.layout.FMin
	}
	if f > 1 {
		f = 1
	}
	return float64(n.read) >= f*float64(n.unfetched())
}

// cap is the most one miss may trigger: a few round trips' worth of bytes on
// the current link. This is the depth selection: the climb stops at the level
// the link justifies.
func (t *Tree) cap() int64 { return int64(t.layout.MaxFetchBDP * float64(t.bdp())) }

// bdp is one round trip's worth of bytes on the current link, or the leaf
// size when the link has not been measured. Read on every promotion rather
// than held, so a link that changes is followed without rebuilding anything.
func (t *Tree) bdp() int64 {
	if b := t.layout.Link.BDP(); b > 0 {
		return b
	}
	return t.layout.LeafBytes
}

// fetch marks everything unstored under n as stored and returns what has to
// be sent, including files the consumer already read: that double-send is at
// most FMin of a leaf, and it is what makes the upper complete.
func (t *Tree) fetch(n *node) []Entry {
	var out []Entry
	stack := []*node{n}
	for len(stack) > 0 {
		at := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if !at.leaf() {
			stack = append(stack, at.kids...)
			continue
		}
		for i := range at.entries {
			st := &at.state[i]
			if st.stored {
				continue
			}
			size := at.entries[i].Size
			st.stored = true
			at.addRead(-min(st.read, size))
			st.read = 0
			at.addFetched(size, 1)
			out = append(out, at.entries[i])
		}
	}
	return out
}

func (n *node) addRead(delta int64) {
	for at := n; at != nil; at = at.parent {
		at.read += delta
	}
}

func (n *node) addFetched(bytes int64, files int) {
	for at := n; at != nil; at = at.parent {
		at.fetched += bytes
		at.fetchedFiles += files
	}
}

// Stored is how much the tree has marked stored, in bytes and files.
func (t *Tree) Stored() (bytes int64, files int) {
	if t.root == nil {
		return 0, 0
	}
	return t.root.fetched, t.root.fetchedFiles
}

func (t *Tree) unstoredCount() int {
	if t.root == nil {
		return 0
	}
	return t.root.unfetchedFiles()
}

// Unstored takes the smallest unstored files up to the limits, marks them
// stored, and returns them for sending. The walk's tail: smallest first,
// because the win is a round trip saved per file and the cheapest files buy
// the most of them, over files the reads have not reached, which is what is
// left after demand has had its say.
func (t *Tree) Unstored(maxBytes int64, maxFiles int) []Entry {
	if t.root == nil || maxFiles <= 0 || maxBytes <= 0 {
		return nil
	}
	type cand struct {
		leaf *node
		idx  int
	}
	var cands []cand
	var stack []*node
	stack = append(stack, t.root)
	for len(stack) > 0 {
		at := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if !at.leaf() {
			stack = append(stack, at.kids...)
			continue
		}
		for i := range at.entries {
			if !at.state[i].stored {
				cands = append(cands, cand{at, i})
			}
		}
	}
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i].leaf.entries[cands[i].idx], cands[j].leaf.entries[cands[j].idx]
		if a.Size != b.Size {
			return a.Size < b.Size
		}
		return a.Path < b.Path
	})

	var out []Entry
	var size int64
	for _, cd := range cands {
		e := cd.leaf.entries[cd.idx]
		if len(out) >= maxFiles || (len(out) > 0 && size+e.Size > maxBytes) {
			break
		}
		st := &cd.leaf.state[cd.idx]
		st.stored = true
		cd.leaf.addRead(-min(st.read, e.Size))
		st.read = 0
		cd.leaf.addFetched(e.Size, 1)
		out = append(out, e)
		size += e.Size
	}
	return out
}
