# Pin the Checkout Once per Caller Call

Chose one hardened commit reader per `ObserveBaseInputs`, `EvaluateRebuild`,
or single-file call over pinning once per file. The pin binds an operation to
one resolved Git directory, isolates its index, and requires SHA-1 at open.
It neither proves repository identity nor freezes the objects at that path.
The caller already proves repository identity once for its call.

Each read still validates the exact SHA-1 name and inspects its tree for
regular-file mode and size. Only `ls-tree` and `cat-file` run after open;
neither depends on earlier reads or the scratch index. Existing verification
already uses one hardened runner for recipes at both base and head.

## Refute-First Findings

A fresh-context independent review ran a hardened command matrix on Apple
Git 2.50.1 before implementation. Its findings were:

- **Confirmed:** The absolute Git-directory pin survives checkout symlink
  retargeting. Per-read argument, mode, size, and plumbing checks supply the
  documented exact-tree and absence guarantees.
- **Disproved by checks:** Reusing scratch state changes reads. A corrupt
  checkout index, dirty worktree, and hostile inherited Git configuration
  and repository environment left the committed bytes unchanged.
- **Confirmed:** Replacing the pinned Git directory with a SHA-1 repository
  lacking the commit, SHA-256 (including compatibility mode), or no directory
  fails through plumbing. None returns content or treats failure as absence.
- **Allowed by the issue's explicit decision:** SHA-256 replacement after
  open reports `ErrGitPlumbing` rather than `ErrUnsupportedRepo`. An invalid
  commit paired with an unopenable checkout may report the checkout fault
  first in grouped project-image calls. The single-file wrapper preserves
  argument validation before open.
- **Allowed by acceptance 6:** Retargeting the checkout symlink deliberately
  keeps reading the original repository, even when a fresh per-file pin would
  fail against the new target. Equality with fresh pins excludes this case;
  it preserves the repository identity established at open.
- **Confirmed limitation:** A writer inside the resolved Git directory can
  change its contents. Neither per-file nor grouped pinning makes multiple
  Git commands atomic; that writer is outside what the pin protects.

## Behavior Comparison

Reconstructed the original observer, rebuild gate, and single-file verifier
from main at `f19c9091` in a temporary package. Old and new implementations
returned identical complete observations, rebuild decisions, and error classes
on the existing exact-commit fixture cases and 48 generated commits spanning
source-only changes, admitted dependencies, missing lockfiles, unsupported
inputs, recipe changes, and malformed manifests. Ten direct file reads and
four recipe reads also matched in content, presence, and exact error text.
The changed-checkout tests separately confirm the allowed pin-stability and
error-class differences above, and verify that reads after close fail.

Revisit when a reader is kept longer than one caller call, shared between
goroutines, or gains a command that uses the scratch index.
