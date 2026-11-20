package domain

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// testGitHubToken matches the github_token credential rule.
var testGitHubToken = "ghp_" + strings.Repeat("A", 36)

func TestScreenFailureEvidence(t *testing.T) {
	t.Parallel()
	errText := "open /Users/someone/handoff/out: permission denied"
	for name, tc := range map[string]struct {
		reason   string
		withheld []string
		check    func(t *testing.T, got string)
	}{
		"withholds the underlying error": {
			reason:   "export verification failed: " + errText,
			withheld: []string{errText},
			check: func(t *testing.T, got string) {
				if strings.Contains(got, "/Users/someone") || got != "export verification failed: "+redactedEvidence {
					t.Fatalf("evidence = %q, want the error withheld", got)
				}
			},
		},
		"redacts a credential": {
			reason: "export carried " + testGitHubToken + " in a file",
			check: func(t *testing.T, got string) {
				if strings.Contains(got, testGitHubToken) || !strings.Contains(got, redactedEvidence) {
					t.Fatalf("evidence = %q, want the token redacted", got)
				}
			},
		},
		"keeps head and tail under the cap": {
			reason: "start " + strings.Repeat("é", 400) + " the specific cause",
			check: func(t *testing.T, got string) {
				if len(got) > MaxHandoffEvidenceBytes || !utf8.ValidString(got) ||
					!strings.HasPrefix(got, "start ") || !strings.HasSuffix(got, "the specific cause") ||
					!strings.Contains(got, evidenceElision) {
					t.Fatalf("evidence of %d bytes = %q, want both ends under the cap", len(got), got)
				}
			},
		},
		"names something when nothing is left": {
			reason: errText, withheld: []string{errText},
			check: func(t *testing.T, got string) {
				if got != redactedEvidence {
					t.Fatalf("evidence = %q, want the bare token", got)
				}
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := ScreenFailureEvidence(tc.reason, tc.withheld...)
			tc.check(t, got)
			screened := HandoffCheckFailure{Check: WardCheckExportVerification, Evidence: got}
			if err := screened.Validate(); err != nil {
				t.Fatalf("screened evidence does not validate: %v", err)
			}
		})
	}
}

func TestHandoffCheckFailureValidateRefusesUnscreenedEvidence(t *testing.T) {
	t.Parallel()
	for name, check := range map[string]HandoffCheckFailure{
		"unknown check": {Check: "nope", Evidence: "x"},
		"empty":         {Check: WardCheckTeardown},
		"over the cap":  {Check: WardCheckTeardown, Evidence: strings.Repeat("x", MaxHandoffEvidenceBytes+1)},
		"credential":    {Check: WardCheckTeardown, Evidence: "saw " + testGitHubToken},
	} {
		if err := check.Validate(); !errors.Is(err, ErrOutcomeInconsistent) {
			t.Errorf("%s: Validate = %v, want ErrOutcomeInconsistent", name, err)
		}
	}
}

// TestOutcomeHandoffCheckPlacement: a failed outcome with cause
// handoff_check names its check, a lost outcome may, and no other outcome
// carries one.
func TestOutcomeHandoffCheckPlacement(t *testing.T) {
	t.Parallel()
	check := &HandoffCheckFailure{Check: WardCheckExportVerification, Evidence: "entry cap", ContentEvidence: true}
	for name, tc := range map[string]struct {
		status ExecutionOutcomeStatus
		cause  FailureCause
		check  *HandoffCheckFailure
		ok     bool
	}{
		"failed check names it":      {ExecutionOutcomeFailed, FailureCauseHandoffCheck, check, true},
		"failed check without one":   {ExecutionOutcomeFailed, FailureCauseHandoffCheck, nil, false},
		"lost with a check":          {ExecutionOutcomeLost, "", check, true},
		"lost without a check":       {ExecutionOutcomeLost, "", nil, true},
		"other cause with a check":   {ExecutionOutcomeFailed, FailureCauseUnknown, check, false},
		"canceled with a check":      {ExecutionOutcomeCanceled, "", check, false},
		"export rejected unchecked":  {ExecutionOutcomeFailed, FailureCauseExportRejected, nil, true},
		"export rejected with check": {ExecutionOutcomeFailed, FailureCauseExportRejected, check, false},
	} {
		err := validateOutcomeHandoffCheck(tc.status, tc.cause, tc.check)
		if (err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok %t", name, err, tc.ok)
		}
	}
}

func TestDecideRetryOutlook(t *testing.T) {
	t.Parallel()
	content := &HandoffCheckFailure{Check: WardCheckExportVerification, Evidence: "cap", ContentEvidence: true}
	transient := &HandoffCheckFailure{Check: WardCheckTeardown, Evidence: "timed out"}
	for name, tc := range map[string]struct {
		outcome ExecutionOutcomeStatus
		cause   FailureCause
		check   *HandoffCheckFailure
		want    RetryOutlook
	}{
		"F42 loss over the cap":    {ExecutionOutcomeLost, "", content, RetryOutlookWouldRepeat},
		"loss with no evidence":    {ExecutionOutcomeLost, "", nil, RetryOutlookMaySucceed},
		"loss on a teardown check": {ExecutionOutcomeLost, "", transient, RetryOutlookMaySucceed},
		"canceled":                 {ExecutionOutcomeCanceled, "", nil, RetryOutlookMaySucceed},
		"F60 export rejected":      {ExecutionOutcomeFailed, FailureCauseExportRejected, nil, RetryOutlookWouldRepeat},
		"invalid configuration":    {ExecutionOutcomeFailed, FailureCauseInvalidConfiguration, nil, RetryOutlookWouldRepeat},
		"provider unavailable":     {ExecutionOutcomeFailed, FailureCauseProviderUnavailable, nil, RetryOutlookMaySucceed},
		"unknown":                  {ExecutionOutcomeFailed, FailureCauseUnknown, nil, RetryOutlookMaySucceed},
	} {
		if got := DecideRetryOutlook(tc.outcome, tc.cause, tc.check); got != tc.want {
			t.Errorf("%s: outlook = %q, want %q", name, got, tc.want)
		}
	}
}

