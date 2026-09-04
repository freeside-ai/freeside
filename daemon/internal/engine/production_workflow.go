package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

// Production workflow lane (#237, plan §5.12): a run created by `freesided
// submit` from an operator-approved, digest-addressed specification, executed
// unattended by the real stage driver. The lane shares the store, admission,
// and driver contracts with the 1A.0 walking skeleton but binds its
// invocation to input artifacts alone (ConversationID nil, the shape
// domain.AgentInvocation reserves for exactly this), so no conversation or
// attention item participates in dispatch.
//
// Ownership marker: a Run carries no workflow-kind discriminator, so each
// lane needs its own concrete marker (see reconcileRuns). The production
// marker is the deterministic invocation-request outbox row committed in the
// same transaction as the run: its idempotency key is the lane's derived
// invocation id and its kind is KindProductionInvocationRequested, so a
// foreign run can never acquire one retroactively and a submit replay
// converges on it.

// KindProductionInvocationRequested is the outbox kind for the production
// lane's dispatch intent. Exported with its backup-payload extractor so the
// daemon composition can register it (store.BackupPayloadDigestExtractor).
const KindProductionInvocationRequested = string(domain.ProductionInvocationRequestedKind)

// kindProductionStageTerminal is the inbox kind recording the engine's
// at-most-once acceptance of a production stage's terminal outcome.
const kindProductionStageTerminal = "production_stage_terminal"

// productionStageName is the single Phase 1A.2 implement stage (§11 1A.2).
const productionStageName = "implement"

// productionSpecificationArtifactType is the exact role registered by
// freesided submit for the one invocation-owned production input.
const productionSpecificationArtifactType = domain.ArtifactKindSpecification

// productionPolicyArtifactType is the exact role registered by freesided
// submit for the canonical resolved-policy bytes.
const productionPolicyArtifactType = domain.ArtifactKindPolicy

const (
	maxProductionPublicationTitleBytes = 256
	// productionInvocationVersionNamespace is the marker version's namespace,
	// and productionInvocationRequestVersionNumber the release this binary
	// implements. They compose productionInvocationRequestVersion, and a test
	// pins that composition so the three cannot drift apart.
	productionInvocationVersionNamespace     = "freeside.production-invocation/v"
	productionInvocationRequestVersionNumber = 2
	productionInvocationRequestVersion       = "freeside.production-invocation/v2"
)

var ErrImplementationRunReserved = errors.New("implementation run is reserved by specification")

func productionStageID(runID domain.RunID) domain.StageID {
	return domain.StageID("implement-" + string(runID))
}

const productionInvocationIDPrefix = "inv-implement-"

func productionInvocationID(runID domain.RunID) domain.InvocationID {
	return domain.InvocationID(productionInvocationIDPrefix + string(runID))
}

// productionRunIDFromInvocationID inverts productionInvocationID, reporting
// false for a key this lane could not have derived. It attributes a marker row
// to a run by the key the row is filed under, never by its payload: the
// payload is exactly what has failed to reconstruct at the one call site.
func productionRunIDFromInvocationID(id domain.InvocationID) (domain.RunID, bool) {
	runID, ok := strings.CutPrefix(string(id), productionInvocationIDPrefix)
	if !ok || runID == "" {
		return "", false
	}
	return domain.RunID(runID), true
}

func productionVerificationInvocationID(runID domain.RunID) domain.InvocationID {
	return domain.InvocationID("verify-production-" + string(runID))
}

func productionVerificationInvocationIDForProducer(
	runID domain.RunID, producer domain.InvocationID,
) domain.InvocationID {
	if producer == productionInvocationID(runID) {
		return productionVerificationInvocationID(runID)
	}
	if round, ok := remediationRoundForInvocation(runID, producer); ok {
		return domain.InvocationID(fmt.Sprintf("verify-remediation-%d-%s", round, runID))
	}
	if strings.HasPrefix(string(producer), "inv-operator-feedback-") {
		return domain.InvocationID("verify-" + string(producer))
	}
	return ""
}

// productionInvocationRequest is the outbox payload for one production
// dispatch intent. Unlike the conversation lane's request it carries the run
// and stage directly: there is no attention item to resolve them through.
type productionInvocationRequest struct {
	Version      string                `json:"version"`
	InvocationID domain.InvocationID   `json:"invocation_id"`
	RunID        domain.RunID          `json:"run_id"`
	StageID      domain.StageID        `json:"stage_id"`
	Publication  ProductionPublication `json:"publication"`
	Legacy       bool                  `json:"-"`
}

type productionInvocationRequestWire struct {
	Version      json.RawMessage     `json:"version,omitempty"`
	InvocationID domain.InvocationID `json:"invocation_id"`
	RunID        domain.RunID        `json:"run_id"`
	StageID      domain.StageID      `json:"stage_id"`
	Publication  json.RawMessage     `json:"publication,omitempty"`
}

// ProductionPublication carries operator-authored, reviewer-facing pull-request
// content plus claimed public commit attribution. It is deliberately separate
// from the agent specification and driver summary: neither is a safe public-
// content boundary. Production composition verifies CommitAuthor against the
// App registration selected by the repository token before execution/import.
type ProductionPublication struct {
	Title        string                 `json:"title"`
	Body         string                 `json:"body"`
	CommitAuthor ProductionCommitAuthor `json:"commit_author"`
}

// ProductionCommitAuthor is the claimed public GitHub App bot identity used
// for the daemon-authored import commit. Bot user ID and slug are attribution
// metadata, not publication authority; the selected App installation remains
// the token and forge authority and must authenticate this claim.
type ProductionCommitAuthor struct {
	AppSlug   string `json:"app_slug"`
	BotUserID int64  `json:"bot_user_id"`
}

// Name is GitHub's canonical App bot login used as the Git author name.
func (a ProductionCommitAuthor) Name() string { return a.AppSlug + "[bot]" }

// Email is GitHub's canonical App bot no-reply address, which associates the
// import commit with the App account and its avatar.
func (a ProductionCommitAuthor) Email() string {
	return fmt.Sprintf("%d+%s@users.noreply.github.com", a.BotUserID, a.Name())
}

func (a ProductionCommitAuthor) validate() error {
	if a.BotUserID <= 0 || a.AppSlug == "" || len(a.AppSlug) > 34 {
		return errors.New("production commit author requires a canonical App slug and positive bot user id")
	}
	for index, char := range a.AppSlug {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') ||
			(char == '-' && index > 0 && index < len(a.AppSlug)-1) {
			continue
		}
		return fmt.Errorf("production commit author App slug %q is invalid", a.AppSlug)
	}
	return nil
}

// Validate rejects metadata that cannot be safely persisted as one GitHub
// title and body. The body limit reserves every publisher-owned section,
// including the identity marker, advisories, and disposition history.
func (p ProductionPublication) Validate() error {
	if !utf8.ValidString(p.Title) || !utf8.ValidString(p.Body) {
		return errors.New("production publication metadata is not valid UTF-8")
	}
	if p.Title == "" || p.Title != strings.TrimSpace(p.Title) ||
		strings.ContainsAny(p.Title, "\r\n") {
		return errors.New("production publication title must be one non-empty trimmed line")
	}
	if len(p.Title) > maxProductionPublicationTitleBytes {
		return fmt.Errorf("production publication title exceeds %d bytes", maxProductionPublicationTitleBytes)
	}
	if strings.TrimSpace(p.Body) == "" {
		return errors.New("production publication body must be non-empty")
	}
	if err := publish.ValidateCandidateBody(p.Body); err != nil {
		return fmt.Errorf("production publication body: %w", err)
	}
	if err := p.CommitAuthor.validate(); err != nil {
		return err
	}
	return nil
}

// ProductionRunSpec is the fixed input `freesided submit` registers: the
// operator-approved specification, resolved policy, and reviewer-facing
// publication metadata (§5.12).
type ProductionRunSpec struct {
	RunID     domain.RunID
	ProjectID domain.ProjectID
	// SpecArtifactID and PolicyArtifactID name the pre-registered artifacts
	// whose digests become the run's SpecDigest and PolicyDigest. Submission
	// verifies the registration rather than trusting caller-supplied digests.
	SpecArtifactID domain.ArtifactID
	// PolicyArtifactID addresses the canonical ResolvedPolicy.Keys bytes;
	// ResolvedPolicy is persisted atomically with the run.
	PolicyArtifactID domain.ArtifactID
	ResolvedPolicy   domain.ResolvedPolicy
	Publication      ProductionPublication
	// WorkUnit, when present, is the operator's explicit §5.18 work-unit
	// declaration, captured verbatim in the submission transaction; nil
	// submits an undeclared run, which records nothing (capture stores
	// explicit declarations only).
	WorkUnit      *domain.WorkUnitDeclarationInput
	CampaignID    domain.CampaignID
	AttemptNumber int
	AttemptReason string
	ParentRunID   domain.RunID
}

// ProductionRun reports the durable identities one submission converges on.
type ProductionRun struct {
	Run          domain.Run
	InvocationID domain.InvocationID
	StageID      domain.StageID
}

// SubmitProductionRun idempotently persists one production run with its
// implement stage, its artifact-bound invocation, and the dispatch intent the
// engine's reconcile loop picks up, all in one transaction. It is a package
// function over the store, not an Engine method: submission needs no driver
// or attention composition, so `freesided submit` can call it in-process.
//
// A replay converges on the stored state; a retry whose fixed bindings
// disagree with the stored run fails rather than retargeting it.
func SubmitProductionRun(ctx context.Context, st *store.Store, spec ProductionRunSpec) (ProductionRun, error) {
	return submitProductionRun(ctx, st, spec, nil)
}

