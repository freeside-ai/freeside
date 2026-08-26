package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/pathfold"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

const (
	findingMaterialityThresholdKey = "review.adjudication_materiality_threshold"
	findingConfidenceThresholdKey  = "review.adjudication_confidence_threshold"
)

// findingAdjudicator is the engine-side judgment seam for the residue that the
// exact classifier/containment fast path cannot decide. Production deliberately
// leaves it nil until #843; tests install a deterministic fake.
type findingAdjudicator interface {
	Adjudicate(context.Context, findingAdjudicationRequest) ([]domain.FindingAdjudicationEntry, error)
}

type findingAdjudicationRequest struct {
	RunID                     domain.RunID
	Round                     int
	ApprovedSpecDigest        domain.Digest
	InstructionSnapshotDigest domain.Digest
	ResolvedPolicyDigest      domain.Digest
	DeclaredPaths             []string
	Dissent                   *findingAdjudicationDissent
	Findings                  []findingAdjudicationInput
}

type findingAdjudicationInput struct {
	Finding        domain.Finding
	Classification *domain.Classification
	Compatibility  domain.WorkUnitCompatibility
}

type findingAdjudicationDissentKind string

const (
	findingDissentImportPathRejected   findingAdjudicationDissentKind = "import_path_rejected"
	findingDissentRemediatorPushback   findingAdjudicationDissentKind = "remediator_pushback"
	findingDissentRemediationReemitted findingAdjudicationDissentKind = "remediation_reemitted"
)

// findingAdjudicationDissent is the typed re-entry carrier reserved for #842
// and #843. Dissent never widens scope itself; it forces a fresh adjudication.
type findingAdjudicationDissent struct {
	Kind       findingAdjudicationDissentKind
	FindingIDs []domain.FindingID
	Evidence   string
}

func resolvedFindingThreshold(policy domain.ResolvedPolicy, keyName string) domain.DispatchThreshold {
	for _, key := range policy.Keys {
		if key.Key != keyName {
			continue
		}
		switch domain.DispatchThreshold(key.Value) {
		case domain.DispatchThresholdMedium:
			return domain.DispatchThresholdMedium
		case domain.DispatchThresholdHigh:
			return domain.DispatchThresholdHigh
		}
		return domain.DefaultDispatchThreshold
	}
	return domain.DefaultDispatchThreshold
}

func classificationMeets(value string, threshold domain.DispatchThreshold) bool {
	switch threshold {
	case domain.DispatchThresholdMedium:
		return value == "medium" || value == "high"
	case domain.DispatchThresholdHigh:
		return value == "high"
	}
	return false
}

func pathExists(root, path string) (bool, error) {
	current := root
	parts := strings.Split(filepath.FromSlash(path), string(filepath.Separator))
	for i, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		switch {
		case errors.Is(err, os.ErrNotExist):
			return false, nil
		case err != nil:
			return false, err
		case i < len(parts)-1 && info.Mode()&os.ModeSymlink != 0:
			// A Git tree cannot contain a descendant beneath a symlink blob.
			// Do not follow the worktree link and accidentally resolve a host path.
			return false, nil
		}
	}
	return true, nil
}

func (w *productionPublicationWorkflow) reconcileFindingAdjudication(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
	record domain.ReviewRecord,
	baseRoot, candidateRoot string,
) (productionReviewGateState, error) {
	return w.reconcileFindingAdjudicationWithDissent(
		ctx, task, binding, record, baseRoot, candidateRoot, nil)
}

