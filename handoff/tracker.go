// Package handoff tracks cross-agent handoffs (Ross ↔ Joanne) per Slack
// thread and globally per day, so the harness can enforce the runaway-loop
// guardrails defined in BimRoss/claude-code-ross#257.
//
// Constraints (v1 defaults; see #257):
//   - Per-thread hop cap: 5 cross-agent hops in any 24h window.
//   - Per-thread spawn cap: 10 cross-agent spawns total (catches a longer
//     non-converging conversation that hasn't tripped the hop cap yet).
//   - Global daily cap: 50 cross-agent spawns workspace-wide, resets at
//     midnight UTC. Protects the shared Claude Max quota from one chatty
//     channel starving every other channel for the rest of the day.
//   - Cycle detection: if the last hop in this thread was *from* the same
//     agent we're about to spawn (i.e. ping-pong without a human in between),
//     refuse — caught one tick too early but cheap insurance.
//   - Kill switch: presence of `.ross-loops/cross-agent-disabled` in the
//     workspace short-circuits everything (drop the event silently).
//
// State lives in `<workspaceBase>/.ross-loops/handoffs.json` so it survives
// pod rollouts and is shared between Ross and Joanne (both binaries can
// read/write the same per-channel workspace).
package handoff

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	// MaxHopsPerThread is the hop cap inside a 24h window. A hop = one
	// cross-agent handoff (Joanne→Ross or Ross→Joanne).
	MaxHopsPerThread = 5
	// MaxSpawnsPerThread is the spawn count cap per thread (no time window
	// — covers longer non-converging conversations).
	MaxSpawnsPerThread = 10
	// MaxSpawnsPerDay is the global per-day cap across the whole workspace.
	MaxSpawnsPerDay = 50
	// HopWindow is the rolling window for MaxHopsPerThread.
	HopWindow = 24 * time.Hour

	stateFile  = "handoffs.json"
	killSwitch = "cross-agent-disabled"
	stateDir   = ".ross-loops"
)

// DropReason names a structural reason for refusing a cross-agent handoff.
// Logged on every drop so operators can grep for "what caused that".
type DropReason string

const (
	ReasonKillSwitch         DropReason = "kill_switch"
	ReasonHopCap             DropReason = "hop_cap"
	ReasonThreadSpawnCeiling DropReason = "thread_spawn_ceiling"
	ReasonGlobalDailyCeiling DropReason = "global_daily_ceiling"
	ReasonCycleDetect        DropReason = "cycle_detect"
)

// Hop is one recorded cross-agent handoff.
type Hop struct {
	From string    `json:"from"`   // bot user ID of the initiator
	To   string    `json:"to"`     // bot user ID being handed off to
	At   time.Time `json:"at"`     // when the handoff was admitted
	MsgTS string   `json:"msg_ts"` // Slack ts of the triggering message
}

// ThreadState is the per-(channel, thread_ts) running tally.
type ThreadState struct {
	Channel   string `json:"channel"`
	ThreadTS  string `json:"thread_ts"`
	Hops      []Hop  `json:"hops"`
	StartedAt time.Time `json:"started_at"`
}

// state is the on-disk schema for `handoffs.json`.
type state struct {
	Threads        map[string]*ThreadState `json:"threads"`
	GlobalDailyKey string                  `json:"global_daily_key"` // "2026-05-30"
	GlobalDailyN   int                     `json:"global_daily_n"`
}

// Tracker enforces the v1 caps. Construct with New(workspaceBase). Safe for
// concurrent use; serialized via an internal mutex (read-modify-write of the
// state file is cheap and rare enough that a single lock is fine).
type Tracker struct {
	workspaceBase string
	mu            sync.Mutex
	clock         func() time.Time
}

// New returns a Tracker rooted at workspaceBase. The state file is created
// lazily on first write.
func New(workspaceBase string) *Tracker {
	return &Tracker{workspaceBase: workspaceBase, clock: time.Now}
}

// statePath returns the path to handoffs.json under workspaceBase.
func (t *Tracker) statePath() string {
	return filepath.Join(t.workspaceBase, stateDir, stateFile)
}

// KillSwitchPath returns the path to the kill-switch flag file. Operators
// can drop or remove it via natural-language commands to either bot.
func (t *Tracker) KillSwitchPath() string {
	return filepath.Join(t.workspaceBase, stateDir, killSwitch)
}

