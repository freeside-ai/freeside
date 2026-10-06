package publish_test

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

// pastSettle outlasts the default settle interval.
const pastSettle = 11 * time.Minute

// unprovenCommitted is a create that committed and answered 500.
var unprovenCommitted = fakeCreateResponse{Status: http.StatusInternalServerError, Commit: true}

// unprovenLost is a create that answered 500 and created nothing.
var unprovenLost = fakeCreateResponse{Status: http.StatusInternalServerError}

func TestFollowUpFilerFilesAnApprovedFilingOnce(t *testing.T) {
	h := newFilingHarness(t)
	run := h.seedRun("run-a", nil, nil)
	approved := h.approve(run, 0)
	declined := h.propose(run, 1, domain.ActionDecline)
	undecided := h.propose(run, 2, "")

	h.mustPass()

	issues := h.forgeIssues()
	if len(issues) != 1 {
		t.Fatalf("forge holds %d issues, want 1", len(issues))
	}
	issue := issues[0]
	if issue.Title != filingTitle(run, 0) || !strings.HasPrefix(issue.Body, "The sync client retries") ||
		strings.Contains(issue.Body, "freeside") ||
		!slices.Equal(issue.Labels, approved.Proposal.FilingProposal.Labels) || len(issue.Labels) != 2 ||
		issue.Milestone == nil || *issue.Milestone != 7 {
		t.Fatalf("created issue = %+v", issue)
	}
	h.assertLedgered(approved.ID, issue.Number)
	for name, instance := range map[string]domain.ProposalInstance{"declined": declined, "undecided": undecided} {
		if intent := h.intent(instance.ID); intent != nil {
			t.Fatalf("%s filing opened intent %+v", name, intent)
		}
	}

	h.advance(pastSettle)
	h.mustPass()
	h.restart()
	h.mustPass()
	if got := h.creates(); got != 1 {
		t.Fatalf("create requests = %d, want 1", got)
	}
}

