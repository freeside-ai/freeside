package engine

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// The duplicate search a follow-up filing waits on (plan §7 Finding
// Adjudication, issue #3146). Before an entry's filing is proposed, the
// filing target's open issues are listed and the finding adjudicator's
// duplicate_search site judges whether one already tracks the work. Its
// answer is recorded as a domain.DuplicateSearch, and the filing gate admits
// a filing only under a record that answered "new".
//
// The record decides whether an issue is filed at all and, through its
// handle rule, which open issue a duplicate verdict names, so this file is
// Tier A. What it re-checks: the store binds each record to the adjudication
// entry it searched for, and the domain refuses a verdict naming a handle the
// record did not list; the site refuses a handle it was not shown.

// followUpFilingDuplicateSearchUnavailable is the refusal-notice class for a
// search that could not run: no lister or site, a failed listing, or a failed
// or unusable answer. Such an entry gets no record and so no filing.
const followUpFilingDuplicateSearchUnavailable = "duplicate_search_unavailable"

// duplicateSearchFinding is the finding as the site sees it: the words the
// adjudicator judged and its rationale for setting the finding aside. An
// external finding is quoted as the adjudicator read it, never as a finding.
type duplicateSearchFinding struct {
	Message   string                  `json:"message,omitempty"`
	Location  *domain.FindingLocation `json:"location,omitempty"`
	External  *quotedExternalFinding  `json:"external_finding,omitempty"`
	Rationale string                  `json:"rationale"`
}

// unsearchedFollowUpFiling is one entry with no record yet, with the finding
// text the site reads.
type unsearchedFollowUpFiling struct {
	entry   followUpFilingEntry
	finding string
}

// duplicateSearchWork is what one search pass has to do.
type duplicateSearchWork struct {
	target     domain.FollowUpFilingTarget
	boundIssue int
	unsearched []unsearchedFollowUpFiling
}

// searchFollowUpDuplicates records a duplicate search for each entry of kind
// that has neither a filing instance nor a record. It runs outside any
// transaction, before the write that proposes the filings, because it calls
// GitHub and a model; each record commits in a write of its own.
//
// One listing serves every entry of the pass, and the run's bound issue is
// left out of it: the run is closing that issue, so it cannot track follow-up
// work. An empty listing records "new" with no site call.
//
// A search that cannot run raises the run's follow-up filing notice and
// returns nil, so the caller's dispositions still commit and the entry gets no
// filing. Once the notice exists no later pass searches again, so a parked run
// re-entering on each reconcile makes no further listing or provider call. A
// cancelled context returns its error instead: the reconcile ended, the search
// did not fail.
func (w *productionPublicationWorkflow) searchFollowUpDuplicates(
	ctx context.Context,
	projectID domain.ProjectID,
	artifact domain.FindingAdjudication,
	routes map[domain.FindingID]domain.AdjudicationRoute,
	kind domain.FollowUpSourceKind,
) error {
	entries := followUpFilingEntries(artifact, routes, kind)
	if len(entries) == 0 {
		return nil
	}
	var work duplicateSearchWork
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		work, err = readDuplicateSearchWork(ctx, tx, artifact, entries)
		return err
	}); err != nil || len(work.unsearched) == 0 {
		return err
	}
	unavailable := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return w.store.Write(ctx, func(tx *store.WriteTx) error {
			return w.recordFollowUpFilingRefused(ctx, tx, projectID, artifact.RunID,
				followUpFilingDuplicateSearchUnavailable)
		})
	}
	if w.openIssues == nil {
		return unavailable()
	}
	listing, err := w.openIssues.ListOpenIssues(ctx, work.target.Repo, work.target.RepositoryID,
		work.boundIssue, domain.MaxDuplicateSearchCandidates)
	if err != nil {
		return unavailable()
	}
	recorded := make([]domain.DuplicateSearchCandidate, 0, len(listing.Issues))
	shown := make([]inference.DuplicateSearchCandidate, 0, len(listing.Issues))
	for index, issue := range listing.Issues {
		handle := domain.DuplicateSearchHandle(index + 1)
		recorded = append(recorded, domain.DuplicateSearchCandidate{Handle: handle, Number: issue.Number, Title: issue.Title})
		shown = append(shown, inference.DuplicateSearchCandidate{Handle: handle, Title: issue.Title, BodyExcerpt: issue.BodyExcerpt})
	}
	if len(shown) > 0 && (w.inference == nil || !w.inference.SupportsSite(inference.DuplicateSearchSiteID)) {
		return unavailable()
	}
	for _, pending := range work.unsearched {
		input := domain.DuplicateSearchInput{
			RunID: artifact.RunID, AdjudicationDigest: artifact.Digest,
			FindingID: pending.entry.entry.FindingID, Kind: kind,
			Repository: domain.FollowUpFilingRepository{
				Repo: work.target.Repo, RepositoryID: work.target.RepositoryID,
			},
			CandidateLimit: domain.MaxDuplicateSearchCandidates, Truncated: listing.Truncated,
			Candidates: recorded,
		}
		if len(shown) > 0 {
			if err := w.claimProviderDispatch(ctx, artifact.RunID); err != nil {
				return err
			}
			answer, err := w.inference.SearchDuplicates(ctx, string(projectID), string(artifact.RunID),
				inference.DuplicateSearchInput{RunID: artifact.RunID, Finding: pending.finding, Candidates: shown})
			if err != nil {
				return unavailable()
			}
			input.DuplicateOf, input.Confidence, input.Reason = answer.DuplicateOf, answer.Confidence, answer.Reason
		}
		input.CreatedAt = w.attentionCreatedAt()
		search, err := domain.NewDuplicateSearch(input)
		if err != nil {
			return err
		}
		// The candidates come from the listing and the answer names only a
		// shown handle, so a store refusal is a contradiction between stored
		// rows, not a failed search, and it propagates.
		if err := w.store.Write(ctx, func(tx *store.WriteTx) error {
			return tx.PutDuplicateSearch(ctx, search)
		}); err != nil {
			return err
		}
	}
	return nil
}

