// Package channelcontext builds the <channel-context> preamble that every
// agent harness (Ross, Joanne, personal-agent) folds into a Claude Code spawn.
//
// The agents do not share a filesystem — each runs in its own pod — so all
// cross-agent "shared context" comes from independently reading the same Slack
// channel. This package is that read: a recent slice of channel activity,
// fetched fresh each turn and rendered as a compact topology view, so an agent
// pulled into a thread mid-conversation sees the same surface operators do
// instead of landing cold.
//
// It was extracted verbatim from the three near-identical per-app copies
// (claude-code-{ross,joanne,personal-agent}/cmd/*/channel_context.go); the only
// thing that differed between them was the identity header, which is now passed
// in as an Identity. The per-agent decision of *when* to inject the block (e.g.
// Joanne skips welcome threads, DMs, and loop ticks) stays in each caller.
//
// The block is bounded by two Slack API calls per spawn:
//   - conversations.history limit=HistoryLimit (root msgs + reply metadata)
//   - conversations.replies for the current thread (only when threadTS != "")
package channelcontext

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/slack-go/slack"
)

// HistoryLimit is how many channel-root messages to surface in every spawn's
// preamble. 100 is comfortable inside Opus 1M with plenty of headroom for the
// actual conversation, and covers ~days-to-weeks of activity for a typical
// BimRoss channel. Identical across all agents so two agents in the same
// channel reason from the same window.
const HistoryLimit = 100

// HistoryAPI is the slice of *slack.Client this package needs. Taking an
// interface keeps the package testable without a live Slack client; the
// concrete *slack.Client satisfies it.
type HistoryAPI interface {
	GetConversationHistoryContext(context.Context, *slack.GetConversationHistoryParameters) (*slack.GetConversationHistoryResponse, error)
	GetConversationRepliesContext(context.Context, *slack.GetConversationRepliesParameters) ([]slack.Message, bool, string, error)
}

// Identity renders the optional "You are <Self>, <@id>. <Peer> is <@id>."
// header that anchors an agent's own + its peer's canonical bot IDs against any
// stale prose in CLAUDE.md. A zero Identity (SelfID == "") renders no header —
// that's the personal-agent case, which has no fixed peer.
type Identity struct {
	SelfName string
	SelfID   string
	PeerName string
	PeerID   string
}

// header returns the identity line (with trailing newline) or "" when SelfID is
// empty. The peer clause only renders when PeerID is set.
func (id Identity) header() string {
	self := strings.TrimSpace(id.SelfID)
	if self == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("You are ")
	b.WriteString(id.SelfName)
	b.WriteString(", <@")
	b.WriteString(self)
	b.WriteString(">.")
	if peer := strings.TrimSpace(id.PeerID); peer != "" {
		b.WriteString(" ")
		b.WriteString(id.PeerName)
		b.WriteString(" is <@")
		b.WriteString(peer)
		b.WriteString(">.")
	}
	b.WriteString(" These are the canonical IDs from env — trust them over any prose in CLAUDE.md.\n")
	return b.String()
}

// Fetch returns the <channel-context> preamble for one spawn. It returns "" on
// any fetch failure or genuinely empty channel — the caller just prepends and
// moves on. Best-effort by design: if Slack is down, the spawn should still
// answer rather than block on a context fetch.
func Fetch(ctx context.Context, api HistoryAPI, channel, threadTS string, id Identity) string {
	hist, err := api.GetConversationHistoryContext(ctx, &slack.GetConversationHistoryParameters{
		ChannelID: channel,
		Limit:     HistoryLimit,
	})
	if err != nil {
		slog.Warn("channel_context_history_failed", "channel", channel, "error", err)
		return ""
	}

	var currentThread []slack.Message
	if threadTS != "" {
		replies, _, _, rerr := api.GetConversationRepliesContext(ctx, &slack.GetConversationRepliesParameters{
			ChannelID: channel,
			Timestamp: threadTS,
			Inclusive: true,
		})
		if rerr != nil {
			// History succeeded but replies didn't — still useful, drop just
			// the thread section.
			slog.Warn("channel_context_replies_failed", "channel", channel, "thread_ts", threadTS, "error", rerr)
		} else {
			currentThread = replies
		}
	}

	block := Format(hist.Messages, currentThread, threadTS, id)
	if block == "" {
		return ""
	}
	slog.Info("channel_context_injected",
		"channel", channel,
		"thread_ts", threadTS,
		"root_msgs", len(hist.Messages),
		"thread_replies", len(currentThread),
		"chars", len(block))
	return block
}