// submitProductionRun is the single production intake transaction. Only the
// authenticated specification approval path can supply a reservation grant;
// external callers use SubmitProductionRun and therefore cannot bypass a
// pre-approval implementation claim.
func submitProductionRun(
	ctx context.Context,
	st *store.Store,
	spec ProductionRunSpec,
	specificationGrant *specificationRequest,
) (ProductionRun, error) {
	if st == nil {
		return ProductionRun{}, errors.New("submit production run: nil store")
	}
	if spec.RunID == "" || spec.ProjectID == "" {
		return ProductionRun{}, fmt.Errorf("submit production run: run and project ids are required: %w", domain.ErrEmptyID)
	}
	if spec.SpecArtifactID == "" || spec.PolicyArtifactID == "" {
		return ProductionRun{}, fmt.Errorf("submit production run %q: spec and policy artifact ids are required: %w",
			spec.RunID, domain.ErrEmptyID)
	}
	if err := spec.Publication.Validate(); err != nil {
		return ProductionRun{}, fmt.Errorf("submit production run %q: %w", spec.RunID, err)
	}
	if err := spec.ResolvedPolicy.Validate(); err != nil {
		return ProductionRun{}, fmt.Errorf("submit production run %q resolved policy: %w", spec.RunID, err)
	}
	if spec.ResolvedPolicy.RunID != spec.RunID {
		return ProductionRun{}, fmt.Errorf(
			"submit production run %q resolved policy names run %q: %w",
			spec.RunID, spec.ResolvedPolicy.RunID, domain.ErrParentKeyMismatch)
	}

	stageID := productionStageID(spec.RunID)
	invocationID := productionInvocationID(spec.RunID)
	payload, err := json.Marshal(productionInvocationRequest{
		Version:      productionInvocationRequestVersion,
		InvocationID: invocationID, RunID: spec.RunID, StageID: stageID,
		Publication: spec.Publication,
	})
	if err != nil {
		return ProductionRun{}, fmt.Errorf("submit production run %q: %w", spec.RunID, err)
	}
	invocation, err := domain.NewAgentInvocation(
		invocationID, []domain.ArtifactID{spec.SpecArtifactID}, nil, 0,
	)
	if err != nil {
		return ProductionRun{}, fmt.Errorf("submit production run %q: %w", spec.RunID, err)
	}
	publicationReservation, err := publish.NewReservation(
		domain.ProductionPublicationInvocationID(spec.RunID), spec.RunID,
	)
	if err != nil {
		return ProductionRun{}, fmt.Errorf("submit production run %q publication reservation: %w", spec.RunID, err)
	}

	var (
		run        domain.Run
		runCreated bool
	)
	err = st.Write(ctx, func(tx *store.WriteTx) error {
		if err := authorizeProductionSubmission(ctx, tx, spec, specificationGrant); err != nil {
			return err
		}
		// The digests come from the registered artifacts, not the caller: a
		// submission for bytes the store does not hold is refused, and the
		// run's trusted configuration is bound to what was actually
		// registered (§5.8).
		specArtifact, err := tx.GetArtifact(ctx, spec.SpecArtifactID)
		if err != nil {
			return fmt.Errorf("specification artifact %q: %w", spec.SpecArtifactID, err)
		}
		policyArtifact, err := tx.GetArtifact(ctx, spec.PolicyArtifactID)
		if err != nil {
			return fmt.Errorf("policy artifact %q: %w", spec.PolicyArtifactID, err)
		}
		if specArtifact.Type != productionSpecificationArtifactType {
			return fmt.Errorf(
				"specification artifact %q has type %q: %w",
				spec.SpecArtifactID, specArtifact.Type, domain.ErrParentKeyMismatch,
			)
		}
		if policyArtifact.Type != productionPolicyArtifactType {
			return fmt.Errorf(
				"policy artifact %q has type %q: %w",
				spec.PolicyArtifactID, policyArtifact.Type, domain.ErrParentKeyMismatch,
			)
		}
		if policyArtifact.Digest != spec.ResolvedPolicy.Digest {
			return fmt.Errorf(
				"policy artifact %q digest %q disagrees with resolved policy %q: %w",
				spec.PolicyArtifactID, policyArtifact.Digest,
				spec.ResolvedPolicy.Digest, domain.ErrParentKeyMismatch)
		}

		want := domain.Run{
			ID: spec.RunID, ProjectID: spec.ProjectID,
			SpecDigest: specArtifact.Digest, PolicyDigest: policyArtifact.Digest,
			CampaignID: spec.CampaignID, AttemptNumber: spec.AttemptNumber,
			AttemptReason: spec.AttemptReason, ParentRunID: spec.ParentRunID,
			Stages: []domain.Stage{{
				ID: stageID, RunID: spec.RunID,
				Name: productionStageName, Attempts: []domain.Attempt{},
			}},
		}
		if err := want.Validate(); err != nil {
			return err
		}
		if err := authenticateProductionAttempt(ctx, tx, spec, specArtifact.Digest, specificationGrant); err != nil {
			return err
		}

		existing, err := tx.GetRun(ctx, spec.RunID)
		switch {
		case err == nil:
			if existing.ProjectID != want.ProjectID || existing.SpecDigest != want.SpecDigest ||
				existing.PolicyDigest != want.PolicyDigest || existing.CampaignID != want.CampaignID ||
				existing.AttemptNumber != want.AttemptNumber || existing.AttemptReason != want.AttemptReason ||
				existing.ParentRunID != want.ParentRunID {
				return fmt.Errorf("fixed bindings disagree with stored run: %w", domain.ErrImmutableTransition)
			}
			if _, ok := findProductionStage(existing); !ok {
				return fmt.Errorf("stored run has no %q stage: %w", productionStageName, domain.ErrParentKeyMismatch)
			}
			// The marker is the production lane's ownership fact and must
			// already be part of an existing run's atomic creation. Creating
			// it here would let an explicit --run-id retroactively claim a
			// structurally compatible run from another workflow.
			entry, markerErr := tx.GetOutbox(ctx, string(invocationID))
			if markerErr != nil {
				if errors.Is(markerErr, store.ErrNotFound) {
					return fmt.Errorf(
						"stored run has no production ownership marker: %w",
						domain.ErrImmutableTransition,
					)
				}
				return markerErr
			}
			if entry.Kind != KindProductionInvocationRequested ||
				!bytes.Equal(entry.Payload, payload) {
				return fmt.Errorf(
					"stored run has a foreign production marker: %w",
					domain.ErrImmutableTransition,
				)
			}
			storedPolicy, policyErr := tx.GetResolvedPolicy(ctx, spec.RunID)
			if policyErr != nil {
				if errors.Is(policyErr, store.ErrNotFound) {
					return fmt.Errorf(
						"stored production run has no resolved policy: %w",
						domain.ErrImmutableTransition,
					)
				}
				return policyErr
			}
			if storedPolicy.RunID != spec.ResolvedPolicy.RunID ||
				storedPolicy.Digest != spec.ResolvedPolicy.Digest ||
				!slices.Equal(storedPolicy.Keys, spec.ResolvedPolicy.Keys) {
				return fmt.Errorf(
					"stored production run has a foreign resolved policy: %w",
					domain.ErrImmutableTransition,
				)
			}
			storedInvocation, invocationErr := tx.GetAgentInvocation(ctx, invocationID)
			if invocationErr != nil {
				if errors.Is(invocationErr, store.ErrNotFound) {
					return fmt.Errorf(
						"stored production run has no agent invocation: %w",
						domain.ErrImmutableTransition,
					)
				}
				return invocationErr
			}
			if storedInvocation.ID != invocation.ID ||
				!slices.Equal(storedInvocation.InputIDs, invocation.InputIDs) ||
				storedInvocation.ConversationID != nil ||
				storedInvocation.ThroughSequence != invocation.ThroughSequence {
				return fmt.Errorf(
					"stored production run has a foreign agent invocation: %w",
					domain.ErrImmutableTransition,
				)
			}
			run = existing
		case errors.Is(err, store.ErrNotFound):
			if err := tx.PutRun(ctx, want); err != nil {
				return err
			}
			run = want
			runCreated = true
		default:
			return err
		}

		if runCreated {
			// A committed publisher intent at the derived production key is
			// external history, not resumable state for a run that did not exist
			// when this transaction began. Refuse it before this new run claims
			// the key; the transaction rollback keeps every run-local row absent.
			if err := publish.CheckInvocationAvailable(ctx, tx, publicationReservation); err != nil {
				return err
			}
			// A newly created production run must create its invocation and
			// policy and ownership marker in this transaction. Pre-existing
			// lane keys are not partial production state: they belong to
			// something else or are damaged, and adopting them would recreate
			// the same retroactive-claim bug under a different object.
			if _, err := tx.GetResolvedPolicy(ctx, spec.RunID); err == nil {
				return fmt.Errorf(
					"new production run's resolved policy already exists: %w",
					domain.ErrImmutableTransition,
				)
			} else if !errors.Is(err, store.ErrNotFound) {
				return err
			}
			if _, err := tx.GetAgentInvocation(ctx, invocationID); err == nil {
				return fmt.Errorf(
					"new production run's invocation already exists: %w",
					domain.ErrImmutableTransition,
				)
			} else if !errors.Is(err, store.ErrNotFound) {
				return err
			}
			if _, err := tx.GetOutbox(ctx, string(invocationID)); err == nil {
				return fmt.Errorf(
					"new production run's ownership marker already exists: %w",
					domain.ErrImmutableTransition,
				)
			} else if !errors.Is(err, store.ErrNotFound) {
				return err
			}
			if err := tx.PutResolvedPolicy(ctx, spec.ResolvedPolicy); err != nil {
				return err
			}
			if err := tx.PutAgentInvocation(ctx, invocation); err != nil {
				return err
			}
			entry, inserted, err := tx.EnqueueOutbox(
				ctx, string(invocationID), KindProductionInvocationRequested, payload,
			)
			if err != nil {
				return err
			}
			if !inserted || entry.Kind != KindProductionInvocationRequested ||
				!bytes.Equal(entry.Payload, payload) {
				return fmt.Errorf(
					"new production run did not create its ownership marker: %w",
					domain.ErrImmutableTransition,
				)
			}
		}
		// Publication ownership is reserved in the same transaction as a new
		// run and is re-checked on every converged submission. The producing
		// export does not exist yet, so the reservation is the only safe durable
		// claim until the execution-bound publisher promotes it.
		if err := publish.ClaimInvocation(ctx, tx, publicationReservation); err != nil {
			return err
		}
		// The §5.18 work-unit declaration rides the run-creating transaction
		// and only that one: whether a run is declared is fixed at intake. A
		// converged re-submission must re-state the same declaration
		// (compared modulo the stamped instant); a disagreeing one is
		// refused like any other fixed binding, since a changed declaration
		// is a different unit, never an update; and declaring an existing
		// undeclared run is refused too — the run may already be executing,
		// published, or terminal, where a retroactive declaration mints a
		// unit no remaining pass can ever bind or complete (and a
		// pre-capture run must not gain one, the migration's no-backfill
		// rule).
		if spec.WorkUnit != nil {
			declared, err := domain.NewWorkUnitDeclaration(
				*spec.WorkUnit, spec.RunID, spec.ProjectID, time.Now().UTC())
			if err != nil {
				return fmt.Errorf("work unit declaration: %w", err)
			}
			existing, declErr := tx.GetWorkUnitDeclarationByRun(ctx, spec.RunID)
			switch {
			case declErr == nil:
				want := declared
				want.DeclaredAt = existing.DeclaredAt
				if !reflect.DeepEqual(want, existing) {
					return fmt.Errorf(
						"stored work-unit declaration disagrees with the submission: %w",
						domain.ErrImmutableTransition)
				}
			case errors.Is(declErr, store.ErrNotFound):
				if !runCreated {
					return fmt.Errorf(
						"stored run was submitted without a work-unit declaration: %w",
						domain.ErrImmutableTransition)
				}
				if err := tx.RecordWorkUnitDeclaration(ctx, declared); err != nil {
					return err
				}
			default:
				return declErr
			}
		}
		// The submitted milestone rides only the creating transaction: the
		// atomic creation makes it exact, and a replayed submission against
		// a run persisted before migration 0024 must not backfill a
		// submission instant that was never observed (the migration's
		// no-backfill rule). A post-0024 replay converges on the row the
		// creation already wrote.
		if !runCreated {
			return nil
		}
		observedInvocation := invocationID
		return tx.AppendRunMilestone(ctx, domain.RunMilestone{
			RunID: spec.RunID, Kind: domain.MilestoneRunSubmitted,
			InvocationID: &observedInvocation, RecordedAt: time.Now().UTC(),
		})
	})
	if err != nil {
		return ProductionRun{}, fmt.Errorf("submit production run %q: %w", spec.RunID, err)
	}
	return ProductionRun{Run: run, InvocationID: invocationID, StageID: stageID}, nil
}

