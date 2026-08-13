package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/elaborate"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/intake"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// Label-initiator intake reconciliation (plan §5.11, §5.12, issue #659). A
// standalone process loop, beside the active-resource reconciler, that observes
// each configured initiator's labeled open issues and, per occurrence,
// admits exactly one run proposal (idempotent across restarts and re-observation),
// starts it under WIP caps when the recorded mode authorizes auto_start, and
// supersedes an open proposal whose issue lost the label or closed. It observes
// no issue content: only the issue number, its lifecycle state, and label
// presence flow in, so §5.13's "no event bodies, target identities, or
// authority" holds structurally. Decisions and rationale:
// devlog/2026-08-13-2210-label-intake-reconciliation.md.

const (
	// defaultIntakeInterval is the reconciliation cadence. Label intake is a
	// polling reconciler (webhooks are Phase 2), so it re-observes on this
	// interval; conditional requests keep an unchanged labeled set cheap.
	defaultIntakeInterval = 15 * time.Minute

	// intakeForgeResearchHost is the forge API host the loop guarantees in the
	// resolved policy's research allowlist, so the elaborator's issue fetch (the
	// only place issue content legitimately enters, as research) is admitted
	// regardless of operator allowlist configuration.
	intakeForgeResearchHost = "https://api.github.com"
)

// errIntakeRepositoryRebound marks a pass whose initiator repository name no
// longer resolves to the configured numeric identity (§5.18). It fails the
// pass closed so no occurrence is admitted or superseded under a rebound name.
var errIntakeRepositoryRebound = errors.New(
	"initiator repository name resolves to a different repository id")

// errIntakeForgeHostNotAllowed marks an initiator whose research allowlist does
// not admit the forge host, so the elaborator's issue fetch could not succeed.
// Admission fails closed rather than silently widening the operator's policy.
var errIntakeForgeHostNotAllowed = errors.New(
	"initiator research allowlist does not admit the forge host")

// errIntakeDeferDepartureRetire signals that a departed occurrence has a start
// decided but not yet launched, so it must not be retired this pass. It is not a
// failure: the next pass launches the decided start (launchDecidedDeparture) and
// then retires the occurrence.
var errIntakeDeferDepartureRetire = errors.New(
	"deferring departure retire until the decided start launches")

// intakeInitiator is one configured label initiator: which (repository, label)
// to observe, the project its runs belong to, the rein-resolved policy keys, and
// the daemon's publication identity. Occurrence, admission, mode, WIP, and
// supersession decisions are the loop's; this is the static configuration a
// workflow definition supplies (the rein resolver and YAML parsing are a later
// unit, so this is populated directly for now).
type intakeInitiator struct {
	Repo              string
	RepositoryID      int64
	Label             string
	ProjectID         domain.ProjectID
	PolicyKeys        []domain.PolicyKey
	CommitAuthor      engine.ProductionCommitAuthor
	ExpectedCostUnits int
	ComponentCount    int
}

func (i intakeInitiator) validate() error {
	switch {
	case i.Repo == "":
		return errors.New("intake initiator repo is required")
	case i.RepositoryID <= 0:
		return fmt.Errorf("intake initiator repository_id %d must be positive", i.RepositoryID)
	case i.Label == "":
		return errors.New("intake initiator label is required")
	case i.ProjectID == "":
		return errors.New("intake initiator project is required")
	case len(i.PolicyKeys) == 0:
		return errors.New("intake initiator policy keys are required")
	}
	return nil
}

// intakeReconciler drives the label-intake loop over a fixed set of initiators.
// It owns no durable state: occurrences, admissions, refusals, and supersessions
// all live in the store, so a restart resumes convergence from the tables.
type intakeReconciler struct {
	store        *store.Store
	blobs        *signet.BlobStore
	engine       *engine.Engine
	attention    *signet.Service
	observeLabel labelIssueObserver
	observeIssue issueObserver
	// evictLabel drops the label-issue conditional-request cache for one
	// (repo, label) after a durable intake write fails, so the next tick
	// re-observes unconditionally instead of riding a 304. Optional.
	evictLabel func(repo, label string)
	initiators []intakeInitiator
	now        func() time.Time
}

