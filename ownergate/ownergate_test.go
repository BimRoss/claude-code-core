package ownergate

import (
	"testing"

	"github.com/slack-go/slack/slackevents"
)

const (
	ownerUID = "U_OWNER"
	otherUID = "U_OTHER"
	selfUID  = "U_SELF"
)

func TestCheck_disabledWhenUnset(t *testing.T) {
	ev := &slackevents.MessageEvent{
		User:        otherUID,
		Text:        "hey channel",
		ChannelType: "channel",
	}
	if got := Check("", selfUID, ev, false); got != Pass {
		t.Errorf("empty owner ID must pass everything; got %v", got)
	}
}

func TestCheck_ownerMention_topLevel_claimsThread(t *testing.T) {
	ev := &slackevents.MessageEvent{
		User:        ownerUID,
		Text:        "<@" + selfUID + "> please",
		ChannelType: "channel",
	}
	if got := Check(ownerUID, selfUID, ev, false); got != PassClaim {
		t.Errorf("owner @-mention should pass + claim; got %v", got)
	}
}

func TestCheck_ownerMention_inThread_claimsThread(t *testing.T) {
	ev := &slackevents.MessageEvent{
		User:            ownerUID,
		Text:            "<@" + selfUID + "> follow up",
		ChannelType:     "channel",
		ThreadTimeStamp: "1700000000.000100",
	}
	if got := Check(ownerUID, selfUID, ev, false); got != PassClaim {
		t.Errorf("owner @-mention in thread should pass + claim; got %v", got)
	}
}

func TestCheck_ownerDM(t *testing.T) {
	ev := &slackevents.MessageEvent{
		User:        ownerUID,
		Text:        "hi",
		ChannelType: "im",
	}
	if got := Check(ownerUID, selfUID, ev, false); got != Pass {
		t.Errorf("owner DM should pass; got %v", got)
	}
}

func TestCheck_ownerThreadReply_owned_passes(t *testing.T) {
	ev := &slackevents.MessageEvent{
		User:            ownerUID,
		Text:            "follow up",
		ChannelType:     "channel",
		ThreadTimeStamp: "1700000000.000100",
	}
	if got := Check(ownerUID, selfUID, ev, true); got != Pass {
		t.Errorf("owner reply in an owned thread should pass; got %v", got)
	}
}

func TestCheck_ownerThreadReply_unowned_drops(t *testing.T) {
	ev := &slackevents.MessageEvent{
		User:            ownerUID,
		Text:            "follow up",
		ChannelType:     "channel",
		ThreadTimeStamp: "1700000000.000100",
	}
	if got := Check(ownerUID, selfUID, ev, false); got != SilentDrop {
		t.Errorf("owner reply in an unowned thread should be silent; got %v", got)
	}
}

func TestCheck_ownerMentionsOtherInOwnedThread_releases(t *testing.T) {
	ev := &slackevents.MessageEvent{
		User:            ownerUID,
		Text:            "<@U_HUMAN> can you take this?",
		ChannelType:     "channel",
		ThreadTimeStamp: "1700000000.000100",
	}
	if got := Check(ownerUID, selfUID, ev, true); got != SilentDropRelease {
		t.Errorf("owner @-mention of another user in owned thread should release; got %v", got)
	}
}

func TestCheck_ownerMentionsOtherInUnownedThread_drops(t *testing.T) {
	ev := &slackevents.MessageEvent{
		User:            ownerUID,
		Text:            "<@U_HUMAN> heads up",
		ChannelType:     "channel",
		ThreadTimeStamp: "1700000000.000100",
	}
	if got := Check(ownerUID, selfUID, ev, false); got != SilentDrop {
		t.Errorf("owner @-mention of other in unowned thread is just chatter; got %v", got)
	}
}

func TestCheck_ownerPlainChannelMessage(t *testing.T) {
	ev := &slackevents.MessageEvent{
		User:        ownerUID,
		Text:        "hey everyone",
		ChannelType: "channel",
	}
	if got := Check(ownerUID, selfUID, ev, false); got != SilentDrop {
		t.Errorf("owner plain channel chatter should be silent; got %v", got)
	}
}

func TestCheck_nonOwnerMentionRejects(t *testing.T) {
	ev := &slackevents.MessageEvent{
		User:        otherUID,
		Text:        "<@" + selfUID + "> hi",
		ChannelType: "channel",
	}
	if got := Check(ownerUID, selfUID, ev, false); got != Reject {
		t.Errorf("non-owner @-mention should reject; got %v", got)
	}
}

func TestCheck_nonOwnerDMRejects(t *testing.T) {
	ev := &slackevents.MessageEvent{
		User:        otherUID,
		Text:        "hello",
		ChannelType: "im",
	}
	if got := Check(ownerUID, selfUID, ev, false); got != Reject {
		t.Errorf("non-owner DM should reject; got %v", got)
	}
}

