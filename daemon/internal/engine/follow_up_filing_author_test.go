package engine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	"github.com/freeside-ai/freeside/daemon/internal/inference/fake"
	"github.com/freeside-ai/freeside/daemon/internal/publicationtext"
)

func followUpFilingTestDraft() inference.FollowUpFilingDraft {
	return inference.FollowUpFilingDraft{
		Title: "Bound the sync client's retries",
		Objective: "The sync client retries a failed push without limit. " +
			"Bound the retries so a dead remote stops the push instead of looping.",
		NonGoals:          "Changing the push protocol or its backoff curve.",
		AffectedContracts: "none",
		Acceptance: "- A push against a dead remote stops after the retry bound.\n" +
			"- A push that succeeds on a retry still completes.",
		Scope: "daemon/internal/sync/",
		Kind:  domain.FollowUpFilingKindFix,
	}
}

func followUpFilingTestFinding(external bool) (domain.Finding, domain.FindingAdjudicationEntry) {
	finding := domain.Finding{
		Message:  "Retry budget is unbounded\nThe client retries a failed push forever.",
		Location: &domain.FindingLocation{Path: "daemon/internal/sync/push.go", StartLine: 40, EndLine: 52},
	}
	if external {
		finding.Message = "Please bound the retries here, they loop \"forever\"."
		finding.External = &domain.ExternalFindingProvenance{ReviewerLogin: "maintainer"}
	}
	return finding, domain.FindingAdjudicationEntry{Rationale: "adjacent to the goal: the unit changes the push path, not its retries"}
}

// assertPassesFollowUpFilingScreen checks a rendered filing against the
// screen the store's gate runs again.
func assertPassesFollowUpFilingScreen(t *testing.T, title, body domain.ScreenedIssueText) {
	t.Helper()
	if err := errors.Join(
		publicationtext.ScreenIssueTitle(title.Ruleset, title.Text, domain.MaxFollowUpFilingTitleBytes),
		publicationtext.ScreenIssueBody(body.Ruleset, body.Text, domain.MaxFollowUpFilingBodyBytes),
	); err != nil || title.Verdict != domain.ScreeningVerdictPassed || body.Verdict != domain.ScreeningVerdictPassed {
		t.Fatalf("rendered text does not pass its own screen: %v", err)
	}
}

// The golden fixtures pin the rendered title, kind, and body for a full draft
// of each finding source, each single-field fallback, the title fallback, and
// the no-draft case, and each passes the issue-text screen.
func TestAuthoredFollowUpFilingGoldens(t *testing.T) {
	t.Parallel()
	refused := "Ask @octocat first."
	withField := func(field, text string) inference.FollowUpFilingDraft {
		draft := followUpFilingTestDraft()
		switch field {
		case "title":
			draft.Title = text
		case "objective":
			draft.Objective = text
		case "non_goals":
			draft.NonGoals = text
		case "affected_contracts":
			draft.AffectedContracts = text
		case "acceptance":
			draft.Acceptance = text
		case "scope":
			draft.Scope = text
		default:
			t.Fatalf("unknown field %q", field)
		}
		return draft
	}
	full := followUpFilingTestDraft()
	cases := []struct {
		name       string
		external   bool
		draft      *inference.FollowUpFilingDraft
		boundIssue int
	}{
		{"follow_up_filing_authored", false, &full, 1234},
		{"follow_up_filing_authored_external", true, &full, 0},
		{"follow_up_filing_unauthored", false, nil, 1234},
	}
	for _, field := range []string{"title", "objective", "non_goals", "affected_contracts", "acceptance", "scope"} {
		draft := withField(field, refused)
		cases = append(cases, struct {
			name       string
			external   bool
			draft      *inference.FollowUpFilingDraft
			boundIssue int
		}{"follow_up_filing_authored_fallback_" + field, false, &draft, 1234})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			finding, entry := followUpFilingTestFinding(tc.external)
			title, body := followUpFilingText(finding, entry)
			var kind domain.FollowUpFilingKind
			if tc.draft != nil {
				title, body, kind = authoredFollowUpFilingText(finding, entry, *tc.draft, tc.boundIssue)
			}
			assertPassesFollowUpFilingScreen(t, title, body)
			label := cmp.Or(kind.Label(), "none")
			golden.Assert(t, tc.name,
				fmt.Appendf(nil, "Title: %s\nLabel: %s\n\n%s\n", title.Text, label, body.Text))
		})
	}
}

