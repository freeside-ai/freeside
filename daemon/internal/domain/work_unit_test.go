package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func intPtr(n int) *int { return &n }

// validCaptureFixture builds one internally consistent set of capture
// records: a declared unit bound to issue 443, published as PR 450 onto
// main, with the merge and issue-closure observations that exactly satisfy
// the issue-closure criterion. Each test mutates one coordinate.
func validCaptureFixture() (domain.WorkUnitDeclaration, domain.WorkUnitPRBinding, domain.PullMergeFact, domain.IssueStateFact) {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	decl := domain.WorkUnitDeclaration{
		ID: domain.WorkUnitIDForRun("run-1"), RunID: "run-1", ProjectID: "project-1",
		CompletionCriterion: domain.CompletionBoundIssueClosedByMergedPR,
		BoundIssue:          intPtr(443),
		DependsOnIssues:     []int{440, 442},
		DeclaredPaths:       []string{"daemon/", "devlog/"},
		ContractSerialized:  true,
		DeclaredAt:          ts,
	}
	binding := domain.WorkUnitPRBinding{
		UnitID: decl.ID, Repo: "owner/repo", RepositoryID: 84958515,
		PRNumber: 450, BaseRef: "main", HeadSHA: "cafebabe",
		RecordedAt: ts.Add(time.Hour),
	}
	pull := domain.PullMergeFact{
		Repo: "owner/repo", RepositoryID: 84958515, PRNumber: 450,
		State: domain.PullRequestClosed, Merged: true,
		MergeCommitSHA: "deadbeef", BaseRef: "main", HeadSHA: "cafebabe",
		ObservedAt: ts.Add(2 * time.Hour),
	}
	issue := domain.IssueStateFact{
		Repo: "owner/repo", RepositoryID: 84958515, IssueNumber: 443,
		State: domain.IssueClosed, ClosedByCommitSHA: "deadbeef",
		ObservedAt: ts.Add(2 * time.Hour),
	}
	return decl, binding, pull, issue
}

func TestWorkUnitDeclarationValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*domain.WorkUnitDeclaration)
		wantErr error
	}{
		{"underived id", func(d *domain.WorkUnitDeclaration) { d.ID = "workunit-other" }, domain.ErrWorkUnitInconsistent},
		{"no run", func(d *domain.WorkUnitDeclaration) { d.RunID = "" }, domain.ErrEmptyID},
		{"no project", func(d *domain.WorkUnitDeclaration) { d.ProjectID = "" }, domain.ErrEmptyID},
		{"zero criterion", func(d *domain.WorkUnitDeclaration) { d.CompletionCriterion = "" }, domain.ErrInvalidCompletionCriterion},
		{"issue criterion without bound issue", func(d *domain.WorkUnitDeclaration) { d.BoundIssue = nil }, domain.ErrWorkUnitInconsistent},
		{"non-positive bound issue", func(d *domain.WorkUnitDeclaration) { d.BoundIssue = intPtr(0) }, domain.ErrNonPositive},
		{"non-positive dependency", func(d *domain.WorkUnitDeclaration) { d.DependsOnIssues = []int{-1, 440} }, domain.ErrNonPositive},
		{"unsorted dependencies", func(d *domain.WorkUnitDeclaration) { d.DependsOnIssues = []int{442, 440} }, domain.ErrDependenciesNotCanonical},
		{"duplicate dependencies", func(d *domain.WorkUnitDeclaration) { d.DependsOnIssues = []int{440, 440} }, domain.ErrDependenciesNotCanonical},
		{"empty declared path", func(d *domain.WorkUnitDeclaration) { d.DeclaredPaths = []string{""} }, domain.ErrEmptyField},
		{"unsorted declared paths", func(d *domain.WorkUnitDeclaration) { d.DeclaredPaths = []string{"devlog/", "daemon/"} }, domain.ErrDeclaredPathsNotCanonical},
		{"duplicate declared paths", func(d *domain.WorkUnitDeclaration) { d.DeclaredPaths = []string{"daemon/", "daemon/"} }, domain.ErrDeclaredPathsNotCanonical},
		{"zero declared_at", func(d *domain.WorkUnitDeclaration) { d.DeclaredAt = time.Time{} }, domain.ErrMissingTimestamp},
		{"non-UTC declared_at", func(d *domain.WorkUnitDeclaration) {
			d.DeclaredAt = d.DeclaredAt.In(time.FixedZone("PST", -8*3600))
		}, domain.ErrTimestampNotUTC},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			decl, _, _, _ := validCaptureFixture()
			tc.mutate(&decl)
			if err := decl.Validate(); !errors.Is(err, tc.wantErr) {
				t.Fatalf("Validate() = %v, want %v", err, tc.wantErr)
			}
		})
	}
	t.Run("pr-merged criterion allows an absent bound issue", func(t *testing.T) {
		decl, _, _, _ := validCaptureFixture()
		decl.CompletionCriterion = domain.CompletionBoundPRMerged
		decl.BoundIssue = nil
		if err := decl.Validate(); err != nil {
			t.Fatalf("Validate() = %v, want nil", err)
		}
	})
}

