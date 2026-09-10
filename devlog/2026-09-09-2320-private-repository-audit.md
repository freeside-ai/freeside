# Audit Private Repositories Without Paid Governance Features

The owner requires private repository support. Making a controlled fixture
public does not solve the onboarding defect for ordinary private work.

GitHub returns HTTP 403 with a plan-upgrade message for branch protection and
ruleset listing on a private repository where those features are unavailable.
The same repository's workflow, environment and runner reads succeed. GitHub
documents paid-plan requirements for private-repository
[branch protections](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/managing-protected-branches/about-protected-branches)
and [rulesets](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/managing-rulesets/about-rulesets).
Freeside previously classified every such 403 as an audit failure.

We chose explicit capability evidence over either making the repository public
or treating every 403 as an empty setting. Only the exact observed GitHub plan
response at the branch-protection endpoint or the first ruleset-list page
becomes `{"plan_unavailable":true}`. Other permissions, authentication, rate
limits, malformed responses and partial ruleset collection remain errors.
Provider error text is transient and never retained or rendered.

These governance settings contribute to the audit digest, not the derived
automation privilege flags. The unavailable marker preserves that distinction:
all workflow and effective-authority reads remain mandatory, the operator
reviews the resulting profile, and a later capability/configuration change
produces a different digest and uses the existing reapproval gate. Existing
successful and absent-feature evidence keeps its encoding.

Independent checks try the same response at required-authority endpoints,
after a complete ruleset page and at a ruleset detail; each must fail. Other
403 messages and malformed copies must fail without exposing response text.
The successful unavailable cases must preserve all effective-authority facts
and all other retained evidence.

The independent refutation review found no confirmed defect. The proposed
failure cases of skipping required authority reads, accepting a partial
ruleset audit and exposing provider response text are rejected by the focused
regression tests. Capability changes produce distinct approval-bound digests.

Revisit when GitHub changes this response or exposes explicit feature
availability independently of its plan-gated governance APIs.
