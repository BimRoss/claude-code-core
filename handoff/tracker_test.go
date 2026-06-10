package handoff

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTracker(t *testing.T) *Tracker {
	t.Helper()
	dir := t.TempDir()
	return New(dir)
}

func TestAdmit_FirstHopAllowed(t *testing.T) {
	tr := newTracker(t)
	reason, err := tr.AdmitOrDrop("UJoanne", "URoss", "C123", "1.0", "1.0")
	if err != nil {
		t.Fatalf("AdmitOrDrop: %v", err)
	}
	if reason != "" {
		t.Fatalf("first hop should be admitted, got drop reason %q", reason)
	}
}

func TestAdmit_KillSwitchDrops(t *testing.T) {
	tr := newTracker(t)
	if err := tr.EngageKillSwitch(); err != nil {
		t.Fatalf("EngageKillSwitch: %v", err)
	}
	reason, err := tr.AdmitOrDrop("UJoanne", "URoss", "C123", "1.0", "1.0")
	if err != nil {
		t.Fatalf("AdmitOrDrop: %v", err)
	}
	if reason != ReasonKillSwitch {
		t.Fatalf("kill-switched call should drop with ReasonKillSwitch, got %q", reason)
	}
	if err := tr.DisengageKillSwitch(); err != nil {
		t.Fatalf("DisengageKillSwitch: %v", err)
	}
	reason, err = tr.AdmitOrDrop("UJoanne", "URoss", "C123", "1.0", "1.0")
	if err != nil {
		t.Fatalf("AdmitOrDrop after disengage: %v", err)
	}
	if reason != "" {
		t.Fatalf("after disengage, hop should admit, got %q", reason)
	}
}

func TestAdmit_HopCapDropsAfterFive(t *testing.T) {
	tr := newTracker(t)
	// Five hops alternating Joanne↔Ross so cycle-detect doesn't fire first.
	pairs := [][2]string{
		{"UJoanne", "URoss"},
		{"URoss", "UJoanne"},
		{"UJoanne", "URoss"},
		{"URoss", "UJoanne"},
		{"UJoanne", "URoss"},
	}
	for i, p := range pairs {
		reason, err := tr.AdmitOrDrop(p[0], p[1], "C1", "thread", "msg")
		if err != nil {
			t.Fatalf("hop %d AdmitOrDrop: %v", i, err)
		}
		if reason != "" {
			t.Fatalf("hop %d should admit, got drop %q", i, reason)
		}
	}
	// 6th hop alternating direction — must drop on hop cap (not cycle).
	reason, err := tr.AdmitOrDrop("URoss", "UJoanne", "C1", "thread", "msg")
	if err != nil {
		t.Fatalf("AdmitOrDrop: %v", err)
	}
	if reason != ReasonHopCap {
		t.Fatalf("6th hop should drop with ReasonHopCap, got %q", reason)
	}
}

func TestAdmit_CycleDetect(t *testing.T) {
	tr := newTracker(t)
	// J→R admitted (first hop, no prior).
	if reason, err := tr.AdmitOrDrop("UJoanne", "URoss", "C1", "thread", "m1"); err != nil || reason != "" {
		t.Fatalf("first hop: err=%v reason=%q", err, reason)
	}
	// Healthy alternation: R→J. last.To=R, new to=J. Different target, admit.
	if reason, err := tr.AdmitOrDrop("URoss", "UJoanne", "C1", "thread", "m2"); err != nil || reason != "" {
		t.Fatalf("R→J after J→R should admit; err=%v reason=%q", err, reason)
	}
	// Tight cycle: spawn R again immediately after R was spawned (target
	// repeats). last.To=J? wait — re-check: hop 2 was R→J so last.To=J.
	// New hop J→R has to=R != J, so this DOES admit (it's normal
	// alternation). For the actual cycle: we need two hops in a row with
	// the same to. Construct that:
	if reason, err := tr.AdmitOrDrop("UJoanne", "URoss", "C1", "thread", "m3"); err != nil || reason != "" {
		t.Fatalf("J→R again (healthy alternation) should admit; err=%v reason=%q", err, reason)
	}
	// Now: last hop ended at to=R. Try another hop with to=R. Cycle.
	reason, err := tr.AdmitOrDrop("UJoanne", "URoss", "C1", "thread", "m4")
	if err != nil {
		t.Fatalf("AdmitOrDrop: %v", err)
	}
	if reason != ReasonCycleDetect {
		t.Fatalf("two hops in a row with same target should cycle-detect; got %q", reason)
	}
}

