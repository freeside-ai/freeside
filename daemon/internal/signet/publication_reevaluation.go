package signet

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

const (
	// PublicationReevaluationRequestedKind identifies a command-authorized
	// production publication reevaluation intent.
	PublicationReevaluationRequestedKind = "production_publication_reevaluation_requested"
	// PublicationReevaluationCompletedKind identifies the command-keyed,
	// write-once fact that a reevaluation reached a terminal outcome.
	PublicationReevaluationCompletedKind       = "production_publication_reevaluation_completed"
	publicationReevaluationKeyPrefix           = "production-reevaluation/"
	publicationReevaluationCompletionKeyPrefix = "production-reevaluation-completed/"
	publicationReevaluationItemPrefix          = "production-publish-blocked-reevaluation/"
	publicationReevaluationMilestonePrefix     = "publish-production-reevaluation/"
)

// PublicationReevaluationOutcome is the terminal result named by a completion
// marker. The marker establishes terminality; its references are re-read only
// to authenticate that fact against the accepted command and durable result.
type PublicationReevaluationOutcome string

const (
	PublicationReevaluationPublished       PublicationReevaluationOutcome = "published"
	PublicationReevaluationBlocked         PublicationReevaluationOutcome = "blocked"
	PublicationReevaluationReviewEscalated PublicationReevaluationOutcome = "review_escalated"
)

func (o PublicationReevaluationOutcome) valid() bool {
	switch o {
	case PublicationReevaluationPublished,
		PublicationReevaluationBlocked,
		PublicationReevaluationReviewEscalated:
		return true
	}
	return false
}

// PublicationReevaluationRequest is the immutable signet-to-engine intent.
// The engine re-reads every referenced record before treating it as authority.
type PublicationReevaluationRequest struct {
	RunID              domain.RunID  `json:"run_id"`
	ItemID             domain.ItemID `json:"item_id"`
	ItemVersion        int           `json:"item_version"`
	CommandID          string        `json:"command_id"`
	PRHeadSHA          string        `json:"pr_head_sha"`
	TrustProfileDigest domain.Digest `json:"trust_profile_digest"`
	ReviewRound        int           `json:"review_round"`
}

// PublicationReevaluationCompletion is written in the same transaction as
// the production terminal record. EvidenceItemID names a durable item to
// authenticate; it is never trusted as authority on its own.
type PublicationReevaluationCompletion struct {
	RunID                domain.RunID                   `json:"run_id"`
	CommandID            string                         `json:"command_id"`
	IntentKey            string                         `json:"intent_key"`
	Outcome              PublicationReevaluationOutcome `json:"outcome"`
	PRHeadSHA            string                         `json:"pr_head_sha"`
	EvidenceItemID       domain.ItemID                  `json:"evidence_item_id"`
	EvidenceItemVersion  int                            `json:"evidence_item_version"`
	TerminalInvocationID domain.InvocationID            `json:"terminal_invocation_id"`
}

// PublicationReevaluationCompletionKey keys the terminal fact by the accepted
// command. Unlike the intent key, the run is authenticated from the payload
// and accepted command chain rather than duplicated into this identity.
func PublicationReevaluationCompletionKey(commandID string) string {
	return publicationReevaluationCompletionKeyPrefix + commandID
}

// PublicationReevaluationTerminalInvocationID gives the completion's paired
// terminal record the same command-keyed identity as its marker.
func PublicationReevaluationTerminalInvocationID(commandID string) domain.InvocationID {
	return domain.InvocationID("production-reevaluation-terminal/" + commandID)
}

// PublicationReevaluationBlockedMilestoneInvocationID names the run
// milestone a reevaluation appends when it blocks again. run_milestones is
// unique on (run, kind, invocation), so a rerun that reused the run's
// publication invocation would silently drop its block: the milestone would
// keep the original reason and the hold recorded while the rerun was queued
// would never clear. publication_ready keeps the plain publication
// invocation: a run converges on one PR, and every ready authority binds to
// that invocation. The run ID is encoded like the intent and item keys so
// the identity inverts for every valid run ID.
func PublicationReevaluationBlockedMilestoneInvocationID(
	runID domain.RunID, commandID string,
) domain.InvocationID {
	return domain.InvocationID(publicationReevaluationMilestonePrefix +
		encodedReevaluationRunID(runID) + "/" + commandID)
}

