package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func initialProductionAttempt() domain.ProductionAttempt {
	implementationRunID := domain.RunID("run-1")
	campaignID, err := engine.ProductionCampaignIDForImplementation(implementationRunID)
	if err != nil {
		panic(err)
	}
	specificationRunID, err := engine.SpecificationRunIDForImplementation(implementationRunID)
	if err != nil {
		panic(err)
	}
	return domain.ProductionAttempt{
		CampaignID: campaignID, AttemptNumber: 1, Kind: domain.ProductionAttemptInitial,
		SourceDigest: "sha256:source", PublicationDigest: "sha256:publication",
		SpecificationRunID:  specificationRunID,
		ImplementationRunID: implementationRunID,
	}
}

func TestProductionAttemptAllocationApprovalAndRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store.db")
	st, err := store.Open(ctx, path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	initial := initialProductionAttempt()
	missingPublication := initial
	missingPublication.PublicationDigest = ""
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutProductionAttempt(ctx, missingPublication)
	}); !errors.Is(err, domain.ErrEmptyField) {
		t.Fatalf("initial attempt without publication digest = %v, want ErrEmptyField", err)
	}
	preapproved := initial
	preapproved.ApprovedSpecDigest = "sha256:forged"
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutProductionAttempt(ctx, preapproved)
	}); !errors.Is(err, domain.ErrImmutableTransition) {
		t.Fatalf("pre-approved initial attempt = %v, want ErrImmutableTransition", err)
	}
	var approved domain.ProductionAttempt
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutProductionAttempt(ctx, initial); err != nil {
			return err
		}
		var err error
		approved, err = tx.ApproveProductionAttempt(ctx, initial.CampaignID, 1, "sha256:approved")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = store.Open(ctx, path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	var got domain.ProductionAttempt
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		got, err = tx.GetProductionAttemptByRun(ctx, initial.ImplementationRunID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, approved) {
		t.Fatalf("reopened attempt = %+v, want %+v", got, approved)
	}

	retry := domain.ProductionAttempt{
		CampaignID: initial.CampaignID, AttemptNumber: 2, Kind: domain.ProductionAttemptRetry,
		Reason: "retry after repair", ParentRunID: initial.ImplementationRunID,
		SourceDigest: initial.SourceDigest, PublicationDigest: initial.PublicationDigest,
		ApprovedSpecDigest: approved.ApprovedSpecDigest,
		SpecificationRunID: initial.SpecificationRunID,
		ImplementationRunID: func() domain.RunID {
			id, err := engine.ProductionAttemptRunID(initial.CampaignID, 2)
			if err != nil {
				panic(err)
			}
			return id
		}(),
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutProductionAttempt(ctx, retry); err != nil {
			return err
		}
		return tx.PutProductionAttempt(ctx, retry)
	}); err != nil {
		t.Fatal(err)
	}
	changedApproved := retry
	changedApproved.AttemptNumber = 3
	changedApproved.ParentRunID = retry.ImplementationRunID
	changedApproved.ImplementationRunID = "run-3"
	changedApproved.ApprovedSpecDigest = "sha256:different-approved-spec"
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutProductionAttempt(ctx, changedApproved)
	}); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("changed approved-spec lineage = %v, want ErrParentKeyMismatch", err)
	}
	changedPublication := retry
	changedPublication.AttemptNumber = 3
	changedPublication.ParentRunID = retry.ImplementationRunID
	changedPublication.ImplementationRunID = "run-3"
	changedPublication.PublicationDigest = "sha256:different-publication"
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutProductionAttempt(ctx, changedPublication)
	}); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("changed publication lineage = %v, want ErrParentKeyMismatch", err)
	}
	foreignRoot := retry
	foreignRoot.AttemptNumber = 3
	foreignRoot.ParentRunID = retry.ImplementationRunID
	foreignRoot.ImplementationRunID = "run-3"
	foreignRoot.SpecificationRunID = "run-specification-foreign"
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutProductionAttempt(ctx, foreignRoot)
	}); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("foreign specification root = %v, want ErrParentKeyMismatch", err)
	}
	skipped := retry
	skipped.AttemptNumber = 4
	skipped.ImplementationRunID = "run-4"
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutProductionAttempt(ctx, skipped)
	}); !errors.Is(err, domain.ErrNonContiguous) {
		t.Fatalf("skipped allocation = %v, want ErrNonContiguous", err)
	}
}
