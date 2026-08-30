package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	"github.com/freeside-ai/freeside/daemon/internal/pathfold"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

const (
	findingMaterialityThresholdKey = "review.adjudication_materiality_threshold"
	findingConfidenceThresholdKey  = "review.adjudication_confidence_threshold"
	maxAdjudicationAttachmentBytes = 1 << 20
	maxAdjudicationAttachmentTotal = 1 << 20
)

// findingAdjudicator is the engine-side judgment seam for the residue that the
// exact classifier/containment fast path cannot decide. Production installs the
// inference adapter when the adjudicator site is configured; tests may install
// a deterministic fake.
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
	Feedback                  *findingAdjudicationFeedback
	Findings                  []findingAdjudicationInput
	PriorEntries              []domain.FindingAdjudicationEntry
}

type findingAdjudicationFeedback struct {
	InvocationID       domain.InvocationID
	ConversationID     domain.ConversationID
	ThroughSequence    int
	PrefixDigest       domain.Digest
	ConversationPrefix []byte
	Attachments        []findingAdjudicationAttachment
}

type findingAdjudicationAttachment struct {
	Digest  domain.Digest
	Content string
}

type findingAdjudicationInput struct {
	Finding        domain.Finding
	Classification *domain.Classification
	Surface        string
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
		diminishing, state, handled, found, err := w.reconcileExistingReviewDiminishing(
			ctx, productionReviewDiminishingItemID(task.RunID, record.Round))
		if err != nil {
			return productionReviewPending, err
		}
		if found {
			if handled {
				return state, nil
			}
			if diminishing.action == domain.ActionApplyThenFinish ||
				diminishing.action == domain.ActionContinueUnderPolicy {
				return productionReviewContinue, nil
			}
			return productionReviewPending, domain.ErrTransitionCommandMismatch
		}
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
		artifact, err = w.reviseFindingAdjudication(
			ctx, task, binding, record, artifact, dissent, baseRoot, candidateRoot)
		if err != nil {
			return productionReviewPending, err
		}
		return w.executeFindingAdjudication(ctx, task, record, artifact, candidateRoot)
	case !errors.Is(err, store.ErrNotFound):
		return productionReviewPending, err
	}
	parked, err := w.findingAdjudicationFailSafeParked(ctx, task, record)
	if err != nil {
		return productionReviewPending, err
	}
	if parked {
		return productionReviewPending, nil
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
		surfacePath := ""
		if surface != nil && finding.Location != nil {
			surfacePath = finding.Location.Path
		}
		residue = append(residue, findingAdjudicationInput{
			Finding: finding, Classification: classification,
			Surface: surfacePath, Compatibility: compatibility,
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
		if err != nil || validateModelAdjudicationEntries(
			residue, modelEntries, confidenceThreshold) != nil {
			return w.parkUnacceptedFindingBatch(ctx, task, record)
		}
		entries = append(entries, modelEntries...)
	}

	// #1002 replaces the empty digest with the prospective surface digest from domain.NextDecisionSurface.
	artifact, err = domain.NewFindingAdjudication(
		record.RunID, record.Round, binding.run.SpecDigest, record.InstructionDigest,
		binding.resolvedPolicy.Digest, entries, "", w.now().UTC())
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

const findingAdjudicationResultVersion = "freeside.finding-adjudication-result/v1"

type findingAdjudicationResult struct {
	Version           string                            `json:"version"`
	PredecessorDigest domain.Digest                     `json:"predecessor_digest"`
	Unavailable       bool                              `json:"unavailable"`
	Entries           []domain.FindingAdjudicationEntry `json:"entries"`
}

type findingAdjudicationDiscussion struct {
	item         domain.AttentionItem
	request      domain.ConversationInvocationIntent
	invocation   domain.AgentInvocation
	conversation domain.Conversation
	reply        *domain.Message
}

// reviseFindingAdjudication consumes only an authenticated Discuss turn bound
// to the current artifact. Each prior entry is reconsidered while engine facts
// are re-derived from the bound trees. The successor remains behind a new human
// decision item, so conversation text cannot execute a route.
func (w *productionPublicationWorkflow) reviseFindingAdjudication(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
	record domain.ReviewRecord,
	prior domain.FindingAdjudication,
	dissent *findingAdjudicationDissent,
	baseRoot, candidateRoot string,
) (domain.FindingAdjudication, error) {
	discussion, err := w.findingAdjudicationDiscussion(ctx, task, prior)
	if err != nil || discussion == nil {
		return prior, err
	}
	if w.signet == nil || w.artifacts == nil || w.findingAdjudicator == nil {
		return prior, nil
	}
	if err := w.reserveFindingAdjudicationRevisionAttention(
		task, record, prior.Revision+1); err != nil {
		return prior, err
	}
	prefixDigest, prefix, err := discussion.conversation.PrefixContent(discussion.invocation.ThroughSequence)
	if err != nil {
		return prior, err
	}
	feedback := findingAdjudicationFeedback{
		InvocationID: discussion.invocation.ID, ConversationID: discussion.conversation.ID,
		ThroughSequence: discussion.invocation.ThroughSequence,
		PrefixDigest:    prefixDigest, ConversationPrefix: prefix,
	}

	var entries []domain.FindingAdjudicationEntry
	if discussion.reply != nil {
		residue, deriveErr := w.findingAdjudicationRevisionInputs(
			ctx, binding, record, prior, baseRoot, candidateRoot, false)
		if deriveErr != nil {
			return prior, deriveErr
		}
		entries, err = w.loadFindingAdjudicationResult(prior, *discussion.reply,
			residue, resolvedFindingThreshold(binding.resolvedPolicy, findingConfidenceThresholdKey))
		if errors.Is(err, inference.ErrAdjudicationNotAvailable) {
			return prior, w.retireFindingAdjudicationDiscussion(ctx, *discussion)
		}
		if err != nil {
			return prior, err
		}
	} else {
		feedback.Attachments, err = w.materializeFindingAdjudicationAttachments(
			discussion.conversation, discussion.invocation.ThroughSequence)
		if errors.Is(err, inference.ErrAdjudicationNotAvailable) {
			return prior, w.completeUnavailableFindingAdjudicationDiscussion(ctx, *discussion, prior)
		}
		if err != nil {
			return prior, err
		}
		entries, err = w.revisedFindingAdjudicationEntries(
			ctx, binding, record, prior, baseRoot, candidateRoot, dissent, feedback)
		if errors.Is(err, inference.ErrAdjudicationNotAvailable) {
			return prior, w.completeUnavailableFindingAdjudicationDiscussion(ctx, *discussion, prior)
		}
		if err != nil {
			return prior, err
		}
		body, digest, err := encodeFindingAdjudicationResult(prior, entries)
		if err != nil {
			return prior, err
		}
		if _, err := w.artifacts.Put(digest, bytes.NewReader(body)); err != nil {
			return prior, fmt.Errorf("store finding-adjudication result: %w", err)
		}
		reply := signet.AgentReply{
			Body: findingAdjudicationReplySummary(prior, entries), Attachments: []domain.Digest{digest},
		}
		if err := w.acceptFindingAdjudicationCompletion(ctx, *discussion, prior, reply); err != nil {
			return prior, err
		}
	}

	durableFeedback := domain.AdjudicationFeedback{
		InvocationID: feedback.InvocationID, ConversationID: feedback.ConversationID,
		ThroughSequence: feedback.ThroughSequence, PrefixDigest: feedback.PrefixDigest,
	}
	// #1002 replaces the empty digest with the prospective surface digest from domain.NextDecisionSurface.
	successor, err := domain.NewSuccessorFindingAdjudication(
		prior, durableFeedback, entries, "", w.now().UTC())
	if err != nil {
		return prior, err
	}
	if err := runDurableTransitionHook(w.transitionHook,
		DurableTransitionFindingAdjudication, DurableTransitionBefore); err != nil {
		return prior, err
	}
	if err := w.store.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutFindingAdjudication(ctx, successor); err != nil {
			return err
		}
		predecessor, err := tx.GetAttentionItem(ctx, discussion.request.ItemID)
		if err != nil {
			return err
		}
		if predecessor.Status != domain.StatusOpen ||
			predecessor.ItemVersion != discussion.request.ItemVersion+1 {
			return domain.ErrParentKeyMismatch
		}
		predecessor.Status = domain.StatusSuperseded
		predecessor.ItemVersion++
		if err := tx.PutAttentionItem(ctx, predecessor); err != nil {
			return err
		}
		findings, err := loadAdjudicationFindings(ctx, tx, successor)
		if err != nil {
			return err
		}
		replacement, err := w.newFindingAdjudicationAttentionItem(task, successor, findings)
		if err != nil {
			return err
		}
		if err := tx.PutAttentionItem(ctx, replacement); err != nil {
			return err
		}
		return tx.MarkOutboxDispatched(ctx, string(discussion.invocation.ID))
	}); err != nil {
		return prior, err
	}
	if err := runDurableTransitionHook(w.transitionHook,
		DurableTransitionFindingAdjudication, DurableTransitionAfter); err != nil {
		return prior, err
	}
	return successor, nil
}