// Run drives the loop: an immediate startup pass, then one pass per interval.
// A per-initiator or per-occurrence failure is logged and isolated, never
// loop-fatal, so a transient forge or store error for one issue does not stop
// intake for the others or bring down the daemon.
func (r *intakeReconciler) Run(ctx context.Context, interval time.Duration, logger *slog.Logger) error {
	if interval <= 0 {
		return fmt.Errorf("intake reconcile interval %s must be positive", interval)
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	logger = logger.With("subsystem", "label-intake")
	logger.Info("label-intake reconciler started", "interval", interval, "initiators", len(r.initiators))
	r.reconcile(ctx, logger)
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			logger.Info("label-intake reconciler stopped")
			return nil
		case <-timer.C:
		}
		r.reconcile(ctx, logger)
		timer.Reset(interval)
	}
}

func (r *intakeReconciler) reconcile(ctx context.Context, logger *slog.Logger) {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	for _, init := range r.initiators {
		if ctx.Err() != nil {
			return
		}
		if err := r.reconcileInitiator(ctx, init, logger); err != nil {
			if ctx.Err() != nil {
				return
			}
			if r.evictLabel != nil {
				r.evictLabel(init.Repo, init.Label)
			}
			logger.Error("label-intake initiator pass failed",
				"repo", init.Repo, "label", init.Label, "error", err)
		}
	}
}

func (r *intakeReconciler) reconcileInitiator(ctx context.Context, init intakeInitiator, logger *slog.Logger) error {
	if err := init.validate(); err != nil {
		return err
	}
	obs, err := r.observeLabel(ctx, init.Repo, init.Label)
	if err != nil {
		return fmt.Errorf("observe labeled issues: %w", err)
	}
	// Fail closed when the initiator repository name no longer resolves to the
	// configured numeric identity (§5.18): admitting or superseding under a
	// rebound name would record occurrence, project, and work-unit authority for
	// the wrong repository. The name alone can never surface the rebinding, so
	// nothing is processed this pass until the observed id agrees.
	if obs.RepositoryID != init.RepositoryID {
		return fmt.Errorf(
			"label scan for %s observed repository id %d, configured %d (repository rebound?): %w",
			init.Repo, obs.RepositoryID, init.RepositoryID, errIntakeRepositoryRebound)
	}
	labeledOpen := make(map[int]bool, len(obs.Issues))
	numbers := make([]int, 0, len(obs.Issues))
	for _, issue := range obs.Issues {
		// The query filters open+labeled, but reconfirm the observation shape
		// before allocating an occurrence: a non-labeled or non-open entry is
		// not an intake subject.
		if !issue.HasLabel || !strings.EqualFold(issue.State, "open") {
			continue
		}
		if !labeledOpen[issue.Number] {
			labeledOpen[issue.Number] = true
			numbers = append(numbers, issue.Number)
		}
	}
	// Deterministic order so the WIP cap fills predictably across a pass and a
	// re-observation converges rather than racing map iteration order.
	slices.Sort(numbers)
	for _, number := range numbers {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := r.reconcilePresent(ctx, init, number); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			logger.Error("label-intake occurrence pass failed",
				"repo", init.Repo, "label", init.Label, "issue", number, "error", err)
		}
	}
	return r.reconcileDepartures(ctx, init, labeledOpen, logger)
}

// reconcilePresent advances the active occurrence for a labeled-open issue,
// admits it when unadmitted, and applies the mode/WIP/start decision.
func (r *intakeReconciler) reconcilePresent(ctx context.Context, init intakeInitiator, issueNumber int) error {
	var occurrence domain.IntakeOccurrence
	if err := r.store.Write(ctx, func(tx *store.WriteTx) error {
		var err error
		occurrence, _, err = tx.AllocateNextIntakeOccurrence(
			ctx, init.Repo, init.RepositoryID, issueNumber, init.Label, r.now())
		return err
	}); err != nil {
		return fmt.Errorf("allocate occurrence: %w", err)
	}
	if occurrence.Admission == nil {
		var err error
		occurrence, err = r.admit(ctx, init, occurrence)
		if err != nil {
			return fmt.Errorf("admit occurrence: %w", err)
		}
	}
	return r.decide(ctx, init, occurrence)
}

