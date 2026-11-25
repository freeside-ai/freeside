package domain

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// RequiredCheckState is the combined state of the checks a published pull
// request's base branch requires, read on the ready item's bound head
// (issue #3598).
type RequiredCheckState string

const (
	// RequiredCheckPending: some required check has no completed job on the
	// head yet, or GitHub Actions produces none Freeside can read.
	RequiredCheckPending RequiredCheckState = "pending"
	// RequiredCheckPassed: every required check concluded success, skipped,
	// or neutral.
	RequiredCheckPassed RequiredCheckState = "passed"
	// RequiredCheckFailed: at least one required check concluded otherwise.
	RequiredCheckFailed RequiredCheckState = "failed"
)

// AllRequiredCheckStates is the single registration point for required
// check states.
var AllRequiredCheckStates = []RequiredCheckState{
	RequiredCheckPending,
	RequiredCheckPassed,
	RequiredCheckFailed,
}

func (s RequiredCheckState) valid() bool {
	switch s {
	case RequiredCheckPending, RequiredCheckPassed, RequiredCheckFailed:
		return true
	default:
		return false
	}
}

// CheckFailureClass is what the job facts GitHub reports say about a failed
// required check. It decides Freeside's reaction: an infrastructure failure
// is rerun, a code failure is reworked, and any other failure waits on a
// person.
type CheckFailureClass string

const (
	// CheckFailureInfrastructure: at least one failed job never got a runner
	// (no runner and no steps).
	CheckFailureInfrastructure CheckFailureClass = "infrastructure"
	// CheckFailureCode: no failed job is infrastructure, and at least one
	// concluded failure with a failed step.
	CheckFailureCode CheckFailureClass = "code"
	// CheckFailureOther: anything else, such as a job cancelled part-way or
	// timed out.
	CheckFailureOther CheckFailureClass = "other"
)

// AllCheckFailureClasses is the single registration point for check failure
// classes.
var AllCheckFailureClasses = []CheckFailureClass{
	CheckFailureInfrastructure,
	CheckFailureCode,
	CheckFailureOther,
}

func (c CheckFailureClass) valid() bool {
	switch c {
	case CheckFailureInfrastructure, CheckFailureCode, CheckFailureOther:
		return true
	default:
		return false
	}
}

const (
	// MaxRequiredCheckFailedJobs bounds the failed job names one card shows.
	MaxRequiredCheckFailedJobs = 10
	// MaxRequiredCheckJobNameBytes bounds one failed job name. Job names come
	// from the repository's workflow files, so they are untrusted.
	MaxRequiredCheckJobNameBytes = 200
	// MaxRequiredCheckReruns is how many times Freeside reruns an
	// infrastructure failure on one head before it leaves the card failed.
	MaxRequiredCheckReruns = 2
)

// RequiredCheck is the daemon's latest reading of a ready_for_final_review
// item's required check (issue #3598). The reconciler writes it while the
// item is open and its pull request is still on the bound head and base; it
// is never client-supplied (AttentionItemInput carries no path to it).
// ValidateAttentionItemTransition lets it change only on an open item and
// keeps Reruns from decreasing.
type RequiredCheck struct {
	State RequiredCheckState `json:"state"`
	// FailureClass is set exactly when State is failed.
	FailureClass *CheckFailureClass `json:"failure_class"`
	// HeadSHA is the head the jobs were read on: always the item's
	// PRHeadSHA, because a moved head is never read.
	HeadSHA string `json:"head_sha"`
	// FailedJobs are the screened names of the failed jobs, earliest first.
	// Non-empty exactly when State is failed, nil otherwise (nil and [] would
	// be two encodings of "none").
	FailedJobs []string `json:"failed_jobs"`
	// Reruns is how many times Freeside has rerun an infrastructure failure
	// on HeadSHA.
	Reruns int `json:"reruns"`
	// ObservedAt is the UTC instant of the reading that set State.
	ObservedAt time.Time `json:"observed_at"`
}

