package sessionqueue

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/slack-go/slack/slackevents"
)

// fileRef is a concrete F for tests — a small struct with JSON tags so the
// marker's Files []F round-trip is genuinely exercised (not just nil).
type fileRef struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

// testGrace keeps functional tests fast. grace must be > 0 (New treats
// <= 0 as "use the 750ms default"), so we pass a tiny positive value.
const testGrace = time.Millisecond

func newTestQueue() *Queue[fileRef] { return New[fileRef](testGrace) }

func msgEvent(channel, threadTS, msgTS string) *slackevents.MessageEvent {
	return &slackevents.MessageEvent{
		Type:            "message",
		Channel:         channel,
		ChannelType:     "channel",
		User:            "U_TEST",
		Text:            "hello " + msgTS,
		TimeStamp:       msgTS,
		ThreadTimeStamp: threadTS,
	}
}

func waitFor(t *testing.T, d time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", d)
}

func TestNewGraceVerbatim(t *testing.T) {
	// grace is stored verbatim: 0 disables (Ross's ROSS_SESSION_GRACE_MS=0),
	// negative clamps to 0, positive passes through. The 750ms default is the
	// caller's concern (DefaultGrace), not New's.
	if got := New[fileRef](0).grace; got != 0 {
		t.Errorf("New(0) grace = %s, want 0 (disabled)", got)
	}
	if got := New[fileRef](-5 * time.Second).grace; got != 0 {
		t.Errorf("New(-5s) grace = %s, want 0 (clamped)", got)
	}
	if got := New[fileRef](100 * time.Millisecond).grace; got != 100*time.Millisecond {
		t.Errorf("New(100ms) grace = %s, want 100ms", got)
	}
	if got := New[fileRef](DefaultGrace).grace; got != 750*time.Millisecond {
		t.Errorf("New(DefaultGrace) grace = %s, want 750ms", got)
	}
}

func TestRunsImmediatelyWhenIdle(t *testing.T) {
	q := newTestQueue()
	var inflight sync.WaitGroup
	done := make(chan struct{})
	run := func(batch []*Msg[fileRef]) {
		if len(batch) != 1 {
			t.Errorf("expected batch of 1, got %d", len(batch))
		}
		close(done)
	}
	q.Enqueue("sess-A", msgEvent("C1", "t1", "m1"), nil, "", &inflight, run, nil, nil)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("first message did not run")
	}
	inflight.Wait()
}

func TestCoalescesBurstIntoSingleBatch(t *testing.T) {
	q := newTestQueue()
	var inflight sync.WaitGroup

	gate := make(chan struct{})
	var batches [][]string
	var batchesMu sync.Mutex

	run := func(batch []*Msg[fileRef]) {
		ids := make([]string, len(batch))
		for i, qm := range batch {
			ids[i] = qm.Event.TimeStamp
		}
		batchesMu.Lock()
		batches = append(batches, ids)
		hold := len(batches) == 1
		batchesMu.Unlock()
		if hold {
			<-gate
		}
	}

	ackCount := atomic.Int32{}
	onAck := func(_ *slackevents.MessageEvent) { ackCount.Add(1) }

	q.Enqueue("sess-burst", msgEvent("C1", "t1", "m1"), nil, "", &inflight, run, nil, onAck)
	waitFor(t, time.Second, func() bool {
		batchesMu.Lock()
		defer batchesMu.Unlock()
		return len(batches) == 1
	})

	q.Enqueue("sess-burst", msgEvent("C1", "t1", "m2"), nil, "", &inflight, run, nil, onAck)
	q.Enqueue("sess-burst", msgEvent("C1", "t1", "m3"), nil, "", &inflight, run, nil, onAck)
	q.Enqueue("sess-burst", msgEvent("C1", "t1", "m4"), nil, "", &inflight, run, nil, onAck)

	if got := ackCount.Load(); got != 3 {
		t.Fatalf("expected 3 acks (m2, m3, m4 queued); got %d", got)
	}

	gate <- struct{}{}
	inflight.Wait()

	batchesMu.Lock()
	defer batchesMu.Unlock()
	if len(batches) != 2 {
		t.Fatalf("expected 2 batches (m1, [m2 m3 m4]); got %d: %v", len(batches), batches)
	}
	if len(batches[0]) != 1 || batches[0][0] != "m1" {
		t.Errorf("first batch should be [m1], got %v", batches[0])
	}
	if len(batches[1]) != 3 || batches[1][0] != "m2" || batches[1][2] != "m4" {
		t.Errorf("second batch should be [m2 m3 m4], got %v", batches[1])
	}
	if got := q.DepthForTest("sess-burst"); got != 0 {
		t.Errorf("queue not drained: depth=%d", got)
	}
}