// admit performs the per-occurrence admission orchestration, each step
// idempotent and replay-convergent: derive the run identities, build the
// resolved policy, synthesize and store the coordinates-only work-item document,
// persist the reserved elaboration run, register the project authority, mint the
// declaration, admit the run proposal under the occurrence's derived key, and
// bind the admission. See the decision note for the reserved-run adoption model.
func (r *intakeReconciler) admit(
	ctx context.Context, init intakeInitiator, occurrence domain.IntakeOccurrence,
) (domain.IntakeOccurrence, error) {
	implementationRunID := intakeImplementationRunID(occurrence)
	elaborationRunID, err := engine.ElaborationRunIDForImplementation(implementationRunID)
	if err != nil {
		return domain.IntakeOccurrence{}, err
	}
	// The forge host must be in the operator's own research allowlist with
	// authentic provenance; the loop never silently widens operator-attributed
	// policy (a rewritten value under an unchanged provenance would falsely
	// attest an egress the source never authorized).
	if err := requireForgeResearchHost(init.PolicyKeys); err != nil {
		return domain.IntakeOccurrence{}, err
	}
	resolvedPolicy, err := domain.NewResolvedPolicy(elaborationRunID, init.PolicyKeys)
	if err != nil {
		return domain.IntakeOccurrence{}, fmt.Errorf("resolve policy: %w", err)
	}
	workItemBody := intakeWorkItemDocument(occurrence)
	workItem, err := submissionArtifact(domain.ArtifactKindSpecification, domain.Digest(contentaddr.Sum(workItemBody)))
	if err != nil {
		return domain.IntakeOccurrence{}, err
	}
	policyBody, err := json.Marshal(resolvedPolicy.Keys)
	if err != nil {
		return domain.IntakeOccurrence{}, fmt.Errorf("encode policy keys: %w", err)
	}
	policyArtifact, err := submissionArtifact(domain.ArtifactKindPolicy, resolvedPolicy.Digest)
	if err != nil {
		return domain.IntakeOccurrence{}, err
	}
	project, err := domain.NewProject(init.ProjectID, init.Repo, init.RepositoryID)
	if err != nil {
		return domain.IntakeOccurrence{}, fmt.Errorf("project authority: %w", err)
	}
	// Bytes land before metadata: an artifact row must never name a digest the
	// blob store cannot serve, since admission materializes stage inputs (the
	// work-item document in the specification role) by digest.
	if _, err := r.blobs.Put(workItem.Digest, bytes.NewReader(workItemBody)); err != nil {
		return domain.IntakeOccurrence{}, fmt.Errorf("store work-item bytes: %w", err)
	}
	if _, err := r.blobs.Put(policyArtifact.Digest, bytes.NewReader(policyBody)); err != nil {
		return domain.IntakeOccurrence{}, fmt.Errorf("store policy bytes: %w", err)
	}
	reservedRun := engine.NewReservedElaborationRun(
		elaborationRunID, init.ProjectID, workItem.Digest, resolvedPolicy.Digest)
	if err := r.store.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutArtifact(ctx, workItem); err != nil {
			return err
		}
		if err := tx.PutArtifact(ctx, policyArtifact); err != nil {
			return err
		}
		if err := tx.PutRun(ctx, reservedRun); err != nil {
			return err
		}
		if err := tx.PutResolvedPolicy(ctx, resolvedPolicy); err != nil {
			return err
		}
		if err := tx.RegisterProject(ctx, project); err != nil {
			return err
		}
		_, err := tx.MintIntakeDeclaration(ctx,
			occurrence.RepositoryID, occurrence.IssueNumber, occurrence.Label, occurrence.Ordinal, elaborationRunID)
		return err
	}); err != nil {
		return domain.IntakeOccurrence{}, fmt.Errorf("reserve run and mint declaration: %w", err)
	}
	declaredPaths := domain.CanonicalDeclaredPaths(resolvedPolicy)
	admission, err := r.engine.AdmitProposal(ctx, engine.ProposalAdmission{
		ProjectID:       init.ProjectID,
		ProposalBatchID: intakeProposalBatchID(occurrence),
		AdmissionKey:    occurrence.ProposalAdmissionKey(),
		Kind:            domain.EffectRunProposal,
		Parameters: domain.RunProposalParameters{
			SubjectHandle:     domain.OpaqueSubjectHandle(domain.WorkUnitIDForRun(elaborationRunID)),
			Intent:            domain.RunProposalIntentImplement,
			ExpectedCostUnits: init.ExpectedCostUnits,
			Scope: domain.RunProposalScope{
				ComponentCount:    max(init.ComponentCount, 1),
				DeclaredPathCount: len(declaredPaths),
			},
		},
		// A label proposal's subject is fixed to the occurrence's own issue, so
		// start_with_changes (subject revision) is not a label-intake flow and
		// offering it would strand the occurrence (decision note, Decision 4).
		RequestedDecision: []domain.Action{
			domain.ActionStart, domain.ActionDecline, domain.ActionSnooze,
		},
	})
	if err != nil {
		return domain.IntakeOccurrence{}, fmt.Errorf("admit proposal: %w", err)
	}
	var bound domain.IntakeOccurrence
	if err := r.store.Write(ctx, func(tx *store.WriteTx) error {
		var err error
		bound, err = tx.BindIntakeAdmission(ctx,
			occurrence.RepositoryID, occurrence.IssueNumber, occurrence.Label, occurrence.Ordinal,
			admission.Instance.ID, policyArtifact.ID)
		return err
	}); err != nil {
		return domain.IntakeOccurrence{}, fmt.Errorf("bind admission: %w", err)
	}
	return bound, nil
}

