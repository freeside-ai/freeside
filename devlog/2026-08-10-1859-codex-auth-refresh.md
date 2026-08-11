# Codex Auth Refresh Ownership

Issue #448 requires the daemon to rotate Codex subscription credentials without
giving the reviewer a refresh token or risking a second use of an already spent
token family.

## Decisions

Chose a host-owned refresh transaction inside the existing
`AuthStoreMutationLease` over container-side refresh because the lease is the
authoritative exclusion boundary for the mutable identity store. Review launch
now performs structural admission, identity-scoped revocation admission,
lease-held refresh when the access token is below the proactive threshold, and
then the existing lifetime admission. This ordering requires narrow review
configuration seams for the leaser, provider client, and durable auth state;
the earlier assumption that the lifecycle needed no new seam did not survive
the pre-refresh lifetime gates already present at three launch boundaries.

Chose to derive an access-only agent snapshot from the current host store over
moving the host store or teaching the reviewer to refresh. The host file keeps
the rotating refresh token, while snapshot construction replaces it with an
explicit empty value before any agent volume is seeded. The existing snapshot
digest and pre-start reconstruction gates bind the derived bytes, so a refresh
token cannot enter the container through this path.

Chose a same-directory, hardened pending response followed by atomic rename and
directory sync over in-place truncation. Before the provider call, the daemon
durably records a credential-free intent that binds the exact predecessor body,
inode, owner, and mode. A valid pending response is committed on the next
lease-held launch without calling the provider again. Commit and recovery use
the predecessor binding as a compare-and-swap: an exact predecessor may accept
the pending rotation, while any different current file proves that another
writer or operator superseded the transaction and must be preserved. An intent
without a complete pending response means the old refresh token may have been
consumed, so the identity requires re-enrollment rather than a provider retry.

Chose the auth identity's `AuthStoreVolume` as the trusted store locator for
read-only host snapshots. A refresh lease is authority only for the identity it
names, so launch rejects a snapshot path that does not resolve to that locator.
This closes the configuration case where lease A could otherwise mutate store
B without adding a second mutable-path authority.

Chose an advisory `system_health` AttentionItem plus an explicit authenticated
identity predicate over a blocking health item. Blocking posture is global
unattended admission control and would stop unrelated identities. The marker's
deterministic ID binds a digest of the auth identity, and reconstruction checks
the complete expected item shape rather than trusting its free-text reason.
Only that identity's review admission refuses while its open marker exists.

Chose a safe provider error type containing only HTTP status and vendor error
code over surfacing response bodies. Refresh request and response credentials
remain confined to the host transaction and are never formatted into errors or
durable attention state. The client also bypasses ambient proxies and refuses
redirects. Returned access, ID, and account fields are rejected if they expose
either the predecessor or replacement refresh token before any agent snapshot
is written.

## Refute-First Findings

Confirmed and fixed: the original per-run lease holder could converge across
daemon processes; every transaction now mints a unique holder, and even a
healthy-token launch acquires and verifies the identity lease. The same window
remains held through snapshot admission and the successful container start,
with final marker and fence checks at both boundaries. Immediately before
Start, the holder renews the exact fence, re-reads it, and requires enough
post-read lifetime to bound Start. The lease is released before the durable
ReviewSource handoff; a release failure therefore reaps the still-recoverable
`starting` launch under a detached bounded recovery context instead of
returning an untracked live container. Start receives a context created at the
verified fence and capped by both its operation timeout and the lease's
remaining window, so scheduler delay cannot extend it past lease expiry.

Confirmed and fixed: mutable refresh strategy or snapshot support could
invalidate a lease holder's already-read authority. Those mutation semantics
now join provider, lease declaration, and store locator as immutable identity
bindings; only independently measured execution parallelism remains mutable.

Confirmed and fixed: a provider call without a durable pre-call record could be
replayed after an ambiguous response or crash. The intent/pending transaction
now distinguishes pre-call, ambiguous, staged, committed, and superseded states
without persisting credentials in the intent. Intent persistence failures that
occur before the provider call remain operational and retryable: partial stages
are removed, and a rename whose directory sync fails is rolled back before the
error returns. Publication uses an atomic no-replace rename, and bounded
identity-specific stage reaping removes leftovers before a retry. A separately
observed existing intent, or a failed rollback that cannot durably prove the
canonical intent absent, remains ambiguous and requires re-enrollment.

Confirmed and fixed: body-only replacement could clobber operator re-enrollment
or copy metadata from a swapped inode. Reads now capture bytes and metadata from
one hardened descriptor, and commit authenticates the exact predecessor before
rename.

Confirmed and fixed: untrusted refresh fields and errors could alias or format
refresh credentials. Both old and new refresh values are barred from every
container-visible token field, and all external failures are mapped to fixed
safe messages before persistence or return. A successful response with an
empty replacement or one that repeats the predecessor refresh token is also
rejected before host-store mutation, because the provider may already have
consumed that single-use credential. Replacement refresh tokens are checked
against both predecessor-visible and returned-visible fields before either set
is overwritten, and a returned access token must clear the proactive refresh
threshold so an unusable response cannot drain the rotating family. Recovery
reapplies the same predecessor-aware checks to staged response bytes. Before
the pending body is staged, its digest and the exact live validation instant are
durably bound into the intent; restart recovery requires both bindings before
it can commit the rotation, without comparing token lifetime to the later wall
clock.

Confirmed and fixed: the initial health-item design was either globally
blocking or too weak to refuse one identity, and terminal-item authentication
omitted `DecidedAt`. The advisory item is paired with an explicit authenticated
identity predicate; resolved occurrences authenticate and allow a later open
occurrence.

Confirmed and fixed: cancellation could prevent the durable re-enrollment
marker, and a post-response persistence failure could make a recoverable
pending result unreachable. Marker writes use a detached bounded context;
complete pending state remains recoverable before any marker is created.

## Rejected Alternatives

- Extending general review recovery with auth bodies was rejected because its
  journal deliberately contains topology and intent only; persisting credential
  material there would expand the leak surface.
- Globally blocking unattended work was rejected because revocation belongs to
  one identity, not to daemon health as a whole.
- Retrying after an incomplete post-response commit was rejected because the
  provider may already have consumed the old refresh token.

Revisit when the provider's refresh protocol or the identity store moves away
from the Codex 0.147.0 JSON contract, or when admission gains a first-class
identity-scoped health gate that can replace the local predicate.