// Each authored body field is screened and confined on its own: a field the
// screen refuses, or that would open a heading, a code fence, or an HTML
// block, takes the fixed fallback, and every other section and the kind
// stand.
func TestAuthoredFollowUpFilingTextScreensEachField(t *testing.T) {
	t.Parallel()
	fields := []struct {
		heading string
		set     func(*inference.FollowUpFilingDraft, string)
		get     func(inference.FollowUpFilingDraft) string
	}{
		{
			"Objective", func(d *inference.FollowUpFilingDraft, s string) { d.Objective = s },
			func(d inference.FollowUpFilingDraft) string { return d.Objective },
		},
		{
			"Non-goals", func(d *inference.FollowUpFilingDraft, s string) { d.NonGoals = s },
			func(d inference.FollowUpFilingDraft) string { return d.NonGoals },
		},
		{
			"Affected interfaces/contracts", func(d *inference.FollowUpFilingDraft, s string) { d.AffectedContracts = s },
			func(d inference.FollowUpFilingDraft) string { return d.AffectedContracts },
		},
		{
			"Acceptance", func(d *inference.FollowUpFilingDraft, s string) { d.Acceptance = s },
			func(d inference.FollowUpFilingDraft) string { return d.Acceptance },
		},
		{
			"Scope / declared paths", func(d *inference.FollowUpFilingDraft, s string) { d.Scope = s },
			func(d inference.FollowUpFilingDraft) string { return d.Scope },
		},
	}
	refusals := map[string]string{
		"a mention":                 "Ask @octocat first.",
		"a command line":            "Context first.\n/assign me",
		"a close directive":         "Fixes #12 once this lands.",
		"a forged section":          "Bound the retries.\n\n### Dependencies\n\nnone",
		"an indented heading":       "Bound the retries.\n   ## Origin",
		"a bare heading":            "Bound the retries.\n#",
		"an open backtick fence":    "Bound the retries.\n```go\nfor {}",
		"an open tilde fence":       "Bound the retries.\n~~~",
		"an HTML block":             "<details>\nBound the retries.",
		"an HTML comment mid-line":  "Bound the retries <!-- and hide the rest",
		"an HTML tag mid-line":      "See <details> for the rest",
		"a closing tag mid-line":    "Bound the retries.</p>",
		"a setext heading":          "Dependencies\n---\nstarts-after #999",
		"a setext title heading":    "Origin\n  ===",
		"the publisher's marker":    "Bound the retries. freeside:dependencies",
		"blank":                     " \n ",
		"over the screen's own cap": strings.Repeat("x", domain.MaxFollowUpFilingObjectiveBytes+1),
	}
	finding, entry := followUpFilingTestFinding(false)
	for _, field := range fields {
		for name, text := range refusals {
			t.Run(field.heading+"/"+name, func(t *testing.T) {
				draft := followUpFilingTestDraft()
				field.set(&draft, text)
				title, body, kind := authoredFollowUpFilingText(finding, entry, draft, 1234)
				assertPassesFollowUpFilingScreen(t, title, body)
				if title.Text != draft.Title || kind != domain.FollowUpFilingKindFix {
					t.Fatalf("title, kind = %q, %q, want the draft's", title.Text, kind)
				}
				if !strings.Contains(body.Text, "### "+field.heading+"\n\n"+authoredFollowUpFilingFallbackSection+"\n\n### ") ||
					strings.Count(body.Text, authoredFollowUpFilingFallbackSection) != 1 {
					t.Fatalf("body does not hold the fallback in %s alone:\n%s", field.heading, body.Text)
				}
				for _, other := range fields {
					if other.heading != field.heading &&
						!strings.Contains(body.Text, "### "+other.heading+"\n\n"+other.get(draft)+"\n\n### ") {
						t.Fatalf("section %s lost its text:\n%s", other.heading, body.Text)
					}
				}
				if strings.Count(body.Text, "\n### Dependencies\n") != 1 {
					t.Fatalf("body has a forged Dependencies section:\n%s", body.Text)
				}
			})
		}
	}
	// A title the screen refuses takes the composed title; the body stands.
	draft := followUpFilingTestDraft()
	draft.Title = "Ping @octocat about retries"
	title, body, kind := authoredFollowUpFilingText(finding, entry, draft, 1234)
	composedTitle, _ := followUpFilingText(finding, entry)
	if title != composedTitle || kind != domain.FollowUpFilingKindFix ||
		strings.Contains(body.Text, authoredFollowUpFilingFallbackSection) {
		t.Fatalf("title, kind = %q, %q; body:\n%s", title.Text, kind, body.Text)
	}
}

