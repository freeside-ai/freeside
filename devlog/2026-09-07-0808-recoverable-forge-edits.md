# Accept Recoverable Errors in Routine Forge Edits

The owner chose ordinary care for routine issue, comment, plan, and tracker
edits. A small risk of recoverable concurrent-update errors is acceptable;
caution, rereads, and baseline verification should be used lightly. Missing
forge locks or atomic transactions must not make routine work impossible.

This revises the decision in
`2026-08-16-2219-staged-workflow-adoption.md`, which rejected a reread-only
fallback and required a transaction-wide conditional write or verified
exclusive writer. That decision identified a real limitation: a later
unconditional write can overwrite an intervening edit, and its read-back can
still pass. Its remedy made a guarantee the project could not supply a
precondition for ordinary progress. Issue #813 records that stronger proposal;
the present choice removes its guarantee as a prerequisite for routine edits.

GitHub's [REST guidance](https://docs.github.com/en/rest/using-the-rest-api/best-practices-for-using-the-rest-api#use-conditional-requests)
says conditional writes are unsupported unless the endpoint documents an
exception. Its [edit history](https://docs.github.com/en/communities/moderating-comments-and-conversations/tracking-changes-in-a-comment)
can help recover earlier text. History is a recovery aid, not proof that no
update was lost or that a stale record caused no harm.

Kept existing claims, planning reservations, expiry, authorization,
relationship checks, and implementation's integration gates. Known competing
writers still need coordination, including two planners editing the same
tracker from different issues. A new exclusivity relationship still checks
both endpoints. The policy accepts the remaining race; it does not claim
these checks provide enforced exclusivity.

Chose reads of the current target and relevant baseline, preservation of
unrelated text, and verification of each saved result. Changes to contributing
facts earn refreshed evidence and recomputation. Unrelated branch movement or
an advisory object's changed timestamp does not justify restarting planning
or collecting everything again. Partial writes are repaired from current
records within scope or reported as incomplete.

Rejected extending reservations to every tracker, adding permanent
incomplete-state markers, and freezing every read input. That machinery costs
more than the accepted risk warrants for routine coordination text. This
choice does not relax runtime trust boundaries or allow an agent to start
known conflicting or unauthorized work.

Revisit when observed lost updates repeatedly misdirect work, or a shared
writer mechanism can reduce that harm without turning ordinary edits into a
blocking transaction protocol.