func authorizeProductionSubmission(
	ctx context.Context,
	tx *store.WriteTx,
	spec ProductionRunSpec,
	grant *specificationRequest,
) error {
	claim, err := getSpecificationImplementationClaim(ctx, tx, spec.RunID)
	if errors.Is(err, store.ErrNotFound) {
		if grant != nil {
			return fmt.Errorf("specification grant for unreserved implementation run %q: %w",
				spec.RunID, domain.ErrParentKeyMismatch)
		}
		reserved, evidenceErr := hasSpecificationReservationEvidence(ctx, &tx.ReadTx, spec.RunID)
		if evidenceErr != nil {
			return evidenceErr
		}
		if reserved {
			return fmt.Errorf("implementation run %q has damaged specification reservation state: %w",
				spec.RunID, ErrImplementationRunReserved)
		}
		return nil
	}
	if err != nil {
		return err
	}
	if grant == nil {
		return fmt.Errorf("implementation run %q: %w", spec.RunID, ErrImplementationRunReserved)
	}
	if err := grant.validate(); err != nil {
		return fmt.Errorf("authenticate implementation reservation: %w", err)
	}
	verified, err := verifySpecificationTerminal(ctx, &tx.ReadTx, *grant)
	if err != nil {
		return fmt.Errorf("authenticate implementation reservation: %w", err)
	}
	if err := authorizeSpecificationImplementation(verified, spec.SpecArtifactID); err != nil {
		return fmt.Errorf("authenticate implementation reservation: %w", err)
	}
	if claim.Kind != KindSpecificationImplementationClaim || !claim.Dispatched() ||
		grant.ImplementationRunID != spec.RunID || grant.ProjectID != spec.ProjectID ||
		grant.PolicyArtifactID != spec.PolicyArtifactID || grant.Publication != spec.Publication ||
		grant.CampaignID != spec.CampaignID || grant.AttemptNumber != spec.AttemptNumber ||
		!sameSpecificationWorkUnit(grant.WorkUnit, spec.WorkUnit) {
		return fmt.Errorf("implementation reservation disagrees with production submission: %w",
			domain.ErrParentKeyMismatch)
	}
	return nil
}

func authenticateProductionAttempt(
	ctx context.Context,
	tx *store.WriteTx,
	spec ProductionRunSpec,
	approvedSpecDigest domain.Digest,
	grant *specificationRequest,
) error {
	if spec.CampaignID == "" {
		if spec.AttemptNumber != 0 || spec.AttemptReason != "" || spec.ParentRunID != "" {
			return fmt.Errorf("partial production attempt lineage: %w", domain.ErrProductionAttemptInconsistent)
		}
		return nil
	}
	var (
		attempt domain.ProductionAttempt
		err     error
	)
	if grant != nil {
		attempt, err = tx.ApproveProductionAttempt(ctx, spec.CampaignID, spec.AttemptNumber, approvedSpecDigest)
	} else {
		attempt, err = tx.GetProductionAttempt(ctx, spec.CampaignID, spec.AttemptNumber)
	}
	if err != nil {
		return fmt.Errorf("authenticate production attempt %s/%d: %w", spec.CampaignID, spec.AttemptNumber, err)
	}
	if grant == nil && attempt.Kind != domain.ProductionAttemptRetry {
		return fmt.Errorf("initial production attempt requires specification grant: %w", domain.ErrParentKeyMismatch)
	}
	if grant == nil && attempt.Kind == domain.ProductionAttemptRetry {
		parentRun, err := tx.GetRun(ctx, attempt.ParentRunID)
		if err != nil {
			return fmt.Errorf("load retry parent %q: %w", attempt.ParentRunID, err)
		}
		observation, err := tx.ObserveRun(ctx, attempt.ParentRunID)
		if err != nil {
			return fmt.Errorf("observe retry parent %q: %w", attempt.ParentRunID, err)
		}
		conclusion, err := AuthenticatedProductionRunConclusion(ctx, &tx.ReadTx, parentRun, observation)
		if err != nil {
			return fmt.Errorf("conclude retry parent %q: %w", attempt.ParentRunID, err)
		}
		if !conclusion.Final {
			return fmt.Errorf("retry parent %q is still live: %w", attempt.ParentRunID, domain.ErrParentKeyMismatch)
		}
	}
	if grant != nil {
		if len(grant.InputArtifactIDs) != 1 {
			return fmt.Errorf("specification grant source inputs: %w", domain.ErrParentKeyMismatch)
		}
		sourceArtifact, err := tx.GetArtifact(ctx, grant.InputArtifactIDs[0])
		if err != nil {
			return fmt.Errorf("load specification source artifact: %w", err)
		}
		if attempt.SourceDigest != sourceArtifact.Digest ||
			attempt.PublicationDigest != grant.PublicationDigest ||
			attempt.SpecificationRunID != grant.SpecificationRunID {
			return fmt.Errorf("production attempt disagrees with specification grant: %w", domain.ErrParentKeyMismatch)
		}
	}
	if attempt.ImplementationRunID != spec.RunID || attempt.ApprovedSpecDigest != approvedSpecDigest ||
		attempt.ParentRunID != spec.ParentRunID || attempt.Reason != spec.AttemptReason {
		return fmt.Errorf("production attempt disagrees with run: %w", domain.ErrParentKeyMismatch)
	}
	return nil
}

func hasSpecificationReservationEvidence(
	ctx context.Context, tx *store.ReadTx, implementationRunID domain.RunID,
) (bool, error) {
	for _, specificationRunID := range specificationRunIDCandidates(implementationRunID) {
		if _, err := tx.GetRun(ctx, specificationRunID); err == nil {
			return true, nil
		} else if !errors.Is(err, store.ErrNotFound) {
			return false, err
		}
		if _, err := tx.GetOutbox(ctx, string(specificationInvocationID(specificationRunID, 1))); err == nil {
			return true, nil
		} else if !errors.Is(err, store.ErrNotFound) {
			return false, err
		}
	}
	// This fallback is a conservative existence probe used only to refuse an
	// ungranted production submission when the deterministic reservation claim
	// is missing. It never grants execution or transition authority, so parsing
	// any surviving marker here does not substitute for transition-chain
	// verification; a match can only turn the answer from "unreserved" into
	// "damaged reservation, fail closed."
	for _, list := range []func(context.Context, string) ([]store.QueueEntry, error){
		tx.ListPendingOutbox,
		tx.ListDispatchedOutbox,
		tx.ListQuarantinedOutbox,
	} {
		entries, err := list(ctx, KindSpecificationInvocationRequested)
		if err != nil {
			return false, err
		}
		for _, entry := range entries {
			request, err := decodeSpecificationRequest(entry)
			if err == nil && request.ImplementationRunID == implementationRunID {
				return true, nil
			}
		}
	}
	return false, nil
}

// ProductionInvocationBackupPayloadDigests validates a durable production
// dispatch intent for the local checkpoint-health scan. The request is
// self-contained and needs no external blobs.
func ProductionInvocationBackupPayloadDigests(entry store.QueueEntry) ([]domain.Digest, error) {
	if _, err := decodeProductionRequest(entry); err != nil {
		return nil, err
	}
	return nil, nil
}

// ProductionInvocationPublication reconstructs reviewer and commit-author
// metadata from the durable production ownership marker. Present is false for
// a released v1 marker, which owns execution but carries no publication
// authority.
func ProductionInvocationPublication(entry store.QueueEntry) (ProductionPublication, bool, error) {
	request, err := decodeProductionRequest(entry)
	if err != nil {
		return ProductionPublication{}, false, err
	}
	if request.Legacy {
		return ProductionPublication{}, false, nil
	}
	return request.Publication, true, nil
}

// errProductionMarkerUnsupportedVersion marks a stored marker written by a
// newer daemon: its shape is a released contract this binary does not know,
// so reconstruction is impossible here but succeeds again after an upgrade.
// errProductionMarkerUnreadable marks every other reconstruction failure of a
// marker row, which no upgrade repairs. Both classify one run out of this
// daemon's production lane (see quarantineProductionMarker); neither loosens
// a released version's canonical decode.
var (
	errProductionMarkerUnsupportedVersion = errors.New("production marker version is not supported")
	errProductionMarkerUnreadable         = errors.New("production marker cannot be authenticated")
)

// authenticateProductionMarker authenticates one stored marker as run runID's,
// classifying every failure so a caller can quarantine the run instead of
// failing its whole reconcile pass. The gates are decodeProductionRequest's
// unchanged ones plus the run the caller loaded the row under; classification
// adds a wrapper, never a tolerance.
func authenticateProductionMarker(
	entry store.QueueEntry, runID domain.RunID,
) (productionInvocationRequest, error) {
	request, err := decodeProductionRequest(entry)
	switch {
	case errors.Is(err, errProductionMarkerUnsupportedVersion):
		return productionInvocationRequest{}, err
	case err != nil:
		return productionInvocationRequest{}, fmt.Errorf("%w: %w", errProductionMarkerUnreadable, err)
	}
	if request.RunID != runID {
		return productionInvocationRequest{}, fmt.Errorf(
			"%w: production marker names run %q, loaded under %q: %w",
			errProductionMarkerUnreadable, request.RunID, runID, domain.ErrParentKeyMismatch,
		)
	}
	return request, nil
}

