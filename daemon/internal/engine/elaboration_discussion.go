package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/elaborate"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

const unavailableSpecDiscussionReply = "Discussion is unavailable for this specification right now; the decision set is unchanged."

type elaborationDiscussionRequest domain.ElaborationDiscussionInvocationIntent

type elaborationDiscussionTerminal struct {
	InvocationID        domain.InvocationID `json:"invocation_id"`
	DiscussInvocationID domain.InvocationID `json:"discuss_invocation_id"`
	Reply               string              `json:"reply"`
	Delivered           bool                `json:"delivered"`
}

type elaborationDiscussionBinding struct {
	request       elaborationDiscussionRequest
	base          elaborationRequest
	binding       invocationBinding
	discussion    attentionDiscussion
	specification domain.Artifact
}

func specDiscussionInvocationID(commandID string) domain.InvocationID {
	// Client-generated discuss invocation IDs always occupy the inv- namespace.
	// Keep this daemon-owned invocation disjoint even when a client chooses a
	// command ID that resembles an elaboration discussion identity.
	return domain.InvocationID("elaboration-discussion-" + commandID)
}

func specDiscussionArtifactID(commandID string) domain.ArtifactID {
	return domain.ArtifactID("spec-discussion-" + commandID)
}

func specDiscussionInputArtifactIDs(
	base elaborationRequest, specificationID, discussionArtifactID domain.ArtifactID,
) []domain.ArtifactID {
	inputs := slices.DeleteFunc(slices.Clone(base.InputArtifactIDs), func(id domain.ArtifactID) bool {
		return base.PriorSpecArtifactID != nil && id == *base.PriorSpecArtifactID ||
			slices.Contains(base.FeedbackArtifactIDs, id)
	})
	if !slices.Contains(inputs, specificationID) {
		inputs = append(inputs, specificationID)
	}
	inputs = append(inputs, base.FeedbackArtifactIDs...)
	return append(inputs, discussionArtifactID)
}

func (r elaborationDiscussionRequest) validate() error {
	return domain.ElaborationDiscussionInvocationIntent(r).Validate()
}

func encodeElaborationDiscussionRequest(request elaborationDiscussionRequest) ([]byte, error) {
	if err := request.validate(); err != nil {
		return nil, err
	}
	body, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	if strictjson.Limit(len(body)) > maxElaborationContractBytes {
		return nil, fmt.Errorf("elaboration discussion request exceeds contract limit: %w", domain.ErrClaimTextTooLarge)
	}
	return body, nil
}

func decodeElaborationDiscussionRequest(entry store.QueueEntry) (elaborationDiscussionRequest, error) {
	if entry.Kind != KindElaborationDiscussionRequested {
		return elaborationDiscussionRequest{}, domain.ErrParentKeyMismatch
	}
	var request elaborationDiscussionRequest
	if err := strictjson.Decode(entry.Payload, &request, strictjson.RejectInvalidUTF8, maxElaborationContractBytes); err != nil {
		return elaborationDiscussionRequest{}, err
	}
	if err := request.validate(); err != nil {
		return elaborationDiscussionRequest{}, err
	}
	canonical, err := encodeElaborationDiscussionRequest(request)
	if err != nil {
		return elaborationDiscussionRequest{}, err
	}
	if entry.IdempotencyKey != string(request.InvocationID) || !bytes.Equal(entry.Payload, canonical) {
		return elaborationDiscussionRequest{}, domain.ErrParentKeyMismatch
	}
	return request, nil
}

// ElaborationDiscussionBackupPayloadDigests authenticates a discussion marker
// for backup closure. The payload contains artifact IDs, not bare digests.
func ElaborationDiscussionBackupPayloadDigests(entry store.QueueEntry) ([]domain.Digest, error) {
	if _, err := decodeElaborationDiscussionRequest(entry); err != nil {
		return nil, err
	}
	return nil, nil
}

