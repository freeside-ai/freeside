# policy

Per-project policy configuration: initiators, review policy, gates, budgets, security mode, telemetry (see `docs/plan.md` §5.12). The Phase 1 workflow is a Go state machine in `daemon/`; YAML here is policy only, never a pipeline DSL (a DSL waits for three genuinely different workflow shapes).

This directory is **control-plane** content: the daemon loads it only from an approved default-branch commit, running stages snapshot its digests, and workspace copies are data (see `docs/plan.md` §5.8). It holds the policy for work *on Freeside itself*, which becomes a managed repo only as the bootstrap test after the deliberately boring first repository proves the path (plan §11); a consumed repo's policy lives in that repo.

- **Toolchain:** YAML (policy values interpreted by the daemon's code-defined state machines).
- **Scope boundary:** policy configuration only. Changes here are control-plane changes: gated, reviewed like code, never batched silently into feature PRs.
- **Status:** layout reserved by the admitted-agent contract (below); no files until the #867 cutover's baseline patch lands the first agents.

## Agents, Fragments, and Lineups

The admitted-agent tree (`docs/plan.md` §5.4, revision 39; the contract types live in `daemon/internal/domain`). Every document here is operator-authored configuration, reviewed as a diff; the daemon never writes this tree. Enrollments, generations, and admissions are facts and live in the daemon's store, never here.

Reserved layout:

```text
policy/
  agents/
    <agent-name>            # one four-line agent document (who/through/running/asking)
    <agent-name>.attended   # unattended-eligible mark: exact agent + launch digests
  fragments/
    routes/<route-name>     # service operator, protocol, authorities, billing, terms basis
    adapters/<adapter-name> # Freeside adapter build, pinned harness build, capabilities
    offers/<route-name>/<offer-name>  # one route's offer of one model
  lineup                    # the deployment lineup: one line per role naming an agent
  agents.lock               # the name-to-digest map for the tree revision
```

Binding rules, fixed by the contract:

- **Names are tree-level.** The name-to-digest map (`agents.lock`) binds each name to the digest of its canonical body; no name enters any digest, so a rename changes nothing and a content edit changes exactly the digests that consumed it.
- **An offer lives under its route.** Which route an offer is authored under is a tree fact (the directory), supplied to resolution as context, deliberately outside the offer's digest.
- **The lineup names digests.** Each role line resolves to `<agent-name>@<agent-digest>` in the resolved policy (`lineup.role.<stage>` keys); a lineup that named only a name could follow a tree edit nobody approved for the role.
- **The attended mark rides beside the agent, outside the hashed body**, naming the exact agent and launch digests it was given for; a line edit is a different agent, so the mark does not carry.
