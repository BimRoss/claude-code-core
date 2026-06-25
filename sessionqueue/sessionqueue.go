// Package sessionqueue is the shared per-session serialization, batch
// coalescing, and on-disk persistence layer for all three agents (Ross,
// Joanne, personal-agent). It is a genericized extraction of Ross's
// session_queue.go + queue_persistence.go.
//
// Why this exists
//
// Two messages that resolve to the same Claude session UUID (same thread,
// same DM, same loop) would race on the same `--resume` JSONL if their
// handlers ran concurrently. The shared JSONL persists "the agent is about
// to call tool X" between turns, and two concurrent readers each
// independently fire those pending calls — the duplicate-onboarding-email
// bug on Joanne 2026-06-01 (5 emails sent when 1 was intended).
//
// The queue solves it two ways at once:
//
//  1. Serialization. At most one spawn runs per session_id at a time. A
//     second arrival on the same session waits in the queue until the
//     active spawn finishes.
//
//  2. Coalescing. When the active spawn finishes, the runner drains ALL
//     waiters at once and hands them to the Runner as a single batch
//     (texts joined, files unioned, reply targeted at the latest message
//     by the caller's Runner). A user who types five rapid follow-ups gets
//     one reply that addresses all five — instead of five reply turns.
//
// Top-level channel messages compute a unique session_id per message (see
// the session package's Deriver), so they claim immediately and never
// contend. Routing every message through this queue is therefore safe.
//
// Persistence
//
// The in-memory waiter list vanishes silently on OOMKill (no SIGTERM). To
// survive that, every enqueue writes a marker to
// <workspaceBase>/<channel>/.session-queue/<ts>.json; the drain removes it
// as it pops each waiter into a batch (the running batch is then covered by
// the agent's own resume-marker path). On boot, Replay walks every
// workspace's .session-queue dir and re-injects each surviving message.
// Markers older than queueStaleness (30m) are removed without replay so a
// forgotten queue from days ago can't surprise the channel with a stale
// reply.
//
// Instance-scoped: each agent owns one *Queue[F] (no package globals), and
// is generic over its own file-reference type F (which must JSON-serialize).
package sessionqueue

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/slack-go/slack/slackevents"
)

// queueDir is the per-workspace directory that holds queue markers.
// Standardized across agents (Ross historically used ".ross-queue"; old
// markers from that name orphan once on the cutover deploy — acceptable,
// same as a restart, and the staleness sweep ignores them).
const queueDir = ".session-queue"

// queueStaleness is the boot-replay cutoff: anything older than this is
// treated as abandoned and removed without replay (mirrors resume-marker
// semantics so a forgotten queue can't surprise a channel days later).
const queueStaleness = 30 * time.Minute

// DefaultGrace is the inter-batch sleep agents should use when their grace
// env (e.g. ROSS_SESSION_GRACE_MS) is UNSET. Pass it to New for the unset
// case; pass the parsed value (including 0 to disable) otherwise. See drain
// for why the delay exists.
const DefaultGrace = 750 * time.Millisecond

// Msg is a single queued Slack message plus its file references. Event and
// Files are exported because the Runner consumes them; the rest is internal
// bookkeeping.
type Msg[F any] struct {
	Event *slackevents.MessageEvent
	Files []F

	workspaceBase string
	cancelled     atomic.Bool
}

// Runner is the per-batch work the queue invokes. Production wires this to
// the agent's handler via a synthesized merged event; tests substitute a
// fake.
type Runner[F any] func(batch []*Msg[F])

// sessionQueue is the per-session waiter list. One per live session_id.
type sessionQueue[F any] struct {
	mu      sync.Mutex
	running bool
	waiters []*Msg[F]
}

// Queue is one agent's instance of the per-session serialization +
// coalescing + persistence machinery. Construct with New.
type Queue[F any] struct {
	grace time.Duration

	// mu guards both maps below.
	mu       sync.Mutex
	sessions map[string]*sessionQueue[F]
	// queuedByMsg lets MarkCancelled find a queued (not-yet-started)
	// message by its channel+ts, so a 🔴 reaction on a waiter cancels it
	// before it is folded into a batch.
	queuedByMsg map[string]*Msg[F]
}