// publicationReevaluationBlockedMilestoneCoordinates inverts
// PublicationReevaluationBlockedMilestoneInvocationID.
func publicationReevaluationBlockedMilestoneCoordinates(
	invocation domain.InvocationID,
) (domain.RunID, string, bool) {
	rest, ok := strings.CutPrefix(string(invocation), publicationReevaluationMilestonePrefix)
	if !ok {
		return "", "", false
	}
	encodedRunID, commandID, ok := strings.Cut(rest, "/")
	if !ok || encodedRunID == "" || commandID == "" {
		return "", "", false
	}
	runID, ok := decodeReevaluationRunID(encodedRunID)
	if !ok {
		return "", "", false
	}
	return runID, commandID, true
}

func publicationReevaluationCompletionCommandID(key string) (string, bool) {
	commandID, ok := strings.CutPrefix(key, publicationReevaluationCompletionKeyPrefix)
	return commandID, ok && commandID != ""
}

// EncodePublicationReevaluationCompletion validates and canonically encodes
// the completion fact before the engine records it.
func EncodePublicationReevaluationCompletion(
	completion PublicationReevaluationCompletion,
) ([]byte, error) {
	if err := validatePublicationReevaluationCompletion(completion); err != nil {
		return nil, err
	}
	return json.Marshal(completion)
}

// DecodePublicationReevaluationCompletion strictly decodes a completion fact.
func DecodePublicationReevaluationCompletion(payload []byte) (PublicationReevaluationCompletion, error) {
	var completion PublicationReevaluationCompletion
	if err := strictjson.Decode(payload, &completion, strictjson.TolerateInvalidUTF8, strictjson.NoLimit); err != nil {
		return PublicationReevaluationCompletion{}, fmt.Errorf(
			"decode publication reevaluation completion: %w",
			errors.Join(err, domain.ErrParentKeyMismatch))
	}
	if err := validatePublicationReevaluationCompletion(completion); err != nil {
		return PublicationReevaluationCompletion{}, err
	}
	return completion, nil
}

func validatePublicationReevaluationCompletion(completion PublicationReevaluationCompletion) error {
	if completion.RunID == "" || completion.CommandID == "" || completion.IntentKey == "" ||
		!completion.Outcome.valid() || completion.PRHeadSHA == "" ||
		completion.EvidenceItemID == "" || completion.EvidenceItemVersion < 1 ||
		completion.TerminalInvocationID != PublicationReevaluationTerminalInvocationID(completion.CommandID) {
		return fmt.Errorf("publication reevaluation completion is incomplete: %w",
			domain.ErrParentKeyMismatch)
	}
	return nil
}

// PublicationReevaluationKey binds one reevaluation intent to its run and
// accepted command.
func PublicationReevaluationKey(runID domain.RunID, commandID string) string {
	return publicationReevaluationKeyPrefix + encodedReevaluationRunID(runID) + "/" + commandID
}

// PublicationReevaluationCoordinates inverts PublicationReevaluationKey.
func PublicationReevaluationCoordinates(key string) (domain.RunID, string, bool) {
	rest, ok := strings.CutPrefix(key, publicationReevaluationKeyPrefix)
	if !ok {
		return "", "", false
	}
	encodedRunID, commandID, ok := strings.Cut(rest, "/")
	if !ok || encodedRunID == "" || commandID == "" {
		return "", "", false
	}
	runID, ok := decodeReevaluationRunID(encodedRunID)
	if !ok {
		return "", "", false
	}
	return runID, commandID, true
}

// ReevaluatedBlockedItemID gives each reevaluation attempt a fresh terminal
// item namespace while keeping its accepted command visible in the identity.
func ReevaluatedBlockedItemID(runID domain.RunID, commandID string) domain.ItemID {
	return domain.ItemID(publicationReevaluationItemPrefix +
		encodedReevaluationRunID(runID) + "/" + commandID)
}

// ReevaluatedBlockedItemCoordinates inverts ReevaluatedBlockedItemID.
func ReevaluatedBlockedItemCoordinates(itemID domain.ItemID) (domain.RunID, string, bool) {
	rest, ok := strings.CutPrefix(string(itemID), publicationReevaluationItemPrefix)
	if !ok {
		return "", "", false
	}
	encodedRunID, commandID, ok := strings.Cut(rest, "/")
	if !ok || encodedRunID == "" || commandID == "" {
		return "", "", false
	}
	runID, ok := decodeReevaluationRunID(encodedRunID)
	if !ok {
		return "", "", false
	}
	return runID, commandID, true
}

func encodedReevaluationRunID(runID domain.RunID) string {
	return base64.RawURLEncoding.EncodeToString([]byte(runID))
}

