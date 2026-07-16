# 0001: License Freeside under AGPL-3.0-or-later

- Status: Accepted
- Date: 2026-07-15
- Decider: Ben Nelson-Weiss

## Context

Freeside is a monorepo whose dominant component is `freesided`, a Go daemon
with a network API consumed by macOS and iOS clients. The original licensing
candidate was AGPL-3.0, but the decision was deferred with open-source
packaging to Phase 4 while the repository was private and largely empty.

Private-repository GitHub Actions capacity is now constraining development,
which changes the timing assumption behind that deferral. Public repositories
can use standard GitHub-hosted runners without consuming private-repository
minutes. The Phase 4 architecture still centers on the network daemon, so the
original project classification remains valid.

## Decision

License the entire Freeside monorepo under AGPL-3.0-or-later and make the
repository public after the licensing change lands. Applying one license
repo-wide keeps the daemon, clients, API specification, policy, prompts, and
documentation under a coherent grant as they evolve together.

The owner applies the same grant to every revision in the repository's history
for which he holds copyright. Material that identifies a different license or
copyright holder remains governed by those terms. This explicit historical
grant avoids publishing earlier, apparently unlicensed snapshots without
rewriting the repository's commit graph.

AGPL reciprocity is intentional: a modified Freeside offered to users over a
network must offer those users the corresponding source. Freeside is a
standalone control plane rather than a library intended for embedding, so
strong network copyleft protects improvements to Freeside itself without
sacrificing a primary distribution model.

## Consequences

- Recipients may use, study, modify, and redistribute Freeside under the
  AGPL-3.0-or-later terms.
- Distributing a modified version, or exposing one to users over a network,
  carries the license's corresponding-source obligations.
- Some organizations prohibit or specially review AGPL software. That may
  reduce enterprise adoption; the owner accepts that cost in favor of keeping
  network-deployed improvements available to their users.
- This decision does not assert that a future App Store binary can be
  distributed under AGPL without additional terms. Before App Store
  submission, review the then-current store agreement and choose a compatible
  custom EULA, additional permission, client-specific license, or non-store
  distribution path in a separate licensing decision.
- Repository visibility changes only after this license is present on the
  default branch and the historical grant is recorded, avoiding ambiguous
  public snapshots.

## Alternatives considered

- **MPL-2.0** would reduce enterprise-policy friction but provides only
  file-level reciprocity and would permit proprietary network-service forks.
- **GPL-3.0-or-later** fits the local clients but does not close the daemon's
  network-use gap.
- **LGPL-3.0-or-later** targets dynamically linked libraries, not this
  standalone Go service and application monorepo.
- **CC BY-SA 4.0** fits knowledge artifacts but is not an appropriate primary
  software license.

## References

- [Issue #18](https://github.com/freeside-ai/freeside/issues/18)
- [Repository-publication follow-up #101](https://github.com/freeside-ai/freeside/issues/101)
- [Originating decision note](../../devlog/2026-07-08-1051-scaffold-phase0.md)
- [Plan revision 9 decision record](../history/decisions.md#revision-9-current)
- [Licensing philosophy](../../LICENSING-PHILOSOPHY.md)
- [GNU AGPL 3.0](https://www.gnu.org/licenses/agpl-3.0.html)
- [GitHub Actions billing and usage](https://docs.github.com/en/actions/concepts/billing-and-usage)
- [Apple licensed-application EULA](https://www.apple.com/legal/macapps/stdeula/)
