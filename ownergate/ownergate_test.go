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
