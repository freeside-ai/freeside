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
// enter an item's evidence snapshot and are never auto-uploaded.
type Result struct {
	CommitSHA string              `json:"commit_sha,omitempty"`
	TreeSHA   string              `json:"tree_sha,omitempty"`
	Changes   []Change            `json:"changes"`
	Findings  []Finding           `json:"findings"`
	Claims    []domain.AgentClaim `json:"claims"`
	// CommitPlanNotice is a daemon-derived reason for consuming but not using
	// the plan channel, or for the plan_preferred unified fallback.
	CommitPlanNotice *domain.CommitPlanNoticeReason `json:"commit_plan_notice,omitempty"`
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
	em, emPresent, err := loadEvidenceManifest(handoffDir, opts.Policy)
	if err != nil {
		return Result{}, err
	}
	planRaw, planPresent, err := loadCommitPlan(handoffDir, opts.Policy)
	if err != nil {
		return Result{}, err
	}
	scratch, err := os.MkdirTemp("", "freeside-import-")
	if err != nil {
		return Result{}, fmt.Errorf("create import scratch: %w", err)
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	blobs, evidenceBlobs, err := verifyBlobs(handoffDir, scratch, m, em, emPresent, planPresent, opts.Policy)
	if err != nil {
		return Result{}, err
	}
	// Evidence is a separate §5.6 channel: valid entries become labeled agent
	// claims; any invalid evidence (bad magic/type, forged provenance) fails the
	// whole import closed, the same posture as the repo channel's integrity
	// violations. Built before commit construction so no clean commit is
	// produced for a handoff with hostile evidence.
	claims, err := buildClaims(em, evidenceBlobs, opts.Policy)
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
	if err := preflightCommitPlanBase(base); err != nil {
		return Result{}, err
	}
	changes, findings, err := deriveChanges(ctx, g, base, m, blobs)
	if err != nil {
		return Result{}, err
	}
	findings = append(findings, applyPolicy(changes, opts.Policy)...)
	findings = append(findings, detectCollisions(changes, base)...)
	secretFindings, err := scanSecrets(blobs, changes, opts.Policy.SecretMaxScanBytes)
	if err != nil {
		return Result{}, err
	}
	findings = append(findings, secretFindings...)
	if opts.Policy.CommitPlan == domain.CommitPlanPlanPreferred && planPresent {
		findings = append(findings, scanCommitPlanStrings(planRaw)...)
	}
	sortFindings(findings)
	result := Result{
		Changes:  make([]Change, 0, len(changes)),
		Findings: findings,
		Claims:   claims,
	}
	if planPresent && opts.Policy.CommitPlan == domain.CommitPlanSingleCommit {
		setPlanNotice(&result, domain.CommitPlanNoticePresentButNotHonored)
	}
	if result.Findings == nil {
		result.Findings = []Finding{}
	}
	if result.Claims == nil {
		result.Claims = []domain.AgentClaim{}
	}
	for _, c := range changes {
		result.Changes = append(result.Changes, c.public())
	}
	for _, f := range findings {
		if f.Kind.blocksCommit() {
			return result, nil
		}
	}
	if opts.Policy.CommitPlan == domain.CommitPlanPlanPreferred {
		if len(changes) == 0 {
			if planPresent {
				setPlanNotice(&result, domain.CommitPlanNoticePresentButNotHonored)
			}
			tree, commit, err := buildCommit(ctx, g, opts, changes)
			if err != nil {
				return Result{}, err
			}
			result.TreeSHA, result.CommitSHA = tree, commit
			return result, nil
		}
		if !planPresent {
			setPlanNotice(&result, domain.CommitPlanNoticeAbsent)
		} else {
			if opts.planValidationHook != nil {
				if err := opts.planValidationHook(); err != nil {
					return Result{}, fmt.Errorf("commit-plan validation: %w", err)
				}
			}
			groups, planErr := decodeAndResolveCommitPlan(planRaw, changes, base, opts.Policy)
			if planErr == nil {
				if messageFindings := screenCommitMessages(groups, opts.Policy); len(messageFindings) == 0 {
					tree, commit, err := buildCommitPlan(ctx, g, opts, groups, changes)
					if err != nil {
						return Result{}, err
					}
					result.TreeSHA, result.CommitSHA = tree, commit
					return result, nil
				}
				setPlanNotice(&result, domain.CommitPlanNoticeScreening)
			} else if errors.Is(planErr, errPlanStructural) {
				setPlanNotice(&result, domain.CommitPlanNoticeStructural)
			} else {
				return Result{}, planErr
			}
		}
	}
	tree, commit, err := buildCommit(ctx, g, opts, changes)
	if err != nil {
		return Result{}, err
	}
	result.TreeSHA, result.CommitSHA = tree, commit
	return result, nil
}

func setPlanNotice(result *Result, reason domain.CommitPlanNoticeReason) {
	result.CommitPlanNotice = &reason
}
