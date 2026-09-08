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
func wedged() (*prober, chan struct{}) {
	block := make(chan struct{})
	return &prober{mounted: func(string) bool {
		<-block
		return true
	}}, block
}

// ask puts one bounded question to the prober and insists it went unanswered.
func ask(t *testing.T, p *prober) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	if err := p.alive(ctx, wedgedSpec()); err == nil {
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

// The bound must not outlive the wedge: a prober that has seen a mount fail to
// answer reports it serving again as soon as the Lstat returns, rather than
// keeping the in-flight probe and its verdict.
//
// It does not distinguish joining that probe's answer from starting a fresh
// one, which finish's ordering decides and nothing here observes.
func TestAliveProbesAgainOnceTheLstatReturns(t *testing.T) {
	p, block := wedged()

	ask(t, p)
	close(block)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := p.alive(ctx, wedgedSpec()); err != nil {
		t.Errorf("the mount answered and the prober did not ask again: %v", err)
	}
}
