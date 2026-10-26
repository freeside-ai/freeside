# Classify Project-Image Build Failures

Work unit: #1792. Source: `devlog/2026-10-06-1236-policy-gated-rebuild.md`.

## Decision

A project-image build failure is a verdict on the candidate only on positive
evidence that the candidate caused it; everything else retries under the
publication lane's `publication_environment` hold.

- **Install verdict.** `appleBackend.Build` returns `ErrInstallFailed` when the
  failed `container build` output holds a bare `npm error code EINTEGRITY`
  line, or `npm error code EUSAGE` followed by `npm ci`'s out-of-sync message.
  The engine blocks it as `build_or_proof_failed`, with npm's lines leading the
  evidence.
- **Proof verdict.** `ErrProofFailed` now means a proof ran and its result
  failed. A container-runtime command that failed inside a proof is marked
  with the package-private `runtimeFault` and keeps no verdict; so does a base
  tag that drifted from the approved digest, which is daemon configuration.

Chose reading the build's existing output over changing the Containerfile (a
distinct exit code or marker file for the install step) because the
Containerfile and preparation helper feed `PreparationDigest()`: any edit makes
every admitted image stale until #2333 lands.

Chose a fault marker over a proof marker for the provenance and allowlist
checks. Those checks have many comparison and decode failures and few runtime
calls, and every failure was a verdict before, so marking the runtime calls
changes only what the issue names. The marker type adds no message text. The
allowlist check's cleanup failures are marked too, because cleanup runs after
the check has its result. A check that failed and whose cleanup then failed
carries both, and the fault wins: the build retries.

## Codes Chosen and Left Retrying

npm 11.16.0 (the npm Node 24.18.0 ships) was run for each case; the fixtures
are in `daemon/internal/projectimage/testdata/npm`.

- **Verdict:** `EINTEGRITY`; `EUSAGE` with the out-of-sync message.
- **Retry:** `ECONNREFUSED`, `ECONNRESET`, `ENOTFOUND`, `EAI_AGAIN`,
  `ETIMEDOUT`, `FETCH_ERROR` (npm's fetch timeout, not `ETIMEDOUT` as the plan
  assumed), `E503` and other 5xx, `E404`, `ETARGET`, and any output the
  classifier doesn't recognize. `E404` and `ETARGET` can be the candidate's
  (a package that never existed) but also a registry that lost or hasn't yet
  replicated a version, so they need evidence the output doesn't give.

## Refute-First Results

- **Network faults that look like `EINTEGRITY`: confirmed, allowed by
  decision.** npm reports `EINTEGRITY` for a close-delimited response
  truncated mid-body and for a 200 response whose body a proxy replaced. A
  truncated body with a `Content-Length` reports `ECONNRESET` and retries. The
  rebuild gate requires every changed lock entry to resolve from an `https`
  URL on a declared registry host, and the build proxy carries `https` as an
  opaque CONNECT tunnel, so a substituted body needs a declared registry
  serving wrong bytes for an immutable tarball. Truncation remains possible
  only against a declared registry that sends close-delimited bodies (neither
  a length nor chunked encoding), since the proxy passes an upstream reset on
  as a clean close; npm then retries the tarball twice before it reports.
  Either case blocks the candidate, with the wanted and received hashes in
  the evidence.
- **Candidate text forging an npm code line: disproved by a check.** A
  lockfile `deprecated` message with embedded newlines prints as
  `npm warn deprecated npm error code EINTEGRITY`: npm prefixes every line of
  a message. The classifier matches whole lines after BuildKit's prefix, so
  the candidate can't forge a code line. A forged match would only block the
  candidate's own publication.
- **Hiding a real install failure: confirmed, allowed by decision.** npm
  prints its error last and the runner keeps the output's last 1 MiB, but a
  candidate whose `npm ci` prints more than that after the code line (tens of
  thousands of out-of-sync entries), or enough before it to reach BuildKit's
  per-step log limit, can push the code line out. BuildKit's closing excerpt
  keeps only ten lines. The build then retries, which is the behavior before
  this change; a verdict needs evidence, and no evidence means a retry.
- **Cleanup failure after a passing allowlist check becoming a proof failure:
  confirmed, fixed.** Raised by the independent reviewer; the cleanup errors
  are now marked.
- **Preflight failure as the candidate's: disproved.** The preflight runs the
  built image, not the base, but `true` comes from the base's layers, and the
  candidate's dependencies only add files beside them. Provenance has
  already bounded every layer at 1 GiB before the preflight, so an image too
  large to start fails there as a verdict.
- **Callers depending on the old classes: disproved.** No caller in `cmd/`
  branches on `ErrProofFailed`, and none string-matches the changed messages.

## Limits

- `container run` can't tell a runtime failure from the workload's own exit, so
  a runtime that fails after the preflight and still returns an exit code stays
  a proof failure.
- Host scratch failures inside a provenance check (a temporary file it can't
  create) and a runtime that answers with output the check can't decode are
  not runtime commands that failed, and keep today's verdict.
- A retried fault records no cause under the `publication_environment` hold
  until #2333 raises host-fault items.

Revisit when the Node toolchain pin moves to a new npm major, or when #2337
admits Go dependency changes and Go module downloads need the same rule.
