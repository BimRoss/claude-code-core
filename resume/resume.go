// Package resume is the shared resume-marker store for all three agents.
//
// A resume marker is the on-disk record of a Slack message whose `claude` spawn
// was interrupted by a graceful-drain SIGTERM. On the next pod boot the agent
// scans markers and re-spawns each surviving turn (a replay distinct from the
// session-queue replay: a resume marker means the spawn was already underway, so
// it replays through handleMessage directly, not by re-enqueueing).
//
// This is the same marker-store pattern as core/sessionqueue's persistence half
// (atomic tmp+rename writes, per-channel dirs, corrupt-skip load, staleness
// sweep) genericized over the agent's file-ref type. NOTE (follow-up): the two
// stores are near-identical and could share a lower-level marker primitive.
//
// What is NOT here: detectSelfDeploy. That reads the agent's watches.json (the
// watch feature-module, Ross/PA-only — Joanne has no watches), so it stays
// agent-side. Agents compute SelfDeploy themselves and pass it into
// WriteReplayContext; the portable hint mechanism (ReplayContext +
// WriteReplayContext) lives here so every agent can use it.
package resume

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// DefaultStaleness is how long an interrupted turn may sit before boot-replay
// gives up (a normal rollout lands a new pod within ~drainCap). Agents pass
// this (or their own value) to Stale.
const DefaultStaleness = 30 * time.Minute

// markerDir is the per-workspace directory holding resume markers. Standardized
// across agents (was .ross-resume / .joanne-resume); existing markers under the
// old names orphan once on the cutover deploy — harmless, same as a restart.
const markerDir = ".session-resume"

// replayContextFile is the sibling hint file written into markerDir by
// WriteReplayContext. The marker loader skips it (it is not a marker).
const replayContextFile = "replay-context.json"

// Marker is the on-disk record of an interrupted turn, generic over the agent's
// file-ref type F (must JSON-serialize). Mirrors the synthetic MessageEvent the
// dispatcher reconstructs on replay; ChannelType is carried so a replayed DM
// isn't misrouted as a channel.
type Marker[F any] struct {
	Channel       string    `json:"channel"`
	ChannelType   string    `json:"channel_type,omitempty"`
	User          string    `json:"user"`
	MessageTS     string    `json:"message_ts"`
	ThreadTS      string    `json:"thread_ts"`
	Text          string    `json:"text"`
	Files         []F       `json:"files,omitempty"`
	InterruptedAt time.Time `json:"interrupted_at"`
	// SourcePath is the file this marker was loaded from (set by LoadMarkers,
	// never serialized). Lets replay remove the exact file even for markers
	// under a legacy layout. Use RemoveMarkerFile(m.SourcePath).
	SourcePath string `json:"-"`
}

// markerPath is the on-disk location for a (workspace, messageTS).
func markerPath(workspace, messageTS string) string {
	return filepath.Join(workspace, markerDir, messageTS+".json")
}

// WriteMarker persists a marker atomically (tmp-then-rename). workspace is the
// per-channel dir (e.g. <base>/<channel>). Errors are returned for the caller
// to log; the caller does not block the turn on a marker write failure.
func WriteMarker[F any](workspace string, m Marker[F]) error {
	dir := filepath.Join(workspace, markerDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir resume dir: %w", err)
	}
	final := markerPath(workspace, m.MessageTS)
	tmp := final + ".tmp"
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal marker: %w", err)
	}
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return fmt.Errorf("write marker tmp: %w", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		return fmt.Errorf("rename marker: %w", err)
	}
	return nil
}

// RemoveMarker deletes the marker for (workspace, messageTS). Idempotent —
// missing-file is success. Use at handler start (clear a prior interrupt
// record) and from the stale sweep.
func RemoveMarker(workspace, messageTS string) error {
	return removePath(markerPath(workspace, messageTS))
}

// RemoveMarkerFile deletes a marker by its exact path (Marker.SourcePath).
// Idempotent. Use on replay to remove a loaded marker regardless of layout.
func RemoveMarkerFile(path string) error {
	if path == "" {
		return nil
	}
	return removePath(path)
}

func removePath(path string) error {
	err := os.Remove(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// LoadMarkers scans every channel workspace under workspaceBase and returns
// surviving markers (with SourcePath set). Corrupt/unreadable markers are
// logged and skipped — a bad file must never block startup. The replay-context
// sibling file is skipped.
func LoadMarkers[F any](workspaceBase string) []Marker[F] {
	entries, err := os.ReadDir(workspaceBase)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("resume_scan_workspace_base_failed", "error", err)
		}
		return nil
	}
	var markers []Marker[F]
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		markers = append(markers, loadInWorkspace[F](filepath.Join(workspaceBase, e.Name()))...)
	}
	return markers
}

func loadInWorkspace[F any](workspace string) []Marker[F] {
	dir := filepath.Join(workspace, markerDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("resume_scan_dir_failed", "workspace", workspace, "error", err)
		}
		return nil
	}
	var markers []Marker[F]
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" || e.Name() == replayContextFile {
			continue
		}
		path := filepath.Join(dir, e.Name())
		body, err := os.ReadFile(path)
		if err != nil {
			slog.Warn("resume_marker_read_failed", "path", path, "error", err)
			continue
		}
		var m Marker[F]
		if err := json.Unmarshal(body, &m); err != nil {
			slog.Warn("resume_marker_parse_failed", "path", path, "error", err)
			continue
		}
		if m.Channel == "" || m.MessageTS == "" {
			slog.Warn("resume_marker_missing_required_fields", "path", path)
			continue
		}
		m.SourcePath = path
		markers = append(markers, m)
	}
	return markers
}

// Stale reports whether an interrupted-at time is older than staleness.
func Stale(interruptedAt, now time.Time, staleness time.Duration) bool {
	return now.Sub(interruptedAt) > staleness
}

// ReplayContext is the hint written before a replayed spawn so the spawned
// agent can calibrate its first reply (acknowledge the restart instead of
// resuming silently, and note a self-deploy). The spawned agent reads + deletes
// it at startup.
//
// SelfDeploy is CALLER-PROVIDED: detecting it requires the agent's watches.json
// (Ross/PA), which Joanne lacks — so the agent computes it and sets this field.
type ReplayContext struct {
	InterruptedAt time.Time `json:"interrupted_at"`
	IsDM          bool      `json:"is_dm"`
	SelfDeploy    bool      `json:"self_deploy"`
}

// WriteReplayContext persists the hint into the workspace's marker dir.
// Best-effort: a write failure just means the spawn falls back to normal
// startup, so errors are logged and not returned.
func WriteReplayContext(workspace string, rc ReplayContext) {
	dir := filepath.Join(workspace, markerDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.Warn("replay_context_mkdir_failed", "error", err)
		return
	}
	path := filepath.Join(dir, replayContextFile)
	body, err := json.MarshalIndent(rc, "", "  ")
	if err != nil {
		slog.Warn("replay_context_marshal_failed", "error", err)
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		slog.Warn("replay_context_write_failed", "error", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		slog.Warn("replay_context_rename_failed", "error", err)
	}
}

// ReplayContextPath is the on-disk location of the hint for a workspace, so the
// spawned agent's startup routine can read + delete it.
func ReplayContextPath(workspace string) string {
	return filepath.Join(workspace, markerDir, replayContextFile)
}
