package engine

import (
	"errors"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
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
	spec := []byte("# Preserve Replay Messages\n\nA run imported before an upgrade must still publish.\n")
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
	derivation := FallbackCommitMessageInput{
		Spec: spec, RunID: runID, SpecDigest: specDigest, Policy: policy,
	}
	current := baseOptions
	current.CommitMessage = FallbackCommitMessage(derivation)
	v1 := baseOptions
	v1.CommitMessage = fallbackCommitMessageV1(derivation)
	if v1.CommitMessage == current.CommitMessage {
		t.Fatalf("fixture spec yields no reason, so v1 equals v2:\n%s", v1.CommitMessage)
	}
	for _, tc := range []struct {
		name    string
		options importer.Options
		wantErr bool
	}{
		{"legacy empty message", baseOptions, false},
		{"current derived message", current, false},
		{"v1 derived message", v1, false},
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

func fallbackCommitMessageGoldenInput() FallbackCommitMessageInput {
	issue := 214
	return FallbackCommitMessageInput{
		Spec: []byte("# Retry Stalled Webhook Deliveries\n\n" +
			"Webhook deliveries that time out are never retried, so a short provider\n" +
			"outage silently drops events. Retrying with backoff recovers them without\n" +
			"operator action. The retry budget is configurable.\n\n" +
			"## Acceptance\n\n- A timed-out delivery is retried.\n"),
		BoundIssue: &issue,
		RunID:      "run-webhook-retry",
		SpecDigest: domain.Digest("sha256:" + strings.Repeat("c", 64)),
		Policy:     importer.Policy{MessageRuleset: domain.MessageRulesetGitHub1},
	}
}

// TestFallbackCommitMessageGoldens pins both recipes for one specification
// with a reason paragraph. The v1 golden was captured from the pre-v2 code
// and must never change: publication replay accepts runs imported under it.
func TestFallbackCommitMessageGoldens(t *testing.T) {
	t.Parallel()
	in := fallbackCommitMessageGoldenInput()
	golden.Assert(t, "fallback_commit_message_v1", []byte(fallbackCommitMessageV1(in)))
	golden.Assert(t, "fallback_commit_message_v2", []byte(FallbackCommitMessage(in)))
	for _, message := range []string{fallbackCommitMessageV1(in), FallbackCommitMessage(in)} {
		if !FallbackCommitMessageMatches(in, message) {
			t.Errorf("own derivation not matched:\n%s", message)
		}
	}
	if FallbackCommitMessageMatches(in, FallbackCommitMessage(in)+"\n") {
		t.Error("altered message matched")
	}
}

func TestFallbackSpecificationReason(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("word ", 70)
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"plain paragraph", "Deliveries are dropped on timeout.", "Deliveries are dropped on timeout."},
		{"joined lines", "Deliveries are\ndropped on timeout.\n\nMore.", "Deliveries are dropped on timeout."},
		{"crlf", "Deliveries drop.\r\nOn timeout.\r\n", "Deliveries drop. On timeout."},
		{"two-sentence cut", "First one. Second one! Third one?", "First one. Second one!"},
		{"lowercase after period continues", "Use e.g. the retry path. Then stop.", "Use e.g. the retry path. Then stop."},
		{"over-length pair keeps first", "First one. Then " + long + "end.", "First one."},
		{"over-length first sentence", long + "end. Short.", ""},
		{"stops at a list", "Reason here.\n- item", "Reason here."},
		{"stops at a heading", "Reason here.\n## Next", "Reason here."},
		{"indented continuation", "Reason\n    continues here.", "Reason continues here."},
		{"title only", "", ""},
		{"bullet first", "- item", ""},
		{"ordered first", "1. item", ""},
		{"paren ordered first", "2) item", ""},
		{"heading first", "## Why", ""},
		{"fence first", "```go\ncode\n```", ""},
		{"tilde fence first", "~~~\ncode\n~~~", ""},
		{"quote first", "> quoted", ""},
		{"table first", "| a | b |", ""},
		{"html first", "<div>x</div>", ""},
		{"indented code first", "    code()", ""},
		{"tab code first", "\tcode()", ""},
		{"thematic break first", "***", ""},
		{"setext heading", "Not prose\n---", ""},
		{"issue reference", "Breaks #12 again.", ""},
		{"close directive", "Fixes #3 for good.", ""},
		{"cross-repo reference", "Mirrors owner/repo#4 now.", ""},
		{"gh reference", "Mirrors GH-5 now.", ""},
		{"mention", "Asked by @someone today.", ""},
		{"url", "See https://example.com for more.", ""},
		{"www host", "See www.example.com for more.", ""},
		{"www after underscore", "See _www.example.com for more.", ""},
		// A word longer than a line would be hard-split by wrapping, and
		// the split could start a link token the screen never saw.
		{"split www", "Q" + strings.Repeat("q", 71) + "www.example.com now.", ""},
		{"split gh reference", "Q" + strings.Repeat("q", 71) + "GH-12 now.", ""},
		{"split hex token", "Q" + strings.Repeat("q", 71) + "deadbeef12 now.", ""},
		{"line-length word kept", strings.Repeat("Q", 72) + " stays.", strings.Repeat("Q", 72) + " stays."},
		{"hex token", "Reverts deadbeef1 today.", ""},
		{"escaped reference", "Breaks &#35;12 again.", ""},
		{"secret", "Token ghp_" + strings.Repeat("x", 36) + " leaked.", ""},
		{"publisher marker", "Writes freeside:marker text.", ""},
		{"control character", "Bell \a rings.", ""},
		{"trailer", "Signed-off-by: someone.", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			spec := "# Title\n\n" + tc.body
			if got := fallbackSpecificationReason([]byte(spec)); got != tc.want {
				t.Fatalf("reason = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestFallbackCommitMessageDropsReasonFailingWholeMessage covers a reason
// that passes its own screen but fails in the assembled message: the
// result is the v1 message, never the rewrite floor.
func TestFallbackCommitMessageDropsReasonFailingWholeMessage(t *testing.T) {
	t.Parallel()
	// Wrapping puts the trailer word at a line start, where the github/1
	// trailer rule matches; on the reason's single line it does not.
	trailer := "Z" + strings.Repeat("z", 69) + " Signed-off-by: someone."
	base := FallbackCommitMessageInput{
		RunID: "run-9", SpecDigest: "sha256:spec",
		Policy: importer.Policy{MessageRuleset: domain.MessageRulesetGitHub1},
	}
	for _, tc := range []struct {
		name   string
		reason string
		policy func(v1 string) importer.Policy
	}{
		{"wrapped trailer", trailer, func(string) importer.Policy { return base.Policy }},
		{"byte cap", "A reason that fits alone.", func(v1 string) importer.Policy {
			return importer.Policy{
				MessageRuleset:        domain.MessageRulesetGitHub1,
				MaxCommitMessageBytes: len(v1) + 10,
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := base
			in.Spec = []byte("# Keep The Floor Away\n\n" + tc.reason + "\n")
			if fallbackSpecificationReason(in.Spec) != tc.reason {
				t.Fatalf("fixture reason was screened out on its own")
			}
			in.Policy = tc.policy(fallbackCommitMessageV1(in))
			v1 := fallbackCommitMessageV1(in)
			if strings.HasPrefix(v1, "REWRITE ME") {
				t.Fatalf("fixture v1 is the floor:\n%s", v1)
			}
			if got := FallbackCommitMessage(in); got != v1 {
				t.Fatalf("message = %q, want v1 %q", got, v1)
			}
		})
	}
}