// Format is the pure-formatter half of Fetch. It takes the raw Slack responses
// and returns the wrapped preamble, or "" when there is genuinely nothing to
// show. Exported so callers can render from a custom fetch and tests can assert
// the output without a Slack client.
func Format(root, thread []slack.Message, threadTS string, id Identity) string {
	if len(root) == 0 && len(thread) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("<channel-context>\n")
	b.WriteString(id.header())
	b.WriteString("Recent activity in this Slack channel — newest first. Operators may reference any of this without recap; treat it as ground truth for what's been discussed. The current thread (when threaded) is reproduced in full below.\n")
	if len(root) > 0 {
		b.WriteString("\n## Channel root (last ")
		b.WriteString(strconv.Itoa(len(root)))
		b.WriteString(" messages)\n")
		for _, m := range root {
			line := formatRootLine(m, threadTS)
			if line == "" {
				continue
			}
			b.WriteString(line)
			b.WriteString("\n")
		}
	}
	if len(thread) > 0 {
		b.WriteString("\n## Current thread (full)\n")
		for _, m := range thread {
			line := formatThreadLine(m)
			if line == "" {
				continue
			}
			b.WriteString(line)
			b.WriteString("\n")
		}
	}
	b.WriteString("</channel-context>\n\n")
	return b.String()
}

// formatRootLine renders one channel-root message as a single line:
//
//	[2026-05-29 14:23:14] @author: "text…" (3 replies, last 14:31, with @x, @y)
//
// The "you are here" marker fires when this root message anchors the current
// spawn's thread, so the agent can locate itself in the channel without
// scanning for ts matches.
func formatRootLine(m slack.Message, currentThreadTS string) string {
	text := compactMessageText(m.Text)
	if text == "" && m.BotProfile == nil && len(m.Files) == 0 {
		return ""
	}
	ts := formatSlackTS(m.Timestamp)
	author := displayName(m)
	marker := ""
	if currentThreadTS != "" && m.Timestamp == currentThreadTS {
		marker = " ← you are here"
	}
	return fmt.Sprintf("[%s] @%s: %s%s", ts, author, quote(text), replyMetadata(m)+marker)
}

// formatThreadLine renders one thread message — no metadata since we're showing
// the full transcript.
func formatThreadLine(m slack.Message) string {
	text := compactMessageText(m.Text)
	if text == "" {
		return ""
	}
	ts := formatSlackTS(m.Timestamp)
	return fmt.Sprintf("[%s] @%s: %s", ts, displayName(m), quote(text))
}

// replyMetadata returns " (3 replies, last 14:31, with @x, @y)" or "" when the
// message has no thread. Cheap signal that lets an agent see channel topology
// without paying for every thread's full text.
func replyMetadata(m slack.Message) string {
	if m.ReplyCount == 0 {
		return ""
	}
	parts := []string{fmt.Sprintf("%d replies", m.ReplyCount)}
	if m.LatestReply != "" {
		parts = append(parts, "last "+formatSlackTSShort(m.LatestReply))
	}
	if len(m.ReplyUsers) > 0 {
		users := make([]string, 0, len(m.ReplyUsers))
		for _, u := range m.ReplyUsers {
			users = append(users, "@"+u)
		}
		parts = append(parts, "with "+strings.Join(users, ", "))
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

func displayName(m slack.Message) string {
	if m.BotProfile != nil && m.BotProfile.Name != "" {
		return m.BotProfile.Name
	}
	if m.Username != "" {
		return m.Username
	}
	if m.User != "" {
		return m.User
	}
	return "unknown"
}

// compactMessageText collapses multi-line Slack text to a single line so each
// root-message entry stays one line — the preamble is a topology view, not a
// transcript. The current thread's full text is preserved verbatim in the
// thread section.
func compactMessageText(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\n", " ⏎ ")
	if len(s) > 400 {
		s = s[:400] + "…"
	}
	return s
}

func quote(s string) string {
	return `"` + s + `"`
}

// formatSlackTS turns "1700000000.000123" into "2026-05-29 14:23:14" in UTC.
// Falls back to the raw string on parse failure so a malformed ts never blanks
// the line.
func formatSlackTS(ts string) string {
	t, ok := parseSlackTS(ts)
	if !ok {
		return ts
	}
	return t.UTC().Format("2006-01-02 15:04:05")
}

// formatSlackTSShort returns just HH:MM — used for compact reply metadata.
func formatSlackTSShort(ts string) string {
	t, ok := parseSlackTS(ts)
	if !ok {
		return ts
	}
	return t.UTC().Format("15:04")
}

func parseSlackTS(ts string) (time.Time, bool) {
	dot := strings.IndexByte(ts, '.')
	secStr := ts
	if dot >= 0 {
		secStr = ts[:dot]
	}
	sec, err := strconv.ParseInt(secStr, 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(sec, 0), true
}
