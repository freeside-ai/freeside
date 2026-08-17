# Make Production Retry Intent Durable

Production acceptance retries are explicit campaign attempts, not new content
identities. Attempt 1 retains the existing implementation-run derivation
byte-for-byte, and its campaign ID is deterministically derived from that run
ID. Later run IDs derive from the campaign and the store-allocated monotonic
attempt number. This preserves ordinary submit idempotency and makes a retry
possible without changing an approved specification's bytes.

Each attempt is a durable record that binds the original source digest, the
elaboration run that supplied approval authority, the approved specification
digest, the resulting implementation run, and, for retries, the exact parent
run and operator reason. The approval digest is the only field unavailable at
initial submit; the authenticated specification-approval transition may fill
it once. Run rows repeat the operator-facing campaign tuple in extracted
columns and canonical JSON, and reconstruction cross-checks both the columns
and attempt record before returning the run.

Rejected alternatives:

- Random campaign IDs would require a lookup before an exact submit could
  converge and would make first-submit crash recovery depend on mutable state.
- Specification wording edits encode operational intent as content and can
  collide when wording is reused, the failure this change exists to remove.
- `submit --new-attempt` couples idempotent intake and deliberate retry in one
  flag surface. Dedicated `reattempt` and `resume` verbs make minting versus
  non-minting behavior explicit.
- A resume that supervises or replaces lifecycle state would absorb #795.
  This unit only validates the exact run is live and reattaches its existing
  observation path.

The retry allocator records its derived run ID before production intake. If a
process stops in that narrow interval, the same parent-and-reason invocation
adopts the incomplete record; a different intent fails closed until the first
attempt is completed. This keeps ordinal allocation store-owned without
requiring one large cross-package transaction refactor.

Elaboration intake created before the campaign contract remains legacy after
upgrade. An exact submit replay authenticates its original campaign-less run,
request, dispatched implementation claim, policy, and invocation and then
returns that state without silently adding lineage. A changed replay still
fails closed, and all newly created submissions use the campaign contract.

The original publication file's raw-byte digest is also durable attempt
identity. Decoding and re-encoding publication metadata is not reversible, so
reattempt returns the stored digest rather than deriving a different digest
from canonical JSON. The digest is intentionally persisted only in production
attempt lineage: it is required by the CLI retry contract, while API and app
projections have no consumer for raw submission-byte identity.

Plan revision 33 records this as a material production-acceptance contract:
approval and deliberate retry preserve durable campaign lineage, while resume
remains a non-minting operation on one live run.

The compatibility sweep initially covered legacy manual replay but missed the
parallel label-intake reservation and the newly added request digest. Both were
pre-upgrade durable payload shapes, so the upgrade gate must compare the old
shape as a whole: campaign-less reserved runs remain startable, and old replay
payloads omit publication identity rather than failing byte equality. Revisit
when another durable submission field is added: enumerate every pre-upgrade
request and reservation form before tightening reconstruction.

The returned-object refute pass also showed that agreement between an initial
attempt's approved digest and its implementation run is not approval evidence:
both rows can be altered together. Reconstruction therefore re-authenticates
the elaboration policy, terminal output artifact, and, when the policy requires
human approval, the resolved attention item and its immutable approve command.
Changing both repeated rows to another digest now fails closed because the
independent approval authority still names the bytes the operator accepted.

Revisit when #795 adds lifecycle supervision: it may call the exact-run resume
primitive, but must not weaken terminal refusal or turn resume into implicit
attempt creation.
