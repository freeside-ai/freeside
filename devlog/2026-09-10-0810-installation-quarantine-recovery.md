# Recover Through a Fresh Native Installation

The owner assigned supported recovery after an expired onboarding expansion
caused terminal installation quarantine. The existing command could renew the
old installation ID but served authority withdrew it, making resume impossible.
A zero-ID request also failed because the authored stale binding still occupied
the account. Manual file edits would destroy the supported recovery boundary.

We chose explicit `onboard -recover-installation <old-id>` over automatic
reactivation or quarantine clearing. Under the existing document/journal lock,
the store verifies the exact terminal quarantine and durably retains the prior
authority before the operation replaces the withdrawn binding and its matching
pending request. Unrelated bindings and pending requests are preserved. The
replacement is a bounded zero-ID envelope with no current repository authority
and exactly one requested repository. Ordinary native selection, live audit,
image review and promotion remain required. Previous intent approvals cannot
authorize the replacement revision or installation.

The archive is separate from active authority, so the authored document format
and the terminal journal format stay unchanged. A conflicting or unreadable
archive refuses recovery before active authority changes. A crash after archival
but before replacement permits a retry only when the retained bytes still match.
The journal is never rewritten by recovery. Approval history and database rows
are outside this operation; existing onboarding retention rules still apply to
later onboarding runs in the selected database.

Recovery makes #283's deferred boundary reachable: an old installation that
survived its attempted deletion could match a later zero-ID envelope. The served
snapshot now carries the journal's withdrawn IDs. Both janitor candidate
selection and onboarding token readiness reject those exact IDs while allowing
a new installation on the same account. This retains terminal quarantine
without banning future installations for the account.

Refutation covers rejected replacement without writes, archive failure,
preserved prior bytes and unrelated bindings, restart, stale approvals, and an
old installation coexisting remotely with its legitimate replacement. Fixtures
are verification of the mechanism, not live acceptance.

Independent review found no old-ID authority bypass. Its retention concern is
valid for later ordinary onboarding: that workflow already prunes superseded
workflow-audit bodies. Recovery returns the pending request before that workflow
runs and does not alter database evidence. The operator must preserve required
historical evidence separately before later onboarding in the same database;
this change does not widen or silently change the existing retention contract.

Revisit when supported onboarding needs atomic restoration of several
repositories or a different explicit recovery interface. Do not reuse old
installation authority to shortcut that review.
