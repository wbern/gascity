---
title: "Codex crm polecat spawn-loop — incident findings + confidence audit (2026-07-20)"
author: gas-city-wbern/architect
status: cause CONFIRMED; fix not yet built
scope: crm/gastown.polecat (codex-polecat, gpt-5.6-terra) on running binary fork-ce1476243
---

# What happened

William switched the crm crew to an all-codex pool (Claude quota low). The codex
polecats spawned and died ~1/min without claiming any of the RED PR-backlog beads
("codex says there's no work before it dies"). Investigated jointly by
gas-city-infra/devops and gas-city-wbern/architect. Multiple layered causes; one
deep recurring root.

# Confidence scale

- **CONFIRMED** — directly verified: live read + code anchor and/or reproduced. High confidence.
- **STRONG** — multiple corroborating signals; not isolated-repro-proven.
- **HYPOTHESIS** — plausible, not verified.
- **RETRACTED** — asserted then disproved (owned).

# Findings + confidence

| # | Claim | Confidence | Evidence / basis |
|---|-------|-----------|------------------|
| 1 | **Root cause = nvf76 claim-identity gap (gci-310k).** `gc hook --claim` matches ownership by exact identity string; a fresh pool member gets a new runtime id but work stays pinned to the dead instance's id → new id not in `IdentityCandidates` → can't adopt its pool's claimed bead, can't fresh-claim (already assigned) → `no_work` → respawn loop. | **CONFIRMED** | 3 axes: (a) code `cmd/gc/cmd_hook_claim.go:271` `hookClaimMatchesRoute` + `:279` `hookClaimHasIdentity(claimed.Assignee, opts.IdentityCandidates)`; (b) history — no identity-normalization commit exists (`git log --grep` empty) → not in fork-ce1476243; (c) live — `crm-1g4vjm.7` pinned `gc.session_name=…kb-gc2-zzw2o`, `crm-1g4vjm.1` `…gc2-pttkk`, while live instances are `gc2-o2iiz`/`gc2-ldpj9`. Also independently root-caused by devops (gci-310k, mail gc2-wisp-9jmga4, 12:47) with kb-pool repro. |
| 2 | nvf76 is the **SOLE remaining** blocker after the compounding layers were fixed. | **STRONG (not isolated-repro-proven)** | All corroborating signals point here + config caveat names it, but I have NOT run a clean isolated scratch repro that rules out every other factor. This is the one thing left to prove during the fix. |
| 3 | The deployed `481dd9b` "claim-first" pack change only **masks** the symptom; claim still yields `no_work`. | **STRONG** | Stated in the gci-310k archive/bead; consistent with live churn persisting. Not personally re-derived from the pack source. |
| 4 | **`gc sling --no-formula` suppresses the pool default formula** (`mol-polecat-work`), producing a bare auto-convoy with no worker child → nothing claimable. Compounding cause devops introduced in the reject fix + manual re-slings. | **CONFIRMED** | `cmd/gc/cmd_sling.go:152` flag doc "suppress default formula (route raw bead)"; `:877` applies default unless `--no-formula`; `agents/polecat/agent.toml:35` `default_sling_formula="mol-polecat-work"`; `mol-steve-triage.toml:118` documents the correct pattern; devops observed formula=null/no-worker convoys. |
| 5 | **Sling idempotency trap** — `gc sling` skips ("already routed … skipping") when `metadata.gc.routed_to` is set even if the prior molecule was orphaned; must clear `routed_to` first. | **CONFIRMED** | Live `--dry-run` on crm-1g4vjm.6/k3dtde printed "already routed … Without --force, sling would skip routing"; devops reproduced. |
| 6 | **Removing native bd broke claiming.** `~/go/bin/bd` → `bd.disabled-devops-20260720`, but `GC_BD_REAL` still points at it → shim hard-fails → shelled `bd` reads "no work". Compounding, self-inflicted, now resolved. | **CONFIRMED** | `bd.disabled-devops-20260720` on disk; `.gc/shimbin/bd --version` → `stat …/go/bin/bd: no such file or directory`; devops admitted (mail gc2-wisp-f8r73t) + restored (bd 1.1.0). |
| 7 | **"~60s cold-start timeout is the cause."** | **RETRACTED** | I pattern-matched off `session.cold_start_timeout` event names without checking the value. `[session].startup_timeout = "15m"` (city.toml:287; default 60s). Cold-start is NOT the ~1/min recycle. This was a guess dressed as a finding — the miss William caught the pattern of. |
| 8 | Skill distribution (gci-aam5): **gas-city-basics IS mounted** (`[imports.basics]` in `/Users/willi/gc2/pack.toml`), and the real cause was **empty live skill dirs** (0 SKILL.md; template had 14). | **CONFIRMED** | Live pack.toml read; `find` (0 vs 14); reproduced on deployed binary (empty dir absent, populated shows `basics.<name>`); devops executed the sync → 14 `basics.*` live (mail gc2-wisp-2dtg6h). |
| 9 | Skill edits no longer recycle the fleet (a22adca0b + 8dd704296), so a live skill populate needs a **manual wake** to reach running agents. | **CONFIRMED (mechanism) / HYPOTHESIS (live wake)** | Commits present in binary (`merge-base --is-ancestor`), drain-neutrality tests green; the "needs a wake" operational consequence ties to open upstream #3459 — not personally live-repro'd. |
| 10 | Provider brittleness: switching claude→codex silently broke flows (resume-only handoffs + `--no-formula` + shim floor). Filed durable fix **gc2-z7j83 (P1)**. | **CONFIRMED (pattern)** | The three sub-bugs above are each CONFIRMED; the "provider-coupling" framing is my synthesis, not a separate measurement. |

