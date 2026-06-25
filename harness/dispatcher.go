// Package harness is the shared Slack-event dispatch loop for all three agents
// (#24 S3). It owns the message-path routing every agent duplicated — dedupe,
// self-skip, channel policy, an ordered pre-gate pipeline, the unified
// gate.Decide + thread-ownership persistence, file extraction, and the handoff
// of an admitted message to the agent's spawn wiring.
//
// What stays per-agent is injected as hooks: the PreGate pipeline (intake,
// engagement, allowlist, trial, handoff, kill-switch, pasession — heterogeneous,
// ordered, sometimes mode-specific), the spawn Route (synthesize-batch + enqueue
// + handleMessage — agent-specific), and the reaction path (feature glue:
// watch-stop, gmail-confirm, yes/no/ship). The dispatcher is generic over the
// agent's file-ref type F.
package harness

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/bimross/claude-code-core/gate"
	"github.com/bimross/claude-code-core/threadowner"
	"github.com/bimross/claude-code-core/threadownership"
	"github.com/slack-go/slack/slackevents"
)

// PreGateVerdict is the outcome of one pre-gate hook.
type PreGateVerdict int

const (
	// Continue: this hook passes; run the next pre-gate (or the gate).
	Continue PreGateVerdict = iota
	// Drop: stop here, no spawn (reason logged). Intake-interception,
	// allowlist/trial denials, etc.
	Drop
	// Admit: bypass gate.Decide and spawn directly. The cross-agent handoff
	// path: an admitted peer-bot @-mention spawns without the normal gate.
	Admit
)

// PreGate is one ordered admission/side-effect hook. Side-effect-only hooks
// (e.g. engagement recording) do their work and return Continue. Order is
// behavior-sensitive (e.g. engagement records BEFORE allowlist/trial drops so
// the /admin panel counts attempts) — agents supply the exact order.
type PreGate func(ctx context.Context, ev *slackevents.MessageEvent) (PreGateVerdict, string)

// Dispatcher routes one already-acked Slack EventsAPI callback. Construct one
// per pod and feed it from whatever ingress (socket mode / http-events) — the
// ingress differs only in the source; everything from dedupe onward is here.
type Dispatcher[F any] struct {
	// Gate selects mode (default/personal/team) and carries the per-mode gate
	// inputs. Gate.BotID is the agent's bot user id (self-skip + mentions).
	Gate gate.Config
	// Ownership is the unified thread-ownership store (Redis-enum adapter for
	// the fleet, per-pod file for owner modes).
	Ownership threadownership.Store

	// Ctx is the handler context (drain-aware).
	Ctx context.Context

	// EventID extracts the Slack event_id from the raw envelope (for dedupe).
	EventID func(raw json.RawMessage) string
	// Seen reports whether event_id was already handled (markSeen: true = dup).
	Seen func(eventID string) bool
	// ChannelAllowed reports whether to process events for a channel, plus the
	// drop reason for logging.
	ChannelAllowed func(channelID string) (bool, string)

	// PreGates run in order before the gate. See PreGate.
	PreGates []PreGate

	// ExtractFiles pulls the agent's file refs off the raw envelope.
	ExtractFiles func(raw json.RawMessage) []F

	// Route hands an admitted message to the agent's spawn wiring (synthesize +
	// sessionqueue.Enqueue + handleMessage). Same closure the boot-time
	// Queue.Replay reinjects through.
	Route func(ev *slackevents.MessageEvent, files []F)

	// OnReject, if set, runs when a disallowed sender directly addressed us
	// (gate Reject — owner modes). Used for a polite refusal. nil = silent.
	OnReject func(ev *slackevents.MessageEvent)

	// HandleReaction handles the entire reaction path (agent feature glue:
	// stop, yes/no/ship, gmail-confirm, watch-stop). The dispatcher only routes
	// ReactionAddedEvent here after dedupe. nil = reactions ignored. (Unifying
	// the shared reaction scaffold — freshness/channel/inflight — is a planned
	// follow-up; reactions are feature-specific today.)
	HandleReaction func(ctx context.Context, ev *slackevents.ReactionAddedEvent)
}

