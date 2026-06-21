// Package silentnarration is the shared runtime safety net for the
// "when to send no reply, write nothing" rule documented in
// [replypolicy.NarrationRules]. The prompt-side rule is the primary fix —
// this package is the backstop that catches the model when it writes a
// short meta-commentary line instead of staying silent.
//
// LooksLike(s) returns true if s is short, single-line self-narration about
// the model's choice not to reply (e.g. "Staying silent.", "Not addressed
// to me.", "This message is for Grant, not me. No reply."). Each consuming
// agent's reply gate calls LooksLike on the model's stdout and drops the
// post if it matches. The wired-up callsites currently live in
// cmd/{ross,joanne,personal-agent}/handlers.go.
//
// Before this package existed, the regex catalog was triplicated across
// ross/cmd, joanne/cmd, and personal-agent/cmd as `silent_narration.go`,
// each with the same "ported from ross" comment and slowly drifting
// patterns. Per the 2026-06-17 promote-to-core audit, the catalog moves
// here and the agent repos import it.
//
// The catalog grows by incident — the comment on each pattern block names
// the incident that surfaced the shape. Add a new shape only when a leak
// actually happened in the wild; the prompt-side rule should be the first
// fix, and this regex list is for shapes the model keeps re-emitting
// despite the prompt.
package silentnarration

import (
	"regexp"
	"strings"
)

// MaxChars is the upper bound at which we still consider a reply "short
// enough to plausibly be self-narration." Real replies that incidentally
// contain a matching phrase (e.g. a long deploy summary that mentions "no
// response needed yet from CI") get a pass.
const MaxChars = 200

// patterns is the allowlist of self-referential meta-commentary the model
// sometimes emits when it should have exited silently. Each pattern is
// anchored to a phrase that has no business appearing in a genuine reply.
var patterns = []*regexp.Regexp{
	// Original #263 catalog — the most common shapes.
	regexp.MustCompile(`(?i)\bstaying\s+(silent|quiet)\b`),
	regexp.MustCompile(`(?i)\bi(?:'ll| will)\s+stay\s+(silent|quiet)\b`),
	regexp.MustCompile(`(?i)\bnot\s+addressed\s+to\s+me\b`),
	regexp.MustCompile(`(?i)\bno\s+(response|reply)\s+(needed|requested|warranted|required)\b`),
	regexp.MustCompile(`(?i)\bnothing\s+to\s+add\b`),
	regexp.MustCompile(`(?i)\bsilent\s+exit\b`),
	regexp.MustCompile(`(?i)\bexiting\s+silent\b`),
	regexp.MustCompile(`(?i)\bno\s+action\s+(needed|required)\s+from\s+me\b`),
	regexp.MustCompile(`(?i)\bremaining\s+(silent|quiet)\b`),
	regexp.MustCompile(`(?i)\bno\s+response\s+requested\b`),
	// ross#299 — "Staying out." / "This message is addressed to <person>"
	// shapes that escaped the original allowlist.
	regexp.MustCompile(`(?i)\bstaying\s+out\b`),
	regexp.MustCompile(`(?i)\bthis\s+message\s+is\s+addressed\s+to\b`),
	// Garth Franster thread (2026-06-16) — Ross posted the literal
	// `<@U_GARTH> is being asked, not me. Empty stdout.` Catch both the
	// "is being asked, not me" framing and the "Empty stdout" instruction-
	// marker echo as independent leaks.
	regexp.MustCompile(`(?i)\bis\s+being\s+asked,?\s+not\s+me\b`),
	regexp.MustCompile(`(?i)\bempty\s+stdout\b`),
	// ross#295 — watcher-tick leaks. Internal phase-machine vocabulary that
	// has no business landing in a Slack thread.
	regexp.MustCompile(`(?i)\banchor\s+updated\b`),
	regexp.MustCompile(`(?i)\bphase\s+(advanced|unchanged)\b`),
	// Tnarg Retsof thread (2026-06-18, C0B6SB6UA4E, ross#427) — Ross posted
	// four sentences of meta-narration about who the message was really
	// for. Each shape catches independently.
	regexp.MustCompile(`(?i)\bnot\s+for\s+me\b`),
	regexp.MustCompile(`(?i)\bmy\s+(bot|user)\s+id\s+is\b`),
	regexp.MustCompile(`(?i)\bthe\s+message\s+(tags|mentions|is\s+for)\b`),
	// joanne#-silent-exit-parity (2026-06-18): Joanne leaked
	// "ha, you three carry on — just lurking." and a "stepping out, <@x>
	// all yours" step-out when she moved to full silent-exit parity.
	regexp.MustCompile(`(?i)\blurking\b`),
	regexp.MustCompile(`(?i)\bstepping\s+(out|back)\b`),
	// 2026-06-21 (ross#439, mac-thread C0B5W8L5744) — Ross leaked
	// "This message is for Grant, not me. No reply." and "Nothing for me
	// to add." The existing patterns required "the message" (mine said
	// "this"), a follow-up word after "no reply" (mine was bare), and
	// "nothing to add" with no words between (mine had "for me" in the
	// middle).
	regexp.MustCompile(`(?i)\bthis\s+message\s+is\s+for\b`),
	regexp.MustCompile(`(?i)\bno\s+(response|reply)\b\.?$`),
	regexp.MustCompile(`(?i)\bnothing\s+(for\s+me\s+)?to\s+add\b`),
}

// LooksLike reports whether s is short, single-line meta-commentary about
// the model's choice not to reply. When true, the harness should suppress
// the post rather than letting the narration leak into Slack.
//
// The detector intentionally errs on the side of letting messages through:
// false positives are worse than false negatives, since the prompt-side
// rule (see [replypolicy.NarrationRules]) is the primary fix and this is
// just a safety net.
func LooksLike(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > MaxChars {
		return false
	}
	// Multi-line replies are almost always real content. Self-narration is
	// a single declarative sentence.
	if strings.ContainsAny(s, "\n") {
		return false
	}
	for _, re := range patterns {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}