func (w *productionPublicationWorkflow) completeUnavailableFindingAdjudicationDiscussion(
	ctx context.Context, discussion findingAdjudicationDiscussion, prior domain.FindingAdjudication,
) error {
	body, digest, err := encodeUnavailableFindingAdjudicationResult(prior)
	if err != nil {
		return err
	}
	if _, err := w.artifacts.Put(digest, bytes.NewReader(body)); err != nil {
		return fmt.Errorf("store unavailable finding-adjudication result: %w", err)
	}
	if err := w.acceptFindingAdjudicationCompletion(ctx, discussion, prior, signet.AgentReply{
		Body:        "I could not safely use the supplied feedback, so the prior recommendation remains parked. Please discuss again with bounded text evidence.",
		Attachments: []domain.Digest{digest},
	}); err != nil {
		return err
	}
	return w.retireFindingAdjudicationDiscussion(ctx, discussion)
}

func (w *productionPublicationWorkflow) retireFindingAdjudicationDiscussion(
	ctx context.Context, discussion findingAdjudicationDiscussion,
) error {
	return w.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.MarkOutboxDispatched(ctx, string(discussion.invocation.ID))
	})
}

func (w *productionPublicationWorkflow) acceptFindingAdjudicationCompletion(
	ctx context.Context, discussion findingAdjudicationDiscussion, prior domain.FindingAdjudication,
	reply signet.AgentReply,
) error {
	return w.signet.AcceptAgentCompletion(ctx, discussion.invocation.ID, reply,
		signet.WithPreCommitGate(func(ctx context.Context, tx *store.ReadTx) error {
			item, err := tx.GetAttentionItem(ctx, discussion.request.ItemID)
			if err != nil {
				return err
			}
			if item.Status != domain.StatusOpen || item.ItemVersion != discussion.request.ItemVersion ||
				item.Type != domain.AttentionFindingAdjudication || item.FindingAdjudication == nil ||
				item.FindingAdjudication.AdjudicationDigest != prior.Digest || item.ConversationID == nil ||
				*item.ConversationID != discussion.request.ConversationID {
				return domain.ErrParentKeyMismatch
			}
			head, err := tx.GetFindingAdjudicationForRound(ctx, prior.RunID, prior.Round)
			if err != nil || head.Digest != prior.Digest {
				return errors.Join(err, domain.ErrParentKeyMismatch)
			}
			return nil
		}),
	)
}

