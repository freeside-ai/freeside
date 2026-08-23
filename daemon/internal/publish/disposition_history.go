package publish

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

const (
	dispositionHistoryEncodingVersion = "freeside-disposition-history/v1"
	dispositionHistoryMarkerName      = "freeside:disposition-history"
	dispositionHistoryOpenMarker      = "<!-- " + dispositionHistoryMarkerName + " version=" + dispositionHistoryEncodingVersion + " -->"
	dispositionHistoryCloseMarker     = "<!-- /" + dispositionHistoryMarkerName + " -->"
)

// dispositionHistoryInput is the trusted store snapshot rendered into a
// published pull request (plan §5.15 rule 4 and §7). Reviews and dispositions
// are immutable authority records; readiness is re-derived from the current
// authorization, review, run policy, and compiled requirement registry.
type dispositionHistoryInput struct {
	runID                     domain.RunID
	headSHA                   string
	expectedInstructionDigest domain.Digest
	reviews                   []domain.ReviewRecord
	findings                  []domain.Finding
	dispositions              []domain.ReviewDispositionRecord
	readiness                 domain.ReadinessVerdict
	readinessProofs           []dispositionReadinessProof
}

type dispositionReadinessProof struct {
	requirementKey   domain.RequirementKey
	resolutionDigest domain.Digest
	proofDigest      domain.Digest
	recipeDigest     domain.Digest
}

// DispositionHistory is an immutable, canonical snapshot. Its fields stay
// private so a caller cannot mutate slices after validation and make repeated
// publication attempts render different bytes from the same value.
type DispositionHistory struct {
	sourceStore               *store.Store
	runID                     domain.RunID
	headSHA                   string
	expectedInstructionDigest domain.Digest
	reviews                   []domain.ReviewRecord
	findings                  []domain.Finding
	dispositions              []domain.ReviewDispositionRecord
	readiness                 domain.ReadinessVerdict
	readinessProofs           []dispositionReadinessProof
}

const (
	// maxRenderedDispositionClaimBytes keeps one third-party claim from
	// consuming the forge-facing section budget by itself.
	maxRenderedDispositionClaimBytes = 4 << 10
	// maxRenderedDispositionHistoryBytes leaves one quarter of GitHub's PR-body
	// limit for operator prose and the publisher identity marker. The complete
	// unbounded rendering remains digest-addressed when the section hits this
	// aggregate cap.
	maxRenderedDispositionHistoryBytes = 48 << 10
)

// LoadDispositionHistory reads one transactionally consistent snapshot from
// the durable store, then validates and canonicalizes it. The returned value
// remembers that store identity; Publisher accepts it only when its own
// decision boundary uses the same store, so a caller cannot substitute a
// detached or fabricated authoritative-looking section.
func LoadDispositionHistory(
	ctx context.Context,
	st *store.Store,
	c Candidate,
	expectedInstructionDigest domain.Digest,
) (DispositionHistory, error) {
	if st == nil {
		return DispositionHistory{}, fmt.Errorf("load disposition history: nil store")
	}
	if c.AuthorizationID == nil {
		return DispositionHistory{}, fmt.Errorf("load disposition history: candidate has no authorization")
	}
	var history DispositionHistory
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		auth, err := tx.GetCandidateAuthorization(ctx, *c.AuthorizationID)
		if err != nil {
			return err
		}
		profile, err := tx.LatestTrustProfile(ctx, c.Repo)
		if err != nil {
			return err
		}
		history, err = loadDispositionHistoryFromTx(
			ctx, tx, st, c, profile, auth, expectedInstructionDigest,
		)
		return err
	}); err != nil {
		return DispositionHistory{}, fmt.Errorf("load disposition history: %w", err)
	}
	return history, nil
}

