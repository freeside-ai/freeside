package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publicationtext"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// Issue comment proposals (plan §7, issue #3147). A follow-up entry whose
// duplicate search named an open issue gets an issue_comment proposal on that
// issue in place of a filing, and its effect_proposal card, and nothing more:
// no decision, no approval, and no forge write. A person decides the card,
// and the issue comment dispatcher posts only after an approve.
//
// The comment frames untrusted finding text, and model text from the search,
// into a post on the repository, so this file is Tier A. What it re-checks:
// every field is screened under github-issue/1 and takes fixed text on
// refusal; the target issue comes only from the search record, which the
// store re-reads at admission; and the repository's latest trust profile must
// accept its issue_comment workflows, since an App-authored comment starts
// them. The dispatcher checks that set again against the default branch
// before it posts.

const (
	// issueCommentLead opens every composed comment, so the finding after it
	// reads as Freeside's report and not as the issue author's words.
	issueCommentLead = "A review finding on a Freeside work unit matches this issue."
	// issueCommentReasonLimit bounds, in bytes, the search's reason a comment
	// quotes, so the finding still fits after it.
	issueCommentReasonLimit = 1000

	// issueCommentFallbackBody and externalIssueCommentFallbackBody replace
	// a composed body the github-issue/1 screen refuses. A comment cannot be
	// approved with changes, so the operator approves or declines the card as
	// written. Each names the finding by the ID the daemon derived for it, and
	// holds no agent, reviewer, or repository text; a finding ID the screen
	// would refuse takes the same text without it.
	issueCommentFallbackBody = "A review finding on a Freeside work unit matches this issue. " +
		"Its text did not pass the issue-text screen, so it is not copied here. " +
		"Read the finding%s and its adjudication in the run's review record."
	externalIssueCommentFallbackBody = "A finding a reviewer outside Freeside left on a Freeside work unit's " +
		"pull request matches this issue. Its text did not pass the issue-text screen, so it is not copied here. " +
		"Read the finding%s in its review thread on the pull request."

	// issueCommentRefusedCode is the run notice's diagnostic code.
	issueCommentRefusedCode = "issue_comment_refused"
)

// issueCommentRefusedItemID identifies the one notice a run raises when its
// duplicates cannot be proposed as comments.
func issueCommentRefusedItemID(runID domain.RunID) domain.ItemID {
	return domain.ItemID("issue-comment-refused-" + string(runID))
}

// issueCommentText composes a comment's body from the finding, its
// adjudication entry, and the search that matched it. No model writes the
// body beyond the search's reason, which it quotes as the reason. The body is
// screened, and a refused or blank body takes its fixed text, which names
// only the finding's ID.
func issueCommentText(
	finding domain.Finding, entry domain.FindingAdjudicationEntry, search domain.DuplicateSearch,
) domain.ScreenedIssueText {
	header := issueCommentLead
	if reason := strings.TrimSpace(boundValidText(search.Reason, issueCommentReasonLimit)); reason != "" {
		header += "\n\nWhy it matches: " + reason
	}
	var body string
	fallback := issueCommentFallbackBody
	if finding.External != nil {
		fallback = externalIssueCommentFallbackBody
		if _, draft := externalFollowUpFilingDraft(finding, entry); draft != "" {
			// Not cut: a cut could split the quote's code span. A body over
			// the limit takes the fallback.
			body = header + "\n\n" + draft
		}
	} else if _, draft := followUpFilingDraft(finding, entry); draft != "" {
		body = strings.TrimSpace(boundValidText(header+"\n\n"+draft, domain.MaxFollowUpFilingBodyBytes))
	}
	if !issueCommentBodyPasses(body) {
		body = fmt.Sprintf(fallback, " ("+string(finding.ID)+")")
	}
	if !issueCommentBodyPasses(body) {
		body = fmt.Sprintf(fallback, "")
	}
	return domain.ScreenedIssueText{
		Text: body, Ruleset: domain.IssueTextRulesetGitHubIssue1, Verdict: domain.ScreeningVerdictPassed,
	}
}

func issueCommentBodyPasses(body string) bool {
	return body != "" && publicationtext.ScreenIssueBody(
		domain.IssueTextRulesetGitHubIssue1, body, domain.MaxFollowUpFilingBodyBytes) == nil
}