func AuthenticateElaborationDiscussionTransition(
	ctx context.Context, tx *store.ReadTx, entry store.QueueEntry,
	runID domain.RunID, stageID domain.StageID,
) error {
	request, err := decodeElaborationDiscussionRequest(entry)
	if err != nil {
		return err
	}
	if request.ElaborationRunID != runID || elaborationStageID(runID) != stageID {
		return domain.ErrParentKeyMismatch
	}
	_, err = verifyElaborationDiscussionBinding(ctx, tx, request)
	return err
}

func verifyElaborationDiscussionBinding(
	ctx context.Context, tx *store.ReadTx, request elaborationDiscussionRequest,
) (elaborationDiscussionBinding, error) {
	baseEntry, err := tx.GetOutbox(ctx, string(elaborationInvocationID(request.ElaborationRunID, request.Iteration)))
	if err != nil {
		return elaborationDiscussionBinding{}, err
	}
	base, err := decodeElaborationRequest(baseEntry)
	if err != nil {
		return elaborationDiscussionBinding{}, err
	}
	verified, err := verifyElaborationTerminal(ctx, tx, base)
	if err != nil {
		return elaborationDiscussionBinding{}, err
	}
	if verified.specification == nil || verified.approval == nil ||
		verified.binding.binding.run.ID != request.ElaborationRunID ||
		base.ImplementationRunID != request.ImplementationRunID || base.ProjectID != request.ProjectID ||
		base.PolicyArtifactID != request.PolicyArtifactID || base.Iteration != request.Iteration ||
		verified.specification.ID != request.SpecArtifactID || verified.approval.ID != request.ItemID {
		return elaborationDiscussionBinding{}, domain.ErrParentKeyMismatch
	}
	discussionArtifactID := specDiscussionArtifactID(
		strings.TrimPrefix(string(request.DiscussInvocationID), "inv-"),
	)
	expectedInputs := specDiscussionInputArtifactIDs(base, verified.specification.ID, discussionArtifactID)
	invocation, err := tx.GetAgentInvocation(ctx, request.InvocationID)
	if err != nil {
		return elaborationDiscussionBinding{}, err
	}
	if invocation.ID != request.InvocationID || !slices.Equal(request.InputArtifactIDs, expectedInputs) ||
		!slices.Equal(invocation.InputIDs, expectedInputs) ||
		invocation.ConversationID == nil || *invocation.ConversationID != request.ConversationID ||
		invocation.ThroughSequence != request.ThroughSequence {
		return elaborationDiscussionBinding{}, domain.ErrParentKeyMismatch
	}
	conversation, err := tx.GetConversation(ctx, request.ConversationID)
	if err != nil {
		return elaborationDiscussionBinding{}, err
	}
	prefixDigest, _, err := conversation.PrefixContent(request.ThroughSequence)
	if err != nil || prefixDigest != request.PrefixDigest {
		return elaborationDiscussionBinding{}, errors.Join(err, domain.ErrParentKeyMismatch)
	}
	discussionArtifact, err := tx.GetArtifact(ctx, discussionArtifactID)
	if err != nil {
		return elaborationDiscussionBinding{}, err
	}
	if discussionArtifact.Digest != request.PrefixDigest ||
		requireElaborationOutputProvenance(
			discussionArtifact, domain.ArtifactKindResearch, domain.ProducerDaemon, base.InvocationID,
		) != nil {
		return elaborationDiscussionBinding{}, domain.ErrParentKeyMismatch
	}
	discussEntry, err := tx.GetOutbox(ctx, string(request.DiscussInvocationID))
	if err != nil {
		return elaborationDiscussionBinding{}, err
	}
	discussRequest, err := domain.DecodeConversationInvocationIntent(discussEntry.Payload)
	if err != nil {
		return elaborationDiscussionBinding{}, err
	}
	discussInvocation, err := tx.GetAgentInvocation(ctx, request.DiscussInvocationID)
	if err != nil {
		return elaborationDiscussionBinding{}, err
	}
	if discussEntry.Kind != kindAgentInvocationRequested || discussEntry.Quarantined() ||
		discussRequest.InvocationID != request.DiscussInvocationID ||
		discussRequest.ConversationID != request.ConversationID || discussRequest.ItemID != request.ItemID ||
		discussRequest.ItemVersion != request.ItemVersion || discussInvocation.ConversationID == nil ||
		*discussInvocation.ConversationID != request.ConversationID ||
		discussInvocation.ThroughSequence != request.ThroughSequence {
		return elaborationDiscussionBinding{}, domain.ErrParentKeyMismatch
	}
	return elaborationDiscussionBinding{
		request: request, base: base,
		binding: invocationBinding{
			run: verified.binding.binding.run, item: *verified.approval,
			invocation: invocation, conversation: conversation,
		},
		discussion: attentionDiscussion{
			entry: discussEntry, request: discussRequest,
			invocation: discussInvocation, conversation: conversation, item: *verified.approval,
		},
		specification: *verified.specification,
	}, nil
}

