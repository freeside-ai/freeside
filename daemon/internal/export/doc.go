// Package export is the gauntlet's trusted export helper (§5.6): given a
// workspace mounted read-only, it emits digest-addressed content blobs and
// a normalized manifest onto the exporter's own root filesystem, optionally
// lifting the reserved commit plan as one opaque capped member, and never
// executes or parses workspace content. It ships inside the ward's pinned exporter
// image (the #73/#76 seam); the ward collects the output via container
// export of the stopped exporter, and the hostile importer consumes it.
//
// The manifest+blob layout is a gauntlet-internal wire contract, versioned
// in-tree (ManifestVersion). The helper records what it sees and normalizes
// only regular files; every non-regular entry (symlink, submodule pointer,
// special file, unusual mode, the workspace's own .git, non-UTF-8 names) is
// recorded faithfully for the importer's publish-blocking enforcement,
// never silently dropped. Policy enforcement (base SHA, allowlists, size
// limits, control-plane restrictions) is the importer's job, not this
// package's.
//
// Layout, by concept:
//
//   - manifest.go  schema v1 types, EntryKind enum, Validate, Encode
//   - errors.go    sentinel validation errors (wrapped with %w)
//   - walk.go      read-only workspace walk and entry classification
//   - blob.go      digest-addressed blob writing
//   - commit_plan.go opaque reserved-path lift
//   - export.go    the Export orchestrator the helper binary calls
//
// See docs/plan.md §5.6 (the gauntlet), §5.7/§5.8 (why non-regular and
// control-plane changes are publish-blocking downstream), and
// docs/spikes/workspace-handoff.md (Required backend contract check 6).
package export