func TestAdmit_GlobalDailyCap(t *testing.T) {
	tr := newTracker(t)
	// Burn through the global cap with hops alternating across many
	// distinct threads so per-thread caps don't fire first.
	for i := 0; i < MaxSpawnsPerDay; i++ {
		thread := "t" + string(rune('A'+i%26)) + "-" + string(rune('0'+i/26))
		reason, err := tr.AdmitOrDrop("UJoanne", "URoss", "C1", thread, "m")
		if err != nil {
			t.Fatalf("hop %d: %v", i, err)
		}
		if reason != "" {
			t.Fatalf("hop %d in fresh thread should admit, got %q", i, reason)
		}
	}
	// Next one anywhere should hit the global cap.
	reason, err := tr.AdmitOrDrop("UJoanne", "URoss", "C1", "fresh-thread", "m")
	if err != nil {
		t.Fatalf("AdmitOrDrop: %v", err)
	}
	if reason != ReasonGlobalDailyCeiling {
		t.Fatalf("over-cap hop should drop with ReasonGlobalDailyCeiling, got %q", reason)
	}
}

func TestAdmit_DailyResetAtMidnightUTC(t *testing.T) {
	tr := newTracker(t)
	day1 := time.Date(2026, 5, 30, 23, 59, 0, 0, time.UTC)
	tr.clock = func() time.Time { return day1 }
	// Fill global cap on day 1.
	for i := 0; i < MaxSpawnsPerDay; i++ {
		thread := "t" + string(rune('A'+i%26)) + "-" + string(rune('0'+i/26))
		if reason, err := tr.AdmitOrDrop("UJoanne", "URoss", "C1", thread, "m"); err != nil || reason != "" {
			t.Fatalf("day1 hop %d: err=%v reason=%q", i, err, reason)
		}
	}
	if reason, _ := tr.AdmitOrDrop("UJoanne", "URoss", "C1", "blocked", "m"); reason != ReasonGlobalDailyCeiling {
		t.Fatalf("expected global cap on day1 over-cap; got %q", reason)
	}
	// Cross midnight UTC.
	day2 := time.Date(2026, 5, 31, 0, 0, 1, 0, time.UTC)
	tr.clock = func() time.Time { return day2 }
	reason, err := tr.AdmitOrDrop("UJoanne", "URoss", "C1", "day2-thread", "m")
	if err != nil {
		t.Fatalf("day2 AdmitOrDrop: %v", err)
	}
	if reason != "" {
		t.Fatalf("first hop after midnight UTC should admit, got %q", reason)
	}
}

func TestAdmit_ThreadSpawnCeiling(t *testing.T) {
	tr := newTracker(t)
	// Spawn ceiling = 10 total hops in one thread, irrespective of time.
	// To exercise it without the hop cap firing, space each hop > 24h
	// apart so each is alone in its rolling 24h window.
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < MaxSpawnsPerThread; i++ {
		t0 := base.Add(time.Duration(i) * 25 * time.Hour)
		tr.clock = func() time.Time { return t0 }
		from, to := "UJoanne", "URoss"
		if i%2 == 1 {
			from, to = "URoss", "UJoanne"
		}
		if reason, err := tr.AdmitOrDrop(from, to, "C1", "t", "m"); err != nil || reason != "" {
			t.Fatalf("hop %d (t=%s): err=%v reason=%q", i, t0, err, reason)
		}
	}
	// Hop 11 in the same thread — hop window is empty (last hop > 24h ago),
	// but spawn ceiling is hit.
	t11 := base.Add(time.Duration(MaxSpawnsPerThread) * 25 * time.Hour)
	tr.clock = func() time.Time { return t11 }
	reason, err := tr.AdmitOrDrop("UJoanne", "URoss", "C1", "t", "m")
	if err != nil {
		t.Fatalf("AdmitOrDrop: %v", err)
	}
	if reason != ReasonThreadSpawnCeiling {
		t.Fatalf("expected ReasonThreadSpawnCeiling after %d hops in same thread, got %q", MaxSpawnsPerThread, reason)
	}
}

func TestAdmit_StatePersistedAcrossInstances(t *testing.T) {
	dir := t.TempDir()
	tr := New(dir)
	if reason, err := tr.AdmitOrDrop("UJoanne", "URoss", "C1", "t", "m1"); err != nil || reason != "" {
		t.Fatalf("first instance: err=%v reason=%q", err, reason)
	}
	// New tracker, same workspace — should see the hop on disk.
	tr2 := New(dir)
	s, err := tr2.load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(s.Threads) != 1 {
		t.Fatalf("expected 1 thread persisted, got %d", len(s.Threads))
	}
	ts := s.Threads["C1:t"]
	if ts == nil || len(ts.Hops) != 1 {
		t.Fatalf("expected 1 hop persisted, got %#v", ts)
	}
}

func TestKillSwitchPath(t *testing.T) {
	tr := newTracker(t)
	want := filepath.Join(tr.workspaceBase, ".ross-loops", "cross-agent-disabled")
	if got := tr.KillSwitchPath(); got != want {
		t.Fatalf("KillSwitchPath() = %q, want %q", got, want)
	}
	if tr.KillSwitchEngaged() {
		t.Fatal("kill switch should start disengaged")
	}
	if err := tr.EngageKillSwitch(); err != nil {
		t.Fatalf("EngageKillSwitch: %v", err)
	}
	if !tr.KillSwitchEngaged() {
		t.Fatal("kill switch should be engaged after EngageKillSwitch")
	}
	// The flag file should be readable / exist.
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("flag file should exist: %v", err)
	}
}
