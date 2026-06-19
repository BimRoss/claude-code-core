package replypolicy

import (
	"strings"
	"testing"
)

func TestOwnedSectionMandatesReply(t *testing.T) {
	s := Section(Owned)
	// The defining contract of Owned mode: an owner address always warrants a
	// substantive reply, never empty stdout. This is the regression guard for
	// the channel silent-exit bug (2026-06-19).
	for _, want := range []string{
		"you MUST write a substantive reply",
		"never answer one with empty stdout",
		"already decided this message is for you",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("Owned section missing %q", want)
		}
	}
	if !MentionsRepliesMandate(s) {
		t.Error("MentionsRepliesMandate(Owned) = false, want true")
	}
}

func TestOwnedSectionDoesNotInheritOpenPremise(t *testing.T) {
	// The exact false premise that caused the bug: Owned must NOT tell the
	// model it's spawned for every message and has to decide addressivity.
	s := Section(Owned)
	for _, banned := range []string{
		"spawns you for *every*",
		"decide for yourself",
	} {
		if strings.Contains(s, banned) {
			t.Errorf("Owned section leaked Open premise: %q", banned)
		}
	}
}

func TestOpenSectionKeepsSelfDecideFraming(t *testing.T) {
	s := Section(Open)
	if !strings.Contains(s, "every*") {
		t.Error("Open section should keep the 'spawned for every message' framing")
	}
	// Open mode must NOT carry the owner-gated mandate — it has no single owner.
	if MentionsRepliesMandate(s) {
		t.Error("Open section should not carry the owner-address reply mandate")
	}
}

func TestBothModesShareNarrationRules(t *testing.T) {
	owned, open := Section(Owned), Section(Open)
	// The shared narration-leak contract must appear verbatim in both, from
	// the single source — that's the whole point of factoring it out.
	probe := "The way to send no reply is to **write nothing.**"
	if !strings.Contains(owned, probe) {
		t.Error("Owned section missing shared narration rules")
	}
	if !strings.Contains(open, probe) {
		t.Error("Open section missing shared narration rules")
	}
	if !strings.Contains(owned, "just lurking.") || !strings.Contains(open, "just lurking.") {
		t.Error("leak-phrase list must be identical (single source) across modes")
	}
}

func TestNarrationRulesIsSupersetOfAllAgents(t *testing.T) {
	// The shared list must be a superset of every agent's historical
	// leak-phrase coaching so migrating any agent onto it loses no example.
	// PA: "just lurking"/"stepping out"; Ross: "I'll stay quiet on this one";
	// Joanne: "carry on"/"all yours".
	nr := NarrationRules()
	for _, phrase := range []string{
		"Staying silent.",
		"Not for me.",
		"I'll stay quiet on this one.",
		"just lurking.",
		"all yours.",
		"carry on",
		"My bot ID is",
	} {
		if !strings.Contains(nr, phrase) {
			t.Errorf("shared narration rules dropped agent phrase: %q", phrase)
		}
	}
}

func TestNarrationRulesIsTheSharedSource(t *testing.T) {
	nr := NarrationRules()
	if !strings.Contains(nr, "The way to send no reply is to **write nothing.**") {
		t.Error("NarrationRules missing its anchor sentence")
	}
	// Both Section variants must embed exactly this text — that's what makes
	// it the single source across PA (Section) and Ross/Joanne (NarrationRules).
	if !strings.Contains(Section(Owned), nr) {
		t.Error("Section(Owned) does not embed NarrationRules() verbatim")
	}
	if !strings.Contains(Section(Open), nr) {
		t.Error("Section(Open) does not embed NarrationRules() verbatim")
	}
}

func TestDefaultModeIsOpen(t *testing.T) {
	// An unknown mode must fall back to the conservative shared-responder
	// behavior, never to the owner-gated mandate (which assumes a gate that
	// may not exist).
	if Section(Mode(999)) != Section(Open) {
		t.Error("unknown mode should fall back to Open")
	}
}
