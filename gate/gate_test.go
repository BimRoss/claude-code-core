package gate

import (
	"testing"

	"github.com/bimross/claude-code-core/threadowner"
	"github.com/slack-go/slack/slackevents"
)

// Bot/user ids used across the table.
const (
	bRoss   = "BROSS"
	bJoanne = "BJOANNE"
	uOwner  = "UOWNER"
	uOther  = "USTRANGER"
	bSelf   = "BSELF"
	bPeer   = "BPEER"
)

func ev(user, text, threadTS, msgTS, channelType string) *slackevents.MessageEvent {
	return &slackevents.MessageEvent{
		User: user, Text: text, ThreadTimeStamp: threadTS, TimeStamp: msgTS, ChannelType: channelType,
	}
}

func eq(t *testing.T, got, want Decision, row string) {
	t.Helper()
	if got != want {
		t.Errorf("row %s: got %+v want %+v", row, got, want)
	}
}

// rossCfg / joanneCfg: default mode (no owner), fleet routing.
func rossCfg() Config {
	return Config{Self: "ross", BotID: bRoss, Me: threadowner.OwnerRoss, CounterpartBotID: bJoanne}
}
func joanneCfg() Config {
	return Config{Self: "joanne", BotID: bJoanne, Me: threadowner.OwnerJoanne, CounterpartBotID: bRoss}
}

// ownerCfg: personal/team mode.
func ownerCfg(team, ownerInCh bool) Config {
	return Config{Self: "self", OwnerID: uOwner, BotID: bSelf, TeamEnabled: team, OwnerInChannel: ownerInCh}
}

func TestDecisionTable_DefaultMode(t *testing.T) {
	// Row 1: new thread, generic → Ross responds+owns; Joanne silent.
	eq(t, Decide(rossCfg(), ev(uOther, "hello", "", "100", "channel"), State{}),
		Decision{Respond: true, SetOwner: "ross"}, "1 ross")
	eq(t, Decide(joanneCfg(), ev(uOther, "hello", "", "100", "channel"), State{}),
		Decision{Respond: false, SetOwner: "ross"}, "1 joanne-silent")

	// Row 2: new thread, @Joanne only → Joanne responds+owns.
	eq(t, Decide(joanneCfg(), ev(uOther, "hey <@"+bJoanne+">", "", "100", "channel"), State{}),
		Decision{Respond: true, SetOwner: "joanne"}, "2")

	// Row 3: dual-mention → both respond; Ross owns.
	dual := ev(uOther, "<@"+bRoss+"> <@"+bJoanne+"> go", "", "100", "channel")
	eq(t, Decide(rossCfg(), dual, State{}), Decision{Respond: true, SetOwner: "ross"}, "3 ross")
	eq(t, Decide(joanneCfg(), dual, State{}), Decision{Respond: true, SetOwner: "ross"}, "3 joanne")

	// Row 4: in-thread generic, owned → current owner responds, no change.
	owned := State{CurrentOwner: threadowner.OwnerRoss, HasOwner: true}
	eq(t, Decide(rossCfg(), ev(uOther, "more", "100", "101", "channel"), owned),
		Decision{Respond: true}, "4 owner-responds")
	eq(t, Decide(joanneCfg(), ev(uOther, "more", "100", "101", "channel"), owned),
		Decision{Respond: false}, "4 non-owner-silent")

	// Row 5: in-thread generic, NO owner → Ross-default.
	eq(t, Decide(rossCfg(), ev(uOther, "more", "100", "101", "channel"), State{}),
		Decision{Respond: true, SetOwner: "ross"}, "5 ross")
	eq(t, Decide(joanneCfg(), ev(uOther, "more", "100", "101", "channel"), State{}),
		Decision{Respond: false, SetOwner: "ross"}, "5 joanne-silent")

	// Row 6: in-thread @other → other responds, ownership flips. From Ross's
	// view, @Joanne in a Ross-owned thread → Ross drops, owner becomes joanne.
	eq(t, Decide(rossCfg(), ev(uOther, "<@"+bJoanne+"> take it", "100", "101", "channel"), owned),
		Decision{Respond: false, SetOwner: "joanne"}, "6 ross-hands-off")
	// From Joanne's view (she's @other), she responds + claims.
	eq(t, Decide(joanneCfg(), ev(uOther, "<@"+bJoanne+"> take it", "100", "101", "channel"), owned),
		Decision{Respond: true, SetOwner: "joanne"}, "6 joanne-claims")

	// Row 7: channel locked to Joanne; @Ross does NOT flip it.
	rl := rossCfg()
	rl.Lock = threadowner.OwnerJoanne
	eq(t, Decide(rl, ev(uOther, "<@"+bRoss+"> hi", "", "100", "channel"), State{}),
		Decision{Respond: true, SetOwner: "joanne"}, "7 ross-mention-no-flip")
}