func (e *Engine) loadElaborationDiscussionBinding(
	ctx context.Context, entry store.QueueEntry,
) (elaborationDiscussionRequest, invocationBinding, error) {
	request, err := decodeElaborationDiscussionRequest(entry)
	if err != nil {
		return elaborationDiscussionRequest{}, invocationBinding{},
			fmt.Errorf("%w: %w", errElaborationDiscussionMarkerUnreadable, err)
	}
	var verified elaborationDiscussionBinding
	recoveredCompletion := false
	if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		verified, err = verifyElaborationDiscussionBinding(ctx, tx, request)
		if err == nil && verified.binding.item.Status == domain.StatusOpen &&
			verified.binding.item.ItemVersion != request.ItemVersion {
			_, err = signet.ReconstructAgentCompletion(ctx, tx, request.DiscussInvocationID)
			recoveredCompletion = err == nil
		}
		return err
	}); err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, domain.ErrParentKeyMismatch) ||
			errors.Is(err, domain.ErrImmutableTransition) {
			return elaborationDiscussionRequest{}, invocationBinding{},
				fmt.Errorf("%w: %w", errElaborationDiscussionMarkerUnreadable, err)
		}
		return elaborationDiscussionRequest{}, invocationBinding{}, err
	}
	if verified.binding.item.Status == domain.StatusOpen &&
		verified.binding.item.ItemVersion != request.ItemVersion && !recoveredCompletion ||
		verified.discussion.entry.Dispatched() || verified.discussion.entry.Quarantined() {
		return elaborationDiscussionRequest{}, invocationBinding{},
			fmt.Errorf("%w: discussion intent is not pending: %w",
				errElaborationDiscussionMarkerUnreadable, domain.ErrParentKeyMismatch)
	}
	return request, verified.binding, nil
}

func (e *Engine) quarantinePendingElaborationDiscussionMarker(
	ctx context.Context, entry store.QueueEntry, cause error,
) (bool, error) {
	if !errors.Is(cause, errElaborationDiscussionMarkerUnreadable) {
		return false, nil
	}
	var run domain.Run
	if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		commandID, ok := strings.CutPrefix(entry.IdempotencyKey, "elaboration-discussion-")
		if entry.Kind != KindElaborationDiscussionRequested || !ok || commandID == "" {
			return domain.ErrParentKeyMismatch
		}
		discussInvocationID := domain.InvocationID("inv-" + commandID)
		discussEntry, err := tx.GetOutbox(ctx, string(discussInvocationID))
		if err != nil {
			return err
		}
		discussRequest, err := domain.DecodeConversationInvocationIntent(discussEntry.Payload)
		if err != nil {
			return err
		}
		discussInvocation, err := tx.GetAgentInvocation(ctx, discussInvocationID)
		if err != nil {
			return err
		}
		item, err := tx.GetAttentionItem(ctx, discussRequest.ItemID)
		if err != nil {
			return err
		}
		if discussEntry.Kind != kindAgentInvocationRequested ||
			discussEntry.IdempotencyKey != string(discussInvocationID) ||
			discussRequest.InvocationID != discussInvocationID ||
			discussInvocation.ConversationID == nil ||
			*discussInvocation.ConversationID != discussRequest.ConversationID ||
			item.ConversationID == nil || *item.ConversationID != discussRequest.ConversationID ||
			item.Type != domain.AttentionSpecApproval || item.Subject.Type != domain.SubjectRun ||
			item.Subject.RunID == nil || item.Subject.ID != domain.SubjectID(*item.Subject.RunID) ||
			item.ProjectID == "" || item.ItemVersion < discussRequest.ItemVersion {
			return domain.ErrParentKeyMismatch
		}
		run, err = tx.GetRun(ctx, *item.Subject.RunID)
		if err != nil {
			return err
		}
		if run.ProjectID != item.ProjectID {
			return domain.ErrParentKeyMismatch
		}
		return nil
	}); err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, domain.ErrParentKeyMismatch) {
			return false, nil
		}
		return false, err
	}
	return true, recordProductionQuarantine(
		ctx, e.store, e.signet, elaborationDiscussionMarkerQuarantinePrefix,
		run.ID, run.ProjectID, elaborationDiscussionQuarantineUnreadable,
	)
}

