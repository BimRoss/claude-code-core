// Package spawnretry holds the shared policy for retrying a failed `claude`
// spawn on a DIFFERENT OAuth-pool slot.
//
// Motivation: the pooled Claude Max tokens each have their own rolling window
// and account state. A per-account soft-throttle (or other transient upstream
// hiccup) on ONE token surfaces as a fast, non-zero exit whose error the CLI
// writes to stdout — not a clean 429. On 2026-06-25 this took the form of
// bursty `exit status 1` / `stderr=""` failures weighted heavily to one pool
// slot while the others stayed healthy. The right reflex is to immediately
// re-run the same prompt on another slot rather than surfacing a "please
// re-send".
//
// The pool is N slots, not two (see core/oauthpool). This policy therefore
// allows retrying across EVERY remaining untried slot — the caller drives slot
// selection via oauthpool.Pool.Others and reports whether any untried slot is
// left (HasUntriedSlot). Termination is guaranteed: the pool is finite and each
// retry consumes one slot; MaxAttempts is a defensive backstop on top of that.
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

// DefaultMaxAttempts is the backstop total-attempt cap used when a Decision
// leaves MaxAttempts unset (0). With a single alternate slot it reproduces the
// original "one original + one retry" behavior; callers with a larger pool pass
// MaxAttempts = pool size to allow iterating every slot once.
const DefaultMaxAttempts = 2

// Decision describes the spawn attempt that just failed. The caller fills it
// in and calls ShouldRetry to decide whether to re-run on another slot.
type Decision struct {
	// Attempt is the 0-based index of the attempt that just failed (the first
	// spawn is attempt 0).
	Attempt int
	// MaxAttempts is the total attempts allowed (original + retries). Callers
	// pass the pool size so a spawn may try each slot once. 0 means use
	// DefaultMaxAttempts (the legacy one-retry behavior).
	MaxAttempts int
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
	// HasUntriedSlot is true when at least one OTHER pool slot has not been
	// tried yet and is available (not in cooldown). With a single slot — or
	// once every slot has been tried — there is nowhere else to go. The caller
	// computes this from oauthpool.Pool.Others(tried).
	HasUntriedSlot bool
	// FastFailWindow overrides DefaultFastFailWindow when > 0.
	FastFailWindow time.Duration
}

// ShouldRetry reports whether the just-failed attempt is a safe, likely-
// transient fast-fail worth one more try on another OAuth slot.
//
// Note it does NOT consult rate-limit classification: a fast-fail that matches
// a limit pattern on one slot is precisely the per-account soft-throttle we
// want to dodge by switching slots, rather than slamming a shared rate-limit
// window across the whole pool. Genuine all-slots limits still get the
// shared-window treatment because that classification runs on the FINAL
// attempt's output after retries are exhausted.
func (d Decision) ShouldRetry() bool {
	window := d.FastFailWindow
	if window <= 0 {
		window = DefaultFastFailWindow
	}
	maxAttempts := d.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = DefaultMaxAttempts
	}
	switch {
	case d.Attempt >= maxAttempts-1:
		// Defensive backstop: spent the allotted attempts.
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
	case !d.HasUntriedSlot:
		// No other slot left to retry on.
		return false
	case d.Duration > window:
		// Slow failure: real work may have run, so re-running is unsafe.
		return false
	default:
		return true
	}
}
