package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publicationtext"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// Follow-up filing proposals (plan §5.17, §7). A finding adjudication that
// concludes "this is separate work" produces a follow_up_filing proposal and
// its effect_proposal card, and nothing more: no decision, no approval, and no
// forge write. A person decides the card.
//
// Two conclusions produce one. A deferred disposition does, in the
// transaction that writes its row. An accepted park_separate_work route does,
// once the operator has decided the round's finding_adjudication item.

const (
	// followUpFilingFallbackTitle and followUpFilingFallbackBody replace a
	// composed field the github-issue/1 screen refuses, so a finding that
	// quotes an @name or opens a line with a path still gets its proposal. A
	// filing cannot be approved with changes, so the operator approves or
	// declines the card as written. Neither constant holds agent or repository
	// text.
	followUpFilingFallbackTitle = "Follow up on a review finding set aside as separate work"
	followUpFilingFallbackBody  = "A review finding on this work unit was set aside as follow-up work. " +
		"Its text did not pass the issue-text screen, so it is not copied here. " +
		"Read the finding and its adjudication in the run's review record before acting on this issue."

	followUpFilingBatchVersion = "freeside.follow-up-filing.batch/v1"
)

// errFollowUpFilingUndeclared marks a run submitted without a work-unit
// declaration. The proposal gate resolves policy and project through the
// declaration, so such a run can carry no filing.
var errFollowUpFilingUndeclared = errors.New("run has no work-unit declaration")

// followUpFilingBatchID groups the filings one adjudication artifact yields.
func followUpFilingBatchID(adjudication domain.Digest) domain.ProposalBatchID {
	sum := sha256.Sum256([]byte(followUpFilingBatchVersion + "\x00" + string(adjudication)))
	return domain.ProposalBatchID("batch-follow-up-filing-" + hex.EncodeToString(sum[:]))
}

// followUpFilingRefusedItemID identifies the one notice a run raises when no
// filing can be proposed for it.
func followUpFilingRefusedItemID(runID domain.RunID) domain.ItemID {
	return domain.ItemID("follow-up-filing-refused-" + string(runID))
}

// followUpFilingText composes a filing's title and body from the finding and
// its adjudication entry. No model writes either field, and nothing here
// reaches the filing target. The title is the finding message's first
// non-blank line; the body is the message, the location when there is one,
// and the adjudication rationale. Each is cut to its size limit and then
// screened, and a refused or blank field takes its fixed text alone.
func followUpFilingText(
	finding domain.Finding, entry domain.FindingAdjudicationEntry,
) (domain.ScreenedIssueText, domain.ScreenedIssueText) {
	title := ""
	for line := range strings.Lines(finding.Message) {
		if fields := strings.Fields(line); len(fields) > 0 {
			title = strings.Join(fields, " ")
			break
		}
	}
	title = strings.TrimSpace(boundValidText(title, domain.MaxFollowUpFilingTitleBytes))
	if title == "" || publicationtext.ScreenIssueTitle(
		domain.IssueTextRulesetGitHubIssue1, title, domain.MaxFollowUpFilingTitleBytes) != nil {
		title = followUpFilingFallbackTitle
	}

	var parts []string
	if message := strings.TrimSpace(finding.Message); message != "" {
		parts = append(parts, message)
	}
	if finding.Location != nil {
		parts = append(parts, "Location: "+finding.Location.String())
	}
	if rationale := strings.TrimSpace(entry.Rationale); rationale != "" {
		parts = append(parts, "Adjudication rationale: "+rationale)
	}
	body := strings.TrimSpace(boundValidText(strings.Join(parts, "\n\n"), domain.MaxFollowUpFilingBodyBytes))
	if body == "" || publicationtext.ScreenIssueBody(
		domain.IssueTextRulesetGitHubIssue1, body, domain.MaxFollowUpFilingBodyBytes) != nil {
		body = followUpFilingFallbackBody
	}

	passed := func(text string) domain.ScreenedIssueText {
		return domain.ScreenedIssueText{
			Text: text, Ruleset: domain.IssueTextRulesetGitHubIssue1, Verdict: domain.ScreeningVerdictPassed,
		}
	}
	return passed(title), passed(body)
}