func (e *Engine) enqueuePendingSpecDiscussion(
	ctx context.Context, verified verifiedElaborationTerminal,
) (bool, error) {
	var pending *store.QueueEntry
	if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		commands, err := tx.ListCommandsForItem(ctx, verified.approval.ID)
		if err != nil {
			return err
		}
		for _, command := range commands {
			if command.Action != domain.ActionDiscuss {
				continue
			}
			entry, err := tx.GetOutbox(ctx, "inv-"+command.CommandID)
			if err != nil {
				return err
			}
			if entry.Dispatched() || entry.Quarantined() {
				continue
			}
			if pending != nil {
				return domain.ErrDuplicate
			}
			pending = &entry
		}
		return nil
	}); err != nil {
		return false, err
	}
	if pending == nil {
		return false, nil
	}
	discussion, err := e.loadAttentionDiscussion(ctx, *pending)
	if err != nil {
		return false, err
	}
	if discussion == nil || discussion.item.ID != verified.approval.ID {
		return false, domain.ErrParentKeyMismatch
	}
	return e.enqueueSpecDiscussion(ctx, verified, *discussion)
}

func (e *Engine) enqueueSpecDiscussion(
	ctx context.Context, verified verifiedElaborationTerminal, discussion attentionDiscussion,
) (bool, error) {
	base := verified.binding.request
	digest, prefix, err := discussion.conversation.PrefixContent(discussion.invocation.ThroughSequence)
	if err != nil {
		return false, err
	}
	if _, err := e.elaboration.blobs.Put(digest, bytes.NewReader(prefix)); err != nil {
		return false, err
	}
	commandID := strings.TrimPrefix(string(discussion.invocation.ID), "inv-")
	artifactID := specDiscussionArtifactID(commandID)
	artifact, err := domain.NewArtifact(domain.ArtifactInput{
		ID: artifactID, Type: domain.ArtifactKindResearch, Digest: digest,
		Provenance: domain.Provenance{
			ProducerClass:        domain.ProducerDaemon,
			ProducerInvocationID: base.InvocationID, HeadBinding: domain.HeadIndependent,
			SensitivityClass: domain.SensitivityNormal,
		},
	}, nil)
	if err != nil {
		return false, err
	}
	inputs := specDiscussionInputArtifactIDs(base, verified.specification.ID, artifactID)
	request := elaborationDiscussionRequest{
		Version: elaborationDiscussionRequestVersion, ElaborationRunID: base.ElaborationRunID,
		ImplementationRunID: base.ImplementationRunID, ProjectID: base.ProjectID,
		Iteration: base.Iteration, InvocationID: specDiscussionInvocationID(commandID),
		DiscussInvocationID: discussion.invocation.ID, ConversationID: discussion.conversation.ID,
		ThroughSequence: discussion.invocation.ThroughSequence, PrefixDigest: digest,
		ItemID: discussion.item.ID, ItemVersion: discussion.request.ItemVersion,
		InputArtifactIDs: inputs, SpecArtifactID: verified.specification.ID,
		PolicyArtifactID: base.PolicyArtifactID,
	}
	payload, err := encodeElaborationDiscussionRequest(request)
	if err != nil {
		return false, err
	}
	conversationID := discussion.conversation.ID
	invocation, err := domain.NewAgentInvocation(
		request.InvocationID, request.InputArtifactIDs, &conversationID, request.ThroughSequence,
	)
	if err != nil {
		return false, err
	}
	var deliveryErr error
	if importer.ContainsSecret(prefix) {
		deliveryErr = ErrElaborationInputUndeliverable
	} else {
		deliveryErr = e.validateProspectiveDelivery(ctx, verified.binding.binding.run, invocation,
			e.elaboration.promptPackage, true, map[domain.ArtifactID]domain.Artifact{artifactID: artifact})
	}
	if deliveryErr != nil && !errors.Is(deliveryErr, ErrElaborationInputUndeliverable) {
		return false, deliveryErr
	}
	inserted := false
	err = e.store.Write(ctx, func(tx *store.WriteTx) error {
		current, err := verifyElaborationTerminal(ctx, &tx.ReadTx, base)
		if err != nil {
			return err
		}
		item, err := tx.GetAttentionItem(ctx, request.ItemID)
		if err != nil {
			return err
		}
		generic, err := tx.GetOutbox(ctx, string(request.DiscussInvocationID))
		if err != nil {
			return err
		}
		if current.approval == nil || current.specification == nil ||
			current.specification.ID != request.SpecArtifactID ||
			item.Status == domain.StatusOpen && item.ItemVersion != request.ItemVersion ||
			item.ConversationID == nil ||
			*item.ConversationID != request.ConversationID || generic.Dispatched() || generic.Quarantined() {
			return domain.ErrParentKeyMismatch
		}
		if err := tx.PutArtifact(ctx, artifact); err != nil {
			return err
		}
		if err := tx.PutAgentInvocation(ctx, invocation); err != nil {
			return err
		}
		stored, created, err := tx.EnqueueOutbox(ctx, string(request.InvocationID), KindElaborationDiscussionRequested, payload)
		if err != nil {
			return err
		}
		if !created && (stored.Kind != KindElaborationDiscussionRequested || !bytes.Equal(stored.Payload, payload)) {
			return domain.ErrImmutableTransition
		}
		inserted = created
		return nil
	})
	if err != nil {
		return false, err
	}
	if deliveryErr != nil {
		return false, e.acceptSpecDiscussionReply(ctx, request, unavailableSpecDiscussionReply)
	}
	return inserted, nil
}

