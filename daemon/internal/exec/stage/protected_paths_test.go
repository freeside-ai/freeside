package stage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// protectingTrustProfile widens each repository-specific class by one pattern,
// so the import policy covers every control-plane class.
func protectingTrustProfile(t *testing.T) domain.AutomationTrustProfile {
	t.Helper()
	profile, err := domain.NewAutomationTrustProfile(domain.AutomationTrustProfileInput{
		Repo:                       testBase.Repo,
		RepositoryID:               testBase.RepositoryID,
		PRExecution:                domain.PRExecutionAuditedSameRepo,
		CandidateAutomationChanges: domain.AutomationChangesBlocked,
		PRGitHubTokenPermissions:   domain.TokenPermissionsReadOnly,
		CommitPlan:                 domain.CommitPlanSingleCommit,
		MessageRuleset:             domain.MessageRulesetGitHub1,
		WorkflowAuditDigest:        "sha256:workflow-audit",
		Review: domain.ReviewSettings{
			Mode: domain.ReviewFreesideInvoked, ConfigDigest: "sha256:review-config",
		},
		ProtectedPaths: domain.ProtectedPathConfig{
			ExtraVerificationControlPatterns: []string{".freeside/recipe.yaml"},
			ExtraPromptsAndPolicyPatterns:    []string{"prompts/**"},
			ExtraEgressAndTrustPatterns:      []string{"config/egress-allowlist.json"},
			ExtraMaterialityRulesPatterns:    []string{"policy/**"},
		},
	})
	if err != nil {
		t.Fatalf("NewAutomationTrustProfile: %v", err)
	}
	return profile
}

// TestProtectedPathEditFailsRunAtImport drives a released candidate through
// the real importer under publish_strict. An edit in any non-advisory class,
// the built-in automation-control and git-metadata defaults included, fails
// the run with a failed outcome and records no export, so nothing reaches
// verification, review, or publication. A reviewer-instruction edit is
// tolerated and the candidate completes.
func TestProtectedPathEditFailsRunAtImport(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		path string
		kind importer.FindingKind
	}{
		{".github/workflows/ci.yml", importer.FindingAutomationControlPath},
		{".gitattributes", importer.FindingGitMetadataPath},
		{".freeside/recipe.yaml", importer.FindingVerificationRecipePath},
		{"prompts/system.md", importer.FindingPromptsPolicyPath},
		{"config/egress-allowlist.json", importer.FindingEgressTrustPath},
		{"policy/materiality.yaml", importer.FindingMaterialityRulesPath},
		{"AGENTS.md", importer.FindingReviewerInstructionPath},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			workspace := t.TempDir()
			full := filepath.Join(workspace, filepath.FromSlash(tc.path))
			if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, []byte("edited\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			outDir, err := os.MkdirTemp("", "freeside-handoff-"+testRunIDFor(testInvoke)+"-out-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(outDir) })
			manifest, err := export.Export(os.DirFS(workspace), outDir, export.Options{})
			if err != nil {
				t.Fatal(err)
			}
			repo, base := emptyBaseRepo(t)
			records := newStubExports()
			d := newTestDriver(t, &stubGate{}, records)
			d.seeder = recoveryGitSeeder{repo: repo}
			d.imports.Policy, err = d.imports.Policy.WithProtectedPaths(protectingTrustProfile(t))
			if err != nil {
				t.Fatal(err)
			}
			if d.imports.Policy.FindingProfile != nil {
				t.Fatal("fixture must import under the default publish_strict profile")
			}
			spec := testStartSpec()
			spec.Base.BaseSHA = base
			in := orphanWithSpec(t, d, phaseExported,
				&releasedExport{Dir: outDir, Manifest: manifest, ObservedBaseSHA: base}, spec)
			reconcileClean(t, ctx, d)
			result, err := d.Collect(ctx, in.InvocationID)
			if err != nil {
				t.Fatal(err)
			}
			if tc.kind == importer.FindingReviewerInstructionPath {
				if result.Status != exec.StatusCompleted || result.HeadSHA == "" || len(records.records) != 1 {
					t.Fatalf("reviewer-instruction edit = %+v, want a completed candidate", result)
				}
				return
			}
			if result.Status != exec.StatusFailed || len(records.records) != 0 {
				t.Fatalf("%s edit = %+v with %d exports, want a failed run and no export",
					tc.path, result, len(records.records))
			}
			outcome, ok := records.outcomes[in.InvocationID]
			if !ok || outcome.Status != domain.ExecutionOutcomeFailed ||
				!strings.Contains(outcome.Summary, "1 publish-blocking findings") {
				t.Fatalf("outcome = %+v (present=%t), want failed with one publish-blocking finding", outcome, ok)
			}
			rejection, found, err := records.LookupExportRejection(ctx, in.InvocationID)
			if err != nil || !found || len(rejection.Findings) != 1 ||
				rejection.Findings[0].Kind != string(tc.kind) || rejection.Findings[0].Path != tc.path {
				t.Fatalf("rejection = %+v (found=%t, err=%v), want one %s finding on %s",
					rejection, found, err, tc.kind, tc.path)
			}
		})
	}
}