// followUpFilingRoute is the adjudication route that concludes each source
// kind (plan §7 routing table). The switch dispatches behaviour and so omits
// default; the trailing return guards an unregistered kind, which matches no
// entry.
func followUpFilingRoute(kind domain.FollowUpSourceKind) domain.AdjudicationRoute {
	switch kind {
	case domain.FollowUpSourceDeferredDisposition:
		return domain.RouteDefer
	case domain.FollowUpSourceSeparateWorkVerdict:
		return domain.RouteParkSeparateWork
	}
	return ""
}

// followUpFilingEntry is one adjudication entry that yields a filing, with
// its 1-based position in the artifact.
type followUpFilingEntry struct {
	ordinal int
	entry   domain.FindingAdjudicationEntry
}

// followUpFilingEntries selects the entries a source kind proposes for: the
// entry's own route and the finding's effective route must both be the kind's
// route. The operator can only move a finding to decline or dispute, so the
// two differ exactly when a recommended route was overridden, and an
// overridden finding yields no filing.
func followUpFilingEntries(
	artifact domain.FindingAdjudication,
	routes map[domain.FindingID]domain.AdjudicationRoute,
	kind domain.FollowUpSourceKind,
) []followUpFilingEntry {
	route := followUpFilingRoute(kind)
	var entries []followUpFilingEntry
	for index, entry := range artifact.Entries {
		if entry.Route == route && routes[entry.FindingID] == route {
			entries = append(entries, followUpFilingEntry{ordinal: index + 1, entry: entry})
		}
	}
	return entries
}

// followUpFilingPlan is what one pass still has to propose: the selected
// entries with no instance yet, and the subject and target they file under.
type followUpFilingPlan struct {
	pending []followUpFilingEntry
	handle  domain.OpaqueSubjectHandle
	policy  domain.ResolvedPolicy
	target  domain.FollowUpFilingTarget
}

// planFollowUpFilings reads what is left to propose. An entry whose instance
// already exists is dropped before anything is recomposed. Allocation alone
// would also find the row, but it compares content, and a renamed repository
// or a later daemon that composes the text differently would turn every
// parked run's replay into an immutable conflict.
//
// The admission key is the artifact digest and the entry's position: the
// artifact is immutable and digest-addressed, so the position is stable, and
// a revised adjudication takes new keys.
func planFollowUpFilings(
	ctx context.Context,
	tx *store.ReadTx,
	artifact domain.FindingAdjudication,
	entries []followUpFilingEntry,
) (followUpFilingPlan, error) {
	existing, err := tx.ListProposalBatch(ctx, followUpFilingBatchID(artifact.Digest))
	if err != nil {
		return followUpFilingPlan{}, err
	}
	proposed := make(map[int]bool, len(existing))
	for _, instance := range existing {
		proposed[instance.Admission.EmissionOrdinal] = true
	}
	var plan followUpFilingPlan
	for _, candidate := range entries {
		if !proposed[candidate.ordinal] {
			plan.pending = append(plan.pending, candidate)
		}
	}
	if len(plan.pending) == 0 {
		return plan, nil
	}
	plan.handle = domain.OpaqueSubjectHandle(domain.WorkUnitIDForRun(artifact.RunID))
	declaration, policy, err := tx.ResolveProposalSubject(ctx, plan.handle)
	if errors.Is(err, store.ErrNotFound) {
		return followUpFilingPlan{}, fmt.Errorf("follow-up filing for run %q: %w",
			artifact.RunID, errFollowUpFilingUndeclared)
	}
	if err != nil {
		return followUpFilingPlan{}, err
	}
	project, err := tx.GetProject(ctx, declaration.ProjectID)
	if errors.Is(err, store.ErrNotFound) {
		return followUpFilingPlan{}, fmt.Errorf("follow-up filing project %q: %w",
			declaration.ProjectID, store.ErrProjectAuthorityMissing)
	}
	if err != nil {
		return followUpFilingPlan{}, err
	}
	plan.policy = policy
	plan.target, err = domain.DeriveFollowUpFilingTarget(project, policy)
	return plan, err
}