func TestOffersSameAgentRetry(t *testing.T) {
	t.Parallel()
	implementer, reviewer := RoleImplementer, RoleName("reviewer")
	facts := func(stage StageName, role *RoleName, outcome ExecutionOutcomeStatus, cause FailureCause) ExecutionFailureFacts {
		f := ExecutionFailureFacts{Outcome: outcome, Cause: cause, Stage: stage, InvocationID: "inv"}
		if role != nil {
			f.Admitted = &InvocationAdmissionFacts{Role: role}
		}
		f.RetryOutlook = DecideRetryOutlook(outcome, cause, nil)
		return f
	}
	for name, tc := range map[string]struct {
		facts ExecutionFailureFacts
		want  bool
	}{
		"lost specification":         {facts(StageNameSpecification, nil, ExecutionOutcomeLost, ""), true},
		"canceled implementer":       {facts(StageNameImplementation, &implementer, ExecutionOutcomeCanceled, ""), true},
		"provider unavailable":       {facts(StageNameImplementation, &implementer, ExecutionOutcomeFailed, FailureCauseProviderUnavailable), true},
		"unknown failure":            {facts(StageNameImplementation, &implementer, ExecutionOutcomeFailed, FailureCauseUnknown), true},
		"quota has its own remedy":   {facts(StageNameImplementation, &implementer, ExecutionOutcomeFailed, FailureCauseQuota), false},
		"would repeat":               {facts(StageNameImplementation, &implementer, ExecutionOutcomeFailed, FailureCauseExportRejected), false},
		"remediation round":          {facts(StageNameImplementation, &reviewer, ExecutionOutcomeLost, ""), false},
		"implementation without one": {facts(StageNameImplementation, nil, ExecutionOutcomeLost, ""), false},
		"review stage":               {facts(StageNameReview, nil, ExecutionOutcomeLost, ""), false},
		"older card without outlook": {ExecutionFailureFacts{Outcome: ExecutionOutcomeLost, Stage: StageNameSpecification, InvocationID: "inv"}, false},
	} {
		if got := tc.facts.OffersSameAgentRetry(); got != tc.want {
			t.Errorf("%s: offers = %t, want %t", name, got, tc.want)
		}
	}
}

// TestExportRejectionFactsBoundAndScreen is F60's projection: at most five
// findings plus the true total, and a credential-shaped path only as its
// redaction token.
func TestExportRejectionFactsBoundAndScreen(t *testing.T) {
	t.Parallel()
	secretPath := "notes/" + testGitHubToken + ".txt"
	findings := []ExportRejectionFinding{{Kind: "secret", Path: secretPath}, {Kind: "symlink", PathHex: "6869"}}
	for range 6 {
		findings = append(findings, ExportRejectionFinding{Kind: "oversized", Path: "big.bin"})
	}
	facts := NewExportRejectionFacts(ExportRejection{
		InvocationID: "inv", AdmissionID: "adm", Findings: findings, TotalFindings: 12, RecordedAt: time.Unix(0, 0),
	})
	if err := facts.Validate(); err != nil {
		t.Fatalf("projection does not validate: %v", err)
	}
	if len(facts.Findings) != MaxExportRejectionFacts || facts.Total != 12 {
		t.Fatalf("facts = %+v, want five findings of twelve", facts)
	}
	if got := facts.Findings[0].Path; got != RedactedName(secretPath) || strings.Contains(got, testGitHubToken) {
		t.Fatalf("credential path shows as %q, want its token", got)
	}
	if facts.Findings[1].Path != "" {
		t.Fatalf("a path_hex finding shows path %q, want none", facts.Findings[1].Path)
	}
	unscreened := facts
	unscreened.Findings = []ExportRejectionFindingFacts{{Kind: "secret", Path: secretPath}}
	if err := unscreened.Validate(); !errors.Is(err, ErrCardFactInconsistent) {
		t.Fatalf("unscreened path Validate = %v, want ErrCardFactInconsistent", err)
	}
}

// TestFailureRecordsWithoutNewFieldsKeepTheirBytes: an outcome or card fact
// written before #3440 re-encodes without any of the fields #3440 added, so
// its stored body is unchanged and a replayed comparison against it still
// converges.
func TestFailureRecordsWithoutNewFieldsKeepTheirBytes(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		stored string
		into   any
	}{
		"outcome": {
			stored: `{"invocation_id":"inv","admission_id":"adm","status":"lost","recorded_at":"2026-10-01T09:00:00Z"}`,
			into:   &ExecutionOutcome{},
		},
		"card facts": {
			stored: `{"outcome":"failed","cause":"unknown","stage":"implementation","invocation_id":"inv"}`,
			into:   &ExecutionFailureFacts{},
		},
	} {
		if err := json.Unmarshal([]byte(tc.stored), tc.into); err != nil {
			t.Fatalf("%s: decode: %v", name, err)
		}
		again, err := json.Marshal(tc.into)
		if err != nil {
			t.Fatalf("%s: encode: %v", name, err)
		}
		var got map[string]any
		if err := json.Unmarshal(again, &got); err != nil {
			t.Fatalf("%s: re-decode: %v", name, err)
		}
		for _, key := range []string{"handoff_check", "export_rejection", "retry_outlook"} {
			if _, present := got[key]; present {
				t.Errorf("%s: re-encoding added %q: %s", name, key, again)
			}
		}
	}
}
