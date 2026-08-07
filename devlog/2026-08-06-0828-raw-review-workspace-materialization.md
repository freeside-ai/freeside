# Raw Review Workspace Materialization

Issue #534 needs a retained review workspace whose bytes Ward can prove against
the candidate commit. The user chose raw Git-object materialization over a
checkout/reset implementation, and chose same-invocation reseeding when the
review source reports that no invocation exists.

## Decision

Retain the candidate into a freshly claimed destination from `ls-tree` and raw
`cat-file --batch` output. The engine adapter requires the sealed checkout minted
by its transport, and the underlying operation never modifies the source or
accepts an existing destination. Admit only regular blob modes `100644` and
`100755`, validate the complete tree before mutation, rebuild the
checkout-owned index without checkout filters, and verify the resulting bytes,
executable modes, paths, and detached HEAD. Refuse symlink mode `120000`,
gitlink mode `160000`, line-break paths Ward cannot attest, and every other
unsupported kind before claiming the destination.

Stream each blob to disk and stream the verifying hash. Bound the candidate's
regular-file tree to 100,000 entries and 512 MiB, Ward's default host-seeding
ceilings, and bound the plumbing listing itself to 64 MiB. Ward also counts Git
metadata and directories, so it may refuse a near-limit retained checkout more
strictly. These are producer safety ceilings; Ward remains the authorization
gate.

Keep the materializer and Ward's proof producer as independent implementations.
Their contract is tested by passing a materialized, `.gitattributes`-bearing
tree through Ward's real raw-byte observer and `verifyBaseProof`; extracting a
shared verifier would make the producer and prover repeat the same defect and
weaken the proof.

When `Inspect` reports `ErrUnknownInvocation`, always rebuild the retained path
from the repository-only checkout, even when an owned directory already exists.
The call site already holds that semantic answer, so workspace creation takes no
new review-source dependency or query. Build the replacement in a sibling first,
then remove the old non-empty directory and rename the replacement. A crash in
the absent window fails closed and the next unknown-invocation pass rebuilds it.

## Rationale

`git reset --hard` is not a raw tree extractor. It honors candidate-controlled
attributes such as `eol`, `ident`, and `working-tree-encoding`, while Ward hashes
worktree bytes with `hash-object --no-filters`. Git can therefore call a
transformed workspace clean while Ward correctly rejects it. Neutralizing
attributes with `-c attr.tree=<empty>`, `GIT_ATTR_NOSYSTEM`, and
`core.attributesfile=/dev/null` was rejected: `attr.tree` requires Git 2.40 or
newer, and older Git silently ignores the unknown configuration key. That would
reintroduce the same production crash loop without a compatibility error. Raw
object reads are version-independent and match Ward's trust model by
construction.

The lane currently requires Ward's `irregular=absent` proof, and host seeding
refuses symlinks and all non-regular source nodes. Faithfully writing symlinks or
gitlinks would therefore add behavior no review can approve. The smaller
fail-closed refusal is consistent with the lane's actual contract. A producer
defect remains an availability failure: Ward is still the independent gate, so
incorrect materialization cannot make a bad workspace pass.

The stale-path fix does not recover the already-recorded round-one contradiction
for run `run-bc28d74f`. A fixed daemon advances that run to round two, whose
invocation ID and workspace path are different. Reseeding instead protects an
upgrade or crash while the same round remains pending, where the same invocation
resumes onto a pre-fix unmaterialized directory.

## Refute-First Findings

Confirmed and fixed: a public destructive `dir string` API did not prove
checkout ownership before recursively clearing the nominated directory.
Retention now claims a fresh destination, leaves the source untouched, and the
engine adapter requires its sealed checkout capability. Confirmed and fixed:
lexical overlap checks missed symlink and `/tmp`-style aliases; retention now
resolves the physical source and destination parent before claiming the child.
Confirmed and fixed:
line-break paths pass Git object validation but cannot pass Ward's line-oriented
host digest, so they are rejected before destination creation. Confirmed and
fixed: buffering whole blobs and an unbounded tree listing exposed the daemon to
pre-Ward memory exhaustion; blob writing and verification now stream under the
same entry/byte posture Ward applies.

The post-push automated review confirmed two remaining pre-Ward amplification
paths and they were fixed in the same producer boundary: each tree path is now
bounded to 4,096 bytes, 256 components, and 255 bytes per component before
ancestor walks or filesystem writes, and the 100,000-entry ceiling counts
unique implicit directories as well as regular files. This mirrors the import
lane's existing total-path ceilings, adds the portable filesystem-name bound,
and closes superlinear validation, inode amplification, and deterministic
post-claim `ENAMETOOLONG` retries.
The path gate also reuses the importer's `GitUnsafeComponent` rule, so HFS
ignorable characters and NTFS trailing-dot, trailing-space, short-name,
alternate-stream, and backslash aliases cannot collapse onto `.git` when the
tree reaches a downstream filesystem. Sharing that producer-side repository
path posture does not weaken the proof: Ward's verifier remains an independent
implementation of raw workspace observation and comparison.