// TestFollowUpFilerSurvivesAKillAtEveryStep stops a filing at each point
// where the filer reaches outside the store, which brackets every durable
// step and both sides of the create call, then restarts the daemon. Whatever
// the stop point, recovery ends in one issue and one ledger row, or in the
// ambiguous outcome and its notice, and never sends a second create.
func TestFollowUpFilerSurvivesAKillAtEveryStep(t *testing.T) {
	const createPath = "POST " + testRepoPath + "/issues"
	var stops []string
	for kill := 1; ; kill++ {
		stoppedAt := ""
		t.Run(fmt.Sprintf("stop-%d", kill), func(t *testing.T) {
			h := newFilingHarness(t)
			instance := h.approve(h.seedRun("run-a", nil, nil), 0)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			steps := 0
			h.step = func(name string) {
				if steps++; steps == kill {
					stoppedAt = name
					cancel()
				}
			}
			err := h.filer.Pass(ctx)
			h.step = nil
			if stoppedAt == "" {
				if err != nil {
					t.Fatalf("uninterrupted pass: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("pass stopped at %q reported no error", stoppedAt)
			}

			h.restart()
			h.mustPass()
			h.advance(pastSettle)
			h.mustPass()
			h.advance(pastSettle)
			h.mustPass()

			issues := h.forgeIssues()
			if got := h.creates(); got > 1 || len(issues) > 1 {
				t.Fatalf("stopped at %q: %d create requests, %d issues", stoppedAt, got, len(issues))
			}
			// Only a create that was marked dispatched and never reached the
			// forge leaves nothing to adopt.
			if stoppedAt == "send "+createPath {
				if len(issues) != 0 {
					t.Fatalf("stopped at %q: forge holds %+v", stoppedAt, issues)
				}
				h.assertTerminal(instance.ID, domain.FollowUpFilingAmbiguous, "", "follow_up_filing_ambiguous")
				return
			}
			if len(issues) != 1 {
				t.Fatalf("stopped at %q: forge holds %d issues, want 1", stoppedAt, len(issues))
			}
			h.assertLedgered(instance.ID, issues[0].Number)
		})
		if stoppedAt == "" {
			break
		}
		stops = append(stops, stoppedAt)
	}
	// The stop points must include both sides of the create call, a point
	// after the intent opened, and a point after the pre-dispatch set was
	// recorded (the create request's own token).
	for _, want := range []string{"send " + createPath, "receive " + createPath} {
		if !slices.Contains(stops, want) {
			t.Fatalf("stop points %q never reached %q", stops, want)
		}
	}
	if last := stops[len(stops)-3:]; last[0] != "token" {
		t.Fatalf("stop points end %q, want the create request's token before its send", last)
	}
}

func TestFollowUpFilerSerializesFilingsInARepository(t *testing.T) {
	h := newFilingHarness(t)
	run := h.seedRun("run-a", nil, nil)
	first, second := h.approve(run, 0), h.approve(run, 1)
	h.respond(unprovenCommitted)

	h.mustPass()
	h.mustPass()
	if intent := h.intent(first.ID); intent == nil || intent.Terminal != nil {
		t.Fatalf("first filing intent = %+v, want outstanding", intent)
	}
	if intent := h.intent(second.ID); intent != nil {
		t.Fatalf("second filing opened %+v while the first is outstanding", intent)
	}
	if got := h.creates(); got != 1 {
		t.Fatalf("create requests = %d, want 1", got)
	}

	h.advance(pastSettle)
	h.mustPass()
	issues := h.forgeIssues()
	if len(issues) != 2 {
		t.Fatalf("forge holds %d issues, want 2", len(issues))
	}
	h.assertLedgered(first.ID, issues[0].Number)
	h.assertLedgered(second.ID, issues[1].Number)
}

func TestFollowUpFilerValidatesCandidates(t *testing.T) {
	const adopted, none = "adopted", "ambiguous"
	cases := []struct {
		name string
		// before plants issues before the create, after plants them once the
		// create went unproven.
		before, after func(h *filingHarness)
		want          string
		wantNumber    int
	}{
		{name: "one candidate", after: func(h *filingHarness) { h.plant(h.botIssue(900)) }, want: adopted, wantNumber: 900},
		{name: "no candidate", want: none},
		{name: "two candidates", after: func(h *filingHarness) {
			h.plant(h.botIssue(900))
			h.plant(h.botIssue(901))
		}, want: none},
		{name: "another account with the bot login", after: func(h *filingHarness) {
			issue := h.botIssue(900)
			issue.UserID = filingBotID + 1
			h.plant(issue)
		}, want: none},
		{name: "a user account with the bot id", after: func(h *filingHarness) {
			issue := h.botIssue(900)
			issue.UserType = "User"
			h.plant(issue)
		}, want: none},
		{name: "created before the intent opened", after: func(h *filingHarness) {
			issue := h.botIssue(900)
			issue.UpdatedAt = issue.CreatedAt
			issue.CreatedAt = issue.CreatedAt.Add(-time.Hour)
			h.plant(issue)
		}, want: none},
		{name: "in the pre-dispatch set", before: func(h *filingHarness) { h.plant(h.botIssue(900)) }, want: none},
		// The window opens a clock-skew allowance before the intent: an issue
		// stamped inside it is the filing's own if it appeared after the
		// create was sent, and is not if it was already there.
		{name: "stamped inside the skew allowance after the create", after: func(h *filingHarness) {
			issue := h.botIssue(900)
			issue.CreatedAt = issue.CreatedAt.Add(-2 * time.Minute)
			h.plant(issue)
		}, want: adopted, wantNumber: 900},
		{name: "stamped inside the skew allowance before the create", before: func(h *filingHarness) {
			issue := h.botIssue(900)
			issue.CreatedAt = issue.CreatedAt.Add(-2 * time.Minute)
			h.plant(issue)
		}, want: none},
		{name: "stamped before the skew allowance", after: func(h *filingHarness) {
			issue := h.botIssue(900)
			issue.UpdatedAt = issue.CreatedAt
			issue.CreatedAt = issue.CreatedAt.Add(-6 * time.Minute)
			h.plant(issue)
		}, want: none},
		{name: "one candidate listed twice", after: func(h *filingHarness) {
			h.plant(h.botIssue(900))
			h.plant(h.botIssue(900))
		}, want: adopted, wantNumber: 900},
		{name: "a pull request", after: func(h *filingHarness) {
			issue := h.botIssue(900)
			issue.IsPR = true
			h.plant(issue)
		}, want: none},
		{name: "one candidate among issues that are not", before: func(h *filingHarness) {
			h.plant(h.botIssue(899))
		}, after: func(h *filingHarness) {
			pull := h.botIssue(901)
			pull.IsPR = true
			h.plant(pull)
			h.plant(h.botIssue(900))
			h.gh.filing.pageSize = 1
		}, want: adopted, wantNumber: 900},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newFilingHarness(t)
			instance := h.approve(h.seedRun("run-a", nil, nil), 0)
			if tc.before != nil {
				tc.before(h)
			}
			h.respond(unprovenLost)
			h.mustPass()
			if tc.after != nil {
				tc.after(h)
			}

			h.advance(9 * time.Minute)
			h.mustPass()
			if intent := h.intent(instance.ID); intent == nil || intent.Terminal != nil {
				t.Fatalf("intent before the settle interval = %+v, want outstanding", intent)
			}
			h.advance(2 * time.Minute)
			h.mustPass()

			if tc.want == adopted {
				h.assertLedgered(instance.ID, tc.wantNumber)
			} else {
				item := h.assertTerminal(instance.ID, domain.FollowUpFilingAmbiguous, "", "follow_up_filing_ambiguous")
				if !strings.Contains(item.Reason, filingRepo) || !strings.Contains(item.Reason, fmt.Sprint(testRepoID)) {
					t.Fatalf("ambiguous notice %q does not name the repository", item.Reason)
				}
			}
			if got := h.creates(); got != 1 {
				t.Fatalf("create requests = %d, want 1", got)
			}
		})
	}
}

