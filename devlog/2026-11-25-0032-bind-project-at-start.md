# Daemon Start Binds the Configured Project

#3177 taught specifier triage to read the issue a bare-URL submission names,
but #2382's routing rerun still escalated one. The cause was ordering, not
URL parsing: submission and triage find the issue through the project's
`projects` row (`managedSourceIssue`, through `researchScope`), and on a
fresh root the only writers were label intake and a run's first execution
admission. Triage chooses the specifier's tier before the specifier is
admitted, so the first submission to a fresh root read as its bare URL.
#3177's tests registered the project in the fixture and never saw the gap.

## Chose to Bind at Daemon Start

When the configuration names a project (`ClaudeDriverConfig.ProjectID`),
daemon start writes the binding with `RegisterProject` from the configured
repository name and ID, before the engine loop runs or the API serves a
submission. The planning stage chose this approach, and the agent kept it.

It grants nothing new. The first admission writes the same binding from the
same configuration, and `RegisterProject` behaves as a new admission's write
does: it follows a rename and refuses another repository ID as an immutable
conflict.

Rejected:

- **Binding at submission.** `freesided submit` can write to a root with no
  daemon running, and then has no configured repository to bind.
- **Falling back to the configured repository inside the engine.** The
  engine holds no repository configuration, and a fallback would be a second
  source for a binding the store already owns.

## What Changes for an Operator

- **A conflicting row now fails the start.** A stored row bound to another
  repository ID used to let the daemon start and refuse its first admission.
  It now refuses the start, which reports the same conflict sooner.
- **The repository ID is fixed at first start, not first admission.** A root
  started once with a wrong configured repository ID keeps that binding even
  if no work ran, and the settings gate (`CheckProjectRepositoryBinding`)
  then refuses the corrected ID. Only an unattended start checks the ID,
  against the admitted image's provenance, before the bind; an attended
  start binds it unchecked. Before this change, the same wrong ID was bound
  by the first task anyway.
- **Other readers of the row act from the first start.** Image upkeep files
  image-policy adoption cards for the configured project
  (`adoptionProject`), and a daemon-origin repair submission passes its
  project check (`gateDaemonSubmissionSource`), where a fresh root used to
  wait for its first admission. Both are what the row exists to allow.
- **A submission made before any start binds the project records no
  issue.** One written with no daemon running, or to a daemon started
  without a driver, keeps no issue on the task, though triage after a later
  start reads it.
- **A daemon composed from flags is unchanged.** It names no project, binds
  nothing at start, and its first bare-URL submission still triages from the
  text until an admission binds the project.

Revisit when another path writes the `projects` row before submission (for
example, onboarding or a settings write), or when `managedSourceIssue` stops
reading that row.
