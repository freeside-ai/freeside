package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

const (
	ReviewContinueWhileNewMaterialFindings = "new_material_findings"
	defaultReviewLowValueStreak            = 2
	defaultReviewHardRoundLimit            = 25
	reviewDiminishingReasonBindingPrefix   = "\nBinding: "
	reviewDiminishingFinishReasonPrefix    = "Review ended by finish_now.\nAuthority: "
)

// ReviewConvergencePolicy is the daemon-internal resolved policy consumed by
// the convergence controller. It is not a sync or persistence contract.
type ReviewConvergencePolicy struct {
	Digest                        domain.Digest
	ContinueWhile                 string
	LowValueStreakBeforeAttention int
	HardRoundLimit                int
	// DriftGrowthStreakBeforeAttention is the growth_without_blockers streak
	// length. Zero means the key is unset and the growth rule is off, so a
	// policy that predates the key resolves exactly as it did before.
	DriftGrowthStreakBeforeAttention int
	// DriftAuditAfter is the first round that runs a drift audit. Zero means
	// the key is unset and the audit is off.
	DriftAuditAfter int
	// DriftAuditRoute is how an over_hardened verdict routes. It resolves to
	// the recorded default when the key is unset, and means nothing while the
	// audit is off.
	DriftAuditRoute domain.DriftAuditRoute
}

// ReviewConvergenceState is the trusted decision-time input to the pure
// diminishing-yield evaluator.
type ReviewConvergenceState struct {
	History          domain.ReviewYieldHistory
	Records          []domain.ReviewRecord
	Dispositions     []domain.ReviewDispositionRecord
	Findings         map[domain.FindingID]domain.Finding
	MaterialFindings map[int]map[domain.FindingID]struct{}
	Policy           ReviewConvergencePolicy
	Decisions        []ReviewDiminishingDecision
}

func (tx *ReadTx) ReviewConvergencePolicy(
	ctx context.Context, runID domain.RunID,
) (ReviewConvergencePolicy, error) {
	resolved, err := tx.GetResolvedPolicy(ctx, runID)
	if err != nil {
		return ReviewConvergencePolicy{}, err
	}
	policy := ReviewConvergencePolicy{
		Digest: resolved.Digest, ContinueWhile: ReviewContinueWhileNewMaterialFindings,
		LowValueStreakBeforeAttention: defaultReviewLowValueStreak,
		HardRoundLimit:                defaultReviewHardRoundLimit,
		DriftAuditRoute:               domain.DefaultDriftAuditRoute,
	}
	for _, key := range resolved.Keys {
		switch key.Key {
		case "review.continue_while":
			if key.Value != ReviewContinueWhileNewMaterialFindings {
				return ReviewConvergencePolicy{}, fmt.Errorf(
					"resolved review.continue_while %q: %w", key.Value, domain.ErrParentKeyMismatch)
			}
			policy.ContinueWhile = key.Value
		case "review.low_value_streak_before_attention":
			streak, err := strconv.Atoi(key.Value)
			if err != nil || streak < 1 {
				return ReviewConvergencePolicy{}, fmt.Errorf(
					"resolved review.low_value_streak_before_attention %q: %w",
					key.Value, domain.ErrNonPositive)
			}
			policy.LowValueStreakBeforeAttention = streak
		case "review.hard_round_limit":
			limit, err := strconv.Atoi(key.Value)
			if err != nil || limit < 1 {
				return ReviewConvergencePolicy{}, fmt.Errorf(
					"resolved review.hard_round_limit %q: %w", key.Value, domain.ErrNonPositive)
			}
			policy.HardRoundLimit = limit
		case "review.drift_growth_streak_before_attention":
			streak, err := strconv.Atoi(key.Value)
			if err != nil || streak < 1 {
				return ReviewConvergencePolicy{}, fmt.Errorf(
					"resolved review.drift_growth_streak_before_attention %q: %w",
					key.Value, domain.ErrNonPositive)
			}
			policy.DriftGrowthStreakBeforeAttention = streak
		case "review.drift_audit_after":
			round, err := strconv.Atoi(key.Value)
			if err != nil || round < 1 {
				return ReviewConvergencePolicy{}, fmt.Errorf(
					"resolved review.drift_audit_after %q: %w", key.Value, domain.ErrNonPositive)
			}
			policy.DriftAuditAfter = round
		case "review.drift_audit_route":
			route, err := domain.ParseDriftAuditRoute(key.Value)
			if err != nil {
				return ReviewConvergencePolicy{}, fmt.Errorf(
					"resolved review.drift_audit_route: %w: %w", err, domain.ErrParentKeyMismatch)
			}
			policy.DriftAuditRoute = route
		}
	}
	return policy, nil
}