func (w *productionPublicationWorkflow) reconcileFindingAdjudicationWithDissent(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
	record domain.ReviewRecord,
	baseRoot, candidateRoot string,
	dissent *findingAdjudicationDissent,
) (productionReviewGateState, error) {
	complete, err := w.reviewRoundDispositionComplete(ctx, record)
	if err != nil {
		return productionReviewPending, err
	}
	if complete {
		return productionReviewPassed, nil
	}

	var artifact domain.FindingAdjudication
	err = w.store.Read(ctx, func(tx *store.ReadTx) error {
		var getErr error
		artifact, getErr = tx.GetFindingAdjudicationForRound(ctx, record.RunID, record.Round)
		return getErr
	})
	switch {
	case err == nil:
		return w.executeFindingAdjudication(ctx, task, record, artifact, candidateRoot)
	case !errors.Is(err, store.ErrNotFound):
		return productionReviewPending, err
	}

	// Classification is advisory availability: a missing or failed annotation
	// becomes model residue and parks, never a workflow-wide outage.
	_, _ = w.classifyReviewFindings(ctx, task, record)
	materialityThreshold := resolvedFindingThreshold(
		binding.resolvedPolicy, findingMaterialityThresholdKey)
	confidenceThreshold := resolvedFindingThreshold(
		binding.resolvedPolicy, findingConfidenceThresholdKey)
	declaredPaths := domain.CanonicalDeclaredPaths(binding.resolvedPolicy)

	entries := make([]domain.FindingAdjudicationEntry, 0, len(record.FindingIDs))
	residue := make([]findingAdjudicationInput, 0, len(record.FindingIDs))
	for _, findingID := range record.FindingIDs {
		var finding domain.Finding
		var classification *domain.Classification
		if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
			var getErr error
			finding, getErr = tx.GetFinding(ctx, findingID)
			if getErr != nil {
				return getErr
			}
			got, getErr := tx.GetClassification(ctx, findingID, record.Round)
			if errors.Is(getErr, store.ErrNotFound) {
				return nil
			}
			if getErr != nil {
				return getErr
			}
			classification = &got
			return nil
		}); err != nil {
			return productionReviewPending, err
		}
		if classification != nil {
			if w.inference == nil {
				classification = nil
			} else if _, err := w.inference.EvaluateClassification(finding, *classification); err != nil {
				classification = nil
			}
		}
		surface, err := domain.DeriveRemediationSurface(finding.Location,
			func(path string) (bool, bool, error) {
				inBase, baseErr := pathExists(baseRoot, path)
				if baseErr != nil {
					return false, false, baseErr
				}
				inCandidate, candidateErr := pathExists(candidateRoot, path)
				return inBase, inCandidate, candidateErr
			})
		if err != nil {
			return productionReviewPending, err
		}
		compatibility := domain.EngineCompatibility(surface, declaredPaths,
			func(patterns []string, path string) bool {
				return pathfold.MatchAny(patterns, path, false)
			})
		fastPath := (dissent == nil || !slices.Contains(dissent.FindingIDs, finding.ID)) &&
			classification != nil && compatibility == domain.CompatibilityAllowed &&
			classificationMeets(classification.Materiality, materialityThreshold) &&
			classificationMeets(classification.Confidence, confidenceThreshold)
		if fastPath {
			allowed := domain.CompatibilityAllowed
			entry, err := domain.NewEngineAdjudicationEntry(
				finding.ID, domain.GoalRequired, &allowed, domain.RouteRemediate,
				"The finding is confidently material and its normalized location is contained by the declared work-unit paths.",
				[]string{finding.Location.String()}, nil, nil, nil, nil,
			)
			if err != nil {
				return productionReviewPending, err
			}
			entries = append(entries, entry)
			continue
		}
		residue = append(residue, findingAdjudicationInput{
			Finding: finding, Classification: classification, Compatibility: compatibility,
		})
	}

	if len(residue) > 0 {
		if w.findingAdjudicator == nil {
			return w.parkUnacceptedFindingBatch(ctx, task, record)
		}
		modelEntries, err := w.findingAdjudicator.Adjudicate(ctx, findingAdjudicationRequest{
			RunID: record.RunID, Round: record.Round,
			ApprovedSpecDigest:        binding.run.SpecDigest,
			InstructionSnapshotDigest: record.InstructionDigest,
			ResolvedPolicyDigest:      binding.resolvedPolicy.Digest,
			DeclaredPaths:             slices.Clone(declaredPaths), Dissent: dissent,
			Findings: residue,
		})
		if err != nil || len(modelEntries) != len(residue) {
			return w.parkUnacceptedFindingBatch(ctx, task, record)
		}
		residueIDs := make(map[domain.FindingID]struct{}, len(residue))
		for _, input := range residue {
			residueIDs[input.Finding.ID] = struct{}{}
		}
		for _, entry := range modelEntries {
			if _, ok := residueIDs[entry.FindingID]; !ok || entry.Producer != domain.AdjudicationProducerModel {
				return w.parkUnacceptedFindingBatch(ctx, task, record)
			}
			delete(residueIDs, entry.FindingID)
			accepted, acceptErr := entry.Accepted(confidenceThreshold)
			if acceptErr != nil || !accepted {
				return w.parkUnacceptedFindingBatch(ctx, task, record)
			}
			entries = append(entries, entry)
		}
		if len(residueIDs) != 0 {
			return w.parkUnacceptedFindingBatch(ctx, task, record)
		}
	}

	artifact, err = domain.NewFindingAdjudication(
		record.RunID, record.Round, binding.run.SpecDigest, record.InstructionDigest,
		binding.resolvedPolicy.Digest, entries, w.now().UTC(),
	)
	if err != nil {
		return productionReviewPending, err
	}
	if err := runDurableTransitionHook(w.transitionHook,
		DurableTransitionFindingAdjudication, DurableTransitionBefore); err != nil {
		return productionReviewPending, err
	}
	if err := w.store.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutFindingAdjudication(ctx, artifact)
	}); err != nil {
		return productionReviewPending, err
	}
	if err := runDurableTransitionHook(w.transitionHook,
		DurableTransitionFindingAdjudication, DurableTransitionAfter); err != nil {
		return productionReviewPending, err
	}
	return w.executeFindingAdjudication(ctx, task, record, artifact, candidateRoot)
}

