package dircache

// The prefetch sender: demand batches from the tree first, the walk's
// smallest leftovers when the consumer is quiet, or (eager) back to back.
// tree.go is the policy; docs/caching.md is why.

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// Policy is whether a share attached with prefetch is filled, and how.
type Policy string

const (
	// PolicyOff attaches the share for invalidation and write-back only.
	// The default: landing a batch costs more round trips than reading the
	// file until the apply pipelines (ADR 0045).
	PolicyOff Policy = "off"

	// PolicyEager sends the whole tree smallest first, up to the budget,
	// as fast as the link allows. Reads are ignored.
	PolicyEager Policy = "eager"

	// PolicyTree sends what is read and its neighbourhood, doubling as the
	// evidence grows, and walks the rest only while nothing is being read.
	PolicyTree Policy = "tree"
)

// ParsePolicy reads a policy word. Empty is off.
func ParsePolicy(s string) (Policy, error) {
	switch Policy(s) {
	case "", PolicyOff:
		return PolicyOff, nil
	case PolicyEager, PolicyTree:
		return Policy(s), nil
	default:
		return "", fmt.Errorf("%q is not a prefetch policy; want %s, %s or %s", s, PolicyOff, PolicyEager, PolicyTree)
	}
}

const (
	// quietFor is how long reads must have stopped before the walk sends
	// anything. The consumer's reads are the point; the walk is for when it
	// is not reading.
	quietFor = 2 * time.Second

	// walkBatch bounds what the walk sends at once. Small on purpose: the
	// channel is one stream, and a demand batch queued behind a walk batch
	// waits for the whole of it. 16 MiB is about 13s on a 10 Mbit link,
	// arriving exactly when the demand batch matters most.
	walkBatch = 1 << 20

	// walkEvery is how often the walk looks for a gap to fill.
	walkEvery = 500 * time.Millisecond

	// bandwidthFloor is the smallest batch a bandwidth estimate is taken
	// from. A batch under it is spent mostly on the round trip, so timing
	// it says nothing about the link's throughput.
	bandwidthFloor = 256 << 10
)

// prefetch is one share's tree and the sender behind it.
type prefetch struct {
	share, root string
	state       *shareState

	mu       sync.Mutex
	tree     *Tree
	queue    [][]Entry // demand batches, in order
	lastRead time.Time // when a read last arrived, for the walk to yield to
	walked   bool      // the walk has yielded everything it will

	kick chan struct{} // the sender is woken, never blocked
}

// Touch records that the consumer read n bytes of path through the live
// mount, and queues whatever the tree decides for the sender. Called on the
// NFS server's own path for every read, so it never waits on the link; under
// any policy but the tree it costs one comparison.
func (c *Cache) Touch(share, path string, n int64) {
	if c.Policy != PolicyTree {
		return
	}
	p := c.prefetchFor(share)
	if p == nil {
		return
	}
	// The observer spells a path from the share root with a leading slash,
	// as the protocol does; the walk spells an Entry without one, as a tar
	// does. One key, or every read misses the tree.
	path = strings.TrimPrefix(path, "/")
	p.mu.Lock()
	p.lastRead = time.Now()
	got := p.tree.Read(path, n)
	if len(got) > 0 {
		p.queue = append(p.queue, got)
	}
	p.mu.Unlock()
	if len(got) > 0 {
		p.wake()
	}
}

func (p *prefetch) wake() {
	select {
	case p.kick <- struct{}{}:
	default:
	}
}

// prefetchFor is the share's prefetch, or nil for a share without one.
func (c *Cache) prefetchFor(share string) *prefetch {
	c.shares.mu.Lock()
	defer c.shares.mu.Unlock()
	return c.shares.prefetch[share]
}

// startTree is Attach for the tree policy: the walk feeds the tree as it
// goes, the sender drains demand first and the walk's leftovers when the
// consumer is quiet.
func (c *Cache) startTree(share, root string, state *shareState) {
	tree, err := NewTree(nil, Layout{Link: c.link()})
	if err != nil {
		// A Layout nobody could serve is a programming error, and the share
		// still works: served live, as any share is before its prefetch.
		c.log().Error("the prefetch layout is not usable; the share is served live", "share", share, "err", err)
		c.shares.finish(state, false, err)
		return
	}
	p := &prefetch{share: share, root: root, state: state, tree: tree, kick: make(chan struct{}, 1)}
	c.shares.mu.Lock()
	if c.shares.prefetch == nil {
		c.shares.prefetch = map[string]*prefetch{}
	}
	c.shares.prefetch[share] = p
	c.shares.mu.Unlock()

	go c.send(p)
	go c.feedTree(p)
}

