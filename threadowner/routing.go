package threadowner

import "strings"

// Decision is the routing verdict for a single incoming Slack message,
// computed by Decide. Callers translate Respond into "spawn or drop" and
// NewOwner (when non-empty and different from the recorded owner) into a
// Store.Set call before spawning.
type Decision struct {
	// Respond is true when the calling agent should spawn for this message.
	Respond bool
	// NewOwner is the agent who should own the thread after this message.
	// Empty string means "no change". When non-empty and different from the
	// current recorded owner, the caller should persist it before spawning.
	NewOwner Owner
}

// Decide is the canonical routing rule, shared verbatim by both wrappers.
// Parameters describe the message from the calling agent's perspective:
//
//   - me / other: the calling agent and its counterpart.
//   - currentOwner / hasOwner: the recorded thread owner, if any.
//   - mentionsMe / mentionsOther: did the message text @-mention me/other?
//   - isNewThread: true for the very first message in a thread (no
//     ThreadTimeStamp, or ThreadTimeStamp == TimeStamp). False for any
//     reply inside an existing thread.
//
// Rules (mirror of the ticket — keep this comment in sync):
//
//  1. New thread, generic → Ross (default), Ross owns.
//  2. New thread, @Joanne only → Joanne, Joanne owns.
//  3. New thread, both @-mentioned → both respond; Ross owns subsequent.
//  4. In-thread generic → current owner only; no ownership change.
//     If no owner is recorded, fall back to Ross-default (Joanne stays
//     silent, Ross responds and writes himself as owner on spawn).
//  5. In-thread @other → other responds AND ownership flips permanently.
func Decide(me Owner, currentOwner Owner, hasOwner, mentionsMe, mentionsOther, isNewThread bool) Decision {
	switch {
	case mentionsMe && mentionsOther:
		// Rule 3: dual-mention — both respond, Ross owns going forward.
		return Decision{Respond: true, NewOwner: OwnerRoss}

	case mentionsOther && !mentionsMe:
		// Rule 2 (new thread) and Rule 5 (in-thread): hand off entirely.
		// `me` does not respond; the other agent will write itself as
		// owner on its own spawn. We still return NewOwner so the caller
		// can optionally pre-write — but the canonical write happens on
		// the responding side. Returning the value lets a caller race
		// the write if it wants to be defensive; both wrappers today
		// only write on Respond=true and that's fine.
		return Decision{Respond: false, NewOwner: other(me)}

	case mentionsMe && !mentionsOther:
		// Rule 2/5 from `me`'s side: I'm explicitly addressed. Respond
		// and claim ownership (flips on a previously-owned-by-other
		// thread; idempotent if I already owned it).
		return Decision{Respond: true, NewOwner: me}

	default:
		// No mentions — generic message.
		if isNewThread {
			// Rule 1: new thread always belongs to Ross.
			return Decision{Respond: me == OwnerRoss, NewOwner: OwnerRoss}
		}
		// Rule 4: in-thread, route to current owner.
		if hasOwner {
			return Decision{Respond: currentOwner == me}
		}
		// No owner recorded (e.g. thread predates this feature). Fall
		// back to Ross-default. Joanne stays silent; Ross responds and
		// will write owner=ross as part of his spawn path. NewOwner is
		// returned for both sides so Ross's caller writes it; Joanne's
		// caller will see Respond=false and skip the write.
		return Decision{Respond: me == OwnerRoss, NewOwner: OwnerRoss}
	}
}

// other returns the counterpart agent.
func other(me Owner) Owner {
	if me == OwnerRoss {
		return OwnerJoanne
	}
	return OwnerRoss
}

// DecideWithLock applies channel-lock policy on top of Decide. When lockedTo
// is empty, behavior is identical to Decide.
//
// When lockedTo is non-empty, the channel has a fixed owner that does not
// flip on @-mentions:
//
//   - mentionsMe: I respond. Ownership stays with lockedTo (no flip).
//   - mentionsOther only: I drop. Other agent responds on its own side.
//   - no mentions: I respond iff me == lockedTo. The other agent stays silent
//     regardless of new-thread / in-thread or recorded owner.
//
// NewOwner is always lockedTo so any persisted record matches channel policy,
// even when an unlocked-era message wrote a different owner into the store.
//
// Use case: the welcome channel (C0APJT7PYUU) is Joanne's domain. Ross only
// spawns there on an explicit <@ross> mention.
func DecideWithLock(me, lockedTo Owner, currentOwner Owner, hasOwner, mentionsMe, mentionsOther, isNewThread bool) Decision {
	if lockedTo == "" {
		return Decide(me, currentOwner, hasOwner, mentionsMe, mentionsOther, isNewThread)
	}
	if mentionsMe {
		return Decision{Respond: true, NewOwner: lockedTo}
	}
	if mentionsOther {
		return Decision{Respond: false, NewOwner: lockedTo}
	}
	return Decision{Respond: me == lockedTo, NewOwner: lockedTo}
}

// ParseChannelLocks parses a LOCKED_CHANNELS env value into a lookup map.
// Format: comma-separated "<channelID>:<owner>" pairs, e.g.
// "C0APJT7PYUU:joanne,C0XXX:ross". Whitespace around pairs is trimmed.
// Unknown owner strings and malformed pairs are silently skipped — the
// caller treats a missing key as "channel is unlocked".
func ParseChannelLocks(s string) map[string]Owner {
	m := map[string]Owner{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		i := strings.IndexByte(part, ':')
		if i <= 0 || i == len(part)-1 {
			continue
		}
		k := strings.TrimSpace(part[:i])
		v := strings.TrimSpace(part[i+1:])
		switch Owner(v) {
		case OwnerRoss, OwnerJoanne:
			m[k] = Owner(v)
		}
	}
	return m
}
