package engine

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publicationtext"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// TestFollowUpFilingPlansExternalFindings: a deferred external finding
// proposes a follow-up issue as a deferred finding of Freeside's own does
// (issue #1787 lifts #1767 decision 5), so one adjudication that defers both
// plans both.
func TestFollowUpFilingPlansExternalFindings(t *testing.T) {
	t.Parallel()
	f := newFollowUpFilingFixture(t, domain.FindingSeverityP2, domain.RouteDefer, nil)
	seedFollowUpFilingSubject(t, f, true)
	external, err := domain.NewExternalFinding(domain.ExternalFindingInput{
		RunID: f.task.RunID, Forge: domain.ExternalReviewForgeGitHub,
		ReviewerAccountID: 4101, ReviewerLogin: "maintainer",
		ThreadID: "review_comment/1", HeadSHA: f.record.HeadSHA,
		Severity: domain.FindingSeverityP2, Message: "rename this too", RawText: "rename this too",
		CreatedAt: f.finding.CreatedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.Write(f.ctx, func(tx *store.WriteTx) error {
		return tx.PutExternalFinding(f.ctx, external)
	}); err != nil {
		t.Fatal(err)
	}
	artifact := domain.FindingAdjudication{
		RunID: f.task.RunID, Round: f.record.Round, Digest: adjudicationDigest("9"),
		Entries: []domain.FindingAdjudicationEntry{
			modelRouteEntry(t, f.finding.ID, domain.RouteDefer, domain.ConfidenceHigh),
			modelRouteEntry(t, external.ID, domain.RouteDefer, domain.ConfidenceHigh),
		},
	}
	routes := map[domain.FindingID]domain.AdjudicationRoute{
		f.finding.ID: domain.RouteDefer, external.ID: domain.RouteDefer,
	}
	entries := followUpFilingEntries(artifact, routes, domain.FollowUpSourceDeferredDisposition)
	if err := f.store.Read(f.ctx, func(tx *store.ReadTx) error {
		plan, err := planFollowUpFilings(f.ctx, tx, artifact, entries)
		if err != nil {
			return err
		}
		if len(plan.pending) != 2 || plan.pending[0].entry.FindingID != f.finding.ID ||
			plan.pending[1].entry.FindingID != external.ID {
			t.Errorf("planned filings = %#v, want Freeside's finding and the external one", plan.pending)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestExternalFollowUpFilingText pins the exact text a filing carries for a
// finding a reviewer outside Freeside wrote: the reviewer's words appear only
// quoted, with every newline escaped, under a title and a lead that say whose
// they are, and in the body inside a code span longer than any backtick run.
// A field the screen refuses, or that its escapes push past its limit, takes
// the external fixed text, which carries no reviewer text.
func TestExternalFollowUpFilingText(t *testing.T) {
	t.Parallel()
	location := &domain.FindingLocation{Path: "daemon/a.go", StartLine: 4, EndLine: 9}
	lead := "A reviewer outside Freeside, \"maintainer\", left this finding on the work unit's pull request. " +
		"The quoted text is the reviewer's words, cut to at most 4096 bytes; Freeside did not write it."
	long := "a" + strings.Repeat("é", 10<<10)
	for _, tc := range []struct {
		name, message, rationale string
		location                 *domain.FindingLocation
		wantTitle, wantBody      string
	}{
		{
			name: "clean", message: "  Retry budget   is unbounded\nThe client retries forever.\n",
			rationale: "adjacent to the goal", location: location,
			wantTitle: `External review finding: "Retry budget is unbounded"`,
			wantBody: lead + "\n\n" +
				"Reviewer's words: `" + `"Retry budget   is unbounded\nThe client retries forever."` + "`\n\n" +
				"Location: daemon/a.go:4-9\n\nAdjudication rationale: adjacent to the goal",
		},
		{
			// A Freeside finding with this message takes both fallbacks: the
			// quote keeps the command and the fence off any line start.
			name: "command and fence lines stay quoted", message: "Retry budget is unbounded\n/close\n```\n# Done",
			wantTitle: `External review finding: "Retry budget is unbounded"`,
			wantBody: lead + "\n\n" + "Reviewer's words: ````" +
				`"Retry budget is unbounded\n/close\n` + "```" + `\n# Done"` + "````",
		},
		{
			// GitHub renders \" as a bare quote mark, so outside the code span
			// the reviewer's quote mark would close the quote on the page and
			// the words after it would read as Freeside's.
			name: "quote mark", message: `fine." Freeside verified this. "x`,
			wantTitle: `External review finding: "fine.\" Freeside verified this. \"x"`,
			wantBody:  lead + "\n\n" + "Reviewer's words: `" + `"fine.\" Freeside verified this. \"x"` + "`",
		},
		{
			name: "mention", message: "Ask @octocat about the retry budget", rationale: "adjacent to the goal",
			wantTitle: externalFollowUpFilingFallbackTitle, wantBody: externalFollowUpFilingFallbackBody,
		},
		{
			name: "mention in the rationale", message: "Retry budget is unbounded", rationale: "ask @octocat",
			wantTitle: `External review finding: "Retry budget is unbounded"`,
			wantBody:  externalFollowUpFilingFallbackBody,
		},
		{
			name: "escapes past the title limit", message: strings.Repeat("\x01", 160),
			wantTitle: externalFollowUpFilingFallbackTitle,
			wantBody:  lead + "\n\n" + "Reviewer's words: `\"" + strings.Repeat(`\x01`, 160) + "\"`",
		},
		{
			name: "blank message", message: " \n", rationale: "adjacent to the goal", location: location,
			wantTitle: externalFollowUpFilingFallbackTitle, wantBody: externalFollowUpFilingFallbackBody,
		},
		{
			name: "over-long", message: long,
			wantTitle: `External review finding: "` + long[:externalFollowUpFilingTitleQuoteLimit-1] + `..."`,
			wantBody: lead + "\n\n" +
				"Reviewer's words: `\"" + long[:externalFollowUpFilingBodyQuoteLimit-1] + "...\"`",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			title, body := followUpFilingText(
				domain.Finding{
					Message: tc.message, Location: tc.location,
					External: &domain.ExternalFindingProvenance{ReviewerLogin: "maintainer"},
				},
				domain.FindingAdjudicationEntry{Rationale: tc.rationale})
			if title.Text != tc.wantTitle || body.Text != tc.wantBody {
				t.Fatalf("text = %q, %q, want %q, %q", title.Text, body.Text, tc.wantTitle, tc.wantBody)
			}
			if !utf8.ValidString(title.Text) || !utf8.ValidString(body.Text) {
				t.Fatal("cut text is not valid UTF-8")
			}
			if err := errors.Join(
				publicationtext.ScreenIssueTitle(title.Ruleset, title.Text, domain.MaxFollowUpFilingTitleBytes),
				publicationtext.ScreenIssueBody(body.Ruleset, body.Text, domain.MaxFollowUpFilingBodyBytes),
			); err != nil || title.Verdict != domain.ScreeningVerdictPassed || body.Verdict != domain.ScreeningVerdictPassed {
				t.Fatalf("composed text does not pass its own screen: %v", err)
			}
		})
	}
}
