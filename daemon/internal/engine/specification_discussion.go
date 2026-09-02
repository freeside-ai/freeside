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
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/specify"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

const unavailableSpecDiscussionReply = "Discussion is unavailable for this specification right now; the decision set is unchanged."

type specificationDiscussionRequest domain.SpecificationDiscussionInvocationIntent

type specificationDiscussionTerminal struct {
	InvocationID        domain.InvocationID `json:"invocation_id"`
	DiscussInvocationID domain.InvocationID `json:"discuss_invocation_id"`
	Reply               string              `json:"reply"`
	Delivered           bool                `json:"delivered"`
}

type specificationDiscussionBinding struct {
	request       specificationDiscussionRequest
	base          specificationRequest
	binding       invocationBinding
	discussion    attentionDiscussion
	specification domain.Artifact
}

func specDiscussionInvocationID(commandID string) domain.InvocationID {
	// Client-generated discuss invocation IDs always occupy the inv- namespace.
	// Keep this daemon-owned invocation disjoint even when a client chooses a
	// command ID that resembles a specification discussion identity.
	return domain.InvocationID("specification-discussion-" + commandID)
}

func specDiscussionArtifactID(commandID string) domain.ArtifactID {
	return domain.ArtifactID("spec-discussion-" + commandID)
}

