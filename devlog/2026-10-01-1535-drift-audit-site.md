# Drift Audit Site: Contract Shape, Cited Findings, and Fail-Safe

Work unit #1050, plan §7 "Review Drift". This note covers the drift-auditor
inference site: its authority contract, what a reversal may cite, how a failed
call is reported, and its limits. The site stops at its client method;
#1051 adds the trigger, loads the inputs, stores the artifact, and routes the
verdict. The artifact's own decisions are in
`2026-10-01-1220-drift-audit-contract.md`.

## Chose a Third Authority Contract Over Reusing the Adjudicator's

An annotate site carries exactly one authority contract. The site gets its own
`DriftAuditContract`: the verdicts, the confidence scale, the one work-reducing
verdict (`over_hardened`), and the classifier's severity mappings, ceilings,
and second-adjudication rules, copied unchanged.

Rejected:

- **Reuse `AdjudicationContract`.** Its validation requires the adjudicator's
  goal, compatibility, and route lattice and compares the rows with the
  domain's. The audit routes no finding, so it has no rows to declare, and
  loosening that validation to admit an empty lattice would weaken the
  adjudicator's check.
- **Reuse `AnnotationContract`.** Its work-reducing outputs are
  materiality-and-confidence cells. Mapping a verdict onto materiality would
  name the lattice wrongly for anyone inspecting the site.

The reversal list is not a lattice axis. The verdict fixes its cardinality, and
`domain.NewDriftAudit` enforces that.

The ceilings are carried even though this site applies none of them. Plan
§5.13 requires every ceiling-bounded annotation site to declare them, and plan
§7 applies the Finding Adjudication ceilings to a reversal unchanged: the route
gate needs a second adjudication before reversing the fix of a critical or high
finding. The contract's validation fails if they diverge from the classifier's.

## Chose to Check a Citation Against What the Site Was Shown

A reversal's finding must appear in the supplied dispositions or the supplied
adjudication entries. Any other id fails the audit.

The issue first said each cited finding must exist in the run's disposition
history. A finding from the audited round's own batch can have an adjudication
entry and no disposition yet, and plan §7 has the route gate park that citation
as a real verdict, which also uses up the run's one automatic route. Rejecting
it at the site would turn a verdict the plan parks into a failure the plan
ignores, so the looser rule is the one that matches the plan.

The check is an exact string match on the id. The model copies ids from its
input; a near-miss is an invented identity.

This is the first of two checks. `store.PutDriftAudit` re-checks every id
against the run's review records when #1051 stores the artifact, and the route
gate checks that the finding's latest disposition is `fixed`.

## Chose a Typed Error and No Artifact for Every Failure

`AuditDrift` returns a valid artifact or `ErrDriftAuditNotAvailable` with the
zero artifact. That covers the client's fallbacks (timeout, budget, driver
error, input over the limit, schema refusal), a parse or binding failure after
a schema-valid response, an artifact over `domain.MaxDriftAuditBytes`, and a
call the client refuses outright.

The fail-safe body is `{"verdict":null}`. It fails the site's own output
schema, so no path can parse the fallback bytes into an artifact. The
adjudicator's fail-safe is a schema-valid empty batch; an audit has no empty
verdict, and a schema-valid default would have to pick one.

With no artifact, a failure can't count as the run's first `over_hardened`
verdict. This unit has no engine path, so recording the failure against its
round and continuing the loop is #1051's half of the plan's fail-safe default.

The method parses the response a second time after the client has validated
it, and re-runs the schema check there. The duplication is deliberate: the
artifact is built from the bytes the method parsed, not from a claim that
another function checked them.

## Chose the Adjudicator's Limits and No Shortening

Timeout 120 seconds, 2 MiB of input, 10,000 compute units, audit sampling
every tenth call, 30-day retention. The output bound is
`domain.MaxDriftAuditBytes`. The daemon's shared per-root allowance is sized
to the largest site timeout, so matching the adjudicator's keeps it correct.

Two diffs and the specification must fit in the input bound. Over it, the call
fails safe and the round gets no verdict. Rejected: truncating or summarizing a
diff. An auditor shown part of a change would return a verdict about a change
the run doesn't have, with nothing in the artifact saying so. A diff that isn't
valid UTF-8 fails the same way.

The site makes one call per audit and never retries; `drift_audits` holds one
row per round.

The base and head commits bind the artifact and are not sent as fields. The
diff metrics already name both commits, and the model has no use for them
beyond that.

## Refute-First Findings

An independent pass tried to get a verdict out of responses that should fail,
with scratch tests through `AuditDrift` and the scripted driver.

Confirmed and fixed:

- **Case-folded keys became verdicts.** `encoding/json` matches object keys to
  struct fields case-insensitively. `{"verdict":"converged","Verdict":"stuck"}`
  is neither an unknown field nor a byte-exact duplicate to the shared strict
  decoder, so the later key won and the call returned a `stuck` artifact. The
  same worked for `VERDICT`, for `Finding_Id` inside a reversal, and for the
  Unicode fold `reverſals`. The site now requires the response and each
  reversal to carry exactly their keys, byte for byte, before the typed decode
  counts. No invented finding got through even before the fix, because the
  cited-finding check ran on the final value.
- **The contract's lattice was not tied to the domain.** A site could register
  a `DriftAuditContract` naming a verdict or confidence the artifact refuses.
  No artifact leaked, because the parser checks the domain's enums, but the
  inspectable contract would have been false. Validation now requires the
  domain's lists, as the adjudicator's requires the domain's rows.

Allowed by decision:

- **The shared strict decoder still folds key case for the other sites.** The
  finding adjudicator accepts `{"entries":[],"Entries":[...]}` and uses the
  second list. Changing the shared decoder changes every site's schema, which
  is outside this unit. Follow-up: #1675.
- **Invisible, non-whitespace text passes the nonempty check.** A zero-width
  space or `\u0000` as the whole explanation, undo, or rationale is accepted.
  The rule is `DriftAudit.Validate`'s, which this unit doesn't change, and the
  text is advisory prose a person reads on the card; it gates nothing.
- **A lone surrogate is stored as U+FFFD.** The decoder replaces it before
  validation, so the artifact is self-consistent and its digest covers the
  replaced text. Only the sampled response bytes differ. A finding id is
  matched after the same replacement, so it can only match an id the site was
  shown.
- **A nil client panics.** Every client method does; the daemon composes the
  client or doesn't register the engine's inference at all.

Disproved: byte-exact and escaped duplicate keys at either level; unknown and
binding fields (`run_id`, `digest`); `null`, object, or string reversals and a
`null` reversal element; numbers or booleans for strings; padded or upper-cased
verdicts; trailing data, a BOM, raw NUL, invalid UTF-8, a top-level array, an
empty body, and the fail-safe bytes; whitespace and case variants of a shown
finding id; repeated ids; a response at the output bound and one byte over; an
artifact over its bound once the binding fields are added; an audit-sample
persistence failure; an unbound round, run, commit, or digest; aliasing between
the returned artifact, the input, and the registered contract; an annotate site
with no contract or two, and a non-annotate site with one.

## Revisit When

- #1053's replay evidence shows audits often exceed the input bound. The
  answer is then a plan change on what the auditor sees, not a truncation
  here.
- #1051 finds a finding of the audited round with neither an adjudication
  entry nor a disposition when the audit runs. A correct citation of it would
  fail at the site, and the rule would widen to the store's (any finding a
  review record lists).
- The verdicts prove poor without finding text. The allowlist is plan §7's;
  widening it is a plan change.
- The site gains a retry, which needs more than one row per round.
