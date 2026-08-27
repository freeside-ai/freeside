# Yield Convergence Preserves Candidate Review Authority

The owner chose structured `YieldHistory` on the diminishing-review item over
an untyped `Reason`-only summary because the human decision depends on ordered,
immutable round evidence. #964 widened that existing carrier without changing
its wire shape; #844 consumes it and keeps the daemon-authored reason as a
canonical binding to the run, round, head, policy, finding batch, and accepted
adjudication.

Chose a command-authorized `finish_now` store path over relaxing
`FindingAdjudication.AuthorizesFinalDisposition` or accepting caller-authored
deferred records. The existing `ReviewDispositionDeferred` is sufficient, but
the store reconstructs the exact concluded item and command, re-derives the
displayed yield, and re-proves the current policy, review record, finding batch,
and adjudication before writing the complete round in one transaction. The
general disposition path remains unchanged and rejects the same deferral when
that command authority is absent.

Chose candidate-bound finish semantics over a readiness exception.
`finish_now` dispositions complete the already reviewed head;
`apply_then_finish` applies the accepted round and owes one independent review
of the resulting candidate. Findings on that final review receive a fresh
version-bound item after adjudication, never an automatic deferral or another
automatic remediation cycle. `continue_under_policy` records its allowance as
the immutable concluded command under the bound policy digest, so restart
reconstruction resets only the low-yield streak while the hard round cap stays
terminal.

Adversarial reconstruction rejects mismatched item, version, subject run,
round, head, action, finding-batch or adjudication digest, stale policy, and
decoded-row tampering. This closes the returned-object trust boundary without
adding a decoded authority bit or a persistence migration.

Automated review and the follow-up refute pass found two reconstruction gaps:
the engine initially forgot continuation actions after dispositions became
complete, and the store authenticated a copied diminishing cause without
re-running the stopping rule. The corrected design makes the store-owned
decision-time evaluator the single predicate for both item creation and
command reconstruction, excludes action-written current-round dispositions,
and authenticates prior allowances through one cached causal walk. It also
pins one item identity per run and round. This preserves restart behavior and
prevents a canonical-looking fabricated item from authorizing deferral.
The next review found that decision-time disposition reads filtered decoded
run and round fields before validating their authoritative scope. They now
validate every row's finding, review, and remediation or adjudication binding
before excluding the exact current decision boundary; only included rows
follow diminishing-command authority, avoiding a recursive current-action
re-gate without letting copied-key corruption disappear. Prior diminishing
history likewise enumerates canonical run-and-round item identities, then
authenticates each item and command; it never filters on a decoded binding's
claimed round. Observation uses the same canonical enumeration so a coherently
rewritten copied run key cannot hide an accepted action from daemon telemetry,
and reconstructed commands are re-gated against the exact item's offered
actions before their effects are trusted. The item project is also rebound to
the authoritative run, preventing a coherently rewritten attention row from
presenting one project's decision as authority over another project's run.
Historical reconstruction scope-authenticates every disposition row before
selection, then follows command authority only for the same run's causally
prior rounds. Future `finish_now` dispositions therefore cannot recursively
re-enter the earlier `apply_then_finish` decision that made their review final.

The hard-cap round itself is terminal for further review work: its causal
diminishing gate remains available with `finish_now`, but it cannot offer
`apply_then_finish` or `continue_under_policy`, whose promised next review the
engine is forbidden to start.

Revisit when continuation gains human-specified bounds, diminishing decisions
receive a shared typed action binding, or readiness no longer requires a
complete disposition set on an independently reviewed candidate head.