func TestCancelledWaiterSkippedFromBatch(t *testing.T) {
	q := newTestQueue()
	var inflight sync.WaitGroup

	gate := make(chan struct{})
	var batches [][]string
	var batchesMu sync.Mutex

	run := func(batch []*Msg[fileRef]) {
		ids := make([]string, len(batch))
		for i, qm := range batch {
			ids[i] = qm.Event.TimeStamp
		}
		batchesMu.Lock()
		batches = append(batches, ids)
		hold := len(batches) == 1
		batchesMu.Unlock()
		if hold {
			<-gate
		}
	}

	var cancelled []string
	var cancelMu sync.Mutex
	onCancel := func(m *slackevents.MessageEvent) {
		cancelMu.Lock()
		cancelled = append(cancelled, m.TimeStamp)
		cancelMu.Unlock()
	}

	q.Enqueue("sess-cancel", msgEvent("C1", "t1", "m1"), nil, "", &inflight, run, onCancel, nil)
	waitFor(t, time.Second, func() bool {
		batchesMu.Lock()
		defer batchesMu.Unlock()
		return len(batches) == 1
	})

	q.Enqueue("sess-cancel", msgEvent("C1", "t1", "m2"), nil, "", &inflight, run, onCancel, nil)
	q.Enqueue("sess-cancel", msgEvent("C1", "t1", "m3"), nil, "", &inflight, run, onCancel, nil)

	if !q.MarkCancelled("C1", "m2") {
		t.Fatal("expected to find m2 queued")
	}

	gate <- struct{}{}
	inflight.Wait()

	batchesMu.Lock()
	defer batchesMu.Unlock()
	if len(batches) != 2 {
		t.Fatalf("expected 2 batches, got %v", batches)
	}
	if len(batches[1]) != 1 || batches[1][0] != "m3" {
		t.Errorf("second batch should be [m3] (m2 cancelled), got %v", batches[1])
	}
	cancelMu.Lock()
	defer cancelMu.Unlock()
	if len(cancelled) != 1 || cancelled[0] != "m2" {
		t.Errorf("expected m2 to be cancelled, got %v", cancelled)
	}
}

func TestMarkCancelledUnknownReturnsFalse(t *testing.T) {
	q := newTestQueue()
	if q.MarkCancelled("C1", "nope") {
		t.Error("MarkCancelled on unknown (channel,ts) should return false")
	}
}

func TestRunnerPanicDoesNotStrandSuccessors(t *testing.T) {
	q := newTestQueue()
	var inflight sync.WaitGroup

	gate := make(chan struct{})
	var seenBatches [][]string
	var mu sync.Mutex
	run := func(batch []*Msg[fileRef]) {
		ids := make([]string, len(batch))
		for i, qm := range batch {
			ids[i] = qm.Event.TimeStamp
		}
		mu.Lock()
		seenBatches = append(seenBatches, ids)
		mu.Unlock()
		if ids[0] == "m1" {
			<-gate
			panic("simulated handler panic")
		}
	}

	q.Enqueue("sess-panic", msgEvent("C1", "t1", "m1"), nil, "", &inflight, run, nil, nil)
	waitFor(t, time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(seenBatches) == 1
	})
	q.Enqueue("sess-panic", msgEvent("C1", "t1", "m2"), nil, "", &inflight, run, nil, nil)

	gate <- struct{}{}
	inflight.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(seenBatches) != 2 || seenBatches[1][0] != "m2" {
		t.Fatalf("successor did not run after predecessor panic: %v", seenBatches)
	}
}

func TestDifferentSessionsRunConcurrently(t *testing.T) {
	q := newTestQueue()
	var inflight sync.WaitGroup

	var started atomic.Int32
	hold := make(chan struct{})
	run := func(_ []*Msg[fileRef]) {
		started.Add(1)
		<-hold
	}

	q.Enqueue("sess-A", msgEvent("C1", "tA", "m1"), nil, "", &inflight, run, nil, nil)
	q.Enqueue("sess-B", msgEvent("C1", "tB", "m2"), nil, "", &inflight, run, nil, nil)

	waitFor(t, time.Second, func() bool { return started.Load() == 2 })

	close(hold)
	inflight.Wait()
}

