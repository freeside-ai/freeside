# Role Chip Selection Picker

#2600 turns each New Task role chip from a list of tree agents into a pick
of enrollment, model, and effort. The 4 Nov canvas
(`docs/design/handoff-2026-11-04-settings-usage-new-task/`) draws the chip
but not its menu, the reset rule, or the summary's short form; this note
settles them.

## One Menu With Sections

Chose one menu per role over one submenu per enrollment: Default and the
enrollments as one picker, then, once an enrollment is chosen, a Model
section, an Effort section, and a footnote naming the harness build and
discovery time. Each pick closes the menu, as a system menu does, and
reopening it shows the sections for the new choice. Nested submenus would
put the effort three levels deep, and the reset rule already fills model
and effort on the first pick, so most choices take one click.

Each enrollment shows its pool's reading as the usage view words it: the
window with the least left, then when that window's reading was observed
(`read`, or `stale since` past its `stale_at`), and the collector's last
attempt (`unsupported`) only when no reading stands behind the window
(#1722). A later failed refresh leaves the reading's time standing, since
the reading is what the entry reports. Chose the tightest window over
listing every window, because a menu entry has one line and the tightest
window is the one that stops work. A reading never reorders, hides, or
disables an entry.

## The Reset Rule

- **Picking an enrollment** sets the role's lineup model when the
  enrollment offers it, otherwise the enrollment's first model, at the
  harness default. Discovery names no default model, so the first model in
  harness order stands in for one. The lineup model is kept as the tree
  names it, which may be the provider model an alias resolves to; the
  daemon matches either (`DiscoveredModelCatalog.Serves`), and keeping it
  lets the summary see the lineup's model.
- **Picking a model** keeps the effort when the new model reports it,
  otherwise resets it to the harness default.
- **An enrollment not discovered yet** offers only what the tree's agents
  run on it, which may not include the harness default. There the model's
  first carried effort stands in, so a pick never lands on a selection the
  form would then block.

Revisit when: discovery records a default model, or admission stops
running tree-carried selections on an undiscovered enrollment.

## The Summary's Short Form

- **Enrollment alone** when the selection's model and effort are the
  lineup's for every role it is named for: the operator moved the role to
  another account and changed nothing else.
- **Otherwise `enrollment · model · effort`**, with the discovered display
  name, and no effort for the harness default (`codex-b · GPT Mini`). The
  harness default sends no effort option, so naming it adds 18 characters
  that say nothing.
- **Two selections that would read alike both read in full.** Each can
  match its own role's lineup line and read as one shared enrollment, which
  would name two choices the same.
- **The Default entry reads in full** (`Default · lineup <enrollment> ·
  <model> · <effort>`): the enrollment alone is defined against the lineup,
  so it says nothing in the lineup's own entry.