// decide applies the mode/WIP/start decision to an admitted occurrence. It is
// the single entry for both start paths: a proposal already decided start (by an
// operator on a propose card, or by a prior auto_start pass) is launched; an open
// undecided card is either left for the operator (propose), refused (downgraded
// mode or an exhausted WIP cap), or auto-started.
func (r *intakeReconciler) decide(ctx context.Context, init intakeInitiator, occurrence domain.IntakeOccurrence) error {
	if occurrence.Admission == nil {
		return nil
	}
	elaborationRunID := occurrence.Admission.Subject.ElaborationRunID
	started, err := r.elaborationStarted(ctx, elaborationRunID)
	if err != nil {
		return err
	}
	if started {
		return nil
	}
	decidedStart, err := r.proposalDecidedStart(ctx, occurrence.Admission.ProposalInstanceID, occurrence.Admission.ProposalDigest)
	if err != nil {
		return err
	}
	if decidedStart {
		// An operator decided start on a propose card, or a prior pass recorded
		// the auto_start decision but crashed before launching: launch now
		// (SubmitElaborationRun converges).
		return r.launch(ctx, init, occurrence)
	}
	// The card is open and undecided. A prior durable refusal leaves it an
	// ordinary propose card; do not re-decide it.
	if occurrence.Refusal != nil {
		return nil
	}
	var resolved domain.ResolvedPolicy
	if err := r.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		resolved, err = tx.GetResolvedPolicy(ctx, elaborationRunID)
		return err
	}); err != nil {
		return err
	}
	policy, err := intake.ParseIntakePolicy(resolved)
	if err != nil {
		return fmt.Errorf("parse intake policy: %w", err)
	}
	switch {
	case policy.Downgraded():
		// A preset-sourced auto_start is not authorized: record the durable
		// downgrade and leave the card an ordinary proposal.
		return r.refuse(ctx, occurrence, domain.IntakeRefusalModeNotAuthorized)
	case !policy.AutoStartAuthorized():
		// propose: leave the card for the operator.
		return nil
	default:
		return r.autoStart(ctx, init, occurrence, policy)
	}
}

// autoStart applies an authorized auto_start: it refuses on a missing or stale
// subject input or an exhausted WIP cap (leaving the card an ordinary proposal),
// otherwise records the daemon-attributed start decision and launches.
func (r *intakeReconciler) autoStart(
	ctx context.Context, init intakeInitiator, occurrence domain.IntakeOccurrence, policy intake.IntakePolicy,
) error {
	missing, stale, err := r.subjectInputStatus(ctx, occurrence)
	if err != nil {
		return err
	}
	switch {
	case missing:
		return r.refuse(ctx, occurrence, domain.IntakeRefusalSubjectInputMissing)
	case stale:
		return r.refuse(ctx, occurrence, domain.IntakeRefusalSubjectInputStale)
	}
	// The WIP count and its consequence run under one write so they cannot race
	// another writer. The occurrence's own reserved run is always active here
	// (just admitted), so the cap bounds the count of OTHER active project runs.
	start := false
	if err := r.store.Write(ctx, func(tx *store.WriteTx) error {
		active, err := tx.CountActiveProjectRuns(ctx, occurrence.Admission.Subject.ProjectID)
		if err != nil {
			return err
		}
		others := max(active-1, 0)
		if policy.WIPCapExhausted(others) {
			_, err := tx.RecordIntakeRefusal(ctx,
				occurrence.RepositoryID, occurrence.IssueNumber, occurrence.Label, occurrence.Ordinal,
				domain.IntakeRefusalWIPCapExhausted, r.now())
			return err
		}
		start = true
		return nil
	}); err != nil || !start {
		return err
	}
	// Record the daemon-attributed start through the decision ledger (GQ2), then
	// launch only the decision this call actually made. If an operator declined
	// the card or a departure superseded it between the WIP gate and here,
	// StartRunProposalUnattended records no start and reports started=false: an
	// explicit non-start decision must never become an autonomous run. A
	// concurrent decided-start is relaunched by decide's already-decided path on
	// the next pass; launch is idempotent.
	started, err := r.attention.StartRunProposalUnattended(ctx,
		domain.ItemID(occurrence.Admission.ProposalInstanceID), intakeStartCommandID(occurrence))
	if err != nil {
		return fmt.Errorf("record auto_start decision: %w", err)
	}
	if !started {
		return nil
	}
	return r.launch(ctx, init, occurrence)
}