# The fix (gc-core, architect lane — NOT yet built)

Per gci-310k: (1) normalize claim identity to canonical binding-id + pool-base so a
fresh member recognizes its pool's work; (2) release assignee on instance death /
own by pool-eligibility not dead-instance id; (3) dispatcher-executed
drain/workflow-finalize. Do NOT rely on the deterministic claim-first mask.
Prereq/adjacent: dedupe the duplicate gc + bd binaries on PATH (deployment-integrity
risk that a fixed binary may not be the one that runs).

# Honest self-assessment

- The **root cause (nvf76) is CONFIRMED**, but the claim that it's the *sole* remaining
  blocker is **STRONG, not proven** — the clean isolated repro is still owed and is
  step 1 of the fix.
- I got **cold-start wrong** (#7) by trusting an event name over the config value. That
  is the calibration lesson: check the threshold, don't pattern-match the symptom.
- Everything tagged CONFIRMED has a live read or code anchor behind it; treat STRONG/
  HYPOTHESIS rows as still-falsifiable.

## Update — fix investigation (2026-07-20, later)

The reclamation reaper already exists in core and is deployed: `repairStrandedPoolWorkerBead` (commit 7eb0c7045 / PR #4088), gated by `poolFreeable && hasAssignedWork && !storeQueryPartial && marker aged past strandedRepairConfirmGrace(2m)`. So nvf76 is **"the deployed reaper isn't firing,"** not "no reaper." Packs have zero lifecycle reaping (only mol-polecat-work + business monitors) — reaping is core's job per SDK self-sufficiency.

**Hypothesis tested and REFUTED (deterministic reconcile test):** the demand-deadlock idea — that pending pool demand sets `shouldWake=true` → `poolFreeable=false` → repair skipped. Test `TestReconcileSessionBeads_StrandedRepairVsPoolDemand_gci310k` shows the reaper releases stranded work **with or without** `poolDesired` demand. So `shouldWake`/demand is NOT the gate. (Committed as a regression guard.)

**Remaining gate candidates (need a live trace / quiesced pool to disambiguate):**
1. `storeQueryPartial` true under the fork's Dolt latency (gci-8qm3) → repair skipped every tick. **Leading candidate** — would make nvf76 partly a *symptom* of the store-determinism issue; fix is store health (devops) or decoupling repair from the non-degraded-read gate.
2. Confirmation marker never ages 2m — less likely, since each respawn mints a NEW session bead (the dead bead's marker should age undisturbed).
3. `hasAssignedWork` false because assignees were manually released — then the current loop is "codex not claiming UNASSIGNED work," a different proximate cause than stranded-repair.

**Honest status:** cause 100% (repro green); reaper exists + works in isolation (test green); wrong fix (demand) ruled out. Pinning the live gate needs `gc trace` on a quiesced pool per reconciler-debugging.md — blocked by live churn.

## CONFIRMED ROOT CAUSE (2026-07-20, corroborated by a live polecat's self-diagnosis)

After ruling out every layer (query stable=8, codex-env store read returns work, all readiness filters pass the head bead crm-yeb98x, route matches GC_TEMPLATE=crm/gastown.polecat), the actual root — independently reached by a live crm codex polecat and matching gci-a8y — is a SPAWN/CLAIM invariant gap:

- The pool worker is spawned FOR a specific bead: env carries GC_TRIGGER_WORK_BEAD_ID=crm-1g4vjm.4 (+ GC_TRIGGER_BEAD_ID, GC_TRIGGER_BEAD_STORE_REF=rig:crm). But spawn does NOT atomically claim/pin that bead — it stays open+unassigned.
- The mandated startup `gc hook --claim --drain-ack` is UNSCOPED: it runs a global ready query that does NOT constrain to GC_TRIGGER_WORK_BEAD_ID. It returns no_work or surfaces the wrong item (a polecat's read-only `gc hook` selected gc2-z7j83 — a P1 non-CRM bead not routed to this pool — and correctly refused to steal it, then parked).

Net: nothing makes a worker claim its OWN trigger bead; the generic claim is unscoped → no_work / wrong-task → trigger bead stays unassigned → pool re-spawns for the same demand → loop. This is gci-a8y, pinned.

### The fix (gc-core, well-defined)
1. On pool-worker spawn, when GC_TRIGGER_WORK_BEAD_ID is set: validate it is routed to this pool and ATOMICALLY CLAIM that exact bead onto the spawned session (pin it). No unscoped race.
2. `gc hook --claim` must PRIORITIZE/REQUIRE the trigger bead when GC_TRIGGER_WORK_BEAD_ID is present; fall back to the generic unscoped query only for genuinely pool-idle sessions.
3. Hygiene: `gc prime --hook` injects only a timestamp today — it should enforce/communicate the trigger-claim invariant. Separately, 768 stale is_blocked flags (bd recompute-blocked) can hide ready work but are not this cause.

Separate/real: the ~60s codex cold-start crash (gc2-z7j83); nvf76 assigned-to-dead adoption gap (green repro, this repo); reaper works (green regression guard). Those are distinct from THIS loop, whose cause is the unclaimed-trigger + unscoped-hook gap above.
