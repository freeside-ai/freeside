# Require the Source Issue on a CLI-Submitted Task

Work unit #1928. A task submitted with `freesided submit` whose source is one
GitHub issue URL published a pull request with no reference to that issue,
while the same source typed into a client published `Closes #N`. The two
doors now agree, and the operator's publication file is what makes them
agree.

## Chose an Operator-Declared Source Over a Derived One

Chose to have the operator name the issue in the publication file, using the
record a client-composed task saves (`freeside.client-publication/v2` with
`source_issue`), and to have `freesided submit` and `freesided preflight`
refuse a file that disagrees with the task source. Nothing new is stored and
the publisher is unchanged: the record already carried the field, and the
publisher already wrote the reference from it.

Rejected: deriving the reference inside `freesided submit`, the way client
submission composes its own record. The operator's publication bytes are part
of a CLI submission's identity, and a replay is accepted only when the
recorded bindings match. A derived record would differ from the file the
operator wrote, so a submission saved before the change would stop replaying
as the task it created.

Rejected: writing a reference onto a literal `title` and `body` record. A
literal record is published as written, and stored literal records must keep
rendering the same bytes. A publisher-written line on a literal record would
change the body of pull requests already published from one.

## Kept One Rule for Both Doors

Chose one exported function, `engine.TaskSourceIssue`, for "this task source
is exactly one canonical issue URL". Client submission and `freesided submit`
both call it. The harness does not restate the rule in shell: preflight
refuses, and `scripts/run-real-work.sh` only warns about files the daemon
accepts but that publish less than an operator may expect.

## Exempted What the Database Already Accepted

Chose the shape the egress-policy check already uses. The CLI side lets a
refused file through only when the submission's saved journal exists, and the
daemon side accepts it only when the stored submission row equals this one. A
journal with no stored row creates no work. A legacy run-id lookup is exempt
because it never creates a task. Preflight reads the journal alone and
requires the same task bytes and an equivalent publication record; submit
stays the authority.

## Findings That Shaped the Change

- **The refusal text repeats no submitted bytes.** Preflight prints it in the
  composition manifest, so it names the field and the fix, never the file's
  contents.
- **The closure gate needs a work-unit declaration.** With a declared source
  and no declaration, the pull request carries `Source issue: <url>`, nothing
  waits, and publication is not held. An end-to-end test pins both outcomes
  for a `cli:` submission. What a missing declaration costs the task itself
  is #1931.
- **The submit result gained a field.** An older CLI decodes the result
  strictly and rejects `source_issue` after a newer daemon accepted the task.
  The harness builds both from one tree, and a retry with the current CLI
  converges on the accepted task.

## Revisit When

Revisit when `freesided submit` gains a submission format whose identity does
not bind the operator's publication bytes, since deriving the reference would
then break no saved submission. Revisit the warnings when #1931 changes what
a missing work-unit declaration means.