func (w *productionPublicationWorkflow) findingAdjudicationDiscussion(
	ctx context.Context, task productionPublicationTask, prior domain.FindingAdjudication,
) (*findingAdjudicationDiscussion, error) {
	itemID := productionFindingAdjudicationItemID(task.RunID, prior.Round, prior.Revision)
	var result *findingAdjudicationDiscussion
	err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		item, err := tx.GetAttentionItem(ctx, itemID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if item.Status != domain.StatusOpen {
			return nil
		}
		if item.Type == domain.AttentionReviewDispute {
			return nil
		}
		if item.Type != domain.AttentionFindingAdjudication || item.FindingAdjudication == nil ||
			item.FindingAdjudication.AdjudicationDigest != prior.Digest ||
			item.FindingAdjudication.RunID != prior.RunID ||
			item.FindingAdjudication.Round != prior.Round {
			return domain.ErrParentKeyMismatch
		}
		if item.ConversationID == nil {
			return nil
		}
		commands, err := tx.ListCommandsForItem(ctx, itemID)
		if err != nil {
			return err
		}
		var command *domain.Command
		for index := range commands {
			if commands[index].Action == domain.ActionDiscuss &&
				(command == nil || commands[index].ItemVersion > command.ItemVersion) {
				command = &commands[index]
			}
		}
		if command == nil {
			return nil
		}
		invocationID := domain.InvocationID("inv-" + command.CommandID)
		intent, err := tx.GetOutbox(ctx, string(invocationID))
		if err != nil {
			return err
		}
		request, err := domain.DecodeConversationInvocationIntent(intent.Payload)
		if err != nil {
			return err
		}
		invocation, err := tx.GetAgentInvocation(ctx, invocationID)
		if err != nil {
			return err
		}
		conversation, err := tx.GetConversation(ctx, *item.ConversationID)
		if err != nil {
			return err
		}
		if intent.Quarantined() || intent.Kind != string(domain.AgentInvocationRequestedKind) ||
			intent.IdempotencyKey != string(invocationID) || request.InvocationID != invocationID ||
			request.ItemID != itemID || request.ConversationID != conversation.ID ||
			invocation.ConversationID == nil || *invocation.ConversationID != conversation.ID ||
			invocation.ThroughSequence != request.ItemVersion-1 {
			return domain.ErrParentKeyMismatch
		}
		var reply *domain.Message
		if invocation.ThroughSequence < len(conversation.Messages) {
			candidate := conversation.Messages[invocation.ThroughSequence]
			if candidate.ID != domain.MessageID("msg-agent-"+string(invocationID)) ||
				candidate.Author != domain.AuthorAgent || candidate.Sequence != invocation.ThroughSequence+1 {
				return domain.ErrParentKeyMismatch
			}
			reply = &candidate
		}
		if (reply == nil && (conversation.Status != domain.ConversationAwaitingAgent ||
			item.ItemVersion != request.ItemVersion)) ||
			(reply != nil && (conversation.Status != domain.ConversationIdle ||
				item.ItemVersion != request.ItemVersion+1)) {
			return domain.ErrParentKeyMismatch
		}
		result = &findingAdjudicationDiscussion{
			item: item, request: request, invocation: invocation, conversation: conversation, reply: reply,
		}
		return nil
	})
	return result, err
}

