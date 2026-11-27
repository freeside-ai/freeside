package importer

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// Result is one import's account: the derived change set, the
// accumulated publish-blocking findings, the labeled agent claims from
// the evidence channel, and the produced commit. CommitSHA and TreeSHA
// are empty when a blocking finding withheld construction
// (FindingKind.blocksCommit); an empty Findings list with a set
// CommitSHA is a clean import. Claims carries the §5.15 rule-2 agent
// artifacts (empty when the handoff has no evidence channel); they never
// enter an item's evidence snapshot and are never auto-uploaded. A review
// guide claim is head-bound to CommitSHA when the import built a commit and
// head-independent otherwise.
type Result struct {
	CommitSHA string              `json:"commit_sha,omitempty"`
	TreeSHA   string              `json:"tree_sha,omitempty"`
	Changes   []Change            `json:"changes"`
	Findings  []Finding           `json:"findings"`
	Claims    []domain.AgentClaim `json:"claims"`
	// CommitPlanNotice is a daemon-derived reason for consuming but not using
	// the plan channel, or for the plan_preferred unified fallback.
	CommitPlanNotice *domain.CommitPlanNoticeReason `json:"commit_plan_notice,omitempty"`
	// ReviewGuideNotice records how the importer trimmed the review guide to
	// its caps, or that it left out a guide too large to read; nil when the
	// guide arrived whole or there was none. Omitted when nil, so the digest
	// of a result without one is the digest it had before the field existed.
	ReviewGuideNotice *domain.ReviewGuideNotice `json:"review_guide_notice,omitempty"`
}

// importIntake holds the verified, read-only intake outputs shared by Import
// and CheckCommitPlan before any commit or git object is constructed.
type importIntake struct {
	scratch     string
	git         *gitRunner
	base        map[string]treeEntry
	changes     []plannedChange
	findings    []Finding
	claims      []domain.AgentClaim
	guideNotice *domain.ReviewGuideNotice
	planRaw     []byte
	planPresent bool
}

// close stops the reader's git processes before removing their scratch HOME.
func (in *importIntake) close() {
	if in == nil {
		return
	}
	if in.git != nil {
		in.git.close()
	}
	if in.scratch != "" {
		_ = os.RemoveAll(in.scratch)
	}
}

// planDecision is the result of running the commit-plan structural and
// message-screening checks on an intake's plan bytes and derived change set.
type planDecision struct {
	groups          []resolvedCommitGroup
	messageFindings []commitMessageFinding
	structuralErr   error
}

// runIntake executes every read-only intake step before commit construction:
// manifest, path gate, evidence, plan load, blob verification, claims, base
// verification, base tree, change derivation, policy, collisions, secret
// scans, base-ignored addition exclusion, and commit-plan secret scanning.
// When requireHeadAtBase is true (Import), checkout HEAD must equal BaseSHA;
// otherwise (CheckCommitPlan), BaseSHA only needs to resolve to a commit.
func runIntake(
	ctx context.Context, handoffDir, checkoutDir string, opts Options, requireHeadAtBase bool,
) (*importIntake, error) {
	m, err := loadManifest(handoffDir, opts.Policy)
	if err != nil {
		return nil, err
	}
	if err := gatePaths(m); err != nil {
		return nil, err
	}
	em, emPresent, err := loadEvidenceManifest(handoffDir, opts.Policy)
	if err != nil {
		return nil, err
	}
	planRaw, planPresent, err := loadCommitPlan(handoffDir, opts.Policy)
	if err != nil {
		return nil, err
	}
	scratch, err := os.MkdirTemp("", "freeside-import-")
	if err != nil {
		return nil, fmt.Errorf("create import scratch: %w", err)
	}
	in := &importIntake{
		scratch:     scratch,
		planRaw:     planRaw,
		planPresent: planPresent,
	}
	ok := false
	defer func() {
		if !ok {
			in.close()
		}
	}()
	blobs, evidenceBlobs, err := verifyBlobs(handoffDir, scratch, m, em, emPresent, planPresent, opts.Policy)
	if err != nil {
		return nil, err
	}
	// Evidence is a separate §5.6 channel: valid entries become labeled agent
	// claims; any invalid evidence (bad magic/type, forged provenance) fails the
	// whole import closed, the same posture as the repo channel's integrity
	// violations. Built before commit construction so no clean commit is
	// produced for a handoff with hostile evidence.
	claims, evidenceFindings, guideNotice, err := buildClaims(em, evidenceBlobs, opts)
	if err != nil {
		return nil, err
	}
	in.claims = claims
	in.guideNotice = guideNotice
	g, err := newGitRunner(ctx, opts, checkoutDir, scratch)
	if err != nil {
		return nil, err
	}
	in.git = g
	if requireHeadAtBase {
		if err := g.verifyBase(ctx, opts.BaseSHA); err != nil {
			return nil, err
		}
	} else {
		if err := g.verifyBaseCommit(ctx, opts.BaseSHA); err != nil {
			return nil, err
		}
	}
	base, err := g.baseTree(ctx, opts.BaseSHA)
	if err != nil {
		return nil, err
	}
	in.base = base
	if err := preflightCommitPlanBase(base); err != nil {
		return nil, err
	}
	changes, findings, err := deriveChanges(ctx, g, base, m, blobs)
	if err != nil {
		return nil, err
	}
	findings = append(findings, applyPolicy(changes, opts.Policy)...)
	findings = append(findings, detectCollisions(changes, base)...)
	secretFindings, err := scanSecrets(blobs, changes)
	if err != nil {
		return nil, err
	}
	findings = append(findings, secretFindings...)
	findings = append(findings, evidenceFindings...)
	ignored, err := ignoredAdditions(ctx, g, base, changes, opts, scratch)
	if err != nil {
		return nil, err
	}
	// Security and size checks above still cover the complete returned change
	// set. Findings about content no commit will hold no longer apply to an
	// excluded, base-ignored addition; a secret match and the path, collision,
	// and total-size findings stay.
	kept := changes[:0]
	for _, c := range changes {
		if !ignored[c.path] {
			kept = append(kept, c)
		}
	}
	changes = kept
	keptFindings := findings[:0]
	for _, f := range findings {
		if !ignored[f.Path] || !f.Kind.inapplicableToExcludedAddition() {
			keptFindings = append(keptFindings, f)
		}
	}
	findings = keptFindings
	if opts.Policy.CommitPlan == domain.CommitPlanPlanPreferred && planPresent {
		findings = append(findings, scanCommitPlanStrings(planRaw)...)
	}
	sortFindings(findings)
	in.changes = changes
	in.findings = findings
	ok = true
	return in, nil
}

