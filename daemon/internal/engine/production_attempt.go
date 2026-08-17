package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// ProductionReattemptSpec identifies one deliberate retry request. Exactly one
// of ParentRunID and CampaignID is required; the store resolves a campaign to
// its latest complete attempt and allocates the next ordinal.
type ProductionReattemptSpec struct {
	ParentRunID domain.RunID
	CampaignID  domain.CampaignID
	Reason      string
}

// ProductionReattempt reports the new run and the lineage/input identities
// the CLI exposes alongside the ordinary submit lanes.
type ProductionReattempt struct {
	Run                     ProductionRun
	Attempt                 domain.ProductionAttempt
	ElaborationInvocationID domain.InvocationID
	ElaborationStageID      domain.StageID
	SourceArtifactID        domain.ArtifactID
	RootSourceArtifactID    domain.ArtifactID
	PolicyArtifactID        domain.ArtifactID
	PolicyDigest            domain.Digest
	Publication             ProductionPublication
	HasWorkUnit             bool
}

// ReattemptProductionRun creates one new implementation attempt from the
// approved specification of a terminal campaign run. The store allocates the
// ordinal and persists its lineage before the ordinary production-intake gate
// creates the derived run; an interrupted command reuses the same incomplete
// allocation when invoked again with the same parent and reason.
func ReattemptProductionRun(
	ctx context.Context, st *store.Store, spec ProductionReattemptSpec,
) (ProductionReattempt, error) {
	if st == nil {
		return ProductionReattempt{}, errors.New("reattempt production run: nil store")
	}
	reason := strings.TrimSpace(spec.Reason)
	if reason == "" || reason != spec.Reason {
		return ProductionReattempt{}, errors.New("reattempt production run: a non-empty trimmed reason is required")
	}
	if (spec.ParentRunID == "") == (spec.CampaignID == "") {
		return ProductionReattempt{}, errors.New("reattempt production run: exactly one parent run or campaign is required")
	}

	var (
		attempt        domain.ProductionAttempt
		parentRun      domain.Run
		parentPolicy   domain.ResolvedPolicy
		request        elaborationRequest
		sourceArtifact domain.Artifact
		policyArtifact domain.Artifact
	)
	err := st.Write(ctx, func(tx *store.WriteTx) error {
		var latest domain.ProductionAttempt
		var err error
		if spec.CampaignID != "" {
			latest, err = tx.LatestProductionAttempt(ctx, spec.CampaignID)
		} else {
			latest, err = tx.GetProductionAttemptByRun(ctx, spec.ParentRunID)
			if err == nil {
				latest, err = tx.LatestProductionAttempt(ctx, latest.CampaignID)
			}
		}
		if err != nil {
			return err
		}

		// Recover the only safe partial state: allocation committed, derived run
		// absent, and the retried operator intent is byte-identical.
		if latest.Kind == domain.ProductionAttemptRetry {
			_, runErr := tx.GetRun(ctx, latest.ImplementationRunID)
			if errors.Is(runErr, store.ErrNotFound) {
				parentMatches := spec.ParentRunID == "" || latest.ParentRunID == spec.ParentRunID
				if latest.Reason != reason || !parentMatches {
					return fmt.Errorf("campaign %q has incomplete attempt %d: %w",
						latest.CampaignID, latest.AttemptNumber, domain.ErrImmutableTransition)
				}
				attempt = latest
				return loadReattemptInputs(ctx, tx, attempt.ParentRunID, &parentRun,
					&parentPolicy, &request, &sourceArtifact, &policyArtifact)
			}
			if runErr != nil {
				return runErr
			}
		}

		parentAttempt := latest
		if spec.ParentRunID != "" {
			parentAttempt, err = tx.GetProductionAttemptByRun(ctx, spec.ParentRunID)
			if err != nil {
				return err
			}
		}
		if err := loadReattemptInputs(ctx, tx, parentAttempt.ImplementationRunID, &parentRun,
			&parentPolicy, &request, &sourceArtifact, &policyArtifact); err != nil {
			return err
		}
		observation, err := tx.ObserveRun(ctx, parentRun.ID)
		if err != nil {
			return err
		}
		conclusion := domain.ConcludeRun(observation)
		if !conclusion.Final {
			return fmt.Errorf("parent run %q is %s; use resume while it is live",
				parentRun.ID, conclusion.Outcome)
		}
		number := latest.AttemptNumber + 1
		runID, err := ProductionAttemptRunID(latest.CampaignID, number)
		if err != nil {
			return err
		}
		attempt = domain.ProductionAttempt{
			CampaignID: latest.CampaignID, AttemptNumber: number,
			Kind: domain.ProductionAttemptRetry, Reason: reason,
			ParentRunID: parentRun.ID, SourceDigest: parentAttempt.SourceDigest,
			PublicationDigest:   parentAttempt.PublicationDigest,
			ApprovedSpecDigest:  parentRun.SpecDigest,
			ElaborationRunID:    parentAttempt.ElaborationRunID,
			ImplementationRunID: runID,
		}
		return tx.PutProductionAttempt(ctx, attempt)
	})
	if err != nil {
		return ProductionReattempt{}, fmt.Errorf("reattempt production run: %w", err)
	}

	resolved, err := domain.NewResolvedPolicy(attempt.ImplementationRunID, parentPolicy.Keys)
	if err != nil {
		return ProductionReattempt{}, fmt.Errorf("reattempt production run: %w", err)
	}
	run, err := SubmitProductionRun(ctx, st, ProductionRunSpec{
		RunID: attempt.ImplementationRunID, ProjectID: parentRun.ProjectID,
		SpecArtifactID: sourceArtifact.ID, PolicyArtifactID: policyArtifact.ID,
		ResolvedPolicy: resolved, Publication: request.Publication,
		WorkUnit:   cloneElaborationWorkUnit(request.WorkUnit),
		CampaignID: attempt.CampaignID, AttemptNumber: attempt.AttemptNumber,
		AttemptReason: attempt.Reason, ParentRunID: attempt.ParentRunID,
	})
	if err != nil {
		return ProductionReattempt{}, fmt.Errorf("reattempt production run: %w", err)
	}
	return ProductionReattempt{
		Run: run, Attempt: attempt, SourceArtifactID: sourceArtifact.ID,
		RootSourceArtifactID:    request.InputArtifactIDs[0],
		ElaborationInvocationID: elaborationInvocationID(attempt.ElaborationRunID, 1),
		ElaborationStageID:      elaborationStageID(attempt.ElaborationRunID),
		PolicyArtifactID:        policyArtifact.ID, PolicyDigest: policyArtifact.Digest,
		Publication: request.Publication, HasWorkUnit: request.WorkUnit != nil,
	}, nil
}