func (w *productionPublicationWorkflow) materializeFindingAdjudicationAttachments(
	conversation domain.Conversation, through int,
) ([]findingAdjudicationAttachment, error) {
	if err := conversation.Validate(); err != nil {
		return nil, err
	}
	if through < 1 || through > len(conversation.Messages) {
		return nil, domain.ErrParentKeyMismatch
	}
	start := 0
	for index, message := range conversation.Messages[:through] {
		if message.Author == domain.AuthorAgent {
			start = index + 1
		}
	}
	attachments := make([]findingAdjudicationAttachment, 0)
	total := 0
	for _, message := range conversation.Messages[start:through] {
		for _, digest := range message.Attachments {
			remaining := maxAdjudicationAttachmentTotal - total
			if remaining <= 0 {
				return nil, fmt.Errorf("%w: Discuss attachments exceed %d bytes",
					inference.ErrAdjudicationNotAvailable, maxAdjudicationAttachmentTotal)
			}
			limit := min(maxAdjudicationAttachmentBytes, remaining)
			reader, err := w.artifacts.Open(digest)
			if err != nil {
				return nil, fmt.Errorf("open Discuss attachment %q: %w", digest, err)
			}
			body, readErr := io.ReadAll(io.LimitReader(reader, int64(limit+1)))
			closeErr := reader.Close()
			if readErr != nil || closeErr != nil {
				return nil, errors.Join(readErr, closeErr)
			}
			if len(body) > limit {
				return nil, fmt.Errorf("%w: Discuss attachment %q exceeds bounded input",
					inference.ErrAdjudicationNotAvailable, digest)
			}
			if domain.Digest(contentaddr.Sum(body)) != digest {
				return nil, fmt.Errorf("discuss attachment %q: %w", digest, domain.ErrParentKeyMismatch)
			}
			if !utf8.Valid(body) {
				return nil, fmt.Errorf("%w: Discuss attachment %q is not UTF-8 text",
					inference.ErrAdjudicationNotAvailable, digest)
			}
			total += len(body)
			attachments = append(attachments, findingAdjudicationAttachment{
				Digest: digest, Content: string(body),
			})
		}
	}
	return attachments, nil
}

