package resume

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

type fileRef struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

func TestWriteLoadRoundTrip_WithFiles(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "C1")
	m := Marker[fileRef]{
		Channel: "C1", ChannelType: "im", User: "U1",
		MessageTS: "1716000000.000100", ThreadTS: "", Text: "hi",
		Files:         []fileRef{{ID: "F1", URL: "http://x/f1"}},
		InterruptedAt: time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC),
	}
	if err := WriteMarker(ws, m); err != nil {
		t.Fatal(err)
	}
	got := LoadMarkers[fileRef](base)
	if len(got) != 1 {
		t.Fatalf("loaded %d markers, want 1", len(got))
	}
	g := got[0]
	if g.Channel != "C1" || g.MessageTS != m.MessageTS || g.Text != "hi" {
		t.Fatalf("marker fields lost: %+v", g)
	}
	if len(g.Files) != 1 || g.Files[0].ID != "F1" || g.Files[0].URL != "http://x/f1" {
		t.Fatalf("Files []F did not round-trip: %+v", g.Files)
	}
	if g.SourcePath == "" {
		t.Fatal("SourcePath should be set by LoadMarkers")
	}
}

func TestLoad_SkipsReplayContextAndCorrupt(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "C1")
	dir := filepath.Join(ws, markerDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A valid marker.
	if err := WriteMarker(ws, Marker[fileRef]{Channel: "C1", MessageTS: "1.1", InterruptedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	// The replay-context sibling — must be skipped (not parsed as a marker).
	WriteReplayContext(ws, ReplayContext{InterruptedAt: time.Now(), IsDM: true})
	// A corrupt file — must be skipped, not fatal.
	if err := os.WriteFile(filepath.Join(dir, "2.2.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A marker missing required fields — skipped.
	if err := os.WriteFile(filepath.Join(dir, "3.3.json"), []byte(`{"text":"no channel"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got := LoadMarkers[fileRef](base)
	if len(got) != 1 || got[0].MessageTS != "1.1" {
		t.Fatalf("expected only the 1 valid marker, got %d: %+v", len(got), got)
	}
}

func TestRemoveMarker_Idempotent(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "C1")
	if err := WriteMarker(ws, Marker[fileRef]{Channel: "C1", MessageTS: "1.1", InterruptedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := RemoveMarker(ws, "1.1"); err != nil {
		t.Fatal(err)
	}
	if err := RemoveMarker(ws, "1.1"); err != nil {
		t.Fatalf("second remove should be idempotent: %v", err)
	}
	if got := LoadMarkers[fileRef](base); len(got) != 0 {
		t.Fatalf("marker should be gone, got %d", len(got))
	}
}

func TestRemoveMarkerFile_BySourcePath(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "C1")
	if err := WriteMarker(ws, Marker[fileRef]{Channel: "C1", MessageTS: "9.9", InterruptedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	m := LoadMarkers[fileRef](base)[0]
	if err := RemoveMarkerFile(m.SourcePath); err != nil {
		t.Fatal(err)
	}
	if got := LoadMarkers[fileRef](base); len(got) != 0 {
		t.Fatalf("marker should be gone after RemoveMarkerFile, got %d", len(got))
	}
	if err := RemoveMarkerFile(""); err != nil {
		t.Fatalf("empty path should be a no-op: %v", err)
	}
}

func TestStale(t *testing.T) {
	now := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	if !Stale(now.Add(-31*time.Minute), now, DefaultStaleness) {
		t.Error("31m old should be stale at 30m threshold")
	}
	if Stale(now.Add(-29*time.Minute), now, DefaultStaleness) {
		t.Error("29m old should NOT be stale at 30m threshold")
	}
}

func TestReplayContextWriteAndPath(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "C1")
	WriteReplayContext(ws, ReplayContext{InterruptedAt: time.Now(), IsDM: true, SelfDeploy: true})
	if _, err := os.Stat(ReplayContextPath(ws)); err != nil {
		t.Fatalf("replay context not written at ReplayContextPath: %v", err)
	}
	// It must NOT show up as a marker.
	if got := LoadMarkers[fileRef](base); len(got) != 0 {
		t.Fatalf("replay-context should not be loaded as a marker, got %d", len(got))
	}
}

func TestLoad_EmptyBaseNoError(t *testing.T) {
	if got := LoadMarkers[fileRef](filepath.Join(t.TempDir(), "does-not-exist")); got != nil {
		t.Fatalf("missing base should yield nil, got %v", got)
	}
}