func loadReattemptInputs(
	ctx context.Context,
	tx *store.WriteTx,
	parentRunID domain.RunID,
	parentRun *domain.Run,
	parentPolicy *domain.ResolvedPolicy,
	request *elaborationRequest,
	sourceArtifact, policyArtifact *domain.Artifact,
) error {
	var err error
	*parentRun, err = tx.GetRun(ctx, parentRunID)
	if err != nil {
		return err
	}
	parentAttempt, err := tx.GetProductionAttemptByRun(ctx, parentRunID)
	if err != nil {
		return err
	}
	if parentRun.CampaignID != parentAttempt.CampaignID ||
		parentRun.AttemptNumber != parentAttempt.AttemptNumber ||
		parentAttempt.ApprovedSpecDigest == "" ||
		parentRun.SpecDigest != parentAttempt.ApprovedSpecDigest {
		return fmt.Errorf("parent run %q attempt binding disagrees: %w",
			parentRunID, domain.ErrParentKeyMismatch)
	}
	*parentPolicy, err = tx.GetResolvedPolicy(ctx, parentRunID)
	if err != nil {
		return err
	}
	marker, err := tx.GetOutbox(ctx, string(productionInvocationID(parentRunID)))
	if err != nil {
		return err
	}
	publication, present, err := ProductionInvocationPublication(marker)
	if err != nil || !present {
		return fmt.Errorf("parent run %q publication metadata unavailable: %w",
			parentRunID, errors.Join(err, domain.ErrParentKeyMismatch))
	}
	elaborationMarker, err := tx.GetOutbox(ctx,
		string(elaborationInvocationID(parentAttempt.ElaborationRunID, 1)))
	if err != nil {
		return err
	}
	*request, err = decodeElaborationRequest(elaborationMarker)
	if err != nil {
		return err
	}
	if err := authenticateElaborationRoot(ctx, &tx.ReadTx, *request); err != nil {
		return err
	}
	initialAttempt, err := tx.GetProductionAttempt(ctx, parentAttempt.CampaignID, 1)
	if err != nil {
		return err
	}
	if request.CampaignID != parentAttempt.CampaignID || request.AttemptNumber != 1 ||
		request.ElaborationRunID != initialAttempt.ElaborationRunID ||
		request.ImplementationRunID != initialAttempt.ImplementationRunID ||
		request.PublicationDigest != initialAttempt.PublicationDigest {
		return fmt.Errorf("parent run %q campaign root disagrees: %w",
			parentRunID, domain.ErrParentKeyMismatch)
	}
	rootSource, err := tx.GetArtifact(ctx, request.InputArtifactIDs[0])
	if err != nil {
		return err
	}
	if rootSource.Digest != parentAttempt.SourceDigest {
		return fmt.Errorf("parent run %q source lineage disagrees: %w",
			parentRunID, domain.ErrParentKeyMismatch)
	}
	if request.Publication != publication || request.ProjectID != parentRun.ProjectID {
		return fmt.Errorf("parent run %q elaboration binding disagrees: %w",
			parentRunID, domain.ErrParentKeyMismatch)
	}
	invocation, err := tx.GetAgentInvocation(ctx, productionInvocationID(parentRunID))
	if err != nil {
		return err
	}
	if len(invocation.InputIDs) != 1 {
		return fmt.Errorf("parent run %q has %d implementation inputs: %w",
			parentRunID, len(invocation.InputIDs), domain.ErrParentKeyMismatch)
	}
	*sourceArtifact, err = tx.GetArtifact(ctx, invocation.InputIDs[0])
	if err != nil {
		return err
	}
	*policyArtifact, err = tx.GetArtifact(ctx, request.PolicyArtifactID)
	if err != nil {
		return err
	}
	if sourceArtifact.Digest != parentRun.SpecDigest || policyArtifact.Digest != parentPolicy.Digest {
		return fmt.Errorf("parent run %q artifacts disagree: %w", parentRunID, domain.ErrParentKeyMismatch)
	}
	return nil
}
