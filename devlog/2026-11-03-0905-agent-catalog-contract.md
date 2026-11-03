# Agent Catalog Contract (#2494)

The issue body holds the contract (`agent_catalog` on the bootstrap,
`lineup_revision` on the heartbeat) and its plan comment the steps. AGENTS.md
makes a note mandatory for a contract change. This note records the choices
the contract left open.

## Decisions

- **Chose a catalog built once at startup over a projection per read.** The
  daemon loads the agent tree once, so `freesided` projects it with
  `signet.AgentCatalogOf` right after the Claude composition and hands signet
  a func that returns it. Bootstrap and heartbeat read no store row for it,
  and a projection error stops startup: the tree already parsed, so an error
  is a daemon bug, not an operator's.
- **Chose the heartbeat's `lineup_revision` over a sync revision bump.** A
  restart on a changed tree writes nothing, so no revision moves. The client
  compares the heartbeat's value with its adopted catalog's, as it already
  does for `unattended_operation`, and bootstraps on a difference. Null on
  both sides (no tree) settles without a bootstrap.
- **Chose no cache format bump.** `agentCatalog` is an additive optional in
  `CachedState`. A cache from before this change loads with a nil catalog, and
  the next heartbeat's non-null `lineup_revision` differs from it, which
  triggers the bootstrap that fills it. A bump would also discard rows for no
  gain.
- **Chose plain `$ref` over `allOf` for the catalog's non-nullable `Digest`
  and `EffortLevel` fields.** The spec uses `allOf` only to make a ref
  nullable. A non-nullable `allOf` would generate a `value1` wrapper in Swift
  for no reason.
- **Chose to repeat the offer lookup in signet over exporting agenttree's
  closure.** It is three lines and keeps `agenttree`'s API unchanged. It
  keys the offer by the agent's own route, as the closure does.
- **The mock answers a policy with no catalog as 500.** The daemon's routing
  hook returns `ErrOperatorSettingUnavailable` without a tree, which the
  HTTP layer leaves unclassified (500; see
  `2026-11-02-1338-usage-command-contract.md`). The mock matches that rather
  than inventing a 400.

Revisit when the tree becomes reloadable without a restart: the func passed
to `signet.WithAgentCatalog` must then read the live tree, though the
heartbeat comparison holds as it is.