// New constructs an empty Queue. grace is the inter-batch sleep between
// consecutive spawns of the same session, stored VERBATIM: a grace of 0
// disables the delay entirely (the documented Ross behavior for
// ROSS_SESSION_GRACE_MS=0), a negative value is clamped to 0.
//
// The 750ms default is the CALLER's concern, not New's: an agent resolves its
// own env and passes DefaultGrace when the env is unset, the parsed value
// otherwise (including 0 to disable). Folding the default into New here would
// make a deliberate "0 = disable" indistinguishable from "unset", silently
// re-enabling a 750ms delay an operator turned off.
func New[F any](grace time.Duration) *Queue[F] {
	if grace < 0 {
		grace = 0
	}
	return &Queue[F]{
		grace:       grace,
		sessions:    map[string]*sessionQueue[F]{},
		queuedByMsg: map[string]*Msg[F]{},
	}
}

// key is the (channel, ts) identity used to find a queued waiter. Matches
// Ross's inflightKey format.
func key(channel, ts string) string { return channel + ":" + ts }

// Enqueue routes a message through the per-session queue. If no spawn is in
// flight for sessionID, the message runs immediately on a new goroutine.
// Otherwise it appends to the waiter list and returns; the in-flight runner
// drains all waiters (as one coalesced batch) when its current spawn
// finishes.
//
// workspaceBase is the PVC root (typically /data/workspaces). When
// non-empty, each enqueue writes a marker so the waiter survives
// OOMKill/SIGKILL — Replay re-enqueues it on the next pod start. Empty
// workspaceBase disables persistence (tests).
//
// inflight is the same WaitGroup the dispatch loop uses so drain-on-SIGTERM
// still accounts for queued work.
//
// onCancel runs when a single queued waiter was already cancelled
// (MarkCancelled) before it reached a batch — production posts the same
// "🔴 stopped" UX. onAck runs when a message is enqueued behind an
// in-flight predecessor — used to add the ⏳ reaction immediately.
func (q *Queue[F]) Enqueue(
	sessionID string,
	ev *slackevents.MessageEvent,
	files []F,
	workspaceBase string,
	inflight *sync.WaitGroup,
	run Runner[F],
	onCancel func(*slackevents.MessageEvent),
	onAck func(*slackevents.MessageEvent),
) {
	msgKey := key(ev.Channel, ev.TimeStamp)
	qm := &Msg[F]{Event: ev, Files: files, workspaceBase: workspaceBase}

	// Persist BEFORE we add to the in-memory waiter list. If the write
	// fails we log and continue: in-memory queuing still works, we just
	// lose the OOMKill-survival guarantee for this message. The reverse
	// order would briefly expose a window where an OOM after the in-memory
	// add but before the disk write loses a "queued" waiter without any
	// record.
	if err := q.writeMarker(workspaceBase, marker[F]{
		SessionID:   sessionID,
		Channel:     ev.Channel,
		ChannelType: ev.ChannelType,
		User:        ev.User,
		MessageTS:   ev.TimeStamp,
		ThreadTS:    ev.ThreadTimeStamp,
		Text:        ev.Text,
		Files:       files,
		EnqueuedAt:  time.Now().UTC(),
	}); err != nil {
		slog.Warn("queue_marker_write_failed",
			"error", err,
			"session_id", sessionID,
			"channel", ev.Channel,
			"msg_ts", ev.TimeStamp)
	}

	q.mu.Lock()
	sq, ok := q.sessions[sessionID]
	if !ok {
		sq = &sessionQueue[F]{}
		q.sessions[sessionID] = sq
	}
	q.queuedByMsg[msgKey] = qm
	q.mu.Unlock()

	sq.mu.Lock()
	wasRunning := sq.running
	sq.waiters = append(sq.waiters, qm)
	if !wasRunning {
		sq.running = true
	}
	sq.mu.Unlock()

	if wasRunning {
		slog.Info("session_queue_waited",
			"session_id", sessionID,
			"channel", ev.Channel,
			"msg_ts", ev.TimeStamp)
		if onAck != nil {
			onAck(ev)
		}
		return
	}

	inflight.Add(1)
	go q.drain(sessionID, sq, inflight, run, onCancel)
}

