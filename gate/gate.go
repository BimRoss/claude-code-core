// Package gate is the unified admission+routing decision for all three agents.
//
// The key reframe (see docs/design/dispatcher-gate.md, #24): ownergate and
// threadowner are ORTHOGONAL axes selected by MODE, never merged —
//
//   - default mode (ownerID == ""): ownergate is a no-op; threadowner routes
//     (which of the Ross/Joanne fleet pair responds).
//   - personal/team mode (ownerID set): no counterpart, so threadowner is
//     absent; ownergate admits.
//
// Decide composes the existing, proven predicates (ownergate.CheckTeam,
// threadowner.DecideWithLock) and maps their outputs to one Decision the
// dispatcher acts on. It deliberately keeps the logic IN those packages — this
// is glue + a single decision surface, not a reimplementation.
package gate

import (
	"github.com/bimross/claude-code-core/ownergate"
	"github.com/bimross/claude-code-core/threadowner"
	"github.com/slack-go/slack/slackevents"
)

// Decision is the unified gate verdict. It collapses ownergate's five-value
// Decision and threadowner's {Respond, NewOwner} into the four things the
// dispatcher needs to do.
type Decision struct {
	// Respond: spawn for this message.
	Respond bool
	// SetOwner: set the thread owner to this id ("" = no change). For default
	// mode it's the routed agent ("ross"/"joanne"); for owner modes it's Self
	// on a claim.
	SetOwner string
	// Clear: clear thread ownership (operator handed the thread off — owner
	// @-mentions someone else in a thread we owned).
	Clear bool
	// Reject: a disallowed sender DIRECTLY addressed us (DM or @-mention).
	// Distinct from a silent drop so the dispatcher can log the attempt and/or
	// post a polite refusal. Never set in default mode.
	Reject bool
}

// Config selects the mode and carries the per-mode inputs. Mode is derived:
// OwnerID == "" → default; OwnerID set + !TeamEnabled → personal; OwnerID set +
// TeamEnabled → team.
type Config struct {
	// Self is the id this agent writes as the thread owner on a claim.
	// Default mode: string(Me). Owner modes: the agent's stable self id.
	Self string

	// OwnerID is the configured owner's Slack user id; "" means default
	// (fleet-routing) mode. BotID is this agent's Slack bot user id (mention
	// detection).
	OwnerID string
	BotID   string

	// Team mode (owner modes only): TeamEnabled turns it on; OwnerInChannel is
	// the caller-resolved, fail-closed owner-presence bool.
	TeamEnabled    bool
	OwnerInChannel bool

	// Default mode only: Me is this agent in the fleet; CounterpartBotID is the
	// other agent's bot id (for dual-mention / @other detection); Lock pins a
	// channel owner ("" = unlocked).
	Me               threadowner.Owner
	CounterpartBotID string
	Lock             threadowner.Owner

	// ChannelPolicy, if set, is a default-mode override consulted FIRST: when it
	// returns ok, its Decision is used and threadowner is bypassed. This is how
	// Joanne's welcome-routing (force-own + respond in the welcome channel / on
	// a soft-trigger) plugs in without the gate knowing about onboarding.
	ChannelPolicy func(ev *slackevents.MessageEvent) (Decision, bool)
}

// State is the thread-ownership context for this message, resolved by the
// caller from ThreadOwnership.
type State struct {
	// OwnsThread: do WE own this thread? (ownergate input — owner modes).
	OwnsThread bool
	// CurrentOwner / HasOwner: the recorded fleet owner (threadowner input —
	// default mode).
	CurrentOwner threadowner.Owner
	HasOwner     bool
}

// Decide returns the unified verdict for one inbound message.
//
// Pre-gate short-circuits (kill switch, cross-agent handoff admission, the
// pasession escape hatch) are the dispatcher's job and run BEFORE Decide —
// they're admission overrides, not part of the ownergate/threadowner
// composition.
func Decide(cfg Config, ev *slackevents.MessageEvent, st State) Decision {
	if cfg.OwnerID == "" {
		return decideDefault(cfg, ev, st)
	}
	return decideOwner(cfg, ev, st)
}

// decideDefault routes via threadowner (fleet mode). ChannelPolicy wins first.
func decideDefault(cfg Config, ev *slackevents.MessageEvent, st State) Decision {
	if cfg.ChannelPolicy != nil {
		if d, ok := cfg.ChannelPolicy(ev); ok {
			return d
		}
	}
	mentionsMe := ownergate.MentionsUser(ev.Text, cfg.BotID)
	mentionsOther := cfg.CounterpartBotID != "" && ownergate.MentionsUser(ev.Text, cfg.CounterpartBotID)
	isNewThread := ev.ThreadTimeStamp == "" || ev.ThreadTimeStamp == ev.TimeStamp

	d := threadowner.DecideWithLock(cfg.Me, cfg.Lock, st.CurrentOwner, st.HasOwner, mentionsMe, mentionsOther, isNewThread)
	return Decision{Respond: d.Respond, SetOwner: string(d.NewOwner)}
}

// decideOwner admits via ownergate (personal/team mode) and maps the verdict.
func decideOwner(cfg Config, ev *slackevents.MessageEvent, st State) Decision {
	od := ownergate.CheckTeam(cfg.OwnerID, cfg.BotID, ev, st.OwnsThread, ownergate.TeamOptions{
		Enabled:        cfg.TeamEnabled,
		OwnerInChannel: cfg.OwnerInChannel,
	})
	switch od {
	case ownergate.Pass:
		return Decision{Respond: true}
	case ownergate.PassClaim:
		return Decision{Respond: true, SetOwner: cfg.Self}
	case ownergate.SilentDropRelease:
		return Decision{Clear: true}
	case ownergate.Reject:
		return Decision{Reject: true}
	default: // ownergate.SilentDrop
		return Decision{}
	}
}
