# Prove Returned Follow-Up Issue Membership

Issue: #1831.

The live repository-ID check proves the requested target, but it cannot prove
that an issue returned by the forge belongs to that repository. Both create
and list decoding now require `repository_url` and `url` to agree with the
configured API root, trusted repository name, and positive issue number.
A lookup of the returned number in the target repository is insufficient:
issue numbers can collide across repositories.

Filing also opts into exact-collection pagination. A next link must retain
the API origin and all listing filters; only the sequential page number may
advance. The collection may use either the trusted repository name or GitHub’s
documented `/repositories/{id}/issues` path bound to the live repository ID.
The initial exact-path plan incorrectly excluded this ordinary GitHub response;
[the corrected issue record](https://github.com/freeside-ai/freeside/issues/1831#issuecomment-6061479714)
records why numeric pagination is supported while foreign IDs are rejected.
Requests use the verified numeric collection from the first page onward,
including when a valid next link spells the target by name. A name may be
rebound after the live ID check. GitHub documents that Apps retain
[read access to public repositories](https://docs.github.com/en/apps/using-github-apps/installing-a-github-app-from-a-third-party),
so a single-repository token does not prove a public listing stayed on its
original repository. Numeric routing closes that race; returned name URLs
still fail closed on a rename. Creates remain restricted by the token's
verified single-repository write grant.
Checking only returned rows misses an empty foreign page. Rejecting the link
before sending it preserves the complete-listing requirement. Other readers
retain their existing pagination policy.

An invalid 201 remains unproven because creation may have committed. The
existing settlement path can adopt a later valid candidate, but cannot resend
that create. An invalid listing row fails the whole read; dropping it could
turn several candidates into an apparently unique candidate.

## Refute-First Findings

- **Confirmed and fixed:** The old decoder accepted foreign or contradictory
  membership evidence. Ledger-level regressions fail against the old code and
  pass with validation, including a foreign issue number also present in the
  target repository.
- **Disproved by checks:** Invalid create evidence might permit a resend or
  survive restart as accepted lineage. Tests assert no immediate ledger row,
  reopen the store, recover from valid evidence, and assert one create.
- **Disproved by checks:** A valid first-page candidate could hide an invalid
  later row, or an empty foreign page could leave a partial listing accepted.
  Tests assert no ledger row, no foreign request, and the existing bounded
  terminal ambiguity. Pre-dispatch failures send no create.
- **Confirmed and fixed:** An untrusted next link could retain the collection
  path while changing filters or skipping pages. The reader now preserves the
  initial filters and allows only the next numbered page before dispatch.
- **Confirmed and fixed:** Parsing a malformed next-query pair can discard it
  during normalization. The reader rejects any query ParseQuery reports as
  malformed before it compares filters or sends the next request.
- **Confirmed and fixed:** A legacy Link extractor could hide an empty next
  target or malformed query syntax. Filing now consumes every Link header and
  rejects malformed or duplicate next entries. Ledger tests prove an empty next
  target cannot adopt a candidate, request another page, or resend a create.
- **Confirmed and fixed:** Link parsing initially overwrote a body decode error.
  Separate errors preserve failure even when JSON partially populates a row; a
  valid next link cannot turn that partial decode into an accepted listing.
- **Disproved by checks:** Trusted numeric pagination could be rejected or a
  foreign numeric ID accepted. Tests recover a ledger candidate through the
  live-ID-bound collection and reject a different ID before the next request.
- **Confirmed and fixed:** A name rebound after the live ID read could return
  an otherwise admissible public issue under the old name. Filing now requests
  all pages by the verified numeric ID. Ledger regressions rebind the named
  route between resolution and reading, including a named second-page alias,
  and prove no replacement issue is adopted and no create is resent.
- **Allowed by the issue contract:** URL matching uses the configured API root
  and trusted repository naming convention exactly, as the reply boundary
  does. It does not add case normalization, rename, or redirect support.
  Positive HTTP fixtures include an API root with a path prefix.

Revisit when supported forge behavior requires canonicalization different
from the configured repository spelling or API root. That change must retain
proof of returned-resource membership and complete collection reads.
