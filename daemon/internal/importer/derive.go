package importer

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/export"
)

// deriveChanges compares the manifest's full snapshot against the
// enforced base tree and returns the derived change set plus the
// findings §5.6's non-regular class produces. The manifest is the
// entire workspace, so what changed is computed here against trusted
// base state, never taken from workspace parentage: an entry identical
// to base is elided, an entry absent from base is an addition, and a
// base path absent from the manifest is a deletion.
//
// Non-regular entries (symlink, submodule, special, unusual mode) are
// publish-blocking when they represent a change against base; a
// symlink or submodule whose base slot is byte-identical is unchanged
// content the built tree retains, not a candidate change. Deletions of
// non-regular base entries are changes to non-regular content and
// flagged the same way. Subtrees the exporter could not see into
// (a directory that became a submodule) suppress derived deletions
// beneath them: the blocking finding carries the risk, and a
// mass-deletion must never fall out of blindness.
func deriveChanges(ctx context.Context, g *gitRunner, base map[string]treeEntry, m export.Manifest, blobs map[export.Digest]blobInfo) ([]plannedChange, []Finding, error) {
	var changes []plannedChange
	var findings []Finding
	consumed := make(map[string]bool, len(m.Entries))
	var opaque []string

	addFinding := func(f Finding) { findings = append(findings, f) }
	addChange := func(c plannedChange) { changes = append(changes, c) }

	for _, e := range m.Entries {
		switch e.Kind {
		case export.EntryRegular:
			c, fs, err := deriveRegular(ctx, g, base, e, blobs)
			if err != nil {
				return nil, nil, err
			}
			consumed[e.Path] = true
			if c != nil {
				addChange(*c)
			}
			for _, f := range fs {
				addFinding(f)
			}
		case export.EntrySymlink:
			c, fs, err := deriveSymlink(ctx, g, base, e)
			if err != nil {
				return nil, nil, err
			}
			consumed[e.Path] = true
			if c != nil {
				addChange(*c)
			}
			for _, f := range fs {
				addFinding(f)
			}
		case export.EntrySubmodule:
			consumed[e.Path] = true
			opaque = append(opaque, e.Path)
			if be, ok := base[e.Path]; ok && be.mode == "160000" {
				break // the base pointer is retained; workspace-side content is invisible by design
			}
			addChange(plannedChange{path: e.Path, kind: changeKindAgainst(base, e.Path)})
			addFinding(Finding{Path: e.Path, Kind: FindingNonRegularChange, Detail: "submodule"})
		case export.EntrySpecial:
			consumed[e.Path] = true
			addChange(plannedChange{path: e.Path, kind: changeKindAgainst(base, e.Path)})
			addFinding(Finding{Path: e.Path, Kind: FindingNonRegularChange, Detail: "special file"})
		case export.EntryUnusualMode:
			consumed[e.Path] = true
			addChange(plannedChange{path: e.Path, kind: changeKindAgainst(base, e.Path)})
			addFinding(Finding{Path: e.Path, Kind: FindingNonRegularChange, Detail: "unusual mode " + *e.Mode})
		case export.EntryGitDir:
			// Recorded by contract; never part of the tree and never a
			// change. §5.6: workspace .git never influences the import. A
			// tracked base path can never live under .git, so no
			// suppression is needed.
		case export.EntryInvalidPath:
			// A non-representable name the exporter recorded without
			// descending into (walk.go: file or directory alike). Its raw
			// bytes are opaque exactly like a submodule's: the exporter
			// saw nothing beneath it, so base content there must not
			// derive as a deletion. Mark the exact base path consumed and
			// suppress any base path beneath it. Validate already proved
			// the hex decodes.
			raw, err := hex.DecodeString(e.PathHex)
			if err != nil {
				return nil, nil, fmt.Errorf("invalid_path entry %q: %w", e.PathHex, err)
			}
			consumed[string(raw)] = true
			opaque = append(opaque, string(raw))
			addFinding(Finding{PathHex: e.PathHex, Kind: FindingInvalidPathEntry})
		}
	}

	opaqueSet := make(map[string]struct{}, len(opaque)+1)
	for _, p := range opaque {
		opaqueSet[p] = struct{}{}
	}
	// The reserved evidence subtree is excluded from the repo channel in both
	// directions: the export walk never emits it (walk.go), so a base path at or
	// under it is absent from the manifest and would otherwise derive as a
	// deletion, silently removing tracked base content when the agent stages
	// evidence. Retain it instead — consumed protects an exact base file/dir with
	// the reserved name, opaque protects base paths beneath the subtree.
	consumed[export.EvidenceWorkspaceDir] = true
	opaqueSet[export.EvidenceWorkspaceDir] = struct{}{}
	for path, be := range base {
		if consumed[path] || underAnyOpaque(opaqueSet, path) {
			continue
		}
		c := plannedChange{path: path, kind: ChangeDeleted}
		if !utf8.ValidString(path) {
			// The base tracks a non-representable name the candidate
			// removed. Report it losslessly by raw bytes (a lossy Path
			// would let JSON clients confuse distinct names and defeat
			// allowlist matching) and block: an unrepresentable path in
			// the change set is publish-blocking, exactly like an
			// invalid_path manifest entry.
			c.pathHex = hex.EncodeToString([]byte(path))
			addFinding(Finding{PathHex: c.pathHex, Kind: FindingInvalidPathEntry, Detail: "deletes a non-representable base path"})
		} else if be.mode != "100644" && be.mode != "100755" {
			addFinding(Finding{Path: path, Kind: FindingNonRegularChange, Detail: "deletes non-regular base entry (mode " + be.mode + ")"})
		}
		changes = append(changes, c)
	}

	sortChanges(changes)
	sortFindings(findings)
	return changes, findings, nil
}

