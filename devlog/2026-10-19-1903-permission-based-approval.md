# Landing Approval Rests on Repository Permission, Not a Login

The landing record names an Approver permission of `write` instead of the
`bennelsonweiss` login that `devlog/2026-10-19-1500-landing-record.md`
chose. That note's decision stands in intent: the owner approves, and an
agent that adds `approved` violates the record. What changed is the
mechanism the `land-approved-prs` skill reads.

The assumption that changed: the skill no longer matches the label's actor
against a recorded login. It counts an `approved` label only when the
collaborator who added it holds the recorded permission or a higher role,
and a record naming one Approver login stops `bad-record` before any PR is
listed. Keeping the login bullet would leave Freeside unable to run a
landing batch at all.

Chose `write`, the skill's floor and the agent-setup audit's default. The
project has one collaborator, an admin, so `write` and `admin` admit the
same person today, and the floor is the setting that keeps working when
the owner adds a collaborator who should be able to land.

Rejected `admin` or `maintain` as a tighter floor that mirrors "sole
approver". It would preserve the earlier wording but defends against a
collaborator who does not exist, and the audit raises the floor only when
the owner asks.

Revisit when a collaborator gains `write` access who should not be able to
land PRs; then raise Approver permission in a reviewed PR.