// validateCurrentDispositionHistory re-reads every authority-bearing input
// inside the publication decision transaction. The caller-provided snapshot
// is only a proposed rendering: publication accepts it when the same store
// produces the same canonical bytes at the decision boundary.
func validateCurrentDispositionHistory(
	ctx context.Context,
	tx *store.ReadTx,
	st *store.Store,
	c Candidate,
	profile domain.AutomationTrustProfile,
	auth domain.CandidateAuthorization,
) error {
	if c.DispositionHistory == nil {
		return nil
	}
	if c.DispositionHistory.sourceStore != st {
		return fmt.Errorf("disposition history came from another store: %w", domain.ErrParentKeyMismatch)
	}
	fresh, err := loadDispositionHistoryFromTx(
		ctx, tx, st, c, profile, auth, c.DispositionHistory.expectedInstructionDigest,
	)
	if err != nil {
		return err
	}
	proposedDigest, err := c.DispositionHistory.digest()
	if err != nil {
		return err
	}
	freshDigest, err := fresh.digest()
	if err != nil {
		return err
	}
	if proposedDigest != freshDigest {
		return fmt.Errorf("disposition history changed before publication: %w", ErrPublicationConflict)
	}
	return nil
}

func validatePersistedReadinessProofs(
	ctx context.Context, tx *store.ReadTx, history DispositionHistory,
) error {
	if len(history.readinessProofs) != len(domain.ProductionRequirementDefinitions()) {
		return fmt.Errorf("incomplete readiness proof set: %w", domain.ErrParentKeyMismatch)
	}
	for _, expected := range history.readinessProofs {
		resolution, err := tx.GetRequirementResolution(ctx, expected.resolutionDigest)
		if err != nil {
			return err
		}
		if resolution.RequirementKey != expected.requirementKey {
			return domain.ErrParentKeyMismatch
		}
		if resolution.CheckClass == domain.CheckClassIndependentReview {
			tx.AuthorizeIndependentReviewRecipe(expected.recipeDigest)
		}
		proof, err := tx.GetCheckProof(ctx, expected.proofDigest)
		if err != nil {
			return err
		}
		if proof.RequirementResolutionDigest != expected.resolutionDigest ||
			proof.RecipeDigest != expected.recipeDigest {
			return domain.ErrParentKeyMismatch
		}
	}
	return nil
}

