// Package statusroute decides where a transient "heartbeat" / progress line
// is allowed to land for a BimRoss Slack agent (Ross, Joanne, or a per-user
// personal agent).
//
// The harness posts a heartbeat on the agent's behalf when a spawn runs long
// and the operator hasn't heard back ("Still on it — hang tight."). That line
// is reassurance, not content. The hard rule, from makeacompany-ai#676: a
// heartbeat must never land at a channel root. Root is reserved for terminal
// content — a human reply threads under the message, a managed loop threads
// under its anchor, and a per_tick digest loop reserves root for the single
// digest line. A "still going" filler at root defeats all three.
//
// Before this package existed, each agent started its progress pinger
// unconditionally and let its reply closure route the post. That closure
// routes a per_tick digest tick (channel surface, no thread ts) straight to
// channel root, so the heartbeat leaked filler to root on every long digest
// tick (the 2026-06-24 us-states-programs-seed leak). The pinger goroutine
// was also triplicated across the three agent repos and had already drifted
// (Joanne still emitted the pre-humanization "still working (Xm elapsed)"
// phrasing that ross#201/#202 retired). Per the 2026-06-17 promote-to-core
// audit, the routing decision moves here and the agents gate their pinger on
// it, so all three behave identically.
//
// This package owns only the routing decision. Emission (phrasing, jitter,
// the min-gap clock, reusing the agent's reply closure to post) stays in each
// agent — that is where the spawn-local state lives.
package statusroute

// Target is where a heartbeat for a given spawn may be posted.
type Target int

const (
	// TargetThread posts the heartbeat under the spawn's active thread — a
	// human reply's thread ts, or a managed loop's anchor ts. Never root.
	TargetThread Target = iota
	// TargetDMRoot posts at the conversation root, which is only reached for
	// a DM / group-DM. A DM is already its own thread, so its root is not a
	// channel root and a heartbeat there is fine.
	TargetDMRoot
	// TargetSuppress means do not post a heartbeat at all. This is the
	// per_tick digest-loop case: a channel surface whose root is reserved for
	// the one terminal digest line, with no thread to nest a heartbeat under.
	TargetSuppress
)

// Spawn is the routing context a heartbeat decision needs. Both fields are
// already resolved by the agent before it would start its progress pinger.
type Spawn struct {
	// ChannelSurface is true for a public or private channel, false for a DM
	// or group-DM. Mirrors the agent's isChannelSurface(msg.ChannelType).
	ChannelSurface bool
	// ThreadTS is the timestamp the spawn's replies nest under. Empty means
	// "no thread." On a channel surface an empty ThreadTS is exactly the
	// per_tick digest-loop shape (the synthetic tick carries no thread ts);
	// a managed loop carries its anchor ts here and a human reply carries the
	// message's own ts, so both are non-empty.
	ThreadTS string
}

// HeartbeatTarget reports where, if anywhere, a transient heartbeat for this
// spawn may be posted. The single invariant it enforces: a heartbeat never
// lands at a channel root.
func HeartbeatTarget(s Spawn) Target {
	if !s.ChannelSurface {
		// A DM is its own thread; posting at its root is not a channel-root
		// post, so the heartbeat is allowed.
		return TargetDMRoot
	}
	if s.ThreadTS == "" {
		// Channel surface with nothing to thread under — the per_tick digest
		// tick. Suppress so the heartbeat can't leak to channel root.
		return TargetSuppress
	}
	// Threaded human reply or managed-loop anchor tick — nest under ThreadTS.
	return TargetThread
}

// ShouldPostHeartbeat is the convenience predicate an agent uses to gate its
// progress pinger: start/emit the heartbeat only when it has a destination
// that is not a channel root.
func ShouldPostHeartbeat(s Spawn) bool {
	return HeartbeatTarget(s) != TargetSuppress
}
