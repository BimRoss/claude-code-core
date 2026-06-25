# Harness unification — design doc (claude-code-core#24)

Status: **DRAFT / for consensus.** Author: 2026-06-25. Parent epic: #21.

This doc is the design-first artifact #24 requires *before* any extraction. Its
job is to (a) fix the architecture and (b) surface the genuine decisions so we
agree on them before writing code. Open decisions are collected at the end.

## Goal & non-goals

**Goal:** stop maintaining the same harness three times. Lift the ~70%
copy-pasted dispatcher/spawn/session machinery from `claude-code-ross`,
`claude-code-joanne`, and `claude-code-personal-agent` into a shared
`core/harness`, so a behavior or fix lands once.

**Explicit non-goals (the legitimate differences we KEEP):**
- Ross/Joanne keep **baked creds** (Google sidecar + GitHub PAT). No Connections
  migration (epic Decision 6, descoped).
- Ross/Joanne stay **default-mode** (answer anyone in channel, routed by the
  two-agent fleet rules). Personal/team agents stay **owner-gated**. We are not
  collapsing the bots into one identity.
- We are **not** forcing Ross/Joanne off Socket Mode as a precondition (see
  Ingress below). Ingress becomes a parameter, not a migration gate.
- One-binary vs N-entrypoints is **not decided here** (epic Decision 2); the
  extraction is structured so either is possible.

The design principle throughout: **shared harness with clean seams for the
real differences** — parameters, not a forced monolith.

## Core insight: the two gates are orthogonal, mode-selected — NOT merged

The audit framed `ownergate` ↔ `threadowner` as a conflicting merge. Reading the
code, they're **two different axes that never both do work for the same agent**:

