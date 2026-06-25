package spawnretry

import (
	"testing"
	"time"
)

// base is a Decision that, on its own, SHOULD retry. Each test flips one field
// to assert that field independently blocks (or, for the happy cases, allows)
// the retry.
func base() Decision {
	return Decision{
		Attempt:         0,
		MaxAttempts:     2,
		NonZeroExit:     true,
		Duration:        1500 * time.Millisecond, // the observed ~1.5s fast-fail
		PostedToChannel: false,
		SessionLockRace: false,
		Aborted:         false,
		HasUntriedSlot:  true,
	}
}

func TestShouldRetry_HappyPath(t *testing.T) {
	if !base().ShouldRetry() {
		t.Fatal("a fast, output-free, non-zero exit with an untried slot should retry")
	}
}

func TestShouldRetry_Blockers(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Decision)
	}{
		{"spent all attempts", func(d *Decision) { d.Attempt = 1 }},
		{"clean exit", func(d *Decision) { d.NonZeroExit = false }},
		{"aborted (timeout/stop/drain)", func(d *Decision) { d.Aborted = true }},
		{"session lock race", func(d *Decision) { d.SessionLockRace = true }},
		{"already posted to channel", func(d *Decision) { d.PostedToChannel = true }},
		{"no untried slot", func(d *Decision) { d.HasUntriedSlot = false }},
		{"slow failure", func(d *Decision) { d.Duration = 30 * time.Second }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := base()
			c.mutate(&d)
			if d.ShouldRetry() {
				t.Errorf("%s should block the retry, but ShouldRetry returned true", c.name)
			}
		})
	}
}

func TestShouldRetry_IteratesNSlots(t *testing.T) {
	// A 4-slot pool: attempts 0,1,2 may retry (untried slots remain); attempt 3
	// is the last slot, so it must not retry.
	d := base()
	d.MaxAttempts = 4
	for attempt := 0; attempt < 3; attempt++ {
		d.Attempt = attempt
		if !d.ShouldRetry() {
			t.Errorf("attempt %d of a 4-slot pool should still retry", attempt)
		}
	}
	d.Attempt = 3 // last slot tried
	if d.ShouldRetry() {
		t.Error("attempt 3 (final slot) of a 4-slot pool must not retry")
	}
}

func TestShouldRetry_DefaultMaxAttemptsIsOneRetry(t *testing.T) {
	// MaxAttempts unset (0) falls back to DefaultMaxAttempts: one retry only.
	d := base()
	d.MaxAttempts = 0
	d.Attempt = 0
	if !d.ShouldRetry() {
		t.Fatal("attempt 0 with default cap should retry once")
	}
	d.Attempt = 1
	if d.ShouldRetry() {
		t.Fatal("attempt 1 with default cap (2) must not retry")
	}
}

func TestShouldRetry_BoundaryDuration(t *testing.T) {
	d := base()
	d.Duration = DefaultFastFailWindow // exactly at the window: still eligible
	if !d.ShouldRetry() {
		t.Errorf("duration == window (%s) should still be eligible", DefaultFastFailWindow)
	}
	d.Duration = DefaultFastFailWindow + time.Nanosecond // just over: blocked
	if d.ShouldRetry() {
		t.Errorf("duration just over window should block")
	}
}

func TestShouldRetry_CustomWindow(t *testing.T) {
	d := base()
	d.FastFailWindow = 3 * time.Second
	d.Duration = 5 * time.Second // over the custom window, under the default
	if d.ShouldRetry() {
		t.Errorf("5s should exceed a 3s custom window and block")
	}
	d.Duration = 2 * time.Second
	if !d.ShouldRetry() {
		t.Errorf("2s is under the 3s custom window and should retry")
	}
}