func TestCheck_nonOwnerPlainChannelSilent(t *testing.T) {
	ev := &slackevents.MessageEvent{
		User:        otherUID,
		Text:        "hey everyone",
		ChannelType: "channel",
	}
	if got := Check(ownerUID, selfUID, ev, false); got != SilentDrop {
		t.Errorf("non-owner channel chatter must be silent; got %v", got)
	}
}

func TestCheck_nonOwnerThreadReplySilent(t *testing.T) {
	ev := &slackevents.MessageEvent{
		User:            otherUID,
		Text:            "thanks",
		ChannelType:     "channel",
		ThreadTimeStamp: "1700000000.000100",
	}
	if got := Check(ownerUID, selfUID, ev, false); got != SilentDrop {
		t.Errorf("non-owner unmentioned thread reply should be silent; got %v", got)
	}
}

func TestCheck_bothMentionedOwnerClaims(t *testing.T) {
	// "@Ross @Garth do X" from owner — self was explicitly addressed, so
	// claim the thread. Sibling-agent filtering (skipping when *only* the
	// sibling is mentioned) lives downstream of the gate.
	ev := &slackevents.MessageEvent{
		User:        ownerUID,
		Text:        "<@U_ROSS> and <@" + selfUID + "> please",
		ChannelType: "channel",
	}
	if got := Check(ownerUID, selfUID, ev, false); got != PassClaim {
		t.Errorf("owner multi-mention including self should claim; got %v", got)
	}
}

func TestCheck_mpimTreatedAsDM(t *testing.T) {
	ev := &slackevents.MessageEvent{
		User:        ownerUID,
		Text:        "hey",
		ChannelType: "mpim",
	}
	if got := Check(ownerUID, selfUID, ev, false); got != Pass {
		t.Errorf("mpim with owner should pass like a DM; got %v", got)
	}
}

// Message-shape constructors for the team-mode matrix. Each returns a fresh
// event so subtests never share mutable state.
func dm(user string) *slackevents.MessageEvent {
	return &slackevents.MessageEvent{User: user, Text: "hi", ChannelType: "im"}
}
func channelMention(user string) *slackevents.MessageEvent {
	return &slackevents.MessageEvent{User: user, Text: "<@" + selfUID + "> please", ChannelType: "channel"}
}
func channelThreadReply(user string) *slackevents.MessageEvent {
	return &slackevents.MessageEvent{User: user, Text: "follow up", ChannelType: "channel", ThreadTimeStamp: "1700000000.000100"}
}
func channelTopLevel(user string) *slackevents.MessageEvent {
	return &slackevents.MessageEvent{User: user, Text: "hey everyone", ChannelType: "channel"}
}

// TestCheckTeam_matrix is the full team-mode truth table:
// {teamMode off/on} × {ownerInChannel true/false} × {owner/non-owner} ×
// {DM / channel-mention / channel-thread-reply-in-owned-thread /
// channel-top-level-no-mention}.
//
// Invariants asserted by construction:
//   - team-off rows must equal the strict Check result (cross-checked in
//     TestCheckTeam_teamOff_equalsCheck).
//   - team-on admits a NON-owner channel mention ONLY when ownerInChannel.
//   - team-on never admits a non-owner DM (always Reject).
//   - ownerInChannel=false (fail-closed) behaves exactly like strict.
//   - the OWNER's result is identical across every team/ownerInChannel combo.
func TestCheckTeam_matrix(t *testing.T) {
	type row struct {
		name           string
		ev             *slackevents.MessageEvent
		ownsThread     bool
		teamEnabled    bool
		ownerInChannel bool
		want           Decision
	}
	rows := []row{
		// ---- team OFF: must mirror strict Check exactly ----
		{"off/owner/dm", dm(ownerUID), false, false, false, Pass},
		{"off/owner/mention", channelMention(ownerUID), false, false, false, PassClaim},
		{"off/owner/threadreply/owned", channelThreadReply(ownerUID), true, false, false, Pass},
		{"off/owner/toplevel", channelTopLevel(ownerUID), false, false, false, SilentDrop},
		{"off/nonowner/dm", dm(otherUID), false, false, false, Reject},
		{"off/nonowner/mention", channelMention(otherUID), false, false, false, Reject},
		{"off/nonowner/threadreply/owned", channelThreadReply(otherUID), true, false, false, SilentDrop},
		{"off/nonowner/toplevel", channelTopLevel(otherUID), false, false, false, SilentDrop},

		// ---- team ON, ownerInChannel TRUE: non-owner admitted on owner's terms ----
		{"on+in/owner/dm", dm(ownerUID), false, true, true, Pass},
		{"on+in/owner/mention", channelMention(ownerUID), false, true, true, PassClaim},
		{"on+in/owner/threadreply/owned", channelThreadReply(ownerUID), true, true, true, Pass},
		{"on+in/owner/toplevel", channelTopLevel(ownerUID), false, true, true, SilentDrop},
		{"on+in/nonowner/dm", dm(otherUID), false, true, true, Reject}, // DMs never relaxed
		{"on+in/nonowner/mention", channelMention(otherUID), false, true, true, PassClaim},
		{"on+in/nonowner/threadreply/owned", channelThreadReply(otherUID), true, true, true, Pass},
		{"on+in/nonowner/threadreply/unowned", channelThreadReply(otherUID), false, true, true, SilentDrop},
		{"on+in/nonowner/toplevel", channelTopLevel(otherUID), false, true, true, SilentDrop},

		// ---- team ON, ownerInChannel FALSE: fail-closed → strict ----
		{"on+out/owner/dm", dm(ownerUID), false, true, false, Pass},
		{"on+out/owner/mention", channelMention(ownerUID), false, true, false, PassClaim},
		{"on+out/nonowner/dm", dm(otherUID), false, true, false, Reject},
		{"on+out/nonowner/mention", channelMention(otherUID), false, true, false, Reject},
		{"on+out/nonowner/threadreply/owned", channelThreadReply(otherUID), true, true, false, SilentDrop},
		{"on+out/nonowner/toplevel", channelTopLevel(otherUID), false, true, false, SilentDrop},
	}

	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			opts := TeamOptions{Enabled: r.teamEnabled, OwnerInChannel: r.ownerInChannel}
			if got := CheckTeam(ownerUID, selfUID, r.ev, r.ownsThread, opts); got != r.want {
				t.Errorf("CheckTeam(%+v, opts=%+v) = %v, want %v", r.ev, opts, got, r.want)
			}
		})
	}
}