// issueCommentsAccepted reports whether the target repository's latest trust
// profile accepts its issue_comment workflows: it must read, validate, name
// the target's repository ID, and carry an accepted set (a profile from
// before the set was recorded carries none). Only a cancelled context is an
// error; every other failure is a refusal, so no comment is proposed under a
// profile that cannot be read.
func issueCommentsAccepted(ctx context.Context, tx *store.ReadTx, target domain.FollowUpFilingTarget) (bool, error) {
	profile, err := tx.LatestTrustProfile(ctx, target.Repo)
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if err != nil {
		return false, nil
	}
	return profile.Validate() == nil && profile.RepositoryID == target.RepositoryID &&
		profile.AllowIssueCommentWorkflows, nil
}

// proposeIssueComments writes, in the caller's transaction, one comment
// proposal and its open card for each duplicate entry, under the filing
// batch and the entry's admission key: an entry gets a filing or a comment,
// never both. When the repository's trust profile does not accept its
// issue_comment workflows, it proposes nothing and raises the run's one
// notice instead.
func (w *productionPublicationWorkflow) proposeIssueComments(
	ctx context.Context,
	tx *store.WriteTx,
	projectID domain.ProjectID,
	artifact domain.FindingAdjudication,
	plan followUpFilingPlan,
	duplicates []duplicateFollowUpEntry,
) error {
	if len(duplicates) == 0 {
		return nil
	}
	accepted, err := issueCommentsAccepted(ctx, &tx.ReadTx, plan.target)
	if err != nil {
		return err
	}
	if !accepted {
		return w.recordRunProposalNotice(ctx, tx, projectID, artifact.RunID,
			issueCommentRefusedItemID(artifact.RunID), issueCommentRefusedCode, fmt.Sprintf(
				"Issue comments cannot be proposed for run %s: the trust profile for %s does not accept its issue_comment workflows, which a comment from Freeside would start. Its findings that match an open issue have no comment proposal.",
				artifact.RunID, plan.target.Repo))
	}
	batchID := followUpFilingBatchID(artifact.Digest)
	now := w.attentionCreatedAt()
	repository := domain.FollowUpFilingRepository{Repo: plan.target.Repo, RepositoryID: plan.target.RepositoryID}
	for _, duplicate := range duplicates {
		finding, err := tx.GetFinding(ctx, duplicate.entry.FindingID)
		if err != nil {
			return err
		}
		proposal, err := domain.NewEffectProposal(domain.EffectIssueComment, domain.IssueCommentInput{
			SubjectHandle: plan.handle, Repository: repository, Search: duplicate.search,
			Body: issueCommentText(finding, duplicate.entry, duplicate.search),
		}, plan.policy)
		if err != nil {
			return err
		}
		instance, inserted, err := tx.AllocateProposalInstance(ctx, domain.ProposalAdmissionKey{
			Source: domain.ProposalSourceRunEmission, ExportIdentity: artifact.Digest,
			EmissionOrdinal: duplicate.ordinal,
		}, batchID, proposal, now)
		if errors.Is(err, domain.ErrFollowUpFilingSourceStale) {
			// The source stopped backing a comment before this pass proposed it.
			continue
		}
		if err != nil {
			return err
		}
		if !inserted {
			continue
		}
		if _, err := signet.OpenIssueCommentItem(ctx, tx, w.artifacts, instance.ID, now); err != nil {
			return err
		}
	}
	return nil
}

// issueCommentsSettled reports whether the duplicates leave nothing to write:
// none is waiting, or the profile refuses them and the run's notice exists.
// An accepted profile with a duplicate waiting means a comment to propose.
func issueCommentsSettled(
	ctx context.Context, tx *store.ReadTx, artifact domain.FindingAdjudication,
	plan followUpFilingPlan, duplicates []duplicateFollowUpEntry,
) (bool, error) {
	if len(duplicates) == 0 {
		return true, nil
	}
	accepted, err := issueCommentsAccepted(ctx, tx, plan.target)
	if err != nil || accepted {
		return false, err
	}
	_, err = tx.GetAttentionItem(ctx, issueCommentRefusedItemID(artifact.RunID))
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}