// drain is the per-session worker goroutine. It pops the queued waiters as
// a single coalesced batch on each iteration, runs the batch through run,
// then loops to handle anything that arrived during the run. Exits when the
// queue stays empty.
func (q *Queue[F]) drain(
	sessionID string,
	sq *sessionQueue[F],
	inflight *sync.WaitGroup,
	run Runner[F],
	onCancel func(*slackevents.MessageEvent),
) {
	defer inflight.Done()
	for {
		sq.mu.Lock()
		if len(sq.waiters) == 0 {
			sq.running = false
			sq.mu.Unlock()
			// GC the empty queue. Re-check under the instance lock to
			// avoid racing a fresh enqueue that grabbed sq but hasn't
			// taken sq.mu yet.
			q.mu.Lock()
			if cur, ok := q.sessions[sessionID]; ok && cur == sq {
				sq.mu.Lock()
				if len(sq.waiters) == 0 && !sq.running {
					delete(q.sessions, sessionID)
				}
				sq.mu.Unlock()
			}
			q.mu.Unlock()
			return
		}
		// Drain ALL pending waiters into a single batch — that's the
		// coalescing step. Partition out anyone MarkCancelled marked
		// cancelled before they reached the batch.
		batch := make([]*Msg[F], 0, len(sq.waiters))
		cancelled := make([]*Msg[F], 0)
		for _, qm := range sq.waiters {
			if qm.cancelled.Load() {
				cancelled = append(cancelled, qm)
			} else {
				batch = append(batch, qm)
			}
		}
		sq.waiters = nil
		sq.mu.Unlock()

		// Forget queued-by-msg entries for everyone in this batch — they
		// are no longer "queued" once we've taken them.
		q.mu.Lock()
		for _, qm := range batch {
			delete(q.queuedByMsg, key(qm.Event.Channel, qm.Event.TimeStamp))
		}
		for _, qm := range cancelled {
			delete(q.queuedByMsg, key(qm.Event.Channel, qm.Event.TimeStamp))
		}
		q.mu.Unlock()

		// Remove queue markers for everyone in this drain — running
		// batches are then covered by the agent's resume marker on
		// context.Canceled, and cancelled waiters need no replay. A remove
		// failure here only leaks a stale marker (the boot scan removes it
		// on its next staleness pass) so we log and proceed.
		for _, qm := range batch {
			if err := q.removeMarker(qm.workspaceBase, qm.Event.Channel, qm.Event.TimeStamp); err != nil {
				slog.Warn("queue_marker_remove_failed",
					"error", err, "channel", qm.Event.Channel, "msg_ts", qm.Event.TimeStamp)
			}
		}
		for _, qm := range cancelled {
			if err := q.removeMarker(qm.workspaceBase, qm.Event.Channel, qm.Event.TimeStamp); err != nil {
				slog.Warn("queue_marker_remove_failed",
					"error", err, "channel", qm.Event.Channel, "msg_ts", qm.Event.TimeStamp)
			}
		}

		// Cancelled waiters get the stop UX — same as if they'd been live
		// and 🔴'd.
		for _, qm := range cancelled {
			slog.Info("session_queue_skipped_cancelled",
				"channel", qm.Event.Channel, "msg_ts", qm.Event.TimeStamp)
			if onCancel != nil {
				safeRunCancel(onCancel, qm.Event)
			}
		}

		if len(batch) == 0 {
			// Everyone in this drain was cancelled. Loop to check for new
			// arrivals (or to exit if the queue is now empty).
			continue
		}
		if len(batch) > 1 {
			slog.Info("session_queue_batch_coalesced",
				"session_id", sessionID,
				"channel", batch[0].Event.Channel,
				"batch_size", len(batch),
				"latest_ts", batch[len(batch)-1].Event.TimeStamp)
		}
		safeRun(run, batch)
		// Grace delay before the next batch can claim --resume. Claude Code
		// holds an internal session lockfile that releases asynchronously
		// from process exit (Joanne 2026-06-13: claude_done at T then
		// --resume at T+0.3s errored with "Session ID ... is already in
		// use"). The queue serializes batches at the Go level, but
		// cmd.Wait() returning ≠ claude-code's lock released. A small sleep
		// here is the cheapest place to absorb that gap.
		time.Sleep(q.grace)
	}
}

