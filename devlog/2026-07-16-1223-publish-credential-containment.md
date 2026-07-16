# Publish credential containment: protected storage, scopes, audit substrate

Work unit: #80 (publish lane head, 1A.1). Mandatory note: credential-leak
surface plus a returned-object trust boundary (the manifest conversion
response), and two owner-visible mechanism choices the plan leaves open.

## Protected storage is file-based, not Keychain

Plan §10 says only "protected storage" and "single config directory";
the mechanism was this unit's to decide. Chosen: a dedicated
credentials directory (`<credentialsDir>/github-app/app.pem` +
`app.json`), directories 0700, files 0600, permissions re-asserted on
every load and failing closed on any group/other bit (a widened key is
treated as exposed, never silently narrowed).

Rejected: macOS Keychain. It would bind the daemon to cgo and one
platform, is hostile to headless tests, and §5.2's dedicated-user model
already provides the isolation Keychain would add; §5.14 reserves
Keychain for client device credentials. Revisit when: the daemon runs
on multi-user hosts, or a supported keychain/secret-service integration
is prioritized across target platforms.

## Containment is structural, and the checkpoint-manifest test is partial

`NewKeystore(credentialsDir, stateDir)` refuses overlap in either
direction, resolving symlinks through each path's deepest existing
ancestor. The state dir is the future checkpoint surface (§5.10) and
the surface workspace mounts derive from (§5.4); disjointness at
construction plus the walk-every-write test is the strongest assertion
available while no checkpoint code exists. `Keystore.Dir()` is the one
authoritative path the checkpoint unit must exclude. Residual gap,
accepted: the literal "checkpoint manifest excludes the key path" test
can only land with the checkpoint unit; #80's PR records it as the
acceptance-2 caveat. Revisit when: the checkpoint/backup unit lands
(its acceptance must include the manifest-level exclusion test against
`Keystore.Dir()`).

## Minimum scopes are final-shape, pinned in a golden

