// Package replypolicy is the single source of truth for the "when to reply /
// when not to reply" instruction block shared by the BimRoss Slack agents
// (Ross, Joanne, and the per-user personal agents).
//
// It exists because that block drifted. The personal agent reused Ross/Joanne's
// framing — "the harness spawns you for *every* message, decide for yourself
// whether it's for you" — which is true for a shared channel-responder but
// false for an owner-gated personal agent, whose dispatcher has *already*
// decided the message is addressed before the model runs. Fed a large
// <channel-context> block, the personal agent over-applied the silence rules
// and went silent even on a direct owner @-mention (the channel silent-exit
// bug, 2026-06-19).
//
// The fix is to make the gated-vs-open distinction a parameter, not a
// copy-paste. Each agent renders Section(mode) for its mode; the shared
// narration-leak rules live in exactly one place so they can't fall out of
// sync across agents.
package replypolicy

import "strings"

// Mode selects which reply policy an agent follows.
type Mode int

const (
	// Owned is a single-owner personal agent backed by an owner-gated
	// dispatcher (ownergate). The dispatcher admits only owner DMs, owner
	// @-mentions, and follow-ups in threads the agent already owns; it
	// silently drops everything else *before* the model is spawned. So an
	// Owned agent must NOT re-litigate whether a message is for it — that
	// judgment is already made, and an owner address always warrants a
	// substantive reply.
	Owned Mode = iota
	// Open is a shared channel-responder (Ross, Joanne) spawned for every
	// non-bot message in the channels it sits in. It genuinely must decide,
	// per message, whether the turn is for it, and empty stdout is the
	// legitimate "not for me" signal.
	Open
)

// narrationRules is the wording-independent contract shared by both modes:
// the way to send no reply is to write nothing — never narrate the decision.
// The harness can't tell narration from a real reply, so any non-empty stdout
// becomes a Slack post. Kept in one place so the leak-phrase list stays
// identical across all agents (it grows by incident; divergence is how leaks
// slip back in on one agent after being fixed on another).
const narrationRules = "The way to send no reply is to **write nothing.** Empty stdout, no post. Don't announce it, don't explain it, don't emit a placeholder. Lines like \"Staying silent.\", \"Not addressed to me.\", \"Not for me.\", \"No response needed.\", \"Nothing to add.\", \"Stepping back.\", \"Stepping out.\", \"just lurking.\", or a bare \"Human:\" — and any longer narration that names your own bot ID or paraphrases who a message tags (\"The message tags <@OTHER>, not me.\", \"My bot ID is …\") — are the exact leaks this rule exists to prevent. If you find yourself starting to type one, stop, delete the line, and emit nothing. The harness also suppresses common self-narration phrases at the reply gate as a backstop, but don't lean on it — the right move is to not write the line."

// ownedSection is the reply policy for an owner-gated personal agent. It leads
// with the positive obligation (the gate already decided; reply) precisely
// because the failure mode is the model talking itself *out* of a reply it
// owes.
const ownedSection = `## When to reply (read this first)
**You are owner-gated. The dispatcher already decided this message is for you before it spawned you** — it only spawns you on a DM from your owner, an @-mention of you by your owner, or a follow-up in a thread you already own. It silently drops everything else *before* you ever run. So unlike a shared channel-bot, you are **not** here to second-guess whether a message is addressed to you. That judgment is already made.

**Therefore: if your owner @-mentioned you or DM'd you, you MUST write a substantive reply. Always. A direct @-mention is the strongest possible "this is for you" signal — never answer one with empty stdout.** This holds in a channel exactly as much as in a DM; the ` + "`<channel-context>`" + ` block is background for *what's been discussed*, not a reason to stay quiet. If a turn arrives with a ` + "`[address: ...]`" + ` preamble, that is the harness telling you the dispatcher's decision — trust it and answer. If you ever find yourself about to emit nothing in response to an owner @-mention or DM, that is the bug — answer the question instead.

## When not to reply
The silence cases below are narrow exceptions, and **none of them apply to a direct owner @-mention or DM.** ` + narrationRules + `

- **Send no reply** when:
  - The message is a harness continue-ping like ` + "`Continue from where you left off.`" + ` with no other content.
  - It's side chatter from another human or a peer bot that doesn't address you and isn't in a thread you own. (Your owner @-mentioning you is never side chatter — that always warrants a reply.)
  - It's a system join/leave/topic notice that doesn't introduce a new human.`

// openSection is the reply policy for a shared channel-responder. Here the
// model legitimately owns the addressivity decision, so the framing leads with
// that responsibility.
const openSection = `## When not to reply
The harness spawns you for *every* non-bot message in the surfaces you're in, including ones that aren't really for you. Deciding *not* to reply is a routine, correct outcome — and the way you do it is simple: write nothing to stdout. ` + narrationRules + `

- **Send no reply** when:
  - The message doesn't @-mention you and isn't in a thread you're already part of — it's side chatter between other people.
  - The message @-mentions a *different* agent and your name doesn't appear in it. (A dual-mention that includes you means reply.)
  - The message is a harness continue-ping like ` + "`Continue from where you left off.`" + ` with no other content.
  - It's a system join/leave/topic notice that doesn't introduce a new human.
- **Do reply** when you're @-mentioned, DM'd, or the message lands in a thread you're already participating in.`

// NarrationRules returns the wording-independent "the way to send no reply is
// to write nothing — never narrate the decision" contract, including the
// leak-phrase list. It's exported so agents that keep a bespoke "when not to
// reply" policy (Ross/Joanne carry agent-specific dual-mention + peer rules)
// can still source this one paragraph from here, keeping the leak-phrase list
// — which grows by incident — identical across every agent. Section() embeds
// the same text, so PA (via Section(Owned)) and Ross/Joanne (via this) share
// one copy.
func NarrationRules() string {
	return narrationRules
}

// Section returns the canonical "when to reply / when not to reply"
// instruction block for the given mode, ready to drop into an agent's system
// prompt. Both variants embed the shared narration-leak rules.
func Section(m Mode) string {
	switch m {
	case Owned:
		return ownedSection
	case Open:
		return openSection
	default:
		return openSection
	}
}

// MentionsRepliesMandate reports whether s contains the hard "owner address
// always warrants a reply" obligation that defines Owned mode. Consumers use
// it in a regression test so the mandate can't silently drift out of a
// rendered prompt. Kept as a method on the package (not the consumer) so the
// canonical phrasing and its guard travel together.
func MentionsRepliesMandate(s string) bool {
	return strings.Contains(s, "you MUST write a substantive reply") &&
		strings.Contains(s, "never answer one with empty stdout")
}
