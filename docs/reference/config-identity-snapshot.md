# Resolved agent identity in config JSON

`gc config show --json` includes `resolved_agents`, an array of records with
`qualified_name`, `agent`, and `suspended`. Each record pairs the agent's native canonical
identity (including its import binding) with that agent's resolved configuration.
The records, `config.Providers`, and `config.Workspace.CapacityGroups` come from
the same config load.

Imported and legacy agents may share `Dir`, `Name`, and `Provider` while having
different canonical identities and policies. Consumers must use the paired
records rather than joining the config's agent list to a separate `gc agent
list` request, inferring binding prefixes, or matching list positions.

The projection is for display only. It does not change `config.Agent` JSON or
TOML persistence: runtime `BindingName` remains excluded. Empty inventories emit
an explicit empty array. A missing projection means the CLI does not supply
this contract; admission consumers must refuse rather than guess.

A snapshot does not reserve capacity. Native claim and runtime admission remain
responsible for changes after the snapshot. Consumers must still enforce
suspension, per-agent limits, shared group limits, and exact profile checks.

`suspended` is the effective native suspension decision, including runtime city
and rig holds sampled once for the response. It can be true even when
`agent.Suspended` is false. If runtime suspension state cannot be read, JSON
snapshot generation fails rather than reporting an incomplete admission view.
