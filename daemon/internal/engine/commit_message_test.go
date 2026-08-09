package engine

import (
	"errors"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
)

func TestFallbackCommitMessage(t *testing.T) {
	t.Parallel()
	issue := 81
	input := FallbackCommitMessageInput{
		Spec:       []byte("\n# Deduplicate Production API Dependency Defaults. ###\n\nDetails.\n"),
		BoundIssue: &issue,
		RunID:      "run-482",
		SpecDigest: "sha256:" + domain.Digest(strings.Repeat("a", 64)),
		Policy:     importer.Policy{MessageRuleset: domain.MessageRulesetGitHub1},
	}
	got := FallbackCommitMessage(input)
	wantSubject := "Deduplicate Production API Dependency Defaults (#81)"
	subject, _, _ := strings.Cut(got, "\n")
	if subject != wantSubject {
		t.Fatalf("subject = %q, want %q", subject, wantSubject)
	}
	first, _ := utf8.DecodeRuneInString(subject)
	if !unicode.IsUpper(first) || len([]byte(subject)) > fallbackCommitSubjectMaxBytes ||
		strings.HasSuffix(subject, ".") || strings.Contains(strings.ToLower(subject), "freeside") {
		t.Errorf("subject failed quality contract: %q", subject)
	}
	for _, fact := range []string{"Work item: #81.", "Run ID: run-482.", "Specification digest:"} {
		if !strings.Contains(got, fact) {
			t.Errorf("message missing %q:\n%s", fact, got)
		}
	}
	if err := importer.ScreenMessage(got, input.Policy); err != nil {
		t.Fatalf("derived message did not pass importer screen: %v", err)
	}
	for _, line := range strings.Split(got, "\n") {
		if len([]byte(line)) > fallbackCommitSubjectMaxBytes {
			t.Errorf("line is %d bytes, want <= %d: %q", len([]byte(line)), fallbackCommitSubjectMaxBytes, line)
		}
	}
}