func TestResetForTest(t *testing.T) {
	q := newTestQueue()
	var inflight sync.WaitGroup
	gate := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	run := func(_ []*Msg[fileRef]) {
		once.Do(func() { close(started) })
		<-gate
	}
	q.Enqueue("sess-reset", msgEvent("C1", "t1", "m1"), nil, "", &inflight, run, nil, nil)
	<-started
	q.Enqueue("sess-reset", msgEvent("C1", "t1", "m2"), nil, "", &inflight, run, nil, func(_ *slackevents.MessageEvent) {})
	waitFor(t, time.Second, func() bool { return q.DepthForTest("sess-reset") == 1 })

	q.ResetForTest()
	if got := q.DepthForTest("sess-reset"); got != 0 {
		t.Errorf("after ResetForTest depth = %d, want 0", got)
	}
	close(gate)
	inflight.Wait()
}

// ----- persistence -----

func sampleMarker(channel, msgTS string) marker[fileRef] {
	return marker[fileRef]{
		SessionID:   "sess-" + msgTS,
		Channel:     channel,
		ChannelType: "channel",
		User:        "U_TEST",
		MessageTS:   msgTS,
		ThreadTS:    "t1",
		Text:        "hello " + msgTS,
		Files:       []fileRef{{ID: "F1", URL: "https://files/" + msgTS}},
		EnqueuedAt:  time.Now().UTC(),
	}
}

func TestMarkerRoundtripIncludingFiles(t *testing.T) {
	q := newTestQueue()
	base := t.TempDir()
	m := sampleMarker("C1", "1234.5678")
	if err := q.writeMarker(base, m); err != nil {
		t.Fatalf("write: %v", err)
	}
	path := markerPath(filepath.Join(base, m.Channel), m.MessageTS)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("marker missing on disk: %v", err)
	}
	got := q.loadMarkers(base)
	if len(got) != 1 {
		t.Fatalf("want 1 marker, got %d", len(got))
	}
	if got[0].Channel != m.Channel || got[0].MessageTS != m.MessageTS || got[0].Text != m.Text {
		t.Errorf("loaded marker differs from written: %+v vs %+v", got[0], m)
	}
	// The generic Files []F must survive the JSON round-trip intact.
	if len(got[0].Files) != 1 || got[0].Files[0] != m.Files[0] {
		t.Errorf("Files []F did not round-trip: got %+v want %+v", got[0].Files, m.Files)
	}
}

func TestMarkerRemoveIsIdempotent(t *testing.T) {
	q := newTestQueue()
	base := t.TempDir()
	m := sampleMarker("C1", "1.0")
	if err := q.writeMarker(base, m); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := q.removeMarker(base, m.Channel, m.MessageTS); err != nil {
		t.Fatalf("first remove: %v", err)
	}
	if err := q.removeMarker(base, m.Channel, m.MessageTS); err != nil {
		t.Fatalf("second remove (should be no-op): %v", err)
	}
}

func TestMarkerEmptyWorkspaceBaseNoOp(t *testing.T) {
	q := newTestQueue()
	if err := q.writeMarker("", sampleMarker("C1", "x")); err != nil {
		t.Fatalf("write with empty base: %v", err)
	}
	if err := q.removeMarker("", "C1", "x"); err != nil {
		t.Fatalf("remove with empty base: %v", err)
	}
}

func TestMarkerStaleness(t *testing.T) {
	m := marker[fileRef]{EnqueuedAt: time.Now().Add(-1 * time.Hour)}
	if !markerStale(m, time.Now()) {
		t.Errorf("1h-old marker should be stale")
	}
	fresh := marker[fileRef]{EnqueuedAt: time.Now()}
	if markerStale(fresh, time.Now()) {
		t.Errorf("just-written marker must not be stale")
	}
}

func TestMarkerCorruptFileSkipped(t *testing.T) {
	q := newTestQueue()
	base := t.TempDir()
	good := sampleMarker("C1", "good")
	if err := q.writeMarker(base, good); err != nil {
		t.Fatalf("write good: %v", err)
	}
	badDir := filepath.Join(base, "C1", queueDir)
	if err := os.WriteFile(filepath.Join(badDir, "bad.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write bad: %v", err)
	}
	got := q.loadMarkers(base)
	if len(got) != 1 || got[0].MessageTS != "good" {
		t.Errorf("want only the good marker, got %+v", got)
	}
}

