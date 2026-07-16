package importer

import (
	"context"
	"fmt"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/export"
)

// nullOID is the all-zero object name update-index uses for removals.
const nullOID = "0000000000000000000000000000000000000000"

// buildCommit constructs the daemon-authored commit: base tree into the
// scratch index, verified blobs into the object database, the derived
// changes applied, and the resulting tree written and committed onto
// the enforced base. Two cross-checks hold construction to what content
// verification proved: every ingested object name must equal the
// pure-Go derivation, and the finished tree must differ from base by
// exactly the derived change set (diff-tree, renames off). Any
// disagreement aborts rather than committing a tree that misrepresents
// the candidate.
func buildCommit(ctx context.Context, g *gitRunner, opts Options, changes []plannedChange) (treeSHA, commitSHA string, err error) {
	if err := g.readTree(ctx, opts.BaseSHA); err != nil {
		return "", "", err
	}
	var digests []export.Digest
	seen := make(map[export.Digest]struct{})
	expected := make(map[export.Digest]blobInfo)
	for _, c := range changes {
		if c.oid == "" {
			continue
		}
		if _, ok := seen[c.digest]; ok {
			continue
		}
		seen[c.digest] = struct{}{}
		digests = append(digests, c.digest)
		expected[c.digest] = blobInfo{size: c.size, gitOID: c.oid, verifiedPath: c.verifiedPath}
	}
	ingested, err := g.ingestBlobs(ctx, digests, expected)
	if err != nil {
		return "", "", err
	}
	records := make([]string, 0, len(changes))
	for _, c := range changes {
		switch c.kind {
		case ChangeAdded, ChangeModified:
			if got := ingested[c.digest]; got != c.oid {
				return "", "", fmt.Errorf("blob %s ingested as %s, derivation expected %s: %w", c.digest, got, c.oid, ErrTreeMismatch)
			}
			records = append(records, c.mode+" "+c.oid+"\t"+c.path)
		case ChangeDeleted:
			records = append(records, "0 "+nullOID+"\t"+c.path)
		}
	}
	if err := g.applyIndex(ctx, records); err != nil {
		return "", "", err
	}
	tree, err := g.writeTree(ctx)
	if err != nil {
		return "", "", err
	}
	if err := verifyTreeMatchesChanges(ctx, g, opts.BaseSHA, tree, changes); err != nil {
		return "", "", err
	}
	commit, err := g.commitTree(ctx, tree, opts.BaseSHA, opts.CommitMessage)
	if err != nil {
		return "", "", err
	}
	if opts.ImportRef != "" {
		if err := g.updateRef(ctx, opts.ImportRef, commit); err != nil {
			return "", "", err
		}
	}
	return tree, commit, nil
}

// verifyTreeMatchesChanges is the exact-tree acceptance check: the
// built tree's diff against base must hold exactly the derived change
// set, path for path, mode for mode, object for object.
func verifyTreeMatchesChanges(ctx context.Context, g *gitRunner, baseSHA, treeSHA string, changes []plannedChange) error {
	recs, err := g.diffTree(ctx, baseSHA, treeSHA)
	if err != nil {
		return err
	}
	if len(recs) != len(changes) {
		return fmt.Errorf("tree differs from base in %d paths, derivation planned %d: %w", len(recs), len(changes), ErrTreeMismatch)
	}
	planned := make(map[string]plannedChange, len(changes))
	for _, c := range changes {
		planned[c.path] = c
	}
	for _, r := range recs {
		c, ok := planned[r.path]
		if !ok {
			return fmt.Errorf("tree changes unplanned path %q: %w", r.path, ErrTreeMismatch)
		}
		if r.status != diffStatus(c.kind) {
			return fmt.Errorf("path %q has status %s, planned %s: %w", r.path, r.status, c.kind, ErrTreeMismatch)
		}
		wantMode, wantOID := c.mode, c.oid
		if c.kind == ChangeDeleted {
			wantMode, wantOID = "000000", nullOID
		}
		if r.newMode != wantMode || r.newOID != wantOID {
			return fmt.Errorf("path %q became %s %s, planned %s %s: %w", r.path, r.newMode, r.newOID, wantMode, wantOID, ErrTreeMismatch)
		}
	}
	return nil
}

// diffStatus maps a ChangeKind to the diff-tree status letter that must
// appear for it. The switch omits default so a new ChangeKind must be
// handled; the trailing return covers the invalid zero value (which
// derivation never emits).
func diffStatus(k ChangeKind) string {
	switch k {
	case ChangeAdded:
		return "A"
	case ChangeModified:
		return "M"
	case ChangeDeleted:
		return "D"
	}
	return "?"
}

// importRefValid reports whether a caller-supplied ref is fully
// qualified and free of the characters ref machinery treats specially;
// git validates further on update.
func importRefValid(ref string) bool {
	return strings.HasPrefix(ref, "refs/") && !strings.ContainsAny(ref, " ~^:?*[\\\n")
}