`PublishPermissions` = contents:write, pull_requests:write,
metadata:read. contents and pull_requests are write because minting
requests the scopes the publish path exists to use (#81 opens PRs); a
narrower interim set would make the per-mint audit trail lie the day
#81 lands. metadata:read is GitHub's implied baseline, requested
explicitly so the recorded set is complete. The mint request body is a
golden fixture: widening scopes is a visible diff, per §11's
"deliberately boring first repo" bound. The manifest's default
permissions are the same set, so the App never holds more than the
path uses.

## Mint audit: interim JSONL, SQLite deferred as a contract unit

Every successful mint durably appends a typed `MintRecord` (requested
and granted scopes both, no token field: the secret is unrepresentable
in the audited shape, the `device_credentials` precedent) and a mint
whose audit write fails returns an error: an unauditable token must
not circulate. §5.9 puts audit in SQLite long-term; that migration is
spine-owned contract territory, so it is filed as #107 rather than
folded in (the contract rule). The JSONL file lives under the state
dir deliberately: audit rows are not secret and belong in checkpoints.

## Redaction is type-level, with one found-and-fixed leak

`Secret` redacts through every fmt verb (Formatter, not just Stringer:
`%x` bypasses Stringer), GoString, and MarshalText; `Reveal()` is the
only way out. Found during implementation: `AppCredentials` embeds
`*rsa.PrivateKey`, whose exported big integers `%+v` and `json.Marshal`
print — the struct now carries its own Format/String/GoString/
MarshalJSON. The package-wide sweep test renders every exported
credential-bearing value through every verb and JSON and asserts six
needles (PEM base64, private exponent and prime in decimal, token,
both conversion secrets) never appear.

## Refute-first verification pass

Two independent adversarial lenses (containment/persistence and
redaction/API-leak) ran against the diff with instructions to disprove
the invariants before handoff. Dispositions:

**Confirmed and fixed.** Case-insensitive filesystems defeat the
disjointness check: `filepath.Rel` is lexical and case-sensitive, so on
default APFS `<base>/State/creds` vs `<base>/state` passed while
physically nesting the key inside the checkpoint surface (verified by
probe). Fixed with a case-folded comparison (over-rejects on
case-sensitive volumes, the fail-closed direction) plus regression
cases; folded into the keystore commit. Unicode-normalization (NFC/NFD)
variants remain a weaker sibling the fold does not cover; same
accepted-risk class as the TOCTOU item below.

**Rejected by verification (do not re-raise).** Audit JSONL capturing
token or key material (no token field exists; the recorder only ever
receives a MintRecord). Symlinked or unclean paths and permission
re-assertion bypasses (resolution through the deepest existing ancestor
plus fail-closed mode checks held). fmt/json dispatch escapes on Secret
at reflection depth — slices, maps, pointers, embedding, `%x` (value
receiver puts Format in both method sets; verified by probe). Credential
values reaching error chains, URLs, or off-host Authorization headers
(bodies are drained never wrapped; Go's default client strips
Authorization cross-domain). MarshalText asymmetry corrupting persisted
secrets ([REDACTED] can never reach disk: persistence is an inline
Reveal struct, not a marshal of the redacting types).

**Accepted by decision.** Post-construction TOCTOU (an attacker
swapping a credential-path ancestor for a symlink after NewKeystore):
requires a local attacker with write access inside the daemon user's
directories, outside the §5.2 threat model; revisit with the multi-user
item above. Partial persistence when SaveApp fails between key and
metadata writes: contained inside the protected directory, LoadApp
fails closed, re-registration overwrites; not a leak. Exported
`AppCredentials.Key` (a caller can always print an extracted
`*rsa.PrivateKey`): inherent in AppJWT needing the key; call-site
responsibility. Injected HTTP clients weakening redirect
header-stripping: documented as a constructor constraint rather than
policed, since the daemon composes its own clients.

Hardening adopted from the pass beyond the fix: `appMetadata`'s secret
fields are Secret-typed so no named type holds credential plain
strings; the one sanctioned reveal is SaveApp's inline persistence
struct.

## Codex review rounds (PR #110)

Round 1 confirmed a write-path gap the refute pass missed: SaveApp
wrote the fresh key through a pre-existing widened file
(`os.WriteFile` keeps the old mode), failing closed only after the
bytes landed. Fixed by converging before writing: directories
chmod-narrowed, credential files removed and recreated.

Round 2 confirmed two more, both class-swept rather than line-patched.
A pre-existing symlinked `github-app` child directory relocated every
keystore write onto the state tree (construction validates only the
root; `Stat` follows links): fixed with `Lstat` throughout the
keystore's kind/mode checks, a real-directory gate before `MkdirAll`,
and `O_EXCL` file creation so no pre-existing inode of any kind is
written through. And the durability barrier was incomplete: fsyncing
the audit file does not persist its directory entry, so the first
mint's log could vanish on a crash after the token circulated; fixed
by syncing the parent directory on every record (and the keystore's
key writes gained the same file-plus-entry sync, since losing the key
forces manual reauthentication). The post-construction symlink-swap
TOCTOU remains the accepted risk recorded above; what changed is that
symlinks *observable at write time* now fail closed.

Round 3 raised two P2s, both accepted. The mint boundary now fails
closed on an under-scoped grant (round 8 broadened this into
`ErrGrantMismatch`): GitHub can
grant narrower permissions than requested when the App's installation
was narrowed, and a token missing pull_requests:write would fail the
publish path halfway through its work. And the durability class from
round 2 had a remaining member: a recorder that creates the state root
itself cannot make that root's directory entry durable, so
`NewJSONLRecorder` now requires an existing state root (the caller's
surface) and owns only `publish/`; the keystore's directory creation
gained `mkdirAllSync`, which fsyncs every newly created level plus the
pre-existing ancestor.

Round 4 caught a redaction member the sweep's needle set missed
because the secret is transient: a transport failure on the conversion
exchange wraps `*url.Error`, which renders the full request URL — and
that URL embeds the unconsumed manifest code, which is
credential-equivalent until GitHub consumes it. Errors on that path
now strip the `url.Error` wrapper and name a `{code}`-redacted path.
It also caught that round 3's existing-state-root requirement broke
the opt-in live test (skipped in CI, so invisible to it); the test now
creates the root it owns.

Round 5 extended the same transient-credential class one hop further:
a 3xx from the conversion endpoint, if followed, sends the
code-bearing URL as the Referer to the redirect target. Both
constructors now wrap the injected client to never follow redirects
(`noRedirect`; the GitHub endpoints this package calls never
legitimately redirect), so a 3xx surfaces as the redacted APIError and
the earlier caller-must-not-weaken-redirect-stripping caveat is moot:
no redirect is followed at all.

Round 6 hardened two boundaries. The registration callback now embeds
an unguessable per-attempt nonce in its path, so an unrelated local
request can neither inject a foreign manifest code nor abort the flow
with an empty one; only the redirect carrying this registration's
manifest reaches the exchange. And the mint's returned-object boundary
validates the 201 body before the durable audit write: an empty token
or an expiry not after now is rejected, so a proxy or API regression
cannot advance the audit barrier ahead of a credential the publish
path cannot use.

Round 7 chained one step further on each. The form page embedded the
manifest, whose redirect_url disclosed the callback nonce to anything
that could reach the listener, so the page now lives at its own
unguessable path and every other request on the one-shot listener is a
404: nothing served discloses the callback. And the conversion's
returned-object boundary now rejects a response without a positive App
ID, since a valid PEM with issuer 0 would overwrite working
credentials and fail every later mint. The remaining registration
posture is accepted: the listener is caller-supplied and expected to
be loopback; a caller binding it publicly still gets nonce-gated,
disclosure-free behavior.

Round 8 closed the last lossy projection at the mint's returned-object
boundary: the fixed three-field struct silently dropped unknown
permission keys, and the repository selection was discarded entirely,
so an over-scoped grant would pass the equality check and be audited
as minimal. The grant now decodes losslessly (a permission map plus
repository_selection and the repositories list) and any difference
from the request, narrower or broader, is `ErrGrantMismatch` (the
round-3 sentinel, renamed and widened): the audited
requested-equals-granted row is now proven, not assumed.

Round 9 completed the round-2 symlink class at its last member: the
audit recorder followed a pre-existing symlinked `publish/` directory
(and would append through a symlinked `mints.jsonl`), relocating audit
rows off the state surface it owns while mints reported success. Both
now get the keystore's Lstat discipline and fail closed. This was a
sweep miss on my side in round 2: the class was "keystore paths", and
it should have been "every filesystem location this package owns".

Round 10 held the round-7 identity gate at the exported persistence
boundary too: SaveApp itself now rejects a non-positive App ID, so a
direct caller (not just the conversion path) cannot overwrite working
credentials with an issuer-0 identity.

Round 11 raised two P1s that reopened the SaveApp write path, and both
were real. First, a planted symlink *ancestor*: construction resolves
the credentials root, but a missing ancestor created as a link to the
state tree before the first save would be followed by MkdirAll into
the checkpoint surface. The creation walk now proves every existing
component symlink-free before creating, since a link appearing after
construction is tampering. Second, non-atomic replacement: the old
delete-then-write sequence destroyed the only working credentials
before the replacement was durable, so a mid-save failure (or a
crash, with the manifest code already consumed) left the keystore
empty and unrecoverable. SaveApp now stages both files in a sibling
directory, each fsynced, then renames the old aside, the new in, and
removes the old only after — a crash leaves either the old keystore or
the new one active, never a gap. A related correctness improvement
fell out: the root chmod now only strips group/other bits instead of
forcing 0700, so it removes exposure without clobbering a tighter mode
a caller set.

Round 12 showed that the replacement fix was still incomplete: the two
renames left a crash window with no active directory. The swap is now a
recoverable journal: loads and saves are serialized in-process; an
activation failure rolls the old directory back; restart recovery
prefers the validated old directory, or promotes a fully validated
first-registration staging directory when no old version exists. It
also completed two boundary sweeps: LoadApp now rejects a non-positive
persisted App ID, and Register rejects unspecified/non-loopback
listeners rather than advertising an unusable or externally bound
callback.

Round 13 found a post-activation cleanup failure in the same journal:
an owner-only but non-writable active directory (for example mode 0500
after a restore) can be renamed and replaced, but RemoveAll cannot walk
it. SaveApp therefore reported failure after the replacement was already
active, and the leftover blocked a later SaveApp. (LoadApp still read the
valid active directory, so that part of the finding was rejected by
verification.) Swap cleanup now Lstat-checks each reserved entry,
removes non-directories without following them, restores 0700 after
identifying a real directory, and then removes it. The same-user
check/chmod micro-race remains in the already accepted local-tampering
class. Regressions cover both a read-only previous credential directory
and read-only stale journal entries.

## Fresh-context proactive refute pass after round 12

One read-only lens, given only the base-to-head diff and PR intent,
independently confirmed the swap crash window and found three further
members. All evidence was reproduced.

**Confirmed and fixed.** A malformed `expires_at` in a successful mint
response was decoded directly into `time.Time`; `time.ParseError`
renders the rejected value, so a proxy could put token material there
and make it escape through the error. The raw expiry now decodes as a
redacting `Secret`, parses separately, and returns a content-free error.
The same sweep found permission values, repository selection, and
repository names rendered by grant-mismatch errors; those errors are
now generic too, so no untrusted response string reaches an error.
The manifest accepted caller-supplied permissions and the conversion
discarded GitHub's reported permission set; registration now pins the
manifest to the required set and validates the returned set losslessly
before persistence. Finally, round 9 guarded `mints.jsonl` but failed to
re-check its parent at record time: swapping `state/publish` for a
symlink after recorder construction relocated the audit row. Both the
state root and publish directory are now identity-bound at construction
and revalidated at the write boundary, rejecting both symlinks and
replacement real directories; regressions perform both swaps and prove
no row escapes or splits the audit history. The crash-state sweep also
found that an incomplete lone first-registration stage made LoadApp
return a recovery error forever. Load now removes that unusable stage
durably and returns the ordinary no-credentials state; a later Save can
likewise replace it without manual cleanup.

**Accepted by decision (unchanged class).** The lens recommended an
openat/no-follow directory-descriptor implementation to eliminate the
remaining Lstat/open race. That micro-race requires a local attacker
with write access inside the dedicated daemon user's protected state
tree, the same post-construction TOCTOU class already accepted above.
The observable between-call swap was fixed; the check/open race retains
the existing multi-user-host revisit condition.

Severity did not taper monotonically: round 12 and the proactive pass
found real P1-class durability/redaction failures after apparent
convergence. The widened crash-state, untrusted-string, permission, and
owned-filesystem-location sweeps now carry regression tests; another
recurrence in any of those classes should reopen the corresponding
model rather than patch another leaf.
