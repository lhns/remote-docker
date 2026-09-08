package union

import (
	"context"
	"runtime"
	"testing"
	"time"
)

func wedgedSpec() Spec {
	return Spec{Export: "/m/0011223344556677", Port: 30001, CacheDir: "/var/lib/docker/volumes/v/_data"}
}

// wedged is a prober whose Lstat never returns until the returned channel is
// closed, which is what a FUSE mount with nothing serving it does.
func wedged() (*Prober, chan struct{}) {
	block := make(chan struct{})
	return &Prober{mounted: func(string) bool {
		<-block
		return true
	}}, block
}

// ask puts one bounded question to the prober and insists it went unanswered.
func ask(t *testing.T, p *Prober) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	if err := p.Alive(ctx, wedgedSpec()); err == nil {
		t.Fatal("a wedged mount was reported as serving")
	}
}

// One in-flight Lstat per union, however often it is asked.
//
// The Lstat of a wedged merged path never returns, so its goroutine outlives
// the context that gave up on it and pins an OS thread. unions.awaitGone polls
// this every restartDelay for the whole life of an adopted mount, so a probe
// per call is a goroutine and a thread every two seconds, forever.
func TestAliveKeepsOneProbeAgainstAWedgedMount(t *testing.T) {
	p, block := wedged()
	defer close(block)

	ask(t, p)
	time.Sleep(20 * time.Millisecond)
	before := runtime.NumGoroutine()

	const polls = 20
	for range polls {
		ask(t, p)
	}
	time.Sleep(20 * time.Millisecond)

	if after := runtime.NumGoroutine(); after > before {
		t.Errorf("%d polls left %d goroutines running, up from %d", polls, after, before)
	}
}

// The bound must not outlive the wedge: once the Lstat returns, the next caller
// gets a reading of its own rather than the stale one it blocked on.
func TestAliveProbesAgainOnceTheLstatReturns(t *testing.T) {
	p, block := wedged()

	ask(t, p)
	close(block)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := p.Alive(ctx, wedgedSpec()); err != nil {
		t.Errorf("the mount answered and the prober did not ask again: %v", err)
	}
}