func TestEnqueueWritesMarkerAndDrainRemovesIt(t *testing.T) {
	q := newTestQueue()
	base := t.TempDir()
	var inflight sync.WaitGroup
	done := make(chan struct{})
	run := func(batch []*Msg[fileRef]) {
		// Inside the runner the on-disk marker should already be gone:
		// drain removes it after popping waiters into the batch, before
		// invoking run.
		for _, qm := range batch {
			path := markerPath(filepath.Join(base, qm.Event.Channel), qm.Event.TimeStamp)
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Errorf("marker should be removed before runner fires: stat err=%v", err)
			}
		}
		close(done)
	}
	q.Enqueue("sess-disk", msgEvent("CX", "t1", "ts1"), nil, base, &inflight, run, nil, nil)

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runner did not fire")
	}
	inflight.Wait()
	path := markerPath(filepath.Join(base, "CX"), "ts1")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("marker should be removed after run: stat err=%v", err)
	}
}

func TestEnqueueWritesMarkerForWaiter(t *testing.T) {
	q := newTestQueue()
	base := t.TempDir()
	var inflight sync.WaitGroup
	gate := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	run := func(_ []*Msg[fileRef]) {
		once.Do(func() { close(started) })
		<-gate
	}
	q.Enqueue("sess-wait", msgEvent("CY", "t1", "m1"), nil, base, &inflight, run, nil, nil)
	<-started
	q.Enqueue("sess-wait", msgEvent("CY", "t1", "m2"), nil, base, &inflight, run, nil, func(_ *slackevents.MessageEvent) {})
	waiterPath := markerPath(filepath.Join(base, "CY"), "m2")
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(waiterPath); err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := os.Stat(waiterPath); err != nil {
		t.Fatalf("waiter marker should exist on disk: %v", err)
	}
	close(gate)
	inflight.Wait()
}

func TestReplayReinjectsFreshSkipsStale(t *testing.T) {
	q := newTestQueue()
	base := t.TempDir()

	fresh := sampleMarker("C1", "fresh")
	if err := q.writeMarker(base, fresh); err != nil {
		t.Fatalf("write fresh: %v", err)
	}
	stale := sampleMarker("C2", "stale")
	stale.EnqueuedAt = time.Now().Add(-2 * time.Hour).UTC()
	if err := q.writeMarker(base, stale); err != nil {
		t.Fatalf("write stale: %v", err)
	}

	var got []struct {
		ev    *slackevents.MessageEvent
		files []fileRef
	}
	q.Replay(base, func(ev *slackevents.MessageEvent, files []fileRef) {
		got = append(got, struct {
			ev    *slackevents.MessageEvent
			files []fileRef
		}{ev, files})
	})

	if len(got) != 1 {
		t.Fatalf("expected 1 reinject (fresh only), got %d", len(got))
	}
	if got[0].ev.Channel != "C1" || got[0].ev.TimeStamp != "fresh" {
		t.Errorf("reinjected wrong event: %+v", got[0].ev)
	}
	if got[0].ev.Text != fresh.Text || got[0].ev.User != fresh.User {
		t.Errorf("synthetic event lost fields: %+v", got[0].ev)
	}
	if len(got[0].files) != 1 || got[0].files[0] != fresh.Files[0] {
		t.Errorf("reinjected files []F mismatch: %+v", got[0].files)
	}

	// The stale marker is removed without replay; the fresh marker stays
	// (Replay does not consume it — the reinject → Enqueue path owns
	// removal on drain).
	stalePath := markerPath(filepath.Join(base, "C2"), "stale")
	if _, err := os.Stat(stalePath); !os.IsNotExist(err) {
		t.Errorf("stale marker should have been removed: stat err=%v", err)
	}
	freshPath := markerPath(filepath.Join(base, "C1"), "fresh")
	if _, err := os.Stat(freshPath); err != nil {
		t.Errorf("fresh marker should remain after Replay: %v", err)
	}
}

func TestReplayNoMarkersIsNoOp(t *testing.T) {
	q := newTestQueue()
	base := t.TempDir()
	called := false
	q.Replay(base, func(_ *slackevents.MessageEvent, _ []fileRef) { called = true })
	if called {
		t.Error("Replay should not reinject when there are no markers")
	}
}