// Dispatch processes one already-acked EventsAPI callback. The ack to Slack
// happens before this is called (in the ingress); Dispatch never acks.
func (d *Dispatcher[F]) Dispatch(eventsAPI slackevents.EventsAPIEvent, raw json.RawMessage, retryAttempt int, retryReason string) {
	if eventsAPI.Type != slackevents.CallbackEvent {
		return
	}
	if eventID := d.EventID(raw); d.Seen(eventID) {
		slog.Info("event_duplicate_dropped", "event_id", eventID, "retry_attempt", retryAttempt, "retry_reason", retryReason)
		return
	}
	switch ev := eventsAPI.InnerEvent.Data.(type) {
	case *slackevents.MessageEvent:
		d.dispatchMessage(ev, raw)
	case *slackevents.ReactionAddedEvent:
		if d.HandleReaction != nil {
			d.HandleReaction(d.Ctx, ev)
		}
	}
}

func (d *Dispatcher[F]) dispatchMessage(ev *slackevents.MessageEvent, raw json.RawMessage) {
	if ev.User == d.Gate.BotID {
		return
	}
	if ok, reason := d.ChannelAllowed(ev.Channel); !ok {
		slog.Info("event_dropped_by_channel_policy", "channel", ev.Channel, "reason", reason, "kind", "message")
		return
	}

	admit := false
	for _, pg := range d.PreGates {
		verdict, reason := pg(d.Ctx, ev)
		switch verdict {
		case Drop:
			slog.Info("event_dropped_by_pregate", "channel", ev.Channel, "reason", reason, "msg_ts", ev.TimeStamp)
			return
		case Admit:
			admit = true
		}
		if admit {
			break
		}
	}

	if !admit {
		st := d.resolveState(ev)
		dec := gate.Decide(d.Gate, ev, st)
		d.persistOwnership(ev, dec)
		if dec.Reject {
			slog.Info("event_rejected_by_gate", "channel", ev.Channel, "user", ev.User, "msg_ts", ev.TimeStamp)
			if d.OnReject != nil {
				d.OnReject(ev)
			}
			return
		}
		if !dec.Respond {
			slog.Info("event_dropped_by_gate", "channel", ev.Channel, "thread_ts", ev.ThreadTimeStamp, "user", ev.User, "msg_ts", ev.TimeStamp)
			return
		}
	}

	files := d.ExtractFiles(raw)
	if textEmpty(ev.Text) && len(files) == 0 {
		return
	}
	d.Route(ev, files)
}

// resolveState maps the unified ownership store into gate.State per mode:
// default mode reads the recorded fleet owner; owner modes read whether WE own
// the thread (id == self).
func (d *Dispatcher[F]) resolveState(ev *slackevents.MessageEvent) gate.State {
	id, ok := d.Ownership.Owner(ev.Channel, ev.ThreadTimeStamp)
	if d.Gate.OwnerID == "" {
		return gate.State{CurrentOwner: threadowner.Owner(id), HasOwner: ok}
	}
	return gate.State{OwnsThread: ok && id == d.Gate.Self}
}

// persistOwnership applies the gate's ownership change (keyed by thread root, or
// the message ts for a brand-new thread). Best-effort: a store error is logged,
// never fatal.
func (d *Dispatcher[F]) persistOwnership(ev *slackevents.MessageEvent, dec gate.Decision) {
	key := ev.ThreadTimeStamp
	if key == "" {
		key = ev.TimeStamp
	}
	if dec.Clear {
		if err := d.Ownership.Clear(ev.Channel, key); err != nil {
			slog.Warn("thread_owner_clear_failed", "channel", ev.Channel, "thread_ts", key, "error", err)
		}
		return
	}
	if dec.SetOwner != "" {
		if err := d.Ownership.SetOwner(ev.Channel, key, dec.SetOwner); err != nil {
			slog.Warn("thread_owner_write_failed", "channel", ev.Channel, "thread_ts", key, "owner", dec.SetOwner, "error", err)
		}
	}
}

func textEmpty(s string) bool {
	for _, r := range s {
		if r != ' ' && r != '\t' && r != '\n' && r != '\r' {
			return false
		}
	}
	return true
}