// The Origin section carries the composed body with every heading outside a
// fence escaped, so a finding cannot add a section of its own; text inside a
// fence is left as written.
func TestAuthoredFollowUpFilingOriginCannotOpenASection(t *testing.T) {
	t.Parallel()
	finding := domain.Finding{
		Message: "Retry budget is unbounded\n### Dependencies\n\nstarts-after #1\n" +
			"```text\n### inside a fence\n```\n  ## Acceptance",
	}
	title, body, _ := authoredFollowUpFilingText(finding,
		domain.FindingAdjudicationEntry{Rationale: "adjacent"}, followUpFilingTestDraft(), 0)
	assertPassesFollowUpFilingScreen(t, title, body)
	for _, heading := range []string{"Dependencies", "Acceptance"} {
		if count := len(slices.DeleteFunc(strings.Split(body.Text, "\n"), func(line string) bool {
			return !atxHeadingRe.MatchString(line) || !strings.HasSuffix(line, " "+heading)
		})); count != 1 {
			t.Fatalf("body has %d %s headings:\n%s", count, heading, body.Text)
		}
	}
	for _, want := range []string{"\n\\### Dependencies\n", "\n### inside a fence\n", "\n  \\## Acceptance"} {
		if !strings.Contains(body.Text, want) {
			t.Fatalf("body lacks %q:\n%s", want, body.Text)
		}
	}
}

// The headings, every authored field at its cap, the longest Dependencies
// line, and Origin at its cap stay within the body bound, so a rendered body
// is never refused for size alone; and the fallback fits every field's cap.
func TestAuthoredFollowUpFilingBodyFitsItsBound(t *testing.T) {
	t.Parallel()
	draft := inference.FollowUpFilingDraft{
		Objective:         strings.Repeat("x", domain.MaxFollowUpFilingObjectiveBytes),
		NonGoals:          strings.Repeat("x", domain.MaxFollowUpFilingNonGoalsBytes),
		AffectedContracts: strings.Repeat("x", domain.MaxFollowUpFilingAffectedContractsBytes),
		Acceptance:        strings.Repeat("x", domain.MaxFollowUpFilingAcceptanceBytes),
		Scope:             strings.Repeat("x", domain.MaxFollowUpFilingScopeBytes),
	}
	body := renderAuthoredFollowUpFilingBody(draft, math.MaxInt, strings.Repeat("x", 2*domain.MaxFollowUpFilingOriginBytes))
	if strings.Contains(body, authoredFollowUpFilingFallbackSection) {
		t.Fatalf("a field at its cap took the fallback")
	}
	if len(body) > domain.MaxFollowUpFilingBodyBytes {
		t.Fatalf("rendered body = %d bytes, over %d", len(body), domain.MaxFollowUpFilingBodyBytes)
	}
	for _, limit := range []int{
		domain.MaxFollowUpFilingObjectiveBytes, domain.MaxFollowUpFilingNonGoalsBytes,
		domain.MaxFollowUpFilingAffectedContractsBytes, domain.MaxFollowUpFilingAcceptanceBytes,
		domain.MaxFollowUpFilingScopeBytes,
	} {
		if len(authoredFollowUpFilingFallbackSection) > limit {
			t.Fatalf("the fallback is over a field cap of %d", limit)
		}
	}
}