func decodeReevaluationRunID(encoded string) (domain.RunID, bool) {
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil || len(decoded) == 0 || encodedReevaluationRunID(domain.RunID(decoded)) != encoded {
		return "", false
	}
	return domain.RunID(decoded), true
}

// DecodePublicationReevaluationRequest strictly decodes the durable intent.
func DecodePublicationReevaluationRequest(payload []byte) (PublicationReevaluationRequest, error) {
	var request PublicationReevaluationRequest
	if err := strictjson.Decode(payload, &request, strictjson.TolerateInvalidUTF8, strictjson.NoLimit); err != nil {
		if errors.Is(err, strictjson.ErrTrailingData) {
			return PublicationReevaluationRequest{}, fmt.Errorf(
				"publication reevaluation request has trailing content: %w", domain.ErrParentKeyMismatch)
		}
		return PublicationReevaluationRequest{}, fmt.Errorf(
			"decode publication reevaluation request: %w", errors.Join(err, domain.ErrParentKeyMismatch))
	}
	if request.RunID == "" || request.ItemID == "" || request.ItemVersion < 1 ||
		request.CommandID == "" || request.PRHeadSHA == "" ||
		request.TrustProfileDigest == "" || request.ReviewRound < 1 {
		return PublicationReevaluationRequest{}, fmt.Errorf(
			"publication reevaluation request is incomplete: %w", domain.ErrParentKeyMismatch)
	}
	return request, nil
}

// PublicationReevaluationBackupPayloadDigests validates a durable
// reevaluation intent. The request is self-contained and needs no blobs.
func PublicationReevaluationBackupPayloadDigests(entry store.QueueEntry) ([]domain.Digest, error) {
	request, err := DecodePublicationReevaluationRequest(entry.Payload)
	if err != nil {
		return nil, err
	}
	keyRunID, keyCommandID, ok := PublicationReevaluationCoordinates(entry.IdempotencyKey)
	if !ok || entry.Kind != PublicationReevaluationRequestedKind ||
		keyRunID != request.RunID || keyCommandID != request.CommandID {
		return nil, domain.ErrParentKeyMismatch
	}
	return nil, nil
}

// PublicationReevaluationCompletionBackupPayloadDigests validates a terminal
// marker. Completion markers reference rows, not blob content.
func PublicationReevaluationCompletionBackupPayloadDigests(entry store.QueueEntry) ([]domain.Digest, error) {
	completion, err := DecodePublicationReevaluationCompletion(entry.Payload)
	if err != nil {
		return nil, err
	}
	commandID, ok := publicationReevaluationCompletionCommandID(entry.IdempotencyKey)
	if !ok || entry.Kind != PublicationReevaluationCompletedKind || !entry.Dispatched() ||
		commandID != completion.CommandID {
		return nil, domain.ErrParentKeyMismatch
	}
	return nil, nil
}

// AuthenticatedRunConclusion classifies the outcomes that a milestone-only
// conclusion cannot decide on its own: an append-only publication_blocked
// followed by an accepted reevaluation, and a work-unit completion recorded
// over such a history. The accepted command chain is re-read from immutable
// item history and its durable intents before a resolved block becomes either
// live reevaluation or, with separately authenticated ready authority,
// published or completed. Every other outcome keeps domain.ConcludeRun's
// generic result.
func AuthenticatedRunConclusion(
	ctx context.Context,
	tx *store.ReadTx,
	run domain.Run,
	observation domain.RunObservation,
	publicationReadyAuthenticated bool,
) (domain.RunConclusion, error) {
	conclusion := domain.ConcludeRun(observation)
	if conclusion.Outcome == domain.RunOutcomeCompleted {
		return authenticatedCompletedConclusion(ctx, tx, run, observation, publicationReadyAuthenticated)
	}
	if conclusion.Outcome != domain.RunOutcomeBlocked {
		return conclusion, nil
	}
	readyAfterBlock, readyBeforeLastBlock := publicationReadyOrder(observation)
	if readyBeforeLastBlock {
		return domain.RunConclusion{}, fmt.Errorf(
			"run %q records publication_ready before its last publication_blocked: %w",
			run.ID, domain.ErrParentKeyMismatch,
		)
	}
	resolved, err := publicationBlockResolutionAuthenticated(
		ctx, tx, run, observation, readyAfterBlock,
	)
	if errors.Is(err, errPublicationReevaluationLive) {
		return domain.RunConclusion{Outcome: domain.RunOutcomePending}, nil
	}
	if err != nil {
		return domain.RunConclusion{}, err
	}
	if readyAfterBlock {
		if !resolved || !publicationReadyAuthenticated {
			return domain.RunConclusion{}, fmt.Errorf(
				"run %q has unauthenticated ready-after-blocked authority: %w",
				run.ID, domain.ErrParentKeyMismatch,
			)
		}
		return domain.RunConclusion{Outcome: domain.RunOutcomePublished, Final: true}, nil
	}
	if resolved {
		return domain.RunConclusion{Outcome: domain.RunOutcomePending}, nil
	}
	return conclusion, nil
}

