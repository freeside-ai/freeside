package signet

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// Service is the attention service's command-acceptance boundary (§4
// lifecycle, §5.14): it accepts ClientCommands over the store, enforcing the
// checks the store cannot see from a domain.Command alone (the
// expected_entity_version match) and applying the accepted decision's item
// transition in the same transaction. It also owns the device lifecycle
// policy the store's shapes make enforceable: pairing-code minting and
// redemption (pairing.go) and revocation plus the active-device gate on
// command acceptance (device.go).
type Service struct {
	store *store.Store
	// pairingKey is the daemon-held HMAC key pairing-code digests are derived
	// under (domain.PairingCode.CodeHash); it never enters the store. Nil means
	// pairing is unavailable and fails closed; the daemon composition supplies
	// it.
	pairingKey []byte
	// blobs is the digest-addressed attachment store (plan §5.14 "attachments
	// in the artifact store by digest"). Nil means attachments are unavailable
	// and any command or completion referencing one fails closed; the daemon
	// composition supplies it.
	blobs *BlobStore
	// ntfy is the notification channel the delivery pipeline publishes over
	// (delivery.go, ntfy.go). Nil means delivery submission is unavailable and
	// fails closed; the daemon composition supplies it via WithNtfy.
	ntfy *ntfyChannel
	// effectiveReviewConfig reports the daemon's currently effective reviewer
	// configuration digest for the decision-time adoption gate (issue #611,
	// Codex round 4): an adoption whose resolved target does not approve that
	// digest is rejected instead of accepted-then-permanently-ineffective. A
	// nil func or an empty digest means no effective configuration is known
	// (the dev CLI, the fake driver) and the gate is absent; the engine still
	// re-derives effectiveness on every read either way.
	effectiveReviewConfig func() domain.Digest
	now                   func() time.Time
	rand                  io.Reader
	// logger records durable diagnoses the read projections would otherwise
	// swallow into an undifferentiated 500 (issue #767): when a listing read
	// isolates a run whose observation rows contradict their durable authority,
	// it names that run here. Never nil; a discard handler is the default so a
	// composition that supplies none stays silent.
	logger *slog.Logger
}

// Option configures a Service. The clock and randomness sources exist so
// tests can pin expiry and generated identities; production composition
// passes only WithPairingKey.
type Option func(*Service)

// WithPairingKey supplies the daemon-held pairing key. Without it, minting
// and redeeming pairing codes fail closed.
func WithPairingKey(key []byte) Option {
	return func(s *Service) { s.pairingKey = key }
}

// WithClock overrides the time source used for pairing-code lifetimes,
// expiry-at-redemption, and pairing/revocation timestamps.
func WithClock(now func() time.Time) Option {
	return func(s *Service) { s.now = now }
}

// WithRand overrides the randomness source for pairing codes, device IDs,
// and token secrets.
func WithRand(r io.Reader) Option {
	return func(s *Service) { s.rand = r }
}

// WithBlobStore supplies the digest-addressed attachment store. Without it,
// attachment upload/read and any command or agent completion referencing an
// attachment fail closed.
func WithBlobStore(blobs *BlobStore) Option {
	return func(s *Service) { s.blobs = blobs }
}

// WithEffectiveReviewConfiguration supplies the daemon's effective reviewer
// configuration digest for the decision-time adoption gate on
// adopt_review_configuration commands. The func is read at each decision so
// the composition can bind it before the digest is computed.
func WithEffectiveReviewConfiguration(digest func() domain.Digest) Option {
	return func(s *Service) { s.effectiveReviewConfig = digest }
}

// WithLogger supplies the process logger the read projections record a run
// integrity contradiction through (issue #767). A nil logger is ignored so the
// discard default stands; the daemon composition threads its process logger in.
func WithLogger(logger *slog.Logger) Option {
	return func(s *Service) {
		if logger == nil {
			return
		}
		s.logger = logger.With("subsystem", "signet")
	}
}