func TestDecisionTable_ChannelPolicyOverride(t *testing.T) {
	// Welcome soft-trigger: the policy forces Joanne to respond+own, bypassing
	// threadowner (which would otherwise drop a no-mention top-level message).
	cfg := joanneCfg()
	forced := Decision{Respond: true, SetOwner: "joanne"}
	cfg.ChannelPolicy = func(*slackevents.MessageEvent) (Decision, bool) { return forced, true }
	eq(t, Decide(cfg, ev(uOther, "i need help", "", "100", "channel"), State{}), forced, "7-policy")

	// Policy declines (ok=false) → falls through to normal routing.
	cfg.ChannelPolicy = func(*slackevents.MessageEvent) (Decision, bool) { return Decision{}, false }
	eq(t, Decide(cfg, ev(uOther, "<@"+bJoanne+">", "", "100", "channel"), State{}),
		Decision{Respond: true, SetOwner: "joanne"}, "7-policy-declines")
}

func TestDecisionTable_PersonalMode(t *testing.T) {
	cfg := ownerCfg(false, false)

	// Row 8: owner DM.
	eq(t, Decide(cfg, ev(uOwner, "hi", "", "100", "im"), State{}), Decision{Respond: true}, "8")
	// Row 9: owner @self in channel → claim.
	eq(t, Decide(cfg, ev(uOwner, "<@"+bSelf+"> do it", "", "100", "channel"), State{}),
		Decision{Respond: true, SetOwner: "self"}, "9")
	// Row 10: owner @other in a thread we owned → release.
	eq(t, Decide(cfg, ev(uOwner, "<@"+bPeer+"> you take it", "100", "101", "channel"), State{OwnsThread: true}),
		Decision{Clear: true}, "10")
	// Row 11: owner thread reply, we own → respond.
	eq(t, Decide(cfg, ev(uOwner, "more", "100", "101", "channel"), State{OwnsThread: true}),
		Decision{Respond: true}, "11")
	// Row 12: owner top-level, no mention → no barge-in.
	eq(t, Decide(cfg, ev(uOwner, "thinking out loud", "", "100", "channel"), State{}),
		Decision{}, "12")
	// Row 13: non-owner DM → reject; non-owner @self → reject.
	eq(t, Decide(cfg, ev(uOther, "hi", "", "100", "im"), State{}), Decision{Reject: true}, "13 dm")
	eq(t, Decide(cfg, ev(uOther, "<@"+bSelf+">", "", "100", "channel"), State{}),
		Decision{Reject: true}, "13 mention")
	// Row 14: non-owner chatter → silent.
	eq(t, Decide(cfg, ev(uOther, "unrelated", "", "100", "channel"), State{}), Decision{}, "14")
}

func TestDecisionTable_TeamMode(t *testing.T) {
	in := ownerCfg(true, true)   // team on, owner in channel
	out := ownerCfg(true, false) // team on, owner NOT in channel (fail-closed)

	// Row 15: non-owner @self, owner-in-channel → claim.
	eq(t, Decide(in, ev(uOther, "<@"+bSelf+"> help", "", "100", "channel"), State{}),
		Decision{Respond: true, SetOwner: "self"}, "15")
	// Row 16: non-owner thread reply, we own, owner-in-channel → respond.
	eq(t, Decide(in, ev(uOther, "more", "100", "101", "channel"), State{OwnsThread: true}),
		Decision{Respond: true}, "16")
	// Row 17: non-owner top-level, owner-in-channel → no barge-in.
	eq(t, Decide(in, ev(uOther, "chatter", "", "100", "channel"), State{}), Decision{}, "17")
	// Row 18: non-owner @self, owner NOT in channel → fail-closed reject.
	eq(t, Decide(out, ev(uOther, "<@"+bSelf+"> help", "", "100", "channel"), State{}),
		Decision{Reject: true}, "18 fail-closed")
	// Row 19: owner still behaves exactly as personal mode (team never changes owner handling).
	eq(t, Decide(in, ev(uOwner, "hi", "", "100", "im"), State{}), Decision{Respond: true}, "19 owner-dm")
	eq(t, Decide(in, ev(uOwner, "<@"+bSelf+">", "", "100", "channel"), State{}),
		Decision{Respond: true, SetOwner: "self"}, "19 owner-mention")
}
