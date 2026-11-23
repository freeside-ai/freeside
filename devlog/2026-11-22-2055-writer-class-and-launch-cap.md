# A Writer's Class Defaults to the Cordon, and Only Warded Is Asked For

Issue #3471, tracker #3462, gives the operator the two settings cordoned
runs need: which class a project's writers use, and how many cordoned
launches run at once. This unit stores both and enforces the cap; #3472
routes writers by the class. The choices below are the defaults the
issue's Planning Notes offered the owner, which the owner handed over
unchanged with `Handle #3471`, plus the agent's implementation choices.

## Chose Cordoned by Default, With Warded the Only Override

A writer runs on `cordoned_host_process` unless the project's
`writer_class` names `fresh_vm_read_only_volume_handoff` or the task
requires it. The task requirement accepts only the warded class, so a
task in a warded project can't move itself onto the cordon: a per-task
setting can only strengthen containment, never weaken what the project
chose.

Rejected:

- **A task setting that names either class.** It would let one task
  undo a project's choice to stay warded, and the resolver would need a
  precedence rule between the two.
- **A warded default.** Plan §5.7 makes the cordon the default for the
  implementer and remediator; the warded class is the exception.

The resolver (`domain.ResolveWriterClass`) refuses a value either gate
would refuse rather than defaulting it to the cordon, so a bad value
never picks the weaker containment. Its doc comment names #3472's writer
admission as the caller, which scopes it to writer roles and owns what a
change after a task's first writer admission does.

## Chose a Per-Task Table Over a Task Body Field

The requirement is its own daemon-internal row (`task_writer_classes`,
migration 0149), not a field on the synced task. The app neither shows
nor sets it, so it moves no server revision, and the task's record
(`freesided inspect task`) shows it. Every read re-runs
`TaskWriterClass.Validate`, so a stored row naming the cordon fails the
read instead of reaching the resolver.

## Carried the Class in the Deputy's File

Real work runs from the deputy's `settings.json`, so a stored section
alone would reach no run. The file gains `repository.writer_class` under
the same rule, and `settings import` stores it.

Revisit when the installed daemon runs every project from the stored
records (#2083): the deputy key can then go.