// authenticatedCompletedConclusion accepts a completed conclusion only over a
// publication history whose ready authority is authenticated: a timeline with
// no definitive block needs only the ready authentication the observation
// pass already required; one that records a block must have resolved it by an
// accepted rerun with the ready after it, exactly as a published outcome over
// the same history would. A completion over a still-live reevaluation is a
// contradiction and fails closed. It also re-binds every completion milestone
// to its publication invocation and to the store's re-gated completion record,
// so every caller of AuthenticatedRunConclusion fails closed on a milestone
// those authorities no longer support, not only the sync boundary that binds
// it a second time.
func authenticatedCompletedConclusion(
	ctx context.Context,
	tx *store.ReadTx,
	run domain.Run,
	observation domain.RunObservation,
	publicationReadyAuthenticated bool,
) (domain.RunConclusion, error) {
	if !publicationReadyAuthenticated {
		return domain.RunConclusion{}, fmt.Errorf(
			"run %q completed without authenticated publication_ready authority: %w",
			run.ID, domain.ErrParentKeyMismatch,
		)
	}
	// The completion milestone is a powerless mirror, so no caller may derive
	// a final completed outcome from it without its authorities behind it.
	// The sync boundary binds them in authenticateRunObservation, but the
	// direct-store observers (freesided follow and -snapshot) reach this
	// function through AuthenticatedRunConclusion without that pass, and the
	// completion record is re-gated on every read, so a completion the store
	// no longer supports, or one riding another run's publication invocation,
	// would otherwise read as final completed there while the sync reads fail
	// closed over the same rows. Both surfaces go through the same binding.
	attempts := runAttemptBindings(run)
	for _, milestone := range observation.Milestones {
		if milestone.Kind != domain.MilestoneWorkUnitCompleted {
			continue
		}
		if _, err := authenticatedCompletionMilestone(ctx, tx, run, milestone, attempts); err != nil {
			return domain.RunConclusion{}, err
		}
	}
	completed := domain.RunConclusion{Outcome: domain.RunOutcomeCompleted, Final: true}
	if !publicationBlockedMilestone(observation) {
		return completed, nil
	}
	readyAfterBlock, readyBeforeLastBlock := publicationReadyOrder(observation)
	if readyBeforeLastBlock || !readyAfterBlock {
		return domain.RunConclusion{}, fmt.Errorf(
			"run %q completed without publication_ready after its last publication_blocked: %w",
			run.ID, domain.ErrParentKeyMismatch,
		)
	}
	resolved, err := publicationBlockResolutionAuthenticated(ctx, tx, run, observation, readyAfterBlock)
	if errors.Is(err, errPublicationReevaluationLive) {
		return domain.RunConclusion{}, fmt.Errorf(
			"run %q completed while its publication reevaluation is live: %w",
			run.ID, domain.ErrParentKeyMismatch,
		)
	}
	if err != nil {
		return domain.RunConclusion{}, err
	}
	if !resolved {
		return domain.RunConclusion{}, fmt.Errorf(
			"run %q completed over an unresolved publication block: %w",
			run.ID, domain.ErrParentKeyMismatch,
		)
	}
	return completed, nil
}

func publicationBlockedMilestone(observation domain.RunObservation) bool {
	for _, milestone := range observation.Milestones {
		if milestone.Kind == domain.MilestonePublicationBlocked {
			return true
		}
	}
	return false
}

var errPublicationReevaluationLive = errors.New("publication reevaluation remains live")