The read-only tree validation also reuses the importer's case and Unicode
normalization fold before claiming a destination. It applies that fold to
regular files and their implicit directory nodes, so leaf collisions, folded
file/directory conflicts, and directory-only spelling divergence cannot turn a
valid Git tree into an unmaterializable or differently spelled host tree. A
component trie retains each bounded node and folded component once; it does not
materialize and retain every full ancestor prefix, which would make legal deep
trees a pre-claim memory-amplification path.

Materialization and its verification both stay relative to the opened
`os.Root`. Reconstructing an absolute path during hashing or stray traversal
would make an otherwise admissible near-limit repository path fail only after
the destination root length pushed it beyond the host's absolute-path limit.
Stray traversal uses rooted `ReadDir` recursion rather than `io/fs`: Git and the
importer admit raw non-UTF-8 path bytes on Unix, while `io/fs.ValidPath` does
not.

The same rooted-path posture extends through Ward's independent snapshot
producer and instruction reader. Source walking, destination creation and
copying, repository-binding canonicalization, hashing, instruction discovery,
and recursive instruction imports operate through opened roots. This preserves
the admitted 4,096-byte repository-path ceiling without adding an absolute
snapshot prefix, and preserves raw Unix path bytes without routing them through
`io/fs` validation.

Raw object extraction uses `git cat-file --batch`, with each validated object ID
requested and its declared type and size rechecked before the bounded stream is
consumed. All attribute blobs share one process and all materialized file blobs
share one process, so the 100,000-entry ceiling also bounds protocol work rather
than permitting one serial process launch per entry.

Repository retention copies only `.git`, not the source's incidental worktree,
and applies the same 100,000-entry and 512 MiB ceilings before destination claim
and again while copying. The candidate tree's blob budget did not constrain the
fetched base's reachable history, so an unbounded `os.CopyFS` could exhaust host
disk before Ward reached its own seed budget and then retry the same failure.
An oversized or irregular repository is now a terminal materialization refusal;
ordinary bounded-copy I/O and cancellation failures remain operational errors.
Keeping the repository, rather than synthesizing a history-free object set,
preserves the review workspace's Git history while making its extra host-disk
cost finite and known before mutation.

A post-push refutation corrected what the listing-byte ceiling must guarantee.
Merely discarding bytes after the cap still lets `git ls-tree` consume unbounded
CPU and pipe I/O, and embedding `bytes.Buffer` silently promotes `ReadFrom` so
`io.Copy` can bypass a limiting `Write` method altogether. The bounded buffer
therefore keeps its storage as a named field and returns a sentinel on overflow;
the streaming runner owns the stdout pipe and kill-waits the child immediately
when its consumer refuses more data. The bound now limits producer execution,
not only retained memory.

The same refutation also changed the failure-routing model. Candidate-tree
shape, attribute, and resource-limit refusals are deterministic, so treating
them as an undifferentiated transport error makes both consumers retry work
that cannot succeed. They now carry a dedicated materialization-refusal class
that still unwraps to the broader transport class. The implementer adapter maps
it to a definitive seed refusal, while the review lane records a configuration
failure, raises durable attention, and completes the publication task. Ordinary
Git, filesystem, cancellation, and network failures retain their existing
retry behavior.

Revised by owner decision after new evidence: raw bytes under an attribute such
as `working-tree-encoding` can make porcelain Git status or diff report dirty
even while Ward accepts the exact raw-byte proof. That initially appeared to be
a reviewer-checkout usability tradeoff, but the shared producer also seeds the
implementer workspace. A coding agent could therefore receive a Ward-valid
workspace whose bytes and Git operations do not match the project's declared
encoding. Reject any committed `.gitattributes` directive that may enable
`working-tree-encoding` before destination claim. Re-encoding remains rejected
because it would violate Ward's raw proof model; a deliberately unset or
unspecified attribute remains admissible.

Confirmed and deferred: a process death can strand a random sibling staging
directory. Reclaiming it safely needs authenticated, invocation-bound ownership;
blindly removing a fixed staging path would create another recursive-delete
boundary. Follow-up: #536.

Rejected by verification: raw `cat-file` output plus the independent Ward
observer preserved exact bytes under `ident eol=crlf`, executable owner bits
matched Ward, irregular trees were refused without changing the source or
leaving a destination, and same-invocation replacement completed before the old
retained directory was removed.

Revisit irregular-mode refusal only if both host seeding and Ward's proof
contract begin admitting those node kinds. Revisit the raw producer/prover split
only if the trust model no longer depends on their implementation independence.