// The Dependencies section holds one typed line naming the bound issue, or
// none, the shape docs/coordination.md (Work-Unit Issues) asks for.
func TestFollowUpFilingDependenciesLine(t *testing.T) {
	t.Parallel()
	if got := followUpFilingDependencies(0); got != "none" {
		t.Fatalf("unbound = %q", got)
	}
	got := followUpFilingDependencies(42)
	if strings.Contains(got, "\n") || !strings.HasPrefix(got, "starts-after #42: ") {
		t.Fatalf("bound = %q", got)
	}
}

func authorRequests(f *findingAdjudicationFixture) []inference.Request {
	return slices.DeleteFunc(f.driver.Requests(), func(request inference.Request) bool {
		return request.SiteID != inference.FollowUpFilingAuthorSiteID
	})
}

func scriptFollowUpFilingAuthor(f *findingAdjudicationFixture, body string) {
	f.driver.Script(inference.FollowUpFilingAuthorSiteID,
		fake.Script{Response: inference.Response{Output: []byte(body), ComputeUnits: 1}})
}

// A finding the duplicate search answered "new" is drafted by the site: the
// proposal stores the rendered title and body and the kind, and its labels
// are the policy's plus the kind's.
func TestAuthoredFollowUpFilingCarriesTheKindLabel(t *testing.T) {
	t.Parallel()
	f := newFollowUpFilingFixture(t, domain.FindingSeverityP2, domain.RouteDefer, map[string]string{
		domain.PolicyFollowUpFilingLabels:    "lane:spine, deferral",
		domain.PolicyFollowUpFilingMilestone: "1B",
	})
	seedFollowUpFilingSubject(t, f, true)
	scriptFollowUpFilingAuthor(f, fake.FollowUpFilingAuthorDraft)
	reconcileFollowUpFiling(t, f, productionReviewPassed)

	state := readFollowUpFilingState(t, f, 1)
	assertOneFilingCard(t, f, state, domain.FollowUpSourceDeferredDisposition)
	filing := state.instances[0].Proposal.FilingProposal
	if filing.Kind != domain.FollowUpFilingKindFix ||
		!slices.Equal(filing.Labels, []string{"deferral", "kind:fix", "lane:spine"}) ||
		filing.Milestone == nil || *filing.Milestone != "1B" {
		t.Fatalf("filing kind, labels, milestone = %q, %v, %v", filing.Kind, filing.Labels, filing.Milestone)
	}
	if filing.Title.Text != "Bound the sync client's retries" {
		t.Fatalf("title = %q", filing.Title.Text)
	}
	for _, want := range []string{
		"### Objective\n\nThe sync client retries a failed push without limit.",
		"### Scope / declared paths\n\ndaemon/a.go\n\n",
		"### Dependencies\n\nnone\n\n",
		"### Origin\n\nreview finding\n\nLocation: daemon/a.go:1\n\nAdjudication rationale: deterministic fake route",
	} {
		if !strings.Contains(filing.Body.Text, want) {
			t.Fatalf("body lacks %q:\n%s", want, filing.Body.Text)
		}
	}
	requests := authorRequests(f)
	if len(requests) != 1 {
		t.Fatalf("author requests = %d, want 1", len(requests))
	}
	fields := requests[0].Fields
	if fields["repository"] != "owner/repo" || !strings.HasPrefix(fields["declared_paths"], "[") ||
		!strings.Contains(fields["finding"], `"rationale":"deterministic fake route"`) {
		t.Fatalf("author fields = %#v", fields)
	}
	// A later pass finds the instance and asks the site nothing.
	replayFollowUpFiling(t, f, state.artifact)
	if len(authorRequests(f)) != 1 {
		t.Fatalf("a replay asked the site again")
	}
}