func TestFollowUpFilerBoundsAnIncompleteListing(t *testing.T) {
	t.Run("recovers within the bound", func(t *testing.T) {
		t.Parallel()
		h := newFilingHarness(t)
		instance := h.approve(h.seedRun("run-a", nil, nil), 0)
		h.respond(unprovenCommitted)
		h.mustPass()
		h.advance(pastSettle)
		h.gh.filing.listFailures = 2
		for range 2 {
			if err := h.pass(); err == nil {
				t.Fatal("pass with a failed listing reported no error")
			}
		}
		h.mustPass()
		h.assertLedgered(instance.ID, h.forgeIssues()[0].Number)
	})
	t.Run("ends ambiguous at the bound", func(t *testing.T) {
		t.Parallel()
		h := newFilingHarness(t)
		instance := h.approve(h.seedRun("run-a", nil, nil), 0)
		h.respond(unprovenCommitted)
		h.mustPass()
		h.advance(pastSettle)
		h.gh.filing.listFailures = 3
		for range 2 {
			if err := h.pass(); err == nil {
				t.Fatal("pass with a failed listing reported no error")
			}
		}
		h.mustPass()
		h.assertTerminal(instance.ID, domain.FollowUpFilingAmbiguous, "", "follow_up_filing_ambiguous")
	})
	t.Run("a repository that now resolves elsewhere", func(t *testing.T) {
		t.Parallel()
		h := newFilingHarness(t)
		instance := h.approve(h.seedRun("run-a", nil, nil), 0)
		h.respond(unprovenCommitted)
		h.mustPass()
		h.advance(pastSettle)
		h.gh.repositoryID = testRepoID + 1
		for range 2 {
			if err := h.pass(); err == nil {
				t.Fatal("pass against a rebound repository reported no error")
			}
		}
		h.mustPass()
		h.assertTerminal(instance.ID, domain.FollowUpFilingAmbiguous, "", "follow_up_filing_ambiguous")
	})
}

func TestFollowUpFilerAmbiguousRepositoryAdoptsNothing(t *testing.T) {
	h := newFilingHarness(t)
	run := h.seedRun("run-a", nil, nil)
	first := h.approve(run, 0)
	h.respond(unprovenLost)
	h.mustPass()
	h.advance(pastSettle)
	h.mustPass()
	h.assertTerminal(first.ID, domain.FollowUpFilingAmbiguous, "", "follow_up_filing_ambiguous")

	// A later unproven create in the repository has exactly one candidate,
	// and still ends ambiguous without waiting the settle interval.
	second := h.approve(run, 1)
	h.respond(unprovenCommitted)
	h.mustPass()
	h.mustPass()
	h.assertTerminal(second.ID, domain.FollowUpFilingAmbiguous, "", "follow_up_filing_ambiguous")

	// A confirmed create response still ledgers there.
	third := h.approve(run, 2)
	h.mustPass()
	issues := h.forgeIssues()
	h.assertLedgered(third.ID, issues[len(issues)-1].Number)
	if got := h.creates(); got != 3 {
		t.Fatalf("create requests = %d, want 3", got)
	}
}

func TestFollowUpFilerRefusesADefiniteRejection(t *testing.T) {
	for _, status := range []int{
		http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden,
		http.StatusNotFound, http.StatusGone, http.StatusUnprocessableEntity,
	} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			t.Parallel()
			h := newFilingHarness(t)
			run := h.seedRun("run-a", nil, nil)
			instance, next := h.approve(run, 0), h.approve(run, 1)
			h.respond(fakeCreateResponse{Status: status, Body: `{"message":"Validation Failed: SECRET-BODY"}`})
			h.mustPass()
			item := h.assertTerminal(instance.ID, domain.FollowUpFilingRefused,
				domain.FollowUpFilingRefusalDefiniteRejection, "follow_up_filing_refused")
			if !strings.Contains(item.Reason, fmt.Sprint(status)) || strings.Contains(item.Reason, "SECRET-BODY") {
				t.Fatalf("refusal notice = %q", item.Reason)
			}
			// A refusal frees the repository for the next filing at once.
			h.assertLedgered(next.ID, h.forgeIssues()[0].Number)
			h.advance(pastSettle)
			h.mustPass()
			if got := h.creates(); got != 2 {
				t.Fatalf("create requests = %d, want 2", got)
			}
		})
	}
}

