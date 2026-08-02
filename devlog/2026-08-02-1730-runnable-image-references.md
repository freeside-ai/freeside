# Require Explicit Registry Mode for Runnable Image References

Chose to require exactly one explicit registry mode before any exporter or
agent image build, rather than preserve the successful local-only build or
select a loopback registry port implicitly. Ward cannot resolve a local
content-store `name@digest` on the supported Apple `container` runtime, so a
successful local-only result violates the plan's runnable-image contract.
Failing before the build avoids both an unusable success value and wasted build
work. An implicit port would trade the invalid output for hidden collision and
lifecycle ownership.

The existing `--registry` and `--local-registry-port` paths remain the two
supported modes. Both push, pull, verify, and emit the exact digest reference;
the later shared-helper extraction in #456 must preserve this corrected caller
contract.

Revisit when: the supported runtime can demonstrably resolve a locally built
digest reference without registry seeding, or the image build interface gains
an owner-managed registry allocation mechanism.