func (w *productionPublicationWorkflow) revisedFindingAdjudicationEntries(
	ctx context.Context, binding productionBinding, record domain.ReviewRecord,
	prior domain.FindingAdjudication, baseRoot, candidateRoot string,
	dissent *findingAdjudicationDissent,
	feedback findingAdjudicationFeedback,
) ([]domain.FindingAdjudicationEntry, error) {
	residue, err := w.findingAdjudicationRevisionInputs(
		ctx, binding, record, prior, baseRoot, candidateRoot, true)
	if err != nil {
		return nil, err
	}
	if len(residue) == 0 {
		return nil, inference.ErrAdjudicationNotAvailable
	}
	declaredPaths := domain.CanonicalDeclaredPaths(binding.resolvedPolicy)
	modelEntries, err := w.findingAdjudicator.Adjudicate(ctx, findingAdjudicationRequest{
		RunID: record.RunID, Round: record.Round,
		ApprovedSpecDigest: binding.run.SpecDigest, InstructionSnapshotDigest: record.InstructionDigest,
		ResolvedPolicyDigest: binding.resolvedPolicy.Digest, DeclaredPaths: slices.Clone(declaredPaths),
		Dissent: dissent, Feedback: &feedback, Findings: residue,
		PriorEntries: slices.Clone(prior.Entries),
	})
	if err != nil {
		return nil, errors.Join(inference.ErrAdjudicationNotAvailable, err)
	}
	threshold := resolvedFindingThreshold(binding.resolvedPolicy, findingConfidenceThresholdKey)
	if err := validateModelAdjudicationEntries(residue, modelEntries, threshold); err != nil {
		return nil, errors.Join(inference.ErrAdjudicationNotAvailable, err)
	}
	return modelEntries, nil
}

func (w *productionPublicationWorkflow) findingAdjudicationRevisionInputs(
	ctx context.Context, binding productionBinding, record domain.ReviewRecord,
	prior domain.FindingAdjudication, baseRoot, candidateRoot string,
	refreshClassifications bool,
) ([]findingAdjudicationInput, error) {
	if refreshClassifications {
		_, _ = w.classifyReviewFindings(ctx, productionPublicationTask{
			RunID: record.RunID, ProjectID: binding.run.ProjectID, HeadSHA: record.HeadSHA,
		}, record)
	}
	declaredPaths := domain.CanonicalDeclaredPaths(binding.resolvedPolicy)
	residue := make([]findingAdjudicationInput, 0, len(prior.Entries))
	for _, priorEntry := range prior.Entries {
		var finding domain.Finding
		var classification *domain.Classification
		if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			finding, err = tx.GetFinding(ctx, priorEntry.FindingID)
			if err != nil {
				return err
			}
			got, err := tx.GetClassification(ctx, priorEntry.FindingID, record.Round)
			if errors.Is(err, store.ErrNotFound) {
				return nil
			}
			if err == nil {
				classification = &got
			}
			return err
		}); err != nil {
			return nil, err
		}
		if classification != nil && w.inference != nil {
			if _, err := w.inference.EvaluateClassification(finding, *classification); err != nil {
				classification = nil
			}
		} else {
			classification = nil
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
			return nil, err
		}
		surfacePath := ""
		if surface != nil && finding.Location != nil {
			surfacePath = finding.Location.Path
		}
		residue = append(residue, findingAdjudicationInput{
			Finding: finding, Classification: classification, Surface: surfacePath,
			Compatibility: domain.EngineCompatibility(surface, declaredPaths,
				func(patterns []string, path string) bool { return pathfold.MatchAny(patterns, path, false) }),
		})
	}
	return residue, nil
}

func validateModelAdjudicationEntries(
	residue []findingAdjudicationInput, entries []domain.FindingAdjudicationEntry,
	threshold domain.DispatchThreshold,
) error {
	if len(entries) != len(residue) {
		return domain.ErrParentKeyMismatch
	}
	expected := make(map[domain.FindingID]domain.WorkUnitCompatibility, len(residue))
	for _, input := range residue {
		if _, duplicate := expected[input.Finding.ID]; input.Finding.ID == "" || duplicate {
			return domain.ErrParentKeyMismatch
		}
		expected[input.Finding.ID] = input.Compatibility
	}
	for _, entry := range entries {
		compatibility, ok := expected[entry.FindingID]
		if !ok || !modelBackedAdjudication(entry) ||
			(entry.Producer == domain.AdjudicationProducerEngineModel &&
				compatibility != domain.CompatibilityAllowed) {
			return domain.ErrParentKeyMismatch
		}
		delete(expected, entry.FindingID)
		accepted, err := entry.Accepted(threshold)
		if err != nil || !accepted {
			return errors.Join(err, inference.ErrAdjudicationNotAvailable)
		}
	}
	if len(expected) != 0 {
		return domain.ErrParentKeyMismatch
	}
	return nil
}

