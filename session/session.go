// Package session is the shared Slack-message → Claude-session mapping for all
// three agents (Ross, Joanne, personal-agent).
//
// The mapping (consensus 2026-06-25):
//   - A top-level message starts a NEW session, keyed by its own ts.
//   - A threaded reply RESUMES that session, keyed by the thread root ts.
//     (A thread's root has ts == thread_ts, so a top-level message and every
//     reply under it derive the SAME UUID — that's how a thread resumes.)
//   - Loop ticks share ONE session per loop, so successive ticks --resume and
//     inherit prior-tick memory.
//
// This is UNIFORM across surfaces: DMs behave exactly like channels (the old
// "DM as one rolling session" model is gone). It only holds because the agents
// reply in-thread by default — a top-level reply would fork a new session.
//
// Two correctness properties this package centralizes:
//   - Persona-aware namespace: a hash of the agent's instructions/persona is
//     mixed into the namespace so a persona change yields fresh UUIDs — the old
//     JSONL no longer matches, the agent starts clean instead of --resume-ing
//     onto a transcript whose demonstrated behavior the model anchors on.
//   - Workspace-scoped resume detection: the --session-id vs --resume decision
//     stats the JSONL under the project dir for the SPECIFIC workspace claude
//     runs in, never a glob across all projects — a stale JSONL from a different
//     workspace must not be matched and handed back as an unsatisfiable --resume.
package session

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// loopThreadKeyPrefix namespaces loop-driven session keys so they cannot
// collide with any Slack ts (Slack ts are dotted decimals like
// "1716000000.000100"; "loop:<id>" never parses as one).
const loopThreadKeyPrefix = "loop:"

// Namespace builds the namespace mixed into every derived UUID: a stable
// per-agent prefix plus a short hash of the agent's persona/instructions text.
//
// Mixing the persona hash in is the auto-invalidation lever: when the
// instructions change, every (channel, thread) pair derives a new UUID, the old
// JSONL stops matching, and the next spawn starts fresh against the new
// instructions instead of replaying the old transcript. Pass the agent's full
// instructions/persona string as personaText (for per-user agents, include the
// per-user system prompt so an owner's persona edit takes effect immediately).
// personaText may be empty for an agent with no dynamic persona — the namespace
// then reduces to the bare prefix.
func Namespace(prefix, personaText string) string {
	if personaText == "" {
		return prefix
	}
	sum := sha256.Sum256([]byte(personaText))
	return prefix + hex.EncodeToString(sum[:4]) + ":"
}

// Deriver maps Slack coordinates to a deterministic Claude session UUID under a
// fixed namespace. Construct once per spawn (or cache) with New.
type Deriver struct {
	namespace string
}

// New returns a Deriver for the given namespace (build it via Namespace).
func New(namespace string) *Deriver { return &Deriver{namespace: namespace} }

// UUID produces a deterministic UUIDv5-shaped string from the (channel,
// threadKey) pair under the Deriver's namespace. Same pair → same UUID across
// spawns, which is what lets a thread resume its own session. The format is a
// syntactically valid RFC 4122 v5 UUID; Claude Code's --session-id flag only
// checks syntax, not provenance.
func (d *Deriver) UUID(channelID, threadKey string) string {
	sum := sha1.Sum([]byte(d.namespace + channelID + ":" + threadKey))
	sum[6] = (sum[6] & 0x0f) | 0x50 // version 5
	sum[8] = (sum[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x",
		sum[0:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
}

// Key derives the session UUID for an inbound message. Uniform across surfaces
// (channels and DMs alike):
//
//   - loopID set        → one session per loop (all ticks share it).
//   - top-level message → keyed by its own ts (a NEW session).
//   - threaded reply    → keyed by the thread root ts (RESUMES the session).
func (d *Deriver) Key(channelID, threadTS, messageTS, loopID string) string {
	if loopID != "" {
		return d.UUID(channelID, loopThreadKeyPrefix+loopID)
	}
	if threadTS == "" {
		return d.UUID(channelID, messageTS)
	}
	return d.UUID(channelID, threadTS)
}

// ProjectSlug mirrors Claude Code's project-dir naming: the cwd with every path
// separator replaced by '-', preceded by a leading '-'. E.g.
// "/data/workspaces/C0B5W8L5744" → "-data-workspaces-C0B5W8L5744". Predicting
// this is brittle to Claude Code internal changes, but the cost of NOT scoping
// (matching a JSONL under another workspace's project dir and handing claude a
// --resume it cannot satisfy) is the bug this prevents. Single line to update
// if the naming convention changes.
func ProjectSlug(workspace string) string {
	clean := filepath.Clean(workspace)
	trimmed := strings.TrimPrefix(clean, string(filepath.Separator))
	return "-" + strings.ReplaceAll(trimmed, string(filepath.Separator), "-")
}

// ResolveFlag decides whether claude should be spawned with --session-id
// (create-new) or --resume (continue-existing). Claude Code rejects
// --session-id on a UUID that already has a JSONL ("Session ID is already in
// use"), so the flag must switch once the first turn has persisted.
//
// Detection is scoped to the project dir for THIS workspace (not a glob across
// all projects), so a stale JSONL written from a different cwd is never matched.
func ResolveFlag(home, workspace, sessionUUID string) (flag, id string) {
	jsonl := filepath.Join(home, ".claude", "projects", ProjectSlug(workspace), sessionUUID+".jsonl")
	if _, err := os.Stat(jsonl); err == nil {
		return "--resume", sessionUUID
	}
	return "--session-id", sessionUUID
}

// ResolveFlagForWorkspace is the production entry point: it reads $HOME and
// falls back to "--session-id" if the lookup fails (better a fresh session than
// a crash).
func ResolveFlagForWorkspace(workspace, sessionUUID string) (flag, id string) {
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return "--session-id", sessionUUID
	}
	return ResolveFlag(home, workspace, sessionUUID)
}

// IsLockRace detects the "Session ID <uuid> is already in use" error Claude
// Code prints when a --resume fires before the prior process's internal
// lockfile has released — a recoverable sub-second race, not a real crash. On
// true the caller should log warn and swallow it (no raw stderr to Slack, no
// notice); the operator re-sends.
func IsLockRace(stderr string, waitErr error) bool {
	hay := stderr
	if waitErr != nil {
		hay += " " + waitErr.Error()
	}
	return strings.Contains(hay, "is already in use")
}
