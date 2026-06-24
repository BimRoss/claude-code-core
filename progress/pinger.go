// Package progress posts human-sounding "still working" heartbeats while a
// long task runs, so the operator hears a varied, on-voice line instead of
// silence or a robotic "still working (Xs elapsed)" template.
//
// Shared by Ross and Joanne so both agents sound the same when a task runs
// long. Promoted to core per the 2026-06-17 consolidation audit, which found
// the heartbeat logic had diverged: Ross carried this jittered, rotating-phrase
// version while Joanne still emitted the static elapsed-time line
// (claude-code-joanne#276). Both now import this package.
package progress

import (
	"math/rand"
	"sync/atomic"
	"time"
)

// DefaultPhrases are rotated on each heartbeat; the pinger avoids repeating the
// line it used last so consecutive pings never read identically.
var DefaultPhrases = []string{
	"Still on it — hang tight.",
	"Woof. Still at it, but making progress.",
	"Still going. This one wants some time.",
	"On it. Just working through a few moving pieces.",
	"Yep, still here. Will holler when it's done.",
	"Making progress — almost through the hard part.",
	"Still crunching. More involved than I expected, but I've got it.",
	"Still here, still working. Won't be much longer.",
	"Plugging away. Should have something soon.",
}

const (
	// minGap is the hard floor between any two user-facing posts. A real
	// milestone post (tracked via LastPostedAt) inside this window suppresses
	// the next heartbeat so pings never pile on top of actual output.
	minGap = 3 * time.Minute
	// firstWindow / firstBase set the first ping at ~5 min (firstBase plus a
	// jitter of up to firstWindow): 4–6 min.
	firstBase   = 4 * time.Minute
	firstWindow = 2 * time.Minute
	// nextBase / nextWindow set each subsequent ping at ~5 min ± 2 min: 3–7 min.
	nextBase   = 3 * time.Minute
	nextWindow = 4 * time.Minute
)

// Pinger emits a varied progress line when the operator hasn't heard from the
// agent in a while. First ping fires at ~5 min (±1 min jitter); subsequent
// pings vary 3–7 min, with a hard 3-min floor between any two posts. Because it
// reads LastPostedAt, any real milestone post resets the clock, so heartbeats
// never crowd actual output.
type Pinger struct {
	// Reply posts a line to the operator. Required.
	Reply func(string)
	// LastPostedAt holds the unix-nano timestamp of the most recent
	// user-facing post (heartbeat or real output). Required: the pinger reads
	// it to honor minGap.
	LastPostedAt *atomic.Int64
	// Phrases overrides DefaultPhrases when non-empty.
	Phrases []string
}

// Run blocks until done is closed, emitting heartbeats on the schedule above.
// Spawn it in its own goroutine and close done when the task finishes:
//
//	done := make(chan struct{})
//	go (&progress.Pinger{Reply: reply, LastPostedAt: &lastPostedAt}).Run(done)
//	defer close(done)
func (p *Pinger) Run(done <-chan struct{}) {
	phrases := p.Phrases
	if len(phrases) == 0 {
		phrases = DefaultPhrases
	}
	timer := time.NewTimer(firstBase + time.Duration(rand.Intn(int(firstWindow))))
	defer timer.Stop()
	lastPhrase := -1
	for {
		select {
		case <-done:
			return
		case now := <-timer.C:
			last := time.Unix(0, p.LastPostedAt.Load())
			if now.Sub(last) >= minGap {
				idx := rand.Intn(len(phrases))
				if idx == lastPhrase {
					idx = (idx + 1) % len(phrases)
				}
				lastPhrase = idx
				p.Reply(phrases[idx])
			}
			timer.Reset(nextBase + time.Duration(rand.Intn(int(nextWindow))))
		}
	}
}
