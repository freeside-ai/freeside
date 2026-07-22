# Live Automation Trust Audit

Issue: #182

## Decisions

Chose a fresh, fail-closed GitHub observation immediately before each
publication decision over consuming the latest stored observation. The
observation content-addresses active and inactive workflow YAML, every file
beneath `.github/actions/`, Actions policy (including selected-action policy),
default workflow permissions, environment protection and secret names,
self-hosted runner identity and labels, branch protection, and full ruleset
details. Workflow parsing derives the profile's privilege axes. Remote
reusable workflows remain uninspectable under an installation token unless
their repository and immutable content can be proven, so they fail closed;
local reusable workflows are inspected and conservatively treated as
pull-request-reachable.

Added `gopkg.in/yaml.v3` rather than a bespoke workflow parser because the
live producer must distinguish scalar, sequence, and mapping forms while
preserving GitHub Actions' YAML shapes. Its `yaml.Node` API provides that
concrete capability with one direct dependency.

Chose explicit `allow_reusable_workflows`, `allow_package_publishing`, and
`allow_artifact_consumers` profile axes, with a trust-profile encoding bump,
over the prior permanently disallowed interpretation. Prior-version profiles
require owner re-approval because they never expressed those decisions.

Chose an append-only profile-activation record over insertion order or a
mutable current-profile pointer. Recording byte-identical profile content is
idempotent and does not activate it, which prevents a stale replay of A after
B from silently restoring A; an explicit activation represents the owner's
A-to-B-to-A decision without mutating content-addressed profiles.

Chose one SQLite transaction for the fresh audit record, active-profile read,
drift evaluation, authorization reconstruction, and publication-intent write.
This closes the local trust-check-to-intent race. GitHub reads and effects
cannot participate in that transaction, so a repository administrator can
still change settings after observation; a stable default-branch ref brackets
collection, every later publication re-audits, and any incomplete or changed
observation fails closed.

Did not add `WorkflowAudit` booleans for `PRExecution` or
`CandidateAutomationChanges`. The former is an owner-approved execution
topology whose repository-visible workflow reality is already bound into the
audit digest, while the latter is daemon-enforced importer policy rather than
a GitHub repository fact. Adding approximate booleans would imply an authority
the producer cannot reliably attest.

The resulting GitHub App repository-permission contract is `actions:read`,
`administration:read`, `contents:write`, `environments:read`,
`pull_requests:write`, and `metadata:read`. The write scopes are the existing
publication surface; the read scopes cover the live audit. Requested and
granted values remain losslessly recorded per mint.

## Refute-First Findings

The refute-first pass confirmed that list-level ruleset summaries omit the
rules that carry enforcement policy, so the producer fetches every ruleset's
details and canonicalizes their order. It also confirmed that environment
names alone omit protection authority and that workflow YAML alone omits local
composite-action implementation files; both are now included in the evidence
digest. The exact A-to-B-to-A activation test rejected insertion order as a
current-profile model. The drift-denial test confirmed that the fresh blocking
observation remains in the audit ledger while no publication intent survives.
Automated review then disproved the first workflow parser on two related
lenses: secret expressions were recognized in only one whitespace/dot form,
and workflow/job/container/service scopes were not all scanned. The corrected
parser recognizes dot and bracket secret-context references independent of
spacing and case across every scope that can inject a secret into a PR job.
The same review confirmed that hashing local composite actions without
deriving their executed steps could under-report artifact consumption, so
invoked composite actions are now parsed recursively and unsupported, missing,
or cyclic local actions fail closed.
A later review lens found that an omitted YAML permission block dropped the
repository default's package-write fact. Default read/write and
`pull_request_target` inheritance now carry package publishing forward, while
an explicit workflow or job reduction still clears it and OIDC remains
explicit-only.
The final review lens found that GitHub resolves environment names without
case sensitivity while the initial secret-bearing environment index did not.
The analyzer now normalizes both API-provided names and workflow references,
with scalar and object-form regression coverage.
A subsequent review found that matrix, expression, group, or custom-label
`runs-on` values could conceal a self-hosted runner. PR-job runner selection now
matches the complete literal label set against each repository-visible runner
and fails high for dynamic or group selection when any such runner exists. The
same review confirmed that reusable-workflow authority follows the plan's
PR-reachability boundary, while artifact consumption does not: a privileged
push or schedule workflow can consume artifacts produced by untrusted PR code.
Push-only reusable callers therefore do not set that axis, but local composite
actions are still inspected for trigger-independent artifact consumption;
secret facts remain PR-scoped, and `workflow_call` remains conservatively
PR-reachable without a complete caller graph.

The GitHub permission table confirms that listing environment secrets uses
`environments:read`, not `secrets:read`. The repository-ruleset detail endpoint
also supports `includes_parents=true`; the producer now requests that behavior
explicitly. Every total-count pagination loop now fails closed after 100 full
pages instead of returning partial evidence.

A later pass found two inventory boundaries. The Actions workflow registry is
not SHA-addressable, so the producer now enumerates workflow files from the
pinned audited tree and uses the registry only for active/disabled state; a
tree-only workflow is treated as active to fail high. The repository runner
endpoint cannot prove the absence of organization or enterprise runners, so
dynamic, group, multi-label, and unknown single-label selectors fail high even
when no repository runner is returned; only GitHub's documented standard
hosted labels clear the self-hosted axis without an inventory match. Artifact
consumption remains trigger-independent, which also covers privileged
`workflow_run` consumers. `pull_request_target` token authority deliberately
remains read-write fail-high even when YAML narrows permissions, because the
separate axis approves execution in the privileged base-repository context.

## Boundaries

The installed-App smoke test depends on the maintainer completing #233. Until
then, fixture-backed API tests prove endpoint use, scope failures, stable-ref
retry, settings sensitivity, privilege derivation, and response-body
redaction, but cannot prove the registration's granted permissions.

Revisit when GitHub offers a coherent repository-settings snapshot, when the
first managed repository requires a remote reusable workflow, when a
store-backed decision adapter needs decoration (replace concrete adapter
recognition with an explicit store publisher constructor), or when #233
provides an installed App against which to run the live smoke test.