// readDuplicateSearchWork reads the entries a pass still has to search. A
// refusal planFollowUpFilings names is left to the proposal step, which
// raises its notice; a run whose notice already exists searches nothing.
func readDuplicateSearchWork(
	ctx context.Context,
	tx *store.ReadTx,
	artifact domain.FindingAdjudication,
	entries []followUpFilingEntry,
) (duplicateSearchWork, error) {
	plan, err := planFollowUpFilings(ctx, tx, artifact, entries)
	if _, refused := followUpFilingRefusalClass(err); refused {
		return duplicateSearchWork{}, nil
	}
	if err != nil || len(plan.pending) == 0 {
		return duplicateSearchWork{}, err
	}
	_, err = tx.GetAttentionItem(ctx, followUpFilingRefusedItemID(artifact.RunID))
	if err == nil {
		return duplicateSearchWork{}, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return duplicateSearchWork{}, err
	}
	work := duplicateSearchWork{target: plan.target, boundIssue: plan.boundIssue}
	for _, candidate := range plan.pending {
		_, err := tx.GetDuplicateSearch(ctx, artifact.Digest, candidate.entry.FindingID)
		if err == nil {
			continue
		}
		if !errors.Is(err, store.ErrNotFound) {
			return duplicateSearchWork{}, err
		}
		finding, err := tx.GetFinding(ctx, candidate.entry.FindingID)
		if err != nil {
			return duplicateSearchWork{}, err
		}
		text, err := duplicateSearchFindingText(finding, candidate.entry)
		if err != nil {
			return duplicateSearchWork{}, err
		}
		work.unsearched = append(work.unsearched, unsearchedFollowUpFiling{entry: candidate, finding: text})
	}
	return work, nil
}

// duplicateSearchFindingText composes the site's finding field from stored
// rows only.
func duplicateSearchFindingText(finding domain.Finding, entry domain.FindingAdjudicationEntry) (string, error) {
	view := duplicateSearchFinding{Rationale: entry.Rationale}
	if finding.External != nil {
		quoted, err := quoteExternalFinding(finding)
		if err != nil {
			return "", err
		}
		view.External = &quoted
	} else {
		view.Message, view.Location = finding.Message, finding.Location
	}
	body, err := json.Marshal(view)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// searchedNewFollowUpFilings keeps the pending entries whose duplicate search
// answered "new". An entry with no record waits for a search, and one whose
// search named an open issue gets no filing.
func searchedNewFollowUpFilings(
	ctx context.Context,
	tx *store.ReadTx,
	artifact domain.FindingAdjudication,
	pending []followUpFilingEntry,
) ([]followUpFilingEntry, error) {
	var kept []followUpFilingEntry
	for _, candidate := range pending {
		search, err := tx.GetDuplicateSearch(ctx, artifact.Digest, candidate.entry.FindingID)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if search.IsNew() {
			kept = append(kept, candidate)
		}
	}
	return kept, nil
}