// proposeFollowUpFilings writes, in the caller's transaction, one filing
// proposal and its open card for each entry the source kind selects. A
// deferred source needs its disposition row written first in the same
// transaction: admission checks that the finding is effectively deferred
// under this artifact.
//
// It never fails the caller for a refusal that would repeat on every retry.
// A propagated error stops the publication lane, so a run that can carry no
// filing raises one notice instead and its dispositions still commit.
func (w *productionPublicationWorkflow) proposeFollowUpFilings(
	ctx context.Context,
	tx *store.WriteTx,
	projectID domain.ProjectID,
	artifact domain.FindingAdjudication,
	routes map[domain.FindingID]domain.AdjudicationRoute,
	kind domain.FollowUpSourceKind,
) error {
	entries := followUpFilingEntries(artifact, routes, kind)
	if len(entries) == 0 {
		return nil
	}
	plan, err := planFollowUpFilings(ctx, &tx.ReadTx, artifact, entries)
	if class, refused := followUpFilingRefusalClass(err); refused {
		return w.recordFollowUpFilingRefused(ctx, tx, projectID, artifact.RunID, class)
	}
	if err != nil {
		return err
	}
	return w.allocateFollowUpFilings(ctx, tx, artifact, kind, plan)
}

// proposeSeparateWorkFilings proposes for the accepted park_separate_work
// routes in a transaction of its own. Such a route writes no disposition, and
// the paths after acceptance (a convergence stop, a dispute route, a drift
// audit) can return before the disposition write on every pass, so the
// proposal cannot wait for it.
//
// The parked run re-enters on each reconcile, and every committed write
// advances the revision clients sync against, so a pass with nothing left to
// write stays a read.
func (w *productionPublicationWorkflow) proposeSeparateWorkFilings(
	ctx context.Context,
	task productionPublicationTask,
	artifact domain.FindingAdjudication,
	routes map[domain.FindingID]domain.AdjudicationRoute,
) error {
	kind := domain.FollowUpSourceSeparateWorkVerdict
	entries := followUpFilingEntries(artifact, routes, kind)
	if len(entries) == 0 {
		return nil
	}
	settled := false
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		plan, err := planFollowUpFilings(ctx, tx, artifact, entries)
		if _, refused := followUpFilingRefusalClass(err); refused {
			// A refused run has nothing left to write once its notice exists.
			_, err = tx.GetAttentionItem(ctx, followUpFilingRefusedItemID(artifact.RunID))
			settled = err == nil
			if errors.Is(err, store.ErrNotFound) {
				return nil
			}
			return err
		}
		settled = len(plan.pending) == 0
		return err
	}); err != nil || settled {
		return err
	}
	return w.store.Write(ctx, func(tx *store.WriteTx) error {
		return w.proposeFollowUpFilings(ctx, tx, task.ProjectID, artifact, routes, kind)
	})
}

