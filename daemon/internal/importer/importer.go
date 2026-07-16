package importer

import (
	"context"
	"fmt"
	"os"
)

// Result is one import's account: the derived change set, the
// accumulated publish-blocking findings, and the produced commit.
// CommitSHA and TreeSHA are empty when a blocking finding withheld
// construction (FindingKind.blocksCommit); an empty Findings list with
// a set CommitSHA is a clean import.
type Result struct {
	CommitSHA string    `json:"commit_sha,omitempty"`
	TreeSHA   string    `json:"tree_sha,omitempty"`
	Changes   []Change  `json:"changes"`
	Findings  []Finding `json:"findings"`
}

// Import validates the handoff under handoffDir and imports it onto the
// daemon-owned checkout at checkoutDir, whose HEAD must be exactly
// opts.BaseSHA. Integrity violations fail closed with a typed error and
// no Result; policy violations accumulate as Result.Findings, and the
// commit is produced unless a finding's kind blocks construction (see
// the package documentation for the split and its rationale).
func Import(ctx context.Context, handoffDir, checkoutDir string, opts Options) (Result, error) {
	opts = opts.withDefaults()
	if err := opts.validate(); err != nil {
		return Result{}, err
	}
	m, err := loadManifest(handoffDir, opts.Policy)
	if err != nil {
		return Result{}, err
	}
	if err := gatePaths(m); err != nil {
		return Result{}, err
	}
	scratch, err := os.MkdirTemp("", "freeside-import-")
	if err != nil {
		return Result{}, fmt.Errorf("create import scratch: %w", err)
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	blobs, err := verifyBlobs(handoffDir, scratch, m, opts.Policy)
	if err != nil {
		return Result{}, err
	}
	g, err := newGitRunner(ctx, opts, checkoutDir, scratch)
	if err != nil {
		return Result{}, err
	}
	if err := g.verifyBase(ctx, opts.BaseSHA); err != nil {
		return Result{}, err
	}
	base, err := g.baseTree(ctx, opts.BaseSHA)
	if err != nil {
		return Result{}, err
	}
	changes, findings, err := deriveChanges(ctx, g, base, m, blobs)
	if err != nil {
		return Result{}, err
	}
	findings = append(findings, applyPolicy(changes, opts.Policy)...)
	findings = append(findings, detectCollisions(changes, base)...)
	sortFindings(findings)
	result := Result{
		Changes:  make([]Change, 0, len(changes)),
		Findings: findings,
	}
	if result.Findings == nil {
		result.Findings = []Finding{}
	}
	for _, c := range changes {
		result.Changes = append(result.Changes, c.public())
	}
	for _, f := range findings {
		if f.Kind.blocksCommit() {
			return result, nil
		}
	}
	tree, commit, err := buildCommit(ctx, g, opts, changes)
	if err != nil {
		return Result{}, err
	}
	result.TreeSHA, result.CommitSHA = tree, commit
	return result, nil
}