// ReviewDiminishingBinding is the canonical daemon-authored payload embedded
// in an item's immutable Reason. The command binds the item version; this
// payload lets reconstruction re-prove the exact policy and adjudication the
// rendered decision described without adding a shared contract field.
type ReviewDiminishingBinding struct {
	ItemID                        domain.ItemID                 `json:"item_id"`
	RunID                         domain.RunID                  `json:"run_id"`
	Round                         int                           `json:"round"`
	HeadSHA                       string                        `json:"head_sha"`
	FindingIDs                    []domain.FindingID            `json:"finding_ids"`
	AdjudicationDigest            domain.Digest                 `json:"adjudication_digest"`
	FindingBatchDigest            domain.Digest                 `json:"finding_batch_digest"`
	PolicyDigest                  domain.Digest                 `json:"policy_digest"`
	ContinueWhile                 string                        `json:"continue_while"`
	LowValueStreakBeforeAttention int                           `json:"low_value_streak_before_attention"`
	HardRoundLimit                int                           `json:"hard_round_limit"`
	Cause                         domain.ReviewDiminishingCause `json:"cause"`
}

func (b ReviewDiminishingBinding) validate() error {
	if b.ItemID == "" || b.RunID == "" || b.HeadSHA == "" || len(b.FindingIDs) == 0 ||
		b.AdjudicationDigest == "" || b.FindingBatchDigest == "" || b.PolicyDigest == "" {
		return domain.ErrEmptyField
	}
	if !slices.IsSorted(b.FindingIDs) {
		return domain.ErrFindingsNotCanonical
	}
	for index, findingID := range b.FindingIDs {
		if findingID == "" || (index > 0 && findingID == b.FindingIDs[index-1]) {
			return domain.ErrFindingsNotCanonical
		}
	}
	if b.Round < 1 || b.LowValueStreakBeforeAttention < 1 || b.HardRoundLimit < 1 {
		return domain.ErrNonPositive
	}
	if b.ContinueWhile != ReviewContinueWhileNewMaterialFindings {
		return domain.ErrParentKeyMismatch
	}
	if _, err := domain.ParseReviewDiminishingCause(string(b.Cause)); err != nil {
		return domain.ErrParentKeyMismatch
	}
	return nil
}

func ReviewDiminishingReason(binding ReviewDiminishingBinding) (string, error) {
	if err := binding.validate(); err != nil {
		return "", err
	}
	var summary string
	switch binding.Cause {
	case domain.ReviewDiminishingLowValue:
		summary = "Review yield has remained low under the resolved policy."
	case domain.ReviewDiminishingFixedRecurrence:
		summary = "A finding recurred after a fixed disposition."
	case domain.ReviewDiminishingFinalFindings:
		summary = "The one final candidate-bound review found material issues."
	case domain.ReviewDiminishingGrowthWithoutBlockers:
		summary = "The change kept growing while review found no critical or high issue."
	case domain.ReviewDiminishingDriftAudit:
		summary = "A drift audit judged the change against the approved specification."
	}
	body, err := json.Marshal(binding)
	if err != nil {
		return "", err
	}
	return summary + reviewDiminishingReasonBindingPrefix + string(body), nil
}

// ReviewDiminishingItemID returns the sole command-bearing item identity for a
// run and review round.
func ReviewDiminishingItemID(runID domain.RunID, round int) domain.ItemID {
	return domain.ItemID(fmt.Sprintf("production-review-diminishing-%s-%d", runID, round))
}

// ReviewDiminishingRequestedActions returns only decisions whose promised
// review work fits within the resolved hard-round limit.
func ReviewDiminishingRequestedActions(round, hardRoundLimit int) []domain.Action {
	actions := []domain.Action{domain.ActionFinishNow}
	if round < hardRoundLimit {
		actions = append(actions, domain.ActionApplyThenFinish, domain.ActionContinueUnderPolicy)
	}
	return actions
}

func reviewDiminishingBinding(reason string) (ReviewDiminishingBinding, error) {
	_, body, ok := strings.Cut(reason, reviewDiminishingReasonBindingPrefix)
	if !ok || body == "" {
		return ReviewDiminishingBinding{}, domain.ErrParentKeyMismatch
	}
	var binding ReviewDiminishingBinding
	if err := strictjson.Decode(
		[]byte(body), &binding, strictjson.RejectInvalidUTF8, domain.MaxFindingAdjudicationBytes,
	); err != nil {
		return ReviewDiminishingBinding{}, err
	}
	canonical, err := ReviewDiminishingReason(binding)
	if err != nil || canonical != reason {
		return ReviewDiminishingBinding{}, domain.ErrParentKeyMismatch
	}
	return binding, nil
}