func loadDispositionHistoryFromTx(
	ctx context.Context,
	tx *store.ReadTx,
	st *store.Store,
	c Candidate,
	profile domain.AutomationTrustProfile,
	auth domain.CandidateAuthorization,
	expectedInstructionDigest domain.Digest,
) (DispositionHistory, error) {
	if c.RecipeDigest == nil {
		return DispositionHistory{}, domain.ErrEmptyField
	}
	if err := validateAuthorizationCandidate(c, auth); err != nil {
		return DispositionHistory{}, err
	}
	reviews, err := tx.ListReviewRecords(ctx, c.RunID)
	if err != nil {
		return DispositionHistory{}, err
	}
	dispositions, err := tx.ListFindingDispositions(ctx, c.RunID)
	if err != nil {
		return DispositionHistory{}, err
	}
	reviews, dispositions = currentDispositionLineage(reviews, dispositions)
	if len(reviews) == 0 {
		return DispositionHistory{}, fmt.Errorf("no current review lineage: %w", domain.ErrParentKeyMismatch)
	}
	findingIDs := make([]domain.FindingID, 0)
	seenFindingIDs := make(map[domain.FindingID]struct{})
	for _, lineageReview := range reviews {
		for _, findingID := range lineageReview.FindingIDs {
			if _, seen := seenFindingIDs[findingID]; seen {
				continue
			}
			seenFindingIDs[findingID] = struct{}{}
			findingIDs = append(findingIDs, findingID)
		}
	}
	slices.Sort(findingIDs)
	findings := make([]domain.Finding, 0, len(findingIDs))
	for _, findingID := range findingIDs {
		finding, err := tx.GetFinding(ctx, findingID)
		if err != nil {
			return DispositionHistory{}, err
		}
		findings = append(findings, finding)
	}
	review := reviews[len(reviews)-1]
	failure, failureErr := tx.LatestReviewFailure(ctx, c.RunID)
	if failureErr != nil && !errors.Is(failureErr, store.ErrNotFound) {
		return DispositionHistory{}, failureErr
	}
	if failureErr == nil && failure.Round >= review.Round {
		return DispositionHistory{}, fmt.Errorf("latest review failure supersedes clean review: %w", domain.ErrParentKeyMismatch)
	}
	if review.ConfigurationDigest != profile.Review.ConfigDigest {
		return DispositionHistory{}, fmt.Errorf(
			"review record configuration is %s, profile pins %s: %w",
			review.ConfigurationDigest, profile.Review.ConfigDigest,
			domain.ErrReviewConfigurationUnapproved,
		)
	}
	if expectedInstructionDigest == "" || review.InstructionDigest != expectedInstructionDigest ||
		review.BaseSHA != auth.BaseSHA {
		return DispositionHistory{}, fmt.Errorf("review authority disagrees with current publication decision: %w", domain.ErrParentKeyMismatch)
	}
	run, err := tx.GetRun(ctx, c.RunID)
	if err != nil {
		return DispositionHistory{}, err
	}
	generation := st.VerificationFloorRegistryGeneration()
	setDigest, err := domain.ProductionRequirementSetDigest(generation)
	if err != nil {
		return DispositionHistory{}, err
	}
	definitions := domain.ProductionRequirementDefinitions()
	resolutions := make([]domain.RequirementResolution, 0, len(definitions))
	for _, definition := range definitions {
		expected, err := domain.NewRequirementResolution(domain.RequirementResolutionInput{
			RequirementKey: definition.Key, CheckClass: definition.Class,
			Kind: definition.Kind, Applicable: definition.Applicable,
			BaseDependent: definition.BaseDependent, RequirementSetDigest: setDigest,
			FloorRegistryGeneration: generation, ResolvedPolicyDigest: run.PolicyDigest,
		})
		if err != nil {
			return DispositionHistory{}, err
		}
		resolutions = append(resolutions, expected)
	}
	base := domain.BaseRevision{
		Repo: c.Repo, RepositoryID: profile.RepositoryID,
		BaseRef: c.BaseRef, BaseSHA: auth.BaseSHA,
	}
	verificationProof, err := domain.NewCheckProof(resolutions[0], c.HeadSHA, &base, *c.RecipeDigest)
	if err != nil {
		return DispositionHistory{}, err
	}
	reviewRecipeBody, err := json.Marshal(struct {
		Configuration domain.Digest `json:"configuration"`
		Instructions  domain.Digest `json:"instructions"`
	}{review.ConfigurationDigest, review.InstructionDigest})
	if err != nil {
		return DispositionHistory{}, err
	}
	reviewRecipe := domain.Digest(contentaddr.Sum(reviewRecipeBody))
	reviewProof, err := domain.NewCheckProof(resolutions[1], c.HeadSHA, &base, reviewRecipe)
	if err != nil {
		return DispositionHistory{}, err
	}
	verificationState, err := domain.NewPassedCheckState(resolutions[0], verificationProof)
	if err != nil {
		return DispositionHistory{}, err
	}
	reviewState, err := domain.NewPassedCheckState(resolutions[1], reviewProof)
	if err != nil {
		return DispositionHistory{}, err
	}
	readinessProofs := []dispositionReadinessProof{
		{
			requirementKey: resolutions[0].RequirementKey, resolutionDigest: resolutions[0].Digest,
			proofDigest: verificationProof.Digest, recipeDigest: verificationProof.RecipeDigest,
		},
		{
			requirementKey: resolutions[1].RequirementKey, resolutionDigest: resolutions[1].Digest,
			proofDigest: reviewProof.Digest, recipeDigest: reviewProof.RecipeDigest,
		},
	}
	readiness, err := domain.EvaluateReadiness(
		domain.EvaluationTarget{CandidateHead: c.HeadSHA, Base: &base},
		resolutions, []domain.CheckState{verificationState, reviewState}, nil,
	)
	if err != nil {
		return DispositionHistory{}, err
	}
	return newDispositionHistory(dispositionHistoryInput{
		runID: c.RunID, headSHA: c.HeadSHA,
		expectedInstructionDigest: expectedInstructionDigest, reviews: reviews,
		findings: findings, dispositions: dispositions, readiness: readiness,
		readinessProofs: readinessProofs,
	}, st)
}