// safeRun isolates Runner panics so one crashing batch doesn't strand the
// rest of the session's queue.
func safeRun[F any](run Runner[F], batch []*Msg[F]) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("session_queue_runner_panic",
				"panic", r,
				"batch_size", len(batch))
		}
	}()
	run(batch)
}

func safeRunCancel(onCancel func(*slackevents.MessageEvent), msg *slackevents.MessageEvent) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("session_queue_cancel_panic", "panic", r, "msg_ts", msg.TimeStamp)
		}
	}()
	onCancel(msg)
}

// MarkCancelled finds a queued (not-yet-started) message by its
// (channel, ts) and marks it cancelled. Returns true if a queued waiter was
// found and newly cancelled. Used when 🔴 lands on a message that hasn't
// started running yet. The runner detects the flag when it drains the
// waiter list and invokes onCancel instead of folding the message into the
// batch.
func (q *Queue[F]) MarkCancelled(channel, ts string) bool {
	msgKey := key(channel, ts)
	q.mu.Lock()
	qm, ok := q.queuedByMsg[msgKey]
	q.mu.Unlock()
	if !ok {
		return false
	}
	return qm.cancelled.CompareAndSwap(false, true)
}

// ----- persistence -----

// marker is the on-disk record of a Slack message that landed in the queue.
// EnqueuedAt is set by writeMarker, not the caller. F must JSON-serialize.
type marker[F any] struct {
	SessionID   string    `json:"session_id"`
	Channel     string    `json:"channel"`
	ChannelType string    `json:"channel_type,omitempty"`
	User        string    `json:"user"`
	MessageTS   string    `json:"message_ts"`
	ThreadTS    string    `json:"thread_ts"`
	Text        string    `json:"text"`
	Files       []F       `json:"files,omitempty"`
	EnqueuedAt  time.Time `json:"enqueued_at"`
}

// markerPath returns the canonical on-disk location for a marker. One file
// per (workspace, message_ts).
func markerPath(workspace, messageTS string) string {
	return filepath.Join(workspace, queueDir, messageTS+".json")
}

// writeMarker persists the marker atomically (tmp-then-rename). Errors are
// returned so the caller can log; the caller does NOT block the enqueue on
// failure. If workspaceBase is empty, this is a no-op (test path).
func (q *Queue[F]) writeMarker(workspaceBase string, m marker[F]) error {
	if workspaceBase == "" {
		return nil
	}
	workspace := filepath.Join(workspaceBase, m.Channel)
	dir := filepath.Join(workspace, queueDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir queue dir: %w", err)
	}
	final := markerPath(workspace, m.MessageTS)
	tmp := final + ".tmp"
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal queue marker: %w", err)
	}
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return fmt.Errorf("write queue marker tmp: %w", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		return fmt.Errorf("rename queue marker: %w", err)
	}
	return nil
}

// removeMarker deletes the marker for a (workspace, messageTS). Idempotent —
// missing-file is success. If workspaceBase is empty, this is a no-op.
func (q *Queue[F]) removeMarker(workspaceBase, channel, messageTS string) error {
	if workspaceBase == "" {
		return nil
	}
	workspace := filepath.Join(workspaceBase, channel)
	err := os.Remove(markerPath(workspace, messageTS))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// loadMarkers scans every channel workspace under workspaceBase and returns
// surviving queue markers. Corrupt or unreadable markers are logged and
// skipped; a bad file in one channel must never block startup for the
// others.
func (q *Queue[F]) loadMarkers(workspaceBase string) []marker[F] {
	entries, err := os.ReadDir(workspaceBase)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("queue_scan_workspace_base_failed", "error", err)
		}
		return nil
	}
	var markers []marker[F]
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		workspace := filepath.Join(workspaceBase, e.Name())
		markers = append(markers, q.loadMarkersInWorkspace(workspace)...)
	}
	return markers
}