// ErrReviewDiminishingSimplificationUnproven refuses card facts that say
// continue_under_policy will run the audit's simplification round. That claim
// holds only when the reversal list passed the route gate (plan §7 Review
// Drift), and the gate arrives with #1051; until then nothing can re-prove it.
var ErrReviewDiminishingSimplificationUnproven = errors.New(
	"review diminishing facts promise a simplification round no route gate has proven")

// gateReviewDiminishingItem re-proves an item's review-diminishing card facts
// against the records they copy, so a client never trusts the copy: the cause
// must be the one the item's own Reason binds, that binding must name this
// item's run, round, and head, and drift facts must be the stored DriftAudit
// they name, for the bound run and round. It reads only the
// item's Reason and the audit, never another item, and both are immutable, so
// every reconstruction tier runs it.
//
// An item without the facts passes: items stored before the field existed, and
// review-escalation items whose Reason carries no binding, have none.
func (tx *ReadTx) gateReviewDiminishingItem(ctx context.Context, item domain.AttentionItem) error {
	facts := item.ReviewDiminishing
	if facts == nil {
		return nil
	}
	binding, err := reviewDiminishingBinding(item.Reason)
	if err != nil {
		return fmt.Errorf("reason binding: %w", err)
	}
	// The binding must be this item's own: the item the bound round mints, on
	// the bound run and head. Otherwise a card could show another run's cause
	// or audit while every comparison below still held.
	ownSubject := item.Subject.Type == domain.SubjectRun &&
		item.Subject.ID == domain.SubjectID(binding.RunID) && item.Subject.RunID != nil &&
		*item.Subject.RunID == binding.RunID
	if item.Type != domain.AttentionReviewDiminishing || binding.ItemID != item.ID ||
		binding.ItemID != ReviewDiminishingItemID(binding.RunID, binding.Round) ||
		!ownSubject || item.PRHeadSHA != binding.HeadSHA {
		return domain.ErrParentKeyMismatch
	}
	if facts.Cause != binding.Cause {
		return fmt.Errorf("card cause %q, reason binds %q: %w",
			facts.Cause, binding.Cause, domain.ErrParentKeyMismatch)
	}
	drift := facts.DriftAudit
	if drift == nil {
		return nil
	}
	audit, err := tx.GetDriftAudit(ctx, drift.AuditDigest)
	if err != nil {
		// A missing audit is a broken binding, not a missing item: a caller
		// reads ErrNotFound from an item load as "no such item".
		if errors.Is(err, ErrNotFound) {
			return fmt.Errorf("drift audit: %s: %w", err.Error(), domain.ErrParentKeyMismatch)
		}
		return fmt.Errorf("drift audit: %w", err)
	}
	if audit.RunID != binding.RunID || audit.Round != binding.Round ||
		audit.Verdict != drift.Verdict || audit.Confidence != drift.Confidence ||
		audit.Explanation != drift.Explanation ||
		!slices.Equal(audit.Reversals, drift.Reversals) {
		return fmt.Errorf("drift audit %q: %w", drift.AuditDigest, domain.ErrParentKeyMismatch)
	}
	if drift.SimplificationOnContinue {
		return ErrReviewDiminishingSimplificationUnproven
	}
	return nil
}

type ReviewDiminishingDecision struct {
	Item    domain.AttentionItem
	Command *domain.Command
	Binding ReviewDiminishingBinding
}

func (tx *ReadTx) ReviewDiminishingDecision(
	ctx context.Context, itemID domain.ItemID,
) (ReviewDiminishingDecision, error) {
	return tx.reviewDiminishingDecision(
		ctx, itemID, map[domain.ItemID]ReviewDiminishingDecision{}, map[domain.ItemID]struct{}{})
}

func (tx *ReadTx) reviewDiminishingDecision(
	ctx context.Context,
	itemID domain.ItemID,
	cache map[domain.ItemID]ReviewDiminishingDecision,
	visiting map[domain.ItemID]struct{},
) (ReviewDiminishingDecision, error) {
	if decision, ok := cache[itemID]; ok {
		return decision, nil
	}
	if _, cycle := visiting[itemID]; cycle {
		return ReviewDiminishingDecision{}, domain.ErrParentKeyMismatch
	}
	visiting[itemID] = struct{}{}
	decision, err := tx.reviewDiminishingDecisionUncached(ctx, itemID, cache, visiting)
	delete(visiting, itemID)
	if err == nil {
		cache[itemID] = decision
	}
	return decision, err
}

