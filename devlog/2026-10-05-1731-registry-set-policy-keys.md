# Registry Set Policy Keys

Issue #1627 adds the `provider_registry` egress profile (plan §5.4) and the
project-policy keys that declare its registry set. It is a contract unit:
`daemon/internal/domain`, production-stage admission in
`daemon/internal/engine`, and the API enum move together. Nothing enforces
the set yet. #1628 teaches the ward proxy to admit it, and #1629 reads it at
the policy-gated image rebuild.

The earlier note `devlog/2026-08-21-1510-registry-egress-profile.md` decided
that `provider_registry` is its own risk class. This note records how the
class is declared and admitted.

## Two Policy Keys

Chose two resolved-policy keys over one key whose presence opts the writer
in. `execution.egress_profile` selects the writer's profile, and
`execution.registry_set` declares the hosts. One key was rejected because
plan §5.15 has the rebuild gate read the set "whatever the writer's
profile": a project must be able to declare registries for its image build
while its writer stays on `provider_only`.

- **An absent profile key means `provider_only`.** A policy that declares
  neither key resolves as it did before, with the same policy digest, because
  the digest covers present keys only.
- **The profile key selects two values.** It accepts `provider_only` and
  `provider_registry`. `provider_web_read` is still chosen through a
  capability manifest with its wider-exposure record, and
  `clean_verification` is never the writer's.
- **Opting in without a set is a policy error.** So is opting in with a set
  that fails validation. A malformed set under `provider_only` does not
  refuse the writer: the writer does not use it, and #1629 refuses it at the
  gate that does.

## The Policy Value Is a Host Array

Chose a bare canonical JSON array of host names as the policy value, with a
versioned record (`encoding_version`, `hosts`) as the form the content
address covers. The issue contract and its implementation plan disagreed
here: the contract says "a canonical JSON array of lowercase host names",
and the plan sketched the versioned object as the value itself. The contract
is authoritative, so the value is the array. The versioned record is kept
for the digest because the domain's content-addressed types carry an
explicit encoding version, and a later change to what an entry holds then
changes the version instead of colliding with an old digest.

The reader refuses anything but the canonical spelling: unsorted or
duplicate hosts, whitespace, escapes, or a wrapping object. One set has one
policy value, so the value an operator reviews is the value that is hashed.
`NewRegistrySet` does not sort or deduplicate for the same reason.

## The Daemon Checks Syntax, the Operator Checks Meaning

Chose a syntactic host check over any attempt to decide whether a host is a
registry. Plan §5.4 makes a per-project addition "a reviewed operator change
that admits only a public package registry the project's dependency
manifests resolve against". The daemon cannot know that, so it refuses only
what could never qualify:

- Anything but lowercase letters, digits, hyphens, and dots. That rules out
  schemes, ports, paths, userinfo, wildcards, and IPv6 literals.
- A single-label name, an empty label, or a label that starts or ends with a
  hyphen.
- A final label that starts with a digit. No public top-level domain does,
  and every spelling a resolver reads as an IPv4 address ends in a numeric
  label, including the short and hexadecimal forms.
- A reserved top label: the IANA special-use names (`alt`, `arpa`, `example`,
  `invalid`, `local`, `localhost`, `onion`, `test`) and the labels ICANN
  withholds from delegation because private networks use them (`corp`,
  `home`, `internal`, `mail`).

The reserved list stops at the two registries. Private conventions such as
`lan`, `localdomain`, and `svc` were left out: they are not reserved, the
list of them has no end, and refusing some would suggest the rest are safe.
A punycode label (`xn--`) is accepted without decoding. Whether it is a
look-alike of a real registry is the same question as whether any name is the
registry the operator meant.

A declared name is relative, and a public name can still resolve to a
private address. Both are properties of the connection, not of the
declaration: #1628's proxy has to resolve the name as a rooted one and refuse
a non-public address.

## No New Admission Field

Chose to leave `ExecutionAdmission` unchanged over adding a
`registry_set_digest`. The admission already records `policy_digest`, which
binds both keys, and the profile it was admitted under. A new field would
have re-encoded every admission golden for a value the record already
implies. The realized allowlist belongs on the agent binding's
`effective_egress`, which #1628 fills.

Admission now reads the run's stored policy to learn the requested profile,
so the binding has to be real. A writer attempt whose run has no stored
policy, or whose stored policy digest is not the run's, is refused. Falling
back to `provider_only` was rejected: it is the safe profile, but the
admission would then record a `policy_digest` nothing was read from. The
production driver already refuses to start such a run when it resolves the
path allowlist.

## A Request Is Not Enforcement

Chose to refuse a requested profile the composition cannot enforce over
recording it and relying on the downstream gates. The writer's profile is
checked against `EnforceableEgressProfiles` after policy and any capability
manifest have selected it. The production composition declares only
`provider_only`, so a policy that opts in admits nothing until #1628 adds
`provider_registry` to that set.

