// Package importer is the gauntlet's hostile importer (§5.6): it
// validates the export helper's manifest and content blobs and produces
// a daemon-authored clean commit on a fresh daemon-owned checkout at
// the enforced base SHA. Every handoff byte is untrusted: the importer
// never trusts workspace .git, hooks, config, or agent-written
// manifests, and re-validates everything the manifest claims before any
// of it can influence the import.
//
// Outcomes split into two classes, deliberately:
//
//   - Integrity violations fail closed as typed errors (no Result, no
//     commit): an unreadable, oversized, or invalid manifest, a path
//     smuggling a git-metadata component, a path that is both a file
//     and a directory, and (with later units of this package) blob and
//     base violations.
//   - Policy violations accumulate as publish-blocking Findings on the
//     Result: §5.6's non-regular change class, §5.5 automation-control
//     and §5.8 reviewer-instruction paths, allowlist and size policy,
//     path collisions, and best-effort secret matches.
//
// Publish-blocking findings still produce the commit when the tree can
// faithfully represent the candidate: §5.5 routes blocked changes
// through control-plane change, which needs the imported commit to
// exist, and §12 forbids such changes reaching publication, not import;
// the publication gate consumes Result.Findings. The commit is withheld
// only when the tree cannot faithfully hold the candidate: a changed
// non-regular kind, an invalid_path entry, or a needed-but-omitted
// blob (FindingKind.blocksCommit is the dispatch).
//
// Layout, by concept:
//
//   - errors.go   fail-closed sentinels
//   - finding.go  FindingKind enum, Finding, the blocking dispatch
//   - options.go  Options and Policy with defaults
//   - handoff.go  manifest intake: capped read, strict decode,
//     re-validation at the trust boundary
//   - paths.go    structural path gates (git-component injection,
//     file/directory conflicts)
//   - blobs.go    blob store audit and content verification
//   - change.go   ChangeKind enum and the derived Change shapes
//   - gitrunner.go  hardened git plumbing (the package's only os/exec
//     use); no hooks, no user/system/workspace config, scratch index,
//     pinned identity, no working-tree materialization
//   - commit.go   scratch-index tree and commit construction with the
//     exact-tree acceptance cross-check
//   - derive.go   change derivation against the enforced base (the
//     manifest is a full snapshot; what changed is computed here, never
//     taken from workspace parentage)
//   - importer.go the Import orchestrator and Result
//   - policy.go   §5.5/§5.8 path classes, the declared-scope
//     allowlist, and size policy over the change set
//   - collision.go  case- and Unicode-normalization-fold collisions on
//     a case-insensitive checkout (the reference deployment is APFS)
//   - secrets.go  best-effort secret scan of added/modified textual
//     content (§5.4), findings by rule and line, never the bytes
//
// Lane: gauntlet. See docs/plan.md §5.4–§5.8 and the export package
// (the manifest+blob wire contract's producer).
package importer