func (w *productionPublicationWorkflow) parkUnacceptedFindingBatch(
	ctx context.Context, task productionPublicationTask, record domain.ReviewRecord,
) (productionReviewGateState, error) {
	reason := fmt.Sprintf(
		"Review found %d issue(s); deterministic routing could not accept the complete adjudication batch.",
		len(record.FindingIDs))
	if err := w.putReviewAttention(ctx, task, record, reason, domain.AttentionReviewDispute); err != nil {
		return productionReviewPending, err
	}
	return productionReviewPending, nil
}

func (w *productionPublicationWorkflow) reviewRoundDispositionComplete(
	ctx context.Context, record domain.ReviewRecord,
) (bool, error) {
	if record.Outcome == domain.ReviewClean {
		return true, nil
	}
	var dispositions []domain.ReviewDispositionRecord
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		dispositions, err = tx.ListFindingDispositions(ctx, record.RunID)
		return err
	}); err != nil {
		return false, err
	}
	for _, findingID := range record.FindingIDs {
		if !slices.ContainsFunc(dispositions, func(disposition domain.ReviewDispositionRecord) bool {
			return disposition.FindingID == findingID && disposition.Round == record.Round
		}) {
			return false, nil
		}
	}
	return true, nil
}

func findingAdjudicationBinding(artifact domain.FindingAdjudication) domain.FindingAdjudicationBinding {
	proposals := make([]domain.FindingAdjudicationProposal, 0, len(artifact.Entries))
	for _, entry := range artifact.Entries {
		proposal := domain.FindingAdjudicationProposal{
			FindingID: entry.FindingID, Producer: entry.Producer,
			GoalRelationship: entry.GoalRelationship, Compatibility: entry.Compatibility,
			Route: entry.Route, Rationale: entry.Rationale,
			CitedRules: slices.Clone(entry.CitedRules), Assumptions: slices.Clone(entry.Assumptions),
			OpenQuestions: slices.Clone(entry.OpenQuestions), Confidence: entry.Confidence,
		}
		if entry.GoalRelationship == domain.GoalContradictory {
			alternative := domain.RouteDecline
			consequence := "Record the finding as declined under the artifact-bound contradiction."
			if entry.Route == domain.RouteDecline {
				alternative = domain.RouteDispute
				consequence = "Keep the run parked for human adjudication."
			}
			proposal.OfferedAlternatives = []domain.OfferedAlternative{{
				Route: alternative, Consequence: consequence,
			}}
		}
		proposals = append(proposals, proposal)
	}
	return domain.FindingAdjudicationBinding{
		RunID: artifact.RunID, Round: artifact.Round,
		AdjudicationDigest: artifact.Digest, Proposals: proposals,
	}
}

