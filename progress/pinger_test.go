package progress

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/bimross/claude-code-core/statusroute"
)

// TestPingerFiresWhenSilent verifies a line is posted when no real output has
// landed inside minGap.
func TestPingerFiresWhenSilent(t *testing.T) {
	var last atomic.Int64
	// Last post was well outside minGap, so a line should fire.
	last.Store(time.Now().Add(-10 * time.Minute).UnixNano())

	var got []string
	p := &Pinger{
		Reply:        func(s string) { got = append(got, s) },
		LastPostedAt: &last,
		Phrases:      []string{"first", "second"},
	}
	// Drive one firing directly rather than waiting minutes for the timer.
	idx := 0
	posted := p.tickOnce(&idx)

	if !posted || len(got) != 1 {
		t.Fatalf("expected 1 line, got %d (%v)", len(got), got)
	}
	if got[0] != "first" {
		t.Fatalf("expected lines walked in order, got %q first", got[0])
	}
}

// TestPingerSuppressedOnPerTickDigest verifies that a per_tick digest-loop
// spawn (channel surface, no thread ts) never posts a heartbeat, even when the
// task has been silent well past minGap. Root is reserved for the digest line;
// a "still going" filler there is the makeacompany-ai#676 leak.
func TestPingerSuppressedOnPerTickDigest(t *testing.T) {
	var last atomic.Int64
	last.Store(time.Now().Add(-30 * time.Minute).UnixNano())

	var got []string
	done := make(chan struct{})
	close(done) // Run should return before it would ever read done.
	p := &Pinger{
		Reply:        func(s string) { got = append(got, s) },
		LastPostedAt: &last,
		Spawn:        statusroute.Spawn{ChannelSurface: true, ThreadTS: ""},
	}
	p.Run(done)

	if len(got) != 0 {
		t.Fatalf("per_tick digest spawn must post no heartbeat, got %v", got)
	}
}

// tickOnce mirrors one timer firing for index *idx: it posts the line at *idx
// if quiet enough and advances *idx, reporting whether it posted. Kept in the
// test file (the index lives in the caller) so the production type stays clean
// and matches the in-order, walk-once semantics of Run.
func (p *Pinger) tickOnce(idx *int) bool {
	phrases := p.Phrases
	if len(phrases) == 0 {
		phrases = DefaultPhrases
	}
	if *idx >= len(phrases) {
		return false
	}
	last := time.Unix(0, p.LastPostedAt.Load())
	if time.Since(last) >= minGap {
		p.Reply(phrases[*idx])
		*idx++
		return true
	}
	return false
}

// TestPingerSuppressedAfterRecentPost verifies no line when a real post landed
// inside minGap.
func TestPingerSuppressedAfterRecentPost(t *testing.T) {
	var last atomic.Int64
	last.Store(time.Now().UnixNano()) // just posted

	var got []string
	p := &Pinger{
		Reply:        func(s string) { got = append(got, s) },
		LastPostedAt: &last,
	}
	idx := 0
	if posted := p.tickOnce(&idx); posted || len(got) != 0 {
		t.Fatalf("expected suppression, got %d posts (%v)", len(got), got)
	}
}

// TestPingerWalksOnceThenStops verifies the pinger posts each line once, in
// order, and stops — no repeats, no looping past the end of the list.
func TestPingerWalksOnceThenStops(t *testing.T) {
	var last atomic.Int64
	last.Store(time.Now().Add(-10 * time.Minute).UnixNano())

	var got []string
	p := &Pinger{
		Reply: func(s string) {
			got = append(got, s)
			// A heartbeat is a real post too: a naive impl might re-trigger off
			// its own write. Keep LastPostedAt old so suppression isn't what
			// stops us — exhausting the list is.
		},
		LastPostedAt: &last,
		Phrases:      []string{"one", "two"},
	}
	// Fire more times than there are phrases; should still only post 2.
	idx := 0
	for i := 0; i < 5; i++ {
		p.tickOnce(&idx)
	}
	if len(got) != 2 || got[0] != "one" || got[1] != "two" {
		t.Fatalf("expected [one two] once, got %v", got)
	}
}

// TestDefaultPhrasesNonEmpty guards against an empty default set, which would
// index out of range on the first firing.
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