// allocateFollowUpFilings allocates the planned instances and opens a card
// for each one this pass inserted, so a decided card is never reopened.
func (w *productionPublicationWorkflow) allocateFollowUpFilings(
	ctx context.Context,
	tx *store.WriteTx,
	artifact domain.FindingAdjudication,
	kind domain.FollowUpSourceKind,
	plan followUpFilingPlan,
) error {
	batchID := followUpFilingBatchID(artifact.Digest)
	now := w.attentionCreatedAt()
	for _, candidate := range plan.pending {
		finding, err := tx.GetFinding(ctx, candidate.entry.FindingID)
		if err != nil {
			return err
		}
		title, body := followUpFilingText(finding, candidate.entry)
		proposal, err := domain.NewEffectProposal(domain.EffectFollowUpFiling, domain.FollowUpFilingInput{
			SubjectHandle: plan.handle, Target: plan.target,
			Source: domain.FollowUpFilingSource{
				FindingID: candidate.entry.FindingID, AdjudicationDigest: artifact.Digest, Kind: kind,
			},
			Title: title, Body: body,
		}, plan.policy)
		if err != nil {
			return err
		}
		instance, inserted, err := tx.AllocateProposalInstance(ctx, domain.ProposalAdmissionKey{
			Source: domain.ProposalSourceRunEmission, ExportIdentity: artifact.Digest,
			EmissionOrdinal: candidate.ordinal,
		}, batchID, proposal, now)
		if errors.Is(err, domain.ErrFollowUpFilingSourceStale) {
			// The source stopped backing a filing before this pass proposed it.
			continue
		}
		if err != nil {
			return err
		}
		if !inserted {
			continue
		}
		if _, err := signet.OpenFollowUpFilingItem(ctx, tx, instance.ID, now); err != nil {
			return err
		}
	}
	return nil
}

// followUpFilingRefusalClass names a refusal no retry can clear: the run has
// no work-unit declaration, its project record is gone, or its resolved
// follow_up_filing policy is malformed. Every other error is a store fault or
// a broken invariant and propagates.
func followUpFilingRefusalClass(err error) (string, bool) {
	switch {
	case errors.Is(err, errFollowUpFilingUndeclared):
		return "work_unit_undeclared", true
	case errors.Is(err, store.ErrProjectAuthorityMissing):
		return "project_missing", true
	case errors.Is(err, domain.ErrFollowUpFilingPolicyInvalid):
		return "policy_invalid", true
	}
	return "", false
}

// recordFollowUpFilingRefused raises the run's one notice that its follow-up
// filings cannot be proposed. It is a system_health advisory offering
// acknowledge, the type the tree uses for a run-scoped notice nothing else
// can close (#1342). A later refusal for the same run keeps the first notice,
// whatever its class.
func (w *productionPublicationWorkflow) recordFollowUpFilingRefused(
	ctx context.Context,
	tx *store.WriteTx,
	projectID domain.ProjectID,
	runID domain.RunID,
	class string,
) error {
	itemID := followUpFilingRefusedItemID(runID)
	existing, err := tx.GetAttentionItem(ctx, itemID)
	if err == nil {
		validSubject := existing.Subject.Type == domain.SubjectRun &&
			existing.Subject.ID == domain.SubjectID(runID) &&
			existing.Subject.RunID != nil && *existing.Subject.RunID == runID
		if existing.Type != domain.AttentionSystemHealth || existing.ProjectID != projectID || !validSubject {
			return fmt.Errorf("follow-up filing notice %q disagrees with run %q: %w",
				itemID, runID, domain.ErrParentKeyMismatch)
		}
		return nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	subject := domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID}
	names, err := tx.DisplayNamesFor(ctx, projectID, subject)
	if err != nil {
		return err
	}
	createdAt := w.attentionCreatedAt()
	advisory := domain.HealthPostureAdvisory
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: itemID, ProjectID: projectID,
		Subject: subject, Type: domain.AttentionSystemHealth, Priority: domain.PriorityNormal,
		Reason: fmt.Sprintf(
			"Follow-up issues cannot be proposed for run %s (%s). Its deferred and separate-work findings have no filing proposal.",
			runID, class),
		RequestedDecision: []domain.Action{domain.ActionAcknowledge},
		// Advisory: review and publication continue; only the proposals are missing.
		HealthDiagnostic: &domain.HealthDiagnostic{
			Code: "follow_up_filing_refused", Impairs: domain.ImpairedCapabilityNone,
		},
		ItemVersion: 1, InterruptionClass: domain.InterruptionExceptional,
		CreatedAt: &createdAt, DisplayNames: names, Status: domain.StatusOpen, Posture: &advisory,
	}, w.approvedRecipes)
	if err != nil {
		return err
	}
	return tx.PutAttentionItem(ctx, item)
}