- **`ownergate`** = *admission* ("is this sender allowed to talk to an agent
  here?"). For default mode it is a **no-op**: `Check`/`CheckTeam` return `Pass`
  unconditionally when `ownerSlackUserID == ""` (ownergate.go:`checkTeam` first
  line). Admission gating only exists for personal/team agents.
- **`threadowner`** = *fleet routing* ("given admission, which of the
  Ross/Joanne pair owns this thread?" — dual-mention, channel locks,
  Ross-default). It is meaningful only for the **two-agent default fleet**; a
  single owner-gated agent has no counterpart, so it never runs.

So the "reconciliation" is **selection by mode, not a merged decision
function**:

```
mode = default            → ownergate is a no-op → threadowner routes
mode = personal | team    → no counterpart → threadowner is absent → ownergate gates
```

Every "conflict" the audit flagged dissolves once you see it's mode-selected:

| Audit "conflict" | Reality |
|---|---|
| Row 7: dual-mention "both reply" has no owner-mode meaning | It's a *fleet* rule; structurally absent when an owner is set. |
| Row 9: unowned-thread generic → ownergate drops vs threadowner Ross-responds | Different *modes*: personal = don't barge owner's threads (drop); default = Ross-default (respond). Both correct for their mode. |
| Row 10: release vs flip | personal = release claim (hand to a human); default = flip to the counterpart agent. Mode-specific, never simultaneous. |

This is a much smaller and safer design than "one giant decision table," and it
directly serves "easier to manage": each mode's rule stays legible.

## Architecture: the `core/harness` boundary

```
core/harness (NEW shared package)
  Dispatcher
    1. pre-gate     : dedupe, kill-switch, ingress-normalize, handoff admission
    2. gate (mode-selected):
         default        → threadowner.DecideWithLock(...)
         personal|team  → ownergate.CheckTeam(...)
    3. spawn loop   : session-key → channel-context → claude spawn
                      → spawnretry (OAuth slot failover) → stream → post
    4. post-spawn   : silent-narration suppress, resume markers, engagement
  Interfaces the harness depends on (impl wired per agent):
    - Identity        (name, ownerID, mode, ingress, workspace, timeouts…)
    - ThreadOwnership (unified; see below)
    - Ingress         (socket | http-events; strategy param)
    - FeatureModules  (watches, drains, gmail-confirm — #25, stay external)
```

**Stays per-agent (the seams):** identity/creds wiring, which feature-modules
are enabled, the Slack app/manifest. **Moves to core/harness:** everything in
the Dispatcher box above, which is the duplicated 70%.

## Thread-ownership: one interface, two backends

The audit's "two incompatible stores" collapses cleanly. `threadowner.Store` is
already identity-valued:

```go
type Store interface {
    Get(channel, threadTS string) (Owner, bool)
    Set(channel, threadTS string, owner Owner) error
}
```

PA's private `threadOwnerStore` (`owns/claim/release` → bool, file-backed) is
just the **degenerate case where the only possible owner is "me"**:
- `owns(c,t)`     ≡ `Get(c,t) == selfIdentity`
- `claim(c,t)`    ≡ `Set(c,t, selfIdentity)`
- `release(c,t)`  ≡ `Set(c,t, "")`  (add an explicit clear/Delete to the interface)

**Proposal:** adopt `threadowner.Store` everywhere. Add a clear semantic
(`Set("")` or `Delete`). Wire per mode:
- default → existing **Redis** backend, owner-space `{ross, joanne}` (shared by
  the fleet).
- personal/team → **file** backend, owner-space `{selfID}` (per-pod isolated —
  unchanged storage, just behind the shared interface).

`ownergate`'s `ownsThread bool` argument becomes `store.Get()==selfID`. No agent
changes storage backend; PA stays per-pod-isolated. We just stop having two
shapes.

## Parameters: divergent constants become per-agent config (preserve current values)

These MUST become parameters, not silently-unified constants. Current values
preserved unless flagged:

| Param | Ross | Joanne | PA | Note |
|---|---|---|---|---|
| claude spawn timeout | 1h | **20m** | 1h | Joanne's 20m is a deliberate DM-latency choice — keep as param |
| drain cap | 60s | 60s | **bug: 30s local shadows 60s pkg** | **needs a value decision** (see open Q4) |
| session grace (queue) | 750ms | 750ms | **none** | unify to 750ms (PA inherits the race fix) |
| session-key namespace | static `ross-session-v1` | **instructions-hash** (auto-invalidates on persona deploy) | static | Joanne's must stay a `func() string`, not a const |
| queue persistence/replay | yes | yes | **none** | lifting the queue *adds* replay to PA → rename PA resume dir off `.ross-resume` first |
| workspace isolation | per-channel | per-conversation | per-channel | param: isolation strategy |
| team TTL / rate-limit | — | — | 60s / 8-per-min | team-mode params |

## Ingress: a strategy parameter, not a forced migration

Epic Decision 5 said "converge Ross/Joanne onto HTTP events." The audit found
that's ~80% backend work (the gateway has **zero** Ross/Joanne routing — every
event would 404), it revives the duplicate-spawn risk (Events-API retries vs
in-memory per-process dedup), and it **couples three failure domains** onto the
`events.makeacompany.ai` path that NXDOMAIN'd on 2026-06-24.

**Proposal:** the harness abstracts ingress as a **strategy** (`socket |
http-events`). Ross/Joanne keep Socket Mode; PA keeps HTTP events. We get the
code-dedup win **without** the risky migration. Actual convergence onto one
ingress is **deferred** to its own slice (or dropped) — it does not block the
harness extraction and isn't required for "less code." (See open Q3.)

Whichever ingress: the harness must **keep replicas=1** for default-mode agents
(dedupe/queue/inflight/loop-ownership are in-process; HTTP lifts the
socket-singleton *deployment* constraint but not the correctness one).

## The one thing that genuinely must merge: handoff admission

Cross-agent handoff admission has **diverged** and this is a real (small) merge:
Ross admits any peer bot (`isHandoffFromPeerAgent`); PA admits only 🤝+Joanne
(`isHandoffFromJoanne`). The shared pre-gate needs one predicate with the policy
made explicit per mode (default fleet vs owner-gated). Decide the unified policy
during this work; don't let the extraction pick a winner silently.

