package signet_test

import (
	"context"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func TestGetRunProjectsStoredDisplayNames(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	project, err := domain.NewProject("proj-1", "owner/freeside", 724)
	if err != nil {
		t.Fatal(err)
	}
	boundIssue := 724
	policy, err := domain.NewResolvedPolicy("run-1", []domain.PolicyKey{{
		Key: "paths", Value: "daemon/", Provenance: domain.KeyProvenance{
			Source: domain.ProvenanceOverride, Digest: "sha256:policy-source",
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	declaration, err := domain.NewWorkUnitDeclaration(domain.WorkUnitDeclarationInput{
		CompletionCriterion: domain.CompletionBoundIssueClosedByMergedPR,
		BoundIssue:          &boundIssue,
		DeclaredPaths:       domain.CanonicalDeclaredPaths(policy),
	}, "run-1", project.ID, *f.now)
	if err != nil {
		t.Fatal(err)
	}
	err = f.store.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.RegisterProject(ctx, project); err != nil {
			return err
		}
		if err := tx.PutRun(ctx, domain.Run{
			ID: "run-1", ProjectID: project.ID,
			SpecDigest: "sha256:spec", PolicyDigest: policy.Digest,
		}); err != nil {
			return err
		}
		if err := tx.PutResolvedPolicy(ctx, policy); err != nil {
			return err
		}
		return tx.RecordWorkUnitDeclaration(ctx, declaration)
	})
	if err != nil {
		t.Fatal(err)
	}

	snapshot, err := f.service.GetRun(ctx, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Run.DisplayNames == nil {
		t.Fatal("run display_names = nil")
	}
	if got := snapshot.Run.DisplayNames.Project; got != (domain.DisplayName{
		Text: "owner/freeside", Source: domain.DisplayNameSourceName,
	}) {
		t.Errorf("project display name = %#v", got)
	}
	if got := snapshot.Run.DisplayNames.WorkUnit; got != (domain.DisplayName{
		Text: "#724", Source: domain.DisplayNameSourceName,
	}) {
		t.Errorf("work-unit display name = %#v", got)
	}
}
