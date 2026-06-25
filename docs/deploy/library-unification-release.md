# Library-unification — release readiness (deploy runbook)

As of 2026-06-25 the agent-unification library work is **merged to `main` on all
three agents but NOT deployed** — no agent release tag was cut this session.
This doc is the checklist for when we decide to ship it. (Epic: core#21; #24
dispatcher work is separate and NOT part of this deploy.)

## What's in prod RIGHT NOW (unchanged this session)
- Ross **v0.1.216**, Joanne **v0.1.176**, Personal-agent **v0.1.41** — the
  pre-unification harness (each agent's own local oauth/session/streaming/queue/
  resume). Only recent change deployed: the Sonnet-4.6 default flip.
- Nothing from the unification is running. No undeployed-bug exposure: prod runs
  the OLD local code, never the (since-fixed) core sessionqueue.

## What this deploy ships (the `main` delta over the deployed tags)
All three agents adopted the shared `claude-code-core` packages (pinned at
**v0.22.0**): oauthpool + spawnretry (N-slot OAuth failover), session (uniform
mapping + persona-hash), streaming (shared catalog + GWS audit + redaction for
all), resume (shared marker store). Ross + PA additionally adopted sessionqueue
(coalescing + persistence + the drain-GC race fix). Joanne's sessionqueue is
deferred to #24.

Behavior changes that ship with it (intended, signed off):
- Ross/PA gain N-slot OAuth failover; Joanne's OAuth fix propagated to Ross/PA.
- Joanne + PA gain the GWS tool-call audit + secret redaction.
- Sessions are persona-aware everywhere (persona/instructions edit → fresh
  session). **One-time session reset on first deploy** (each live thread starts
  one fresh Claude session; cross-thread memory lives in CLAUDE.md + auto-memory,
  so this is harmless — same as a restart).
- Joanne DMs go uniform per-thread; Joanne loop ticks share one session per loop.
- PA gains queue coalescing + boot-replay.
- Marker dirs standardized `.ross/.joanne-resume`→`.session-resume` and
  `.ross-queue`→`.session-queue`: existing in-flight markers orphan once on the
  cutover (harmless, 30m staleness-swept).

## Pre-deploy checklist
- [ ] All three agents on core **v0.22.0** (consistent baseline) — confirm
      `grep claude-code-core go.mod` in each.
- [ ] **PVC replay-context path** (the one cross-boundary gotcha): the owner-
      curated startup routine that READS `replay-context.json` still points at
      the OLD dir (`.ross-resume/` etc.). The Go code now writes it to
      `.session-resume/`. Until the PVC-side CLAUDE.md/startup-routine is
      repointed, the replay hint is silently not read — **graceful degradation**
      (normal startup, no crash). Either repoint it (PVC edit) at deploy, or
      accept the degraded hint until a follow-up. NOT a blocker.
- [ ] Decide deploy order + canary (below).

## Deploy steps (per agent — canonical recipe: BimRoss/.github RELEASING.md)
Each agent: cut a release tag → image build workflow → auto rancher-admin gitops
PR → Fleet roll. Bare semver, no `v` confusion on the promote input.

Suggested order + gate (prod-only, 50 paying users — use the smoke+canary we
scoped):
1. **Personal-agent first** (per-user pods → cheapest to canary). Tag PA →
   roll ONE pod via the per-owner image flow if available, else watch the first
   pod, then fleet. Smoke `U0SMOKE` first.
2. **Ross** (single replica) — tag → gitops → watch the one pod in prod. The
   roll IS the prod change (no staging); fast-revert is the safety net.
3. **Joanne** (single replica) — same. Watch onboarding paths (welcome/MPIM/
   intake) specifically since session keying + streaming changed there.

## Rollback (forward-only)
Revert the image tag in the agent's rancher-admin manifest (PA:
`makeacompany-ai/backend.yaml` `PERSONAL_AGENT_IMAGE`; Ross/Joanne: their
deployment manifests). Fleet reconciles back within minutes. Re-tag forward once
fixed.

## NOT in this deploy (the #24 dispatcher work)
core/gate, core/threadownership, core/harness.Dispatcher are tagged
(v0.23–v0.25) but **no agent imports them yet** — they're inert until the S3
adoption. This deploy is the library layer only.

## One-line resume pointer
Full state + remaining work: epic core#21 + the auto-memory entry
`project_agent_unification_epic`.
