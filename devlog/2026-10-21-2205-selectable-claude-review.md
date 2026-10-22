# Selectable Claude Review

Issue #397. The `reviewer` lineup line now picks the production review
harness: an agent on `codex_cli` runs the Codex review, one on `claude_code`
runs a Claude review on the Claude ward adapter. The owner authorized the
promotion without shadow evidence on 2026-10-21 (plan revision 92).

## Decisions

- **The lineup selects the harness, not a flag or a setting.** The
  reviewer line already names an agent, and the agent's enrollment names a
  harness, so a second selector could only disagree with it. There is no
  `review.source` key and no API field: a review round's provider already
  comes from the admitted agent's lineage group (#898), so the clients show
  a Claude reviewer with no client change. This was planning default 1, and
  the owner did not veto it.
- **`-review-auth-mode` stays and must agree with the harness.** The
  routed review flags describe the routed reviewer whatever its harness.
  `setup_token` is the only mode a Claude reviewer accepts; Codex keeps
  `subscription` and `api_key`. Deriving the mode for Claude alone was
  rejected: one agree-or-refuse rule for both harnesses is simpler than a
  rule that differs by harness. Preflight and startup refuse a mismatch by
  name (planning default 2).
- **"Conformed" means a code-owned provider registry.** A harness has a
  routed review provider only when the ward keeps one that passes the
  review-source contract in CI; Codex and Claude both do. A reviewer line on
  any other harness is refused at startup (planning default 3).
- **The daemon composes one harness at start; each launch rechecks it.**
  The review source, its lifecycle, and its recovery are built for the
  harness the reviewer line names when the daemon starts. A line moved
  later is refused at every review launch, by harness and by adapter vendor,
  until a restart recomposes the source. Recomposing on a line change while
  running was rejected: it would swap a review source under in-flight
  invocations whose recovery belongs to the other provider. The Codex
  recovery always runs, and one Claude lifecycle and recovery serve the
  routed and shadow Claude sources, so a restart onto either harness still
  reconciles invocations the other left.
- **`auth adopt -review-harness claude_code` emits the reviewer; the
  deputy accepts it** (planning default 4). Adopt writes
  `claude-review-default` on its own offer for `-review-model` and
  `-review-effort`, under the review identity's Claude enrollment, or the
  implementation identity's when `-review-auth-identity` names the same
  identity. The Codex tree
  and every Codex review configuration digest stay byte-identical; the
  cutover digest golden proves it.

## Accepted Consequences

- **Adopt cannot enroll a Claude review identity.** A Claude enrollment
  needs an attested account, and `-claude-account` attests the
  implementation identity's. The operator enrolls the review identity with
  `auth add -client claude_code` first; until then adopt reports it
  `unadoptable`. The deputy likewise records a Codex review identity itself
  but leaves a Claude one to `auth add`.
- **The setup token is bound to the enrollment by shape only.** The routed
  Claude review reads its token from `-review-auth-snapshot`, as the shadow
  source does. The daemon checks the file's shape, not which account issued
  it. A reviewer agent hand-authored on the writer's enrollment records the
  writer's `auth_identity` in its admission snapshot; that is allowed, and
  the clients show the shared lineage.
- **A Claude review binds the head, not the base.** Only the Codex review
  records an expected base. The review still covers the exact head it
  checks out. Follow-up: #2146.
- **Admission does not hold the adapter build.** It holds harness and
  vendor, so an agent on the Claude call adapter would pass admission and
  still run the ward review container. The gap predates this change for the
  shadow reviewer; launch coverage is #1980's.
- **`agent_tree.review_harness` is the one optional deputy key.** Absent
  means `codex_cli`, so every existing settings file keeps its meaning and
  its settings revision. Every other key stays required.

## Evidence

- **Refute-first pass** (`docs/agent-workflow.md` §refute-first): one
  fresh-context reviewer tried to refute the change. Outcomes:
  - Confirmed and fixed: an open contradiction or configuration item an
    earlier daemon parked as "Codex review ..." would stop matching the
    harness-neutral wording, and every reconcile of its run would fail after
    an upgrade. The comparison now accepts the earlier wording, and an
    integration test parks both items under it.
  - Confirmed and fixed: preflight took any `-review-auth-mode` and echoed
    it into `agent_selection` evidence, so a credential pasted into the flag
    reached the manifest. Preflight now refuses an unknown mode while
    parsing, without echoing it.
  - Disproved: no setup token or Codex `auth.json` reaches argv, the
    launcher environment, a digest, or the manifest. The routed Claude
    source is the ward Claude source the shadow arm runs; it mounts the
    snapshot read-only, and its launcher environment never carries the
    token (ward topology test). Preflight refuses the snapshot as review
    instructions.
  - Disproved: credentials do not cross harnesses. Codex refuses
    `setup_token`, Claude refuses the Codex modes, and the Claude source
    refuses a Codex `auth.json` by shape.
  - Disproved: a restart after a harness switch neither misroutes nor
    strands a review. A task pinned to the old digest stops at the
    configuration gate, an unlaunched request is re-admitted against the new
    source, and the provider-neutral Codex recovery always runs.
  - Disproved: Codex digests and existing Codex settings are unchanged.
  - Declined: a unit test that composes the routed Claude source in
    `cmd/freesided`. The ward lifecycles it needs have fixtures only inside
    the ward package, the source and lifecycle are the shadow arm's, and no
    test composes the routed Codex source either. A live deputy run with a
    Claude reviewer has not been done.
- **`scripts/run-real-work.sh` is gone** (#2096), so the issue's acceptance
  bullet for it has nothing to change; the deputy is the only real-work
  launch path.

Revisit when the Claude review needs a base binding, or a third harness
gains a routed review provider.
