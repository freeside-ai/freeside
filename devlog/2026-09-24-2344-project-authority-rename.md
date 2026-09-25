# Keep Project Authority Across a Repository Rename

For #1537, the numeric `RepositoryID` is a project's identity, and the
`projects` row carries the repository's current name. Every
project-to-repository equality check compares the ID only. A different ID is
still a loud `ErrImmutableConflict`. A new admission or intake registration
under the same ID and a new name rewrites the row's name; `project_id` and
`repository_id` never change. A replayed admission verifies the ID and never
renames the row back.

This revises the #1535 decision
(`2026-09-24-2219-project-authority-at-admission.md`) that the row is
write-once by whole body. That rule treated a rename (same ID, new name) as a
rebinding: the next admission failed, closure refused a subject bound under
the old name, and migration 0081 refused to open a store whose admissions for
one project straddled a rename. The only recovery was manual. The assumption
that changed: a name is not identity, and GitHub renames keep the ID.

Why the row follows the name instead of keeping the first one:

- **Name-keyed lookups read `project.Repo`.** Trust profiles, candidate
  authorizations, and the project display label are keyed or rendered by
  name. After a rename the operator records a trust profile under the new
  name; a row stuck on the old name would never match it.
- **Nothing on the forge reads the stored name.** Published PR text uses only
  `Closes #N` or `Refs #N`, so a stale name in an older proposal target cannot
  address the wrong repository.

Rejected options:

- **Keep the first name forever.** Simple and truly write-once, but every
  name-keyed lookup would drift from the configured name after a rename.
- **Add a separate current-name column.** Two names to keep consistent and a
  schema change, for no reader that needs the historical name.
- **Let a replay rename too.** An old admission replayed after a rename would
  flip the row back to the old name until the next new admission.

The backfill in 0081 changed in place: a store whose admissions straddle a
rename failed 0081 and never recorded it, so it reruns the fixed version; a
store that passed 0081 is unaffected. For one ID under several names it takes
the name from the latest admission in recording order.

Limits the contract accepts: the configuration (`-repo` and the label-intake
initiator) is the only source of the new name; renames are not discovered
from GitHub. If those two disagree on the name for one ID, the row follows
whichever wrote last, which changes only the label. Checks that compare a
run's own records with each other or with a name-keyed trust profile still
compare names, so a rename during an in-flight publication fails loud rather
than continuing.

A refute-first review found no blocker. Disproved by checks: only a
byte-identical replay takes the no-rename path, and no other path records a
replay; every new admission base and the intake registration come from
configuration, never from an occurrence or task text; the close write uses
only the target's issue number, inside the PR's own repository, so an old
name never addresses a close; the verified arm takes its reported name from
the project or admission, never from the subject; and the rename `UPDATE` is
bound to the stored `repository_id` and must change exactly one row, while a
failed admission rolls the rename back with its transaction. Allowed by
decision: once a new admission renames the row, re-evaluating the trust of a
run admitted before the rename fails closed until a trust profile exists
under the new name, because that check compares the project with a
name-keyed profile. Before this change the new-name admission failed
outright, so this is no regression.

Revisit when the base repository becomes per run rather than per daemon, when
the daemon discovers renames from the forge, or when trust profiles become
keyed by `RepositoryID`.