// TestCheckTeam_teamOff_equalsCheck proves CheckTeam with a disabled (zero)
// TeamOptions — and with team enabled but owner out-of-channel — is
// byte-for-byte equivalent to the frozen Check across every
// shape/sender/ownership combination. This is the backward-compat + fail-
// closed guarantee.
func TestCheckTeam_teamOff_equalsCheck(t *testing.T) {
	evs := []*slackevents.MessageEvent{
		dm(ownerUID), dm(otherUID),
		channelMention(ownerUID), channelMention(otherUID),
		channelThreadReply(ownerUID), channelThreadReply(otherUID),
		channelTopLevel(ownerUID), channelTopLevel(otherUID),
		{User: ownerUID, Text: "<@U_HUMAN> take this?", ChannelType: "channel", ThreadTimeStamp: "1700000000.000100"},
		{User: ownerUID, Text: "hey", ChannelType: "mpim"},
	}
	for _, owner := range []string{"", ownerUID} {
		for _, ownsThread := range []bool{false, true} {
			for _, ev := range evs {
				strict := Check(owner, selfUID, ev, ownsThread)
				// Disabled team mode AND enabled-but-out-of-channel must both
				// equal strict (the latter is the fail-closed path).
				for _, opts := range []TeamOptions{{}, {Enabled: true, OwnerInChannel: false}} {
					if got := CheckTeam(owner, selfUID, ev, ownsThread, opts); got != strict {
						t.Errorf("CheckTeam(owner=%q, %+v, owns=%v, opts=%+v)=%v, Check=%v",
							owner, ev, ownsThread, opts, got, strict)
					}
				}
			}
		}
	}
}

func TestMentionsAnyOther(t *testing.T) {
	cases := []struct {
		name string
		text string
		self string
		want bool
	}{
		{"empty", "", selfUID, false},
		{"no mentions", "hello there", selfUID, false},
		{"only self", "<@" + selfUID + "> hi", selfUID, false},
		{"other only", "<@U_OTHER> hi", selfUID, true},
		{"self + other", "<@" + selfUID + "> + <@U_OTHER>", selfUID, true},
		{"with display name", "<@U_OTHER|alice> hi", selfUID, true},
		{"self with display name", "<@" + selfUID + "|me> hi", selfUID, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mentionsAnyOther(tc.text, tc.self); got != tc.want {
				t.Errorf("mentionsAnyOther(%q, %q) = %v, want %v", tc.text, tc.self, got, tc.want)
			}
		})
	}
}

func TestMentionsUser(t *testing.T) {
	cases := []struct {
		name string
		text string
		uid  string
		want bool
	}{
		{"empty uid never matches", "<@U_X>", "", false},
		{"plain", "hello <@U_X> world", "U_X", true},
		{"different uid", "hello <@U_Y> world", "U_X", false},
		{"no mentions", "hello world", "U_X", false},
		// MentionsUser intentionally uses a Contains check on the bare
		// <@UID> form, matching Slack's raw event-payload shape. The
		// <@UID|name> shape only appears in user-typed text and is left to
		// the slackMentionPattern regex (used by mentionsAnyOther).
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := MentionsUser(tc.text, tc.uid); got != tc.want {
				t.Errorf("MentionsUser(%q, %q) = %v, want %v", tc.text, tc.uid, got, tc.want)
			}
		})
	}
}