func (tx *ReadTx) reviewDiminishingDecisionUncached(
	ctx context.Context,
	itemID domain.ItemID,
	cache map[domain.ItemID]ReviewDiminishingDecision,
	visiting map[domain.ItemID]struct{},
) (ReviewDiminishingDecision, error) {
	item, err := tx.GetAttentionItem(ctx, itemID)
	if err != nil {
		return ReviewDiminishingDecision{}, err
	}
	binding, err := reviewDiminishingBinding(item.Reason)
	if err != nil {
		return ReviewDiminishingDecision{}, err
	}
	expectedActions := ReviewDiminishingRequestedActions(binding.Round, binding.HardRoundLimit)
	validSubject := item.Subject.Type == domain.SubjectRun &&
		item.Subject.ID == domain.SubjectID(binding.RunID) && item.Subject.RunID != nil &&
		*item.Subject.RunID == binding.RunID
	if item.Type != domain.AttentionReviewDiminishing || item.ID != binding.ItemID ||
		binding.ItemID != ReviewDiminishingItemID(binding.RunID, binding.Round) ||
		!validSubject || item.PRHeadSHA != binding.HeadSHA || item.YieldHistory == nil ||
		!slices.Equal(item.RequestedDecision, expectedActions) || len(item.EvidenceSnapshot) != 0 ||
		len(item.AgentClaims) > 1 {
		return ReviewDiminishingDecision{}, domain.ErrParentKeyMismatch
	}
	if err := tx.authenticateReviewDiminishingSummary(ctx, item, binding); err != nil {
		return ReviewDiminishingDecision{}, err
	}
	run, err := tx.GetRun(ctx, binding.RunID)
	if err != nil {
		return ReviewDiminishingDecision{}, err
	}
	if item.ProjectID != run.ProjectID {
		return ReviewDiminishingDecision{}, domain.ErrParentKeyMismatch
	}
	record, err := tx.reviewRecordForRound(ctx, binding.RunID, binding.Round)
	if err != nil {
		return ReviewDiminishingDecision{}, err
	}
	if record.HeadSHA != binding.HeadSHA || record.Outcome != domain.ReviewFindings ||
		!slices.Equal(record.FindingIDs, binding.FindingIDs) {
		return ReviewDiminishingDecision{}, domain.ErrParentKeyMismatch
	}
	adjudication, err := tx.GetFindingAdjudicationForRound(ctx, binding.RunID, binding.Round)
	if err != nil {
		return ReviewDiminishingDecision{}, err
	}
	if adjudication.Digest != binding.AdjudicationDigest ||
		adjudication.FindingBatchDigest != binding.FindingBatchDigest {
		return ReviewDiminishingDecision{}, domain.ErrParentKeyMismatch
	}
	policy, err := tx.ReviewConvergencePolicy(ctx, binding.RunID)
	if err != nil {
		return ReviewDiminishingDecision{}, err
	}
	if policy.Digest != binding.PolicyDigest || policy.ContinueWhile != binding.ContinueWhile ||
		policy.LowValueStreakBeforeAttention != binding.LowValueStreakBeforeAttention ||
		policy.HardRoundLimit != binding.HardRoundLimit {
		return ReviewDiminishingDecision{}, domain.ErrParentKeyMismatch
	}
	history, err := tx.ReviewYieldHistoryAtDecision(ctx, binding.RunID, binding.Round)
	if err != nil {
		return ReviewDiminishingDecision{}, err
	}
	if !reflect.DeepEqual(*item.YieldHistory, history) {
		return ReviewDiminishingDecision{}, domain.ErrParentKeyMismatch
	}
	convergence, err := tx.reviewConvergenceStateAtDecision(
		ctx, record, cache, visiting)
	if err != nil {
		return ReviewDiminishingDecision{}, err
	}
	cause, stop, err := EvaluateReviewConvergence(convergence, record)
	if err != nil {
		return ReviewDiminishingDecision{}, err
	}
	if !stop || cause != binding.Cause {
		return ReviewDiminishingDecision{}, domain.ErrParentKeyMismatch
	}
	commands, err := tx.ListCommandsForItem(ctx, item.ID)
	if err != nil {
		return ReviewDiminishingDecision{}, err
	}
	var terminal *domain.Command
	for index := range commands {
		command := &commands[index]
		switch command.Action {
		case domain.ActionFinishNow,
			domain.ActionApplyThenFinish,
			domain.ActionContinueUnderPolicy:
			if terminal != nil {
				return ReviewDiminishingDecision{}, domain.ErrParentKeyMismatch
			}
			terminal = command
		default:
			return ReviewDiminishingDecision{}, domain.ErrParentKeyMismatch
		}
	}
	if item.Status == domain.StatusOpen {
		if terminal != nil || item.DecidedAt != nil {
			return ReviewDiminishingDecision{}, domain.ErrParentKeyMismatch
		}
		return ReviewDiminishingDecision{Item: item, Binding: binding}, nil
	}
	if item.Status != domain.StatusResolved || item.DecidedAt == nil || terminal == nil ||
		terminal.ItemVersion+1 != item.ItemVersion || terminal.ItemID != item.ID ||
		terminal.PRHeadSHA != item.PRHeadSHA ||
		!item.Offers(terminal.Action) ||
		!slices.Equal(terminal.ArtifactDigests, item.ArtifactDigests) ||
		terminal.Message != "" || len(terminal.Attachments) != 0 {
		return ReviewDiminishingDecision{}, domain.ErrParentKeyMismatch
	}
	command := *terminal
	return ReviewDiminishingDecision{Item: item, Command: &command, Binding: binding}, nil
}