// currentDispositionLineage keeps the review rounds that share the latest
// pass's exact base, reviewer configuration, and instruction authority. A
// superseded pass under stale instructions or configuration is durable history
// in the store, but it is not part of the review derivation authorizing this
// publication and may correctly have no disposition. Remediation rounds remain
// included because their heads may change while these authority bindings stay
// fixed.
func currentDispositionLineage(
	reviews []domain.ReviewRecord,
	dispositions []domain.ReviewDispositionRecord,
) ([]domain.ReviewRecord, []domain.ReviewDispositionRecord) {
	if len(reviews) == 0 {
		return nil, nil
	}
	latest := reviews[len(reviews)-1]
	currentRounds := make(map[int]struct{}, len(reviews))
	currentReviews := make([]domain.ReviewRecord, 0, len(reviews))
	for _, review := range reviews {
		if review.BaseSHA != latest.BaseSHA ||
			review.ConfigurationDigest != latest.ConfigurationDigest ||
			review.InstructionDigest != latest.InstructionDigest {
			continue
		}
		currentRounds[review.Round] = struct{}{}
		currentReviews = append(currentReviews, review)
	}
	currentDispositions := make([]domain.ReviewDispositionRecord, 0, len(dispositions))
	for _, disposition := range dispositions {
		if _, ok := currentRounds[disposition.Round]; ok {
			currentDispositions = append(currentDispositions, disposition)
		}
	}
	return currentReviews, currentDispositions
}

// newDispositionHistory validates and canonicalizes one publication snapshot.
// It fails closed unless the latest review is clean and bound to the published
// head and every finding-bearing round has exactly one final disposition.
func newDispositionHistory(
	in dispositionHistoryInput, sourceStore *store.Store,
) (DispositionHistory, error) {
	reviews := slices.Clone(in.reviews)
	for i := range reviews {
		reviews[i].FindingIDs = slices.Clone(reviews[i].FindingIDs)
	}
	slices.SortFunc(reviews, func(a, b domain.ReviewRecord) int {
		if a.Round != b.Round {
			return a.Round - b.Round
		}
		return strings.Compare(string(a.InvocationID), string(b.InvocationID))
	})
	findings := slices.Clone(in.findings)
	slices.SortFunc(findings, func(a, b domain.Finding) int {
		return strings.Compare(string(a.ID), string(b.ID))
	})
	dispositions := slices.Clone(in.dispositions)
	slices.SortFunc(dispositions, func(a, b domain.ReviewDispositionRecord) int {
		if a.Round != b.Round {
			return a.Round - b.Round
		}
		return strings.Compare(string(a.FindingID), string(b.FindingID))
	})
	readiness := in.readiness
	readiness.Reasons = slices.Clone(readiness.Reasons)
	readiness.WaiverIDs = slices.Clone(readiness.WaiverIDs)
	readiness.AdvisoryOutcomes = slices.Clone(readiness.AdvisoryOutcomes)
	slices.Sort(readiness.WaiverIDs)
	slices.SortFunc(readiness.AdvisoryOutcomes, func(a, b domain.AdvisoryOutcomeRecord) int {
		if a.RequirementResolutionDigest != b.RequirementResolutionDigest {
			return strings.Compare(string(a.RequirementResolutionDigest), string(b.RequirementResolutionDigest))
		}
		return strings.Compare(string(a.Outcome), string(b.Outcome))
	})
	readinessProofs := slices.Clone(in.readinessProofs)
	slices.SortFunc(readinessProofs, func(a, b dispositionReadinessProof) int {
		return strings.Compare(string(a.requirementKey), string(b.requirementKey))
	})

	history := DispositionHistory{
		sourceStore: sourceStore, runID: in.runID, headSHA: in.headSHA,
		expectedInstructionDigest: in.expectedInstructionDigest, reviews: reviews,
		findings: findings, dispositions: dispositions, readiness: readiness,
		readinessProofs: readinessProofs,
	}
	if err := history.validate(); err != nil {
		return DispositionHistory{}, fmt.Errorf("disposition history: %w", err)
	}
	return history, nil
}

