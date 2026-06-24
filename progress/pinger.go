// Package progress handles the "still working" beat while a long task runs.
//
// The old behavior posted a rotating reassurance line every 3-7 minutes for as
// long as the task ran, so a job that took 25 minutes produced five or six
// near-identical "still going" posts. Grant flagged that on 2026-06-24: those
// lines are noise dressed up as signal. Each one only reminds the operator
// they're still waiting, because none of them carry information the operator
// couldn't already guess.
//
// New behavior: promise once, then go quiet. The pinger walks an ordered, finite
// list of lines exactly once and stops — one expectation-setting line early, one
// humane backstop much later for genuinely long jobs, then silence. Real
// progress is meant to come from the milestone stream (a moving "N of M" count,
// a state change like "opened the PR"), not from this timer. A timer can't know
// a number; all it can honestly say is "still here," and it only needs to say
// that once.
//
// Shared by Ross and Joanne so both agents behave the same when a task runs
// long. Promoted to core per the 2026-06-17 consolidation audit
// (claude-code-joanne#276); both import this package.
package progress

import (
	"math/rand"
	"sync/atomic"
	"time"
)

// DefaultPhrases are posted in order, once each, then the pinger goes silent.
// Keep this list short and each line honest: the first sets the expectation and
// invites the operator to walk away; the last is a single backstop for a job
// that runs unusually long. No reassurance vocabulary, no rule-of-three, no
// em-dash (matches the channel writing rules). Adding a third line just means
// one more (later) post before silence — it does not loop.
var DefaultPhrases = []string{
	"Still going. No need to sit on this, I'll ping you the moment it lands.",
	"Still on it. Bigger than it first looked, but I've got it. I'll holler the second it's done.",
}

const (
	// minGap is the hard floor between any two user-facing posts. A real
	// milestone post (tracked via LastPostedAt) inside this window suppresses
	// the pending line so a heartbeat never lands right on top of real output.
	// When suppressed, the line is held, not consumed — it can still fire later.
	minGap = 3 * time.Minute
	// firstBase / firstWindow set the opening line at ~4-6 min: firstBase plus a
	// jitter of up to firstWindow.
	firstBase   = 4 * time.Minute
	firstWindow = 2 * time.Minute
	// backstopBase / backstopWindow space every line after the first at ~10-14
	// min. Wide on purpose: after the opening promise, silence reads as
	// confidence, so the backstop is a rare safety line, not a stream.
	backstopBase   = 10 * time.Minute
	backstopWindow = 4 * time.Minute
)

// Pinger emits a small, finite set of "still working" lines while the operator
// hasn't heard real output. It walks Phrases in order, one line per firing,
// honoring a minGap floor against LastPostedAt, and stops once the list is
// exhausted. Because any real post (milestone or final answer) updates
// LastPostedAt, a busy task that streams genuine milestones will keep
// suppressing these lines, so they only surface when the agent has truly gone
// quiet.
type Pinger struct {
	// Reply posts a line to the operator. Required.
	Reply func(string)
	// LastPostedAt holds the unix-nano timestamp of the most recent
	// user-facing post (heartbeat or real output). Required: the pinger reads
	// it to honor minGap.
	LastPostedAt *atomic.Int64
	// Phrases overrides DefaultPhrases when non-empty. Walked in order, once.
	Phrases []string
}

// Run blocks until done is closed, emitting at most len(Phrases) lines on the
// schedule above. Spawn it in its own goroutine and close done when the task
// finishes:
//
//	done := make(chan struct{})
//	go (&progress.Pinger{Reply: reply, LastPostedAt: &lastPostedAt}).Run(done)
//	defer close(done)
func (p *Pinger) Run(done <-chan struct{}) {
	phrases := p.Phrases
	if len(phrases) == 0 {
		phrases = DefaultPhrases
	}
	timer := time.NewTimer(firstBase + jitter(firstWindow))
	defer timer.Stop()
	idx := 0
	for {
		select {
		case <-done:
			return
		case now := <-timer.C:
			last := time.Unix(0, p.LastPostedAt.Load())
			if now.Sub(last) >= minGap {
				// Quiet long enough — post the next line and advance. If a real
				// milestone landed inside minGap instead, we hold this line (no
				// advance) and re-check after the next interval.
				p.Reply(phrases[idx])
				idx++
				if idx >= len(phrases) {
					// All lines spent. Go silent for the rest of the task; the
					// milestone stream and the final answer carry it from here.
					return
				}
			}
			timer.Reset(backstopBase + jitter(backstopWindow))
		}
	}
}

// jitter returns a random duration in [0, window) so consecutive pings never
// land on a robotic fixed cadence. window must be > 0.
func jitter(window time.Duration) time.Duration {
	return time.Duration(rand.Intn(int(window)))
}