func modelBackedAdjudication(entry domain.FindingAdjudicationEntry) bool {
	return entry.Producer == domain.AdjudicationProducerModel ||
		entry.Producer == domain.AdjudicationProducerEngineModel
}

func encodeFindingAdjudicationResult(
	prior domain.FindingAdjudication, entries []domain.FindingAdjudicationEntry,
) ([]byte, domain.Digest, error) {
	return encodeFindingAdjudicationResultValue(findingAdjudicationResult{
		Version: findingAdjudicationResultVersion, PredecessorDigest: prior.Digest,
		Entries: append([]domain.FindingAdjudicationEntry{}, entries...),
	})
}

func encodeUnavailableFindingAdjudicationResult(
	prior domain.FindingAdjudication,
) ([]byte, domain.Digest, error) {
	return encodeFindingAdjudicationResultValue(findingAdjudicationResult{
		Version: findingAdjudicationResultVersion, PredecessorDigest: prior.Digest,
		Unavailable: true, Entries: []domain.FindingAdjudicationEntry{},
	})
}

func encodeFindingAdjudicationResultValue(
	result findingAdjudicationResult,
) ([]byte, domain.Digest, error) {
	body, err := json.Marshal(result)
	if err != nil {
		return nil, "", err
	}
	if len(body) > domain.MaxFindingAdjudicationBytes {
		return nil, "", inference.ErrAdjudicationNotAvailable
	}
	return body, domain.Digest(contentaddr.Sum(body)), nil
}

func (w *productionPublicationWorkflow) loadFindingAdjudicationResult(
	prior domain.FindingAdjudication, reply domain.Message, residue []findingAdjudicationInput,
	threshold domain.DispatchThreshold,
) ([]domain.FindingAdjudicationEntry, error) {
	if len(reply.Attachments) != 1 {
		return nil, domain.ErrParentKeyMismatch
	}
	reader, err := w.artifacts.Open(reply.Attachments[0])
	if err != nil {
		return nil, err
	}
	defer func() { _ = reader.Close() }()
	body, err := io.ReadAll(io.LimitReader(reader, domain.MaxFindingAdjudicationBytes+1))
	if err != nil || len(body) > domain.MaxFindingAdjudicationBytes ||
		domain.Digest(contentaddr.Sum(body)) != reply.Attachments[0] {
		return nil, errors.Join(err, domain.ErrParentKeyMismatch)
	}
	var result findingAdjudicationResult
	if err := strictjson.Decode(body, &result, strictjson.RejectInvalidUTF8,
		domain.MaxFindingAdjudicationBytes); err != nil {
		return nil, err
	}
	if result.Version != findingAdjudicationResultVersion || result.PredecessorDigest != prior.Digest {
		return nil, domain.ErrParentKeyMismatch
	}
	if result.Unavailable {
		if len(result.Entries) != 0 {
			return nil, domain.ErrParentKeyMismatch
		}
		return nil, inference.ErrAdjudicationNotAvailable
	}
	if err := validateModelAdjudicationEntries(residue, result.Entries, threshold); err != nil {
		return nil, err
	}
	return result.Entries, nil
}

func findingAdjudicationReplySummary(
	prior domain.FindingAdjudication, entries []domain.FindingAdjudicationEntry,
) string {
	lines := []string{"I reconsidered the adjudication using your feedback. Please review the updated routes:"}
	priorRoutes := make(map[domain.FindingID]domain.AdjudicationRoute, len(prior.Entries))
	for _, entry := range prior.Entries {
		priorRoutes[entry.FindingID] = entry.Route
	}
	for _, entry := range entries {
		lines = append(lines, fmt.Sprintf("- %s: %s → %s. %s",
			entry.FindingID, priorRoutes[entry.FindingID], entry.Route, entry.Rationale))
	}
	return strings.Join(lines, "\n")
}