func NewService(st *store.Store, opts ...Option) *Service {
	s := &Service{store: st, now: time.Now, rand: rand.Reader, logger: slog.New(slog.DiscardHandler)}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// PutItem creates or advances an AttentionItem through the signet boundary.
// Domain validation runs first, then the per-type action policy, before any
// Write begins; a rejected item cannot consume a server revision. The store
// remains responsible for transition, evidence-policy, and persistence gates.
func (s *Service) PutItem(ctx context.Context, item domain.AttentionItem) error {
	if err := item.Validate(); err != nil {
		return fmt.Errorf("put item %q: %w", item.ID, err)
	}
	// The decision instant is stamped only by Submit's concluding transaction
	// (#171): item intake never carries one, so the boundary fails closed
	// instead of trusting the caller's copy (see ErrCallerSetDecidedAt).
	if item.DecidedAt != nil {
		return fmt.Errorf("put item %q: %w", item.ID, ErrCallerSetDecidedAt)
	}
	if err := validateRequestedActions(item.Type, item.RequestedDecision); err != nil {
		return fmt.Errorf("put item %q: %w", item.ID, err)
	}
	// A run proposal is authoritative only when Engine.AdmitProposal creates
	// its instance, evidence carrier, item, and immutable item binding in one
	// transaction. Generic intake has none of that trusted admission context.
	if item.Type == domain.AttentionRunProposal {
		return fmt.Errorf("put item %q: %w", item.ID, ErrProposalAdmissionRequired)
	}
	return s.store.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutAttentionItem(ctx, item)
	})
}

// errReplay abandons the Write transaction of an idempotent retry after the
// original result has been captured: nothing was written, so rolling back
// keeps a replay from bumping the server revision (§5.14 test 4: the
// original committed result, no second effect).
var errReplay = errors.New("idempotent replay: original result captured")