The gates that admit only `provider_only` (`ward`, the Claude start spec,
the stage driver, and the engine's specification and discussion paths) are
unchanged. They would also have refused the launch, but later and with an
admission already recorded under a profile nothing provided.

The check sits in the writer-stage branch only. Every other stage runs under
the composition's default profile, which `WithAdmission` already requires to
be enforceable.

The gate applies to the selected profile, not to how it was selected. A
capability manifest names a profile and no set, and since the manifest's
profile is any valid member, a manifest can now name `provider_registry`.
Admission therefore requires a valid declared set for any writer admitted
under that profile. Refusing the profile inside a manifest was rejected: a
retry under registry access is a legitimate manifest, and the rule that
matters is that the set exists.

The failure card applies the same gate before it offers a manifest. A retry
inherits its parent's policy keys, so a registry manifest offered under a
policy with no valid set could only allocate a run that holds at admission
and never clears. The card withholds that choice. Acceptance gained no check
of its own: it takes only a manifest the card offered, a run's policy does
not change between the offer and the choice, and an error there fails the
reconcile pass for every run. Admission keeps its gate, so the state still
holds one run if it ever appears.

## A Refused Request Holds One Run

Chose a per-invocation hold under `admission_policy_refused` over the two
other outcomes the dispatch loop offers. Both refusals (a profile the
composition does not enforce, and policy content the readers refuse) are
verdicts on one run's own policy.

- **Left unclassified, the refusal stops the workflow.** An error the
  dispatch loop does not recognize ends the reconcile loop, and the same run
  refuses again after a restart. One opted-in project would stop every other
  run. The first draft of this unit had that defect.
- **Ending the pass quietly was rejected.** That is how the loop treats a
  composition-wide verdict such as a backend below the floor, where every
  later intent would refuse too. Here the next run is unaffected, so ending
  the pass would starve it.
- **An unreadable policy stays loud.** A run whose stored policy is missing
  or does not match its digest is a broken binding, not a policy verdict. The
  submission transaction writes both, so nothing reaches this state without
  corruption, and it keeps the failure path corruption has.

The two holds differ in how they clear. An unenforceable profile clears when
the composition enforces it. Refused policy content never clears, because a
run's resolved policy does not change: the run stays held, visibly, until the
operator stops it. That is the right outcome at admission and the wrong place
to learn of a typo, so the keys should also be refused where policy enters
the daemon. `daemon/cmd/freesided` is outside this unit's scope.
Follow-up: #1771.

## The API Enum Becomes a Named Schema

Chose to promote the inline enum on `CapabilityManifestOffer.egress_profile`
to a named `EgressProfile` schema over keeping it inline with a one-off
test. The project's drift pin, `TestOpenAPIEnumsMatchDomain`, compares named
schemas with the domain's registration slices. The generated Swift type is
renamed; the hand-written call sites use implicit member syntax and
`rawValue`, so none needed an edit.

## Publish-Blocking Rests on the Trust Profile

The plan makes the set control-plane policy: a writer change to it blocks
publication. The importer has no built-in pattern for where a project keeps
its policy file. A path is prompts-and-policy because the operator's trust
profile names it (`ExtraPromptsAndPolicyPatterns`), and the importer test
shows the block under that condition. Adding a built-in default was left
out as a risk-posture decision of its own.

Revisit when a policy-file parser lands and gives the declaration a fixed
location: the default pattern can then follow it.

## Refute-First Findings

An independent reviewer tried to prove the change wrong before the first
commit. Each claim and its outcome:

- **Confirmed and fixed: a refused opt-in stopped the reconcile loop.** See
  A Refused Request Holds One Run.
- **Confirmed and fixed: a manifest could select the registry profile
  without a declared set.** Inert while the composition enforces only
  `provider_only`, reachable once #1628 widens it. See A Request Is Not
  Enforcement.
- **Confirmed and fixed: the reserved suffixes missed registered special-use
  names** (`alt`, `arpa`, `onion`, and the withheld `corp`, `home`, `mail`).
- **Confirmed and fixed: tests asserted less than their names.** Added the
  remediation and operator-feedback writer stages, a non-writer stage, the
  manifest-selected profile, and accept cases at the 63-byte and 253-byte
  boundaries.
- **Disproved by a second pass: the hold fix itself.** A fresh reviewer
  tried to make a refused run stop the pass, starve a later run, grow a row
  or notice per pass, leave an attempt or admission half-written, or block
  the operator's stop. None held; the loop-level test in the engine pins the
  first three.
- **Allowed by decision: the hold shows a reason code, not the refusal
  text,** and the held-work notice says the hold may clear by itself. Every
  hold class shares that surface. It misleads only for policy content that
  never clears, which #1771 refuses before a run exists.
- **Allowed by decision: an unenforceable capability manifest still fails
  loudly** in `capabilityManifestForRun`, before the new gate. That path is
  older than this unit, and a manifest is offered only when the composition
  enforces its profile.
- **Confirmed and deferred: the store does not re-gate a `provider_registry`
  admission against the run's policy.** The engine is the only producer and
  applies the rule; `daemon/internal/store` is outside this unit's scope.
  Follow-up: #1772.
- **Allowed by decision: unreserved private suffixes and punycode labels
  pass.** See The Daemon Checks Syntax, the Operator Checks Meaning.
- **Allowed by decision: the importer test would pass for any file name.**
  That is the rule it shows: the trust profile, not a built-in pattern,
  makes the path control-plane.
- **Disproved by checks:** an IP literal, port, path, or userinfo spelling
  that passes the host check (about 85 candidates, none accepted); a
  non-canonical encoding of an accepted set (20 variants, including
  duplicate keys and escapes); a production writer run without a stored
  policy (the submission transaction writes both); a changed digest or
  profile for a policy without the keys; a downstream `provider_only` gate
  that the new member widened; and drift between the API enum, the
  generated client, and the two contract digests.

## Revisit When

- #1628 lands conformance. The set's digest may then belong on the
  proven-allowlist record, which is why `RegistrySet.Digest` exists before
  anything reads it.
- A registry needs a non-default port or a path prefix. Entries are host
  names today, and the encoding version is the place to change that.