func TestFollowUpFilerRetriesATransientRejectionWithinTheBound(t *testing.T) {
	transient := map[string]fakeCreateResponse{
		"429":                      {Status: http.StatusTooManyRequests},
		"403 rate limit exhausted": {Status: http.StatusForbidden, Header: map[string]string{"X-RateLimit-Remaining": "0"}},
		"403 retry-after":          {Status: http.StatusForbidden, Header: map[string]string{"Retry-After": "60"}},
	}
	for name, response := range transient {
		t.Run(name+" spends the bound", func(t *testing.T) {
			t.Parallel()
			h := newFilingHarness(t)
			instance := h.approve(h.seedRun("run-a", nil, nil), 0)
			h.respond(response, response, response)
			for attempt := 1; attempt <= 3; attempt++ {
				h.mustPass()
				// A pass inside the backoff sends nothing.
				h.advance(30 * time.Second)
				h.mustPass()
				if got := h.creates(); got != attempt {
					t.Fatalf("create requests after attempt %d = %d", attempt, got)
				}
				h.advance(31 * time.Second)
			}
			h.assertTerminal(instance.ID, domain.FollowUpFilingRefused,
				domain.FollowUpFilingRefusalRetryBoundSpent, "follow_up_filing_refused")
			h.advance(pastSettle)
			h.mustPass()
			if got := h.creates(); got != 3 {
				t.Fatalf("create requests = %d, want 3", got)
			}
		})
		t.Run(name+" then files", func(t *testing.T) {
			t.Parallel()
			h := newFilingHarness(t)
			instance := h.approve(h.seedRun("run-a", nil, nil), 0)
			h.respond(response)
			h.mustPass()
			h.restart()
			h.mustPass()
			if got := h.creates(); got != 1 {
				t.Fatalf("create requests inside the backoff = %d, want 1", got)
			}
			h.advance(time.Minute)
			h.mustPass()
			h.assertLedgered(instance.ID, h.forgeIssues()[0].Number)
		})
	}
}

func TestFollowUpFilerNeverResendsAnUnprovenCreate(t *testing.T) {
	foreign := `{"number":1,"user":{"id":1,"type":"User"},"created_at":"2026-09-02T12:00:00Z"}`
	unproven := map[string]fakeCreateResponse{
		"500":                           {Status: http.StatusInternalServerError},
		"502":                           {Status: http.StatusBadGateway},
		"503":                           {Status: http.StatusServiceUnavailable},
		"200":                           {Status: http.StatusOK},
		"202":                           {Status: http.StatusAccepted},
		"409":                           {Status: http.StatusConflict},
		"201 that is not an issue":      {Body: `{"message":"ok"}`},
		"201 that is not JSON":          {Body: `<html>`},
		"201 for another account":       {Body: foreign},
		"transport error after sending": {Drop: true},
	}
	for name, response := range unproven {
		for _, committed := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s committed=%t", name, committed), func(t *testing.T) {
				t.Parallel()
				h := newFilingHarness(t)
				instance := h.approve(h.seedRun("run-a", nil, nil), 0)
				// A 201 override always commits in the fake; drop the issue
				// again for the case where the create did not.
				create := response
				create.Commit = committed
				h.respond(create)
				h.mustPass()
				if !committed {
					h.gh.filing.issues = nil
				}
				h.mustPass()
				h.advance(pastSettle)
				h.mustPass()
				if committed {
					h.assertLedgered(instance.ID, h.forgeIssues()[0].Number)
				} else {
					h.assertTerminal(instance.ID, domain.FollowUpFilingAmbiguous, "", "follow_up_filing_ambiguous")
				}
				h.advance(pastSettle)
				h.mustPass()
				if got := h.creates(); got != 1 {
					t.Fatalf("create requests = %d, want 1", got)
				}
			})
		}
	}
}

// TestFollowUpFilerDoesNotFileAnInstanceThatFailsTheScreen rewrites an
// approved filing's stored body to text the screen rejects, with the row's
// digests kept consistent. The store refuses the instance on every read (the
// card's bound digest no longer matches, and the instance gate re-screens the
// text), so the filer never sends it; the pass raises a notice, reports the
// error, and still files the other instances.
func TestFollowUpFilerDoesNotFileAnInstanceThatFailsTheScreen(t *testing.T) {
	const rejected = "Closes #12. Ask @octocat."
	assertNotFiled := func(t *testing.T, h *filingHarness, instance domain.ProposalInstance, creates int) {
		t.Helper()
		if err := h.pass(); err == nil || !strings.Contains(err.Error(), string(instance.ID)) {
			t.Fatalf("pass over a rewritten instance = %v, want an error naming it", err)
		}
		item := h.healthItem(instance.ID, "follow_up_filing_unreadable")
		if item == nil || !strings.Contains(item.Reason, string(instance.ID)) || strings.Contains(item.Reason, "octocat") {
			t.Fatalf("rewritten filing notice = %+v", item)
		}
		// The intent does not read either, so the notice cannot know whether
		// a create was sent and must not say that none was.
		if !strings.Contains(item.Reason, "the issue may exist") {
			t.Fatalf("rewritten filing notice does not warn that an issue may exist: %q", item.Reason)
		}
		for _, issue := range h.forgeIssues() {
			if issue.Body == rejected {
				t.Fatalf("the rewritten body reached the forge: %+v", issue)
			}
		}
		if got := h.creates(); got != creates {
			t.Fatalf("create requests = %d, want %d", got, creates)
		}
	}
	t.Run("before the intent opens", func(t *testing.T) {
		t.Parallel()
		h := newFilingHarness(t)
		run := h.seedRun("run-a", nil, nil)
		rewritten, intact := h.approve(run, 0), h.approve(run, 1)
		h.rewriteBody(rewritten, run, 0, rejected)

		assertNotFiled(t, h, rewritten, 1)
		if intent := h.intent(intact.ID); intent == nil {
			t.Fatal("the intact filing opened no intent")
		}
		h.assertLedgered(intact.ID, h.forgeIssues()[0].Number)

		// Repaired rows read again: the filing files and its notice resolves.
		h.assertNotice(rewritten.ID, "follow_up_filing_unreadable", domain.StatusOpen)
		h.rewriteBody(rewritten, run, 0, filingBody)
		h.mustPass()
		h.assertLedgered(rewritten.ID, h.forgeIssues()[1].Number)
		h.assertNotice(rewritten.ID, "follow_up_filing_unreadable", domain.StatusResolved)
	})
	// Every ledger write re-runs the instance gate, so an intent that is
	// already open cannot be refused once its instance stops reading: it
	// stays outstanding, and the notice says why.
	t.Run("after the intent opens", func(t *testing.T) {
		t.Parallel()
		h := newFilingHarness(t)
		run := h.seedRun("run-a", nil, nil)
		rewritten := h.approve(run, 0)
		h.respond(fakeCreateResponse{Status: http.StatusTooManyRequests})
		h.mustPass()
		h.rewriteBody(rewritten, run, 0, rejected)
		h.advance(pastSettle)

		assertNotFiled(t, h, rewritten, 1)

		// The store rebuilds every intent in a repository before it opens
		// another, so a later filing there waits: nothing is sent for it, and
		// the stall notice says it is not finished.
		behind := h.approve(run, 1)
		assertNotFiled(t, h, rewritten, 1)
		h.advance(16 * time.Minute)
		assertNotFiled(t, h, rewritten, 1)
		if intent := h.intent(behind.ID); intent != nil {
			t.Fatalf("filing behind an unreadable intent opened %+v", intent)
		}
		if h.healthItem(behind.ID, "follow_up_filing_stalled") == nil {
			t.Fatal("filing behind an unreadable intent raised no stall notice")
		}
		if h.healthItem(rewritten.ID, "follow_up_filing_stalled") != nil {
			t.Fatal("the unreadable filing raised a stall notice beside its own")
		}
	})
}