// deriveRegular derives one regular entry: elided when base holds the
// identical blob at the identical mode, an addition or modification
// otherwise. An entry whose blob the exporter omitted is compared
// against base content by streaming digest: an unchanged oversized file
// imports cleanly, a mode-only change reuses the base object (the
// content is already present), and only genuinely changed content is
// the publish-blocking blob_omitted finding (the tree cannot hold
// content that never arrived).
func deriveRegular(ctx context.Context, g *gitRunner, base map[string]treeEntry, e export.Entry, blobs map[export.Digest]blobInfo) (*plannedChange, []Finding, error) {
	gitMode := "100644"
	if *e.Mode == "0755" {
		gitMode = "100755"
	}
	be, inBase := base[e.Path]
	baseRegular := inBase && (be.mode == "100644" || be.mode == "100755")

	if e.BlobOmitted {
		// Compare against base content regardless of mode: if the bytes
		// match, the base object already holds them and the change (if
		// any) is mode-only, which the tree can represent without the
		// withheld blob. Check the base blob's size first (cheap): the
		// manifest chooses which base blob this examines, so a hostile
		// size claim must not force streaming and hashing an arbitrarily
		// large base object. A size mismatch means changed content, no
		// hash needed.
		if baseRegular {
			baseSize, err := g.blobSize(ctx, be.oid)
			if err != nil {
				return nil, nil, err
			}
			if baseSize == *e.Size {
				digest, _, err := g.blobDigest(ctx, be.oid)
				if err != nil {
					return nil, nil, err
				}
				if digest == *e.Digest {
					if be.mode == gitMode {
						return nil, nil, nil // unchanged oversized file; nothing to import
					}
					c := plannedChange{path: e.Path, kind: ChangeModified, mode: gitMode, oid: be.oid, digest: *e.Digest, size: baseSize, fromBase: true}
					return &c, nil, nil
				}
			}
		}
		c := plannedChange{path: e.Path, kind: changeKindAgainst(base, e.Path), mode: gitMode, digest: *e.Digest, size: *e.Size}
		fs := []Finding{{Path: e.Path, Kind: FindingBlobOmitted, Detail: "content changed but its blob was withheld by export caps"}}
		if inBase && !baseRegular {
			// The omitted entry also replaces a non-regular base slot
			// (symlink, submodule): keep the §5.6 classification the
			// stored-blob branch emits, not just blob_omitted.
			fs = append(fs, Finding{Path: e.Path, Kind: FindingNonRegularChange, Detail: "replaces non-regular base entry (mode " + be.mode + ")"})
		}
		return &c, fs, nil
	}

	info := blobs[*e.Digest]
	if baseRegular && be.oid == info.gitOID {
		// be.oid == info.gitOID is only git's SHA-1 object identity, and
		// the checkout uses the sha1 object format. A candidate blob
		// crafted to collide with the base blob's SHA-1 would match here
		// while its bytes differ, so the elide/fromBase shortcut would
		// silently retain base content and skip the scan. The export
		// manifest's SHA-256 is independent collision evidence git does
		// not have: verify the base blob against it before trusting the
		// git-OID match, and fail closed on a mismatch (a SHA-1 collision
		// between differing base and candidate blobs is an attack on the
		// object format, not a legitimate import).
		matches, err := g.blobMatchesDigest(ctx, be.oid, *e.Digest, *e.Size)
		if err != nil {
			return nil, nil, err
		}
		if !matches {
			return nil, nil, fmt.Errorf("base and candidate blobs share git SHA-1 %s but differ by sha256 (%s): %w", be.oid, *e.Digest, ErrDigestMismatch)
		}
		if be.mode == gitMode {
			return nil, nil, nil // byte- and mode-identical to base
		}
		// Content identical to base, mode changed: a mode-only change
		// whose object is already in base, so nothing new is introduced.
		// Marked fromBase so the secret scan does not re-scan unchanged
		// content (a chmod on a token-bearing file is not a new secret).
		c := plannedChange{path: e.Path, kind: ChangeModified, mode: gitMode, oid: info.gitOID, digest: *e.Digest, size: info.size, fromBase: true}
		return &c, nil, nil
	}
	c := plannedChange{path: e.Path, kind: changeKindAgainst(base, e.Path), mode: gitMode, oid: info.gitOID, digest: *e.Digest, size: info.size, verifiedPath: info.verifiedPath}
	if inBase && !baseRegular {
		f := Finding{Path: e.Path, Kind: FindingNonRegularChange, Detail: "replaces non-regular base entry (mode " + be.mode + ")"}
		return &c, []Finding{f}, nil
	}
	return &c, nil, nil
}