// A site that answers badly, or not at all, leaves the filing exactly as it
// was composed before the site: the text TestDeferredDispositionProposes-
// OneFollowUpFiling pins, the policy labels, and no kind.
func TestUnusableFollowUpFilingDraftLeavesTheFilingComposed(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{
		"a kind outside the enum": fake.FollowUpFilingAuthorBadKind,
		"a malformed answer":      fake.FollowUpFilingAuthorMalformed,
		"no answer":               "",
	} {
		t.Run(name, func(t *testing.T) {
			f := newFollowUpFilingFixture(t, domain.FindingSeverityP2, domain.RouteDefer, map[string]string{
				domain.PolicyFollowUpFilingLabels: "lane:spine, deferral",
			})
			seedFollowUpFilingSubject(t, f, true)
			if body != "" {
				scriptFollowUpFilingAuthor(f, body)
			}
			reconcileFollowUpFiling(t, f, productionReviewPassed)
			state := readFollowUpFilingState(t, f, 1)
			assertOneFilingCard(t, f, state, domain.FollowUpSourceDeferredDisposition)
			filing := state.instances[0].Proposal.FilingProposal
			wantBody := "review finding\n\nLocation: daemon/a.go:1\n\nAdjudication rationale: deterministic fake route"
			if filing.Title.Text != "review finding" || filing.Body.Text != wantBody || filing.Kind != "" ||
				!slices.Equal(filing.Labels, []string{"deferral", "lane:spine"}) {
				t.Fatalf("filing = %#v", filing)
			}
			if len(authorRequests(f)) != 1 {
				t.Fatalf("author requests = %d, want 1", len(authorRequests(f)))
			}
		})
	}
}

// A refused dispatch claim (the workflow closed) ends drafting without a call
// and without stopping the pass, so the filing is composed as before the site.
func TestRefusedDispatchClaimLeavesTheFilingComposed(t *testing.T) {
	t.Parallel()
	f := newFollowUpFilingFixture(t, domain.FindingSeverityP2, domain.RouteDefer, map[string]string{
		domain.PolicyFollowUpFilingLabels: "lane:spine, deferral",
	})
	seedFollowUpFilingSubject(t, f, true)
	scriptFollowUpFilingAuthor(f, fake.FollowUpFilingAuthorDraft)
	f.workflow.claimDispatch = func(context.Context, domain.RunID) error { return errors.New("workflow closed") }
	reconcileFollowUpFiling(t, f, productionReviewPassed)
	state := readFollowUpFilingState(t, f, 1)
	filing := state.instances[0].Proposal.FilingProposal
	if filing.Title.Text != "review finding" || filing.Kind != "" ||
		!slices.Equal(filing.Labels, []string{"deferral", "lane:spine"}) {
		t.Fatalf("filing = %#v", filing)
	}
	if len(authorRequests(f)) != 0 {
		t.Fatalf("author requests = %d, want 0", len(authorRequests(f)))
	}
}

// A policy that already fills the label bound leaves no room for the kind's
// label, so the filing takes the authored text without a kind rather than a
// label set every pass would refuse.
func TestFollowUpFilingKindYieldsToAFullLabelSet(t *testing.T) {
	t.Parallel()
	labels := make([]string, domain.MaxFollowUpFilingLabels)
	for i := range labels {
		labels[i] = fmt.Sprintf("label-%02d", i)
	}
	f := newFollowUpFilingFixture(t, domain.FindingSeverityP2, domain.RouteDefer, map[string]string{
		domain.PolicyFollowUpFilingLabels: strings.Join(labels, ","),
	})
	seedFollowUpFilingSubject(t, f, true)
	scriptFollowUpFilingAuthor(f, fake.FollowUpFilingAuthorDraft)
	reconcileFollowUpFiling(t, f, productionReviewPassed)
	state := readFollowUpFilingState(t, f, 1)
	assertOneFilingCard(t, f, state, domain.FollowUpSourceDeferredDisposition)
	filing := state.instances[0].Proposal.FilingProposal
	if filing.Kind != "" || !slices.Equal(filing.Labels, labels) ||
		!strings.Contains(filing.Body.Text, "### Objective\n\n") {
		t.Fatalf("filing kind, labels = %q, %v; body:\n%s", filing.Kind, filing.Labels, filing.Body.Text)
	}
}