## OAuth pool: N-slot, not two keys (Grant, 2026-06-25)

The shared pool is **not** locked to two keys. Design (shipped as `core/oauthpool`,
PR #31):
- **Discovery:** every populated `CLAUDE_CODE_OAUTH_TOKEN` (slot 1) +
  `CLAUDE_CODE_OAUTH_TOKEN_2.._N`. Add a key → add a slot, no code change. Gaps
  tolerated; empty slots skipped.
- **Round-robin** over populated slots (canonical Joanne logic: skip-empty,
  single-populated = no alternate — propagates the 2026-06-20 fix to Ross/PA).
- **Failover iterates the whole pool** (`Others(tried)`), not a two-key bounce.
- **Per-slot cooldown** (`MarkLimited(label, until)`): a rate-limited account is
  skipped until its window resets — the resilience win during a sustained
  squeeze. `Next` falls back to the soonest-recovering slot if all are cooling.

**Coupled change at adoption:** `core/spawnretry`'s fixed `MaxAttempts = 2` /
`HasSecondSlot bool` generalize to "retry across remaining untried pool slots"
(bounded by pool size), driven by `Pool.Others`. The per-attempt safety gates
(fast-fail, no output posted, not aborted) are unchanged. This is where the
richer per-slot failover (epic's deferred OAuth-resilience item) actually lands.

## Extraction sequencing (safe-first — the actual rollout)

Pure-LOC-win, no gate semantics, fast-revert-safe — do these FIRST:
1. `oauth_pool` (triplicated) → core/harness
2. `session_queue` (near-identical; PA gains grace+replay — verify resume dir)
3. `streaming`
4. `resume` markers

Then the harder, design-dependent slice:
5. `ThreadOwnership` unified interface + clear semantic
6. unified pre-gate (dedupe, kill-switch, **handoff predicate**, ingress strategy)
7. mode-selected gate dispatch (ownergate XOR threadowner) + `handleMessage`

Each slice ships behind the existing per-agent path, one at a time, with
fast-revert (epic Decision 8) as the safety net. Env `AGENT_*` rename folds in
here with a fallback chain (epic Decision 7), since the harness reads env in one
place (`resolveIdentity`).

## Resolved decisions (2026-06-25, consensus with Grant)

- **Q1 — Gate model: RESOLVED → mode-selected.** ownergate XOR threadowner by
  mode; NOT a single merged decision function.
- **Q2 — Thread store: RESOLVED → unify.** Adopt `threadowner.Store` everywhere
  with an added clear semantic; PA stays file-backed/per-pod behind the shared
  interface (no storage-backend change).
- **Q3 — Ingress: RESOLVED → strategy param, defer convergence.** Ross/Joanne
  stay Socket Mode; harness supports `socket | http-events` as a param. The
  HTTP convergence (epic Decision 5) is **deferred** to its own future slice —
  same LOC win, none of the migration/blast-radius risk. Default-mode agents
  stay **replicas=1** regardless of ingress.
- **Q4 — PA drainCap: RESOLVED → 60s.** The 30s local const is an accidental
  shadow; standardize on the 60s package default all three intended.
- **Q5 — Handoff predicate: RESOLVED → mode-specific.** Default-mode = "any peer
  bot" (the Ross/Joanne fleet); owner-mode = "🤝 + specific peer only". Preserve
  today's behavior per mode; just make the rule explicit in the shared pre-gate.
- **Q6 — End-state: RESOLVED → keep deferred.** Decide one-binary vs
  N-entrypoints at the end of the extraction, not now. The harness package +
  thin per-agent `main` keeps both possible.

All six settled → the gate slice is unblocked for implementation once the
safe-first extractions (oauth_pool → session_queue → streaming → resume) land.
