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
// Two things suppress a heartbeat. First, a per_tick digest tick (channel
// surface, no thread ts) has nowhere off-root to put it. Second — and this is
// the durable gate — any synthetic scheduler tick (Spawn.IsLoopTick) has no
// operator waiting on it, so a heartbeat is pointless wherever it would land.
// The second gate matters because claude-code-ross#461 made threaded the
// default loop mode: a digest tick now carries an anchor thread ts, so the
// thread-ts test alone no longer catches it and the heartbeat would re-appear
// threaded under the anchor. IsLoopTick keeps the suppression mode-agnostic, so
// it survives #461 and the still-pending Joanne-scheduler flip.
//
// This package owns only the routing decision. Emission (phrasing, jitter,
// the min-gap clock, reusing the agent's reply closure to post) stays in each
// agent — that is where the spawn-local state lives.
package statusroute

// Target is where a heartbeat for a given spawn may be posted.
type Target int

const (
	// TargetThread posts the heartbeat under the spawn's active thread — a
	// human reply's thread ts. Never root. Loop ticks (managed/threaded
	// included) are caught earlier by IsLoopTick and suppressed.
	TargetThread Target = iota
	// TargetDMRoot posts at the conversation root, which is only reached for
	// a DM / group-DM. A DM is already its own thread, so its root is not a
	// channel root and a heartbeat there is fine.
	TargetDMRoot
	// TargetSuppress means do not post a heartbeat at all. Two cases: any
	// synthetic loop tick (no operator waiting, IsLoopTick), or a channel
	// surface with no thread to nest under (the per_tick digest shape, whose
	// root is reserved for the one terminal digest line).
	TargetSuppress
)

// Spawn is the routing context a heartbeat decision needs. Both fields are
// already resolved by the agent before it would start its progress pinger.
type Spawn struct {
	// ChannelSurface is true for a public or private channel, false for a DM
	// or group-DM. Mirrors the agent's isChannelSurface(msg.ChannelType).
	ChannelSurface bool
	// ThreadTS is the timestamp the spawn's replies nest under. Empty means
	// "no thread." On a channel surface an empty ThreadTS is the per_tick
	// digest-loop shape (the synthetic tick carries no thread ts); a managed
	// or (since claude-code-ross#461) threaded loop carries its anchor ts
	// here, and a human reply carries the message's own ts.
	ThreadTS string
	// IsLoopTick is true when this spawn is a synthetic scheduler tick
	// (the agent's loopID != ""), false for a human-triggered spawn. A loop
	// tick has no operator waiting on it, so a heartbeat is pointless wherever
	// it would land — suppress regardless of mode. Before #461, per_tick was
	// the default and a digest tick carried no thread ts, so ThreadTS == ""
	// alone caught it; #461 made threaded the default (digest ticks now carry
	// an anchor thread ts), so this explicit flag is what keeps the heartbeat
	// from re-appearing threaded under the anchor.
	IsLoopTick bool
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
	if s.IsLoopTick {
		// A synthetic scheduler tick has no operator waiting, in any mode:
		// per_tick (root reserved for the digest line), threaded (anchor
		// thread since #461), or managed (deploy-watcher anchor). A heartbeat
		// adds nothing — suppress wherever it would land. See #676 + #461.
		return TargetSuppress
	}
	if s.ThreadTS == "" {
		// Channel surface with nothing to thread under. handleMessage resolves
		// a human reply's threadTS to the message ts, so this is not normally
		// reached for a human; suppress rather than risk a channel-root post.
		return TargetSuppress
	}
	// Threaded human reply — nest under ThreadTS.
	return TargetThread
}

// ShouldPostHeartbeat is the convenience predicate an agent uses to gate its
// progress pinger: start/emit the heartbeat only when it has a destination
// that is not a channel root.
func ShouldPostHeartbeat(s Spawn) bool {
	return HeartbeatTarget(s) != TargetSuppress
}