// authenticateReviewDiminishingSummary accepts the legacy claim-free shape or
// the one summary selected from the preceding remediation invocation's
// immutable claim set. Re-reading that set prevents a coherently rewritten
// item from inventing summary provenance at reconstruction time.
func (tx *ReadTx) authenticateReviewDiminishingSummary(
	ctx context.Context, item domain.AttentionItem, binding ReviewDiminishingBinding,
) error {
	if len(item.AgentClaims) == 0 {
		if len(item.ArtifactDigests) != 0 {
			return domain.ErrParentKeyMismatch
		}
		return nil
	}
	if binding.Round < 2 || len(item.ArtifactDigests) != 1 {
		return domain.ErrParentKeyMismatch
	}

	invocationID := domain.InvocationID(fmt.Sprintf(
		"inv-remediate-%d-%s", binding.Round-1, binding.RunID))
	claims, err := tx.GetAgentClaims(ctx, invocationID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return domain.ErrParentKeyMismatch
		}
		return err
	}
	matched := -1
	for index := range claims {
		if claims[index].Label != summaryEvidenceLabel {
			continue
		}
		if matched >= 0 {
			return domain.ErrParentKeyMismatch
		}
		matched = index
	}
	if matched < 0 {
		return domain.ErrParentKeyMismatch
	}
	summary := claims[matched]
	if summary.Text == nil || summary.Text.MediaType != domain.MediaTypeTextMarkdown ||
		summary.Provenance.ProducerInvocationID != invocationID ||
		!reflect.DeepEqual(item.AgentClaims[0], summary) ||
		item.ArtifactDigests[0] != summary.Digest {
		return domain.ErrParentKeyMismatch
	}
	return nil
}

// ListReviewDiminishingDecisions returns the authenticated decision history in
// review-round order. Open items are included with a nil command.
func (tx *ReadTx) ListReviewDiminishingDecisions(
	ctx context.Context, runID domain.RunID,
) ([]ReviewDiminishingDecision, error) {
	records, err := tx.ListReviewRecords(ctx, runID)
	if err != nil {
		return nil, err
	}
	decisions := make([]ReviewDiminishingDecision, 0, len(records))
	cache := map[domain.ItemID]ReviewDiminishingDecision{}
	visiting := map[domain.ItemID]struct{}{}
	for _, record := range records {
		itemID := ReviewDiminishingItemID(runID, record.Round)
		decision, err := tx.reviewDiminishingDecision(ctx, itemID, cache, visiting)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		decisions = append(decisions, decision)
	}
	slices.SortFunc(decisions, func(left, right ReviewDiminishingDecision) int {
		return left.Binding.Round - right.Binding.Round
	})
	return decisions, nil
}

func (tx *ReadTx) reviewDiminishingDecisionsBefore(
	ctx context.Context,
	runID domain.RunID,
	round int,
	cache map[domain.ItemID]ReviewDiminishingDecision,
	visiting map[domain.ItemID]struct{},
) ([]ReviewDiminishingDecision, error) {
	decisions := make([]ReviewDiminishingDecision, 0, round-1)
	for priorRound := 1; priorRound < round; priorRound++ {
		itemID := ReviewDiminishingItemID(runID, priorRound)
		decision, err := tx.reviewDiminishingDecision(ctx, itemID, cache, visiting)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		decisions = append(decisions, decision)
	}
	return decisions, nil
}

// ReviewConvergenceStateAtDecision reconstructs the trusted convergence input
// through current while excluding dispositions written by current's later
// action.
func (tx *ReadTx) ReviewConvergenceStateAtDecision(
	ctx context.Context, current domain.ReviewRecord,
) (ReviewConvergenceState, error) {
	return tx.reviewConvergenceStateAtDecision(
		ctx, current, map[domain.ItemID]ReviewDiminishingDecision{}, map[domain.ItemID]struct{}{})
}

