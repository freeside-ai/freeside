# A Protected-Path Edit Fails the Run, and the Writer Is Told

Issue #3131. Tier A by path (the importer, admission authority, stage
intent, and Claude prompt render), so a mandatory note.

Chose to correct the documents and give the writer the protected list over
holding a protected-path candidate for an owner decision, because the
classes are non-waivable: no decision could release the held candidate, so
holding would only park a run that must be redone anyway.

## Finding

- **The code fails the run; the README said it held publication.** Under
  `publish_strict`, the default for implementer and remediator stages, a
  match in any control-plane class except `reviewer_instruction`, or in
  `git_metadata`, is fatal at import. The stage records a failed outcome
  ("N publish-blocking findings"), and the candidate never reaches
  verification, review, or a card. Production replay also requires every
  finding to be advisory, so no waiver path exists.
- **Git metadata was never waivable.** `daemon/README.md` and
  [`2026-11-12-0908-freeside-unprotected-paths.md`](2026-11-12-0908-freeside-unprotected-paths.md)
  called it a waivable finding; the code treats it as fatal like the
  control-plane classes. That note stays as written; this one corrects it.
- **Plan §5.8 already matches the code.** It calls these classes
  publish-blocking and non-waivable, so the plan needed no change.

## Decision

- **The documents say what the code does.** `daemon/README.md` now says a
  protected-path edit fails the writer's run at import, and where the
  operator sees it: the failed outcome's count, and the per-finding detail
  in the daemon's `released export definitively rejected by gauntlet
  containment` log line.
- **The writer gets the list at start.** The prompt ends with a "Protected
  paths" section listing each pattern whose match fails the run under the
  invocation's trust profile. The writer prompts tell it to leave those
  paths alone and, when the work needs one, to stop with a `scope_expansion`
  blocked outcome.
- **The list is advice, not authority.** `importer.Policy` builds it with
  the same `Finding.Fatal` predicate the pipeline applies, so the list and
  the gate can't disagree on a class. Import still derives its own patterns
  from the trust profile, so a stale or wrong list can mislead the writer
  but never lets an edit through.
- **The intent records the list.** Restart reconstruction re-renders the
  prompt from the recorded list rather than the current profile, so an
  owner re-approval mid-run can't quarantine an in-flight run. An intent
  recorded before the field existed renders no section and stays
  byte-identical.
- **An unprofiled attended start gets the built-in list.** An attended
  admission binds no profile, and its repository may have none approved
  yet. Import waits for one; start must not newly refuse that run, so the
  list falls back to the built-in patterns, which every profile keeps.

## Rejected

- **Hold the candidate for an owner decision.** It keeps the run's work,
  but nothing could release it without making the classes waivable, which
  plan §5.8 rules out.
- **Route the edit through the scope-conflict channel.** That channel
  refuses a path inside the allowed set, and every protected path is
  inside it.

## Accepted Limits

- **The list follows `publish_strict` on every stage.** A specification
  run imports under the specification profile, where a reviewer-instruction
  edit is also fatal, but its list omits reviewer-instruction patterns. A
  specifier publishes no workspace content, so this only affects advice.
- **The intent's list is not digest-bound.** Every other part of a
  reconstructed prompt is tied to an admission digest; the recorded list is
  not. The intent file is trusted daemon state and the list grants nothing,
  so a re-gate would only add a way to strand in-flight runs.
- **Admission's delivery check omits the section.** A prompt within a few
  hundred bytes of the 1 MiB file limit can pass admission and then fail
  at start as an oversized prompt.
- **An attended run's list can go stale.** An attended admission reads the
  repository's latest profile at import, so a re-approval during the run
  can change what import enforces after the writer was told.

Revisit when #3125 makes automation-control advisory under a profile field:
the list must follow whatever predicate import then uses.
