# Exclude Ignored Build Output At Candidate Construction

The owner assigned #1132 after a live feedback Retry completed its provider
handoff but failed containment on generated JavaScript outside the approved
source paths. The earlier prompt instruction to clean after testing did not
reliably prevent the recurrence. The repair must keep the scope gate intact.

Chose importer-side exclusion of new regular files that are both outside the
declared scope and ignored by the exact trusted base. The exporter remains
Git-blind and returns all bytes for validation. The importer still runs every
existing check over the original changes. It then removes only those additions
from construction and their now-inapplicable allowlist findings. All other
findings remain. No excluded content can enter any intermediate or final commit.

Only out-of-scope additions qualify. In-scope ignored files may be intentional
new repository content and remain unchanged. Previously publishable imports
therefore preserve their replay heads: none contained these blocked additions.
Specification imports and no-change handoffs bypass exclusion and keep their
existing contracts. A commit plan must still cover the resulting candidate;
plans naming excluded files take the existing structural fallback.

Native Git matches only regular `.gitignore` blobs from the exact base in an
isolated scratch repository. It receives no candidate rules, working-tree
metadata, checkout-local excludes, or host configuration. Only rules strictly
above queried files are materialized, so directory-only rules cannot mistake a
candidate regular file for a scratch directory. Directory spelling aliases
fail closed before filesystem materialization. Existing import caps bound the
selected base rule files.

Rejected scope widening and finding-profile tolerance: those would admit
unapproved repository content. Rejected workspace Git inspection: the workspace
index and configuration are untrusted. Rejected automatic workspace deletion:
exclusion at construction needs no cleanup authority and preserves the handoff
for its existing evidence and recovery paths. Rejected prompt-only cleanup as
the repair because the observed run already violated that instruction.

## Refute-First Findings

- Confirmed: materializing a rule beneath a candidate regular leaf can change
  directory-only matching. Restricting materialization to query ancestors and
  a regression for a tracked directory replaced by a file close that case.
- Confirmed: scratch HOME must be outside the matching worktree, or a regular
  candidate named `home` can appear to be a directory.
- Confirmed: filtering before the no-change gate would weaken blocked-terminal
  semantics. No-change imports skip exclusion; the regression still refuses
  ignored additions.
- Confirmed: case and Unicode directory aliases can mix distinct rule scopes
  on the reference filesystem. Matching refuses ambiguous query/rule spellings.
- Confirmed: retaining every ancestor string amplified a valid 4 MB path set
  into over 500 MB before matching. Selection now retains only real base rules;
  alias checks sort full names by components without retaining ancestor maps.
  A deep-path allocation benchmark and an ancestor/sibling ordering regression
  cover the review findings.
- Retained: secret and scan-cap findings, byte caps, non-regular and corrupt
  blobs, protected paths, and tracked modifications/deletions still block.

Revisit when the product needs an explicit generated-content policy beyond
trusted base ignores, or when exporter transport limits make retaining ignored
bytes for validation impractical. Neither concern authorizes weaker checks.
