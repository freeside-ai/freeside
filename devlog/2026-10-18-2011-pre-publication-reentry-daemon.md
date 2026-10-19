# The Daemon Re-Enters A Run Whose Base Advanced Before Publication

Issue #2058's second PR implements plan revision 89
(`devlog/2026-10-18-0940-pre-publication-base-advance.md`). A run whose
publication audit refuses with `ErrTargetBaseAdvanced`, with no publication
intent committed, now seals a daemon-authored re-entry, re-verifies and
re-reviews the prospective merge of its unchanged head into the new base, and
publishes that head against the new base. This note records where the code
departs from the plan text, the trust boundary it keeps, and the rejected
shapes.

## Escalate Remediation Findings Instead Of Remediating

Chose to escalate a re-review round that routes findings to remediation to a
person on a `review_dispute`, and to make `AllowsRemediation` refuse the v5
authority, over the plan's "ordinary adjudication and remediation". This
departs from an owner-approved plan decision, so it is proposed, not settled.
The assumption that changed: the plan assumed a remediation could be admitted
at the re-entry base. Production cannot do that today. A remediation is
admitted at the base the composition derives, and `claudeAdmissionDerivation`
in `daemon/cmd/freesided/claude_driver.go` returns the static configured base,
from which the driver also seeds the writer's workspace. A fix built there is
a candidate on the base the re-entry left behind, reviewed and verified
against a tree it was not built on.

Rejected: rebinding the remediation's source tree to the merge inside the
engine (tried first). The admission record still named the old base, so every
downstream proof disagreed with the workspace. Rejected: changing the
admission derivation and seeding in `cmd/freesided` here. It is outside the
declared scope and changes how every remediation is admitted. Rejected:
silently skipping remediation findings. Escalation keeps them in front of a
person, which is what the published re-entry already does (#1767 decision 1).
Follow-up: #2071.

## Carry Dispositions Through History, Re-Adjudicate Re-Emissions

Chose to carry the earlier rounds' declines and defers into the published
history through disposition lineage segments, and to adjudicate every finding
the re-review emits, over implementing the plan's digest-keyed keep rule in
this PR. The plan keeps a re-emitted declined or deferred identity's
disposition when its bound specification, instruction, and policy digests are
unchanged. No ordinary round carries a re-emission today, so the keep rule is
a new adjudication mechanism, not a binding of an existing one. The shipped
behavior errs safe: a re-emitted finding is decided again, never dropped. The
PR 1 note rejected re-adjudicating every re-emission as wasteful, so this is
also a proposed departure. Follow-up: #2072.

## The Base Comes Only From The Daemon's Own Audit

The v5 authority's base is the tip of the latest workflow audit the daemon
recorded for the run's repository (`LatestRunWorkflowAuditTip`). The store
proves every coordinate from its own records, on every read and again at the
seal:

- Every read: the prior review is the round before, at the same head and a
  different base; the chain from the root ready item runs through unpublished
  re-entries only and nothing on it published; the base is some recorded
  audit tip of the run's repository.
- At the seal only: the base is the latest audit tip, the predecessor is the
  chain tip, and the publication being replaced still holds a bare
  reservation. A committed intent may already have reached the forge, so it
  converges through recovery instead of being replaced by a second PR.
- The producer proof admits only the invocation that exported the head at an
  earlier base.

The publish decision reads the authority from the store, never from the task
payload, and requires the fresh audit's tip to equal the authority's base. A
further advance therefore refuses again and seals the next link. Audits
record no ref, so the every-read gate cannot tell a target-branch tip from
another branch's tip in the same repository; it does not need to, because the
decision audits the target branch afresh. Refute-first findings and their
outcomes are recorded below.

## Blocks Name Their Cause

A conflicting merge, a verification failure at the new base, and a
`hard_round_limit` with no round left each end in `publish_blocked` with hold
reason `base_advanced` and a distinct constant reason. The reasons are
constants, so a conflict's reason cannot list the conflicting paths; paths
are candidate-controlled text and stay out of item reasons, as the transport's
conflict error already keeps them out of error text.

## Known Limitations

- The PR body's verification report and the authoring checkpoint still name
  the admitted base, not the re-entry base. The source-issue closure approval
  binds the re-entry base, so an approval for the admitted base never
  authorizes `Closes`.
- A v5 block offers only the stop action, with a reason telling the operator
  to start a new run.
- A later feedback or remediation cycle on a PR a v5 cycle published is
  admitted at the configured base, so its publication decision refuses on the
  base advance and blocks. Same cause as the remediation escalation; tracked
  in #2071.
- The seal takes the repository's latest audit, which a concurrent run can
  record between the refusal and the seal (#2073). It fails safe: the run
  loses the re-entry or a round, never publishes stale.

## Refute-First Record

An independent fresh-context reviewer tried to break the change before
commit.

- Confirmed, fixed: a run that remediated before its base advanced was
  quarantined on re-entry. The remediation-marker check compared the marker's
  base with `admittedBaseSHA()`, which a v5 task answers with the new base.
  The check compares with the replay's observed base again, and a regression
  test covers a remediated head.
- Confirmed, deferred: the seal can take another run's audit (#2073), and
  cycles after a v5 publication are admitted at the stale base (#2071).
- Disproved by a check: a legacy review record with no base cannot reach the
  seal, because `ReviewRecord.Validate` rejects an empty base.
- Disproved: no seal after an intent commits (the seal and the bare
  reservation check share a transaction); the replaced publication cannot
  commit afterwards (`prepare` refuses it in the intent's transaction); the
  chain cannot branch (identity collision and the chain-tip check); no v5 on
  a published predecessor; the publish base never comes from the payload;
  the prospective merge SHA is admitted only through `AllowsProspectiveMerge`
  and persisted proofs; no forged producer; no remediation in a v5 cycle by
  any path; crash replay converges; v3 and v4 behavior is unchanged.

Revisit when #2071 lands per-attempt admission bases (lift the escalation and
the `AllowsRemediation` refusal), when #2072 lands the keep rule, or when
#2073 gives the audit record its ref, which would let the gates bind the base
to the target branch itself.
