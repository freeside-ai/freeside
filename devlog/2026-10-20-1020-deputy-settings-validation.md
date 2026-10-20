# Validate Deputy Settings Before Taking The Lease

For #2029, chose to refuse known invalid settings before taking the
production rig lease, rather than record a passing composition and then
fail daemon startup. These are usage errors (exit status 2), not failed
self-check cards. An early refusal creates no database, lease record,
credential adoption, or composition evidence. It leaves earlier evidence
alone.

Chose shared validators over a second list of daemon rules. The deputy and
normal startup use the same listener address policy, manual configuration
parser, Claude configuration validator, and prompt loader. One builder maps
settings into the prospective and enabled Claude configurations. A failed
self-check still leaves the daemon's Claude driver unset.

Prompt validation now reads all three packages before composition stores
their exact bytes. This preserves the content digests and avoids validating
one version of a file and persisting a later version. A rejected composition
may leave fewer unused blobs than before; partial blob writes are not an
acceptance record.

Review auth stays subscription-only until #1677 implements API-key
enrollment. The settings enum previously admitted API keys even though
adoption reads subscription credentials. Configured host paths must be
absolute and clean, and the vendor-instruction path must not be the root.
The named auth volume remains outside the host-path rules.

Validation and use remain separate reads. Files, DNS, Tailscale ownership,
listener availability, and other runtime state can change after the early
check. Normal startup therefore keeps its own checks. Passing parsed inputs
through the entire startup path would close some of those windows at the
cost of a wider change; this unit only rejects failures already knowable
before the lease.

## Refute-First Findings

- Confirmed: malformed manual configuration could previously fail after
  passing evidence was saved. The regression requires a usage refusal and
  uses a failing lease-host probe plus absent lease and evidence artifacts.
- Disproved by the configuration comparison: early validation and an enabled
  driver use different settings. Their fields match after excluding the
  launch's judgment digest, which does not exist at early validation.
- Disproved by comparison against `36328c4`: extracting the listener and
  prompt validators changes their acceptance rules. Generated inputs matched
  across 16,191 listener cases and 992 prompt configurations, including
  accepted addresses, network family, bind errors, unexpected-bind closure,
  prompt bytes, and content digests. Independent review found no defect in
  these boundaries.
- Allowed by the work contract: a later file or network change can still
  fail startup. Early validation is not an atomic launch guarantee.

Revisit when API-key enrollment lands, settings move into the store, or a
new startup validator adds a refusal outside the shared checks.