func TestNewWorkUnitDeclarationStampsUTC(t *testing.T) {
	local := time.Date(2026, 1, 2, 3, 4, 5, 0, time.FixedZone("PST", -8*3600))
	decl, err := domain.NewWorkUnitDeclaration(domain.WorkUnitDeclarationInput{
		CompletionCriterion: domain.CompletionBoundPRMerged,
	}, "run-1", "project-1", local)
	if err != nil {
		t.Fatal(err)
	}
	if decl.DeclaredAt.Location() != time.UTC {
		t.Fatalf("DeclaredAt location = %v, want UTC", decl.DeclaredAt.Location())
	}
	if decl.ID != domain.WorkUnitIDForRun("run-1") {
		t.Fatalf("ID = %q, want derived from run", decl.ID)
	}
}

func TestWorkUnitPRBindingValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*domain.WorkUnitPRBinding)
		wantErr error
	}{
		{"no unit", func(b *domain.WorkUnitPRBinding) { b.UnitID = "" }, domain.ErrEmptyID},
		{"no repo", func(b *domain.WorkUnitPRBinding) { b.Repo = "" }, domain.ErrEmptyField},
		{"non-positive repository id", func(b *domain.WorkUnitPRBinding) { b.RepositoryID = 0 }, domain.ErrNonPositive},
		{"non-positive pr number", func(b *domain.WorkUnitPRBinding) { b.PRNumber = 0 }, domain.ErrNonPositive},
		{"no base ref", func(b *domain.WorkUnitPRBinding) { b.BaseRef = "" }, domain.ErrEmptyField},
		{"no head sha", func(b *domain.WorkUnitPRBinding) { b.HeadSHA = "" }, domain.ErrEmptyField},
		{"zero recorded_at", func(b *domain.WorkUnitPRBinding) { b.RecordedAt = time.Time{} }, domain.ErrMissingTimestamp},
		{"non-UTC recorded_at", func(b *domain.WorkUnitPRBinding) {
			b.RecordedAt = b.RecordedAt.In(time.FixedZone("PST", -8*3600))
		}, domain.ErrTimestampNotUTC},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, binding, _, _ := validCaptureFixture()
			tc.mutate(&binding)
			if err := binding.Validate(); !errors.Is(err, tc.wantErr) {
				t.Fatalf("Validate() = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestReadyItemPRBindingValidate(t *testing.T) {
	valid := domain.ReadyItemPRBinding{
		ItemID: "item-ready-1", RunID: "run-1", ProducingInvocationID: "inv-1",
		PublicationInvocationID: "publish-production-run-1",
		PublicationIdentity:     "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Repo:                    "owner/repo",
		RepositoryID:            84958515, PRNumber: 450, BaseRef: "main",
		HeadSHA: "cafebabe", RecordedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
	cases := []struct {
		name    string
		mutate  func(*domain.ReadyItemPRBinding)
		wantErr error
	}{
		{"no item", func(b *domain.ReadyItemPRBinding) { b.ItemID = "" }, domain.ErrEmptyID},
		{"no run", func(b *domain.ReadyItemPRBinding) { b.RunID = "" }, domain.ErrEmptyID},
		{"no producing invocation", func(b *domain.ReadyItemPRBinding) { b.ProducingInvocationID = "" }, domain.ErrEmptyID},
		{"no publication invocation", func(b *domain.ReadyItemPRBinding) { b.PublicationInvocationID = "" }, domain.ErrEmptyID},
		{"invalid publication identity", func(b *domain.ReadyItemPRBinding) { b.PublicationIdentity = "bad" }, domain.ErrEmptyField},
		{"no repo", func(b *domain.ReadyItemPRBinding) { b.Repo = "" }, domain.ErrEmptyField},
		{"non-positive repository id", func(b *domain.ReadyItemPRBinding) { b.RepositoryID = 0 }, domain.ErrNonPositive},
		{"non-positive pr number", func(b *domain.ReadyItemPRBinding) { b.PRNumber = 0 }, domain.ErrNonPositive},
		{"no base ref", func(b *domain.ReadyItemPRBinding) { b.BaseRef = "" }, domain.ErrEmptyField},
		{"no head sha", func(b *domain.ReadyItemPRBinding) { b.HeadSHA = "" }, domain.ErrEmptyField},
		{"zero recorded_at", func(b *domain.ReadyItemPRBinding) { b.RecordedAt = time.Time{} }, domain.ErrMissingTimestamp},
		{"non-UTC recorded_at", func(b *domain.ReadyItemPRBinding) {
			b.RecordedAt = b.RecordedAt.In(time.FixedZone("PST", -8*3600))
		}, domain.ErrTimestampNotUTC},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			binding := valid
			tc.mutate(&binding)
			if err := binding.Validate(); !errors.Is(err, tc.wantErr) {
				t.Fatalf("Validate() = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestPullMergeFactValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*domain.PullMergeFact)
		wantErr error
	}{
		{"no repo", func(f *domain.PullMergeFact) { f.Repo = "" }, domain.ErrEmptyField},
		{"non-positive repository id", func(f *domain.PullMergeFact) { f.RepositoryID = 0 }, domain.ErrNonPositive},
		{"non-positive pr number", func(f *domain.PullMergeFact) { f.PRNumber = 0 }, domain.ErrNonPositive},
		{"zero state", func(f *domain.PullMergeFact) { f.State = "" }, domain.ErrInvalidPullRequestState},
		{"merged without commit", func(f *domain.PullMergeFact) { f.MergeCommitSHA = "" }, domain.ErrMergeFactInconsistent},
		{"commit without merged", func(f *domain.PullMergeFact) { f.Merged = false }, domain.ErrMergeFactInconsistent},
		{"merged while open", func(f *domain.PullMergeFact) { f.State = domain.PullRequestOpen }, domain.ErrMergeFactInconsistent},
		{"no base ref", func(f *domain.PullMergeFact) { f.BaseRef = "" }, domain.ErrEmptyField},
		{"no head sha", func(f *domain.PullMergeFact) { f.HeadSHA = "" }, domain.ErrEmptyField},
		{"zero observed_at", func(f *domain.PullMergeFact) { f.ObservedAt = time.Time{} }, domain.ErrMissingTimestamp},
		{"non-UTC observed_at", func(f *domain.PullMergeFact) {
			f.ObservedAt = f.ObservedAt.In(time.FixedZone("PST", -8*3600))
		}, domain.ErrTimestampNotUTC},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, pull, _ := validCaptureFixture()
			tc.mutate(&pull)
			if err := pull.Validate(); !errors.Is(err, tc.wantErr) {
				t.Fatalf("Validate() = %v, want %v", err, tc.wantErr)
			}
		})
	}
	t.Run("open unmerged is valid", func(t *testing.T) {
		_, _, pull, _ := validCaptureFixture()
		pull.State = domain.PullRequestOpen
		pull.Merged = false
		pull.MergeCommitSHA = ""
		if err := pull.Validate(); err != nil {
			t.Fatalf("Validate() = %v, want nil", err)
		}
	})
}

func TestIssueStateFactValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*domain.IssueStateFact)
		wantErr error
	}{
		{"no repo", func(f *domain.IssueStateFact) { f.Repo = "" }, domain.ErrEmptyField},
		{"non-positive repository id", func(f *domain.IssueStateFact) { f.RepositoryID = 0 }, domain.ErrNonPositive},
		{"non-positive issue number", func(f *domain.IssueStateFact) { f.IssueNumber = 0 }, domain.ErrNonPositive},
		{"zero state", func(f *domain.IssueStateFact) { f.State = "" }, domain.ErrInvalidIssueState},
		{"closing commit on open issue", func(f *domain.IssueStateFact) { f.State = domain.IssueOpen }, domain.ErrIssueFactInconsistent},
		{"zero observed_at", func(f *domain.IssueStateFact) { f.ObservedAt = time.Time{} }, domain.ErrMissingTimestamp},
		{"non-UTC observed_at", func(f *domain.IssueStateFact) {
			f.ObservedAt = f.ObservedAt.In(time.FixedZone("PST", -8*3600))
		}, domain.ErrTimestampNotUTC},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, issue := validCaptureFixture()
			tc.mutate(&issue)
			if err := issue.Validate(); !errors.Is(err, tc.wantErr) {
				t.Fatalf("Validate() = %v, want %v", err, tc.wantErr)
			}
		})
	}
	t.Run("closed without commit attribution is valid", func(t *testing.T) {
		_, _, _, issue := validCaptureFixture()
		issue.ClosedByCommitSHA = ""
		if err := issue.Validate(); err != nil {
			t.Fatalf("Validate() = %v, want nil", err)
		}
	})
}