// Submit accepts one ClientCommand and returns its committed result. The
// checks run in the store's documented acceptance order: idempotency by
// CommandID first (a retry converges on the original result regardless of the
// item's current state), then openness (issue #55; a concluded lifecycle is
// the more fundamental rejection than staleness), then version and binding
// authority, then the action-offered gate delegated to store.PutCommand.
// An accepted decision's item transition commits atomically with the command:
// one Write, one revision, no window where the command exists without its
// resolution.
func (s *Service) Submit(ctx context.Context, in ClientCommand) (CommandResult, error) {
	if err := s.convergeProposalSnoozes(ctx, s.now().UTC()); err != nil {
		return CommandResult{}, fmt.Errorf("submit command %q proposal snoozes: %w", in.CommandID, err)
	}
	newCommand := func(message string) (domain.Command, error) {
		return domain.NewCommand(domain.CommandInput{
			CommandID: in.CommandID, DeviceID: in.DeviceID, ItemID: in.Payload.ItemID,
			ItemVersion: in.Payload.ItemVersion, PRHeadSHA: in.Payload.PRHeadSHA,
			ArtifactDigests: in.Payload.ArtifactDigests, Action: in.Payload.Action,
			Message: message, Attachments: in.Payload.Attachments,
		})
	}
	// Build the structural command first, without interpreting parameterized
	// action content. Durable item policy and command-id replay have precedence
	// over content policy; the canonical typed message is overlaid inside the
	// appropriate branch below.
	command, err := newCommand(in.Payload.Message)
	if err != nil {
		return CommandResult{}, fmt.Errorf("submit command %q: %w", in.CommandID, err)
	}
	if in.ExpectedEntityVersion < 1 {
		return CommandResult{}, fmt.Errorf("submit command %q: expected_entity_version %d: %w",
			in.CommandID, in.ExpectedEntityVersion, domain.ErrNonPositive)
	}

	var result CommandResult
	err = s.store.Write(ctx, func(tx *store.WriteTx) error {
		_, _, getErr := tx.GetCommandSnapshot(ctx, command.CommandID)
		if getErr != nil && !errors.Is(getErr, store.ErrNotFound) {
			return getErr
		}
		replay := getErr == nil
		if !replay {
			// The active-device gate (§5.14 test 15) runs only for a genuinely
			// new command_id, before any item-carrying rejection can leak state
			// to a revoked device. A replay skips it by design (test 16): the
			// replay branch never writes, so returning the recorded result to a
			// now-revoked device produces no new side effect. Reading the device
			// inside the accepting transaction closes the gap between the HTTP
			// authorizer's check and the commit.
			if err := gateActiveDevice(ctx, tx, command.DeviceID); err != nil {
				return fmt.Errorf("submit command %q: %w", command.CommandID, err)
			}
			item, snap, err := tx.GetAttentionItemSnapshot(ctx, command.ItemID)
			if err != nil {
				return fmt.Errorf("submit command %q: %w", command.CommandID, err)
			}
			// Re-run current signet policy against the durable row. PutItem gates
			// new writes, but a pre-policy row or an internal direct-store write
			// must not remain an authority for accepting a now-illegitimate action.
			if err := validateRequestedActions(item.Type, item.RequestedDecision); err != nil {
				return fmt.Errorf("submit command %q: item %q: %w", command.CommandID, item.ID, err)
			}
			if snoozed, err := proposalSnoozed(ctx, tx, item, s.now().UTC()); err != nil {
				return fmt.Errorf("submit command %q: %w", command.CommandID, err)
			} else if snoozed {
				return fmt.Errorf("submit command %q: %w", command.CommandID, ErrProposalSnoozed)
			}
			// The pending-action gate runs only for a genuinely new command_id,
			// after replay and durable-item policy have been judged: an id already
			// on record keeps the command-id-first contract, while an invalid legacy
			// item fails closed before its action's not-yet-implemented effect is
			// considered.
			if _, kind := actionOutcome(command.Action); kind == outcomePending {
				return fmt.Errorf("submit command %q: action %q: %w",
					command.CommandID, command.Action, ErrUnsupportedAction)
			}
			message, err := decisionMessage(in.Payload)
			if err != nil {
				return fmt.Errorf("submit command %q: %w", in.CommandID, err)
			}
			command, err = newCommand(message)
			if err != nil {
				return fmt.Errorf("submit command %q: %w", in.CommandID, err)
			}
			// The per-action content policy sits with the pending gate, inside
			// the new-command branch: a committed command_id retried with
			// malformed content still gets command-id-first idempotent
			// judgment (the #65 ordering), and an error here rolls the Write
			// back, so no revision is consumed.
			if err := s.validateCommandContent(command); err != nil {
				return fmt.Errorf("submit command %q: %w", command.CommandID, err)
			}
			if item.Status != domain.StatusOpen {
				return fmt.Errorf("submit command %q: %w", command.CommandID,
					&ClosedItemError{CommandID: command.CommandID, Item: item, Snapshot: snap})
			}
			if snap.EntityVersion != in.ExpectedEntityVersion {
				return fmt.Errorf("submit command %q: %w", command.CommandID,
					&StaleVersionError{CommandID: command.CommandID, Replacement: item, Snapshot: snap})
			}
			// PutCommand re-gates openness and checks the payload's binding
			// authority and the action-offered set. Submit has already re-gated the
			// durable item's offered set against current per-type signet policy.
			if err := tx.PutCommand(ctx, command); err != nil {
				return translateRejection(err, snap)
			}
			switch status, kind := actionOutcome(command.Action); kind {
			case outcomeConcludes:
				// The stamp-and-flip semantics live in concludeItem, shared
				// with the operating-state transactions below.
				if err := concludeItem(ctx, tx, item, status, s.now().UTC()); err != nil {
					return fmt.Errorf("submit command %q: %w", command.CommandID, err)
				}
			case outcomeDiscusses:
				if err := s.applyDiscuss(ctx, tx, command, item, snap); err != nil {
					return fmt.Errorf("submit command %q: %w", command.CommandID, err)
				}
			case outcomeStopsUnattended:
				if err := s.applyStopUnattended(ctx, tx, command, item, status); err != nil {
					return fmt.Errorf("submit command %q: %w", command.CommandID, err)
				}
			case outcomeResumesUnattended:
				if err := s.applyResumeUnattended(ctx, tx, command, item, status); err != nil {
					return fmt.Errorf("submit command %q: %w", command.CommandID, err)
				}
			case outcomeRecoversReview:
				if err := s.applyReviewRecovery(ctx, tx, command, item, status); err != nil {
					return fmt.Errorf("submit command %q: %w", command.CommandID, err)
				}
			case outcomeRerunsTrustEvaluation:
				if err := s.applyRerunTrustEvaluation(ctx, tx, command, item, status); err != nil {
					return fmt.Errorf("submit command %q: %w", command.CommandID, err)
				}
			case outcomeAdoptsReviewConfiguration:
				if err := s.applyReviewConfigurationRecovery(ctx, tx, command, item, status); err != nil {
					return fmt.Errorf("submit command %q: %w", command.CommandID, err)
				}
			case outcomeResolvesReenrollment:
				if err := s.applyCodexReenrollmentRecovery(ctx, tx, command, item, status); err != nil {
					return fmt.Errorf("submit command %q: %w", command.CommandID, err)
				}
			case outcomeAcceptsRecommendedRoute, outcomeChoosesAlternativeRoute:
				if err := s.applyFindingAdjudicationDecision(ctx, tx, command, item, status); err != nil {
					return fmt.Errorf("submit command %q: %w", command.CommandID, err)
				}
			case outcomeStartsProposal:
				if err := s.applyStartProposal(ctx, tx, command, item, s.now().UTC()); err != nil {
					return fmt.Errorf("submit command %q: %w", command.CommandID, err)
				}
			case outcomeRevisesAndStartsProposal:
				if err := s.applyStartProposalWithChanges(ctx, tx, command, item, s.now().UTC()); err != nil {
					return fmt.Errorf("submit command %q: %w", command.CommandID, err)
				}
			case outcomeDeclinesProposal:
				if err := s.applyDeclineProposal(ctx, tx, command, item, s.now().UTC()); err != nil {
					return fmt.Errorf("submit command %q: %w", command.CommandID, err)
				}
			case outcomeSnoozesProposal:
				if err := s.applySnoozeProposal(ctx, tx, command, item, s.now().UTC()); err != nil {
					return fmt.Errorf("submit command %q: %w", command.CommandID, err)
				}
			case outcomeRecords, outcomePending:
				// Records: the command record is the whole effect. Pending:
				// unreachable, rejected above before PutCommand.
			}
		} else {
			// A valid typed retry must reconstruct the same canonical durable
			// message. If typed decoding fails, retain the structural command:
			// PutCommand will still surface an immutable conflict for a changed
			// body under the occupied id, preserving command-id-first judgment.
			if message, messageErr := decisionMessage(in.Payload); messageErr == nil {
				if canonical, commandErr := newCommand(message); commandErr == nil {
					command = canonical
				}
			}
			// A byte-identical replay is a no-op inside PutCommand; a changed
			// body under the same id surfaces its ErrImmutableConflict here,
			// before any item check, so the item-carrying rejections cannot
			// occur on this path and no translation is needed.
			if err := tx.PutCommand(ctx, command); err != nil {
				return err
			}
		}
		record, snap, err := tx.GetCommandSnapshot(ctx, command.CommandID)
		if err != nil {
			return err
		}
		result = CommandResult{Record: record, Revision: snap.AsOfRevision}
		if replay {
			return errReplay
		}
		return nil
	})
	if err != nil && !errors.Is(err, errReplay) {
		return CommandResult{}, err
	}
	return result, nil
}

