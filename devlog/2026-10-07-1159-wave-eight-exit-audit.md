# Wave 8 Exit Audit: Recovery Identity and Shutdown Ordering

The exit audit of Wave 8 at
`02dd7f8e8acd56581d59296fbffd8db3138b8716` found two recovery assumptions
that do not hold and an advisory shutdown path that delays a writer's hard
stop. These findings change the recommended implementation direction, so
this note records the reasoning required by AGENTS.md. The audit recommends
the changes below; it does not make an owner policy decision or authorize
remediation. The finding issues and #1613 carry the work and its disposition.

## Bind Adoption to the Resource and the Unresolved Effect

**Check repository identity in returned filing candidates.** The filing
contract requires that evidence, but the issue decoder drops the response's
repository fields. A same-API pagination link can lead the candidate search
to another repository. A local HTTP fixture supplied an App-authored,
in-window issue explicitly belonging to that other repository, and the
filer accepted it as a candidate for the intended repository.

Reject the assumption that choosing the initial request path authenticates
all objects in the eventual listing. Bind pagination to the intended
collection and validate returned repository evidence before adopting a
candidate. Numeric App identity, creation time, and absence from the saved
candidate set do not establish repository identity. Follow-up: #1831.

**Keep prior ambiguity from contaminating a later reply's candidate set.**
Serializing only outstanding reply intents leaves a gap after an ambiguous
outcome. The outbox can open a later intent on the same pull request. A late
commit from the first send can then appear as the later intent's single new
App-authored comment. A deterministic probe recorded the later reply as
successful even though its own comment never existed.

Reject the assumption that a terminal ambiguous outcome ends the earlier
request's ability to commit. The never-retry rule prevents a second send of
one effect; it does not distinguish candidates belonging to different
effects. Recovery needs isolation or adequate identity evidence across that
ambiguity. Review-body replies share the pull request conversation, so the
scope of the listing matters as well as the original review thread.
Follow-up: #1832.

This evidence does not settle the open owner question of which human action
resolves an ambiguous filing or reply. It also does not introduce a workflow
audit gate for replies, whose omission was an explicit planning decision.

## Stop the Writer Before Waiting for Advisory Storage

**Preserve notice ordering without postponing termination.** The stall
watch joins an in-flight advisory write before handoff can reach its deferred
writer teardown. The notice's context deliberately ignores writer
cancellation and allows up to two minutes. A context-respecting notice hook
therefore kept teardown unreachable for 119 seconds after the writer wait
expired in a virtual-time probe.

The existing decision to order the final clear after an in-flight raise is
sound. The rejected choice is using that join as a prerequisite for stopping
the live writer. Initiate writer termination on budget expiry or
cancellation, then preserve the ordered, owned notice lifecycle. A test that
counts writer polls cannot establish when the container actually stops.
Follow-up: #1833.

The existing cleanup defect #1806 is related but distinct: image cleanup
detaches cancellation without adding a deadline. Its reproduction showed
that a bounded build can still leave the publication lane waiting forever
on cleanup. Default-disabled automatic rebuild permits a conditional
deferral; it does not make cleanup bounded.

## Keep Freshness and Exit Evidence Honest

**A response cannot inherit a revision observed while it was in flight.**
The device-list probe captured an old response, advanced sync through a
revocation, and then delivered that response. The model stamped those rows
with the new observed revision, preventing the next stale check from
reloading. The daemon still rejected the revoked credential. The defect was
the client presenting stale access information as current, not failed
revocation enforcement. The audit record on #1613 identifies its finding.

Existing #1707 is the other direction of the freshness problem: live backup
health changes the stopped verdict without moving the revision clients
watch. Existing #1755 can retire an exhausted readiness re-entry without
leaving an open alert. Both remain material to the unattended exit proof.

**Keep a demonstrated defect separate from missing evidence.** #1597
disclosed missing real-role peak-memory and observer-overlap measurements.
That is unmet acceptance evidence, not proof that a declared size fails.
Likewise, merged units and package tests do not supply the recorded real-run
and alert evaluation that closes #1613.

This audit leaves the owner choices intact: clean-machine proof placement
(#428/#1621), the default drift route (#1696), the ambiguity-settlement
action, and the stated rebuild-network non-goal (#1793). Existing #1692's
dispatch-time reversal record still warrants an explicit deferral or a
separate contract design, rather than an unreviewed change to how applied
reversals are proved.

## Revisit When

- Forge recovery gains a server idempotency key or another authenticated
  link from a created resource to its intent.
- The ambiguity-settlement action is designed; reassess which collection
  can safely admit a new effect afterwards.
- Automatic rebuild is enabled, making #1806's cleanup bound necessary for
  the publication lane's operational guarantee.
- The recorded exit exercise covers the adverse states and the owner
  dispositions on #1613 are complete.