func (tx *ReadTx) reviewConvergenceStateAtDecision(
	ctx context.Context,
	current domain.ReviewRecord,
	cache map[domain.ItemID]ReviewDiminishingDecision,
	visiting map[domain.ItemID]struct{},
) (ReviewConvergenceState, error) {
	state := ReviewConvergenceState{
		Findings:         map[domain.FindingID]domain.Finding{},
		MaterialFindings: map[int]map[domain.FindingID]struct{}{},
	}
	var err error
	state.History, err = tx.ReviewYieldHistoryAtDecision(ctx, current.RunID, current.Round)
	if err != nil {
		return ReviewConvergenceState{}, err
	}
	state.Records, err = tx.ListReviewRecords(ctx, current.RunID)
	if err != nil {
		return ReviewConvergenceState{}, err
	}
	state.Dispositions, err = tx.loadFindingDispositionsAtDecision(
		ctx, current.RunID, current.Round)
	if err != nil {
		return ReviewConvergenceState{}, err
	}
	selectedDispositions := state.Dispositions[:0:0]
	for _, disposition := range state.Dispositions {
		if disposition.RunID == current.RunID && disposition.Round < current.Round {
			selectedDispositions = append(selectedDispositions, disposition)
		}
	}
	state.Dispositions = selectedDispositions
	state.Policy, err = tx.ReviewConvergencePolicy(ctx, current.RunID)
	if err != nil {
		return ReviewConvergenceState{}, err
	}
	state.Decisions, err = tx.reviewDiminishingDecisionsBefore(
		ctx, current.RunID, current.Round, cache, visiting)
	if err != nil {
		return ReviewConvergenceState{}, err
	}
	for _, review := range state.Records {
		if review.Round > current.Round {
			continue
		}
		for _, findingID := range review.FindingIDs {
			if _, ok := state.Findings[findingID]; ok {
				continue
			}
			finding, err := tx.GetFinding(ctx, findingID)
			if err != nil {
				return ReviewConvergenceState{}, err
			}
			state.Findings[findingID] = finding
		}
		if review.Outcome != domain.ReviewFindings {
			continue
		}
		adjudication, err := tx.GetFindingAdjudicationForRound(ctx, review.RunID, review.Round)
		if errors.Is(err, ErrNotFound) && review.Round < current.Round {
			continue
		}
		if err != nil {
			return ReviewConvergenceState{}, err
		}
		material := map[domain.FindingID]struct{}{}
		for _, entry := range adjudication.Entries {
			// An alternative-route command can choose only decline or dispute,
			// so it cannot manufacture or remove a remediation route.
			if entry.Route == domain.RouteRemediate {
				material[entry.FindingID] = struct{}{}
			}
		}
		state.MaterialFindings[review.Round] = material
	}
	return state, nil
}