func specDiscussionInputArtifactIDs(
	base specificationRequest, specificationID, discussionArtifactID domain.ArtifactID,
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

func (r specificationDiscussionRequest) validate() error {
	return domain.SpecificationDiscussionInvocationIntent(r).Validate()
}

func encodeSpecificationDiscussionRequest(request specificationDiscussionRequest) ([]byte, error) {
	if err := request.validate(); err != nil {
		return nil, err
	}
	body, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	if strictjson.Limit(len(body)) > maxSpecificationContractBytes {
		return nil, fmt.Errorf("specification discussion request exceeds contract limit: %w", domain.ErrClaimTextTooLarge)
	}
	return body, nil
}

func decodeSpecificationDiscussionRequest(entry store.QueueEntry) (specificationDiscussionRequest, error) {
	if entry.Kind != KindSpecificationDiscussionRequested {
		return specificationDiscussionRequest{}, domain.ErrParentKeyMismatch
	}
	var request specificationDiscussionRequest
	if err := strictjson.Decode(entry.Payload, &request, strictjson.RejectInvalidUTF8, maxSpecificationContractBytes); err != nil {
		return specificationDiscussionRequest{}, err
	}
	if err := request.validate(); err != nil {
		return specificationDiscussionRequest{}, err
	}
	canonical, err := encodeSpecificationDiscussionRequest(request)
	if err != nil {
		return specificationDiscussionRequest{}, err
	}
	if entry.IdempotencyKey != string(request.InvocationID) || !bytes.Equal(entry.Payload, canonical) {
		return specificationDiscussionRequest{}, domain.ErrParentKeyMismatch
	}
	return request, nil
}

// SpecificationDiscussionBackupPayloadDigests authenticates a discussion marker
// for backup closure. The payload contains artifact IDs, not bare digests.
func SpecificationDiscussionBackupPayloadDigests(entry store.QueueEntry) ([]domain.Digest, error) {
	if _, err := decodeSpecificationDiscussionRequest(entry); err != nil {
		return nil, err
	}
	return nil, nil
}

func AuthenticateSpecificationDiscussionTransition(
	ctx context.Context, tx *store.ReadTx, entry store.QueueEntry,
	runID domain.RunID, stageID domain.StageID,
) error {
	request, err := decodeSpecificationDiscussionRequest(entry)
	if err != nil {
		return err
	}
	if request.SpecificationRunID != runID || specificationStageID(runID) != stageID {
		return domain.ErrParentKeyMismatch
	}
	_, err = verifySpecificationDiscussionBinding(ctx, tx, request)
	return err
}

func verifySpecificationDiscussionBinding(
	ctx context.Context, tx *store.ReadTx, request specificationDiscussionRequest,
) (specificationDiscussionBinding, error) {
	baseEntry, err := tx.GetOutbox(ctx, string(specificationInvocationID(request.SpecificationRunID, request.Iteration)))
	if err != nil {
		return specificationDiscussionBinding{}, err
	}
	base, err := decodeSpecificationRequest(baseEntry)
	if err != nil {
		return specificationDiscussionBinding{}, err
	}
	verified, err := verifySpecificationTerminal(ctx, tx, base)
	if err != nil {
		return specificationDiscussionBinding{}, err
	}
	if verified.specification == nil || verified.approval == nil ||
		verified.binding.binding.run.ID != request.SpecificationRunID ||
		base.ImplementationRunID != request.ImplementationRunID || base.ProjectID != request.ProjectID ||
		base.PolicyArtifactID != request.PolicyArtifactID || base.Iteration != request.Iteration ||
		verified.specification.ID != request.SpecArtifactID || verified.approval.ID != request.ItemID {
		return specificationDiscussionBinding{}, domain.ErrParentKeyMismatch
	}
	discussionArtifactID := specDiscussionArtifactID(
		strings.TrimPrefix(string(request.DiscussInvocationID), "inv-"),
	)
	expectedInputs := specDiscussionInputArtifactIDs(base, verified.specification.ID, discussionArtifactID)
	invocation, err := tx.GetAgentInvocation(ctx, request.InvocationID)
	if err != nil {
		return specificationDiscussionBinding{}, err
	}
	if invocation.ID != request.InvocationID || !slices.Equal(request.InputArtifactIDs, expectedInputs) ||
		!slices.Equal(invocation.InputIDs, expectedInputs) ||
		invocation.ConversationID == nil || *invocation.ConversationID != request.ConversationID ||
		invocation.ThroughSequence != request.ThroughSequence {
		return specificationDiscussionBinding{}, domain.ErrParentKeyMismatch
	}
	conversation, err := tx.GetConversation(ctx, request.ConversationID)
	if err != nil {
		return specificationDiscussionBinding{}, err
	}
	prefixDigest, _, err := conversation.PrefixContent(request.ThroughSequence)
	if err != nil || prefixDigest != request.PrefixDigest {
		return specificationDiscussionBinding{}, errors.Join(err, domain.ErrParentKeyMismatch)
	}
	discussionArtifact, err := tx.GetArtifact(ctx, discussionArtifactID)
	if err != nil {
		return specificationDiscussionBinding{}, err
	}
	if discussionArtifact.Digest != request.PrefixDigest ||
		requireSpecificationOutputProvenance(
			discussionArtifact, domain.ArtifactKindResearch, domain.ProducerDaemon, base.InvocationID,
		) != nil {
		return specificationDiscussionBinding{}, domain.ErrParentKeyMismatch
	}
	discussEntry, err := tx.GetOutbox(ctx, string(request.DiscussInvocationID))
	if err != nil {
		return specificationDiscussionBinding{}, err
	}
	discussRequest, err := domain.DecodeConversationInvocationIntent(discussEntry.Payload)
	if err != nil {
		return specificationDiscussionBinding{}, err
	}
	discussInvocation, err := tx.GetAgentInvocation(ctx, request.DiscussInvocationID)
	if err != nil {
		return specificationDiscussionBinding{}, err
	}
	if discussEntry.Kind != kindAgentInvocationRequested || discussEntry.Quarantined() ||
		discussRequest.InvocationID != request.DiscussInvocationID ||
		discussRequest.ConversationID != request.ConversationID || discussRequest.ItemID != request.ItemID ||
		discussRequest.ItemVersion != request.ItemVersion || discussInvocation.ConversationID == nil ||
		*discussInvocation.ConversationID != request.ConversationID ||
		discussInvocation.ThroughSequence != request.ThroughSequence {
		return specificationDiscussionBinding{}, domain.ErrParentKeyMismatch
	}
	return specificationDiscussionBinding{
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

func (e *Engine) loadSpecificationDiscussionBinding(
	ctx context.Context, entry store.QueueEntry,
) (specificationDiscussionRequest, invocationBinding, error) {
	request, err := decodeSpecificationDiscussionRequest(entry)
	if err != nil {
		return specificationDiscussionRequest{}, invocationBinding{},
			fmt.Errorf("%w: %w", errSpecificationDiscussionMarkerUnreadable, err)
	}
	var verified specificationDiscussionBinding
	recoveredCompletion := false
	if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		verified, err = verifySpecificationDiscussionBinding(ctx, tx, request)
		if err == nil && verified.binding.item.Status == domain.StatusOpen &&
			verified.binding.item.ItemVersion != request.ItemVersion {
			_, err = signet.ReconstructAgentCompletion(ctx, tx, request.DiscussInvocationID)
			recoveredCompletion = err == nil
		}
		return err
	}); err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, domain.ErrParentKeyMismatch) ||
			errors.Is(err, domain.ErrImmutableTransition) {
			return specificationDiscussionRequest{}, invocationBinding{},
				fmt.Errorf("%w: %w", errSpecificationDiscussionMarkerUnreadable, err)
		}
		return specificationDiscussionRequest{}, invocationBinding{}, err
	}
	if verified.binding.item.Status == domain.StatusOpen &&
		verified.binding.item.ItemVersion != request.ItemVersion && !recoveredCompletion ||
		verified.discussion.entry.Dispatched() || verified.discussion.entry.Quarantined() {
		return specificationDiscussionRequest{}, invocationBinding{},
			fmt.Errorf("%w: discussion intent is not pending: %w",
				errSpecificationDiscussionMarkerUnreadable, domain.ErrParentKeyMismatch)
	}
	return request, verified.binding, nil
}