func TestValidateProductionReplayOptionsCommitMessageCompatibility(t *testing.T) {
	t.Parallel()
	runID := domain.RunID("run-message-replay")
	resolved, err := domain.NewResolvedPolicy(runID, []domain.PolicyKey{{
		Key: "paths", Value: "daemon/**",
		Provenance: domain.KeyProvenance{
			Source: domain.ProvenanceOverride, Digest: "sha256:policy-source",
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := domain.NewAutomationTrustProfile(domain.AutomationTrustProfileInput{
		Repo: "example/repo", RepositoryID: 7,
		PRExecution:                domain.PRExecutionAuditedSameRepo,
		CandidateAutomationChanges: domain.AutomationChangesBlocked,
		PRGitHubTokenPermissions:   domain.TokenPermissionsReadOnly,
		CommitPlan:                 domain.CommitPlanSingleCommit,
		MessageRuleset:             domain.MessageRulesetGitHub1,
		WorkflowAuditDigest:        "sha256:workflow-audit",
		Review: domain.ReviewSettings{
			Mode: domain.ReviewFreesideInvoked, ConfigDigest: "sha256:review",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	policy, err := (importer.Policy{Allowlist: []string{"daemon/**"}}).WithProtectedPaths(profile)
	if err != nil {
		t.Fatal(err)
	}
	spec := []byte("# Preserve Replay Messages\n")
	specDigest := domain.Digest("sha256:" + strings.Repeat("a", 64))
	publication := ProductionPublication{
		CommitAuthor: ProductionCommitAuthor{AppSlug: "freeside", BotUserID: 42},
	}
	baseOptions := importer.Options{
		BaseSHA: strings.Repeat("b", 40), CommitDate: time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC),
		AuthorName: publication.CommitAuthor.Name(), AuthorEmail: publication.CommitAuthor.Email(),
		Policy: policy,
	}
	binding := productionBinding{
		run: domain.Run{ID: runID, SpecDigest: specDigest}, spec: spec, specLoaded: true,
		admission: domain.ExecutionAdmission{
			SpecDigest: specDigest, PolicyDigest: resolved.Digest,
			Base: domain.BaseRevision{BaseSHA: baseOptions.BaseSHA},
		},
		resolvedPolicy: resolved, profile: profile,
	}
	current := baseOptions
	current.CommitMessage = FallbackCommitMessage(FallbackCommitMessageInput{
		Spec: spec, RunID: runID, SpecDigest: specDigest, Policy: policy,
	})
	for _, tc := range []struct {
		name    string
		options importer.Options
		wantErr bool
	}{
		{"legacy empty message", baseOptions, false},
		{"current derived message", current, false},
		{"tampered message", func() importer.Options {
			tampered := current
			tampered.CommitMessage = "Tampered message"
			return tampered
		}(), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			candidate := binding
			candidate.replay.ImportOptions = tc.options
			err := validateProductionReplayOptions(candidate, publication)
			if tc.wantErr && !errors.Is(err, domain.ErrParentKeyMismatch) {
				t.Fatalf("validation error = %v, want parent-key mismatch", err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("validation error = %v", err)
			}
		})
	}
}

func TestFallbackCommitMessageNoIssue(t *testing.T) {
	t.Parallel()
	got := FallbackCommitMessage(FallbackCommitMessageInput{
		Spec: []byte("# Preserve API Names\n"), RunID: "run-9",
		SpecDigest: "sha256:spec", Policy: importer.Policy{},
	})
	if subject, _, _ := strings.Cut(got, "\n"); subject != "Preserve API Names" {
		t.Fatalf("subject = %q", subject)
	}
	if strings.Contains(got, "Work item:") {
		t.Fatalf("undeclared message contains work item:\n%s", got)
	}
}

func TestFallbackCommitMessageSubjectBudgetIncludesIssueSuffix(t *testing.T) {
	t.Parallel()
	title := strings.Repeat("A", fallbackCommitSubjectMaxBytes)
	withoutIssue := FallbackCommitMessage(FallbackCommitMessageInput{
		Spec: []byte("# " + title), RunID: "run-9",
		SpecDigest: "sha256:spec", Policy: importer.Policy{},
	})
	if subject, _, _ := strings.Cut(withoutIssue, "\n"); subject != title {
		t.Fatalf("exact-budget subject = %q", subject)
	}
	issue := 1
	withIssue := FallbackCommitMessage(FallbackCommitMessageInput{
		Spec: []byte("# " + title), BoundIssue: &issue, RunID: "run-9",
		SpecDigest: "sha256:spec", Policy: importer.Policy{},
	})
	if subject, _, _ := strings.Cut(withIssue, "\n"); subject != "REWRITE ME: commit message missing for work item #1" {
		t.Fatalf("suffix-over-budget subject = %q", subject)
	}
}

func TestFallbackCommitMessageFloors(t *testing.T) {
	t.Parallel()
	issue := 81
	for _, tc := range []struct {
		name      string
		spec      string
		bound     *int
		wantFloor string
		wantWhy   string
	}{
		{"absent", "details only", &issue, "REWRITE ME: commit message missing for work item #81", "no leading ATX title"},
		{"over budget", "# " + strings.Repeat("A", 73), &issue, "REWRITE ME: commit message missing for work item #81", "exceeds the 72-byte limit"},
		{"screened", "# Unsafe\x00Title", nil, "REWRITE ME: commit message missing (run run-9)", "message screening failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := FallbackCommitMessage(FallbackCommitMessageInput{
				Spec: []byte(tc.spec), BoundIssue: tc.bound, RunID: "run-9",
				SpecDigest: "sha256:spec", Policy: importer.Policy{},
			})
			if subject, _, _ := strings.Cut(got, "\n"); subject != tc.wantFloor {
				t.Fatalf("subject = %q, want %q", subject, tc.wantFloor)
			}
			if !strings.Contains(strings.ReplaceAll(got, "\n", " "), tc.wantWhy) {
				t.Fatalf("message missing %q:\n%s", tc.wantWhy, got)
			}
			if err := importer.ScreenMessage(got, importer.Policy{}); err != nil {
				t.Fatalf("floor did not pass importer screen: %v", err)
			}
		})
	}
}

func TestFallbackCommitMessageFloorSanitizesAndBoundsRunTrace(t *testing.T) {
	t.Parallel()
	for _, runID := range []domain.RunID{
		"fixes #1",
		domain.RunID("run-" + strings.Repeat("a", 64)),
		domain.RunID(strings.Repeat("b", 150)),
	} {
		got := FallbackCommitMessage(FallbackCommitMessageInput{
			Spec: nil, RunID: runID, SpecDigest: "sha256:spec",
			Policy: importer.Policy{MessageRuleset: domain.MessageRulesetGitHub1},
		})
		if err := importer.ScreenMessage(got, importer.Policy{
			MessageRuleset: domain.MessageRulesetGitHub1,
		}); err != nil {
			t.Errorf("floor for %q failed screen: %v\n%s", runID, err, got)
		}
		for lineNumber, line := range strings.Split(got, "\n") {
			if len([]byte(line)) > fallbackCommitSubjectMaxBytes {
				t.Errorf("floor for %q line %d is %d bytes: %q",
					runID, lineNumber+1, len([]byte(line)), line)
			}
		}
	}
}
