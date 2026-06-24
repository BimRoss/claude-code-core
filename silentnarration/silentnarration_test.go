package silentnarration

import "testing"

func TestLooksLike(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		// ross#263 — the literal incident that motivated the catalog.
		{"incident verbatim", "The message is addressed to John, not me. Staying silent.", true},
		{"staying quiet", "Staying quiet here.", true},
		{"not addressed to me", "Not addressed to me — skipping.", true},
		{"no response needed", "No response needed.", true},
		{"no reply requested", "No reply requested.", true},
		{"nothing to add", "Nothing to add.", true},
		{"will stay quiet", "I'll stay quiet on this one.", true},
		{"i will stay silent", "I will stay silent.", true},
		{"silent exit lowercase", "silent exit", true},
		{"no action needed from me", "No action needed from me.", true},
		{"remaining silent", "Remaining silent.", true},
		{"banned legacy sentinel", "No response requested.", true},

		// ross#299 — "Staying out." / "This message is addressed to <person>"
		// shapes that escaped the original allowlist.
		{"incident 299 verbatim", "This message is addressed to Joanne (<@Joanne>), not me. Staying out.", true},
		{"staying out short", "Staying out.", true},
		{"this message is addressed to", "This message is addressed to Joanne, not me.", true},

		// Garth Franster thread (2026-06-16) — "is being asked, not me" and
		// "Empty stdout" instruction-marker echo.
		{"incident garth verbatim", "<@U0GARTH1234> is being asked, not me. Empty stdout.", true},
		{"is being asked not me", "Joanne is being asked, not me.", true},
		{"is being asked not me no comma", "Joanne is being asked not me.", true},
		{"empty stdout bare", "Empty stdout.", true},
		{"empty stdout lowercase", "empty stdout, no post.", true},

		// ross#295 — watcher-tick leaks.
		{"exiting silent", "Exiting silent.", true},
		{"anchor updated", "Anchor updated.", true},
		{"watcher tick verbatim", "Anchor updated, phase advanced to rolling-out. Exiting silent.", true},
		{"phase unchanged", "Phase unchanged.", true},
		{"phase advanced", "Phase advanced to image-ready.", true},

		// Tnarg Retsof thread (2026-06-18, ross#427).
		{"incident 427 verbatim", "The message tags U0BBDBXNM7T (Tnarg Retsof, who already replied with three cards), not me. My bot ID is U0APX108QE7. Not for me.", true},
		{"not for me bare", "Not for me.", true},
		{"my bot id is", "My bot ID is U0APX108QE7.", true},
		{"my user id is", "My user ID is U0APX108QE7.", true},
		{"the message tags", "The message tags <@U0OTHER>, not me.", true},
		{"the message mentions", "The message mentions Joanne, not me.", true},
		{"the message is for", "The message is for Joanne.", true},

		// joanne#-silent-exit-parity (2026-06-18).
		{"lurking", "ha, you three carry on — just lurking.", true},
		{"stepping out handoff", "Stepping out — <@U0OTHER> all yours.", true},
		{"stepping back", "Stepping back, this one's Joanne's.", true},

		// 2026-06-21 mac-thread (ross#439) — three broadened shapes.
		{"this message is for", "This message is for Grant, not me. No reply.", true},
		{"no reply bare", "No reply.", true},
		{"no response bare", "No response.", true},
		{"nothing for me to add", "Nothing for me to add.", true},
		{"no reply yet legit", "No reply yet from CI — still building.", false},

		// Genuine short replies must pass through.
		{"standing by", "Standing by.", false},
		{"welcome", "Welcome, Brendan.", false},
		{"got it", "Got it, saved. Standing by.", false},
		{"thread reset", "Thread reset — next message starts clean.", false},
		{"thumbs up", "👍", false},
		{"loop killed", "Killed loop pr-roundup.", false},
		{"legit addressed-to question", "The email is addressed to the wrong person — want me to fix it?", false},
		{"empty", "", false},
		{"whitespace only", "   \t  ", false},

		// Long, multi-line, or substantive replies pass through even if they
		// happen to contain a matching phrase — the safety net is conservative.
		{"long message with match", "Deploy is green and rolled out across dev and prod stages. There's nothing to add on the failure scenarios since CI ran clean across all three pipelines and the canary held steady for the full soak window. Let me know if you want a follow-up summary or a diff against the prior release.", false},
		{"multi-line", "Staying silent.\nJust kidding, here's the real reply.", false},

		// ross#464 (C0B6SB6UA4E, Nexus blog-feedback thread) — the leak shape
		// drifted into a multi-line paragraph that beat both the old total
		// char cap (~230 > 200) and the old single-line guard (blank line).
		// Per-line matching catches it: every non-empty line is narration.
		{"incident 464 paragraph", "The thread already got its answer and the closing note doesn't mention me, and there's nothing for me to add.\n\nNo reply needed.", true},
		// One real line among the narration must let the whole thing through —
		// the false-positive guard that keeps per-line matching conservative.
		{"464 mixed real line", "Here's the deploy summary you asked for: all three stages are green and the canary held.\nNo reply needed.", false},
		// The char-cap path on its own: a single ~230-char narration line was
		// waved through by the old 200 cap. With the cap raised it's caught.
		{"464 long single-line narration", "After reading through the whole thread one more time to be sure I wasn't missing an actual request directed at me, the closing note doesn't mention me and there is genuinely nothing for me to add here, so no reply needed.", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := LooksLike(tc.in); got != tc.want {
				t.Fatalf("LooksLike(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