func (w *productionPublicationWorkflow) putFindingAdjudicationAttention(
	ctx context.Context, task productionPublicationTask, record domain.ReviewRecord,
	artifact domain.FindingAdjudication,
) error {
	binding := findingAdjudicationBinding(artifact)
	actions := []domain.Action{
		domain.ActionAcceptRecommendedRoute, domain.ActionDiscuss, domain.ActionStop,
	}
	if slices.ContainsFunc(binding.Proposals, func(proposal domain.FindingAdjudicationProposal) bool {
		return len(proposal.OfferedAlternatives) > 0
	}) {
		actions = append(actions, domain.ActionChooseAlternativeRoute)
	}
	itemID := productionReviewItemID(task.RunID, record.Round)
	var existing *domain.AttentionItem
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		item, err := tx.GetAttentionItem(ctx, itemID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		existing = &item
		return nil
	}); err != nil {
		return err
	}
	if existing != nil {
		if existing.Type != domain.AttentionFindingAdjudication ||
			existing.FindingAdjudication == nil ||
			existing.FindingAdjudication.AdjudicationDigest != artifact.Digest {
			return domain.ErrParentKeyMismatch
		}
		return nil
	}
	runID := task.RunID
	createdAt := w.attentionCreatedAt()
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: itemID, ProjectID: task.ProjectID,
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
		Type:    domain.AttentionFindingAdjudication, Priority: domain.PriorityHigh,
		Reason:            "Choose the artifact-bound route for the adjudicated review findings.",
		RequestedDecision: actions, PRHeadSHA: task.HeadSHA,
		FindingAdjudication: &binding, ItemVersion: 1,
		InterruptionClass: domain.InterruptionPlannedGate, CreatedAt: &createdAt,
		Status: domain.StatusOpen,
	}, w.approvedRecipes)
	if err != nil {
		return err
	}
	return w.attention.PutItem(ctx, item)
}

// productionRemediationUndeliverableItemID identifies the durable escalation
// raised when a remediation candidate cannot be prepared for delivery. It is
// deterministic per run and round and deliberately distinct from the round's
// review item, whose type is immutably bound to the routing decision, so the
// escalation never collides with a review-dispute or finding-adjudication item.
func productionRemediationUndeliverableItemID(runID domain.RunID, round int) domain.ItemID {
	return domain.ItemID(fmt.Sprintf("remediation-undeliverable-%s-%d", runID, round))
}

// remediationUndeliverableRecorded reports whether a prior reconcile already
// terminalized a deterministic undeliverable-input refusal for this round. When
// it has, the caller parks the run rather than re-attempting a preparation that
// deterministically re-refuses.
func (w *productionPublicationWorkflow) remediationUndeliverableRecorded(
	ctx context.Context, task productionPublicationTask, record domain.ReviewRecord,
) (bool, error) {
	itemID := productionRemediationUndeliverableItemID(task.RunID, record.Round)
	recorded := false
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		item, err := tx.GetAttentionItem(ctx, itemID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := verifyRemediationUndeliverableItem(item, task, itemID); err != nil {
			return err
		}
		recorded = true
		return nil
	}); err != nil {
		return false, err
	}
	return recorded, nil
}