func (e *Engine) acceptElaborationDiscussionAttempt(
	ctx context.Context, run domain.Run, attempt domain.Attempt, entry store.QueueEntry,
) (bool, error) {
	request, err := decodeElaborationDiscussionRequest(entry)
	if err != nil {
		return false, err
	}
	if accepted, err := e.elaborationDiscussionAlreadyAccepted(ctx, request); err != nil || accepted {
		return false, err
	}
	var resolved domain.ResolvedPolicy
	if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		_, err := verifyElaborationDiscussionBinding(ctx, tx, request)
		if err == nil {
			resolved, err = tx.GetResolvedPolicy(ctx, run.ID)
		}
		return err
	}); err != nil {
		return false, err
	}
	settings, err := elaborate.ParsePolicy(resolved)
	if err != nil {
		return false, err
	}
	if err := e.cancelExpiredElaboration(ctx, attempt, settings.StageActiveTime); err != nil {
		if MutableAdmissionPolicyRefusal(err) {
			return false, nil
		}
		return false, err
	}
	result, ready, err := e.collectTerminal(ctx, run.ID, attempt)
	if err != nil {
		if MutableAdmissionPolicyRefusal(err) {
			return false, nil
		}
		if !errors.Is(err, ErrInvocationLost) {
			return false, err
		}
		ready = true
	}
	if !ready {
		return false, nil
	}
	reply := unavailableSpecDiscussionReply
	if err == nil && result.Status == exec.StatusCompleted {
		if _, admissibleErr := e.requireElaborationAdmissible(ctx, request.InvocationID); admissibleErr != nil {
			if MutableAdmissionPolicyRefusal(admissibleErr) {
				return false, nil
			}
			return false, admissibleErr
		}
		output, outputErr := e.readElaborationOutput(
			ctx, request.InvocationID, result, elaborate.DecodeTranscript,
		)
		if outputErr == nil && output.Reply != nil && !importer.ContainsSecret([]byte(*output.Reply)) {
			reply = *output.Reply
		}
	}
	if err := e.acceptSpecDiscussionReply(ctx, request, reply); err != nil {
		return false, err
	}
	return true, nil
}