// publicationBlockResolutionAuthenticated follows the deterministic blocked
// item chain from the original production item through every accepted rerun.
// It reads record-tier item history because mutable recipe revocation cannot
// erase the authority of an already accepted command. A resolved tail with no
// successor is the live reevaluation interval; an authenticated completion
// marker closes it.
func publicationBlockResolutionAuthenticated(
	ctx context.Context,
	tx *store.ReadTx,
	run domain.Run,
	observation domain.RunObservation,
	readyAfterBlock bool,
) (bool, error) {
	itemID := domain.ProductionBlockedItemID(run.ID)
	visited := make(map[domain.ItemID]bool)
	matchedReasons := make(map[domain.RunHoldReason]bool)
	var predecessorRequest PublicationReevaluationRequest
	for {
		if visited[itemID] {
			return false, domain.ErrParentKeyMismatch
		}
		visited[itemID] = true
		item, err := tx.GetAttentionItemRecord(ctx, itemID)
		if errors.Is(err, store.ErrNotFound) {
			if itemID == domain.ProductionBlockedItemID(run.ID) {
				return false, fmt.Errorf("run %q publication block has no durable item: %w",
					run.ID, domain.ErrParentKeyMismatch)
			}
			if predecessorRequest.CommandID != "" {
				completion, found, completionErr := authenticatePublicationReevaluationCompletion(
					ctx, tx, run, predecessorRequest,
				)
				if completionErr != nil {
					return false, completionErr
				}
				if !found {
					if !publicationBlockReasonsMatch(observation, matchedReasons) {
						return false, domain.ErrParentKeyMismatch
					}
					if readyAfterBlock {
						return false, errPublicationReevaluationLive
					}
					return true, nil
				}
				switch completion.Outcome {
				case PublicationReevaluationPublished:
					if !readyAfterBlock {
						return false, domain.ErrParentKeyMismatch
					}
					return true, nil
				case PublicationReevaluationReviewEscalated:
					if readyAfterBlock ||
						!publicationBlockReasonsMatch(observation, matchedReasons) {
						return false, domain.ErrParentKeyMismatch
					}
					return false, nil
				case PublicationReevaluationBlocked:
					return false, fmt.Errorf(
						"run %q completed blocked reevaluation has no durable item: %w",
						run.ID, domain.ErrParentKeyMismatch)
				}
			}
			if !publicationBlockReasonsMatch(observation, matchedReasons) {
				return false, fmt.Errorf("run %q publication block reason has no accepted item: %w",
					run.ID, domain.ErrParentKeyMismatch)
			}
			return true, nil
		}
		if err != nil {
			return false, err
		}
		if item.ProjectID != run.ProjectID || item.Subject.RunID == nil ||
			*item.Subject.RunID != run.ID || item.Subject.Type != domain.SubjectRun ||
			item.Subject.ID != domain.SubjectID(run.ID) ||
			item.Type != domain.AttentionPublishBlocked {
			return false, domain.ErrParentKeyMismatch
		}
		reason, definitive := domain.DefinitivePublicationBlockReason(item.Reason)
		if !definitive {
			if predecessorRequest.CommandID != "" {
				completion, found, completionErr := authenticatePublicationReevaluationCompletion(
					ctx, tx, run, predecessorRequest,
				)
				if completionErr != nil {
					return false, completionErr
				}
				if found {
					if completion.Outcome != PublicationReevaluationPublished ||
						!readyAfterBlock || item.Status != domain.StatusSuperseded ||
						!slices.Equal(item.RequestedDecision,
							[]domain.Action{domain.ActionInspectTrustFailure}) ||
						!publicationBlockReasonsMatch(observation, matchedReasons) {
						return false, domain.ErrParentKeyMismatch
					}
					return true, nil
				}
				if readyAfterBlock {
					if !slices.Equal(item.RequestedDecision,
						[]domain.Action{domain.ActionInspectTrustFailure}) ||
						(item.Status != domain.StatusOpen && item.Status != domain.StatusSuperseded) ||
						!publicationBlockReasonsMatch(observation, matchedReasons) {
						return false, domain.ErrParentKeyMismatch
					}
					return false, errPublicationReevaluationLive
				}
			}
			if !publicationHoldItemAuthenticated(run, observation, item) ||
				!publicationBlockReasonsMatch(observation, matchedReasons) {
				return false, domain.ErrParentKeyMismatch
			}
			return true, nil
		}
		legacyActions := []domain.Action{domain.ActionInspectTrustFailure, domain.ActionStop}
		rerunnableActions := []domain.Action{
			domain.ActionRerunTrustEvaluation, domain.ActionInspectTrustFailure, domain.ActionStop,
		}
		if predecessorRequest.CommandID != "" {
			completion, found, completionErr := authenticatePublicationReevaluationCompletion(
				ctx, tx, run, predecessorRequest,
			)
			if completionErr != nil {
				return false, completionErr
			}
			if !found {
				matchedReasons[reason] = true
				if item.ID != ReevaluatedBlockedItemID(run.ID, predecessorRequest.CommandID) ||
					item.Status != domain.StatusOpen ||
					(!slices.Equal(item.RequestedDecision, legacyActions) &&
						!slices.Equal(item.RequestedDecision, rerunnableActions)) ||
					!publicationBlockReasonsMatch(observation, matchedReasons) {
					return false, domain.ErrParentKeyMismatch
				}
				return false, errPublicationReevaluationLive
			}
			if completion.Outcome != PublicationReevaluationBlocked ||
				item.ID != ReevaluatedBlockedItemID(run.ID, predecessorRequest.CommandID) {
				return false, domain.ErrParentKeyMismatch
			}
			predecessorRequest = PublicationReevaluationRequest{}
		}
		matchedReasons[reason] = true
		if item.Status == domain.StatusOpen {
			if !publicationBlockReasonsMatch(observation, matchedReasons) {
				return false, domain.ErrParentKeyMismatch
			}
			return false, nil
		}
		if !slices.Equal(item.RequestedDecision, legacyActions) &&
			!slices.Equal(item.RequestedDecision, rerunnableActions) {
			return false, domain.ErrParentKeyMismatch
		}
		command, rerun, err := definitiveBlockRerunCommand(ctx, tx, item)
		if err != nil {
			return false, err
		}
		if !rerun {
			if !publicationBlockReasonsMatch(observation, matchedReasons) {
				return false, domain.ErrParentKeyMismatch
			}
			return false, nil
		}
		_, predecessorRequest, err = authenticatePublicationReevaluationIntent(
			ctx, tx, run, item, command,
		)
		if err != nil {
			return false, err
		}
		itemID = ReevaluatedBlockedItemID(run.ID, command.CommandID)
	}
}

