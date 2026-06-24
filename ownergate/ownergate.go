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

// TeamOptions carries the additional, caller-resolved inputs that enable
// "team agent" mode — an owner-owned agent that ALSO responds to non-owner
// members of channels the owner belongs to.
//
// FAIL-CLOSED CONTRACT. Membership resolution is the caller's job, not the
// gate's: the caller queries the Slack API for the owner's channel
// membership and passes the result here. The gate trusts the bool verbatim.
// Therefore the caller MUST pass OwnerInChannel=false on ANY uncertainty —
// Slack API error, timeout, rate limit, missing scope, DM (no channel to
// check), or anything it cannot positively confirm. A false here collapses
// team mode back to strict single-owner behavior, so an unverifiable
// owner-membership can never admit a stranger.
//
// Team mode NEVER relaxes DMs. A DM has no channel membership to verify, so
// non-owner DMs are rejected exactly as in strict mode regardless of Enabled
// or OwnerInChannel.
type TeamOptions struct {
	// Enabled turns team mode on. When false, CheckTeam is identical to
	// Check (strict single-owner behavior); the other fields are ignored.
	Enabled bool
	// OwnerInChannel reports whether the configured owner is a member of the
	// channel this message arrived in. Caller-resolved; see the fail-closed
	// contract above. Ignored for DMs and when Enabled is false.
	OwnerInChannel bool
}

// Check enforces single-owner mention-gated mode for personal agents.
//
// This is the original, strict entry point. Its signature and behavior are
// frozen: it is imported by claude-code-ross, claude-code-joanne, and
// claude-code-personal-agent, and must keep compiling and behaving
// identically. It delegates to checkTeam with team mode OFF; team-agent
// callers use CheckTeam.
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
	return checkTeam(ownerSlackUserID, botUserID, ev, ownsThread, TeamOptions{})
}

// CheckTeam is the team-agent entry point. With opts.Enabled == false it is
// byte-for-byte equivalent to Check (strict single-owner mode), so it is a
// safe drop-in. With opts.Enabled == true it additionally admits NON-owner
// members of channels the owner belongs to — but only on the same
// mention-gated terms the owner gets, and only when opts.OwnerInChannel is
// true (the fail-closed gate; see TeamOptions).
//
// Team-mode additions over strict mode (only when Enabled && OwnerInChannel,
// and only for a NON-owner sender in a CHANNEL — never a DM):
//   - Channel @-mention of self → PassClaim (claim the thread, exactly like
//     the owner address path). This is the only way a non-owner starts a
//     conversation; team mode does not barge in.
//   - Channel thread reply with no mention, in a thread we already own →
//     Pass. Follow-ups in a claimed thread continue.
//   - Channel top-level message with no mention → SilentDrop. No barge-in;
//     an @-mention is required to start, same as the owner rule.
//
// Everything else is unchanged from strict mode:
//   - The OWNER always behaves exactly as in Check, regardless of team mode.
//   - Non-owner DMs are always rejected (team mode never relaxes DMs).
//   - When OwnerInChannel is false (the fail-closed path the caller takes on
//     any Slack-API error or uncertainty), non-owner traffic falls through
//     to strict behavior: SilentDrop for chatter, Reject for a direct
//     address. A stranger is never admitted on an unverifiable membership.
func CheckTeam(ownerSlackUserID, botUserID string, ev *slackevents.MessageEvent, ownsThread bool, opts TeamOptions) Decision {
	return checkTeam(ownerSlackUserID, botUserID, ev, ownsThread, opts)
}

// checkTeam is the shared predicate. Check passes a zero TeamOptions (team
// mode off); CheckTeam passes the caller's options. Keeping a single
// implementation guarantees Check and team-off CheckTeam can never diverge.
func checkTeam(ownerSlackUserID, botUserID string, ev *slackevents.MessageEvent, ownsThread bool, opts TeamOptions) Decision {
	if ownerSlackUserID == "" {
		return Pass
	}
	isDM := ev.ChannelType == "im" || ev.ChannelType == "mpim"
	mentionsSelf := MentionsUser(ev.Text, botUserID)
	mentionsOther := mentionsAnyOther(ev.Text, botUserID)
	inThread := ev.ThreadTimeStamp != ""

	if ev.User != ownerSlackUserID {
		// Team mode admits non-owners ONLY in a channel the owner is in, and
		// ONLY on the same mention-gated terms the owner gets. DMs are never
		// relaxed (no channel membership to verify); OwnerInChannel=false is
		// the fail-closed path → fall through to strict rejection below.
		if opts.Enabled && opts.OwnerInChannel && !isDM {
			if mentionsSelf {
				return PassClaim
			}
			if inThread && ownsThread {
				return Pass
			}
			// Top-level, unowned thread, or mention-of-other only: no
			// barge-in. An @-mention of self is required to start.
			return SilentDrop
		}
		// Strict mode (or team mode that cannot admit this message).
		if isDM || mentionsSelf {
			return Reject
		}
		return SilentDrop
	}

	// Owner authored. Unchanged by team mode.
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
