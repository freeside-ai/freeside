# Handoff class and network-free precondition plan amendment (#79)

Plan Revision 12 executes the two workspace-handoff outcomes #79 queued,
as one material plan PR per Document gating. Both decisions were made
and recorded before this unit; this note records only the amendment's
own choices.

- **One revision carries both decisions.** They are the two outcomes of
  the same merged spike (docs/spikes/workspace-handoff.md), queued
  together by #79's contract; splitting them into two revisions would
  manufacture a second gate for no independent decision.
- **§3.4 left untouched.** The issue allowed §3.4 wording changes if
  the unattended precondition list moved. It did not move: unattended
  preconditions live in the §5.7 operating-modes table, so the named
  `supports_networkless_export` requirement landed there, and §3.4's
  cross-reference to §5.7 already covers it.
- **Names cited from code, not prose.** The class string matches
  `daemon/internal/ward/spec.go` (`fresh_vm_read_only_volume_handoff`)
  and the capability matches `daemon/internal/exec/capability.go`
  (`supports_networkless_export`), already enforced for unattended
  admission in `daemon/internal/ward/handoff.go`; the plan now says
  what the code holds.

Prior records this amendment rests on, not restated here: the refuted
same-VM fallback and the workspace-copy cost acceptance (with its
revisit condition) in `2026-07-14-2113-wave1-planning.md`; the
capability naming rationale in
`2026-07-17-2304-networkless-export-capability.md` and
`2026-07-17-2315-networkless-export-proof.md`.

Rejected alternatives: none new; the fallback-class refutation and
capability-name decisions carry theirs in the notes above.
