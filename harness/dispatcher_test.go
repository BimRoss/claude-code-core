package harness

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/bimross/claude-code-core/gate"
	"github.com/bimross/claude-code-core/threadowner"
	"github.com/bimross/claude-code-core/threadownership"
	"github.com/slack-go/slack/slackevents"
)

// harness fixture: an in-memory dedupe + a recording Route + a tmp ownership
// store. Returns the dispatcher and a pointer to the "routed?" flag.
type fix struct {
	d        *Dispatcher[string]
	routed   *bool
	routedEv *slackevents.MessageEvent
	rejected *bool
	reacted  *bool
	seen     map[string]bool
}

func newFix(t *testing.T, cfg gate.Config) *fix {
	t.Helper()
	routed := false
	rejected := false
	reacted := false
	f := &fix{routed: &routed, rejected: &rejected, reacted: &reacted, seen: map[string]bool{}}
	f.d = &Dispatcher[string]{
		Gate:      cfg,
		Ownership: threadownership.NewFile(filepath.Join(t.TempDir(), "o.json")),
		Ctx:       context.Background(),
		EventID:   func(raw json.RawMessage) string { return string(raw) },
		Seen: func(id string) bool {
			if f.seen[id] {
				return true
			}
			f.seen[id] = true
			return false
		},
		ChannelAllowed: func(ch string) (bool, string) { return ch != "CBLOCKED", "blocked" },
		ExtractFiles:   func(raw json.RawMessage) []string { return nil },
		Route:          func(ev *slackevents.MessageEvent, files []string) { routed = true; f.routedEv = ev },
		OnReject:       func(ev *slackevents.MessageEvent) { rejected = true },
		HandleReaction: func(ctx context.Context, ev *slackevents.ReactionAddedEvent) { reacted = true },
	}
	return f
}

func msg(user, text, threadTS, msgTS, channel, channelType string) slackevents.EventsAPIEvent {
	return slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Data: &slackevents.MessageEvent{
				User: user, Text: text, ThreadTimeStamp: threadTS, TimeStamp: msgTS,
				Channel: channel, ChannelType: channelType,
			},
		},
	}
}

func rossDefault() gate.Config {
	return gate.Config{Self: "ross", BotID: "BROSS", Me: threadowner.OwnerRoss, CounterpartBotID: "BJOANNE"}
}
func ownerCfg() gate.Config {
	return gate.Config{Self: "self", OwnerID: "UOWNER", BotID: "BSELF"}
}

func TestDispatch_DedupeAndSelfAndChannel(t *testing.T) {
	f := newFix(t, rossDefault())
	// First delivery routes (new thread, generic, Ross owns).
	f.d.Dispatch(msg("UHUMAN", "hi", "", "100", "C1", "channel"), json.RawMessage(`"e1"`), 0, "")
	if !*f.routed {
		t.Fatal("first message should route")
	}
	// Duplicate event_id → dropped.
	*f.routed = false
	f.d.Dispatch(msg("UHUMAN", "hi again", "", "101", "C1", "channel"), json.RawMessage(`"e1"`), 1, "retry")
	if *f.routed {
		t.Fatal("duplicate event_id must be dropped")
	}
	// Self message → skipped.
	*f.routed = false
	f.d.Dispatch(msg("BROSS", "i am the bot", "", "102", "C1", "channel"), json.RawMessage(`"e2"`), 0, "")
	if *f.routed {
		t.Fatal("self message must be skipped")
	}
	// Blocked channel → dropped.
	*f.routed = false
	f.d.Dispatch(msg("UHUMAN", "hi", "", "103", "CBLOCKED", "channel"), json.RawMessage(`"e3"`), 0, "")
	if *f.routed {
		t.Fatal("blocked channel must be dropped")
	}
}