func authenticatePublicationReevaluationCompletion(
	ctx context.Context,
	tx *store.ReadTx,
	run domain.Run,
	request PublicationReevaluationRequest,
) (PublicationReevaluationCompletion, bool, error) {
	key := PublicationReevaluationCompletionKey(request.CommandID)
	entry, err := tx.GetOutbox(ctx, key)
	if errors.Is(err, store.ErrNotFound) {
		return PublicationReevaluationCompletion{}, false, nil
	}
	if err != nil {
		return PublicationReevaluationCompletion{}, false, err
	}
	completion, err := DecodePublicationReevaluationCompletion(entry.Payload)
	if err != nil {
		return PublicationReevaluationCompletion{}, false, err
	}
	commandID, keyOK := publicationReevaluationCompletionCommandID(entry.IdempotencyKey)
	if !keyOK || entry.Kind != PublicationReevaluationCompletedKind || !entry.Dispatched() ||
		commandID != request.CommandID || completion.RunID != run.ID ||
		completion.CommandID != request.CommandID ||
		completion.IntentKey != PublicationReevaluationKey(run.ID, request.CommandID) ||
		completion.PRHeadSHA != request.PRHeadSHA {
		return PublicationReevaluationCompletion{}, false, domain.ErrParentKeyMismatch
	}

	command, _, err := tx.GetCommandSnapshot(ctx, request.CommandID)
	if err != nil {
		return PublicationReevaluationCompletion{}, false, err
	}
	sourceItem, err := tx.GetAttentionItemRecord(ctx, request.ItemID)
	if err != nil {
		return PublicationReevaluationCompletion{}, false, err
	}
	if _, authenticatedRequest, err := authenticatePublicationReevaluationIntent(
		ctx, tx, run, sourceItem, command,
	); err != nil || authenticatedRequest != request {
		return PublicationReevaluationCompletion{}, false,
			errors.Join(err, domain.ErrParentKeyMismatch)
	}

	evidenceItem, err := tx.GetAttentionItemRecord(ctx, completion.EvidenceItemID)
	if err != nil {
		return PublicationReevaluationCompletion{}, false, err
	}
	if evidenceItem.ID != completion.EvidenceItemID ||
		evidenceItem.ItemVersion != completion.EvidenceItemVersion ||
		evidenceItem.ProjectID != run.ProjectID || evidenceItem.PRHeadSHA != request.PRHeadSHA ||
		evidenceItem.Subject.Type != domain.SubjectRun ||
		evidenceItem.Subject.ID != domain.SubjectID(run.ID) ||
		evidenceItem.Subject.RunID == nil || *evidenceItem.Subject.RunID != run.ID {
		return PublicationReevaluationCompletion{}, false, domain.ErrParentKeyMismatch
	}
	if evidenceItem.ID != request.ItemID {
		return PublicationReevaluationCompletion{}, false, domain.ErrParentKeyMismatch
	}

	terminalEntry, err := tx.GetInbox(ctx, string(completion.TerminalInvocationID))
	if err != nil {
		return PublicationReevaluationCompletion{}, false, err
	}
	var terminal struct {
		InvocationID domain.InvocationID `json:"invocation_id"`
		RunID        domain.RunID        `json:"run_id"`
		StageID      domain.StageID      `json:"stage_id"`
		Status       string              `json:"status"`
		HeadSHA      string              `json:"head_sha,omitempty"`
		Artifacts    []domain.Digest     `json:"artifacts,omitempty"`
		Summary      string              `json:"summary,omitempty"`
	}
	if terminalEntry.Kind != "production_stage_terminal" ||
		terminalEntry.IdempotencyKey != string(completion.TerminalInvocationID) {
		return PublicationReevaluationCompletion{}, false, domain.ErrParentKeyMismatch
	}
	if err := strictjson.Decode(terminalEntry.Payload, &terminal,
		strictjson.TolerateInvalidUTF8, strictjson.NoLimit); err != nil {
		return PublicationReevaluationCompletion{}, false,
			errors.Join(err, domain.ErrParentKeyMismatch)
	}
	if terminal.InvocationID != completion.TerminalInvocationID ||
		terminal.RunID != run.ID || terminal.Status != "completed" ||
		terminal.HeadSHA != request.PRHeadSHA {
		return PublicationReevaluationCompletion{}, false, domain.ErrParentKeyMismatch
	}
	return completion, true, nil
}