// EvaluateReviewConvergence applies the resolved diminishing-yield policy to a
// trusted decision-time state. Growth without blockers is checked last, so a
// round one of the earlier causes also stops keeps that cause, and every
// decision stored under one re-evaluates to the same cause.
func EvaluateReviewConvergence(
	state ReviewConvergenceState, current domain.ReviewRecord,
) (domain.ReviewDiminishingCause, bool, error) {
	currentIndex := -1
	for index, record := range state.Records {
		if record.Round == current.Round {
			currentIndex = index
			break
		}
	}
	if currentIndex < 0 || currentIndex >= len(state.History.Rounds) ||
		!reflect.DeepEqual(state.Records[currentIndex], current) ||
		state.History.Rounds[currentIndex].Round != current.Round {
		return "", false, domain.ErrReviewYieldHistoryInconsistent
	}
	if current.Round > state.Policy.HardRoundLimit {
		return "", false, nil
	}
	if current.Outcome != domain.ReviewFindings {
		return "", false, nil
	}
	if currentIndex > 0 {
		priorRound := state.Records[currentIndex-1].Round
		for _, decision := range state.Decisions {
			if decision.Binding.Round == priorRound && decision.Command != nil &&
				decision.Command.Action == domain.ActionApplyThenFinish {
				return domain.ReviewDiminishingFinalFindings, true, nil
			}
		}
	}

	segmentStart := currentIndex
	for segmentStart > 0 &&
		state.Records[segmentStart-1].ConfigurationDigest == current.ConfigurationDigest {
		segmentStart--
	}
	continuedAfter := state.Records[segmentStart].Round - 1
	for _, decision := range state.Decisions {
		if decision.Binding.Round < state.Records[segmentStart].Round ||
			decision.Binding.Round >= current.Round || decision.Command == nil ||
			decision.Command.Action != domain.ActionContinueUnderPolicy {
			continue
		}
		if decision.Binding.Round > continuedAfter {
			continuedAfter = decision.Binding.Round
		}
	}
	newMaterial := make(map[int]bool, currentIndex-segmentStart+1)
	dispositions := make(map[domain.FindingID]domain.ReviewDisposition, len(state.Dispositions))
	for _, disposition := range state.Dispositions {
		dispositions[disposition.FindingID] = disposition.Disposition
	}
	seen := map[domain.FindingFingerprint]struct{}{}
	for index := segmentStart; index <= currentIndex; index++ {
		record := state.Records[index]
		material := state.MaterialFindings[record.Round]
		newCount := 0
		currentFingerprints := make([]domain.FindingFingerprint, 0, len(record.FindingIDs))
		for _, findingID := range record.FindingIDs {
			finding, ok := state.Findings[findingID]
			if !ok {
				return "", false, domain.ErrReviewYieldHistoryInconsistent
			}
			fingerprint, err := finding.Fingerprint()
			isNew := false
			switch {
			case errors.Is(err, domain.ErrUnfingerprintableFinding):
				isNew = true
			case err != nil:
				return "", false, err
			default:
				currentFingerprints = append(currentFingerprints, fingerprint)
				_, recurring := seen[fingerprint]
				isNew = !recurring
			}
			if isNew {
				newCount++
				_, routedMaterial := material[findingID]
				disposition, dispositioned := dispositions[findingID]
				// Historical rounds without reconstructable adjudication or
				// disposition evidence conservatively keep review running.
				if routedMaterial ||
					(material == nil && (!dispositioned || disposition == domain.ReviewDispositionFixed)) {
					newMaterial[record.Round] = true
				}
			}
		}
		for _, fingerprint := range currentFingerprints {
			seen[fingerprint] = struct{}{}
		}
		if newCount != state.History.Rounds[index].NewFindings {
			return "", false, domain.ErrReviewYieldHistoryInconsistent
		}
	}

	fixed := map[domain.FindingFingerprint]struct{}{}
	for _, disposition := range state.Dispositions {
		if disposition.Round < state.Records[segmentStart].Round ||
			disposition.Round >= current.Round ||
			disposition.Disposition != domain.ReviewDispositionFixed {
			continue
		}
		finding, ok := state.Findings[disposition.FindingID]
		if !ok {
			return "", false, domain.ErrReviewYieldHistoryInconsistent
		}
		fingerprint, err := finding.Fingerprint()
		if errors.Is(err, domain.ErrUnfingerprintableFinding) {
			continue
		}
		if err != nil {
			return "", false, err
		}
		fixed[fingerprint] = struct{}{}
	}
	for _, findingID := range current.FindingIDs {
		finding, ok := state.Findings[findingID]
		if !ok {
			return "", false, domain.ErrReviewYieldHistoryInconsistent
		}
		fingerprint, err := finding.Fingerprint()
		if errors.Is(err, domain.ErrUnfingerprintableFinding) {
			continue
		}
		if err != nil {
			return "", false, err
		}
		if _, recurring := fixed[fingerprint]; recurring {
			return domain.ReviewDiminishingFixedRecurrence, true, nil
		}
	}

	streak := 0
	for index := currentIndex; index >= segmentStart; index-- {
		round := state.History.Rounds[index]
		if round.Round <= continuedAfter || round.FindingsIngested == 0 || newMaterial[round.Round] {
			break
		}
		streak++
	}
	if streak >= state.Policy.LowValueStreakBeforeAttention {
		return domain.ReviewDiminishingLowValue, true, nil
	}
	if state.Policy.DriftGrowthStreakBeforeAttention > 0 {
		growth := 0
		for index := currentIndex; index >= segmentStart; index-- {
			if state.History.Rounds[index].Round <= continuedAfter {
				break
			}
			grew, err := reviewRoundGrewWithoutBlockers(state, index)
			if err != nil {
				return "", false, err
			}
			if !grew {
				break
			}
			growth++
		}
		if growth >= state.Policy.DriftGrowthStreakBeforeAttention {
			return domain.ReviewDiminishingGrowthWithoutBlockers, true, nil
		}
	}
	return "", false, nil
}

// reviewRoundGrewWithoutBlockers reports whether the recorded round at index
// is a growth round (plan §7 Growth without blockers): it has findings, none
// critical or high, and its cumulative net size is strictly greater than the
// previous recorded round's. The run's first recorded round has no predecessor
// and never counts. A round is unverifiable, and so not a growth round, when
// either round has no metrics or the two cumulative pairs start from different
// bases; their nets then measure different comparisons.
func reviewRoundGrewWithoutBlockers(state ReviewConvergenceState, index int) (bool, error) {
	if index == 0 {
		return false, nil
	}
	round, previous := state.History.Rounds[index], state.History.Rounds[index-1]
	if round.FindingsIngested == 0 || round.DiffMetrics == nil || previous.DiffMetrics == nil ||
		round.DiffMetrics.Cumulative.BaseSHA != previous.DiffMetrics.Cumulative.BaseSHA {
		return false, nil
	}
	for _, findingID := range state.Records[index].FindingIDs {
		finding, ok := state.Findings[findingID]
		if !ok {
			return false, domain.ErrReviewYieldHistoryInconsistent
		}
		switch finding.Severity {
		case domain.FindingSeverityP2, domain.FindingSeverityP3:
			continue
		case domain.FindingSeverityP0, domain.FindingSeverityP1:
		}
		// Critical, high, or unset: plan §7 treats a missing severity as high,
		// and credibility filters nothing today, so every such finding counts.
		return false, nil
	}
	net := func(stats domain.DiffStats) int { return stats.Additions - stats.Deletions }
	return net(round.DiffMetrics.Cumulative) > net(previous.DiffMetrics.Cumulative), nil
}