// recordRemediationUndeliverable raises the durable per-run escalation for a
// deterministic pre-invocation preparation refusal. It reuses the execution
// failure attention type that the dispatch-phase delivery refusal already
// raises, so both remediation-delivery boundaries terminalize consistently.
// Like that sibling it writes the item directly and offers only acknowledge:
// AttentionExecutionFailure's signet action policy excludes acknowledge because
// retry has no honoring machinery here, so the intake service would reject it
// (production_workflow.go recordProductionDeliveryRefusal shares this rationale).
// The get-or-put is idempotent across reconciliations.
func (w *productionPublicationWorkflow) recordRemediationUndeliverable(
	ctx context.Context, task productionPublicationTask, record domain.ReviewRecord, cause error,
) error {
	itemID := productionRemediationUndeliverableItemID(task.RunID, record.Round)
	runID := task.RunID
	reason := "Remediation input for the adjudicated findings cannot be delivered to the remediator."
	if cause != nil {
		reason += " " + cause.Error()
	}
	return w.store.Write(ctx, func(tx *store.WriteTx) error {
		existing, err := tx.GetAttentionItem(ctx, itemID)
		if err == nil {
			return verifyRemediationUndeliverableItem(existing, task, itemID)
		}
		if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		createdAt := w.attentionCreatedAt()
		item, err := domain.NewAttentionItem(domain.AttentionItemInput{
			ID: itemID, ProjectID: task.ProjectID,
			Subject:           domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
			Type:              domain.AttentionExecutionFailure,
			Priority:          domain.PriorityHigh,
			Reason:            reason,
			RequestedDecision: []domain.Action{domain.ActionAcknowledge},
			ItemVersion:       1,
			InterruptionClass: domain.InterruptionExceptional,
			CreatedAt:         &createdAt,
			Status:            domain.StatusOpen,
		}, w.approvedRecipes)
		if err != nil {
			return err
		}
		return tx.PutAttentionItem(ctx, item)
	})
}

// verifyRemediationUndeliverableItem fails closed when an existing item at the
// deterministic identity disagrees with the run it must escalate, matching the
// parent-key discipline the other production attention writers enforce.
func verifyRemediationUndeliverableItem(
	item domain.AttentionItem, task productionPublicationTask, itemID domain.ItemID,
) error {
	validSubject := item.Subject.Type == domain.SubjectRun &&
		item.Subject.ID == domain.SubjectID(task.RunID) &&
		item.Subject.RunID != nil && *item.Subject.RunID == task.RunID
	if item.Type != domain.AttentionExecutionFailure ||
		item.ProjectID != task.ProjectID || !validSubject {
		return fmt.Errorf(
			"remediation-undeliverable attention item %q disagrees with run %q: %w",
			itemID, task.RunID, domain.ErrParentKeyMismatch)
	}
	return nil
}

type findingAlternativeChoice struct {
	FindingID domain.FindingID         `json:"finding_id"`
	Route     domain.AdjudicationRoute `json:"route"`
}

func findingRoutesFromDecision(
	artifact domain.FindingAdjudication, command *domain.Command,
) (map[domain.FindingID]domain.AdjudicationRoute, error) {
	if err := artifact.Validate(); err != nil {
		return nil, err
	}
	routes := make(map[domain.FindingID]domain.AdjudicationRoute, len(artifact.Entries))
	for _, entry := range artifact.Entries {
		routes[entry.FindingID] = entry.Route
	}
	if command == nil || command.Action == domain.ActionAcceptRecommendedRoute {
		return routes, nil
	}
	if command.Action != domain.ActionChooseAlternativeRoute {
		return nil, domain.ErrParentKeyMismatch
	}
	var choices []findingAlternativeChoice
	if err := strictjson.Decode([]byte(command.Message), &choices,
		strictjson.RejectInvalidUTF8, domain.MaxFindingAdjudicationBytes); err != nil {
		return nil, err
	}
	if len(choices) == 0 {
		return nil, domain.ErrParentKeyMismatch
	}
	seen := make(map[domain.FindingID]struct{}, len(choices))
	for _, choice := range choices {
		if _, duplicate := seen[choice.FindingID]; duplicate {
			return nil, domain.ErrParentKeyMismatch
		}
		seen[choice.FindingID] = struct{}{}
		if recommended, ok := routes[choice.FindingID]; !ok || recommended == choice.Route {
			return nil, domain.ErrParentKeyMismatch
		}
		switch choice.Route {
		case domain.RouteDecline:
			if err := artifact.AuthorizesFinalDisposition(
				choice.FindingID, domain.ReviewDispositionDeclined); err != nil {
				return nil, err
			}
		case domain.RouteDispute:
			if !slices.ContainsFunc(artifact.Entries, func(entry domain.FindingAdjudicationEntry) bool {
				return entry.FindingID == choice.FindingID &&
					entry.GoalRelationship == domain.GoalContradictory && entry.Compatibility == nil
			}) {
				return nil, domain.ErrParentKeyMismatch
			}
		default:
			return nil, domain.ErrParentKeyMismatch
		}
		routes[choice.FindingID] = choice.Route
	}
	return routes, nil
}