func TestWorkUnitCompletionValidate(t *testing.T) {
	decl, binding, pull, issue := validCaptureFixture()
	completion, ok := domain.EvaluateWorkUnitCompletion(decl, binding, pull, &issue)
	if !ok {
		t.Fatal("fixture did not evaluate as completed")
	}
	cases := []struct {
		name    string
		mutate  func(*domain.WorkUnitCompletion)
		wantErr error
	}{
		{"no unit", func(c *domain.WorkUnitCompletion) { c.UnitID = "" }, domain.ErrEmptyID},
		{"zero criterion", func(c *domain.WorkUnitCompletion) { c.Criterion = "" }, domain.ErrInvalidCompletionCriterion},
		{"non-positive pr number", func(c *domain.WorkUnitCompletion) { c.PRNumber = 0 }, domain.ErrNonPositive},
		{"no merge commit", func(c *domain.WorkUnitCompletion) { c.MergeCommitSHA = "" }, domain.ErrEmptyField},
		{"issue criterion without bound issue", func(c *domain.WorkUnitCompletion) { c.BoundIssue = nil }, domain.ErrCompletionInconsistent},
		{"pr-merged criterion smuggling an issue", func(c *domain.WorkUnitCompletion) {
			c.Criterion = domain.CompletionBoundPRMerged
		}, domain.ErrCompletionInconsistent},
		{"non-positive bound issue", func(c *domain.WorkUnitCompletion) { c.BoundIssue = intPtr(0) }, domain.ErrNonPositive},
		{"zero recorded_at", func(c *domain.WorkUnitCompletion) { c.RecordedAt = time.Time{} }, domain.ErrMissingTimestamp},
		{"non-UTC recorded_at", func(c *domain.WorkUnitCompletion) {
			c.RecordedAt = c.RecordedAt.In(time.FixedZone("PST", -8*3600))
		}, domain.ErrTimestampNotUTC},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mutated := completion
			tc.mutate(&mutated)
			if err := mutated.Validate(); !errors.Is(err, tc.wantErr) {
				t.Fatalf("Validate() = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// TestEvaluateWorkUnitCompletion is the issue #443 acceptance enumeration:
// the bound-issue-closed-by-merged-PR completion case completes, and every
// partial, stacked, and related variation does not (§5.18: partial, stacked,
// or related merges do not complete units).
func TestEvaluateWorkUnitCompletion(t *testing.T) {
	t.Run("bound issue closed by merged PR completes", func(t *testing.T) {
		decl, binding, pull, issue := validCaptureFixture()
		completion, ok := domain.EvaluateWorkUnitCompletion(decl, binding, pull, &issue)
		if !ok {
			t.Fatal("EvaluateWorkUnitCompletion = false, want completed")
		}
		if completion.UnitID != decl.ID || completion.PRNumber != 450 ||
			completion.MergeCommitSHA != "deadbeef" ||
			completion.BoundIssue == nil || *completion.BoundIssue != 443 {
			t.Fatalf("completion coordinates do not restate the satisfying observation: %+v", completion)
		}
		if err := completion.Validate(); err != nil {
			t.Fatalf("completion invalid: %v", err)
		}
		if !completion.RecordedAt.Equal(issue.ObservedAt) && !completion.RecordedAt.Equal(pull.ObservedAt) {
			t.Fatalf("RecordedAt %v is neither satisfying observation", completion.RecordedAt)
		}
	})
	t.Run("bound PR merged criterion completes without issue state", func(t *testing.T) {
		decl, binding, pull, _ := validCaptureFixture()
		decl.CompletionCriterion = domain.CompletionBoundPRMerged
		decl.BoundIssue = nil
		completion, ok := domain.EvaluateWorkUnitCompletion(decl, binding, pull, nil)
		if !ok {
			t.Fatal("EvaluateWorkUnitCompletion = false, want completed")
		}
		if completion.BoundIssue != nil {
			t.Fatalf("pr-merged completion carries a bound issue: %+v", completion)
		}
	})

	notCompleted := []struct {
		name   string
		mutate func(*domain.WorkUnitDeclaration, *domain.WorkUnitPRBinding, *domain.PullMergeFact, *domain.IssueStateFact) *domain.IssueStateFact
	}{
		{"partial: PR merged but bound issue still open", func(_ *domain.WorkUnitDeclaration, _ *domain.WorkUnitPRBinding, _ *domain.PullMergeFact, issue *domain.IssueStateFact) *domain.IssueStateFact {
			issue.State = domain.IssueOpen
			issue.ClosedByCommitSHA = ""
			return issue
		}},
		{"partial: issue closed manually, not by the merge", func(_ *domain.WorkUnitDeclaration, _ *domain.WorkUnitPRBinding, _ *domain.PullMergeFact, issue *domain.IssueStateFact) *domain.IssueStateFact {
			issue.ClosedByCommitSHA = ""
			return issue
		}},
		{"partial: issue closed by a different commit", func(_ *domain.WorkUnitDeclaration, _ *domain.WorkUnitPRBinding, _ *domain.PullMergeFact, issue *domain.IssueStateFact) *domain.IssueStateFact {
			issue.ClosedByCommitSHA = "0ther5ha"
			return issue
		}},
		{"partial: issue state never observed", func(*domain.WorkUnitDeclaration, *domain.WorkUnitPRBinding, *domain.PullMergeFact, *domain.IssueStateFact) *domain.IssueStateFact {
			return nil
		}},
		{"stacked: merged into a base other than the admitted one", func(_ *domain.WorkUnitDeclaration, _ *domain.WorkUnitPRBinding, pull *domain.PullMergeFact, issue *domain.IssueStateFact) *domain.IssueStateFact {
			pull.BaseRef = "feat/base-feature"
			return issue
		}},
		{"unadmitted: merged after the head moved past the recorded binding", func(_ *domain.WorkUnitDeclaration, _ *domain.WorkUnitPRBinding, pull *domain.PullMergeFact, issue *domain.IssueStateFact) *domain.IssueStateFact {
			pull.HeadSHA = "0ther5ha"
			return issue
		}},
		{"related: a different PR merged", func(_ *domain.WorkUnitDeclaration, _ *domain.WorkUnitPRBinding, pull *domain.PullMergeFact, issue *domain.IssueStateFact) *domain.IssueStateFact {
			pull.PRNumber = 451
			return issue
		}},
		{"related: same PR number in a different repository", func(_ *domain.WorkUnitDeclaration, _ *domain.WorkUnitPRBinding, pull *domain.PullMergeFact, issue *domain.IssueStateFact) *domain.IssueStateFact {
			pull.RepositoryID = 999
			return issue
		}},
		{"related: issue closed in a different repository", func(_ *domain.WorkUnitDeclaration, _ *domain.WorkUnitPRBinding, _ *domain.PullMergeFact, issue *domain.IssueStateFact) *domain.IssueStateFact {
			issue.RepositoryID = 999
			return issue
		}},
		{"related: a different issue closed by the merge", func(_ *domain.WorkUnitDeclaration, _ *domain.WorkUnitPRBinding, _ *domain.PullMergeFact, issue *domain.IssueStateFact) *domain.IssueStateFact {
			issue.IssueNumber = 999
			return issue
		}},
		{"not merged: PR closed without merging", func(_ *domain.WorkUnitDeclaration, _ *domain.WorkUnitPRBinding, pull *domain.PullMergeFact, issue *domain.IssueStateFact) *domain.IssueStateFact {
			pull.Merged = false
			pull.MergeCommitSHA = ""
			return issue
		}},
		{"not merged: PR still open", func(_ *domain.WorkUnitDeclaration, _ *domain.WorkUnitPRBinding, pull *domain.PullMergeFact, issue *domain.IssueStateFact) *domain.IssueStateFact {
			pull.State = domain.PullRequestOpen
			pull.Merged = false
			pull.MergeCommitSHA = ""
			return issue
		}},
		{"foreign binding: record belongs to another unit", func(decl *domain.WorkUnitDeclaration, binding *domain.WorkUnitPRBinding, _ *domain.PullMergeFact, issue *domain.IssueStateFact) *domain.IssueStateFact {
			binding.UnitID = domain.WorkUnitIDForRun("run-2")
			return issue
		}},
	}
	for _, tc := range notCompleted {
		t.Run(tc.name, func(t *testing.T) {
			decl, binding, pull, issue := validCaptureFixture()
			issueArg := tc.mutate(&decl, &binding, &pull, &issue)
			if _, ok := domain.EvaluateWorkUnitCompletion(decl, binding, pull, issueArg); ok {
				t.Fatal("EvaluateWorkUnitCompletion = completed, want not completed")
			}
		})
	}
}

func TestCaptureFactMaterialChange(t *testing.T) {
	_, _, pull, issue := validCaptureFixture()
	laterPull := pull
	laterPull.ObservedAt = pull.ObservedAt.Add(15 * time.Minute)
	if laterPull.MaterialChangeFrom(pull) {
		t.Fatal("an instant-only difference must not be material")
	}
	laterPull.State = domain.PullRequestOpen
	laterPull.Merged = false
	laterPull.MergeCommitSHA = ""
	if !laterPull.MaterialChangeFrom(pull) {
		t.Fatal("a state change must be material")
	}
	laterIssue := issue
	laterIssue.ObservedAt = issue.ObservedAt.Add(15 * time.Minute)
	if laterIssue.MaterialChangeFrom(issue) {
		t.Fatal("an instant-only difference must not be material")
	}
	laterIssue.ClosedByCommitSHA = "0ther5ha"
	if !laterIssue.MaterialChangeFrom(issue) {
		t.Fatal("a closing-commit change must be material")
	}
}