type reviewDiminishingFinishAuthority struct {
	ItemID             domain.ItemID `json:"item_id"`
	CommandID          string        `json:"command_id"`
	AdjudicationDigest domain.Digest `json:"adjudication_digest"`
}

func reviewDiminishingFinishReason(
	itemID domain.ItemID, commandID string, adjudicationDigest domain.Digest,
) (string, error) {
	authority := reviewDiminishingFinishAuthority{
		ItemID: itemID, CommandID: commandID, AdjudicationDigest: adjudicationDigest,
	}
	if authority.ItemID == "" || authority.CommandID == "" || authority.AdjudicationDigest == "" {
		return "", domain.ErrEmptyField
	}
	body, err := json.Marshal(authority)
	if err != nil {
		return "", err
	}
	return reviewDiminishingFinishReasonPrefix + string(body), nil
}

func reviewDiminishingFinishAuthorityFromReason(
	reason string,
) (reviewDiminishingFinishAuthority, error) {
	body, ok := strings.CutPrefix(reason, reviewDiminishingFinishReasonPrefix)
	if !ok || body == "" {
		return reviewDiminishingFinishAuthority{}, domain.ErrParentKeyMismatch
	}
	var authority reviewDiminishingFinishAuthority
	if err := strictjson.Decode(
		[]byte(body), &authority, strictjson.RejectInvalidUTF8, 4096,
	); err != nil {
		return reviewDiminishingFinishAuthority{}, err
	}
	canonical, err := reviewDiminishingFinishReason(
		authority.ItemID, authority.CommandID, authority.AdjudicationDigest)
	if err != nil || canonical != reason {
		return reviewDiminishingFinishAuthority{}, domain.ErrParentKeyMismatch
	}
	return authority, nil
}

func (tx *ReadTx) validateReviewDiminishingDeferredDisposition(
	ctx context.Context, disposition domain.ReviewDispositionRecord,
) error {
	if disposition.Disposition != domain.ReviewDispositionDeferred {
		return domain.ErrInvalidDispositionAdjudication
	}
	authority, err := reviewDiminishingFinishAuthorityFromReason(disposition.Reason)
	if err != nil {
		return err
	}
	decision, err := tx.ReviewDiminishingDecision(ctx, authority.ItemID)
	if err != nil {
		return err
	}
	if decision.Command == nil || decision.Command.CommandID != authority.CommandID ||
		decision.Command.Action != domain.ActionFinishNow ||
		decision.Binding.AdjudicationDigest != authority.AdjudicationDigest ||
		disposition.RunID != decision.Binding.RunID || disposition.Round != decision.Binding.Round ||
		disposition.AdjudicationDigest != decision.Binding.AdjudicationDigest ||
		decision.Item.DecidedAt == nil || !disposition.CreatedAt.Equal(*decision.Item.DecidedAt) {
		return domain.ErrTransitionCommandMismatch
	}
	return nil
}

// FinishReviewDiminishing writes the complete displayed batch as deferred in
// one transaction. Its command is re-derived here; callers cannot supply an
// authority bit or a partial finding set.
func (tx *WriteTx) FinishReviewDiminishing(
	ctx context.Context, itemID domain.ItemID,
) error {
	decision, err := tx.ReviewDiminishingDecision(ctx, itemID)
	if err != nil {
		return err
	}
	if decision.Command == nil || decision.Command.Action != domain.ActionFinishNow ||
		decision.Item.DecidedAt == nil {
		return domain.ErrTransitionCommandMismatch
	}
	record, err := tx.reviewRecordForRound(
		ctx, decision.Binding.RunID, decision.Binding.Round)
	if err != nil {
		return err
	}
	reason, err := reviewDiminishingFinishReason(
		itemID, decision.Command.CommandID, decision.Binding.AdjudicationDigest)
	if err != nil {
		return err
	}
	createdAt := decision.Item.DecidedAt.UTC()
	for _, findingID := range record.FindingIDs {
		disposition := domain.ReviewDispositionRecord{
			FindingID: findingID, RunID: record.RunID, Round: record.Round,
			Disposition: domain.ReviewDispositionDeferred, Reason: reason,
			AdjudicationDigest: decision.Binding.AdjudicationDigest, CreatedAt: createdAt,
		}
		if err := tx.putFindingDisposition(ctx, disposition, true); err != nil {
			return err
		}
	}
	return nil
}