func (q *Queue[F]) loadMarkersInWorkspace(workspace string) []marker[F] {
	dir := filepath.Join(workspace, queueDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("queue_scan_dir_failed", "workspace", workspace, "error", err)
		}
		return nil
	}
	var markers []marker[F]
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		body, err := os.ReadFile(path)
		if err != nil {
			slog.Warn("queue_marker_read_failed", "path", path, "error", err)
			continue
		}
		var m marker[F]
		if err := json.Unmarshal(body, &m); err != nil {
			slog.Warn("queue_marker_parse_failed", "path", path, "error", err)
			continue
		}
		if m.Channel == "" || m.MessageTS == "" {
			slog.Warn("queue_marker_missing_required_fields", "path", path)
			continue
		}
		markers = append(markers, m)
	}
	return markers
}

// markerStale returns true if the marker is older than queueStaleness.
func markerStale[F any](m marker[F], now time.Time) bool {
	return now.Sub(m.EnqueuedAt) > queueStaleness
}

// syntheticEvent builds the MessageEvent the dispatcher would have
// constructed from the original Slack delivery.
func syntheticEvent[F any](m marker[F]) *slackevents.MessageEvent {
	return &slackevents.MessageEvent{
		Type:            "message",
		Channel:         m.Channel,
		ChannelType:     m.ChannelType,
		User:            m.User,
		Text:            m.Text,
		TimeStamp:       m.MessageTS,
		ThreadTimeStamp: m.ThreadTS,
	}
}

// Replay scans every workspace under workspaceBase for queue markers and
// re-injects each surviving one via reinject (which the agent wires to its
// standard route → Enqueue path). Markers older than queueStaleness are
// removed without replay. Runs at pod boot, after the Slack auth handshake.
func (q *Queue[F]) Replay(workspaceBase string, reinject func(ev *slackevents.MessageEvent, files []F)) {
	markers := q.loadMarkers(workspaceBase)
	if len(markers) == 0 {
		slog.Info("queue_scan_no_markers")
		return
	}
	slog.Info("queue_scan_found_markers", "count", len(markers))
	now := time.Now().UTC()
	for _, m := range markers {
		if markerStale(m, now) {
			slog.Info("queue_marker_stale_skipped",
				"channel", m.Channel,
				"msg_ts", m.MessageTS,
				"age", now.Sub(m.EnqueuedAt).Round(time.Second))
			if err := q.removeMarker(workspaceBase, m.Channel, m.MessageTS); err != nil {
				slog.Warn("queue_marker_stale_cleanup_failed", "error", err)
			}
			continue
		}
		slog.Info("queue_marker_replaying",
			"channel", m.Channel,
			"msg_ts", m.MessageTS,
			"age", now.Sub(m.EnqueuedAt).Round(time.Second))
		reinject(syntheticEvent(m), m.Files)
	}
}

// ----- test helpers -----

// DepthForTest returns the current waiter count for a session. Test-only.
func (q *Queue[F]) DepthForTest(sessionID string) int {
	q.mu.Lock()
	sq, ok := q.sessions[sessionID]
	q.mu.Unlock()
	if !ok {
		return 0
	}
	sq.mu.Lock()
	defer sq.mu.Unlock()
	return len(sq.waiters)
}

// ResetForTest wipes all in-memory queue state. Test-only.
func (q *Queue[F]) ResetForTest() {
	q.mu.Lock()
	q.sessions = map[string]*sessionQueue[F]{}
	q.queuedByMsg = map[string]*Msg[F]{}
	q.mu.Unlock()
}