// KillSwitchEngaged reports whether the kill-switch flag file exists.
func (t *Tracker) KillSwitchEngaged() bool {
	_, err := os.Stat(t.KillSwitchPath())
	return err == nil
}

// load reads handoffs.json. Returns a zero state if the file doesn't exist
// (first-ever handoff in this workspace).
func (t *Tracker) load() (*state, error) {
	b, err := os.ReadFile(t.statePath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &state{Threads: map[string]*ThreadState{}}, nil
		}
		return nil, fmt.Errorf("read handoffs.json: %w", err)
	}
	var s state
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("parse handoffs.json: %w", err)
	}
	if s.Threads == nil {
		s.Threads = map[string]*ThreadState{}
	}
	return &s, nil
}

// save writes handoffs.json atomically.
func (t *Tracker) save(s *state) error {
	dir := filepath.Join(t.workspaceBase, stateDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal handoffs.json: %w", err)
	}
	tmp := t.statePath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, t.statePath()); err != nil {
		return fmt.Errorf("rename %s: %w", tmp, err)
	}
	return nil
}

func threadKey(channel, threadTS string) string {
	if threadTS == "" {
		// Top-level channel messages — treat the channel itself as the
		// "thread" so we still get per-conversation accounting even when
		// agents talk in the channel root.
		return channel + ":root"
	}
	return channel + ":" + threadTS
}

func dayKey(t time.Time) string {
	return t.UTC().Format("2006-01-02")
}

// AdmitOrDrop attempts to record a cross-agent handoff. If admitted, the
// hop is persisted and (nil, "") is returned. If dropped, returns the
// DropReason; nothing is written to disk on a drop.
//
// `from` is the bot user ID initiating the handoff (e.g. Joanne's bot user
// ID), `to` is the bot user ID being handed off to (e.g. Ross's), and
// (channel, threadTS, msgTS) identify the triggering Slack message.
func (t *Tracker) AdmitOrDrop(from, to, channel, threadTS, msgTS string) (DropReason, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.KillSwitchEngaged() {
		return ReasonKillSwitch, nil
	}

	s, err := t.load()
	if err != nil {
		return "", err
	}

	now := t.clock()
	today := dayKey(now)

	// Global daily ceiling — protects the shared Claude Max quota.
	if s.GlobalDailyKey != today {
		s.GlobalDailyKey = today
		s.GlobalDailyN = 0
	}
	if s.GlobalDailyN >= MaxSpawnsPerDay {
		return ReasonGlobalDailyCeiling, nil
	}

	key := threadKey(channel, threadTS)
	ts, ok := s.Threads[key]
	if !ok {
		ts = &ThreadState{Channel: channel, ThreadTS: threadTS, StartedAt: now}
		s.Threads[key] = ts
	}

	// Per-thread spawn ceiling (total, no time window).
	if len(ts.Hops) >= MaxSpawnsPerThread {
		return ReasonThreadSpawnCeiling, nil
	}

	// Per-thread hop cap inside the rolling 24h window.
	cutoff := now.Add(-HopWindow)
	recent := 0
	for _, h := range ts.Hops {
		if h.At.After(cutoff) {
			recent++
		}
	}
	if recent >= MaxHopsPerThread {
		return ReasonHopCap, nil
	}

	// Cycle detection: the agent we're about to spawn is the *same* one
	// that was spawned by the last hop. That means we'd be re-firing the
	// same agent without an intervening reply from the other side — a
	// self-spawn loop. Healthy alternation always toggles `to` between
	// the two agents; two hops in a row with the same `to` is the bug.
	if n := len(ts.Hops); n > 0 {
		last := ts.Hops[n-1]
		if last.To == to {
			return ReasonCycleDetect, nil
		}
	}

	// Admit.
	ts.Hops = append(ts.Hops, Hop{From: from, To: to, At: now, MsgTS: msgTS})
	s.GlobalDailyN++
	if err := t.save(s); err != nil {
		return "", err
	}
	return "", nil
}

// EngageKillSwitch creates the flag file. Idempotent.
func (t *Tracker) EngageKillSwitch() error {
	dir := filepath.Join(t.workspaceBase, stateDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	return os.WriteFile(t.KillSwitchPath(), []byte("cross-agent handoffs disabled\n"), 0o644)
}

// DisengageKillSwitch removes the flag file. No-op if not engaged.
func (t *Tracker) DisengageKillSwitch() error {
	err := os.Remove(t.KillSwitchPath())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
