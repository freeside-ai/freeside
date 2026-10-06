# Policy-Gated Project-Image Rebuild

Issue #1629 lets the production publication lane rebuild a project image when
a candidate changes its npm dependencies inside the project's declared policy
(plan §5.7, **Policy-gated rebuild**). It is lane work: `domain`, the store
schema, and the block-reason set are unchanged.

## A Refusal Is a Failed Verification, Not a New Block Reason

Chose "a refused gate records an ordinary failed verification checkpoint and
names its clause in a daemon evidence artifact" over "one new block reason per
clause". The implementation plan proposed the second and assumed the reasons
could be mapped in the engine alone.

They cannot. `signet` authenticates a blocked item's reason against
`domain.DefinitivePublicationBlockReason` when it reads the run's blocked
observation and again before it accepts a rerun. A reason outside that set
breaks the card's rerun action. Adding reasons there is a shared-contract
change, and the issue's non-goals forbid one in this unit. #1784 owns that
surface.

The consequences:

- **First publication:** The card shows the shared verification-failure
  reason and `TrustRuleVerificationFailed`. The clause is the first line of
  the item's one evidence artifact. This is the surface a failing preparation
  helper already produces.
- **Re-entry:** A cycle's stop reason is free text, so the stop item names the
  clause directly.
- **Rerun:** A refusal reruns as a failed verification does, only after a
  revised trust profile. Giving the daemon `-base-build-ref` later does not
  clear a `rebuild_not_configured` block by itself.

Rejected: carrying the clause as a blocking `CandidateFinding`. That would
invent class and category meanings on a shared vocabulary to avoid adding a
reason to another.

## The Gate Refuses What It Does Not Recognize

The candidate writes `package.json` and `package-lock.json`, and npm reads
both again inside the build with network access. The build proxy admits any
public host, so the gate is the only control on where that install fetches
from.