func (r *intakeReconciler) launch(ctx context.Context, init intakeInitiator, occurrence domain.IntakeOccurrence) error {
	spec, err := r.startSpec(ctx, init, occurrence)
	if err != nil {
		return err
	}
	if _, err := engine.SubmitElaborationRun(ctx, r.store, spec); err != nil {
		return fmt.Errorf("submit elaboration run: %w", err)
	}
	return nil
}

// startSpec reconstructs the issue-subject ElaborationRunSpec from the admitted
// occurrence, deterministically, so a launch and its replays submit identical
// bytes. The work-item artifact id and the publication are pure functions of the
// occurrence coordinates and the initiator identity; the resolved policy is the
// one persisted at admission.
func (r *intakeReconciler) startSpec(
	ctx context.Context, init intakeInitiator, occurrence domain.IntakeOccurrence,
) (engine.ElaborationRunSpec, error) {
	elaborationRunID := occurrence.Admission.Subject.ElaborationRunID
	var resolved domain.ResolvedPolicy
	if err := r.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		resolved, err = tx.GetResolvedPolicy(ctx, elaborationRunID)
		return err
	}); err != nil {
		return engine.ElaborationRunSpec{}, err
	}
	workItem, err := submissionArtifact(domain.ArtifactKindSpecification,
		domain.Digest(contentaddr.Sum(intakeWorkItemDocument(occurrence))))
	if err != nil {
		return engine.ElaborationRunSpec{}, err
	}
	issue := occurrence.IssueNumber
	return engine.ElaborationRunSpec{
		ElaborationRunID:    elaborationRunID,
		ImplementationRunID: intakeImplementationRunID(occurrence),
		ProjectID:           occurrence.Admission.Subject.ProjectID,
		SourceArtifactID:    workItem.ID,
		PolicyArtifactID:    occurrence.Admission.Subject.PolicyArtifactID,
		ResolvedPolicy:      resolved,
		Publication:         intakePublication(init, occurrence),
		WorkUnit: &domain.WorkUnitDeclarationInput{
			CompletionCriterion: domain.CompletionBoundPRMerged,
			BoundIssue:          &issue,
			DeclaredPaths:       domain.CanonicalDeclaredPaths(resolved),
		},
		Source: domain.ElaborationSource{
			Kind: domain.ElaborationSourceIssueSubject,
			IssueSubject: &domain.IssueSubjectRef{
				Repo: occurrence.Repo, RepositoryID: occurrence.RepositoryID, IssueNumber: occurrence.IssueNumber,
			},
		},
	}, nil
}

// refuse records a durable start refusal and leaves the admitted card an
// ordinary proposal.
func (r *intakeReconciler) refuse(
	ctx context.Context, occurrence domain.IntakeOccurrence, reason domain.IntakeStartRefusalReason,
) error {
	return r.store.Write(ctx, func(tx *store.WriteTx) error {
		_, err := tx.RecordIntakeRefusal(ctx,
			occurrence.RepositoryID, occurrence.IssueNumber, occurrence.Label, occurrence.Ordinal,
			reason, r.now())
		return err
	})
}

// elaborationStarted reports whether the reserved elaboration run's iteration-1
// dispatch marker exists, i.e. a start has already launched it.
func (r *intakeReconciler) elaborationStarted(ctx context.Context, elaborationRunID domain.RunID) (bool, error) {
	present, err := engine.HasElaborationDispatchMarker(ctx, r.store, elaborationRunID)
	if err != nil {
		return false, fmt.Errorf("inspect elaboration start: %w", err)
	}
	return present, nil
}

