package engine

import (
	"context"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// The follow-up authoring a filing proposal reads (plan §5.17, issue #3148).
// After the duplicate search answers "new" for an entry, the finding
// adjudicator's follow_up_filing_author site drafts the work-unit form's
// fields and a work kind from a fixed enum. The draft lives only in memory
// until the proposal transaction renders it into the stored title and body;
// a pass that fails before that commit asks the site again.
//
// Untrusted model text reaches GitHub through the draft, and the draft's
// kind picks a label, so this file is Tier A. What it re-checks: the client
// re-runs the site's schema, which refuses a kind outside the enum; the
// renderer screens each field under github-issue/1 and confines it to its
// section; and the store's gate re-derives the label set from policy and the
// stored kind on admission and on every read.

// followUpFilingDrafts maps a finding to its draft. A finding with no entry
// takes the composed text and no kind.
type followUpFilingDrafts map[domain.FindingID]inference.FollowUpFilingDraft

// prepareFollowUpFilings runs, outside any transaction, everything a filing
// proposal waits on: the duplicate search, then a draft for each entry the
// search answered "new". The caller passes the drafts to
// proposeFollowUpFilings in its write.
func (w *productionPublicationWorkflow) prepareFollowUpFilings(
	ctx context.Context,
	projectID domain.ProjectID,
	artifact domain.FindingAdjudication,
	routes map[domain.FindingID]domain.AdjudicationRoute,
	kind domain.FollowUpSourceKind,
) (followUpFilingDrafts, error) {
	if err := w.searchFollowUpDuplicates(ctx, projectID, artifact, routes, kind); err != nil {
		return nil, err
	}
	return w.authorFollowUpFilings(ctx, projectID, artifact, routes, kind)
}

// unauthoredFollowUpFiling is one entry the pass will propose, with the
// site's input.
type unauthoredFollowUpFiling struct {
	findingID domain.FindingID
	input     inference.FollowUpFilingAuthorInput
}

// authorFollowUpFilings asks the site for a draft of each entry this pass
// will propose: no instance yet, and a duplicate search that answered "new".
// It writes nothing. With no client or no registered site it drafts nothing,
// and an entry whose call fails gets no draft while the others continue, so
// an unavailable site leaves every filing as it was composed before the site.
// A refused dispatch claim (the workflow closed) ends drafting the same way:
// the entries left take their composed text, as they did before the site.
// A cancelled context returns its error: the reconcile ended, the call did
// not fail.
func (w *productionPublicationWorkflow) authorFollowUpFilings(
	ctx context.Context,
	projectID domain.ProjectID,
	artifact domain.FindingAdjudication,
	routes map[domain.FindingID]domain.AdjudicationRoute,
	kind domain.FollowUpSourceKind,
) (followUpFilingDrafts, error) {
	entries := followUpFilingEntries(artifact, routes, kind)
	if len(entries) == 0 || w.inference == nil || !w.inference.SupportsSite(inference.FollowUpFilingAuthorSiteID) {
		return nil, nil
	}
	var work []unauthoredFollowUpFiling
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		work, err = readFollowUpFilingAuthorWork(ctx, tx, artifact, entries)
		return err
	}); err != nil {
		return nil, err
	}
	drafts := make(followUpFilingDrafts, len(work))
	for _, pending := range work {
		if err := w.claimProviderDispatch(ctx, artifact.RunID); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			break
		}
		draft, err := w.inference.AuthorFollowUpFiling(ctx, string(projectID), string(artifact.RunID), pending.input)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			continue
		}
		drafts[pending.findingID] = draft
	}
	return drafts, nil
}

// readFollowUpFilingAuthorWork reads the entries a pass will propose and the
// site's input for each, from stored rows only. A refusal planFollowUpFilings
// names is left to the proposal step, which raises its notice.
func readFollowUpFilingAuthorWork(
	ctx context.Context,
	tx *store.ReadTx,
	artifact domain.FindingAdjudication,
	entries []followUpFilingEntry,
) ([]unauthoredFollowUpFiling, error) {
	plan, err := planFollowUpFilings(ctx, tx, artifact, entries)
	if _, refused := followUpFilingRefusalClass(err); refused {
		return nil, nil
	}
	if err != nil || len(plan.pending) == 0 {
		return nil, err
	}
	searched, _, err := splitSearchedFollowUps(ctx, tx, artifact, plan.pending)
	if err != nil {
		return nil, err
	}
	work := make([]unauthoredFollowUpFiling, 0, len(searched))
	for _, candidate := range searched {
		finding, err := tx.GetFinding(ctx, candidate.entry.FindingID)
		if err != nil {
			return nil, err
		}
		text, err := duplicateSearchFindingText(finding, candidate.entry)
		if err != nil {
			return nil, err
		}
		work = append(work, unauthoredFollowUpFiling{
			findingID: candidate.entry.FindingID,
			input: inference.FollowUpFilingAuthorInput{
				RunID: artifact.RunID, Repository: plan.target.Repo,
				Finding: text, DeclaredPaths: plan.declaredPaths,
			},
		})
	}
	return work, nil
}