// TestStartRecordsProtectedPathsAndReconstructs: a new start renders the
// authority's list into the prompt and records it in the intent, which then
// reconstructs; an intent recorded before the list existed carries no field
// and reconstructs its original prompt.
func TestStartRecordsProtectedPathsAndReconstructs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	t.Run("new intent", func(t *testing.T) {
		t.Parallel()
		entered := make(chan struct{})
		gate := &stubGate{handoffCtxFn: func(ctx context.Context, _ ward.HandoffSpec) (*ward.HandoffResult, error) {
			close(entered)
			<-ctx.Done()
			return nil, ctx.Err()
		}}
		d := newTestDriver(t, gate, newStubExports())
		protected := []string{"**/.gitattributes", ".github/workflows/**"}
		d.authority = stubAuthority{protected: protected}
		spec := testStartSpec()
		inputs := stageInputs(t, &spec)
		if err := d.StartWithInputs(ctx, testInvoke, spec,
			func(context.Context) (exec.StageInputs, error) { return inputs, nil },
		); err != nil {
			t.Fatalf("StartWithInputs: %v", err)
		}
		select {
		case <-entered:
		case <-time.After(10 * time.Second):
			t.Fatal("handoff did not start")
		}
		t.Cleanup(func() { _ = d.Close(context.Background()) })
		raw, err := os.ReadFile(d.intentPath(testRunIDFor(testInvoke)))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), `"protected_paths":["**/.gitattributes",".github/workflows/**"]`) {
			t.Fatalf("intent does not record the list:\n%s", raw)
		}
		in, err := d.loadIntent(ctx, testInvoke)
		if err != nil {
			t.Fatalf("reconstruct: %v", err)
		}
		if !slices.Equal(in.Inputs.ProtectedPaths, protected) ||
			!strings.HasSuffix(in.Prompt, "--- Protected paths ---\n\n**/.gitattributes\n.github/workflows/**\n") {
			t.Fatalf("reconstructed list %q, prompt:\n%s", in.Inputs.ProtectedPaths, in.Prompt)
		}
	})
	t.Run("intent recorded before the list", func(t *testing.T) {
		t.Parallel()
		d := newTestDriver(t, &stubGate{}, newStubExports())
		recorded := orphan(t, d, phaseSeeding, nil)
		raw, err := os.ReadFile(d.intentPath(recorded.RunID))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "protected_paths") {
			t.Fatalf("legacy fixture carries the field:\n%s", raw)
		}
		in, err := d.loadIntent(ctx, testInvoke)
		if err != nil {
			t.Fatalf("reconstruct: %v", err)
		}
		if in.Prompt != recorded.Prompt || strings.Contains(in.Prompt, "Protected paths") {
			t.Fatalf("legacy prompt changed:\n%s", in.Prompt)
		}
	})
	t.Run("refused read commits no intent", func(t *testing.T) {
		t.Parallel()
		d := newTestDriver(t, &stubGate{}, newStubExports())
		refused := errors.New("trust profile unavailable")
		d.authority = stubAuthority{protectedErr: refused}
		spec := testStartSpec()
		inputs := stageInputs(t, &spec)
		err := d.StartWithInputs(ctx, testInvoke, spec,
			func(context.Context) (exec.StageInputs, error) { return inputs, nil })
		if !errors.Is(err, refused) {
			t.Fatalf("StartWithInputs = %v, want the authority's refusal", err)
		}
		if _, err := os.Stat(d.intentPath(testRunIDFor(testInvoke))); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("refused start left an intent: %v", err)
		}
	})
}
