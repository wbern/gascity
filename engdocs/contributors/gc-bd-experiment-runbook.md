# `gc bd` controller-read experiment

This experiment compares only five controller-read shapes that already pass
the trusted managed-`bdshim` early-path gate:

- `show <id> --json`
- JSON `list` without metadata predicates
- the ephemeral JSON `query` shape
- `mol current <id>`
- `mol progress <id>`

`ready`, all writes and claims, explicit `--city`/`--rig`, ambient Dolt
overrides, metadata-filtered lists, and every passthrough/refuse shape bypass
the experiment. They retain their existing paths.

## Controls

The process-global environment controls are intentionally fail-closed:

```sh
GC_BD_EXPERIMENT_ARMS='shim=100,direct=0,legacy=0'
GC_BD_EXPERIMENT_GENERATION=1
```

Weights must name `shim`, `direct`, and `legacy`, total 100, and keep legacy
at or below 10. Invalid settings select `shim=100`. A temporary exact-arm
diagnostic override is `GC_BD_EXPERIMENT_FORCE_ARM=shim|direct|legacy`; known
shape overrides use `GC_BD_EXPERIMENT_SHAPE_OVERRIDES=show_json=direct`.

The immediate rollback is `GC_BD_EXPERIMENT_ARMS='shim=100,direct=0,legacy=0'`.
Do not use the force override for a broad rollout.

## Evidence and gates

Set `GC_BD_EXPERIMENT_LOG` to a protected JSONL file (otherwise a managed city
uses `.gc/bd-experiment.jsonl`). Records contain only schema, build, arm,
closed verb/shape, disposition, exit, stdout byte count, numeric config
generation, and two explicitly in-process timings. They never contain command
arguments, IDs, paths, environment, output, or hashes.

Analyze one build at a time:

```sh
bdexperiment-report /path/to/observations.jsonl
```

The report rejects malformed, mixed-schema, and mixed-build artifacts. Its
timings are not subprocess wall time; use the external `gc-bench` harness for
wall time, CPU, and RSS.

An operator may advance only in this order after the test and live-read gates
are green: `100/0/0`, then `95/5/0`, then `45/45/10`. Compare each arm within
the same shape and build. Stop and roll back to `100/0/0` for any arm-specific
correctness or error regression. Promotion requires no such regression plus a
predeclared direct-path performance threshold. Retiring `bdshim` is outside
this experiment and needs a separate bare-`bd` migration plan.