// proposalDecidedStart reports whether the occurrence's admitted proposal has a
// recorded start decision (from an operator or a prior auto_start pass) whose
// card is no longer open.
func (r *intakeReconciler) proposalDecidedStart(
	ctx context.Context, instanceID domain.ProposalInstanceID, admittedDigest domain.Digest,
) (bool, error) {
	decided := false
	err := r.store.Read(ctx, func(tx *store.ReadTx) error {
		// Authenticate against the decision ledger, not the decoded item status:
		// a genuine start is a digest-bound effect_proposal_decisions row, so a
		// forged or tampered resolved status cannot launch a run autonomously.
		var err error
		decided, err = tx.AuthenticateStartDecision(ctx, instanceID, admittedDigest)
		return err
	})
	if err != nil {
		return false, fmt.Errorf("inspect proposal decision: %w", err)
	}
	return decided, nil
}

// subjectInputStatus re-checks the one elaboration input the admission binding
// names (the policy artifact) plus the reserved run's work-item source at start
// time. The read re-gate deliberately tolerates later unavailability (#720), so
// the start records subject_input_missing / subject_input_stale here rather than
// dispatching against an absent or superseded input.
func (r *intakeReconciler) subjectInputStatus(
	ctx context.Context, occurrence domain.IntakeOccurrence,
) (missing, stale bool, err error) {
	elaborationRunID := occurrence.Admission.Subject.ElaborationRunID
	workItem, err := submissionArtifact(domain.ArtifactKindSpecification,
		domain.Digest(contentaddr.Sum(intakeWorkItemDocument(occurrence))))
	if err != nil {
		return false, false, err
	}
	readErr := r.store.Read(ctx, func(tx *store.ReadTx) error {
		run, err := tx.GetRun(ctx, elaborationRunID)
		if err != nil {
			return err
		}
		policyArtifact, err := tx.GetArtifact(ctx, occurrence.Admission.Subject.PolicyArtifactID)
		if errors.Is(err, store.ErrNotFound) {
			missing = true
			return nil
		}
		if err != nil {
			return err
		}
		if policyArtifact.Type != domain.ArtifactKindPolicy ||
			policyArtifact.Digest != occurrence.Admission.Subject.ResolvedPolicyDigest {
			stale = true
			return nil
		}
		source, err := tx.GetArtifact(ctx, workItem.ID)
		if errors.Is(err, store.ErrNotFound) {
			missing = true
			return nil
		}
		if err != nil {
			return err
		}
		if source.Type != domain.ArtifactKindSpecification || source.Digest != run.SpecDigest {
			stale = true
		}
		return nil
	})
	if readErr != nil {
		return false, false, fmt.Errorf("inspect subject input: %w", readErr)
	}
	return missing, stale, nil
}

// reconcileDepartures advances each present occurrence for this initiator whose
// issue has left the labeled-open set: label removed (issue still open) moves it
// absent, issue closed moves it closed. It distinguishes the two by observing
// the issue directly. An admitted occurrence's still-open proposal is withdrawn
// (SupersedeIntakeProposal leaves a decided card untouched); an occurrence whose
// admission never completed carries no proposal, and is advanced by a bare
// observation so it never lingers present.
func (r *intakeReconciler) reconcileDepartures(
	ctx context.Context, init intakeInitiator, labeledOpen map[int]bool, logger *slog.Logger,
) error {
	var present []domain.IntakeOccurrence
	if err := r.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		present, err = tx.ListPresentIntakeOccurrences(ctx, init.RepositoryID, init.Label)
		return err
	}); err != nil {
		return fmt.Errorf("list present occurrences: %w", err)
	}
	for _, occurrence := range present {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if labeledOpen[occurrence.IssueNumber] {
			continue
		}
		// A start decided between the last present pass and this departure must
		// still launch: retiring the occurrence would otherwise strand the
		// recorded start, since only present labeled issues reach the launch
		// trigger. Launch first and do not retire until it lands.
		if launched, err := r.launchDecidedDeparture(ctx, init, occurrence); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			logger.Error("label-intake departed start launch failed",
				"repo", init.Repo, "label", init.Label, "issue", occurrence.IssueNumber, "error", err)
			continue
		} else if launched {
			logger.Info("label-intake launched a departed decided start before retiring",
				"repo", init.Repo, "label", init.Label, "issue", occurrence.IssueNumber)
		}
		if err := r.advanceDeparture(ctx, occurrence); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, errIntakeDeferDepartureRetire) {
				// A start was decided in the launch/retire window; the next pass
				// launches it before retiring. Expected, not a failure.
				logger.Info("label-intake deferring departure until the decided start launches",
					"repo", init.Repo, "label", init.Label, "issue", occurrence.IssueNumber)
				continue
			}
			logger.Error("label-intake departure failed",
				"repo", init.Repo, "label", init.Label, "issue", occurrence.IssueNumber, "error", err)
		}
	}
	return nil
}