**The gate does not deliver the bound the issue assumes, and no manifest
reader can.** The issue's non-goal says the gate "bounds what `npm ci` fetches
through the lockfile's `resolved` URLs". Two refute-first passes with real
`npm ci` runs disproved that for two successive gates (Refute-First Findings).
What remains open after the second is a class, not an input: a locked version
that does not satisfy the range reaching it makes npm ask the default registry
and follow its metadata. Closing it in the gate means reimplementing npm's
version arithmetic in Go, and every difference between the two would be the
next bypass. The bound belongs where the connection is made (#1793). This
needs an owner decision, because #1629 lists that control as a non-goal.

Chose "refuse anything not positively recognized in what the candidate
changed" over "check each changed entry's `resolved` host". The first
implementation did the second. The gate that ships is the strongest reading
of the manifests this unit could justify, and a fast filter with a named
clause; it is not the egress control.

The reason is how npm treats a lockfile. `npm ci` trusts an entry's `resolved`
only while every dependency edge reaching the entry is satisfied by it. An
edge declared by URL, git reference, or path is satisfied only from that
source, so npm re-resolves it from the spec, and its lockfile check compares
only location and version. The package name it fetches is spliced from the
entry's key. An entry with no `resolved` is fetched by name and version, or
from its `version` when that is a URL. A `hasShrinkwrap` entry hands
resolution to a file inside its tarball. A gate that models one field loses
to the fields it does not model.

The rules that follow:

- **"Unchanged" means byte-equal.** An entry is not re-examined only when the
  base lockfile holds the same bytes under the same key, whitespace aside. No
  model of which fields matter is involved. The same test applies to each
  `package.json` dependency and to `overrides`, `workspaces`, and
  `packageManager`.
- **A changed entry is a registry tarball and nothing else.** A plain
  `node_modules/<name>` key, an exact version, an `https` URL on a declared
  host, a `sha512` integrity value, registry-only dependency specs, and no
  field outside an allowlist. `link`, `inBundle`, `hasShrinkwrap`, `name`,
  and `bundleDependencies` all refuse.
- **A changed `package.json` dependency is a registry version, range, or
  tag,** in all four dependency maps. The lockfile root must agree with all
  four.
- **A repeated JSON key refuses.** It also closes the one known disagreement
  between the two parsers: Go decodes an unpaired surrogate as U+FFFD and
  JavaScript keeps it, so two keys npm tells apart become a repeat here.
- **Fields are looked up by exact key, never through struct tags.**
  `encoding/json` matches struct fields case-insensitively and npm does not.
- **The host check has two halves.** The parsed host must be in the declared
  set, and the raw string must begin with exactly `https://<host>/`. npm
  parses the URL with a different parser; a string with that literal prefix
  names that host to any parser.
- **A bundled entry the base holds is only as approved as its shipper.** With
  no source of its own, it needs its enclosing package unchanged.
- **Every dependency has an entry, and no peer is nested under its
  dependent.** npm treats both as unresolved whatever the entries pin. The
  root and each changed entry are held to this in full. An unchanged entry is
  held to what the candidate could change around it.
- **A host npm reads as a git forge is not a registry,** even when declared.
  npm clones a URL there and checks no integrity value.

Rejected: admitting `npm:` aliases. An alias asks the registry, but for
another package than the entry's key names, and the gate does not model how
npm matches the two. The cost is real: `glob` 10 depends on an alias through
`@isaacs/cliui`, so a change that adds it needs a human rebuild.

Rejected: a repeated-key walker of the gate's own. The daemon's shared check
(`ward.RejectDuplicateJSONKeys`) also refuses two keys in one object that
differ only by case, which npm tells apart, so a tree holding both
`JSONStream` and `jsonstream` is refused. A second walker would have needed
its own entry in the strict-JSON ratchet to avoid a refusal that rare.

Rejected: pinning every `package.json` field. Tool configuration lives there
and changes with ordinary dependency work. Only the three fields that change
how every dependency resolves, or what resolves it, are pinned.

Accepted cost: the gate refuses lockfiles a person would call fine, such as
one that adds a package with bundled dependencies. A refusal costs a human
rebuild; an admission the gate cannot justify costs a fetch from a host
nobody declared.

The delta is against the base's manifests, not the image's. The image records
only hashes. The lane has already proved the image serves the base, which
makes the two the same bytes.

## The Run Binds to a Rebuilt Image Through Write-Once Rows

Two internal inbox rows, keyed by run and verified commit, carry the rebuild:
an intent written before the build, and the binding written after. Neither
is a schema change.

- **A recorded image is matched by environment, not by commit.** The first
  recorded image of the repository that keeps what the admitted image binds
  and bakes the head's manifests is used. Matching by commit would rebuild on
  every remediation round that leaves the manifests alone, and would fail
  forever on a rebuild that reproduces an already-recorded digest, because an
  image record is immutable per reference.
- **The same lookup is the restart path.** A process lost after the build
  recorded its image and before the binding row finds that image and does
  not build again.
- **Every image other than the admitted one is re-gated before use.** A row
  or a record is decoded state. The image must be this repository's, keep the
  admitted image's recipe and base image, run only the builder's fixed
  preparation helper, and be admissible at the commit it verifies.
- **A rebuilt image is held to its recorded environment at its own build
  commit.** `ProjectImage.AdmissibleAt` takes an image at its build commit
  from the record alone, and a rebuilt image is built at the commit it
  verifies, so that rule alone compared nothing. The automated review of the
  PR found it. The engine now compares the baked manifests and the
  preparation digest itself before it asks `AdmissibleAt`.
- **The admitted image still answers for the base.** The rebuilt image
  answers only for the head.

Rejected: an `effective image` field on the admission. It is a shared-type
change, and the admission is recorded before execution, when no head exists.

## Re-Entry Resolves the Image in the Handler

The issue says both binding loaders resolve the effective image. The
first-publication loader does. The re-entry loader cannot: the commit a
re-entered cycle verifies (the head, or the prospective merge) is known only
after the handler builds its workspace. The handler resolves the image there,
before it reads the checkpoint. The effect the contract asks for is the same.

## Which Build Failures Are Verdicts

The contract separates a proof failure, which blocks, from any other builder
fault, which retries. `ErrProofFailed` and `ErrInvalidRequest` are verdicts;
everything else leaves the task queued under the lane's visible
`publication_environment` hold.

Two additions the refute-first pass forced:

- **One build is bounded at one hour, and the bound is a verdict.** The
  builder's proofs run the candidate's verification commands with no bound of
  their own, and the lane handles one task at a time. An unbounded build lets
  one candidate with a hanging test stop publication for every run. As a
  retry, the bound would stall the lane for an hour on every attempt, so it
  blocks as `build_or_proof_failed`. Rejected: an operator flag for the
  bound. Nothing yet shows a project whose install and three proof runs need
  more than an hour.
- **A cancelled pass is never a verdict.** Cancellation kills a running proof,
  and the builder reports that as a proof failure. The lane reports the
  cancellation instead.

Deferred to #1806: the bound ends the build's work, not the cleanup the
builder runs afterwards, which has no deadline. A container runtime that
stalls there holds the lane until a restart. The candidate cannot cause it,
and returning while the builder still runs would race the lane's next
runtime call.

Deferred to #1792: a build failure the candidate causes
outside the proofs retries on every paced attempt. The example is a lockfile
that passes the gate and fails `npm ci`, such as a tarball that does not
match its integrity value. The builder cannot tell that apart from a registry
fault without reading npm's output. The same issue covers the reverse case: a
runtime fault inside a proof, or a base tag that drifted, is terminal.

## Known Limits

- **"Recipe unchanged" compares declarations.** The head must declare the
  recipe digest its base declares, including declaring none. The baked recipe
  is read from the admitted image, since onboarding may have supplied it from
  outside the tree.
- **The base's manifests must parse too.** A base whose lockfile has no root
  entry cannot bound the change and refuses as `lockfile_inconsistent`.
- **A rebuilt image made stale by a daemon upgrade is not a refusal clause.**
  A changed preparation digest makes a bound rebuilt image inadmissible. First
  publication fails the task as it does for a stale admitted image. Re-entry
  stops the cycle on the existing unserved-base reason, whose sentence names
  the admitted image; the quoted cause names the rebuilt one.
- **The build's own fetches are not held to the registry set.** That is the
  issue's stated non-goal, and it leaves the gate as the single control on
  the build's destinations. #1793 adds the control. Until it lands, a daemon
  with `-base-build-ref` can be made to contact an undeclared host by a
  crafted candidate; the README says so where the flag is documented.
- **The gate does not check that a locked version satisfies its range.** The
  known result is a contact with an undeclared host followed by a failed
  `npm ci`, which is the deferred retry case above. A variant that also
  installs is not known and not excluded.

## Refute-First Findings

A fresh-context reviewer attacked the first implementation with real
`npm ci --ignore-scripts` runs.

Confirmed, and fixed by the default-deny gate:

- **A URL, git, or path spec in `package.json`,** with an unchanged lockfile
  root and a declared-host entry. npm fetched from the spec. A
  `peerDependencies`-only change did the same.
- **A lockfile key spliced into the fetch spec,** such as
  `node_modules/https:host#`. Two keys differing only by an unpaired
  surrogate collapsed in Go.
- **`inBundle` taken on trust.** A bundled entry with a URL `version`, a URL
  `name`, or a rewritten `version` on a base entry fetched from that URL. A
  plain bundled entry fetched from the registry unpinned.
- **`hasShrinkwrap` ignored,** including when set on an otherwise unchanged
  entry.
- **Integrity checked only for presence.** `"x"` and a sha1 value passed.

Confirmed, and fixed in the lane:

- **Proofs with no time bound** could stall the sequential lane.
- **An unreadable manifest refused the gate for an image the gate does not
  apply to.** The no-environment case now returns first.
- **A binding row's image was re-gated only where a checkout was at hand.**
  The loader now holds it to everything the admitted image binds.

Deferred, with the reason above: candidate-caused build failures outside the
proofs retry.

Disproved, and not to be re-raised without new evidence:

- **URL forms in `resolved`:** uppercase host, trailing dot, `:443`,
  userinfo, backslash, tab, leading space, uppercase scheme, `https:///`,
  fragment confusion, percent-encoded host, `git+https`.
- **Parser tricks:** a repeated `resolved` key, a unicode-escaped field name,
  a byte-order mark.
- **Other npm inputs:** link entries, the v2 legacy `dependencies` tree,
  traversal keys, an `overrides` mismatch, lifecycle scripts.
- **The surrounding machinery:** the local-source build reads only the object
  store, the write-once rows converge, and the flag validation is sound.

A second fresh-context reviewer attacked the default-deny gate the same way.

Confirmed, and fixed in the gate:

- **A peer dependency nested under its dependent.** npm rejects the peer
  whatever its version, re-resolves it from the registry, replaces a
  root-level entry whose integrity differs, and then follows the registry's
  metadata. With a published package naming a URL dependency, `npm ci`
  installed a tarball from an undeclared host and exited 0.
- **A dependency with no lockfile entry.** npm fetched the package's registry
  metadata and a URL it named before failing.

Reasoned, not run, and fixed: a declared host that is a git forge npm knows
makes `resolved` a repository URL.

Reasoned, not run, and open (the owner decision above): a locked version
that does not satisfy its range. Also reasoned: with a registry set that
excludes the public registry, npm's metadata requests still go there.

Checked for false refusals: a real lockfile adding `express`, `rollup`, and
`react-dom` (102 changed entries) is admitted. One adding `glob` 10 is
refused for its alias.

Disproved by reading npm's resolver, not run: dist-tag, `*`, and empty specs
are always satisfied by an entry with a remote `resolved`.

Not attempted by either pass: differences between Go's and JavaScript's JSON
parsers beyond repeated and surrogate keys, and `package.json` fields other
than the three the gate pins.

## Revisit When

- #1784 lands per-reason block facts: give each clause its own reason on the
  first-publication card and drop the evidence-artifact indirection.
- A second package manager is supported: the lockfile reader is npm-only by
  construction.
- An aliased or bundled dependency is refused often enough to cost real
  rebuilds: model how npm matches an alias, or how a bundle ships, and admit
  it.
- A project's rebuild needs more than an hour: make the bound an operator
  input.