func (w *productionPublicationWorkflow) parkUnacceptedFindingBatch(
	ctx context.Context, task productionPublicationTask, record domain.ReviewRecord,
) (productionReviewGateState, error) {
	reason := fmt.Sprintf(
		"Review found %d issue(s); deterministic routing could not accept the complete adjudication batch.",
		len(record.FindingIDs))
	if err := w.reserveFindingAdjudicationAttention(task, record); err != nil {
		return productionReviewPending, err
	}
	if err := w.putReviewAttention(ctx, task, record, reason, domain.AttentionReviewDispute); err != nil {
		return productionReviewPending, err
	}
	return productionReviewPending, nil
}

func (w *productionPublicationWorkflow) findingAdjudicationFailSafeParked(
	ctx context.Context, task productionPublicationTask, record domain.ReviewRecord,
) (bool, error) {
	var item domain.AttentionItem
	err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		item, err = tx.GetAttentionItem(ctx, productionReviewItemID(task.RunID, record.Round))
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if item.Type != domain.AttentionReviewDispute {
		return false, nil
	}
	validSubject := item.Subject.Type == domain.SubjectRun &&
		item.Subject.ID == domain.SubjectID(task.RunID) && item.Subject.RunID != nil &&
		*item.Subject.RunID == task.RunID
	if item.ProjectID != task.ProjectID || !validSubject || item.PRHeadSHA != task.HeadSHA {
		return false, domain.ErrParentKeyMismatch
	}
	return true, nil
}

func (w *productionPublicationWorkflow) reserveFindingAdjudicationAttention(
	task productionPublicationTask, record domain.ReviewRecord,
) error {
	return w.reserveFindingAdjudicationRevisionAttention(task, record, 1)
}