func (h DispositionHistory) validate() error {
	if h.runID == "" || h.headSHA == "" {
		return domain.ErrEmptyField
	}
	if len(h.reviews) == 0 {
		return fmt.Errorf("no completed review records: %w", domain.ErrParentKeyMismatch)
	}
	if err := h.readiness.Validate(); err != nil {
		return fmt.Errorf("readiness: %w", err)
	}
	if h.readiness.Class == domain.ReadinessBlocked {
		return fmt.Errorf("blocked readiness cannot publish: %w", domain.ErrParentKeyMismatch)
	}

	reviewsByRound := make(map[int]domain.ReviewRecord, len(h.reviews))
	reviewsByInvocation := make(map[domain.InvocationID]domain.ReviewRecord, len(h.reviews))
	expectedFindingIDs := make(map[domain.FindingID]struct{})
	for i, review := range h.reviews {
		if err := review.Validate(); err != nil {
			return fmt.Errorf("review round %d: %w", review.Round, err)
		}
		if review.RunID != h.runID {
			return fmt.Errorf("review round %d run: %w", review.Round, domain.ErrParentKeyMismatch)
		}
		if i > 0 && h.reviews[i-1].Round == review.Round {
			return fmt.Errorf("duplicate review round %d: %w", review.Round, domain.ErrParentKeyMismatch)
		}
		reviewsByRound[review.Round] = review
		reviewsByInvocation[review.InvocationID] = review
		for _, findingID := range review.FindingIDs {
			expectedFindingIDs[findingID] = struct{}{}
		}
	}
	latest := h.reviews[len(h.reviews)-1]
	if h.expectedInstructionDigest == "" || latest.InstructionDigest != h.expectedInstructionDigest ||
		latest.HeadSHA != h.headSHA || latest.Outcome != domain.ReviewClean {
		return fmt.Errorf("latest review is not clean at published head: %w", domain.ErrParentKeyMismatch)
	}

	findingsByID := make(map[domain.FindingID]domain.Finding, len(h.findings))
	for _, finding := range h.findings {
		if err := finding.Validate(); err != nil {
			return fmt.Errorf("finding %s: %w", finding.ID, err)
		}
		if finding.RunID != h.runID {
			return fmt.Errorf("finding %s run: %w", finding.ID, domain.ErrParentKeyMismatch)
		}
		if _, duplicate := findingsByID[finding.ID]; duplicate {
			return fmt.Errorf("duplicate finding %s: %w", finding.ID, domain.ErrParentKeyMismatch)
		}
		findingsByID[finding.ID] = finding
	}
	for findingID := range expectedFindingIDs {
		if _, ok := findingsByID[findingID]; !ok {
			return fmt.Errorf("review finding %s is missing: %w", findingID, domain.ErrParentKeyMismatch)
		}
	}
	for findingID := range findingsByID {
		if _, ok := expectedFindingIDs[findingID]; !ok {
			return fmt.Errorf("finding %s is outside the review lineage: %w", findingID, domain.ErrParentKeyMismatch)
		}
	}

	type dispositionKey struct {
		finding domain.FindingID
		round   int
	}
	byFindingRound := make(map[dispositionKey]domain.ReviewDispositionRecord, len(h.dispositions))
	for _, disposition := range h.dispositions {
		if err := disposition.Validate(); err != nil {
			return fmt.Errorf("finding %s round %d: %w", disposition.FindingID, disposition.Round, err)
		}
		if disposition.RunID != h.runID {
			return fmt.Errorf("finding %s run: %w", disposition.FindingID, domain.ErrParentKeyMismatch)
		}
		review, ok := reviewsByRound[disposition.Round]
		if !ok || !slices.Contains(review.FindingIDs, disposition.FindingID) {
			return fmt.Errorf("finding %s review binding: %w", disposition.FindingID, domain.ErrParentKeyMismatch)
		}
		key := dispositionKey{finding: disposition.FindingID, round: disposition.Round}
		if _, duplicate := byFindingRound[key]; duplicate {
			return fmt.Errorf("duplicate finding %s round %d: %w", disposition.FindingID, disposition.Round, domain.ErrParentKeyMismatch)
		}
		if disposition.Disposition == domain.ReviewDispositionFixed {
			remediation, ok := reviewsByInvocation[disposition.RemediationInvocationID]
			if !ok || remediation.Round <= disposition.Round || remediation.BaseSHA != review.BaseSHA || remediation.HeadSHA == review.HeadSHA {
				return fmt.Errorf("finding %s remediation review: %w", disposition.FindingID, domain.ErrParentKeyMismatch)
			}
		}
		byFindingRound[key] = disposition
	}
	for _, review := range h.reviews {
		for _, findingID := range review.FindingIDs {
			if _, ok := byFindingRound[dispositionKey{finding: findingID, round: review.Round}]; !ok {
				return fmt.Errorf("finding %s round %d has no final disposition: %w", findingID, review.Round, domain.ErrParentKeyMismatch)
			}
		}
	}
	return nil
}