func TestFollowUpFilerRateCap(t *testing.T) {
	t.Run("set", func(t *testing.T) {
		t.Parallel()
		h := newFilingHarness(t)
		run := h.seedRun("run-a", map[string]string{"follow_up_filing.max_per_day": "2"}, nil)
		filings := []domain.ProposalInstance{h.approve(run, 0), h.approve(run, 1), h.approve(run, 2)}
		h.mustPass()
		h.mustPass()
		if got := h.creates(); got != 2 {
			t.Fatalf("create requests = %d, want 2", got)
		}
		if intent := h.intent(filings[2].ID); intent != nil {
			t.Fatalf("capped filing opened %+v", intent)
		}
		item := h.healthItem(filings[2].ID, "follow_up_filing_waiting")
		if item == nil || !strings.Contains(item.Reason, "follow_up_filing.max_per_day") ||
			!strings.Contains(item.Reason, filingRepo) {
			t.Fatalf("capped filing notice = %+v", item)
		}

		h.advance(24*time.Hour - time.Second)
		h.mustPass()
		if got := h.creates(); got != 2 {
			t.Fatalf("create requests inside the window = %d, want 2", got)
		}
		h.assertNotice(filings[2].ID, "follow_up_filing_waiting", domain.StatusOpen)
		h.advance(2 * time.Second)
		h.mustPass()
		h.assertLedgered(filings[2].ID, h.forgeIssues()[2].Number)
		// The wait is over, so its notice does not stay open.
		h.assertNotice(filings[2].ID, "follow_up_filing_waiting", domain.StatusResolved)
	})
	// The window counts from when a create was sent. An intent that waited
	// open through an outage longer than the window still holds its slot once
	// it files.
	t.Run("intent opened before the window", func(t *testing.T) {
		t.Parallel()
		h := newFilingHarness(t)
		run := h.seedRun("run-a", map[string]string{"follow_up_filing.max_per_day": "1"}, nil)
		delayed, held := h.approve(run, 0), h.approve(run, 1)
		working := h.tokens
		h.tokens = errTokenSource{&publish.APIError{
			Status: http.StatusInternalServerError, RequestPath: "/app/installations/1/access_tokens",
		}}
		if err := h.pass(); err == nil {
			t.Fatal("pass with a failing token mint reported no error")
		}
		if intent := h.intent(delayed.ID); intent == nil || len(intent.Attempts) != 0 {
			t.Fatalf("delayed intent = %+v, want open with no attempt", intent)
		}
		h.advance(25 * time.Hour)
		h.tokens = working
		h.mustPass()
		h.mustPass()
		h.assertLedgered(delayed.ID, h.forgeIssues()[0].Number)
		if got := h.creates(); got != 1 {
			t.Fatalf("create requests = %d, want 1", got)
		}
		if intent := h.intent(held.ID); intent != nil || h.healthItem(held.ID, "follow_up_filing_waiting") == nil {
			t.Fatalf("held filing: intent = %+v, want a waiting notice and no intent", intent)
		}
	})
	t.Run("default", func(t *testing.T) {
		t.Parallel()
		h := newFilingHarness(t)
		for _, name := range []string{"run-a", "run-b", "run-c"} {
			run := h.seedRun(name, nil, nil)
			for finding := range 4 {
				h.approve(run, finding)
			}
		}
		h.mustPass()
		h.mustPass()
		if got := h.creates(); got != 10 {
			t.Fatalf("create requests = %d, want the default cap of 10", got)
		}
	})
}

