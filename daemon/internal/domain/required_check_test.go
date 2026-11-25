package domain_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// failedCheck is #3591's first attempt as the card shows it: the macOS test
// job never got a runner.
func failedCheck(head string) *domain.RequiredCheck {
	class := domain.CheckFailureInfrastructure
	return &domain.RequiredCheck{
		State:        domain.RequiredCheckFailed,
		FailureClass: &class,
		HeadSHA:      head,
		FailedJobs:   []string{"daemon / macos (test, darwin 1/1)"},
		ObservedAt:   time.Date(2026, 11, 24, 17, 24, 1, 0, time.UTC),
	}
}

// readyAtHead is a valid ready item bound to a head, as every published one
// is.
func readyAtHead(t *testing.T) domain.AttentionItem {
	t.Helper()
	in := validItemInput(domain.AttentionReadyForFinalReview)
	in.PRHeadSHA = "head"
	return mustItem(t, in)
}

func TestRequiredCheckValidation(t *testing.T) {
	ready := readyAtHead(t)
	ready.RequiredCheck = failedCheck(ready.PRHeadSHA)
	if err := ready.Validate(); err != nil {
		t.Fatalf("ready item with a failed check Validate() = %v, want nil", err)
	}
	pending := ready
	pending.RequiredCheck = &domain.RequiredCheck{
		State: domain.RequiredCheckPending, HeadSHA: ready.PRHeadSHA, Reruns: 2,
		ObservedAt: time.Date(2026, 11, 24, 17, 24, 1, 0, time.UTC),
	}
	if err := pending.Validate(); err != nil {
		t.Fatalf("pending check Validate() = %v, want nil", err)
	}

	wrongType := mustItem(t, validItemInput(domain.AttentionSpecApproval))
	wrongType.RequiredCheck = failedCheck(wrongType.PRHeadSHA)
	if err := wrongType.Validate(); !errors.Is(err, domain.ErrCardFactOutsideItem) {
		t.Errorf("non-ready item Validate() = %v, want ErrCardFactOutsideItem", err)
	}

	code := domain.CheckFailureCode
	unknown := domain.CheckFailureClass("flaky")
	cases := []struct {
		name   string
		mutate func(*domain.RequiredCheck)
		want   error
	}{
		{"unknown state", func(c *domain.RequiredCheck) { c.State = "green" }, domain.ErrInvalidRequiredCheck},
		{"failed without class", func(c *domain.RequiredCheck) { c.FailureClass = nil }, domain.ErrInvalidRequiredCheck},
		{"passed with class", func(c *domain.RequiredCheck) {
			c.State = domain.RequiredCheckPassed
			c.FailedJobs = nil
			c.FailureClass = &code
		}, domain.ErrInvalidRequiredCheck},
		{"unknown class", func(c *domain.RequiredCheck) { c.FailureClass = &unknown }, domain.ErrInvalidRequiredCheck},
		{"failed without jobs", func(c *domain.RequiredCheck) { c.FailedJobs = nil }, domain.ErrInvalidRequiredCheck},
		{"pending with jobs", func(c *domain.RequiredCheck) {
			c.State = domain.RequiredCheckPending
			c.FailureClass = nil
		}, domain.ErrInvalidRequiredCheck},
		{"empty job list", func(c *domain.RequiredCheck) { c.FailedJobs = []string{} }, domain.ErrEmptyField},
		{"too many jobs", func(c *domain.RequiredCheck) {
			c.FailedJobs = make([]string, domain.MaxRequiredCheckFailedJobs+1)
			for i := range c.FailedJobs {
				c.FailedJobs[i] = "job"
			}
		}, domain.ErrInvalidRequiredCheck},
		{"empty job name", func(c *domain.RequiredCheck) { c.FailedJobs = []string{""} }, domain.ErrEmptyField},
		{"long job name", func(c *domain.RequiredCheck) {
			c.FailedJobs = []string{strings.Repeat("j", domain.MaxRequiredCheckJobNameBytes+1)}
		}, domain.ErrInvalidRequiredCheck},
		{"invalid UTF-8 job name", func(c *domain.RequiredCheck) { c.FailedJobs = []string{"job \xff"} }, domain.ErrInvalidRequiredCheck},
		{"credential in job name", func(c *domain.RequiredCheck) {
			c.FailedJobs = []string{"ghp_" + strings.Repeat("a", 36)}
		}, domain.ErrInvalidRequiredCheck},
		{"negative reruns", func(c *domain.RequiredCheck) { c.Reruns = -1 }, domain.ErrInvalidRequiredCheck},
		{"reruns over the bound", func(c *domain.RequiredCheck) { c.Reruns = domain.MaxRequiredCheckReruns + 1 }, domain.ErrInvalidRequiredCheck},
		{"zero observed_at", func(c *domain.RequiredCheck) { c.ObservedAt = time.Time{} }, domain.ErrMissingTimestamp},
		{"non-UTC observed_at", func(c *domain.RequiredCheck) {
			c.ObservedAt = c.ObservedAt.In(time.FixedZone("x", 3600))
		}, domain.ErrTimestampNotUTC},
		{"another head", func(c *domain.RequiredCheck) { c.HeadSHA = "other" }, domain.ErrCardFactInconsistent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			item := ready
			item.RequiredCheck = failedCheck(ready.PRHeadSHA)
			tc.mutate(item.RequiredCheck)
			if err := item.Validate(); !errors.Is(err, tc.want) {
				t.Fatalf("Validate() = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestRequiredCheckTransitions pins the live-reading rule: the check changes
// freely on an open item, never once it closed, and a spent rerun is never
// given back.
func TestRequiredCheckTransitions(t *testing.T) {
	none := readyAtHead(t)
	failed := none
	failed.ItemVersion++
	failed.RequiredCheck = failedCheck(none.PRHeadSHA)
	rerun := failed
	rerun.ItemVersion++
	rerun.RequiredCheck = &domain.RequiredCheck{
		State: domain.RequiredCheckPending, HeadSHA: none.PRHeadSHA, Reruns: 1,
		ObservedAt: failed.RequiredCheck.ObservedAt.Add(time.Minute),
	}
	for _, tc := range []struct {
		name     string
		old, upd domain.AttentionItem
	}{
		{"first reading", none, failed},
		{"rerun returns to pending", failed, rerun},
	} {
		if err := domain.ValidateAttentionItemTransition(tc.old, tc.upd); err != nil {
			t.Errorf("%s = %v, want accepted", tc.name, err)
		}
	}

	givenBack := rerun
	givenBack.ItemVersion++
	givenBack.RequiredCheck = failedCheck(none.PRHeadSHA)
	cleared := rerun
	cleared.ItemVersion++
	cleared.RequiredCheck = nil
	closed := failed
	closed.Status = domain.StatusSuperseded
	afterClose := closed
	afterClose.ItemVersion++
	afterClose.RequiredCheck = rerun.RequiredCheck
	for _, tc := range []struct {
		name     string
		old, upd domain.AttentionItem
	}{
		{"reruns decrease", rerun, givenBack},
		{"reading removed", rerun, cleared},
		{"change after close", closed, afterClose},
	} {
		if err := domain.ValidateAttentionItemTransition(tc.old, tc.upd); !errors.Is(err, domain.ErrImmutableTransition) {
			t.Errorf("%s = %v, want ErrImmutableTransition", tc.name, err)
		}
	}
}

func TestScreenRequiredCheckJobName(t *testing.T) {
	token := "ghp_" + strings.Repeat("a", 36)
	long := strings.Repeat("é", domain.MaxRequiredCheckJobNameBytes)
	for _, tc := range []struct {
		name, in, want string
	}{
		{"plain", "daemon / linux (test)", "daemon / linux (test)"},
		{"credential", token, domain.RedactedName(token)},
		{"empty", "", "[redacted]"},
		{"invalid UTF-8", "job \xff", "job �"},
		{"cut on a rune boundary", long, strings.Repeat("é", domain.MaxRequiredCheckJobNameBytes/2)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := domain.ScreenRequiredCheckJobName(tc.in); got != tc.want {
				t.Fatalf("ScreenRequiredCheckJobName() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRequiredCheckRegistrations proves every registered state and class
// is one Validate accepts, so a member added to a registration without its
// valid() case fails here.
func TestRequiredCheckRegistrations(t *testing.T) {
	for _, state := range domain.AllRequiredCheckStates {
		check := failedCheck("head")
		check.State = state
		if state != domain.RequiredCheckFailed {
			check.FailureClass, check.FailedJobs = nil, nil
		}
		if err := check.Validate(); err != nil {
			t.Errorf("state %q: Validate() = %v, want nil", state, err)
		}
	}
	for _, class := range domain.AllCheckFailureClasses {
		check := failedCheck("head")
		check.FailureClass = &class
		if err := check.Validate(); err != nil {
			t.Errorf("class %q: Validate() = %v, want nil", class, err)
		}
	}
}
