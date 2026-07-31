# Bound Every Installation-Token Expiry to One Declared Lifetime

Work unit: #413. Scope: `daemon/internal/publish` and `devlog/`.

## Decision

Chose one package-private lifetime bound, applied at a single decode point, over
per-caller expiry policy or a caller-supplied bound. GitHub issues every
installation access token with the same fixed one-hour lifetime, so the returned
`expires_at` is a checkable part of the grant rather than a parameter: the plan
already requires "the expected bounded expiry" to be verified before worker
exposure (`docs/plan.md:502-506`), and that phrase is only meaningful against one
declared number. `checkInstallationTokenExpiry` is therefore the sole decoder,
and `mintResolved` plus the janitor's `mintGrantReadToken` both call it, which
covers the worker-bound, onboarding, and grant-read mints without any of them
restating the rule.

**The bound is one hour plus `jwtLifetime` of clock skew.** The skew exists for
the two honest reasons an expiry lands later than local `now + 1h`: request
latency, and a local clock running behind GitHub's. It is derived from
`jwtLifetime` rather than picked: GitHub rejects an App JWT whose `exp` has
passed, so a clock at least `jwtLifetime` behind GitHub's already fails
authentication and never reaches this gate. A first draft used five minutes and
claimed the same property; the refute-first pass disproved it with a measured
5-to-9-minute band in which the App still authenticated while every mint was
refused. Equality removes the band, and 1h9m is still four orders of magnitude
tighter than the reported regression, which carried a century.

**The lower bound is only "after now", deliberately.** A shorter-than-expected
lifetime narrows the authority in circulation rather than extending it, so
rejecting short lifetimes would add availability risk with no security gain. An
earlier draft justified this with `CachedTokenSource`'s `tokenExpirySkew`; the
refute-first pass showed that skew gates cache hits, not the token a fresh mint
returns, so nothing downstream enforces a minimum and the comment no longer
claims one does.

Rejected exporting the rejection sentinel. No caller distinguishes this failure
from the other returned-object refusals (`ErrGrantMismatch` is the exception
because callers key on it), and #411 consumes this package concurrently under a
frozen exported surface. Rejected adding a bound parameter to the constructors
for the same reason: the lifetime is GitHub's contract, not the caller's choice.

Rejected reusing `ErrGrantMismatch` for the expiry refusal even though the class
is the same. An operator reading `ErrGrantMismatch` looks at permissions and
repository scope; a conflated sentinel would misroute that diagnosis.

## Trust Boundary

The expiry is the last field of the mint response that was still trusted. It is
now checked before the token is audited, cached, returned, handed to git, or used
for janitor enumeration.

**The check runs after the permission and repository comparisons, not before.**
An earlier draft checked the cheapest untrusted field first. Both orders refuse
the token, but the refute-first pass showed the early position changed which
error surfaced: a response that is both over-broad and over-long stopped
carrying `ErrGrantMismatch`, which is the sentinel a caller keys permanence on,
so a forged grant would have been downgraded to a retryable failure. The same
reordering had silently emptied the janitor's only over-broad-grant test, which
then passed by stopping at the expiry gate instead. Both mint paths now check
expiry last, and a new case pins the combined over-broad, over-long response to
`ErrGrantMismatch` on each of them.

An accepted expiry is normalized to UTC. The old path kept whatever offset the
response used, so a non-`Z` response now renders differently in the audit row;
the instant is unchanged.

Neither the token nor the rejected expiry text reaches the returned error.
`time.ParseError` renders the value it rejected, and a compromised proxy can put
credential material in any string field, so `checkInstallationTokenExpiry`
returns a fixed reason and never quotes the input. The janitor keeps its
existing shape: a refused token still travels out of `mintGrantReadToken` so the
caller revokes it, because a credential the daemon holds and will not use is
exactly the one that must be taken back.

The janitor's revoke-failure comment previously asserted that an unrevoked token
"stays live for an hour". That claim rested on the unverified value this unit
exists to distrust. It now says the lifetime is bounded only when the mint's
expiry check passed and unknown otherwise, which is why an unrevoked token stops
the pass instead of being waited out.

## Verification

The recorded GitHub response fixture (`testdata/token-response.json`) carries
exactly a one-hour expiry, which is the observed evidence behind the declared
lifetime. Every janitor grant-read fixture previously omitted `expires_at`
entirely, so requiring it moved those fixtures onto a single shared conformant
body; the real endpoint always returns the field, and the omission was only in
the decoder.

A mutation pass measured the regression tests rather than asserting them: with
the upper bound removed from the helper and the janitor's call deleted, the
over-bound cases fail on every mint path and all twelve janitor cases fail. A
second mutation neutralized the janitor's grant comparison and confirmed both
its own case and the new ordering case fail, which is what proves the check
order restored that test's reach.

The refute-first pass ran two independent lenses, one enumerating every expiry
decoder, token construction, cache insertion, grant-read mint, post-mint audit,
and git/GitHub token handoff, the other attacking leakage, residue, revocation
ordering, and gate preservation against `HEAD`. Three findings were confirmed
and fixed here (the skew claim, the emptied grant test with its `ErrGrantMismatch`
masking, and the false lower-bound justification). Rejected by verification, so
not to be re-raised: `Secret` decoding of non-string, duplicate, `null`, and
unicode `expires_at` values leaks nothing and cannot desynchronize checked value
from used value; RFC3339 offsets, leap seconds, and a non-UTC or
monotonic-carrying clock cannot widen the window; both token sources hold their
mutex across resolve, mint, and insert; and a refused janitor token is still
revoked with `errJanitorUnsafe` intact when revocation fails.

Revisit when GitHub changes the documented installation-token lifetime, or when
a mint path needs a different lifetime than the App-installation endpoint's: the
bound is one constant precisely because there is one lifetime today.