func (w *productionPublicationWorkflow) reserveFindingAdjudicationRevisionAttention(
	task productionPublicationTask, record domain.ReviewRecord, revision int,
) error {
	if w.inference == nil || w.findingAdjudicator == nil {
		return nil
	}
	return w.inference.ReserveAttention(
		inference.AdjudicatorSiteID, string(task.ProjectID), string(task.RunID),
		string(productionFindingAdjudicationItemID(task.RunID, record.Round, revision)),
	)
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

// findingLookup reads a stored Finding by ID. Both *store.ReadTx and
// *store.WriteTx satisfy it (WriteTx promotes the ReadTx method), so the
// producer loads the daemon-authenticated finding coordinates from whichever
// transaction handle the caller already holds, never opening a nested one.
type findingLookup interface {
	GetFinding(ctx context.Context, id domain.FindingID) (domain.Finding, error)
}

// loadAdjudicationFindings loads the immutable stored Finding for each artifact
// entry, keyed by finding ID, so findingAdjudicationBinding can project the
// daemon-authenticated message and location the store re-gate authenticates
// (#892). Every entry's finding was loaded when the artifact was produced, so a
// missing one is a store-integrity fault surfaced (GetFinding fails closed)
// rather than a silently blank projection.
func loadAdjudicationFindings(
	ctx context.Context, lookup findingLookup, artifact domain.FindingAdjudication,
) (map[domain.FindingID]domain.Finding, error) {
	findings := make(map[domain.FindingID]domain.Finding, len(artifact.Entries))
	for _, entry := range artifact.Entries {
		if _, ok := findings[entry.FindingID]; ok {
			continue
		}
		finding, err := lookup.GetFinding(ctx, entry.FindingID)
		if err != nil {
			return nil, err
		}
		findings[entry.FindingID] = finding
	}
	return findings, nil
}

func findingAdjudicationBinding(
	artifact domain.FindingAdjudication, findings map[domain.FindingID]domain.Finding,
) domain.FindingAdjudicationBinding {
	proposals := make([]domain.FindingAdjudicationProposal, 0, len(artifact.Entries))
	for _, entry := range artifact.Entries {
		finding := findings[entry.FindingID]
		// Copy the digest-bound offered set and evidence from the artifact entry,
		// and the daemon-authenticated message and location from the stored
		// Finding. The store re-gates every copied field against these same
		// sources, so projecting anything they do not carry fails closed (#893,
		// #892). The message is whitespace-normalized through the one shared
		// derivation the re-gate also uses, so the two cannot diverge.
		proposal := domain.FindingAdjudicationProposal{
			FindingID:        entry.FindingID,
			FindingMessage:   domain.NormalizeFindingMessage(finding.Message),
			FindingLocation:  finding.Location,
			Producer:         entry.Producer,
			GoalRelationship: entry.GoalRelationship, Compatibility: entry.Compatibility,
			Route: entry.Route, Rationale: entry.Rationale, Evidence: slices.Clone(entry.Evidence),
			CitedRules: slices.Clone(entry.CitedRules), Assumptions: slices.Clone(entry.Assumptions),
			OpenQuestions: slices.Clone(entry.OpenQuestions), Confidence: entry.Confidence,
			OfferedAlternatives: slices.Clone(entry.OfferedAlternatives),
		}
		proposals = append(proposals, proposal)
	}
	return domain.FindingAdjudicationBinding{
		RunID: artifact.RunID, Round: artifact.Round,
		AdjudicationDigest: artifact.Digest, Proposals: proposals,
	}
}

func productionFindingAdjudicationItemID(
	runID domain.RunID, round, revision int,
) domain.ItemID {
	if revision == 1 {
		return productionReviewItemID(runID, round)
	}
	return domain.ItemID(fmt.Sprintf("production-review-%s-%d-revision-%d", runID, round, revision))
}

func (w *productionPublicationWorkflow) newFindingAdjudicationAttentionItem(
	task productionPublicationTask, artifact domain.FindingAdjudication,
	findings map[domain.FindingID]domain.Finding,
) (domain.AttentionItem, error) {
	binding := findingAdjudicationBinding(artifact, findings)
	actions := []domain.Action{
		domain.ActionAcceptRecommendedRoute, domain.ActionDiscuss, domain.ActionStop,
	}
	if slices.ContainsFunc(binding.Proposals, func(proposal domain.FindingAdjudicationProposal) bool {
		return len(proposal.OfferedAlternatives) > 0
	}) {
		actions = append(actions, domain.ActionChooseAlternativeRoute)
	}
	runID := task.RunID
	createdAt := w.attentionCreatedAt()
	return domain.NewAttentionItem(domain.AttentionItemInput{
		ID:        productionFindingAdjudicationItemID(task.RunID, artifact.Round, artifact.Revision),
		ProjectID: task.ProjectID,
		Subject:   domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
		Type:      domain.AttentionFindingAdjudication, Priority: domain.PriorityHigh,
		Reason:            "Choose the artifact-bound route for the adjudicated review findings.",
		RequestedDecision: actions, PRHeadSHA: task.HeadSHA,
		FindingAdjudication: &binding, ItemVersion: 1,
		InterruptionClass: domain.InterruptionPlannedGate, CreatedAt: &createdAt,
		Status: domain.StatusOpen,
	}, w.approvedRecipes)
}

func (w *productionPublicationWorkflow) putFindingAdjudicationAttention(
	ctx context.Context, task productionPublicationTask, record domain.ReviewRecord,
	artifact domain.FindingAdjudication,
) error {
	itemID := productionFindingAdjudicationItemID(task.RunID, record.Round, artifact.Revision)
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
	var findings map[domain.FindingID]domain.Finding
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		findings, err = loadAdjudicationFindings(ctx, tx, artifact)
		return err
	}); err != nil {
		return err
	}
	item, err := w.newFindingAdjudicationAttentionItem(task, artifact, findings)
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
	itemID := productionFindingAdjudicationItemID(task.RunID, record.Round, artifact.Revision)
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
		if err := w.reserveFindingAdjudicationAttention(task, record); err != nil {
			return productionReviewPending, err
		}
		if err := w.putReviewAttention(ctx, task, record,
			"Adjudication requires a second decision before a critical or high finding can be declined or deferred.",
			domain.AttentionReviewDispute); err != nil {
			return productionReviewPending, err
		}
		return productionReviewPending, nil
	}
	if needsFindingAttention && item == nil {
		if err := w.reserveFindingAdjudicationAttention(task, record); err != nil {
			return productionReviewPending, err
		}
		if err := w.putFindingAdjudicationAttention(ctx, task, record, artifact); err != nil {
			return productionReviewPending, err
		}
		return productionReviewPending, nil
	}
	diminishing, diminishingState, handled, err := w.reconcileReviewDiminishing(
		ctx, task, record, artifact)
	if err != nil {
		return productionReviewPending, err
	}
	if handled {
		return diminishingState, nil
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
	if diminishing.item != nil && diminishing.item.DecidedAt != nil {
		dispositionAt = diminishing.item.DecidedAt.UTC()
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
		if diminishing.action == domain.ActionApplyThenFinish ||
			diminishing.action == domain.ActionContinueUnderPolicy {
			return productionReviewContinue, nil
		}
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