func (e *Engine) quarantinePendingSpecificationDiscussionMarker(
	ctx context.Context, entry store.QueueEntry, cause error,
) (bool, error) {
	if !errors.Is(cause, errSpecificationDiscussionMarkerUnreadable) {
		return false, nil
	}
	var run domain.Run
	if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		commandID, ok := strings.CutPrefix(entry.IdempotencyKey, "specification-discussion-")
		if entry.Kind != KindSpecificationDiscussionRequested || !ok || commandID == "" {
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
		ctx, e.store, e.signet, specificationDiscussionMarkerQuarantinePrefix,
		run.ID, run.ProjectID, specificationDiscussionQuarantineUnreadable,
	)
}

func (e *Engine) enqueuePendingSpecDiscussion(
	ctx context.Context, verified verifiedSpecificationTerminal,
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
	ctx context.Context, verified verifiedSpecificationTerminal, discussion attentionDiscussion,
) (bool, error) {
	base := verified.binding.request
	digest, prefix, err := discussion.conversation.PrefixContent(discussion.invocation.ThroughSequence)
	if err != nil {
		return false, err
	}
	if _, err := e.specification.blobs.Put(digest, bytes.NewReader(prefix)); err != nil {
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
		// prefix is the JSON-encoded conversation prefix the digest names.
		Metadata: domain.EvidenceMetadata{
			MediaType: domain.EvidenceMediaApplicationJSON, SizeBytes: int64(len(prefix)),
			CreatedAt: e.specification.now().UTC(), Source: domain.EvidenceSourceRun,
			Availability: domain.EvidenceAvailable,
		},
	}, nil)
	if err != nil {
		return false, err
	}
	inputs := specDiscussionInputArtifactIDs(base, verified.specification.ID, artifactID)
	request := specificationDiscussionRequest{
		Version: specificationDiscussionRequestVersion, SpecificationRunID: base.SpecificationRunID,
		ImplementationRunID: base.ImplementationRunID, ProjectID: base.ProjectID,
		Iteration: base.Iteration, InvocationID: specDiscussionInvocationID(commandID),
		DiscussInvocationID: discussion.invocation.ID, ConversationID: discussion.conversation.ID,
		ThroughSequence: discussion.invocation.ThroughSequence, PrefixDigest: digest,
		ItemID: discussion.item.ID, ItemVersion: discussion.request.ItemVersion,
		InputArtifactIDs: inputs, SpecArtifactID: verified.specification.ID,
		PolicyArtifactID: base.PolicyArtifactID,
	}
	payload, err := encodeSpecificationDiscussionRequest(request)
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
		deliveryErr = ErrSpecificationInputUndeliverable
	} else {
		deliveryErr = e.validateProspectiveDelivery(ctx, verified.binding.binding.run, invocation,
			e.specification.promptPackage, true, map[domain.ArtifactID]domain.Artifact{artifactID: artifact})
	}
	if deliveryErr != nil && !errors.Is(deliveryErr, ErrSpecificationInputUndeliverable) {
		return false, deliveryErr
	}
	inserted := false
	err = e.store.Write(ctx, func(tx *store.WriteTx) error {
		current, err := verifySpecificationTerminal(ctx, &tx.ReadTx, base)
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
		if err := putArtifactIdempotent(ctx, tx, artifact); err != nil {
			return err
		}
		if err := tx.PutAgentInvocation(ctx, invocation); err != nil {
			return err
		}
		stored, created, err := tx.EnqueueOutbox(ctx, string(request.InvocationID), KindSpecificationDiscussionRequested, payload)
		if err != nil {
			return err
		}
		if !created && (stored.Kind != KindSpecificationDiscussionRequested || !bytes.Equal(stored.Payload, payload)) {
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

func (e *Engine) acceptSpecificationDiscussionAttempt(
	ctx context.Context, run domain.Run, attempt domain.Attempt, entry store.QueueEntry,
) (bool, error) {
	request, err := decodeSpecificationDiscussionRequest(entry)
	if err != nil {
		return false, err
	}
	if accepted, err := e.specificationDiscussionAlreadyAccepted(ctx, request); err != nil || accepted {
		return false, err
	}
	var resolved domain.ResolvedPolicy
	if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		_, err := verifySpecificationDiscussionBinding(ctx, tx, request)
		if err == nil {
			resolved, err = tx.GetResolvedPolicy(ctx, run.ID)
		}
		return err
	}); err != nil {
		return false, err
	}
	settings, err := specify.ParsePolicy(resolved)
	if err != nil {
		return false, err
	}
	if err := e.cancelExpiredSpecification(ctx, attempt, settings.StageActiveTime); err != nil {
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
		if _, admissibleErr := e.requireSpecificationAdmissible(ctx, request.InvocationID); admissibleErr != nil {
			if MutableAdmissionPolicyRefusal(admissibleErr) {
				return false, nil
			}
			return false, admissibleErr
		}
		output, outputErr := e.readSpecificationOutput(
			ctx, request.InvocationID, result, specify.DecodeTranscript,
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
	ctx context.Context, request specificationDiscussionRequest, reply string,
) error {
	err := e.signet.AcceptAgentCompletion(ctx, request.DiscussInvocationID, signet.AgentReply{Body: reply},
		signet.WithPreCommitGate(func(ctx context.Context, tx *store.ReadTx) error {
			verified, err := verifySpecificationDiscussionBinding(ctx, tx, request)
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
	return e.recordSpecificationDiscussionTerminal(ctx, request, reply, true)
}

func (e *Engine) recordSpecificationDiscussionTerminal(
	ctx context.Context, request specificationDiscussionRequest, reply string, delivered bool,
) error {
	terminal := specificationDiscussionTerminal{
		InvocationID: request.InvocationID, DiscussInvocationID: request.DiscussInvocationID,
		Reply: reply, Delivered: delivered,
	}
	payload, err := json.Marshal(terminal)
	if err != nil {
		return err
	}
	return e.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		stored, inserted, err := tx.RecordInbox(ctx, string(request.InvocationID), kindSpecificationDiscussionTerminal, payload)
		if err != nil {
			return err
		}
		if !inserted && (stored.Kind != kindSpecificationDiscussionTerminal || !bytes.Equal(stored.Payload, payload)) {
			return domain.ErrImmutableTransition
		}
		if err := tx.MarkOutboxDispatched(ctx, string(request.DiscussInvocationID)); err != nil {
			return err
		}
		return tx.MarkOutboxDispatched(ctx, string(request.InvocationID))
	})
}

func (e *Engine) specificationDiscussionAlreadyAccepted(
	ctx context.Context, request specificationDiscussionRequest,
) (bool, error) {
	var terminal specificationDiscussionTerminal
	err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		entry, err := tx.GetInbox(ctx, string(request.InvocationID))
		if err != nil {
			return err
		}
		if entry.Kind != kindSpecificationDiscussionTerminal {
			return domain.ErrParentKeyMismatch
		}
		if err := strictjson.Decode(entry.Payload, &terminal, strictjson.RejectInvalidUTF8, maxSpecificationContractBytes); err != nil {
			return err
		}
		if terminal.InvocationID != request.InvocationID || terminal.DiscussInvocationID != request.DiscussInvocationID ||
			terminal.Delivered != (strings.TrimSpace(terminal.Reply) != "") {
			return domain.ErrParentKeyMismatch
		}
		if _, err := verifySpecificationDiscussionBinding(ctx, tx, request); err != nil {
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