// feedTree walks the share's root into the tree in batches. The tree is
// correct at every moment and merely knows less until the walk is done.
func (c *Cache) feedTree(p *prefetch) {
	var pending []Entry
	flush := func() {
		if len(pending) == 0 {
			return
		}
		p.mu.Lock()
		p.tree.Add(pending)
		p.mu.Unlock()
		pending = nil
	}
	stats := walk(p.root, c.Exclude, func(e Entry) {
		pending = append(pending, e)
		if len(pending) >= maxBatchFiles {
			flush()
		}
	})
	flush()

	p.mu.Lock()
	p.walked = true
	p.mu.Unlock()
	c.shares.mu.Lock()
	p.state.Stats = stats
	c.shares.mu.Unlock()
	p.wake()
}

// send is the share's one sender: demand batches first, in order, and the
// walk's smallest leftovers when nothing has been read for a while.
func (c *Cache) send(p *prefetch) {
	ticker := time.NewTicker(walkEvery)
	defer ticker.Stop()

	for {
		select {
		case <-c.Ctx.Done():
			return
		case <-p.kick:
		case <-ticker.C:
		}

		for {
			batch, demand := c.next(p)
			if len(batch) == 0 {
				break
			}
			if err := c.sendBatch(p.share, p.root, batch, p.state); err != nil {
				// Said once. The share is served live meanwhile; the tree
				// still says these are stored, and the next connection
				// rebuilds it (the record of what was sent is per prefetch).
				c.quiet(c.Ctx, "a share's prefetch could not be sent", "share", p.share, "demand", demand, "err", err)
				return
			}
			// The link as now measured, so the next decision uses it. The
			// tree is not rebuilt for this and forgets nothing.
			p.mu.Lock()
			p.tree.SetLink(c.link())
			p.mu.Unlock()
		}
		c.finishIfWalked(p)
	}
}

// next is what to send now: a queued demand batch, else, if the consumer
// has been quiet and the budget allows, the walk's smallest unstored files.
func (c *Cache) next(p *prefetch) (batch []Entry, demand bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.queue) > 0 {
		batch, p.queue = p.queue[0], p.queue[1:]
		return batch, true
	}
	// Eager has no demand batch to stay out of the way of: it neither
	// waits for quiet nor keeps its batches small.
	eager := c.Policy == PolicyEager
	if !eager && time.Since(p.lastRead) < quietFor {
		return nil, false
	}
	stored, files := p.tree.Stored()
	if c.overBudget(stored, files) {
		return nil, false
	}
	limit := int64(walkBatch)
	if eager {
		limit = batchBytes
	}
	return p.tree.Unstored(min(limit, c.Budget.bytes()-stored), c.Budget.files()-files), false
}

// overBudget reports whether a share holding this much may take no more.
func (c *Cache) overBudget(bytes int64, files int) bool {
	return bytes >= c.Budget.bytes() || files >= c.Budget.files()
}

// finishIfWalked marks the share done once the walk is over and nothing is
// left that the budget allows, which is what status reports on.
func (c *Cache) finishIfWalked(p *prefetch) {
	p.mu.Lock()
	walked := p.walked
	stored, files := p.tree.Stored()
	left := p.tree.unstoredCount()
	p.mu.Unlock()
	if !walked {
		return
	}
	if left > 0 && !c.overBudget(stored, files) {
		return
	}
	// What the tree holds is what will be cached; status reports it against
	// the walk's total. Settled before Done is visible, so a reader that
	// polls for Done never sees a finished share with no figures.
	c.shares.mu.Lock()
	if !p.state.Done {
		p.state.Stats.Files, p.state.Stats.Bytes = files, stored
	}
	c.shares.mu.Unlock()
	// Recorded once, when the prefetch finishes; the sender calls this on
	// every tick afterwards, and the record is a file rewritten per call.
	if c.shares.finish(p.state, left == 0, nil) && c.Record != nil {
		c.Record.Record(p.share, c.shares.paths(p.share))
	}
}

// link is what the policy decides against: the round trip from the caller,
// who has a connection to time, and the bandwidth from this cache's own
// batches, which are the only transfers large enough to measure it on.
func (c *Cache) link() Link {
	var l Link
	if c.Link != nil {
		l = c.Link()
	}
	l.Bandwidth = c.bw.Load()
	return l
}

// observeBandwidth folds one batch of at least bandwidthFloor into a moving
// average that leans on the latest, so a link that changes mid-session is
// followed within a few batches without one odd batch swinging it.
func (c *Cache) observeBandwidth(bytes int64, took time.Duration) {
	if took <= 0 || bytes < bandwidthFloor {
		return
	}
	rate := int64(float64(bytes) / took.Seconds())
	if have := c.bw.Load(); have != 0 {
		rate = (have*3 + rate) / 4
	}
	c.bw.Store(rate)
}