func publicationHoldItemAuthenticated(
	run domain.Run, observation domain.RunObservation, item domain.AttentionItem,
) bool {
	if !slices.Equal(item.RequestedDecision, []domain.Action{domain.ActionInspectTrustFailure}) {
		return false
	}
	switch item.Status {
	case domain.StatusOpen:
		return observation.Hold != nil && observation.Hold.RunID == run.ID &&
			observation.Hold.InvocationID != nil &&
			*observation.Hold.InvocationID == domain.ProductionPublicationInvocationID(run.ID)
	case domain.StatusSuperseded:
		return true
	case domain.StatusResolved, domain.StatusDismissed, domain.StatusExpired:
		return false
	}
	return false
}

func publicationBlockReasonsMatch(
	observation domain.RunObservation,
	matched map[domain.RunHoldReason]bool,
) bool {
	for _, milestone := range observation.Milestones {
		if milestone.Kind == domain.MilestonePublicationBlocked &&
			(milestone.Reason == nil || !matched[*milestone.Reason]) {
			return false
		}
	}
	return true
}

func publicationReadyOrder(observation domain.RunObservation) (bool, bool) {
	lastBlock := -1
	lastReady := -1
	for index, milestone := range observation.Milestones {
		switch milestone.Kind {
		case domain.MilestonePublicationReady:
			lastReady = index
		case domain.MilestonePublicationBlocked:
			lastBlock = index
		case domain.MilestoneRunSubmitted, domain.MilestoneInvocationAdmitted,
			domain.MilestoneInvocationStarted, domain.MilestoneExecutionExportRecorded,
			domain.MilestoneExecutionOutcomeRecorded, domain.MilestoneTerminalRecorded,
			domain.MilestoneWorkUnitCompleted:
		}
	}
	if lastBlock < 0 || lastReady < 0 {
		return false, false
	}
	return lastReady > lastBlock, lastReady < lastBlock
}

func authenticatePublicationReevaluationIntent(
	ctx context.Context,
	tx *store.ReadTx,
	run domain.Run,
	item domain.AttentionItem,
	command domain.Command,
) (bool, PublicationReevaluationRequest, error) {
	key := PublicationReevaluationKey(run.ID, command.CommandID)
	entry, err := tx.GetOutbox(ctx, key)
	if err != nil {
		return false, PublicationReevaluationRequest{}, err
	}
	request, err := DecodePublicationReevaluationRequest(entry.Payload)
	if err != nil {
		return false, PublicationReevaluationRequest{}, err
	}
	keyRunID, keyCommandID, ok := PublicationReevaluationCoordinates(entry.IdempotencyKey)
	if !ok || entry.Kind != PublicationReevaluationRequestedKind || entry.Quarantined() ||
		keyRunID != run.ID || keyCommandID != command.CommandID ||
		request.RunID != run.ID || request.ItemID != item.ID ||
		request.ItemVersion != command.ItemVersion || request.CommandID != command.CommandID ||
		request.PRHeadSHA != command.PRHeadSHA {
		return false, PublicationReevaluationRequest{}, domain.ErrParentKeyMismatch
	}
	project, err := tx.GetProject(ctx, run.ProjectID)
	if err != nil {
		return false, PublicationReevaluationRequest{}, err
	}
	profile, err := tx.GetTrustProfile(ctx, request.TrustProfileDigest)
	if err != nil {
		return false, PublicationReevaluationRequest{}, err
	}
	if project.ID != run.ProjectID || project.RepositoryID != profile.RepositoryID ||
		project.Repo != profile.Repo || request.TrustProfileDigest != profile.ProfileDigest {
		return false, PublicationReevaluationRequest{}, domain.ErrParentKeyMismatch
	}
	return entry.Dispatched(), request, nil
}

