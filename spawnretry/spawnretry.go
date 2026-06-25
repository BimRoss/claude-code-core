// Package spawnretry holds the shared policy for retrying a failed `claude`
// spawn on the OTHER OAuth-pool slot.
//
// Motivation: the two pooled Claude Max tokens each have their own rolling
// window and account state. A per-account soft-throttle (or other transient
// upstream hiccup) on ONE token surfaces as a fast, non-zero exit whose error
// the CLI writes to stdout — not a clean 429. On 2026-06-25 this took the form
// of bursty `exit status 1` / `stderr=""` failures weighted ~9:1 to pool slot
// 2 while slot 1 stayed healthy. The right reflex is to immediately re-run the
// same prompt on the other slot rather than surfacing a "please re-send".
//
// The policy is intentionally NARROW. A spawn is only retried when it failed
// FAST and produced no user-visible output — the signature of a failure that
// happened before the model authenticated or did any real work. Re-running a
// --resume spawn that already streamed output or ran tools would double-post
// to the channel or double-execute side effects, so those are never retried.
package spawnretry

import "time"

// DefaultFastFailWindow bounds how long a retry-eligible failure may take.
// Observed transient fast-fails exit in ~1.5s; 10s leaves generous headroom
// while staying well under any spawn that did real work.
const DefaultFastFailWindow = 10 * time.Second

// MaxAttempts is the total number of spawn attempts: the original plus one
// retry. We never retry more than once — a second failure on a fresh slot is
// no longer "one slot is having a moment" and should surface for real.
const MaxAttempts = 2

// Decision describes the spawn attempt that just failed. The caller fills it
// in and calls ShouldRetry to decide whether to re-run on the other slot.
type Decision struct {
	// Attempt is the 0-based index of the attempt that just failed (the first
	// spawn is attempt 0).
	Attempt int
	// NonZeroExit is true when the process exited non-zero (waitErr != nil).
	// A clean exit — even one carrying an is_error result — is handled by the
	// caller's normal classification path, not here.
	NonZeroExit bool
	// Duration is the wall-clock time of the failed attempt.
	Duration time.Duration
	// PostedToChannel is true if anything (a progress ping, a milestone, any
	// streamed reply) already reached the user. Retrying after that would
	// duplicate output, so it blocks the retry.
	PostedToChannel bool
	// SessionLockRace is true when the failure was classified as a stale
	// session-lockfile race. Retrying the same session would just race again.
	SessionLockRace bool
	// Aborted is true for ctx timeout, user-stop, or graceful-drain cancel —
	// each owns its own handling and must not be retried.
	Aborted bool
	// HasSecondSlot is true when an alternate OAuth token is configured to
	// retry on. With a single slot there is nowhere else to go.
	HasSecondSlot bool
	// FastFailWindow overrides DefaultFastFailWindow when > 0.
	FastFailWindow time.Duration
}

// ShouldRetry reports whether the just-failed attempt is a safe, likely-
// transient fast-fail worth exactly one more try on the other OAuth slot.
//
// Note it does NOT consult rate-limit classification: a fast-fail that matches
// a limit pattern on one slot is precisely the per-account soft-throttle we
// want to dodge by switching slots, rather than slamming a shared rate-limit
// window across both. Genuine all-slots limits still get the shared-window
// treatment because that classification runs on the FINAL attempt's output
// after retries are exhausted.
func (d Decision) ShouldRetry() bool {
	window := d.FastFailWindow
	if window <= 0 {
		window = DefaultFastFailWindow
	}
	switch {
	case d.Attempt >= MaxAttempts-1:
		// Already spent our one retry.
		return false
	case !d.NonZeroExit:
		// Clean exit isn't this failure mode.
		return false
	case d.Aborted:
		// Timeout / user-stop / drain own their handling.
		return false
	case d.SessionLockRace:
		// Same-session lock; retrying races and won't help.
		return false
	case d.PostedToChannel:
		// Retrying would duplicate what's already in the channel.
		return false
	case !d.HasSecondSlot:
		// Nowhere else to retry.
		return false
	case d.Duration > window:
		// Slow failure: real work may have run, so re-running is unsafe.
		return false
	default:
		return true
	}
}