// Validate reports whether the reading is structurally sound on its own.
// AttentionItem.Validate checks it against the item.
func (c RequiredCheck) Validate() error {
	if !c.State.valid() {
		return fmt.Errorf("required check state %q: %w", c.State, ErrInvalidRequiredCheck)
	}
	failed := c.State == RequiredCheckFailed
	if failed != (c.FailureClass != nil) {
		return fmt.Errorf("required check state %q with failure class set=%t: %w",
			c.State, c.FailureClass != nil, ErrInvalidRequiredCheck)
	}
	if c.FailureClass != nil && !c.FailureClass.valid() {
		return fmt.Errorf("required check failure class %q: %w", *c.FailureClass, ErrInvalidRequiredCheck)
	}
	if failed != (c.FailedJobs != nil) {
		return fmt.Errorf("required check state %q with failed jobs set=%t: %w",
			c.State, c.FailedJobs != nil, ErrInvalidRequiredCheck)
	}
	if c.FailedJobs != nil && len(c.FailedJobs) == 0 {
		return fmt.Errorf("required check failed_jobs: %w", ErrEmptyField)
	}
	if len(c.FailedJobs) > MaxRequiredCheckFailedJobs {
		return fmt.Errorf("required check names %d failed jobs, over %d: %w",
			len(c.FailedJobs), MaxRequiredCheckFailedJobs, ErrInvalidRequiredCheck)
	}
	for _, name := range c.FailedJobs {
		if err := validateRequiredCheckJobName(name); err != nil {
			return err
		}
	}
	if c.HeadSHA == "" {
		return fmt.Errorf("required check head_sha: %w", ErrEmptyField)
	}
	if c.Reruns < 0 || c.Reruns > MaxRequiredCheckReruns {
		return fmt.Errorf("required check reruns %d outside 0..%d: %w",
			c.Reruns, MaxRequiredCheckReruns, ErrInvalidRequiredCheck)
	}
	return requireUTC(c.ObservedAt, "required check observed_at")
}

// validateRequiredCheckJobName holds a job name to what a card may show: a
// bounded, valid UTF-8, non-empty string that no credential rule matches
// (ScreenRequiredCheckJobName produces one).
func validateRequiredCheckJobName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("required check failed job name: %w", ErrEmptyField)
	case len(name) > MaxRequiredCheckJobNameBytes:
		return fmt.Errorf("required check failed job name of %d bytes, over %d: %w",
			len(name), MaxRequiredCheckJobNameBytes, ErrInvalidRequiredCheck)
	case !utf8.ValidString(name):
		return fmt.Errorf("required check failed job name is not valid UTF-8: %w", ErrInvalidRequiredCheck)
	case matchesCredentialRule(name):
		return fmt.Errorf("required check failed job name matches a credential rule: %w", ErrInvalidRequiredCheck)
	}
	return nil
}

// ScreenRequiredCheckJobName makes a job name GitHub reported safe to show:
// valid UTF-8, its RedactedName token when a credential rule matches it, and
// cut to MaxRequiredCheckJobNameBytes on a rune boundary. An empty name reads
// as the redaction token, so the card always names something.
func ScreenRequiredCheckJobName(name string) string {
	s := ScreenedName(strings.ToValidUTF8(name, "�"))
	if len(s) > MaxRequiredCheckJobNameBytes {
		cut := MaxRequiredCheckJobNameBytes
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut]
	}
	// Screen again after the cut: a rule anchored on a word boundary can
	// match a prefix the whole name didn't.
	if s == "" || matchesCredentialRule(s) {
		return redactedEvidence
	}
	return s
}

// validateRequiredCheck admits the reading only on a ready_for_final_review
// item, on that item's bound head.
func (i AttentionItem) validateRequiredCheck() error {
	if i.RequiredCheck == nil {
		return nil
	}
	if i.Type != AttentionReadyForFinalReview {
		return fmt.Errorf("item %s type %q carries a required check: %w", i.ID, i.Type, ErrCardFactOutsideItem)
	}
	if err := i.RequiredCheck.Validate(); err != nil {
		return fmt.Errorf("item %s: %w", i.ID, err)
	}
	if i.RequiredCheck.HeadSHA != i.PRHeadSHA {
		return fmt.Errorf("item %s required check head %s is not the item's head %s: %w",
			i.ID, i.RequiredCheck.HeadSHA, i.PRHeadSHA, ErrCardFactInconsistent)
	}
	return nil
}
