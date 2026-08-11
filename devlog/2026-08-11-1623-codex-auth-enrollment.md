# Enroll Codex Auth Under The Mutation Lease

Chose a one-shot operator maintenance command over teaching the long-running
daemon to discover or copy the operator's Codex login. The command accepts an
explicit owner-only input file under a separate owner-only root, while the
daemon retains one fixed live auth-store path per identity. This keeps login
and secret cleanup operator-owned and prevents a temporary credential source
from becoming an ambient runtime dependency.

Chose to prove enrollment by immediately spending the supplied refresh token
through the same mutation-lease transaction used by proactive Codex refresh.
Merely parsing or copying `auth.json` would leave the highest-risk property,
provider acceptance of the refresh chain, untested. A successful enrollment
therefore leaves only the rotated refresh family in the live store and derives
an access-only snapshot with a bounded future expiry before recording verified
evidence.

Chose to consume #684's existing journal, marker, recovery binding, and
`resolve_reenrollment` command rather than introduce a second recovery state
machine. Initial enrollment creates the same blocked marker as re-enrollment;
verification projects exact digest, expiry, identity, and lease-fence evidence,
but only the operator's command-backed action restores admission.

Chose a read-before-project retry over starting a second operation after a
crash between journal verification and attention-item projection. The retry
accepts the latest verified journal only when its exact marker remains the sole
current open occurrence, then independently re-reads the hardened live store,
matches its digest, re-derives an access-only snapshot, and checks the recorded
expiry before idempotent projection. A verified database row alone is not
trusted as proof that the credential file still matches.

Refute-first review found that the first orchestration draft rewrote the live
predecessor before recovering a response-bound pending rotation, and removed
the refresh intent before committing the verified journal outcome. Either
crash window could lose the sole rotated refresh family and retry the already
spent operator token. The final ordering recovers pending or committed targets
before replacement, retains the intent through the journal's verified terminal,
and removes it only afterward. Process-cut tests cover both sides of the file
commit and a verified-before-projection retry with no source file.

The same review found that a lease check outside recovery was too early: a
stale holder could mutate files after the check, including deleting a successor
holder's intent between two cleanup mutations. A later refutation showed that
even a final body-and-inode comparison left an unavoidable syscall-sized race.
The store adapter now authenticates the complete holder, fence, acquisition,
and expiry coordinates inside an immediate write transaction and holds that
transaction through each short filesystem mutation. Lease takeover therefore
cannot interleave with live-store replacement, intent creation, response
binding, pending commit or cleanup, or final intent deletion. A real-store
concurrency test holds one filesystem callback past lease expiry, proves the
successor remains blocked until it returns, then proves the old generation is
refused. The reconstructed origin/main transaction and shared implementation
also make the same success, re-enrollment, and operational decisions, and
commit identical bytes across a 96-case adversarial token corpus.

Automated review found a final returned-object trust gap: enrollment accepted
the attention item returned by projection without rechecking that it was the
open carrier for the just-verified evidence. The ward boundary now validates
the complete item, its open status, the resolving action, and all four binding
coordinates before reporting success. Mutation tests independently corrupt
the carrier state, action, binding presence, identity, fence, digest, and
expiry so neither initial enrollment nor recovery can borrow stale authority.

Revisit when Codex provides a first-class non-interactive device enrollment
flow whose output can be bound without copying an operator-owned login file.