func TestDispatch_PreGatePipeline(t *testing.T) {
	// Drop pre-gate stops everything.
	f := newFix(t, rossDefault())
	f.d.PreGates = []PreGate{
		func(ctx context.Context, ev *slackevents.MessageEvent) (PreGateVerdict, string) {
			return Drop, "intake"
		},
	}
	f.d.Dispatch(msg("UHUMAN", "x", "", "100", "C1", "channel"), json.RawMessage(`"e1"`), 0, "")
	if *f.routed {
		t.Fatal("Drop pre-gate must stop the message")
	}

	// Admit pre-gate bypasses the gate and routes directly (handoff path) — even
	// for an input the gate would have dropped (Joanne's perspective on a
	// generic new thread = silent).
	f2 := newFix(t, joanneDefault())
	f2.d.PreGates = []PreGate{
		func(ctx context.Context, ev *slackevents.MessageEvent) (PreGateVerdict, string) { return Admit, "" },
	}
	f2.d.Dispatch(msg("BPEER", "hey <@BJOANNE>", "", "100", "C1", "channel"), json.RawMessage(`"e2"`), 0, "")
	if !*f2.routed {
		t.Fatal("Admit pre-gate must route directly, bypassing the gate")
	}

	// Continue side-effect hook lets the gate decide normally.
	called := false
	f3 := newFix(t, rossDefault())
	f3.d.PreGates = []PreGate{
		func(ctx context.Context, ev *slackevents.MessageEvent) (PreGateVerdict, string) {
			called = true
			return Continue, ""
		},
	}
	f3.d.Dispatch(msg("UHUMAN", "hi", "", "100", "C1", "channel"), json.RawMessage(`"e3"`), 0, "")
	if !called || !*f3.routed {
		t.Fatalf("Continue hook should run then gate routes: called=%v routed=%v", called, *f3.routed)
	}
}

func joanneDefault() gate.Config {
	return gate.Config{Self: "joanne", BotID: "BJOANNE", Me: threadowner.OwnerJoanne, CounterpartBotID: "BROSS"}
}

func TestDispatch_DefaultModeGateAndOwnership(t *testing.T) {
	// Ross routes a new-thread generic and records ownership.
	f := newFix(t, rossDefault())
	f.d.Dispatch(msg("UHUMAN", "hi", "", "100", "C1", "channel"), json.RawMessage(`"e1"`), 0, "")
	if !*f.routed {
		t.Fatal("ross should route a new-thread generic")
	}
	if id, ok := f.d.Ownership.Owner("C1", "100"); !ok || id != "ross" {
		t.Fatalf("ownership should be ross, got (%q,%v)", id, ok)
	}
	// Joanne stays silent on the same input.
	fj := newFix(t, joanneDefault())
	fj.d.Dispatch(msg("UHUMAN", "hi", "", "100", "C1", "channel"), json.RawMessage(`"e1"`), 0, "")
	if *fj.routed {
		t.Fatal("joanne must stay silent on a generic new thread")
	}
}

func TestDispatch_OwnerModeRejectAndRoute(t *testing.T) {
	f := newFix(t, ownerCfg())
	// Owner DM → routes.
	f.d.Dispatch(msg("UOWNER", "do x", "", "100", "D1", "im"), json.RawMessage(`"e1"`), 0, "")
	if !*f.routed {
		t.Fatal("owner DM should route")
	}
	// Non-owner DM → reject (OnReject fires, no route).
	f2 := newFix(t, ownerCfg())
	f2.d.Dispatch(msg("USTRANGER", "hi", "", "100", "D1", "im"), json.RawMessage(`"e1"`), 0, "")
	if *f2.routed {
		t.Fatal("non-owner DM must not route")
	}
	if !*f2.rejected {
		t.Fatal("non-owner direct address should fire OnReject")
	}
}

func TestDispatch_EmptyMessageAndReaction(t *testing.T) {
	// Empty (no text, no files) → not routed even when the gate would respond.
	f := newFix(t, ownerCfg())
	f.d.Dispatch(msg("UOWNER", "   ", "", "100", "D1", "im"), json.RawMessage(`"e1"`), 0, "")
	if *f.routed {
		t.Fatal("empty message must not route")
	}
	// Reaction routes to HandleReaction.
	f2 := newFix(t, ownerCfg())
	reactEv := slackevents.EventsAPIEvent{
		Type:       slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{Data: &slackevents.ReactionAddedEvent{User: "UOWNER", Reaction: "white_check_mark"}},
	}
	f2.d.Dispatch(reactEv, json.RawMessage(`"e2"`), 0, "")
	if !*f2.reacted {
		t.Fatal("reaction should route to HandleReaction")
	}
}
