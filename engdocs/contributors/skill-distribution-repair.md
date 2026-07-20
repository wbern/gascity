---
title: "Skill distribution — how it works, and the gci-aam5 repair"
audience: gas-city-infra/devops (execution) + architects (reference)
status: verified 2026-07-20 against deployed binary fork-ce1476243 and develop 8db4d32a2
---

# Skill distribution: how it actually works (verified reference)

This is the ground-truth mechanics for how a `gas-city-basics` skill reaches a
running agent, established empirically (reproduced on the deployed binary, not
read-only inference). It supersedes the stale claims in the
`gas-city-authoring-skill` runbook (corrected text at the bottom).

## The five things that must all be true for a skill to reach an agent

1. **The pack is mounted.** A `skills/` directory is only walked if its pack is
   either the **city-root pack** (`<city>/skills/`) or **imported** via
   `[imports.<binding>]`. A pack dir sitting under `packs/` that nobody imports
   reaches no one.
   - Live fact: `/Users/willi/gc2/pack.toml` has `[imports.basics] source =
     "packs/gas-city-basics"`. **gas-city-basics IS mounted** (binding `basics`).
     (It is declared in the city **pack.toml**, not `city.toml`'s
     `[rigs.imports]` — both placements work; this is why a city.toml-only grep
     missed it.)
2. **The skill dir contains a `SKILL.md`.** `discoverImportedSkillEntries`
   **silently skips** any subdirectory that lacks `SKILL.md`
   (`os.Stat(<dir>/SKILL.md)` fails → skipped, no warning). An empty
   `skills/<name>/` dir contributes nothing.
3. **The catalog surfaces it, binding-qualified.** An imported-pack skill appears
   as `<binding>.<name>` — e.g. `basics.gas-city-self-restart` — with `FROM
   basics`. City-**root** skills (`<city>/skills/`) appear **bare** (`FROM city`).
   The SKILL.md frontmatter `name:` stays bare (`gas-city-<thing>`); the binding
   prefix is a catalog/sink-leaf concern.
4. **Materialization writes the sink.** Two stages create symlinks into the
   agent's vendor sink (`<scope-root>/.claude/skills/<name>`, `.codex/skills`,
   `.gemini`, `.opencode`): **stage-1** at `gc start` + every supervisor tick,
   **stage-2** as a per-session `gc internal materialize-skills` PreStart.
5. **The agent picks it up at (re)start.** New sessions get the sink via stage-2
   PreStart. Already-running sessions only converge on a supervisor tick/restart
   — see the wake caveat below.

## Evidence (deployed binary fork-ce1476243)

Reproduced the exact live shape — `[imports.basics]` → `packs/gas-city-basics`,
one empty `gas-city-self-restart/` dir + one populated control skill — and ran
`gc skill list`:

```
NAME                      FROM    PATH
basics.populated-control  basics  .../skills/populated-control/SKILL.md   # populated → visible
# basics.gas-city-self-restart (empty dir) → ABSENT, matching the live symptom
```

Live `gc skill list` shows only the 7 `core.*` builtins because all 13
`packs/gas-city-basics/skills/*/` dirs are empty (0 `SKILL.md`); the template
carries all 14.

## The recycle behaviour changed — don't trust the old runbook

The `gas-city-authoring-skill` runbook says a live skill edit "HOT-RECYCLES the
whole pack." **That is obsolete.** Two fork-local fixes stop it:

- `8dd704296` (gci-rgfm) — skill **content** edits no longer drift the session.
- `a22adca0b` (gcw-nj38) — the `skills:*` keyspace is excluded from the
  restart-gating `CoreFingerprint`, so skill **set** changes (add/remove/rename)
  no longer drain running sessions.

Both are in the deployed binary (`git merge-base --is-ancestor` confirmed) and in
develop; both drain-neutrality regression tests are green. **Neither is in
`upstream/main`** — they are an upstreaming candidate (upstream still carries the
whole-city-recycle-on-skill-edit bug; see open issues below).

### Wake caveat (the new gotcha)

Because edits no longer force a recycle, **already-running agents will NOT
auto-pick-up newly-populated skills.** Worse, upstream **#3459** (OPEN) reports
stage-1 tick materialization does not reliably create/remove the sink symlinks on
a running city until a full supervisor restart — the skill is "advertised but not
loadable" until then. So after populating:

- **New sessions** get the skills automatically (stage-2 PreStart).
- **Running sessions** need a deliberate **wake/recycle** (or supervisor restart)
  to gain them. Plan this; it will not self-heal.

## Related work — we are not alone

- Open upstream, same "skills don't reach every agent" class: **#3643** (mayor
  skill missing across rigs/agents), **#3460** (providerless builtin agents
  skipped), **#4131** (rig+city name-collision shadowing), **#3459** (tick
  materialization unreliable), **#4396** P1 (`gc doctor` hangs on skill
  collision).
- Closed: **#669** (settle pack-v2 skills model), **#1097** (PreStart-race drain
  loop), **#4130** (sink symlink self-heal, already in our binary).

# gci-aam5 repair — devops execution plan

**Corrected root cause:** NOT "mounted nowhere" (it is mounted via
`[imports.basics]`). The sole cause is that the **live skill dirs are empty** —
content was never synced from the template. Fix = populate; no mount change, no
config change, no gascity-source change.

```sh
TPL=/Users/willi/gc2/rigs/gas-city-infra/city-template/packs/gas-city-basics/skills
LIVE=/Users/willi/gc2/packs/gas-city-basics/skills

# 1. sync template content -> live (fills the empty dirs)
cp -Rf "$TPL/." "$LIVE/"

# 2. verify 1:1 and catalog visibility
diff -r "$LIVE" "$TPL"            # expect: no output
gc skill list | grep '^basics\.' # expect: 14 basics.* skills now listed

# 3. deliberately wake running agents so they load the skills (see wake caveat)
#    new sessions get them free; running ones need a recycle/wake.
```

- Do it in a quiet window as a courtesy, but note the **fleet-recycle risk the
  old runbook warns about no longer applies** — the populate will not stampede a
  recycle; the risk is now the opposite (agents won't get skills without a wake).
- Confirm per-agent: `gc skill list --agent <a-running-agent>` shows the
  `basics.*` set.

# Corrected `gas-city-authoring-skill` runbook (replace template + live)

Apply this text to BOTH
`.../city-template/packs/gas-city-basics/skills/gas-city-authoring-skill/SKILL.md`
and the live copy. Changes: (a) add the mount prerequisite + binding-qualified
naming; (b) replace the false "hot-recycle" section with the wake reality.

## Diff summary (what to change)

1. **"Where a skill lives" section** — add a first bullet:
   > **Prerequisite: the pack must be mounted.** `gas-city-basics` is mounted
   > city-wide via `[imports.basics]` in `/Users/willi/gc2/pack.toml`. A skill
   > only reaches agents once its pack is mounted (or is the city-root pack) AND
   > the dir contains a `SKILL.md`. An empty `skills/<name>/` dir is silently
   > skipped.
2. **"Anatomy" section** — after "auto-discovered", add:
   > Auto-discovery applies **within a mounted pack**, not to any dir on disk.
   > Imported-pack skills surface **binding-qualified** as `basics.<name>` in
   > `gc skill list`; keep the frontmatter `name:` bare (`gas-city-<thing>`).
3. **Replace the entire "A LIVE skill edit HOT-RECYCLES the whole pack"
   section** with:

   > ## A live skill edit does NOT recycle — you must wake agents
   >
   > As of `a22adca0b` + `8dd704296` (fork-local), editing/adding/removing a live
   > `packs/<pack>/skills/**` skill **no longer drains or recycles** running
   > sessions — the `skills:*` keyspace is excluded from the restart-gating
   > fingerprint and content edits use a presence marker. Consequence: **running
   > agents will not pick up the change on their own.**
   >
   > - **New sessions** get the skill automatically (stage-2 materialize PreStart).
   > - **Running sessions** need a deliberate **wake/recycle** (worst case a
   >   supervisor restart) to gain it — see upstream #3459 (tick materialization
   >   is unreliable on a running city). Plan the wake; it will not self-heal.
   >
   > The old "quiet window to avoid a fleet stampede" caution is obsolete; the
   > new discipline is "remember to wake the agents that should get the skill."
4. **"Verify" section** — change the third bullet to:
   > - `gc skill list` shows the skill as `basics.<name>`; `gc skill list --agent
   >   <name>` confirms a specific running agent sees it after a wake.
