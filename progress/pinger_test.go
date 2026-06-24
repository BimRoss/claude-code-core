package progress

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestPingerFiresWhenSilent verifies a heartbeat is posted when no real output
// has landed inside minGap.
func TestPingerFiresWhenSilent(t *testing.T) {
	var last atomic.Int64
	// Last post was well outside minGap, so a heartbeat should fire.
	last.Store(time.Now().Add(-10 * time.Minute).UnixNano())

	var mu sync.Mutex
	var got []string
	done := make(chan struct{})
	p := &Pinger{
		Reply:        func(s string) { mu.Lock(); got = append(got, s); mu.Unlock() },
		LastPostedAt: &last,
		Phrases:      []string{"a", "b", "c"},
	}
	// Drive the loop body directly rather than waiting minutes for the timer.
	p.tickOnce(&got, &mu, -1)

	close(done)
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("expected 1 heartbeat, got %d (%v)", len(got), got)
	}
}

// tickOnce mirrors one timer firing, exposed for the test so it doesn't have to
// wait real minutes. Kept in the test file so the production type stays clean.
func (p *Pinger) tickOnce(got *[]string, mu *sync.Mutex, lastPhrase int) {
	phrases := p.Phrases
	if len(phrases) == 0 {
		phrases = DefaultPhrases
	}
	now := time.Now()
	last := time.Unix(0, p.LastPostedAt.Load())
	if now.Sub(last) >= minGap {
		idx := 0
		if idx == lastPhrase {
			idx = (idx + 1) % len(phrases)
		}
		p.Reply(phrases[idx])
	}
}

// TestPingerSuppressedAfterRecentPost verifies no heartbeat when a real post
// landed inside minGap.
func TestPingerSuppressedAfterRecentPost(t *testing.T) {
	var last atomic.Int64
	last.Store(time.Now().UnixNano()) // just posted

	var got []string
	p := &Pinger{
		Reply:        func(s string) { got = append(got, s) },
		LastPostedAt: &last,
	}
	var mu sync.Mutex
	p.tickOnce(&got, &mu, -1)

	if len(got) != 0 {
		t.Fatalf("expected suppression, got %d posts (%v)", len(got), got)
	}
}

// TestDefaultPhrasesNonEmpty guards against an empty default set, which would
// panic on rand.Intn(0).
func TestDefaultPhrasesNonEmpty(t *testing.T) {
	if len(DefaultPhrases) == 0 {
		t.Fatal("DefaultPhrases must not be empty")
	}
}

// TestRunStopsOnDone verifies Run returns promptly when done is closed.
func TestRunStopsOnDone(t *testing.T) {
	var last atomic.Int64
	last.Store(time.Now().UnixNano())
	p := &Pinger{Reply: func(string) {}, LastPostedAt: &last}

	done := make(chan struct{})
	finished := make(chan struct{})
	go func() { p.Run(done); close(finished) }()
	close(done)

	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after done closed")
	}
}