func (h DispositionHistory) validateCandidate(runID domain.RunID, headSHA string) error {
	if h.runID != runID || h.headSHA != headSHA {
		return fmt.Errorf("candidate binding: %w", domain.ErrParentKeyMismatch)
	}
	return h.validate()
}

func (h DispositionHistory) digest() (domain.Digest, error) {
	rendered, err := RenderDispositionHistory(h)
	if err != nil {
		return "", err
	}
	return domain.Digest(contentaddr.Sum([]byte(rendered))), nil
}

// RenderDispositionHistory deterministically renders the publisher-owned PR
// section. Free text from reviewer or remediation records is HTML-escaped, and
// disposition reasons are explicitly labeled as recorded claims rather than
// daemon-authored facts.
func RenderDispositionHistory(h DispositionHistory) (string, error) {
	if err := h.validate(); err != nil {
		return "", fmt.Errorf("render disposition history: %w", err)
	}
	byRound := make(map[int]map[domain.FindingID]domain.ReviewDispositionRecord, len(h.reviews))
	for _, disposition := range h.dispositions {
		if byRound[disposition.Round] == nil {
			byRound[disposition.Round] = make(map[domain.FindingID]domain.ReviewDispositionRecord)
		}
		byRound[disposition.Round][disposition.FindingID] = disposition
	}
	findingsByID := make(map[domain.FindingID]domain.Finding, len(h.findings))
	for _, finding := range h.findings {
		findingsByID[finding.ID] = finding
	}

	var out strings.Builder
	fmt.Fprintf(&out, "%s\n\n## Freeside Disposition History\n\n", dispositionHistoryOpenMarker)
	fmt.Fprintf(&out, "- Published head: %s\n", dispositionCode(h.headSHA))
	fmt.Fprintf(&out, "- Readiness: **%s**\n", dispositionLabel(string(h.readiness.Class)))
	fmt.Fprintf(&out, "- Evaluation set: %s\n", dispositionCode(string(h.readiness.EvaluationSetDigest)))
	for _, proof := range h.readinessProofs {
		fmt.Fprintf(&out, "- Required check %s: resolution %s; proof %s; recipe %s\n",
			dispositionCode(string(proof.requirementKey)),
			dispositionCode(string(proof.resolutionDigest)),
			dispositionCode(string(proof.proofDigest)),
			dispositionCode(string(proof.recipeDigest)))
	}
	for _, waiverID := range h.readiness.WaiverIDs {
		fmt.Fprintf(&out, "- Applied waiver: %s\n", dispositionCode(string(waiverID)))
	}
	for _, advisory := range h.readiness.AdvisoryOutcomes {
		fmt.Fprintf(&out, "- Advisory: %s was **%s**\n",
			dispositionCode(string(advisory.RequirementResolutionDigest)), dispositionLabel(string(advisory.Outcome)))
	}

	for _, review := range h.reviews {
		fmt.Fprintf(&out, "\n### Review Round %d\n\n", review.Round)
		fmt.Fprintf(&out, "- Invocation: %s\n", dispositionCode(string(review.InvocationID)))
		fmt.Fprintf(&out, "- Outcome: **%s**\n", dispositionLabel(string(review.Outcome)))
		fmt.Fprintf(&out, "- Provider: %s\n", dispositionCode(review.Provider))
		fmt.Fprintf(&out, "- Model configuration: %s\n", dispositionCode(review.ModelConfiguration))
		fmt.Fprintf(&out, "- Configuration digest: %s\n", dispositionCode(string(review.ConfigurationDigest)))
		fmt.Fprintf(&out, "- Instruction digest: %s\n", dispositionCode(string(review.InstructionDigest)))
		fmt.Fprintf(&out, "- Cost owner: %s\n", dispositionCode(review.CostOwner))
		fmt.Fprintf(&out, "- Base/head: %s / %s\n", dispositionCode(review.BaseSHA), dispositionCode(review.HeadSHA))
		fmt.Fprintf(&out, "- Completed: %s\n", dispositionCode(review.CompletedAt.UTC().Format(time.RFC3339Nano)))
		fmt.Fprintf(&out, "- Completion evidence: %s\n", dispositionCode(string(review.CompletionEvidence)))
		if len(review.FindingIDs) == 0 {
			out.WriteString("- Findings: none\n")
			continue
		}
		out.WriteString("- Final finding dispositions:\n")
		for _, findingID := range review.FindingIDs {
			finding := findingsByID[findingID]
			disposition := byRound[review.Round][findingID]
			fmt.Fprintf(&out, "  - %s: **%s**\n", dispositionCode(string(findingID)), dispositionLabel(string(disposition.Disposition)))
			if finding.Severity != "" {
				fmt.Fprintf(&out, "    - Severity: %s\n", boundedDispositionClaim(string(finding.Severity)))
			}
			if finding.Location != nil {
				fmt.Fprintf(&out, "    - Location: %s\n", boundedDispositionClaim(finding.Location.String()))
			}
			fmt.Fprintf(&out, "    - Reviewer message (claim): %s\n", boundedDispositionClaim(finding.Message))
			fmt.Fprintf(&out, "    - Recorded rationale (claim): %s\n", boundedDispositionClaim(disposition.Reason))
			fmt.Fprintf(&out, "    - Recorded: %s\n", dispositionCode(disposition.CreatedAt.UTC().Format(time.RFC3339Nano)))
			if disposition.AdjudicationDigest != "" {
				fmt.Fprintf(&out, "    - Adjudication artifact: %s\n", dispositionCode(string(disposition.AdjudicationDigest)))
			}
			if disposition.RemediationInvocationID != "" {
				fmt.Fprintf(&out, "    - Remediation review: %s\n", dispositionCode(string(disposition.RemediationInvocationID)))
			}
		}
	}
	out.WriteString("\n" + dispositionHistoryCloseMarker)
	return boundedDispositionHistory(out.String()), nil
}