// advanceDeparture classifies a departure by a direct issue observation and
// moves the occurrence out of present accordingly. An admitted occurrence
// supersedes its still-open proposal; an occurrence whose admission never
// completed (e.g. a durable mint refusal) has no proposal to withdraw, so a bare
// observation records the departure. Without this an unadmitted occurrence would
// linger present forever, and a later re-label would reuse its ordinal and
// upstream-event identity instead of allocating a fresh occurrence.
func (r *intakeReconciler) advanceDeparture(ctx context.Context, occurrence domain.IntakeOccurrence) error {
	issueObs, err := r.observeIssue(ctx, occurrence.Repo, occurrence.IssueNumber)
	if err != nil {
		return fmt.Errorf("observe departed issue: %w", err)
	}
	reason := domain.IntakeSupersededLabelRemoved
	state := domain.IntakeOccurrenceAbsent
	if strings.EqualFold(issueObs.State, "closed") {
		reason = domain.IntakeSupersededIssueClosed
		state = domain.IntakeOccurrenceClosed
	}
	return r.store.Write(ctx, func(tx *store.WriteTx) error {
		if occurrence.Admission == nil {
			// No admitted proposal to supersede: record the observed departure so the
			// occurrence leaves present and a re-label allocates a fresh occurrence.
			_, err := tx.RecordIntakeObservation(ctx,
				occurrence.RepositoryID, occurrence.IssueNumber, occurrence.Label, occurrence.Ordinal,
				state, r.now())
			return err
		}
		// Atomic re-check: a start decided (item resolved) but not yet launched
		// (no dispatch marker) must not be retired, or the recorded start is
		// stranded since only present passes launch. Reading the item status and
		// the marker in this same write serializes against a concurrent operator
		// start under the store write lock, closing the window between
		// launchDecidedDeparture's read and this retire.
		decided, err := tx.AuthenticateStartDecision(ctx,
			occurrence.Admission.ProposalInstanceID, occurrence.Admission.ProposalDigest)
		if err != nil {
			return err
		}
		if decided {
			if _, err := tx.GetOutbox(ctx,
				engine.ElaborationDispatchMarkerKey(occurrence.Admission.Subject.ElaborationRunID)); errors.Is(err, store.ErrNotFound) {
				return errIntakeDeferDepartureRetire
			} else if err != nil {
				return err
			}
		}
		_, err = tx.SupersedeIntakeProposal(ctx,
			occurrence.RepositoryID, occurrence.IssueNumber, occurrence.Label, occurrence.Ordinal,
			reason, state, r.now())
		return err
	})
}

// launchDecidedDeparture launches an admitted occurrence's proposal when it was
// decided start but not yet started, before the occurrence is retired. It
// reports whether it launched. A proposal still open (no decision) or already
// started needs nothing; only a decided-but-unstarted start would otherwise be
// stranded by the departure. SubmitElaborationRun converges, so a replay is a
// no-op.
func (r *intakeReconciler) launchDecidedDeparture(
	ctx context.Context, init intakeInitiator, occurrence domain.IntakeOccurrence,
) (bool, error) {
	if occurrence.Admission == nil {
		return false, nil
	}
	started, err := r.elaborationStarted(ctx, occurrence.Admission.Subject.ElaborationRunID)
	if err != nil || started {
		return false, err
	}
	decidedStart, err := r.proposalDecidedStart(ctx, occurrence.Admission.ProposalInstanceID, occurrence.Admission.ProposalDigest)
	if err != nil || !decidedStart {
		return false, err
	}
	if err := r.launch(ctx, init, occurrence); err != nil {
		return false, err
	}
	return true, nil
}