// TestFollowUpFilerRefusalAfterAWaitHasItsOwnNotice holds a filing at the
// rate cap and then refuses it at the per-run cap: the refusal's notice is
// not the waiting notice it already had. The waiting notice resolves; the
// refusal's records an outcome and stays open.
func TestFollowUpFilerRefusalAfterAWaitHasItsOwnNotice(t *testing.T) {
	t.Parallel()
	h := newFilingHarness(t)
	run := h.seedRun("run-a", map[string]string{
		"follow_up_filing.max_per_day": "1", "follow_up_filing.max_per_run": "1",
	}, nil)
	h.approve(run, 0)
	held := h.approve(run, 1)
	h.mustPass()
	if h.healthItem(held.ID, "follow_up_filing_waiting") == nil || h.intent(held.ID) != nil {
		t.Fatalf("held filing: intent = %+v, want a waiting notice and no intent", h.intent(held.ID))
	}
	h.advance(24*time.Hour + time.Second)
	h.mustPass()
	h.assertNotice(held.ID, "follow_up_filing_waiting", domain.StatusResolved)
	h.mustPass()
	h.assertTerminal(held.ID, domain.FollowUpFilingRefused,
		domain.FollowUpFilingRefusalPreconditionFailed, "follow_up_filing_capped")
	if got := h.creates(); got != 1 {
		t.Fatalf("create requests = %d, want 1", got)
	}
}

// TestFollowUpFilerToleratesAClockAheadOfTheForge runs the daemon's clock
// ahead of the forge's, so the filing's own issue is stamped before the
// intent opened. Inside the allowance the filing still completes, from the
// create response and from the settled listing alike; beyond it the filing
// fails closed.
func TestFollowUpFilerToleratesAClockAheadOfTheForge(t *testing.T) {
	t.Parallel()
	skewed := func(t *testing.T, behind time.Duration) (*filingHarness, domain.ProposalInstance) {
		h := newFilingHarness(t)
		h.advance(700 * time.Millisecond)
		h.gh.filing.now = func() time.Time { return h.now.Add(-behind) }
		return h, h.approve(h.seedRun("run-a", nil, nil), 0)
	}
	t.Run("create response", func(t *testing.T) {
		t.Parallel()
		h, instance := skewed(t, 90*time.Second)
		h.mustPass()
		h.assertLedgered(instance.ID, h.forgeIssues()[0].Number)
	})
	t.Run("settled listing", func(t *testing.T) {
		t.Parallel()
		h, instance := skewed(t, 90*time.Second)
		h.respond(unprovenCommitted)
		h.mustPass()
		h.advance(pastSettle)
		h.mustPass()
		h.assertLedgered(instance.ID, h.forgeIssues()[0].Number)
	})
	t.Run("beyond the allowance", func(t *testing.T) {
		t.Parallel()
		h, instance := skewed(t, 6*time.Minute)
		h.mustPass()
		h.advance(pastSettle)
		h.mustPass()
		h.assertTerminal(instance.ID, domain.FollowUpFilingAmbiguous, "", "follow_up_filing_ambiguous")
		if got := h.creates(); got != 1 {
			t.Fatalf("create requests = %d, want 1", got)
		}
	})
}

// TestFollowUpFilerNoticesAStalledFiling fails a filing's precondition reads
// without a definite answer. The filing keeps its intent and is retried; a
// notice appears once it has failed for the stall interval and resolves when
// the reads recover, a second stall raises a new notice, and the filing
// completes in the end.
func TestFollowUpFilerNoticesAStalledFiling(t *testing.T) {
	t.Parallel()
	h := newFilingHarness(t)
	run := h.seedRun("run-a", nil, nil)
	stalled, behind := h.approve(run, 0), h.approve(run, 1)
	working := h.tokens
	failing := errTokenSource{&publish.APIError{
		Status: http.StatusForbidden, RequestPath: "/app/installations/1/access_tokens",
	}}
	h.tokens = failing
	for range 3 {
		if err := h.pass(); err == nil {
			t.Fatal("pass with a failing token mint reported no error")
		}
		h.advance(4 * time.Minute)
	}
	if item := h.healthItem(stalled.ID, "follow_up_filing_stalled"); item != nil {
		t.Fatalf("stall notice before the interval: %+v", item)
	}
	h.advance(4 * time.Minute)
	if err := h.pass(); err == nil {
		t.Fatal("pass with a failing token mint reported no error")
	}
	item := h.healthItem(stalled.ID, "follow_up_filing_stalled")
	if item == nil || !strings.Contains(item.Reason, string(stalled.ID)) {
		t.Fatalf("stall notice = %+v", item)
	}
	if intent := h.intent(stalled.ID); intent == nil || intent.Terminal != nil || len(intent.Attempts) != 0 {
		t.Fatalf("stalled intent = %+v, want open with no attempt", intent)
	}
	if intent := h.intent(behind.ID); intent != nil || h.creates() != 0 {
		t.Fatalf("filing behind the stalled one opened %+v; create requests = %d", intent, h.creates())
	}

	// The reads recover and GitHub rate-limits the create: the filing is
	// moving again though not finished, so the notice resolves.
	h.tokens = working
	h.respond(fakeCreateResponse{Status: http.StatusTooManyRequests})
	h.mustPass()
	h.assertNotice(stalled.ID, "follow_up_filing_stalled", domain.StatusResolved)

	// A resolved item is final, so a second stall raises its own notice.
	h.tokens = failing
	for range 2 {
		h.advance(16 * time.Minute)
		if err := h.pass(); err == nil {
			t.Fatal("pass with a failing token mint reported no error")
		}
	}
	h.assertNotice(stalled.ID, "follow_up_filing_stalled", domain.StatusResolved)
	h.assertNotice(stalled.ID, "follow_up_filing_stalled/2", domain.StatusOpen)

	h.tokens = working
	h.mustPass()
	issues := h.forgeIssues()
	if len(issues) != 2 {
		t.Fatalf("forge holds %d issues after recovery, want 2", len(issues))
	}
	h.assertLedgered(stalled.ID, issues[0].Number)
	h.assertLedgered(behind.ID, issues[1].Number)
	h.assertNotice(stalled.ID, "follow_up_filing_stalled/2", domain.StatusResolved)
}