func dispositionCode(value string) string {
	replacer := strings.NewReplacer("\r", `\r`, "\n", `\n`)
	return "<code>" + html.EscapeString(replacer.Replace(value)) + "</code>"
}

// boundedDispositionClaim preserves a deterministic prefix of an untrusted
// claim and identifies the omitted bytes by their content address. The raw
// value remains durable in the finding or disposition record; this only bounds
// its forge-facing representation.
func boundedDispositionClaim(value string) string {
	rendered := dispositionCode(value)
	if len(rendered) <= maxRenderedDispositionClaimBytes {
		return rendered
	}

	digest := contentaddr.Sum([]byte(value))
	suffix := "</code> (truncated; content digest " + dispositionCode(digest) + ")"
	prefix := rendered[len("<code>") : len(rendered)-len("</code>")]
	limit := maxRenderedDispositionClaimBytes - len("<code>") - len(suffix)
	for limit > 0 && !utf8.RuneStart(prefix[limit]) {
		limit--
	}
	return "<code>" + prefix[:limit] + suffix
}

// boundedDispositionHistory preserves only complete rendered lines and binds
// every omitted byte through the digest of the complete canonical rendering.
// The publisher-owned close marker is always restored outside the bounded
// prefix, so third-party volume cannot hide or forge the section boundary.
func boundedDispositionHistory(rendered string) string {
	if len(rendered) <= maxRenderedDispositionHistoryBytes {
		return rendered
	}

	digest := contentaddr.Sum([]byte(rendered))
	suffix := "\n- Disposition history truncated; full rendered digest: " +
		dispositionCode(digest) + "\n\n" + dispositionHistoryCloseMarker
	limit := maxRenderedDispositionHistoryBytes - len(suffix)
	prefix := rendered[:limit]
	if newline := strings.LastIndexByte(prefix, '\n'); newline >= 0 {
		prefix = prefix[:newline]
	}
	return prefix + suffix
}

func dispositionLabel(value string) string {
	return html.EscapeString(strings.ReplaceAll(value, "_", " "))
}

func containsDispositionHistoryMarker(body string) bool {
	lower := strings.ToLower(body)
	return strings.Contains(lower, dispositionHistoryMarkerName)
}