func (e *Engine) acceptSpecDiscussionReply(
	ctx context.Context, request elaborationDiscussionRequest, reply string,
) error {
	err := e.signet.AcceptAgentCompletion(ctx, request.DiscussInvocationID, signet.AgentReply{Body: reply},
		signet.WithPreCommitGate(func(ctx context.Context, tx *store.ReadTx) error {
			verified, err := verifyElaborationDiscussionBinding(ctx, tx, request)
			if err != nil {
				return err
			}
			item := verified.binding.item
			if item.Status == domain.StatusOpen && item.ItemVersion != request.ItemVersion ||
				item.ConversationID == nil || *item.ConversationID != request.ConversationID {
				return domain.ErrParentKeyMismatch
			}
			return nil
		}),
	)
	if err != nil {
		return err
	}
	return e.recordElaborationDiscussionTerminal(ctx, request, reply, true)
}

func (e *Engine) recordElaborationDiscussionTerminal(
	ctx context.Context, request elaborationDiscussionRequest, reply string, delivered bool,
) error {
	terminal := elaborationDiscussionTerminal{
		InvocationID: request.InvocationID, DiscussInvocationID: request.DiscussInvocationID,
		Reply: reply, Delivered: delivered,
	}
	payload, err := json.Marshal(terminal)
	if err != nil {
		return err
	}
	return e.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		stored, inserted, err := tx.RecordInbox(ctx, string(request.InvocationID), kindElaborationDiscussionTerminal, payload)
		if err != nil {
			return err
		}
		if !inserted && (stored.Kind != kindElaborationDiscussionTerminal || !bytes.Equal(stored.Payload, payload)) {
			return domain.ErrImmutableTransition
		}
		if err := tx.MarkOutboxDispatched(ctx, string(request.DiscussInvocationID)); err != nil {
			return err
		}
		return tx.MarkOutboxDispatched(ctx, string(request.InvocationID))
	})
}

func (e *Engine) elaborationDiscussionAlreadyAccepted(
	ctx context.Context, request elaborationDiscussionRequest,
) (bool, error) {
	var terminal elaborationDiscussionTerminal
	err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		entry, err := tx.GetInbox(ctx, string(request.InvocationID))
		if err != nil {
			return err
		}
		if entry.Kind != kindElaborationDiscussionTerminal {
			return domain.ErrParentKeyMismatch
		}
		if err := strictjson.Decode(entry.Payload, &terminal, strictjson.RejectInvalidUTF8, maxElaborationContractBytes); err != nil {
			return err
		}
		if terminal.InvocationID != request.InvocationID || terminal.DiscussInvocationID != request.DiscussInvocationID ||
			terminal.Delivered != (strings.TrimSpace(terminal.Reply) != "") {
			return domain.ErrParentKeyMismatch
		}
		if _, err := verifyElaborationDiscussionBinding(ctx, tx, request); err != nil {
			return err
		}
		return signet.AuthenticateAgentCompletion(
			ctx, tx, request.DiscussInvocationID, signet.AgentReply{Body: terminal.Reply},
		)
	})
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}