// deriveSymlink elides a symlink whose base slot is a symlink with the
// identical target; anything else is a publish-blocking non-regular
// change.
func deriveSymlink(ctx context.Context, g *gitRunner, base map[string]treeEntry, e export.Entry) (*plannedChange, []Finding, error) {
	// JSON replaces invalid UTF-8 in the v1 best-effort target string with
	// U+FFFD. A target containing that rune is therefore ambiguous: it may
	// be literal or may stand for different raw bytes, so it is never safe
	// evidence that a non-regular entry is unchanged.
	targetLossy := strings.ContainsRune(*e.Target, utf8.RuneError)
	if be, ok := base[e.Path]; ok && be.mode == "120000" && !targetLossy {
		baseSize, err := g.blobSize(ctx, be.oid)
		if err != nil {
			return nil, nil, err
		}
		// The untrusted manifest selects this base object. A malformed base
		// can label an arbitrarily large blob as mode 120000, so decide a
		// length mismatch with cat-file -s before buffering its content.
		if baseSize == int64(len(*e.Target)) {
			target, err := g.blobContent(ctx, be.oid)
			if err != nil {
				return nil, nil, err
			}
			if bytes.Equal(target, []byte(*e.Target)) {
				return nil, nil, nil // unchanged; the built tree retains the base entry
			}
		}
	}
	c := plannedChange{path: e.Path, kind: changeKindAgainst(base, e.Path)}
	f := Finding{Path: e.Path, Kind: FindingNonRegularChange, Detail: "symlink"}
	return &c, []Finding{f}, nil
}

// changeKindAgainst reports whether a manifest entry adds or modifies
// relative to base occupancy of its path.
func changeKindAgainst(base map[string]treeEntry, path string) ChangeKind {
	if _, ok := base[path]; ok {
		return ChangeModified
	}
	return ChangeAdded
}

// underAnyOpaque reports whether path sits strictly beneath any opaque
// directory prefix. It walks path's own ancestors and looks each up in
// the set, so the cost is the path's depth, not the number of opaque
// prefixes: a hostile manifest can hold up to MaxEntries opaque
// entries, and testing every base path against every one of them
// (O(base × opaque)) is a CPU-exhaustion cross-product the path caps do
// not bound. Ancestor lookups make it O(base × depth).
func underAnyOpaque(opaque map[string]struct{}, path string) bool {
	if len(opaque) == 0 {
		return false
	}
	for i := 0; i < len(path); i++ {
		if path[i] == '/' {
			if _, ok := opaque[path[:i]]; ok {
				return true
			}
		}
	}
	return false
}

// sortChanges orders the change set bytewise by path, the manifest's
// own canonical order.
func sortChanges(changes []plannedChange) {
	sort.Slice(changes, func(i, j int) bool { return changes[i].path < changes[j].path })
}

// sortFindings orders findings deterministically for rendering and
// goldens: by Path first (an invalid_path finding has an empty Path and
// so sorts ahead of every representable path), then PathHex, kind,
// rule, and line.
func sortFindings(findings []Finding) {
	sort.Slice(findings, func(i, j int) bool {
		a, b := findings[i], findings[j]
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.PathHex != b.PathHex {
			return a.PathHex < b.PathHex
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Rule != b.Rule {
			return a.Rule < b.Rule
		}
		return a.Line < b.Line
	})
}