// evaluateCommitPlan runs the structural plan resolution and commit-message
// screening shared by Import and CheckCommitPlan.
func evaluateCommitPlan(planRaw []byte, changes []plannedChange, base map[string]treeEntry, pol Policy) (planDecision, error) {
	groups, planErr := decodeAndResolveCommitPlan(planRaw, changes, base, pol)
	if planErr != nil {
		if errors.Is(planErr, errPlanStructural) {
			return planDecision{structuralErr: planErr}, nil
		}
		return planDecision{}, planErr
	}
	return planDecision{
		groups:          groups,
		messageFindings: screenCommitMessages(groups, pol),
	}, nil
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
	in, err := runIntake(ctx, handoffDir, checkoutDir, opts, true)
	if err != nil {
		return Result{}, err
	}
	defer in.close()
	result := Result{
		Changes:           make([]Change, 0, len(in.changes)),
		Findings:          in.findings,
		Claims:            in.claims,
		ReviewGuideNotice: in.guideNotice,
	}
	if in.planPresent && opts.Policy.CommitPlan == domain.CommitPlanSingleCommit {
		setPlanNotice(&result, domain.CommitPlanNoticePresentButNotHonored)
	}
	if result.Findings == nil {
		result.Findings = []Finding{}
	}
	if result.Claims == nil {
		result.Claims = []domain.AgentClaim{}
	}
	for _, c := range in.changes {
		result.Changes = append(result.Changes, c.public())
	}
	for _, f := range in.findings {
		if f.Kind.blocksCommit() {
			return result, nil
		}
	}
	if opts.ExpectNoChanges {
		if len(in.changes) > 0 || in.planPresent {
			return Result{}, fmt.Errorf("%w: %d changes, commit plan present: %t",
				ErrUnexpectedChanges, len(in.changes), in.planPresent)
		}
		return result, nil
	}
	if opts.Policy.CommitPlan == domain.CommitPlanPlanPreferred {
		if len(in.changes) == 0 {
			if in.planPresent {
				setPlanNotice(&result, domain.CommitPlanNoticePresentButNotHonored)
			}
			tree, commit, err := buildCommit(ctx, in.git, opts, in.changes)
			if err != nil {
				return Result{}, err
			}
			return withCommit(result, tree, commit)
		}
		if !in.planPresent {
			setPlanNotice(&result, domain.CommitPlanNoticeAbsent)
		} else {
			if opts.planValidationHook != nil {
				if err := opts.planValidationHook(); err != nil {
					return Result{}, fmt.Errorf("commit-plan validation: %w", err)
				}
			}
			dec, err := evaluateCommitPlan(in.planRaw, in.changes, in.base, opts.Policy)
			if err != nil {
				return Result{}, err
			}
			switch {
			case dec.structuralErr != nil:
				setPlanNotice(&result, domain.CommitPlanNoticeStructural)
			case len(dec.messageFindings) > 0:
				setPlanNotice(&result, domain.CommitPlanNoticeScreening)
			default:
				tree, commit, err := buildCommitPlan(ctx, in.git, opts, dec.groups, in.changes)
				if err != nil {
					return Result{}, err
				}
				return withCommit(result, tree, commit)
			}
		}
	}
	tree, commit, err := buildCommit(ctx, in.git, opts, in.changes)
	if err != nil {
		return Result{}, err
	}
	return withCommit(result, tree, commit)
}

// withCommit records the built commit on the result and binds the claims
// that name it. Every path that builds a commit returns through here.
func withCommit(result Result, tree, commit string) (Result, error) {
	result.TreeSHA, result.CommitSHA = tree, commit
	if err := bindReviewGuideClaim(&result); err != nil {
		return Result{}, err
	}
	return result, nil
}

func setPlanNotice(result *Result, reason domain.CommitPlanNoticeReason) {
	result.CommitPlanNotice = &reason
}