func (s *Service) applyRerunTrustEvaluation(
	ctx context.Context, tx *store.WriteTx,
	command domain.Command, item domain.AttentionItem, status domain.ItemStatus,
) error {
	if item.Type != domain.AttentionPublishBlocked || item.Subject.RunID == nil {
		return concludeItem(ctx, tx, item, status, s.now().UTC())
	}
	runID := *item.Subject.RunID
	identityValid := item.ID == domain.ProductionBlockedItemID(runID)
	if !identityValid {
		var err error
		identityValid, err = AuthenticateReevaluatedBlockedItemIdentity(
			ctx, &tx.ReadTx, item.ID, runID, item.ProjectID,
		)
		if err != nil {
			return err
		}
	}
	if !identityValid {
		// The attended fake-publication lane already offered this action as a
		// conclude-only decision. Its consumer is explicitly outside #419, so
		// preserve that behavior until the follow-up wires its own lifecycle.
		return concludeItem(ctx, tx, item, status, s.now().UTC())
	}
	if _, definitive := domain.DefinitivePublicationBlockReason(item.Reason); !definitive ||
		!slices.Equal(item.RequestedDecision, []domain.Action{
			domain.ActionRerunTrustEvaluation, domain.ActionInspectTrustFailure, domain.ActionStop,
		}) {
		return domain.ErrParentKeyMismatch
	}
	project, err := tx.GetProject(ctx, item.ProjectID)
	if err != nil {
		return err
	}
	profile, err := tx.LatestTrustProfile(ctx, project.Repo)
	if err != nil {
		return err
	}
	if profile.RepositoryID != project.RepositoryID {
		return domain.ErrParentKeyMismatch
	}
	// Candidate authorizations are unique per repository, head, and profile.
	// Re-gate before concluding the item so an unchanged profile rolls the
	// command back and leaves the operator a live decision surface.
	authorizations, err := tx.ListCandidateAuthorizations(ctx, project.Repo, item.PRHeadSHA)
	if err != nil {
		return err
	}
	for _, authorization := range authorizations {
		if authorization.TrustProfileDigest == profile.ProfileDigest {
			return fmt.Errorf(
				"trust profile %q already evaluated for %s@%s: %w",
				profile.ProfileDigest, project.Repo, item.PRHeadSHA, domain.ErrDuplicate,
			)
		}
	}
	reviewRound, err := nextPublicationReviewRound(ctx, &tx.ReadTx, runID)
	if err != nil {
		return err
	}
	if err := concludeItem(ctx, tx, item, status, s.now().UTC()); err != nil {
		return err
	}
	request := PublicationReevaluationRequest{
		RunID: runID, ItemID: item.ID, ItemVersion: command.ItemVersion,
		CommandID: command.CommandID, PRHeadSHA: command.PRHeadSHA,
		TrustProfileDigest: profile.ProfileDigest, ReviewRound: reviewRound,
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return err
	}
	key := PublicationReevaluationKey(request.RunID, request.CommandID)
	entry, _, err := tx.EnqueueOutbox(ctx, key, PublicationReevaluationRequestedKind, payload)
	if err != nil {
		return err
	}
	if entry.Kind != PublicationReevaluationRequestedKind || !bytes.Equal(entry.Payload, payload) {
		return fmt.Errorf("publication reevaluation intent %q: %w", key, store.ErrImmutableConflict)
	}
	return nil
}

func nextPublicationReviewRound(
	ctx context.Context, tx *store.ReadTx, runID domain.RunID,
) (int, error) {
	round := 1
	record, err := tx.LatestReviewRecord(ctx, runID)
	if err == nil {
		round = record.Round + 1
	} else if !errors.Is(err, store.ErrNotFound) {
		return 0, err
	}
	failure, err := tx.LatestReviewFailure(ctx, runID)
	if err == nil && failure.Round >= round {
		round = failure.Round + 1
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		return 0, err
	}
	return round, nil
}
