# Registry Egress Profile and Policy-Gated Image Rebuilds

Work unit: #871 (plan revision 38). A safety-policy change, so a note
is mandatory. The revision adds a `provider_registry` egress profile
between `provider_only` and `provider_web_read`, a policy-gated automatic
rebuild of the project image when a dependency change stays inside that
profile's registry set, and the ward conformance that binds the realized
registry allowlist to the proven egress boundary. Nothing in it moves
the `provider_only` default or adds writer code; the implementation
lands in two Wave 7 deferral units.

## Principle

The egress floor is a credential-containment boundary and does not
move. Capability above the floor is added as narrower, separately priced
risk classes (read-only package registries through the allowlisting
proxy) and as machine gates (policy-checked project-image rebuilds),
never by widening the writer toward general web under
`subscription_contained`.

## Why Now: the Hosted-Agent Comparison

Under the default profile the writer reaches only the provider API
through the daemon's CONNECT proxy, and the project image bakes the
dependency closure so verification never fetches. That matches the
hosted agents' default shape as documented when this was written
(2026-08): Codex cloud's agent phase and Claude Code on the web both run
egress-off or proxy-allowlisted by default, and both are the right floor
against the §14 organizing threat.

Freeside is stricter in exactly one place: a changed dependency closure
"fails loudly and requires a new reviewed project image," so every
dependency edit becomes a run-boundary AttentionItem, where the hosted
agents rerun a setup script. That strictness was buying a human round
trip, not containment: the human already reviews the manifest change in
the PR diff and the image provenance record. The revision removes the
round trip without touching the boundary.

## Decision: a Distinct Risk Class, Not a `provider_web_read` Subset

`provider_registry` admits a short, policy-declared set of read-only
registry authorities, consumed read-only (initially the Go module proxy
and sum database, npm, and PyPI plus its file host) through the same
CONNECT proxy. On its merits: no DNS, no direct egress, no
attacker-operated host, and exfiltration bounded to what those
registries' own endpoints accept. The enforcement fact already in code
makes the class hold: the proxy allowlist is per authority with the TLS
server name pinned to the CONNECT authority
(`daemon/internal/ward/egress.go`, `requireTLSServerName`), so a
registry entry cannot be used to reach a shared-CDN neighbor.

The class holds only while every entry is a registry the attacker does
not operate, and SNI pinning proves reachability, not trust. The set is
therefore control-plane policy (a writer change to it is
publish-blocked), and a per-project addition admits only a public
package registry the project's dependency manifests resolve against.
Any other authority is a `provider_web_read` decision with its explicit
wider-exposure record; "added under review" alone was rejected as the
criterion because review of an opaque tunnel endpoint cannot establish
the property the class depends on.

The class is not "read-only" by enforcement. A CONNECT tunnel passes
opaque TLS bytes and cannot constrain HTTP method or path, so a registry
that co-hosts a write endpoint (npm publish) leaves a residual: an
injected writer holding an attacker-supplied registry credential could
publish workspace content there. The residual is recorded in §14 rather
than hidden behind the label; a project policy may exclude such hosts,
and `api_key_isolated` remains the escape for anything wider. (Raised
by the pre-push refute review; accepted as a recorded residual, not
fixed by dropping npm from the initial set, because the containment
property that matters, no attacker-operated host, still holds.)

Rejected: folding the class into `provider_web_read` with a
registry-only allowlist. That record exists to price general read-only
HTTP (URLs, headers, bodies, redirects, DNS); pricing the registry class
under it would either overstate the registry exposure or quietly
understate the web one. Rejected: making `provider_web_read` the
default, and widening the writer to general web; both move the floor.

## Decision: a Machine Gate for In-Policy Dependency Changes

The rebuild gate holds when the manifest delta is lockfile-consistent,
every added or changed package resolves from the project policy's
declared registry set, and the verification recipe is unchanged. The
builder reads that set whatever the writer's profile: builder fetch
authorization and writer egress are different concerns, so a project on
the default `provider_only` still gets the rebuild without widening
writer egress; `provider_registry` is the profile that additionally
exposes the same set to the writer. The
reusable builder then rebuilds from the trusted recipe, reruns the
networkless positive run and the negative probe, records provenance, and
the run resumes against the new digest-pinned reference. Any other delta
(new authority, unpinned or VCS source, recipe change, failed run or
probe) keeps the fail-loud path.

Rejected: leaving every dependency change a human gate (the status quo;
the round trip buys nothing the PR diff does not already give).
Rejected: a setup-script phase with open internet, as the hosted agents
run. The bake step already provides the setup-script outcome, and it
does so without a credential-holding network phase; adding one would
reintroduce the exposure the floor exists to remove.

## Ward Binding

`supports_enforced_provider_egress` gains the registry-host set as part
of the proven allowlist. The realized proxy allowlist is
conformance-checked against the declared profile, not trusted from
configuration; a configured profile whose realized allowlist differs
fails conformance like any other capability mismatch.

## Scheduling

`registry-capable egress profiles` leaves the §11 Phase 2 list. The
Wave 7 (1B.1) deferral drain names two sweep-eligible units: (a) the
profile, its policy field, and ward conformance, `kind:contract`
because `EgressProfile` is a domain enum carried in the admission record
(the spine assigns its contract-chain position at Wave 7 planning); and
(b) the policy-gated rebuild in the reusable builder. The rebuild gate
reads the registry set from the policy field unit (a) declares, so (b)
`starts-after` (a); the contract merges before its dependent starts.
Both build on merged work (#302 proxy enforcement, #334 builder). Wave 6
and #835 are untouched.

Assumption carried into unit (a): `go`, `npm`, and `pip` honor
`HTTPS_PROXY` for registry fetches through a CONNECT proxy. True for all
three today; the implementation unit proves it, this revision states it
only as the mechanism.

Revisit when: real runs show dependency churn is rare enough that unit
(b) is not worth its builder complexity (drop it and keep the human
gate); or a writer research path is requested, which is a separate
revision and not an extension of the #655 elaborator design.

Follow-up: deferral issues for units (a) and (b) are filed after merge
and linked here.