func TestFollowUpFilerPerRunCap(t *testing.T) {
	cases := map[string]struct {
		policy map[string]string
		want   int
	}{
		"default": {nil, 5},
		"set":     {map[string]string{"follow_up_filing.max_per_run": "1"}, 1},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newFilingHarness(t)
			run := h.seedRun("run-a", tc.policy, nil)
			var filings []domain.ProposalInstance
			for finding := range filingFindingsPerRun {
				filings = append(filings, h.approve(run, finding))
			}
			other := h.approve(h.seedRun("run-b", nil, nil), 0)
			h.mustPass()
			if got := h.creates(); got != tc.want+1 {
				t.Fatalf("create requests = %d, want %d", got, tc.want+1)
			}
			for _, over := range filings[tc.want:] {
				item := h.assertTerminal(over.ID, domain.FollowUpFilingRefused,
					domain.FollowUpFilingRefusalPreconditionFailed, "follow_up_filing_capped")
				if !strings.Contains(item.Reason, "follow_up_filing.max_per_run") {
					t.Fatalf("per-run refusal notice = %q", item.Reason)
				}
			}
			// The cap is per origin run: another run's filing is unaffected.
			if filed := h.filed(other.ID); filed == nil {
				t.Fatal("another run's filing was not filed")
			}
		})
	}
}

func TestFollowUpFilerDepthCap(t *testing.T) {
	// file runs one filing from a run bound to parent and returns the issue
	// number it filed, or 0 when it was refused.
	file := func(h *filingHarness, name string, policy map[string]string, parent *int) (domain.ProposalInstance, int) {
		instance := h.approve(h.seedRun(name, policy, parent), 0)
		h.mustPass()
		if filed := h.filed(instance.ID); filed != nil {
			return instance, filed.IssueNumber
		}
		return instance, 0
	}
	assertRefused := func(t *testing.T, h *filingHarness, instance domain.ProposalInstance) {
		t.Helper()
		item := h.assertTerminal(instance.ID, domain.FollowUpFilingRefused,
			domain.FollowUpFilingRefusalPreconditionFailed, "follow_up_filing_capped")
		if !strings.Contains(item.Reason, "follow_up_filing.max_depth") {
			t.Fatalf("depth refusal notice = %q", item.Reason)
		}
	}
	t.Run("default", func(t *testing.T) {
		h := newFilingHarness(t)
		_, first := file(h, "run-a", nil, nil)
		_, second := file(h, "run-b", nil, &first)
		if first == 0 || second == 0 {
			t.Fatalf("filed issues %d and %d, want depths 1 and 2 to file", first, second)
		}
		third, number := file(h, "run-c", nil, &second)
		if number != 0 {
			t.Fatalf("a depth-3 filing filed issue %d under the default cap of 2", number)
		}
		assertRefused(t, h, third)
		if got := h.creates(); got != 2 {
			t.Fatalf("create requests = %d, want 2", got)
		}
		// An issue Freeside did not file starts a fresh ancestry.
		human := 77
		if _, number := file(h, "run-d", nil, &human); number == 0 {
			t.Fatal("a filing under a human-filed issue was refused")
		}
	})
	t.Run("set", func(t *testing.T) {
		h := newFilingHarness(t)
		_, first := file(h, "run-a", nil, nil)
		second, number := file(h, "run-b", map[string]string{"follow_up_filing.max_depth": "1"}, &first)
		if number != 0 {
			t.Fatalf("a depth-2 filing filed issue %d under a cap of 1", number)
		}
		assertRefused(t, h, second)
		raised := map[string]string{"follow_up_filing.max_depth": "3"}
		_, deeper := file(h, "run-c", raised, &first)
		if _, number := file(h, "run-d", raised, &deeper); number == 0 {
			t.Fatal("a depth-3 filing was refused under a cap of 3")
		}
	})
}

