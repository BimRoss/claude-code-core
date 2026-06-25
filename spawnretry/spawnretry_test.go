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
		NonZeroExit:     true,
		Duration:        1500 * time.Millisecond, // the observed ~1.5s fast-fail
		PostedToChannel: false,
		SessionLockRace: false,
		Aborted:         false,
		HasSecondSlot:   true,
	}
}

func TestShouldRetry_HappyPath(t *testing.T) {
	if !base().ShouldRetry() {
		t.Fatal("a fast, output-free, non-zero exit on a 2-slot pool should retry")
	}
}

func TestShouldRetry_Blockers(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Decision)
	}{
		{"already retried", func(d *Decision) { d.Attempt = 1 }},
		{"clean exit", func(d *Decision) { d.NonZeroExit = false }},
		{"aborted (timeout/stop/drain)", func(d *Decision) { d.Aborted = true }},
		{"session lock race", func(d *Decision) { d.SessionLockRace = true }},
		{"already posted to channel", func(d *Decision) { d.PostedToChannel = true }},
		{"no second slot", func(d *Decision) { d.HasSecondSlot = false }},
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

func TestShouldRetry_MaxAttemptsConstant(t *testing.T) {
	// Guards the invariant the policy depends on: exactly one retry.
	if MaxAttempts != 2 {
		t.Fatalf("MaxAttempts = %d, want 2 (one original + one retry)", MaxAttempts)
	}
}