func (w *productionPublicationWorkflow) executeFindingAdjudication(
	ctx context.Context, task productionPublicationTask, record domain.ReviewRecord,
	artifact domain.FindingAdjudication, candidateRoot string,
) (productionReviewGateState, error) {
	itemID := productionReviewItemID(task.RunID, record.Round)
	var item *domain.AttentionItem
	var command *domain.Command
	err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		current, err := tx.GetAttentionItem(ctx, itemID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if current.Type == domain.AttentionReviewDispute {
			item = &current
			return nil
		}
		if current.Type != domain.AttentionFindingAdjudication {
			return domain.ErrParentKeyMismatch
		}
		got, decision, err := tx.FindingAdjudicationDecision(ctx, itemID)
		if err != nil {
			return err
		}
		item, command = &got, decision
		return nil
	})
	if err != nil {
		return productionReviewPending, err
	}
	if command != nil && command.Action == domain.ActionStop {
		return productionReviewPending, nil
	}
	if item != nil && item.Type == domain.AttentionReviewDispute {
		return productionReviewPending, nil
	}
	if item != nil && command == nil {
		return productionReviewPending, nil
	}
	routes, err := findingRoutesFromDecision(artifact, command)
	if err != nil {
		return productionReviewPending, err
	}
	needsFindingAttention := false
	needsDisputeAttention := false
	for _, entry := range artifact.Entries {
		route := routes[entry.FindingID]
		switch route {
		case domain.RouteRemediate:
			// #842 consumes this immutable engine fact. Until then the run parks.
		case domain.RouteDefer, domain.RouteDecline:
			if item == nil && entry.FindingID != "" {
				var finding domain.Finding
				if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
					var getErr error
					finding, getErr = tx.GetFinding(ctx, entry.FindingID)
					return getErr
				}); err != nil {
					return productionReviewPending, err
				}
				if finding.Severity == domain.FindingSeverityP0 || finding.Severity == domain.FindingSeverityP1 {
					needsDisputeAttention = true
					continue
				}
			}
		case domain.RouteParkRevision, domain.RouteParkSeparateWork,
			domain.RouteAttentionHumanDecision, domain.RouteParkUnknown,
			domain.RouteAttentionUnclear:
			needsFindingAttention = true
		case domain.RouteDispute:
			needsDisputeAttention = true
		}
	}

	if needsDisputeAttention {
		if item != nil {
			return productionReviewPending, nil
		}
		if err := w.putReviewAttention(ctx, task, record,
			"Adjudication requires a second decision before a critical or high finding can be declined or deferred.",
			domain.AttentionReviewDispute); err != nil {
			return productionReviewPending, err
		}
		return productionReviewPending, nil
	}
	if needsFindingAttention && item == nil {
		if err := w.putFindingAdjudicationAttention(ctx, task, record, artifact); err != nil {
			return productionReviewPending, err
		}
		return productionReviewPending, nil
	}
	var remediation *preparedRemediationIntent
	if w.artifacts != nil {
		// A deterministic undeliverable-input refusal terminalized on a prior
		// reconcile parks the run; re-preparing would just re-refuse and re-diff.
		parked, checkErr := w.remediationUndeliverableRecorded(ctx, task, record)
		if checkErr != nil {
			return productionReviewPending, checkErr
		}
		if parked {
			return productionReviewPending, nil
		}
		remediation, err = w.prepareRemediationIntent(
			ctx, task, record, artifact, routes, candidateRoot)
		if errors.Is(err, ErrRemediationInputUndeliverable) {
			// A deterministic pre-invocation preparation refusal (for example a
			// remediation input larger than the deliverable limit) can never
			// succeed on retry. Terminalize it as a durable per-run escalation so
			// the production publication lane advances instead of stopping
			// lane-fatally when reconcileTask propagates the raw error.
			if putErr := w.recordRemediationUndeliverable(ctx, task, record, err); putErr != nil {
				return productionReviewPending, putErr
			}
			return productionReviewPending, nil
		}
		if err != nil {
			return productionReviewPending, err
		}
	}

	if err := runDurableTransitionHook(w.transitionHook,
		DurableTransitionFindingAdjudication, DurableTransitionBefore); err != nil {
		return productionReviewPending, err
	}
	dispositionAt := artifact.CreatedAt
	if item != nil && item.DecidedAt != nil {
		dispositionAt = item.DecidedAt.UTC()
	}
	if err := w.store.Write(ctx, func(tx *store.WriteTx) error {
		if remediation != nil {
			if err := remediation.persist(ctx, tx); err != nil {
				return err
			}
		}
		for _, entry := range artifact.Entries {
			route := routes[entry.FindingID]
			var disposition domain.ReviewDisposition
			switch route {
			case domain.RouteDefer:
				disposition = domain.ReviewDispositionDeferred
			case domain.RouteDecline:
				disposition = domain.ReviewDispositionDeclined
			default:
				continue
			}
			reason := fmt.Sprintf("%s (finding adjudication %s)",
				strings.TrimSpace(entry.Rationale), artifact.Digest)
			if err := tx.PutFindingDisposition(ctx, domain.ReviewDispositionRecord{
				FindingID: entry.FindingID, RunID: artifact.RunID, Round: artifact.Round,
				Disposition: disposition, Reason: reason,
				AdjudicationDigest: artifact.Digest, CreatedAt: dispositionAt,
			}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return productionReviewPending, err
	}
	if err := runDurableTransitionHook(w.transitionHook,
		DurableTransitionFindingAdjudication, DurableTransitionAfter); err != nil {
		return productionReviewPending, err
	}
	complete, err := w.reviewRoundDispositionComplete(ctx, record)
	if err != nil {
		return productionReviewPending, err
	}
	if complete {
		return productionReviewPassed, nil
	}
	return productionReviewPending, nil
}

// reenterFindingAdjudication validates the structured dissent carrier. The
// next units attach it to a newly recorded review round; this unit deliberately
// grants no transition or scope-widening authority to the carrier itself.
func validateFindingAdjudicationDissent(dissent findingAdjudicationDissent) error {
	if len(dissent.FindingIDs) == 0 || !slices.IsSorted(dissent.FindingIDs) ||
		strings.TrimSpace(dissent.Evidence) == "" {
		return domain.ErrParentKeyMismatch
	}
	for index, findingID := range dissent.FindingIDs {
		if findingID == "" || (index > 0 && findingID == dissent.FindingIDs[index-1]) {
			return domain.ErrParentKeyMismatch
		}
	}
	switch dissent.Kind {
	case findingDissentImportPathRejected, findingDissentRemediatorPushback,
		findingDissentRemediationReemitted:
		return nil
	}
	return domain.ErrParentKeyMismatch
}

func (w *productionPublicationWorkflow) reenterFindingAdjudication(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
	record domain.ReviewRecord,
	baseRoot, candidateRoot string,
	dissent findingAdjudicationDissent,
) (productionReviewGateState, error) {
	if err := validateFindingAdjudicationDissent(dissent); err != nil {
		return productionReviewPending, err
	}
	return w.reconcileFindingAdjudicationWithDissent(
		ctx, task, binding, record, baseRoot, candidateRoot, &dissent)
}
