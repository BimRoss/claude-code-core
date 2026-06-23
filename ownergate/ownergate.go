// Package ownergate enforces single-owner mention-gated mode for personal
// agents. A personal agent (Garth, future per-user agents) responds only to
// its configured owner: DMs and @-mentions from the owner pass, everything
// else is silently dropped or politely refused.
//
// Decisions are returned as Decision values so the dispatcher can also
// update thread-owner state (claim a thread, release it) atomically with
// the dispatch decision.
//
// Distinct from claude-code-core/threadowner, which is thread-level routing
// (which agent owns this thread). This module is agent-level access control.
package ownergate

import (
	"regexp"
	"strings"

	"github.com/slack-go/slack/slackevents"
)

// Decision is what Check returns for one inbound message. Behavior per
// decision is described in the const block below.
type Decision int

const (
	// Pass: dispatcher continues to spawn. No thread-owner change.
	Pass Decision = iota
	// PassClaim: spawn AND record the agent as the thread owner so
	// follow-up replies in the same thread route back here. Fired on
	// channel @-mentions of the agent.
	PassClaim
	// SilentDrop: drop without reply, no ownership change. Owner chatter
	// without explicit address, non-owner background chatter, or thread
	// replies we don't own.
	SilentDrop
	// SilentDropRelease: drop without reply AND clear our claim on the
	// thread. Fired when the operator @-mentions someone else in a thread
	// we owned — the standard hand-off pattern.
	SilentDropRelease
	// Reject: a non-owner @-mentioned us or DM'd us. Drop without reply.
	// Distinct from SilentDrop so dispatchers can log the address attempt
	// separately for telemetry.
	Reject
)

// slackMentionPattern matches Slack's wire format for an @-mention:
// <@USERID> or <@USERID|displayname>. Group 1 is the user ID.
var slackMentionPattern = regexp.MustCompile(`<@([A-Z0-9_]+)(?:\|[^>]*)?>`)

// MentionsUser reports whether text contains a Slack @-mention of userID.
// Slack renders mentions as `<@USERID>` in raw event text. Empty userID
// returns false so an unconfigured bot user ID can't accidentally match
// every message.
func MentionsUser(text, userID string) bool {
	if userID == "" {
		return false
	}
	return strings.Contains(text, "<@"+userID+">")
}

// mentionsAnyOther reports whether the text contains a Slack @-mention of
// any user other than selfUserID. Used to detect when the operator hands a
// thread off by addressing someone else.
func mentionsAnyOther(text, selfUserID string) bool {
	for _, m := range slackMentionPattern.FindAllStringSubmatch(text, -1) {
		if len(m) >= 2 && m[1] != selfUserID {
			return true
		}
	}
	return false
}

// Check enforces single-owner mention-gated mode for personal agents.
//
// When ownerSlackUserID is empty, returns Pass unconditionally — agents
// running without an owner (e.g. Ross/Joanne in default channel-responder
// mode) keep their existing behavior. This is a no-op fallback;
// personal-agent pods are expected to always set it.
//
// When set:
//   - DMs from the owner always pass (no one else to compete with there).
//   - Channel @-mention of self → pass + claim the thread.
//   - Channel @-mention of someone else (in a thread we owned) → silent drop
//     + release ownership. The operator is handing the thread off.
//   - Channel thread reply with no mention → pass iff we already own the
//     thread, else silent drop.
//   - Channel top-level message with no mention → silent drop. The agent
//     doesn't barge into general chatter; the operator must @-mention to
//     start a conversation.
//   - Non-owner DM or @-mention → friendly rejection.
//   - Non-owner background chatter → silent drop.
func Check(ownerSlackUserID, botUserID string, ev *slackevents.MessageEvent, ownsThread bool) Decision {
	if ownerSlackUserID == "" {
		return Pass
	}
	isDM := ev.ChannelType == "im" || ev.ChannelType == "mpim"
	mentionsSelf := MentionsUser(ev.Text, botUserID)
	mentionsOther := mentionsAnyOther(ev.Text, botUserID)
	inThread := ev.ThreadTimeStamp != ""

	if ev.User != ownerSlackUserID {
		if isDM || mentionsSelf {
			return Reject
		}
		return SilentDrop
	}

	// Owner authored.
	if isDM {
		return Pass
	}
	if mentionsSelf {
		return PassClaim
	}
	if mentionsOther {
		if inThread && ownsThread {
			return SilentDropRelease
		}
		return SilentDrop
	}
	if inThread && ownsThread {
		return Pass
	}
	return SilentDrop
}