func TestFollowUpFilerRefusesAMalformedCap(t *testing.T) {
	for _, key := range []string{
		"follow_up_filing.settle_interval", "follow_up_filing.max_per_day",
		"follow_up_filing.max_depth", "follow_up_filing.max_per_run",
	} {
		for _, value := range []string{"many", "0", "-1"} {
			t.Run(key+"="+value, func(t *testing.T) {
				t.Parallel()
				h := newFilingHarness(t)
				instance := h.approve(h.seedRun("run-a", map[string]string{key: value}, nil), 0)
				h.mustPass()
				item := h.assertTerminal(instance.ID, domain.FollowUpFilingRefused,
					domain.FollowUpFilingRefusalPreconditionFailed, "follow_up_filing_refused")
				if !strings.Contains(item.Reason, key) {
					t.Fatalf("malformed-cap notice %q does not name %s", item.Reason, key)
				}
				if got := h.creates(); got != 0 {
					t.Fatalf("create requests = %d, want 0", got)
				}
			})
		}
	}
}

func TestFollowUpFilerRefusesWithoutTheIssuesPermission(t *testing.T) {
	denials := map[string]error{
		"grant mismatch": fmt.Errorf("mint: %w", publish.ErrGrantMismatch),
		"mint rejected": &publish.APIError{
			Status: http.StatusUnprocessableEntity, RequestPath: "/app/installations/1/access_tokens",
		},
	}
	for name, denial := range denials {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newFilingHarness(t)
			instance := h.approve(h.seedRun("run-a", nil, nil), 0)
			h.tokens = errTokenSource{denial}
			h.mustPass()
			item := h.assertTerminal(instance.ID, domain.FollowUpFilingRefused,
				domain.FollowUpFilingRefusalPreconditionFailed, "follow_up_filing_refused")
			if !strings.Contains(item.Reason, "issues: write") {
				t.Fatalf("permission notice %q does not name the permission", item.Reason)
			}
			if got := h.creates(); got != 0 {
				t.Fatalf("create requests = %d, want 0", got)
			}
		})
	}
}

func TestFollowUpFilerRefusesAnUnresolvableTarget(t *testing.T) {
	cases := map[string]func(h *filingHarness){
		"milestone gone":             func(h *filingHarness) { h.gh.filing.milestones = nil },
		"repository rebound by name": func(h *filingHarness) { h.gh.repositoryID = testRepoID + 1 },
	}
	for name, arrange := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newFilingHarness(t)
			instance := h.approve(h.seedRun("run-a", nil, nil), 0)
			arrange(h)
			h.mustPass()
			h.assertTerminal(instance.ID, domain.FollowUpFilingRefused,
				domain.FollowUpFilingRefusalPreconditionFailed, "follow_up_filing_refused")
			if got := h.creates(); got != 0 {
				t.Fatalf("create requests = %d, want 0", got)
			}
		})
	}
}

// TestFollowUpFilerRunSweepsAtStartOnTickAndOnWake pins Run's real-time
// behavior: a sweep when it starts, which is the recovery after a restart,
// one per tick, and one as soon as it is woken.
func TestFollowUpFilerRunSweepsAtStartOnTickAndOnWake(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newFilingHarness(t)
		run := h.seedRun("run-a", nil, nil)
		first := h.approve(run, 0)
		h.respond(unprovenCommitted)
		// The daemon restarts with the approval durable and nothing begun.
		h.restart()

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			done <- h.filer.Run(ctx, 30*time.Second, func(err error) { t.Errorf("pass: %v", err) })
		}()

		synctest.Wait()
		if got := h.creates(); got != 1 {
			t.Fatalf("create requests after the start sweep = %d, want 1", got)
		}

		h.advance(pastSettle)
		time.Sleep(29 * time.Second)
		synctest.Wait()
		if intent := h.intent(first.ID); intent == nil || intent.Terminal != nil {
			t.Fatalf("intent before the first tick = %+v, want outstanding", intent)
		}
		time.Sleep(2 * time.Second)
		synctest.Wait()
		h.assertLedgered(first.ID, h.forgeIssues()[0].Number)

		second := h.approve(run, 1)
		h.filer.Wake()
		synctest.Wait()
		h.assertLedgered(second.ID, h.forgeIssues()[1].Number)

		cancel()
		if err := <-done; err != nil {
			t.Fatalf("Run after cancel = %v, want nil", err)
		}
	})
}

func TestFollowUpFilerRunRejectsANonPositiveInterval(t *testing.T) {
	h := newFilingHarness(t)
	if err := h.filer.Run(context.Background(), 0, nil); err == nil {
		t.Fatal("Run accepted a zero sweep interval")
	}
}

func TestFollowUpFilerWakeOnANilFilerIsANoOp(t *testing.T) {
	var filer *publish.FollowUpFiler
	filer.Wake()
}
