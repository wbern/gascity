---
title: "Codex crm polecat spawn-loop — incident findings + confidence audit (2026-07-20)"
author: gas-city-wbern/architect
status: CORRECTED — root cause was NOT nvf76; real cause = bd-shim federation + unscoped claim; gc-core fix BUILT (gcw-htdl, TDD green)
scope: crm/gastown.polecat (codex-polecat, gpt-5.6-terra) on running binary fork-ce1476243
---

> **CORRECTION (2026-07-20, later): see "## CORRECTED ROOT CAUSE" at the bottom.**
> Finding #1 below (nvf76 identity-adoption as the blocker of the stuck beads) is
> **RETRACTED**. Live reads show every stuck open bead is UNASSIGNED — the claim
> code keys on the `assignee` field (null), so identity-adoption never applies to
> them. The real cause is the federated bd-shim surfacing global work + an
> unscoped claim. Fix built under gcw-htdl.

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

# CORRECTED ROOT CAUSE (2026-07-20, later — gcw-htdl)

The "sole remaining blocker = nvf76" claim (finding #2, STRONG-not-proven) did
not survive the isolated repro it owed. It is now **RETRACTED** for the stuck
beads, replaced by a CONFIRMED, live-reproduced cause.

**CONFIRMED (direct live A/B reads):**

1. Every stuck *open* bead — `crm-1g4vjm.2/.3/.4/.6/.7` — is **unassigned**
   (`assignee: null`), routed to `crm/gastown.polecat`, and present in
   `bd ready`. The claim code (`cmd_hook_claim.go`) reads the `assignee` field,
   not the stale `gc.session_name` metadata, so the nvf76 "can't adopt work
   assigned to a dead id" path never applies. The fresh-claim route path should
   claim them trivially.
2. **bd vs bd-shim is the differentiator.** `.gc/shimbin/bd -> ~/.local/bin/bdshim`
   is a *federated* beads client (binary strings: `federation_peers`,
   `GC_CITY_PATH`, `sovereignty`, `endpoint`). It **ignores `BEADS_DIR`** and
   reads the city-wide store; real `bd` reads the local rig `.beads`. Same crm
   dir: real `bd ready --assignee=""` → crm beads; shim → `gc2-z7j83`,
   `gc2-wisp-*` (city P1). `--metadata-field` masks it (only crm beads match the
   route), so the leak surfaces via the assigned/empty-target tiers.
3. Through the shim the polecat work_query surfaces global P1 `gc2-z7j83`
   (`routed=null`); `gc hook --claim`'s route filter rejects it → `no_work` →
   `--drain-ack` → session exits → pool respawns. Reproduced in session-template
   context (`GC_ALIAS=crm/gastown.nux`, `GC_TEMPLATE=crm/gastown.polecat`, shim on
   PATH) returning `gc2-z7j83` — furiosa/nux's exact symptom, independently.
4. `GC_TRIGGER_WORK_BEAD_ID`/`GC_TRIGGER_WORK_STORE_REF=rig:crm` are injected by
   the pack and resolvable (`bd show` works), but gc-core `gc hook` consumed them
   **nowhere** — the claim was unscoped.

**RULED OUT:** shim mishandling `--metadata-field` (filters identically to real
bd). The divergence is store *scope*, not filter semantics.

**FIX (gc-core lane — BUILT, gcw-htdl, TDD green):** `gc hook --claim` honors
the injected trigger env. `doHookTriggerClaim` resolves the exact bead by ID
(`store.Get`), runs readiness/route/ownership checks, and claims via the existing
atomic `store.Claim(id)` — immune to which store `bd ready` reads. A triggered
session never falls through to generic selection: gone / taken / misrouted /
lost-race ⇒ drain. Proof: 4 unit + 2 end-to-end tests (nux's exact contract),
RED verified genuine (disabling the branch makes the generic path claim
`gc2-z7j83`). Files: `cmd/gc/cmd_hook_claim.go`, `cmd/gc/cmd_hook.go`.

**Second lane — CORRECTED (do NOT narrow the shim).** An earlier draft said
"rig-scope the shim's reads." That is wrong: `bdshim` (fork-owned,
`cmd/bdshim`) is *intentionally* a city-wide federated client, and its width is
by design — narrowing it fights the federation architecture. The real
untriggered-path bug is a **missing filter, not excess scope**:

- **CONFIRMED (live A/B):** the shim's routed `ready` path drops `--assignee`.
  `cmd/bdshim/main.go` routes `ready` through the controller API
  (`DispatchViaAPI`) and only applies a client-side filter for `--metadata-field`
  (the guarded `list` path, `DispatchListMetadataGuarded`); there is no
  equivalent for `--assignee`. So `bd ready --assignee=<id>` through the shim
  returns the unfiltered city ready set (real `bd` returns empty; `bd list
  --assignee` and `bd ready --metadata-field` are honored). The default
  work_query's **assigned-ready tier** (`bd ready --assignee=$id`,
  `standardAssignedReadyWorkQueryScript`) therefore short-circuits on global P1
  work (`gc2-z7j83`) before the routed probe runs → the untriggered pool member
  sees unrelated work → claim route-filter drains → loop. This is the
  untriggered-path root cause (my trigger fix handles the demand-spawn case; this
  fixes the rest).
- **FIX (bd-shim lane, our code):** make the routed `ready` path honor
  `--assignee` — either forward it to the controller ready endpoint (if it
  supports assignee filtering) or apply a client-side assignee filter mirroring
  `DispatchListMetadataGuarded`. Keep the shim wide. Tracked separately.
- **Upstream note:** `bdshim` is fork-only (upstream has no such shim); real bd
  filters correctly, so upstream's identical work_query is unaffected. Upstream
  did refactor the work_query codegen (#4030/#4060, `Effective*Query`) — a
  divergence to track when porting, not related to this bug.

**Still owed:** (1) live proof of the gc-core trigger fix via rebuilt binary + a
codex polecat (deploy is DevOps's lane); (2) the bdshim `--assignee` filter fix
for untriggered pool members.