// unsupportedProductionMarkerVersion names the marker version this binary does
// not know, or "" for anything it should decode strictly. It reads the version
// envelope leniently and ahead of the strict decode, because a newer version
// normally *adds* a field, and DisallowUnknownFields would otherwise reject
// the payload before the version was ever read: the downgrade this lane exists
// to survive would then be reported as a malformed marker, losing the one
// diagnosis that tells an operator an upgrade repairs the hold.
//
// It is a classifier, never an acceptance path. Every payload it passes over
// still goes through the unchanged strict decode and its gates, so nothing
// this reports can widen what the daemon accepts: it can only decide which
// refusal an operator reads.
func unsupportedProductionMarkerVersion(payload []byte) string {
	var envelope struct {
		Version json.RawMessage `json:"version"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil || len(envelope.Version) == 0 {
		return ""
	}
	version := ""
	if err := json.Unmarshal(envelope.Version, &version); err != nil {
		return ""
	}
	// Only a version in this lane's own namespace that orders *after* the
	// release this binary implements is a newer daemon's. Anything else — a
	// corrupt string, a foreign namespace, a non-canonical number — is an
	// unreadable marker, and must not be told that an upgrade repairs it.
	number, ok := strings.CutPrefix(version, productionInvocationVersionNamespace)
	if !ok {
		return ""
	}
	release, err := strconv.Atoi(number)
	if err != nil || strconv.Itoa(release) != number ||
		release <= productionInvocationRequestVersionNumber {
		return ""
	}
	return version
}

// decodeProductionRequest reconstructs and re-checks one production dispatch// decodeProductionRequest reconstructs and re-checks one production dispatch
// intent against its own row. Queue payloads are opaque to the store, so the
// decoded intent is a reconstruction boundary (the same discipline as
// signet's decodeBoundInvocationRequest).
func decodeProductionRequest(entry store.QueueEntry) (productionInvocationRequest, error) {
	if unsupported := unsupportedProductionMarkerVersion(entry.Payload); unsupported != "" {
		return productionInvocationRequest{}, fmt.Errorf("decode payload: unsupported version %q: %w",
			unsupported, errProductionMarkerUnsupportedVersion)
	}
	var wire productionInvocationRequestWire
	if err := strictjson.Decode(entry.Payload, &wire, strictjson.TolerateInvalidUTF8, strictjson.NoLimit); err != nil {
		if errors.Is(err, strictjson.ErrTrailingData) {
			return productionInvocationRequest{}, errors.New("decode payload: trailing JSON value")
		}
		return productionInvocationRequest{}, fmt.Errorf("decode payload: %w", err)
	}
	if entry.Kind != KindProductionInvocationRequested {
		return productionInvocationRequest{}, fmt.Errorf(
			"intent %q has kind %q: %w", entry.IdempotencyKey, entry.Kind, domain.ErrParentKeyMismatch)
	}
	version := ""
	if len(wire.Version) > 0 {
		if err := json.Unmarshal(wire.Version, &version); err != nil || version == "" {
			return productionInvocationRequest{}, errors.New("decode payload: version must be a non-empty string")
		}
	}
	request := productionInvocationRequest{
		Version: version, InvocationID: wire.InvocationID,
		RunID: wire.RunID, StageID: wire.StageID,
	}
	if request.InvocationID == "" || request.RunID == "" || request.StageID == "" {
		return productionInvocationRequest{}, fmt.Errorf("decode payload: required identity is empty: %w", domain.ErrEmptyID)
	}
	switch {
	case len(wire.Version) == 0 && len(wire.Publication) == 0:
		request.Legacy = true
	case len(wire.Version) == 0 || version == productionInvocationRequestVersion:
		if len(wire.Publication) == 0 || bytes.Equal(wire.Publication, []byte("null")) {
			return productionInvocationRequest{}, errors.New("decode payload: publication is required")
		}
		if err := strictjson.Decode(
			wire.Publication, &request.Publication, strictjson.TolerateInvalidUTF8, strictjson.NoLimit,
		); err != nil {
			if errors.Is(err, strictjson.ErrTrailingData) {
				return productionInvocationRequest{}, errors.New("decode payload publication: trailing JSON value")
			}
			return productionInvocationRequest{}, fmt.Errorf("decode payload publication: %w", err)
		}
		if err := request.Publication.Validate(); err != nil {
			return productionInvocationRequest{}, fmt.Errorf("decode payload: %w", err)
		}
	default:
		return productionInvocationRequest{}, fmt.Errorf("decode payload: unsupported version %q: %w",
			version, errProductionMarkerUnsupportedVersion)
	}
	if string(request.InvocationID) != entry.IdempotencyKey {
		return productionInvocationRequest{}, fmt.Errorf(
			"payload invocation_id %q disagrees with key %q: %w",
			request.InvocationID, entry.IdempotencyKey, domain.ErrParentKeyMismatch)
	}
	if request.InvocationID != productionInvocationID(request.RunID) ||
		request.StageID != productionStageID(request.RunID) {
		return productionInvocationRequest{}, fmt.Errorf(
			"payload identities disagree with lane derivation for run %q: %w",
			request.RunID, domain.ErrParentKeyMismatch)
	}
	canonical, err := canonicalProductionRequestPayload(request)
	if err != nil {
		return productionInvocationRequest{}, fmt.Errorf("encode canonical payload: %w", err)
	}
	if !bytes.Equal(entry.Payload, canonical) {
		return productionInvocationRequest{}, fmt.Errorf(
			"payload is not the canonical %s shape: %w", productionRequestFormat(request), domain.ErrParentKeyMismatch)
	}
	return request, nil
}

func canonicalProductionRequestPayload(request productionInvocationRequest) ([]byte, error) {
	if request.Legacy {
		return json.Marshal(struct {
			InvocationID domain.InvocationID `json:"invocation_id"`
			RunID        domain.RunID        `json:"run_id"`
			StageID      domain.StageID      `json:"stage_id"`
		}{request.InvocationID, request.RunID, request.StageID})
	}
	if request.Version == "" {
		return json.Marshal(struct {
			InvocationID domain.InvocationID   `json:"invocation_id"`
			RunID        domain.RunID          `json:"run_id"`
			StageID      domain.StageID        `json:"stage_id"`
			Publication  ProductionPublication `json:"publication"`
		}{request.InvocationID, request.RunID, request.StageID, request.Publication})
	}
	return json.Marshal(request)
}

func productionRequestFormat(request productionInvocationRequest) string {
	if request.Legacy {
		return "legacy v1"
	}
	if request.Version == "" {
		return "unversioned publication preview"
	}
	return request.Version
}

// ownsProductionRun reports whether this run belongs to the production lane:
// its exact derived dispatch intent exists. The payload is reconstruction
// authority too: trusting only the kind would let a malformed or retargeted
// row classify a foreign run as production-owned before dispatch's stricter
// decoder sees it.
//
// A marker that fails that authentication makes the run unowned here rather
// than failing the pass. Every stored run is scanned, so one unreconstructable
// row would otherwise end reconciliation on every pass and take the daemon's
// unrelated runs down with it (#424, the shape #418 fixed for legacy rows).
// Unowned is the safe answer, not a quiet one: the run is never dispatched or
// accepted as production work, and quarantineProductionMarker records why.
func (e *Engine) ownsProductionRun(ctx context.Context, run domain.Run) (bool, error) {
	var transition authenticatedProductionRunTransition
	err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		transition, err = authenticateProductionRunTransition(ctx, tx, run.ID)
		return err
	})
	if transition.run.ID != "" {
		run = transition.run
	}
	if err == nil {
		var artifacts ArtifactStore
		if e.productionPublication != nil {
			artifacts = e.productionPublication.artifacts
		}
		err = authenticateProductionRunInput(artifacts, transition)
	}
	if err == nil && transition.production == nil {
		// No marker row means the run is not this lane's, which is also how a
		// repaired store looks after the bad row is removed: the hold a
		// quarantine notice describes has ended either way.
		if releaseErr := releaseProductionQuarantine(
			ctx, e.store, e.signet, productionMarkerQuarantinePrefix, run.ID,
		); releaseErr != nil {
			return false, releaseErr
		}
		return false, releaseProductionQuarantine(
			ctx, e.store, e.signet, remediationMarkerQuarantinePrefix, run.ID)
	}
	if _, markerFailure := productionQuarantineReason(err); markerFailure {
		quarantined, quarantineErr := quarantineProductionMarker(
			ctx, e.store, e.signet, run.ID, run.ProjectID, err)
		if quarantineErr != nil {
			return false, errors.Join(
				fmt.Errorf("authenticate production marker for run %q: %w", run.ID, err),
				quarantineErr)
		}
		if quarantined {
			return false, nil
		}
		return false, fmt.Errorf("authenticate production marker for run %q: %w", run.ID, err)
	}
	if errors.Is(err, errRemediationMarkerUnreadable) {
		if quarantineErr := recordProductionQuarantine(
			ctx, e.store, e.signet, remediationMarkerQuarantinePrefix,
			run.ID, run.ProjectID, remediationQuarantineUnreadable,
		); quarantineErr != nil {
			return false, errors.Join(err, quarantineErr)
		}
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("authenticate production transition for run %q: %w", run.ID, err)
	}
	// The marker reads again, so an earlier pass's quarantine no longer
	// describes this run: the upgrade the notice asked for has happened, or
	// the row was repaired. Retiring it here is what keeps the notice a
	// notice rather than a permanent, contradicted claim.
	if err := releaseProductionQuarantine(
		ctx, e.store, e.signet, productionMarkerQuarantinePrefix, run.ID); err != nil {
		return false, err
	}
	return true, releaseProductionQuarantine(
		ctx, e.store, e.signet, remediationMarkerQuarantinePrefix, run.ID)
}

// loadProductionBinding resolves the durable state behind one production
// invocation. The conversation-lane fields of invocationBinding stay zero;
// stageInputSnapshot and admitAttempt already branch on the invocation's nil
// ConversationID.
func (e *Engine) loadProductionBinding(ctx context.Context, request productionInvocationRequest) (invocationBinding, error) {
	var binding invocationBinding
	err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		binding.invocation, err = tx.GetAgentInvocation(ctx, request.InvocationID)
		if err != nil {
			return err
		}
		if binding.invocation.ConversationID != nil {
			return fmt.Errorf("production invocation %q binds conversation %q: %w",
				request.InvocationID, *binding.invocation.ConversationID, domain.ErrParentKeyMismatch)
		}
		if len(binding.invocation.InputIDs) != 1 {
			return fmt.Errorf(
				"production invocation %q has %d input artifacts, want its one specification: %w",
				request.InvocationID, len(binding.invocation.InputIDs),
				domain.ErrParentKeyMismatch,
			)
		}
		specification, err := tx.GetArtifact(ctx, binding.invocation.InputIDs[0])
		if err != nil {
			return err
		}
		binding.run, err = tx.GetRun(ctx, request.RunID)
		if err != nil {
			return err
		}
		if specification.Type != productionSpecificationArtifactType ||
			specification.Digest != binding.run.SpecDigest {
			return fmt.Errorf(
				"production invocation %q input %q is not run %q's specification: %w",
				request.InvocationID, specification.ID, binding.run.ID,
				domain.ErrParentKeyMismatch,
			)
		}
		return nil
	})
	if err != nil {
		return invocationBinding{}, err
	}
	if _, ok := findProductionStage(binding.run); !ok {
		return invocationBinding{}, fmt.Errorf("run %q has no %q stage: %w",
			binding.run.ID, productionStageName, domain.ErrParentKeyMismatch)
	}
	return binding, nil
}

// productionTerminalRecord is the inbox payload recording one production
// stage's terminal outcome at most once. Status is the recorded terminal
// class: completed, failed, canceled, or gone for a session lost without a
// committed result.
type productionTerminalRecord struct {
	InvocationID domain.InvocationID `json:"invocation_id"`
	RunID        domain.RunID        `json:"run_id"`
	StageID      domain.StageID      `json:"stage_id"`
	Status       exec.Status         `json:"status"`
	HeadSHA      string              `json:"head_sha,omitempty"`
	Artifacts    []domain.Digest     `json:"artifacts,omitempty"`
	Summary      string              `json:"summary,omitempty"`
}

// decodeProductionTerminal reconstructs and re-gates one stored terminal
// record against the row it came from and the run it claims. It mirrors
// decodeProductionRequest: the same strict decode, the same derivation
// checks, so a row that survives is one this lane could itself have written.
func decodeProductionTerminal(
	entry store.QueueEntry, run domain.Run,
) (productionTerminalRecord, error) {
	if entry.Kind != kindProductionStageTerminal {
		return productionTerminalRecord{}, fmt.Errorf("inbox row %q has kind %q: %w",
			entry.IdempotencyKey, entry.Kind, domain.ErrParentKeyMismatch)
	}
	var terminal productionTerminalRecord
	if err := strictjson.Decode(entry.Payload, &terminal, strictjson.TolerateInvalidUTF8, strictjson.NoLimit); err != nil {
		if errors.Is(err, strictjson.ErrTrailingData) {
			return productionTerminalRecord{}, fmt.Errorf(
				"decode terminal record %q: trailing JSON value", entry.IdempotencyKey)
		}
		return productionTerminalRecord{}, fmt.Errorf("decode terminal record %q: %w",
			entry.IdempotencyKey, err)
	}
	stage, stageFound := productionStageForInvocation(run, terminal.InvocationID)
	switch {
	case string(terminal.InvocationID) != entry.IdempotencyKey:
		return productionTerminalRecord{}, fmt.Errorf(
			"terminal record %q names invocation %q: %w",
			entry.IdempotencyKey, terminal.InvocationID, domain.ErrParentKeyMismatch)
	case terminal.RunID != run.ID:
		return productionTerminalRecord{}, fmt.Errorf(
			"terminal record %q names run %q, attempt is on %q: %w",
			entry.IdempotencyKey, terminal.RunID, run.ID, domain.ErrParentKeyMismatch)
	case !stageFound || terminal.StageID != stage.ID:
		return productionTerminalRecord{}, fmt.Errorf(
			"terminal record %q disagrees with lane derivation for run %q: %w",
			entry.IdempotencyKey, run.ID, domain.ErrParentKeyMismatch)
	// A non-terminal or unknown status is not a recorded outcome, so it must
	// not stand in for one. StatusGone is this lane's own record of a session
	// lost without a result, so it is admitted alongside the terminal set.
	case terminal.Status != exec.StatusGone && !terminal.Status.Terminal():
		return productionTerminalRecord{}, fmt.Errorf(
			"terminal record %q carries status %q: %w",
			entry.IdempotencyKey, terminal.Status, exec.ErrInvalidStatus)
	}
	return terminal, nil
}

// acceptProductionAttempt closes one production attempt: a completed result
// is re-gated before its first acceptance, while a failed, canceled, or lost
// one is recorded and surfaced as an execution_failure item instead of
// failing the reconcile loop (unattended operation must not wedge the engine
// on one bad run). The durable inbox row is the at-most-once guard, mirroring
// signet's completion intake. It reports true only for an accepted completed
// result.
func (e *Engine) acceptProductionAttempt(ctx context.Context, run domain.Run, attempt domain.Attempt) (bool, error) {
	if attempt.ID != attemptIDFor(attempt.InvocationID) {
		return false, fmt.Errorf("attempt %q disagrees with invocation %q: %w",
			attempt.ID, attempt.InvocationID, domain.ErrParentKeyMismatch)
	}
	stage, ok := productionStageForInvocation(run, attempt.InvocationID)
	if !ok || attempt.StageID != stage.ID {
		return false, fmt.Errorf("attempt binding disagrees with run: %w", domain.ErrParentKeyMismatch)
	}
	operatorFeedback := false
	if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		operatorFeedback, err = authenticateOperatorFeedbackAttempt(
			ctx, tx, attempt.InvocationID, run.ID, attempt.StageID,
		)
		return err
	}); err != nil {
		return false, fmt.Errorf("authenticate operator-feedback attempt: %w", err)
	}
	request, err := (&productionPublicationWorkflow{store: e.store}).loadProductionRequest(ctx, run)
	if err != nil {
		return false, err
	}
	legacy := request.Legacy && attempt.InvocationID == productionInvocationID(run.ID)

	var (
		recorded        *productionTerminalRecord
		outcomeRecorded bool
	)
	err = e.store.Read(ctx, func(tx *store.ReadTx) error {
		// The terminal inbox row is checked first: once collection has recorded
		// one, every later pass re-authenticates it against the driver through
		// the `recorded` path below, never the skip.
		//
		// A stored row is a reconstruction boundary, not authority. Trusting
		// the kind alone would let a corrupted or fabricated row permanently
		// suppress this attempt's collection: no accepted result, and no
		// execution_failure item either, which is the one outcome that makes
		// a failure invisible rather than loud.
		entry, err := tx.GetInbox(ctx, string(attempt.InvocationID))
		switch {
		case err == nil:
			terminal, err := decodeProductionTerminal(entry, run)
			if err != nil {
				return err
			}
			recorded = &terminal
			return nil
		case !errors.Is(err, store.ErrNotFound):
			return err
		}
		// No terminal row. Skip collection only for the #842 delivery refusal:
		// the one engine path that records a failed outcome and its
		// execution_failure item without ever starting the driver or writing a
		// terminal row, so collecting it would call the driver on an invocation
		// it never saw and fail the pass on a dispatched marker. Both the
		// outcome record and that item must be present, because that pair is
		// exactly what recordProductionDeliveryRefusal leaves behind.
		//
		// A driver-recorded failed, canceled, or lost outcome has no item yet
		// (the driver records the outcome before the engine's next pass), so
		// the engine must still collect the terminal and raise it;
		// executionFailureFacts then converges on that stored record. A blocked
		// outcome carries an agent_question item, not an execution_failure one,
		// so it is collected too.
		if _, outcomeErr := tx.GetExecutionOutcomeRecord(ctx, attempt.InvocationID); errors.Is(outcomeErr, store.ErrNotFound) {
			return nil
		} else if outcomeErr != nil {
			return outcomeErr
		}
		if _, itemErr := tx.GetAttentionItem(
			ctx, domain.ItemID("execution-failure-"+string(attempt.InvocationID)),
		); errors.Is(itemErr, store.ErrNotFound) {
			return nil
		} else if itemErr != nil {
			return itemErr
		}
		outcomeRecorded = true
		return nil
	})
	if err != nil {
		return false, err
	}
	if outcomeRecorded {
		return false, nil
	}
	if recorded != nil {
		if legacy && recorded.Status == exec.StatusCompleted {
			durable, err := e.authenticatesLegacyCompletedTerminal(ctx, run, attempt, *recorded)
			if err != nil {
				return false, err
			}
			if durable {
				return false, nil
			}
		}
		if !legacy && !operatorFeedback && e.productionPublication != nil {
			durable, err := e.productionPublication.authenticatesTerminal(ctx, run, *recorded)
			if err != nil {
				return false, err
			}
			if durable {
				return false, nil
			}
		}
		if err := e.authenticateProductionTerminal(ctx, run, attempt, *recorded); err != nil {
			return false, err
		}
		return false, nil
	}
	if !legacy && !operatorFeedback && e.productionPublication != nil {
		queued, err := e.productionPublication.hasQueuedCompletion(ctx, run, attempt.InvocationID)
		if err != nil {
			return false, err
		}
		if queued {
			return false, nil
		}
	}

	result, ready, err := e.collectTerminal(ctx, run.ID, attempt)
	lost := false
	switch {
	case errors.Is(err, ErrInvocationLost):
		lost = true
	case MutableAdmissionPolicyRefusal(err):
		return false, nil
	case err != nil:
		return false, err
	case !ready:
		return false, nil
	}

	terminal := productionTerminalRecord{
		InvocationID: attempt.InvocationID, RunID: run.ID, StageID: attempt.StageID,
		Status: exec.StatusGone,
	}
	if !lost {
		terminal.Status = result.Status
		terminal.HeadSHA = result.HeadSHA
		terminal.Artifacts = result.Artifacts
		terminal.Summary = result.Summary
	}
	if terminal.Status == exec.StatusCompleted {
		// Re-gated at the last point the engine controls before the
		// acceptance commits, for the same reason acceptAttempt re-gates: the
		// trust anchor of an unattended or waived admission can move while a
		// driver call is in flight.
		admission, err := e.productionAdmission(ctx, attempt.InvocationID)
		if err != nil {
			if MutableAdmissionPolicyRefusal(err) {
				return false, nil
			}
			return false, err
		}
		if legacy {
			accepted, err := e.recordProductionTerminal(ctx, run, terminal)
			if MutableAdmissionPolicyRefusal(err) {
				return false, nil
			}
			return accepted, err
		}
		if operatorFeedback {
			accepted, err := e.recordProductionTerminal(ctx, run, terminal)
			if MutableAdmissionPolicyRefusal(err) {
				return false, nil
			}
			return accepted, err
		}
		switch admission.OperatingMode {
		case domain.ModeAttendedDev:
			// A prior build could start production work while attended. Preserve
			// its authentic terminal result, but never turn that attended
			// admission into an automatic publication candidate.
			accepted, err := e.recordProductionTerminal(ctx, run, terminal)
			if MutableAdmissionPolicyRefusal(err) {
				return false, nil
			}
			return accepted, err
		case domain.ModeUnattended:
			if e.productionPublication == nil {
				return false, errors.New("production publication workflow is not configured")
			}
			return false, fmt.Errorf(
				"completed production invocation %q has no atomic publication task: %w",
				attempt.InvocationID, domain.ErrParentKeyMismatch,
			)
		}
		return false, fmt.Errorf("production invocation %q has invalid operating mode %q: %w",
			attempt.InvocationID, admission.OperatingMode, domain.ErrInvalidOperatingMode)
	}
	accepted, err := e.recordProductionTerminal(ctx, run, terminal)
	if MutableAdmissionPolicyRefusal(err) {
		return false, nil
	}
	return accepted, err
}

func (e *Engine) authenticatesLegacyCompletedTerminal(
	ctx context.Context,
	run domain.Run,
	attempt domain.Attempt,
	recorded productionTerminalRecord,
) (bool, error) {
	// A completed export is an independent immutable handoff record. It lets a
	// released v1 terminal remain inert after private driver state disappears,
	// without treating the inbox row alone as proof or granting publication.
	var (
		marker    store.QueueEntry
		admission domain.ExecutionAdmission
		exported  domain.ExecutionExport
	)
	err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		marker, err = tx.GetOutbox(ctx, string(attempt.InvocationID))
		if err != nil {
			return err
		}
		admission, err = tx.GetExecutionAdmissionRecord(ctx, attempt.InvocationID)
		if err != nil {
			return err
		}
		exported, err = tx.GetExecutionExportRecord(ctx, attempt.InvocationID)
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	request, err := decodeProductionRequest(marker)
	if err != nil {
		return false, err
	}
	if !request.Legacy || !marker.Dispatched() ||
		request.RunID != run.ID || request.InvocationID != attempt.InvocationID ||
		admission.RunID != run.ID || admission.StageID != attempt.StageID ||
		exported.InvocationID != attempt.InvocationID || exported.HeadSHA != recorded.HeadSHA {
		return false, fmt.Errorf("legacy completed terminal disagrees with immutable export history: %w",
			domain.ErrParentKeyMismatch)
	}
	return true, nil
}

func (e *Engine) authenticateProductionTerminal(
	ctx context.Context, run domain.Run, attempt domain.Attempt, recorded productionTerminalRecord,
) error {
	if err := e.requireProductionAdmissionRecord(ctx, attempt.InvocationID); err != nil {
		return err
	}
	result, ready, err := e.collectTerminal(ctx, run.ID, attempt)
	lost := errors.Is(err, ErrInvocationLost)
	if err != nil && !lost {
		return err
	}
	if !lost && !ready {
		return fmt.Errorf("terminal record for %q has no collectable driver outcome: %w",
			attempt.InvocationID, domain.ErrParentKeyMismatch)
	}
	actual := productionTerminalRecord{
		InvocationID: attempt.InvocationID, RunID: run.ID, StageID: attempt.StageID,
		Status: exec.StatusGone,
	}
	if !lost {
		actual.Status = result.Status
		actual.HeadSHA = result.HeadSHA
		actual.Artifacts = result.Artifacts
		actual.Summary = result.Summary
	}
	if recorded.InvocationID != actual.InvocationID ||
		recorded.RunID != actual.RunID ||
		recorded.StageID != actual.StageID ||
		recorded.Status != actual.Status ||
		recorded.HeadSHA != actual.HeadSHA ||
		!slices.Equal(recorded.Artifacts, actual.Artifacts) ||
		recorded.Summary != actual.Summary {
		return fmt.Errorf("terminal record for %q disagrees with collected outcome: %w",
			attempt.InvocationID, domain.ErrParentKeyMismatch)
	}
	return nil
}

// requireProductionAdmissionRecord authenticates the immutable admission
// authority for terminal history without re-applying mutable current policy.
// A recorded terminal has already crossed the current-policy acceptance gate;
// replay only proves that the durable record still agrees with the driver.
func (e *Engine) requireProductionAdmissionRecord(
	ctx context.Context, invocationID domain.InvocationID,
) error {
	err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.GetExecutionAdmissionRecord(ctx, invocationID)
		return err
	})
	if err != nil {
		return fmt.Errorf("production invocation %q has no authentic admission record: %w",
			invocationID, err)
	}
	return nil
}

// requireProductionAdmissible applies the same current-policy reconstruction
// gate as the legacy lane, but also requires the admission row to exist.
// Production dispatch cannot create an attempt without that row, so absence
// later is lost audit authority, never a pre-admission legacy attempt.
func (e *Engine) requireProductionAdmissible(
	ctx context.Context, invocationID domain.InvocationID,
) error {
	_, err := e.productionAdmission(ctx, invocationID)
	return err
}

func (e *Engine) productionAdmission(
	ctx context.Context, invocationID domain.InvocationID,
) (domain.ExecutionAdmission, error) {
	found := false
	var admission domain.ExecutionAdmission
	err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		loaded, foundNow, err := tx.LookupExecutionAdmission(ctx, invocationID)
		admission = loaded
		found = foundNow
		return err
	})
	if err != nil {
		return domain.ExecutionAdmission{}, fmt.Errorf("production invocation %q is no longer admissible: %w",
			invocationID, err)
	}
	if !found {
		return domain.ExecutionAdmission{}, fmt.Errorf("production invocation %q has no durable admission: %w",
			invocationID, store.ErrNotFound)
	}
	return admission, nil
}

// recordProductionTerminal commits one terminal record and, for anything but
// a completed result, the deterministic execution_failure item in the same
// transaction. A converged replay (row already present with identical bytes)
// writes nothing.
func (e *Engine) recordProductionTerminal(
	ctx context.Context, run domain.Run, terminal productionTerminalRecord,
) (bool, error) {
	return e.recordProductionTerminalWithAuthority(ctx, run, terminal, true)
}

func (e *Engine) recordProductionTerminalWithAuthority(
	ctx context.Context,
	run domain.Run,
	terminal productionTerminalRecord,
	requireCurrentAdmission bool,
) (bool, error) {
	return e.recordProductionTerminalWithCompletion(
		ctx, run, terminal, requireCurrentAdmission, nil,
	)
}

func (e *Engine) recordProductionTerminalWithCompletion(
	ctx context.Context,
	run domain.Run,
	terminal productionTerminalRecord,
	requireCurrentAdmission bool,
	completion *signet.PublicationReevaluationCompletion,
) (bool, error) {
	payload, err := json.Marshal(terminal)
	if err != nil {
		return false, fmt.Errorf("record terminal for %q: %w", terminal.InvocationID, err)
	}
	inserted := false
	err = e.store.Write(ctx, func(tx *store.WriteTx) error {
		if terminal.Status == exec.StatusCompleted {
			// Repeat the current-policy gate in the terminal write
			// transaction. The earlier check closes the driver-call window;
			// this one prevents a trust-profile update from interleaving
			// between that check and acceptance.
			var err error
			if requireCurrentAdmission {
				_, err = tx.GetExecutionAdmission(ctx, terminal.InvocationID)
			} else {
				_, err = tx.GetExecutionAdmissionRecord(ctx, terminal.InvocationID)
			}
			if err != nil {
				return err
			}
		}
		entry, insertedNow, err := tx.RecordInbox(
			ctx, string(terminal.InvocationID), kindProductionStageTerminal, payload)
		if err != nil {
			return err
		}
		if !insertedNow {
			if entry.Kind != kindProductionStageTerminal || !bytes.Equal(entry.Payload, payload) {
				return fmt.Errorf("terminal record for %q disagrees with stored row: %w",
					terminal.InvocationID, domain.ErrImmutableTransition)
			}
		}
		inserted = insertedNow
		if insertedNow && terminal.Status == exec.StatusBlocked {
			if err := e.recordProductionQuestion(ctx, tx, run, terminal, time.Now().UTC()); err != nil {
				return err
			}
		}
		if insertedNow && terminal.Status != exec.StatusCompleted && terminal.Status != exec.StatusBlocked {
			createdAt := time.Now().UTC()
			facts, err := executionFailureFacts(
				ctx, tx, terminal.InvocationID, terminal.Status, terminal.Summary,
				createdAt, domain.StageNameImplementation,
			)
			if err != nil {
				return err
			}
			if facts != nil && e.admission != nil {
				policy, policyErr := tx.GetResolvedPolicy(ctx, run.ID)
				admission, admissionErr := tx.GetExecutionAdmissionRecord(ctx, terminal.InvocationID)
				if policyErr != nil {
					return policyErr
				}
				if admissionErr != nil {
					return admissionErr
				}
				manifests, manifestErr := domain.CapabilityManifestsFromPolicy(policy)
				if manifestErr == nil {
					for _, manifest := range manifests {
						if manifest.EgressProfile != admission.EgressProfile &&
							slices.Contains(e.admission.environment.EnforceableEgressProfiles, manifest.EgressProfile) {
							facts.OfferedManifests = append(facts.OfferedManifests, manifest.Offer())
						}
					}
				}
			}
			names, err := tx.DisplayNamesFor(ctx, run.ProjectID, domain.Subject{
				Type: domain.SubjectRun, ID: domain.SubjectID(run.ID), RunID: &run.ID,
			})
			if err != nil {
				return err
			}
			item, err := productionFailureItem(run, terminal, createdAt, facts, names)
			if err != nil {
				return err
			}
			if err := tx.PutAttentionItem(ctx, item); err != nil {
				return err
			}
		}
		// The terminal milestone rides the recording transaction; only the
		// closed status class crosses into the projection, never the head,
		// artifacts, or summary (issue #394).
		if insertedNow {
			observed, ok := observedStatus(terminal.Status)
			if !ok {
				return fmt.Errorf("terminal status %q cannot be observed: %w",
					terminal.Status, domain.ErrParentKeyMismatch)
			}
			invocation := terminal.InvocationID
			if err := tx.AppendRunMilestone(ctx, domain.RunMilestone{
				RunID: run.ID, Kind: domain.MilestoneTerminalRecorded,
				InvocationID: &invocation, Terminal: &observed,
				RecordedAt: time.Now().UTC(),
			}); err != nil {
				return err
			}
		}
		if completion != nil {
			completionTerminal := terminal
			completionTerminal.InvocationID = completion.TerminalInvocationID
			completionTerminalPayload, err := json.Marshal(completionTerminal)
			if err != nil {
				return err
			}
			completionTerminalEntry, completionTerminalInserted, err := tx.RecordInbox(
				ctx, string(completion.TerminalInvocationID), kindProductionStageTerminal,
				completionTerminalPayload)
			if err != nil {
				return err
			}
			if completionTerminalEntry.Kind != kindProductionStageTerminal ||
				!bytes.Equal(completionTerminalEntry.Payload, completionTerminalPayload) {
				return fmt.Errorf("publication reevaluation terminal %q disagrees with stored row: %w",
					completion.TerminalInvocationID, domain.ErrImmutableTransition)
			}
			completionPayload, err := signet.EncodePublicationReevaluationCompletion(*completion)
			if err != nil {
				return err
			}
			key := signet.PublicationReevaluationCompletionKey(completion.CommandID)
			marker, markerInserted, err := tx.RecordDispatchedOutbox(
				ctx, key, signet.PublicationReevaluationCompletedKind, completionPayload)
			if err != nil {
				return err
			}
			if markerInserted != completionTerminalInserted || marker.Kind != signet.PublicationReevaluationCompletedKind ||
				!bytes.Equal(marker.Payload, completionPayload) {
				return fmt.Errorf("publication reevaluation completion %q disagrees with terminal record: %w",
					key, domain.ErrImmutableTransition)
			}
		}
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("record terminal for %q: %w", terminal.InvocationID, err)
	}
	if inserted && terminal.Status != exec.StatusCompleted && terminal.Status != exec.StatusBlocked &&
		e.inference != nil {
		// A diagnostic claim is advisory-only. Failure to produce or retain one
		// cannot roll back the durable failure fact or make the engine unavailable.
		_ = e.inference.DiagnoseExecutionFailure(ctx, inference.DiagnosticInput{
			Project: string(run.ProjectID), RootLineage: string(run.ID), RunID: string(run.ID),
			FailureClass: string(terminal.Status), FailingStep: string(terminal.StageID),
			Reason: terminal.Summary,
		})
	}
	return inserted && terminal.Status == exec.StatusCompleted, nil
}

// recordProductionDeliveryRefusal closes one deterministic pre-start refusal.
// A fresh refusal has no admission to close, while a replay of a pre-fix
// admitted attempt records the standard non-export outcome so identity
// capacity and restart reconstruction use the same authority as every other
// failed execution. The attention item and dispatched marker commit beside
// that authority, leaving no successor task for an invocation that never ran.
func (e *Engine) recordProductionDeliveryRefusal(
	ctx context.Context,
	run domain.Run,
	stage domain.Stage,
	invocationID domain.InvocationID,
	summary string,
) error {
	terminal := productionTerminalRecord{
		InvocationID: invocationID,
		RunID:        run.ID,
		StageID:      stage.ID,
		Status:       exec.StatusFailed,
		Summary:      "Input delivery was refused before driver start: " + summary,
	}
	return e.store.Write(ctx, func(tx *store.WriteTx) error {
		current, err := tx.GetRun(ctx, run.ID)
		if err != nil {
			return err
		}
		currentStage, ok := productionStageForInvocation(current, invocationID)
		if !ok || currentStage.ID != stage.ID || current.ProjectID != run.ProjectID {
			return domain.ErrParentKeyMismatch
		}
		hasAttempt := attemptRecorded(current, invocationID)
		admission, admissionErr := tx.GetExecutionAdmissionRecord(ctx, invocationID)
		switch {
		case admissionErr == nil && !hasAttempt:
			return fmt.Errorf("delivery refusal admission has no attempt: %w", domain.ErrParentKeyMismatch)
		case errors.Is(admissionErr, store.ErrNotFound) && hasAttempt:
			return fmt.Errorf("delivery refusal attempt has no admission: %w", domain.ErrParentKeyMismatch)
		case admissionErr != nil && !errors.Is(admissionErr, store.ErrNotFound):
			return admissionErr
		case admissionErr == nil:
			outcome := domain.ExecutionOutcome{
				InvocationID: invocationID,
				AdmissionID:  admission.ID,
				Status:       domain.ExecutionOutcomeFailed,
				Summary:      terminal.Summary,
				RecordedAt:   time.Now().UTC(),
			}
			stored, outcomeErr := tx.GetExecutionOutcomeRecord(ctx, invocationID)
			if outcomeErr == nil {
				outcome.RecordedAt = stored.RecordedAt
				if !reflect.DeepEqual(stored, outcome) {
					return domain.ErrImmutableTransition
				}
			} else if errors.Is(outcomeErr, store.ErrNotFound) {
				if err := tx.RecordExecutionOutcome(ctx, outcome); err != nil {
					return err
				}
			} else {
				return outcomeErr
			}
		}

		itemID := domain.ItemID("execution-failure-" + string(invocationID))
		names, err := tx.DisplayNamesFor(ctx, current.ProjectID, domain.Subject{
			Type: domain.SubjectRun, ID: domain.SubjectID(current.ID), RunID: &current.ID,
		})
		if err != nil {
			return err
		}
		var facts *domain.ExecutionFailureFacts
		if admissionErr == nil {
			facts = &domain.ExecutionFailureFacts{
				Outcome: domain.ExecutionOutcomeFailed, Stage: domain.StageNameImplementation,
				InvocationID: invocationID,
			}
		}
		item, itemErr := tx.GetAttentionItem(ctx, itemID)
		if errors.Is(itemErr, store.ErrNotFound) {
			item, err = productionDeliveryRefusalItem(current, terminal, time.Now().UTC(), facts, names)
			if err != nil {
				return err
			}
			if err := tx.PutAttentionItem(ctx, item); err != nil {
				return err
			}
		} else if itemErr != nil {
			return itemErr
		} else {
			want, err := productionDeliveryRefusalItem(current, terminal, *item.CreatedAt, facts, names)
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(item, want) {
				return domain.ErrImmutableTransition
			}
		}
		return tx.MarkOutboxDispatched(ctx, string(invocationID))
	})
}

// productionFailureItem is the §4 execution_failure notice for a production
// stage that ended without an accepted result. Deterministic identity and
// content, so a replayed pass converges instead of raising a second item.
// Discuss and stop are always executable. A capability retry is offered only
// when the failure facts carry a policy-derived, enforceable alternative.
func productionFailureItem(
	run domain.Run, terminal productionTerminalRecord, createdAt time.Time,
	facts *domain.ExecutionFailureFacts, displayNames *domain.DisplayNames,
) (domain.AttentionItem, error) {
	runID := run.ID
	reason := fmt.Sprintf("Unattended %s stage ended %q without an accepted result.",
		productionStageName, terminal.Status)
	if terminal.Summary != "" {
		reason += " Summary: " + terminal.Summary
	}
	actions := []domain.Action{domain.ActionDiscuss, domain.ActionStop}
	if facts != nil && len(facts.OfferedManifests) != 0 {
		actions = append([]domain.Action{domain.ActionRetryWithCapability}, actions...)
	}
	return domain.NewAttentionItem(domain.AttentionItemInput{
		ID:        domain.ItemID("execution-failure-" + string(terminal.InvocationID)),
		ProjectID: run.ProjectID,
		Subject:   domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(run.ID), RunID: &runID},
		Type:      domain.AttentionExecutionFailure, Priority: domain.PriorityHigh,
		Reason:            reason,
		RequestedDecision: actions,
		ExecutionFailure:  facts,
		DisplayNames:      displayNames,
		ItemVersion:       1, InterruptionClass: domain.InterruptionExceptional,
		CreatedAt: &createdAt,
		Status:    domain.StatusOpen,
	}, nil)
}

// productionDeliveryRefusalItem preserves the acknowledge-only pre-start
// notice. This boundary did not run a stage, so widening it to a conversation
// requires a separate policy decision.
func productionDeliveryRefusalItem(
	run domain.Run, terminal productionTerminalRecord, createdAt time.Time,
	facts *domain.ExecutionFailureFacts, displayNames *domain.DisplayNames,
) (domain.AttentionItem, error) {
	item, err := productionFailureItem(run, terminal, createdAt, facts, displayNames)
	if err != nil {
		return domain.AttentionItem{}, err
	}
	item.RequestedDecision = []domain.Action{domain.ActionAcknowledge}
	return item, item.Validate()
}

// Quarantine reasons for a run whose durable production rows cannot be
// reconstructed. Daemon-authored and fixed: the row is the untrusted input
// here, so the decode error's text — which quotes the stored version, kind,
// and identities — never reaches an operator-facing field (the ward and
// finding rule: never echo the untrusted bytes in the reason). The classified
// reason plus the item's run subject is what an operator needs; the full
// error stays in the daemon's own error path.
const (
	productionQuarantineUnsupportedVersion = "A stored production marker was written by a newer daemon " +
		"than this one. The run is held out of the production lane, and resumes by itself once a " +
		"daemon that can read the marker runs again."
	productionQuarantineUnreadable = "A stored production marker could not be authenticated. " +
		"The run is held out of the production lane, and resumes by itself once the marker " +
		"reconstructs again."
	productionQuarantineUnreadableTask = "A stored production publication task could not be " +
		"reconstructed by this daemon. The run's publication is held, and resumes by itself once a " +
		"daemon that can read the task runs again."
)

// Quarantine notices are per run and per row class: the marker's notice is
// retired when the marker reads again, so a task row's hold must not ride the
// same identity and be retired with it.
const (
	productionMarkerQuarantinePrefix = "production-marker-quarantined-"
	productionTaskQuarantinePrefix   = "production-task-quarantined-"
)

func productionQuarantineItemID(runID domain.RunID) domain.ItemID {
	return productionQuarantineOccurrenceID(productionMarkerQuarantinePrefix, runID, 1)
}

// productionQuarantineReason classifies one marker failure into its operator
// notice, reporting false for every error that is not a marker
// reconstruction failure (a store fault stays loud and retryable).
func productionQuarantineReason(err error) (string, bool) {
	switch {
	case errors.Is(err, errProductionMarkerUnsupportedVersion):
		return productionQuarantineUnsupportedVersion, true
	case errors.Is(err, errProductionMarkerUnreadable):
		return productionQuarantineUnreadable, true
	}
	return "", false
}

// productionQuarantineItem is the §4 execution_failure notice for a run this
// daemon cannot claim, following productionFailureItem: deterministic
// identity and content, so a replayed pass converges instead of raising a
// second item. Stop is the only action offered: nothing an operator can
// decide repairs a stored marker (the repair is a daemon upgrade or the
// row's own removal), retry would re-enter the same failed reconstruction,
// and discuss rides a conversation channel a production run has none of, so
// stop — the boundary's concluding action — is the one this can honour.
func productionQuarantineItem(
	itemID domain.ItemID, runID domain.RunID, projectID domain.ProjectID, reason string,
	names ...*domain.DisplayNames,
) (domain.AttentionItem, error) {
	subjectRunID := runID
	var displayNames *domain.DisplayNames
	if len(names) != 0 {
		displayNames = names[0]
	}
	return domain.NewAttentionItem(domain.AttentionItemInput{
		ID:        itemID,
		ProjectID: projectID,
		Subject: domain.Subject{
			Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &subjectRunID,
		},
		Type: domain.AttentionExecutionFailure, Priority: domain.PriorityHigh,
		Reason:            reason,
		RequestedDecision: []domain.Action{domain.ActionStop},
		DisplayNames:      displayNames,
		ItemVersion:       1, InterruptionClass: domain.InterruptionExceptional,
		CreatedAt: nil,
		Status:    domain.StatusOpen,
	}, nil)
}

// quarantineProductionMarker records the durable notice that one run left the
// production lane, and reports whether cause was a marker failure at all.
// Quarantine is this daemon's own classification, deliberately not a stored
// outbox status: its main cause is a downgrade past a newer marker version,
// which the matching upgrade reverses, and a durable status change would
// strand a marker that is authentic under the daemon that wrote it.
//
// The notice is written once and then left alone: a later pass that finds it
// present writes nothing, so an operator's acknowledgement is never
// overwritten and the item is never duplicated. A store fault while recording
// it is returned, because a run must not leave the lane unrecorded.
func quarantineProductionMarker(
	ctx context.Context,
	st *store.Store,
	attention attentionService,
	runID domain.RunID,
	projectID domain.ProjectID,
	cause error,
) (bool, error) {
	reason, ok := productionQuarantineReason(cause)
	if !ok {
		return false, nil
	}
	err := recordProductionQuarantine(
		ctx, st, attention, productionMarkerQuarantinePrefix, runID, projectID, reason)
	return true, err
}

// productionQuarantineOccurrenceID names the nth notice for one run and row
// class. Deterministic, so a repeated pass converges on the occurrence it
// already recorded, and distinct, because a concluded notice cannot reopen
// (a terminal item_status is final) and reusing its identity would leave a
// recurring quarantine holding a run behind nothing but a historical record.
//
// The occurrence sits between the class prefix and the run id, never appended
// after it. A run id is validated only as non-empty, so appending would make
// run "foo"'s second notice collide with run "foo-2"'s first, and the
// mismatched subject that collision produces is an error on a path whose
// whole purpose is to keep the reconcile loop running. Splitting on the first
// "-" after a digit run is unambiguous, so this form is injective over
// (occurrence, run id).
func productionQuarantineOccurrenceID(
	prefix string, runID domain.RunID, occurrence int,
) domain.ItemID {
	return domain.ItemID(fmt.Sprintf("%s%d-%s", prefix, occurrence, runID))
}

// recordProductionQuarantine writes one run's quarantine notice, converging
// on the notice already there. An open notice for this run is the record, and
// a later pass writes nothing, so an operator's decision is never overwritten
// and the item is never duplicated; a *concluded* notice is history, and the
// current hold gets its own. A store fault while recording it is returned,
// because a run must not leave the lane unrecorded.
func recordProductionQuarantine(
	ctx context.Context,
	st *store.Store,
	attention attentionService,
	prefix string,
	runID domain.RunID,
	projectID domain.ProjectID,
	reason string,
) error {
	subject := domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID}
	names, err := displayNames(ctx, st, projectID, subject)
	if err != nil {
		return fmt.Errorf("derive quarantine display names for run %q: %w", runID, err)
	}
	// The walk stops at the first slot that is free or open, so it only steps
	// over notices an operator has concluded. That history grows one entry per
	// repair, never per pass, so there is no cap: a bound here would have to
	// choose between failing, which ends the reconcile loop this path exists
	// to keep running, and holding the run behind no current notice at all.
	for occurrence := 1; ; occurrence++ {
		itemID := productionQuarantineOccurrenceID(prefix, runID, occurrence)
		current, found, err := readProductionQuarantineItem(ctx, st, itemID)
		if err != nil {
			return fmt.Errorf("read quarantine item for run %q: %w", runID, err)
		}
		if found {
			if current.Status == domain.StatusOpen {
				return refreshProductionQuarantine(ctx, attention, current, runID, projectID, reason)
			}
			continue
		}
		item, err := productionQuarantineItem(itemID, runID, projectID, reason, names)
		if err != nil {
			return fmt.Errorf("construct quarantine item for run %q: %w", runID, err)
		}
		createdAt := time.Now().UTC()
		item.CreatedAt = &createdAt
		if err := attention.PutItem(ctx, item); err != nil {
			if !errors.Is(err, store.ErrStaleWrite) && !errors.Is(err, store.ErrImmutableConflict) {
				return fmt.Errorf("create quarantine item for run %q: %w", runID, err)
			}
			// A concurrent pass created this id first. Its content is not
			// assumed to be the notice this one would have written: re-read
			// and check what is actually stored, so a divergent item is never
			// accepted as this run's quarantine record.
			return confirmProductionQuarantineItem(ctx, st, itemID, item, err)
		}
		return nil
	}
}

// refreshProductionQuarantine keeps the open notice describing the hold that
// is actually current. The stored row is re-checked rather than trusted: it
// carries this run's bindings or it is not this run's notice, and its reason
// is rewritten when the condition changes class, so an operator is never left
// reading that an upgrade will repair a marker that has since become
// malformed instead.
func refreshProductionQuarantine(
	ctx context.Context,
	attention attentionService,
	current domain.AttentionItem,
	runID domain.RunID,
	projectID domain.ProjectID,
	reason string,
) error {
	want, err := productionQuarantineItem(
		current.ID, runID, projectID, reason, current.DisplayNames,
	)
	if err != nil {
		return fmt.Errorf("construct quarantine item for run %q: %w", runID, err)
	}
	want.CreatedAt = current.CreatedAt
	if !sameProductionQuarantineBinding(current, want) {
		return fmt.Errorf("quarantine item %q disagrees with run %q: %w",
			current.ID, runID, domain.ErrParentKeyMismatch)
	}
	if sameProductionQuarantineNotice(current, want) {
		return nil
	}
	// Whole-shape rather than field-by-field: the stored row is a
	// reconstruction, so every operator-facing field is re-derived from the
	// current hold, and a row that drifted in priority, offered action, or
	// interruption class is repaired rather than accepted as the record.
	want.Status = domain.StatusOpen
	want.ItemVersion = current.ItemVersion + 1
	want.Timing = current.Timing
	want.ConversationID = current.ConversationID
	want.ExpiresWhen = current.ExpiresWhen
	if err := attention.PutItem(ctx, want); err != nil {
		// A decision or a sibling pass moved the notice on. Either way the
		// repair is cosmetic next to keeping the loop running, and the next
		// pass reconverges from whatever is stored.
		if errors.Is(err, store.ErrStaleWrite) || errors.Is(err, store.ErrImmutableConflict) {
			return nil
		}
		return fmt.Errorf("refresh quarantine item for run %q: %w", runID, err)
	}
	return nil
}

// sameProductionQuarantineBinding reports whether a stored row is bound to the
// run and class the caller is asking about. These are the fields a notice can
// never legitimately change, so a mismatch is a contradiction, not drift.
func sameProductionQuarantineBinding(current, want domain.AttentionItem) bool {
	return current.ProjectID == want.ProjectID && current.Type == want.Type &&
		current.Subject.Type == want.Subject.Type && current.Subject.ID == want.Subject.ID
}

// sameProductionQuarantineNotice reports whether a stored row is the canonical
// notice this lane would write for the current hold, ignoring only the
// lifecycle fields a decision or a delivery legitimately advances. Comparing
// the whole shape is what makes this check closed: a subset check can only
// ever authenticate the fields someone thought to list.
func sameProductionQuarantineNotice(current, want domain.AttentionItem) bool {
	normalized := current
	normalized.Status = want.Status
	normalized.ItemVersion = want.ItemVersion
	normalized.Timing = want.Timing
	normalized.DecidedAt = want.DecidedAt
	normalized.ExpiresWhen = want.ExpiresWhen
	normalized.ConversationID = want.ConversationID
	normalized.Recommendation = want.Recommendation
	normalized.DecisionSurface = want.DecisionSurface
	return reflect.DeepEqual(normalized, want)
}

// confirmProductionQuarantineItem accepts a concurrently created notice only
// when the stored row is this run's quarantine notice: same bindings, one of
// this lane's reasons, and still open. Anything else keeps the write's
// original conflict.
func confirmProductionQuarantineItem(
	ctx context.Context,
	st *store.Store,
	itemID domain.ItemID,
	want domain.AttentionItem,
	conflict error,
) error {
	current, found, err := readProductionQuarantineItem(ctx, st, itemID)
	if err != nil {
		return errors.Join(conflict, err)
	}
	if !found {
		return conflict
	}
	if !sameProductionQuarantineBinding(current, want) || current.Status != domain.StatusOpen {
		return errors.Join(conflict, fmt.Errorf(
			"quarantine item %q disagrees with this run's notice: %w",
			itemID, domain.ErrParentKeyMismatch))
	}
	// Each contender stamps its own candidate before PutItem. Compare the
	// losing candidate using the durable winner's stamp so a legitimate
	// create race converges without weakening the whole-shape check.
	want.CreatedAt = current.CreatedAt
	if !sameProductionQuarantineNotice(current, want) {
		return errors.Join(conflict, fmt.Errorf(
			"quarantine item %q disagrees with this run's notice: %w",
			itemID, domain.ErrParentKeyMismatch))
	}
	return nil
}

// productionQuarantineNoticeFor reports whether a reason is one this row
// class writes. The class matters, not merely lane membership: a marker
// release must not conclude the task row's notice or the reverse.
func productionQuarantineNoticeFor(prefix, reason string) bool {
	if prefix == productionTaskQuarantinePrefix {
		return reason == productionQuarantineUnreadableTask
	}
	if prefix == specificationMarkerQuarantinePrefix {
		return reason == specificationQuarantineUnreadable
	}
	if legacySpecificationQuarantineNoticeFor(prefix, reason) {
		return true
	}
	if prefix == remediationMarkerQuarantinePrefix {
		return reason == remediationQuarantineUnreadable
	}
	if prefix == operatorFeedbackMarkerQuarantinePrefix {
		return reason == operatorFeedbackQuarantineUnreadable
	}
	return reason == productionQuarantineUnsupportedVersion ||
		reason == productionQuarantineUnreadable
}

func readProductionQuarantineItem(
	ctx context.Context, st *store.Store, itemID domain.ItemID,
) (domain.AttentionItem, bool, error) {
	var item domain.AttentionItem
	err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		item, err = tx.GetAttentionItemRecord(ctx, itemID)
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return domain.AttentionItem{}, false, nil
	}
	if err != nil {
		return domain.AttentionItem{}, false, err
	}
	return item, true, nil
}

// releaseProductionQuarantine supersedes the open quarantine notice once the
// hold it reported on has ended, following supersedeBlockedHold. It
// supersedes rather than resolves: nothing was decided, the condition stopped
// holding. A notice an operator already concluded is left alone, and one that
// binds a different run or type is a contradiction, not this run's notice to
// retire.
func releaseProductionQuarantine(
	ctx context.Context,
	st *store.Store,
	attention attentionService,
	prefix string,
	runID domain.RunID,
) error {
	// Bounded by the same concluded-notice history the recorder walks.
	for occurrence := 1; ; occurrence++ {
		itemID := productionQuarantineOccurrenceID(prefix, runID, occurrence)
		item, found, err := readProductionQuarantineItem(ctx, st, itemID)
		if err != nil {
			return fmt.Errorf("read quarantine item for run %q: %w", runID, err)
		}
		if !found {
			return nil
		}
		if item.Status != domain.StatusOpen {
			continue
		}
		// An item under this id that is not this hold's notice is not this
		// path's to conclude. Leaving it alone rather than erroring is the
		// asymmetry with the recorder: there, a divergent item means the hold
		// cannot be recorded and the run would be held silently, which must
		// surface; here it means someone else's item sits at this id, and
		// failing to retire a notice this lane does not own is harmless while
		// erroring would end the reconcile loop.
		if item.Type != domain.AttentionExecutionFailure ||
			item.Subject.Type != domain.SubjectRun ||
			item.Subject.ID != domain.SubjectID(runID) ||
			!productionQuarantineNoticeFor(prefix, item.Reason) {
			return nil
		}
		item.Status = domain.StatusSuperseded
		item.ItemVersion++
		if err := attention.PutItem(ctx, item); err != nil {
			if !errors.Is(err, store.ErrStaleWrite) && !errors.Is(err, store.ErrImmutableConflict) {
				return fmt.Errorf("retire quarantine item for run %q: %w", runID, err)
			}
			// An operator's decision committed between the read and this
			// write. Their conclusion is the release: the notice is already
			// off the operator's queue, and turning that race into an error
			// would end the reconcile loop this whole path exists to keep
			// running.
			return confirmProductionQuarantineRelease(ctx, st, itemID, runID, err)
		}
		return nil
	}
}

// confirmProductionQuarantineRelease treats a lost release race as converged
// only when the stored notice is genuinely concluded; a still-open item means
// the write failed for a reason the race does not explain.
func confirmProductionQuarantineRelease(
	ctx context.Context,
	st *store.Store,
	itemID domain.ItemID,
	runID domain.RunID,
	conflict error,
) error {
	current, found, err := readProductionQuarantineItem(ctx, st, itemID)
	if err != nil {
		return errors.Join(conflict, err)
	}
	if !found || current.Status == domain.StatusOpen {
		return fmt.Errorf("retire quarantine item for run %q: %w", runID, conflict)
	}
	return nil
}

// quarantinePendingProductionMarker records the quarantine for a pending
// dispatch intent, whose run the caller knows only by the row's key. A run
// the store cannot produce is not quarantinable state — there is nothing to
// hold out of the lane and no subject to file the notice under — so it
// reports false and stays the caller's loud failure.
func (e *Engine) quarantinePendingProductionMarker(
	ctx context.Context, runID domain.RunID, cause error,
) (bool, error) {
	var run domain.Run
	if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		run, err = tx.GetRun(ctx, runID)
		return err
	}); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	return quarantineProductionMarker(ctx, e.store, e.signet, run.ID, run.ProjectID, cause)
}

func findProductionStage(run domain.Run) (domain.Stage, bool) {
	for _, stage := range run.Stages {
		if stage.ID == productionStageID(run.ID) && stage.Name == productionStageName {
			return stage, true
		}
	}
	return domain.Stage{}, false
}