// outcomeKind classifies what accepting an action does beyond recording the
// command itself.
type outcomeKind int

const (
	// outcomeConcludes: the decision itself concludes the item; Submit applies
	// the status transition in the accepting transaction.
	outcomeConcludes outcomeKind = iota
	// outcomeRecords: the command record is the whole server-side effect; the
	// item is left untouched.
	outcomeRecords
	// outcomeDiscusses: the discuss transaction (plan §5.14): append the
	// user's message, supersede the item version, record the invocation, and
	// commit the AgentInvocationRequested outbox intent, all in the accepting
	// transaction (applyDiscuss).
	outcomeDiscusses
	// outcomePending: the action's accepted effect is a transaction a later
	// unit owns; Submit rejects it with ErrUnsupportedAction rather than
	// record a command whose effect would be silently dropped.
	outcomePending
	// outcomeStopsUnattended: the operator stop transaction (plan §4
	// stop_unattended; issue #319): conclude the decided item, append the
	// durable stopped transition, and raise the resume-offering notice, all
	// in the accepting transaction (applyStopUnattended).
	outcomeStopsUnattended
	// outcomeResumesUnattended: the explicit resume transaction (issue #319):
	// conclude the stopped notice and append the resumed transition in the
	// accepting transaction (applyResumeUnattended). Only this operator path
	// writes "resumed"; a restart alone never does.
	outcomeResumesUnattended
	// outcomeRecoversReview: conclude the contradiction item and append the
	// exact-row-bound recovery transition in the accepting transaction.
	outcomeRecoversReview
	// outcomeRerunsTrustEvaluation: conclude the definitive publication block
	// and enqueue the exact-command-bound reevaluation intent in the accepting
	// transaction.
	outcomeRerunsTrustEvaluation
	// outcomeAdoptsReviewConfiguration: conclude the review_configuration item
	// and append the exact-row, profile-supersession-bound configuration
	// recovery transition in the accepting transaction.
	outcomeAdoptsReviewConfiguration
	// outcomeResolvesReenrollment: conclude the revoked-identity marker and
	// append its latest-verified-operation-bound recovery transition.
	outcomeResolvesReenrollment
	// Finding-adjudication outcomes validate the typed choice against the
	// displayed binding before concluding the item.
	outcomeAcceptsRecommendedRoute
	outcomeChoosesAlternativeRoute
	// Proposal outcomes own the instance ledger and exact proposal digest.
	outcomeStartsProposal
	outcomeRevisesAndStartsProposal
	outcomeDeclinesProposal
	outcomeSnoozesProposal
)

