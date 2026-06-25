# Dispatcher + gate unification — design doc (claude-code-core#24)

Status: **DRAFT / for review.** 2026-06-25. Builds on `harness-unification.md` (#30).
This is the design-first artifact #24 requires: the decision table + interfaces +
sub-slice plan, BEFORE any refactor.

## Decisions (Grant, 2026-06-25)

1. **Full dispatcher extraction.** Lift the whole dispatch loop into a shared
   `core/harness`, including Joanne's `handleMessage` restructure to the Runner
   model (which also unblocks Joanne's deferred `sessionqueue` adoption). The
   three harnesses become one parameterized program.
2. **End-state deferred.** Keep building toward `core/harness` consumed by thin
   per-agent `main`s; decide one-binary-+`AGENT_MODE` vs one-image-+N-entrypoints
   only once the wiring layer exists and we can see how thin it is.

## The core reframe (from #30): two orthogonal axes, mode-selected

A unified gate is NOT a merge of `ownergate` and `threadowner` — they are
different axes that never both do work for the same agent:

- **`ownergate` = admission** ("is this sender allowed to talk to an agent
  here?"). For default mode it is a no-op (`Check` returns `Pass` when
  `ownerID == ""`). Real only for personal/team.
- **`threadowner` = fleet routing** ("which of the Ross/Joanne pair owns this
  thread?"). Real only for the two-agent default fleet; a single owner-gated
  agent has no counterpart.

So the unified gate is **mode-selected**:

```
mode = default            → ownergate no-op → threadowner routes
mode = personal | team    → no counterpart → ownergate gates, threadowner absent
```

Mode is derived from config: `ownerID == ""` → default; `ownerID` set + team off
→ personal; `ownerID` set + team on → team.

## The unified Decision shape

`ownergate.Decision` (Pass/PassClaim/SilentDrop/SilentDropRelease/Reject) and
`threadowner.Decision` ({Respond, NewOwner}) collapse to one struct the
dispatcher acts on:

```go
package gate

type Decision struct {
    Respond bool   // spawn for this message?
    SetOwner string // set thread owner to this id ("" = no change)
    Clear   bool   // clear thread ownership (operator handed the thread off)
    Reject  bool   // a disallowed sender DIRECTLY addressed us (telemetry /
                   // optional polite-refuse) — distinct from a silent drop
}
```

Mapping from the existing predicates:
- `ownergate.Pass`              → `{Respond:true}`
- `ownergate.PassClaim`         → `{Respond:true, SetOwner:self}`
- `ownergate.SilentDrop`        → `{}` (drop, no UX)
- `ownergate.SilentDropRelease` → `{Clear:true}` (drop + release)
- `ownergate.Reject`            → `{Reject:true}` (drop, address attempt logged)
- `threadowner {Respond, NewOwner}` → `{Respond, SetOwner:string(NewOwner)}`

## Decision table (the gate-matrix test fixtures)

`self` = the agent; `owner` meaningful only when set. Rows grouped by mode; the
"conflict" rows from the #30 audit are resolved by mode-selection (each is just
the other mode's rule, never simultaneous).

### default mode (Ross/Joanne) — routing axis (threadowner)
| # | inbound | Decision |
|---|---|---|
| 1 | new thread, generic | Ross responds+owns; Joanne silent (`{Respond: me==ross, SetOwner:ross}`) |
| 2 | new thread, @Joanne only | Joanne responds+owns |
| 3 | dual-mention @both | both respond; Ross owns (`SetOwner:ross`) |
| 4 | in-thread generic, owned | current owner responds; no change |
| 5 | in-thread generic, NO owner | Ross-default (responds+owns); Joanne silent |
| 6 | in-thread @other | other responds; ownership flips to other |
| 7 | channel locked (welcome) | lock owner responds on @-mention; no flip (see welcome policy) |

### personal mode (PA strict) — admission axis (ownergate)
| # | inbound | Decision |
|---|---|---|
| 8 | owner DM | `{Respond}` |
| 9 | owner @-mention in channel | `{Respond, SetOwner:self}` (PassClaim) |
| 10 | owner @-mentions someone else in a thread we owned | `{Clear}` (release) |
| 11 | owner thread reply, we own | `{Respond}` |
| 12 | owner top-level, no mention | `{}` (no barge-in) |
| 13 | non-owner DM or @-mention | `{Reject}` |
| 14 | non-owner chatter | `{}` |

### team mode (PA team) — admission + owner-presence
| # | inbound | Decision |
|---|---|---|
| 15 | non-owner @self, owner-in-channel | `{Respond, SetOwner:self}` (PassClaim) |
| 16 | non-owner thread reply, we own, owner-in-channel | `{Respond}` |
| 17 | non-owner top-level / mention-other, owner-in-channel | `{}` (no barge-in) |
| 18 | non-owner anything, owner NOT in channel (or API error) | strict fallback: `{Reject}`/`{}` (fail-closed) |
| 19 | owner (any) | exactly the personal-mode owner rows (unchanged by team) |

**Pre-gate short-circuits** (handled in the dispatcher BEFORE `gate.Decide`,
because they're admission overrides, not routing):
- **kill-switch** (`PERSONAL_AGENT_DISABLED`): silent-drop every inbound event.
- **handoff admission** (mode-specific, per #30 Q5): default = any peer bot;
  owner = 🤝 + specific peer only. If admitted, bypass the normal gate.
- **pasession escape hatch** (owner modes): an open peer-bot session admits the
  peer into an owner thread until its TTL/counter expires; short-circuits to
  `{Respond}`.

## ThreadOwnership interface (the two-store reconciliation)

The audit's "two incompatible stores" collapse to one identity-valued interface
with two backends:

```go
type ThreadOwnership interface {
    Owner(channel, threadTS string) (id string, ok bool)
    SetOwner(channel, threadTS, id string) error
    Clear(channel, threadTS string) error
}
```
- **default mode** (Ross/Joanne): Redis-backed (today's `threadowner.Store`),
  id ∈ {"ross","joanne"} — shared by the fleet.
- **personal/team** (PA): file-backed per-pod (today's `threadOwnerStore`), the
  only possible id is `self` → `Owner()==self` is the `ownsThread bool` ownergate
  needs. No storage-backend change; PA stays per-pod isolated.

`threadowner.Owner` (enum) becomes a string id; `ownergate`'s `ownsThread bool`
becomes `id == self`.

## Joanne's welcome-routing = a default-mode channel policy

Joanne's `isWelcomeReply`/`isWelcomeSoftTrigger` logic forces Joanne to own +
respond in the welcome channel / on soft-triggers — a default-mode OVERRIDE of
the normal threadowner routing (conceptually a generalized channel-lock).

Model it as an optional **ChannelPolicy hook** on the default-mode gate config:
```go
type ChannelPolicy func(ev) (force *Decision, ok bool) // ok → use force, bypass threadowner
```
Joanne supplies its welcome policy; Ross supplies nil. Keeps the gnarly
onboarding rule in Joanne's wiring while the shared gate stays generic. (Channel
locks fold into the same hook.)

## handleMessage → Runner unification (the risky part)

Ross/PA already call `sessionqueue` with a batch Runner. Joanne's batch loop
lives INSIDE `handleMessage` (`claimOrEnqueue` + `runClaudeBatch`), so Joanne
can't use the shared queue/dispatcher yet. Restructure Joanne to the Runner
model (synthesizeBatch + a runner that calls handleMessage), exactly as PA's
sessionqueue adoption did — then Joanne adopts the shared dispatcher + the
deferred `sessionqueue`. This is onboarding-critical; gate it behind the
synthetic-event + welcome-thread tests.

## Ordered sub-slices (each shippable; lowest-risk first)

- **S1 — `core/gate` decision + 13-row gate-matrix test.** Pure logic composing
  ownergate(admission) + threadowner(routing) by mode, ChannelPolicy + handoff +
  pasession hooks, the unified `Decision`. No dispatcher yet. Agents can even
  adopt the decision while keeping their dispatchers (intermediate validation).
- **S2 — `ThreadOwnership` interface** in core + adapt the Redis store and PA's
  file store to it.
- **S3 — extract the dispatcher loop** into `core/harness` parameterized by
  gate + ThreadOwnership + ingress + Runner. Ross + PA adopt first.
- **S4 — Joanne restructure**: handleMessage→Runner + welcome ChannelPolicy +
  adopt the shared dispatcher + the deferred `sessionqueue`. The risky slice;
  heavy tests on welcome/soft-trigger/MPIM.
- **S5 — PA 🔴-stop** (#77) falls out of the shared dispatcher's stop path.
- **S6 — end-state decision** (one binary vs N entrypoints), now that the wiring
  layer exists.

## Residual risks / open items

- Joanne's welcome/soft-trigger/MPIM paths are the highest-risk surface — S4
  needs synthetic-event + welcome-thread test coverage before it ships, and the
  smoke+canary rollout gate.
- `gate.Decision` must preserve the Reject-vs-SilentDrop distinction (telemetry
  + the personal-mode polite-refuse) — don't collapse them.
- The dispatcher's per-agent divergent constants (timeouts, drain caps, grace,
  rate-limit keys) become harness params, NOT unified constants (per the #30
  audit) — inventory before S3.