// intakeImplementationRunID derives the implementation run identity from the
// occurrence's own upstream-event coordinates, so a re-observed occurrence
// always resolves to the same run and the admission converges. The elaboration
// run id derives from this (ElaborationRunIDForImplementation).
func intakeImplementationRunID(occurrence domain.IntakeOccurrence) domain.RunID {
	sum := sha256.Sum256([]byte("freeside.label-intake.implementation/v1\x00" + occurrence.UpstreamEventID()))
	return domain.RunID("run-" + hex.EncodeToString(sum[:]))
}

// intakeProposalBatchID derives a stable proposal-batch identity from the
// occurrence, so a re-admission replay groups under the same batch.
func intakeProposalBatchID(occurrence domain.IntakeOccurrence) domain.ProposalBatchID {
	sum := sha256.Sum256([]byte("freeside.label-intake.batch/v1\x00" + occurrence.UpstreamEventID()))
	return domain.ProposalBatchID("batch-label-intake-" + hex.EncodeToString(sum[:]))
}

// intakeStartCommandID derives the reserved-device start command's stable id, so
// a re-run of an auto_start decision replays the same command rather than
// creating a second.
func intakeStartCommandID(occurrence domain.IntakeOccurrence) string {
	sum := sha256.Sum256([]byte("freeside.label-intake.start-command/v1\x00" + occurrence.UpstreamEventID()))
	return "cmd-label-intake-start-" + hex.EncodeToString(sum[:])
}

// intakeWorkItemDocument is the daemon-authored, coordinates-only work item
// delivered to the elaborator in the specification role. It is a pure function
// of the occurrence coordinates and carries NO observed issue content (GQ1,
// §5.13): the issue's title, body, and comments reach elaboration only as
// elaborator-fetched research, never as authority. Its digest is the reserved
// run's SpecDigest, so the bytes must be deterministic across replays.
func intakeWorkItemDocument(occurrence domain.IntakeOccurrence) []byte {
	return []byte(fmt.Sprintf(
		"# Freeside label-initiator work item\n\n"+
			"This elaboration run was initiated by the presence of an initiator label on a\n"+
			"repository issue. It carries only the issue coordinates below. The issue's own\n"+
			"title, body, and comments are not included here and enter elaboration only as\n"+
			"elaborator-fetched research, never as authority.\n\n"+
			"- repository: %s\n"+
			"- repository_id: %d\n"+
			"- issue: %d\n"+
			"- initiator_label: %s\n",
		occurrence.Repo, occurrence.RepositoryID, occurrence.IssueNumber, occurrence.Label))
}

// intakePublication composes the daemon-authored pull-request metadata for a
// label-initiated run from the occurrence coordinates and the initiator's
// configured commit-author identity. It carries no observed issue content.
func intakePublication(init intakeInitiator, occurrence domain.IntakeOccurrence) engine.ProductionPublication {
	return engine.ProductionPublication{
		Title: fmt.Sprintf("Resolve %s#%d", occurrence.Repo, occurrence.IssueNumber),
		Body: fmt.Sprintf(
			"Automated resolution of issue #%d in %s, initiated by the %q label. "+
				"The implementation is derived from the elaborated specification, not the issue text.",
			occurrence.IssueNumber, occurrence.Repo, occurrence.Label),
		CommitAuthor: init.CommitAuthor,
	}
}

// requireForgeResearchHost fails the admission closed when the initiator's
// resolved research allowlist does not already admit the forge API host. The
// loop must not silently widen operator-attributed policy: the issue fetch's
// egress belongs in the operator's rein under authentic provenance, so an
// allowlist that omits it is a configuration error the operator fixes, not a
// value the loop rewrites while keeping the original provenance (which would
// falsely attest an egress the source never authorized).
func requireForgeResearchHost(keys []domain.PolicyKey) error {
	for _, key := range keys {
		if key.Key != elaborate.PolicyResearchAllowlist {
			continue
		}
		for _, host := range strings.Split(key.Value, ",") {
			if strings.TrimSpace(host) == intakeForgeResearchHost {
				return nil
			}
		}
		return fmt.Errorf("initiator research allowlist omits the forge host %s: %w",
			intakeForgeResearchHost, errIntakeForgeHostNotAllowed)
	}
	return fmt.Errorf("initiator policy has no %s key: %w",
		elaborate.PolicyResearchAllowlist, errIntakeForgeHostNotAllowed)
}
