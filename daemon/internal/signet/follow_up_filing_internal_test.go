package signet

import (
	"errors"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// TestFollowUpFilingFactsShowTheProjectRepository pins which repository the
// card names. The gate compares by id alone, so the stored name is unchecked:
// the facts serve the project's current name, and a stored id that is not the
// project's fails the read.
func TestFollowUpFilingFactsShowTheProjectRepository(t *testing.T) {
	t.Parallel()
	stored := followUpFilingFactsFixture().FollowUpFiling
	proposal := domain.EffectProposal{
		Kind: domain.EffectFollowUpFiling,
		FilingProposal: &domain.FollowUpFilingParameters{
			Repository: stored.Repository, Labels: stored.Labels, Milestone: stored.Milestone,
			Title: stored.Title, Body: stored.Body, Source: stored.Source,
		},
	}
	renamed := domain.Project{ID: "proj-1", Repo: "owner/renamed", RepositoryID: stored.Repository.RepositoryID}
	facts, err := followUpFilingFacts(renamed, proposal, nil, nil)
	if err != nil {
		t.Fatalf("facts after a rename: %v", err)
	}
	want := domain.FollowUpFilingRepository{Repo: "owner/renamed", RepositoryID: stored.Repository.RepositoryID}
	if facts.Repository != want {
		t.Fatalf("repository = %+v, want the project's current %+v", facts.Repository, want)
	}

	other := domain.Project{ID: "proj-1", Repo: stored.Repository.Repo, RepositoryID: stored.Repository.RepositoryID + 1}
	if _, err := followUpFilingFacts(other, proposal, nil, nil); !errors.Is(err, ErrInvalidSyncSnapshot) {
		t.Fatalf("facts for another repository id = %v, want ErrInvalidSyncSnapshot", err)
	}
}