// actionOutcome maps an action to what its acceptance does, following plan
// §4. Concluding actions are the parameterless decisions whose whole accepted
// effect is the status flip plus the durable record (downstream reactions are
// the Wave 2 engine's, the issue's own deferral). Record-only actions have no
// item effect by design: open_pr is navigation, not resolution; acknowledge
// means seen, never resolved; mark_seen decides nothing; inspect_trust_failure
// is navigation; run_doctor leaves a system_health item blocking until the
// diagnostic clears. Stop/resume of unattended operation conclude the decided
// item and append the durable operating transition in the accepting
// transaction (issue #319; applyStopUnattended, applyResumeUnattended).
// Recover review similarly concludes its carrier and appends the command-backed
// exact-row transition (issue #580; applyReviewRecovery). Rerun trust
// evaluation concludes a definitive publication block and enqueues the
// command-bound reevaluation intent (issue #419; applyRerunTrustEvaluation).
// Discuss runs the conversation transaction (plan §5.14
// discuss semantics; applyDiscuss). Pending actions are rejected before any
// transaction because their accepted effect cannot be represented yet: snooze
// needs the timing update; start_with_changes needs the revised proposal
// artifact and supersede transaction (plan §4); convert_to_policy,
// adjudicate, retry_with_capabilities, and
// choose_alternate_profile carry decision parameters DecisionPayload has no
// field for (a #22 contract widening when their consumers land); and
// answer_and_retry, answer_without_retry, and return_to_agent ride the conversation channel but are decisions about a
// prior agent turn, whose accepted effect (what the workflow does with the
// answer) is the Wave 2 engine's, not a plain discuss append. Recording any
// of them today would silently drop the user's data. A behaviour switch, no
// default, so a new Action member must declare its outcome here.
func actionOutcome(action domain.Action) (domain.ItemStatus, outcomeKind) {
	switch action {
	case domain.ActionDismiss:
		return domain.StatusDismissed, outcomeConcludes
	case domain.ActionApprove, domain.ActionStop, domain.ActionFinishNow,
		domain.ActionApplyThenFinish, domain.ActionContinueUnderPolicy,
		domain.ActionRetry:
		return domain.StatusResolved, outcomeConcludes
	case domain.ActionRerunTrustEvaluation:
		return domain.StatusResolved, outcomeRerunsTrustEvaluation
	case domain.ActionRequestChanges:
		return domain.StatusSuperseded, outcomeConcludes
	case domain.ActionStopUnattended:
		return domain.StatusResolved, outcomeStopsUnattended
	case domain.ActionResumeUnattended:
		return domain.StatusResolved, outcomeResumesUnattended
	case domain.ActionRecoverReview:
		return domain.StatusResolved, outcomeRecoversReview
	case domain.ActionAdoptReviewConfiguration:
		return domain.StatusResolved, outcomeAdoptsReviewConfiguration
	case domain.ActionResolveReenrollment:
		return domain.StatusResolved, outcomeResolvesReenrollment
	case domain.ActionAcceptRecommendedRoute:
		return domain.StatusResolved, outcomeAcceptsRecommendedRoute
	case domain.ActionChooseAlternativeRoute:
		return domain.StatusResolved, outcomeChoosesAlternativeRoute
	case domain.ActionStart:
		return domain.StatusResolved, outcomeStartsProposal
	case domain.ActionStartWithChanges:
		return domain.StatusResolved, outcomeRevisesAndStartsProposal
	case domain.ActionDecline:
		return domain.StatusDismissed, outcomeDeclinesProposal
	case domain.ActionSnooze:
		return "", outcomeSnoozesProposal
	case domain.ActionOpenPR, domain.ActionMarkSeen, domain.ActionAcknowledge,
		domain.ActionInspectTrustFailure, domain.ActionRunDoctor:
		return "", outcomeRecords
	case domain.ActionDiscuss:
		return "", outcomeDiscusses
	case domain.ActionConvertToPolicy, domain.ActionAdjudicate,
		domain.ActionRetryWithCapability,
		domain.ActionChooseAlternate,
		domain.ActionAnswerAndRetry, domain.ActionAnswerWithoutRetry,
		domain.ActionReturnToAgent:
		return "", outcomePending
	}
	// Invalid zero value: unreachable past NewCommand's validation and
	// PutCommand's offered-action gate.
	return "", outcomeRecords
}
