package importer

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/export"
)

// TestPlanProblemCodeTokens pins the complete PlanProblemCode list, its
// validity predicate, and that every registered code has a non-empty fixed
// description. A new code must be registered and pinned here deliberately.
func TestPlanProblemCodeTokens(t *testing.T) {
	t.Parallel()
	wantCodes := map[PlanProblemCode]string{ //nolint:gosec // G101: pinned PlanProblemCode names, not credentials
		PlanProblemPlanMissing:               "plan_missing",
		PlanProblemNoChanges:                 "no_changes",
		PlanProblemHandoffRefused:            "handoff_refused",
		PlanProblemInvalidUTF8:               "plan_invalid_utf8",
		PlanProblemInvalidJSON:               "plan_invalid_json",
		PlanProblemTrailingData:              "plan_trailing_data",
		PlanProblemVersionNotFirst:           "plan_version_not_first",
		PlanProblemVersionUnsupported:        "plan_version_unsupported",
		PlanProblemGroupsInvalid:             "plan_groups_invalid",
		PlanProblemGroupsEmpty:               "plan_groups_empty",
		PlanProblemGroupCapExceeded:          "plan_group_cap_exceeded",
		PlanProblemPathCapExceeded:           "plan_path_cap_exceeded",
		PlanProblemGroupInvalid:              "plan_group_invalid",
		PlanProblemGroupNameEmpty:            "plan_group_name_empty",
		PlanProblemGroupDiscriminator:        "plan_group_discriminator",
		PlanProblemGroupRemainderInvalid:     "plan_group_remainder_invalid",
		PlanProblemGroupPathsInvalid:         "plan_group_paths_invalid",
		PlanProblemPathInvalid:               "plan_path_invalid",
		PlanProblemPathReserved:              "plan_path_reserved",
		PlanProblemPathGitMetadata:           "plan_path_git_metadata",
		PlanProblemPathNotInChanges:          "plan_path_not_in_changes",
		PlanProblemPathDuplicated:            "plan_path_duplicated",
		PlanProblemPathUncovered:             "plan_path_uncovered",
		PlanProblemIntermediateTreeCollision: "plan_intermediate_tree_collision",
		PlanProblemIntermediateTreeFileDir:   "plan_intermediate_tree_file_directory",
		PlanProblemMessageOverCap:            "message_over_cap",
		PlanProblemMessageEmpty:              "message_empty",
		PlanProblemMessageControl:            "message_control",
		PlanProblemMessageCloseDirective:     "github_close_directive",
		PlanProblemMessageSkipDirective:      "github_skip_directive",
		PlanProblemMessageSpoofedTrailer:     "github_spoofed_trailer",
		PlanProblemNonRegularChange:          "non_regular_change",
		PlanProblemInvalidPathEntry:          "invalid_path_entry",
		PlanProblemBlobOmitted:               "blob_omitted",
		PlanProblemAutomationControlPath:     "automation_control_path",
		PlanProblemReviewerInstructionPath:   "reviewer_instruction_path",
		PlanProblemGitMetadataPath:           "git_metadata_path",
		PlanProblemVerificationRecipePath:    "verification_recipe_path",
		PlanProblemPromptsPolicyPath:         "prompts_policy_path",
		PlanProblemEgressTrustPath:           "egress_trust_path",
		PlanProblemMaterialityRulesPath:      "materiality_rules_path",
		PlanProblemAllowlistViolation:        "allowlist_violation",
		PlanProblemSizeViolation:             "size_violation",
		PlanProblemPathCollision:             "path_collision",
		PlanProblemSecret:                    "secret",
		PlanProblemSecretScanSkipped:         "secret_scan_skipped",
		PlanProblemCommitPlanSecret:          "commit_plan_secret",
	}
	if len(wantCodes) != len(AllPlanProblemCodes) {
		t.Fatalf("code table covers %d codes, registry has %d", len(wantCodes), len(AllPlanProblemCodes))
	}
	seen := make(map[PlanProblemCode]bool, len(AllPlanProblemCodes))
	for _, code := range AllPlanProblemCodes {
		if !code.valid() {
			t.Errorf("registered code %q is invalid", code)
		}
		if code.description() == "" {
			t.Errorf("registered code %q has empty description", code)
		}
		want, ok := wantCodes[code]
		if !ok {
			t.Errorf("registered code %q has no pinned token", code)
			continue
		}
		if string(code) != want {
			t.Errorf("code %v = %q, want %q", code, string(code), want)
		}
		seen[code] = true
	}
	if len(seen) != len(wantCodes) {
		t.Errorf("registry covers %d unique codes, code table has %d", len(seen), len(wantCodes))
	}
	if PlanProblemCode("").valid() || PlanProblemCode("bogus").valid() {
		t.Error("zero or bogus PlanProblemCode reported valid")
	}
	// Pin 1:1 coverage with commitMessageFindingKind and FindingKind.
	for _, mk := range allCommitMessageFindingKinds {
		if got := planProblemCodeForMessage(mk); !got.valid() || string(got) != string(mk) {
			t.Errorf("planProblemCodeForMessage(%q) = %q, want valid matching token", mk, got)
		}
	}
	for _, fk := range AllFindingKinds {
		if got := planProblemCodeForFinding(fk); !got.valid() || string(got) != string(fk) {
			t.Errorf("planProblemCodeForFinding(%q) = %q, want valid matching token", fk, got)
		}
	}
	if _, err := problemsForStructuralError(errPlanStructural); err == nil {
		t.Error("problemsForStructuralError accepted an uncoded structural error")
	}
}

// TestCheckCommitPlanParityWithImport runs each fixture through both Import
// (under plan_preferred) and CheckCommitPlan and asserts the same accept/reject
// decision plus the expected problem code, group number, path, and absence of
// secret/file content in the check output.
func TestCheckCommitPlanParityWithImport(t *testing.T) {
	t.Parallel()
	secretToken := "ghp_" + strings.Repeat("A", 36)
	escapedSecret := `ghp_\u0041` + strings.Repeat("A", 35)
	overCapMsg := strings.Repeat("x", DefaultMaxCommitMessageBytes+1)

	type wantProblem struct {
		code  PlanProblemCode
		group int
		path  string
	}
	cases := []struct {
		name           string
		baseFiles      map[string]string
		workspaceFiles map[string]string
		plan           []byte
		wantAccept     bool
		wantProblems   []wantProblem
		forbidOutput   []string
	}{
		{
			name:           "valid single-remainder plan",
			baseFiles:      map[string]string{"a.txt": "old\n"},
			workspaceFiles: map[string]string{"a.txt": "new\n"},
			plan:           []byte(`{"version":"freeside.commit-plan/v1","groups":[{"name":"all","message":"Update a","remainder":true}]}`),
			wantAccept:     true,
		},
		{
			name:           "valid multi-group plan",
			baseFiles:      map[string]string{"a.txt": "old\n", "del.txt": "bye\n", "keep.txt": "same\n"},
			workspaceFiles: map[string]string{"a.txt": "new\n", "b.txt": "added\n", "keep.txt": "same\n"},
			plan: []byte(`{"version":"freeside.commit-plan/v1","groups":[` +
				`{"name":"modify","message":"Update a","paths":["a.txt"]},` +
				`{"name":"add","message":"Add b","paths":["b.txt"]},` +
				`{"name":"remove","message":"Remove del","remainder":true}]}`),
			wantAccept: true,
		},
		{
			name:           "missing path (uncovered change)",
			baseFiles:      map[string]string{"a.txt": "old\n", "b.txt": "old\n"},
			workspaceFiles: map[string]string{"a.txt": "new\n", "b.txt": "new\n", "c.txt": "added\n"},
			plan:           []byte(`{"version":"freeside.commit-plan/v1","groups":[{"name":"only-a","message":"Update a","paths":["a.txt"]}]}`),
			wantAccept:     false,
			wantProblems: []wantProblem{
				{code: PlanProblemPathUncovered, path: "b.txt"},
				{code: PlanProblemPathUncovered, path: "c.txt"},
			},
		},
		{
			name:           "duplicated path across groups",
			baseFiles:      map[string]string{"a.txt": "old\n", "b.txt": "old\n"},
			workspaceFiles: map[string]string{"a.txt": "new\n", "b.txt": "new\n"},
			plan: []byte(`{"version":"freeside.commit-plan/v1","groups":[` +
				`{"name":"first","message":"First","paths":["a.txt"]},` +
				`{"name":"second","message":"Second","paths":["a.txt","b.txt"]}]}`),
			wantAccept: false,
			wantProblems: []wantProblem{
				{code: PlanProblemPathDuplicated, group: 2, path: "a.txt"},
			},
		},
		{
			name:           "path outside change set",
			baseFiles:      map[string]string{"a.txt": "old\n", "keep.txt": "same\n"},
			workspaceFiles: map[string]string{"a.txt": "new\n", "keep.txt": "same\n"},
			plan: []byte(`{"version":"freeside.commit-plan/v1","groups":[` +
				`{"name":"g1","message":"Touch keep","paths":["keep.txt"]},` +
				`{"name":"rest","message":"Rest","remainder":true}]}`),
			wantAccept: false,
			wantProblems: []wantProblem{
				{code: PlanProblemPathNotInChanges, group: 1, path: "keep.txt"},
			},
		},
		{
			name:           "file directory collision split across groups",
			baseFiles:      map[string]string{"node": "old-file\n"},
			workspaceFiles: map[string]string{"node/child": "new-child\n"},
			plan: []byte(`{"version":"freeside.commit-plan/v1","groups":[` +
				`{"name":"add-child","message":"Add child","paths":["node/child"]},` +
				`{"name":"drop-node","message":"Drop node","paths":["node"]}]}`),
			wantAccept: false,
			wantProblems: []wantProblem{
				{code: PlanProblemIntermediateTreeFileDir, group: 1, path: "node/child"},
			},
		},
		{
			name:           "forbidden closing phrase",
			baseFiles:      map[string]string{"a.txt": "old\n"},
			workspaceFiles: map[string]string{"a.txt": "new\n"},
			plan:           []byte(`{"version":"freeside.commit-plan/v1","groups":[{"name":"fix","message":"Fixes #42","remainder":true}]}`),
			wantAccept:     false,
			wantProblems: []wantProblem{
				{code: PlanProblemMessageCloseDirective, group: 1},
			},
		},
		{
			name:           "CI skip marker",
			baseFiles:      map[string]string{"a.txt": "old\n"},
			workspaceFiles: map[string]string{"a.txt": "new\n"},
			plan:           []byte(`{"version":"freeside.commit-plan/v1","groups":[{"name":"skip","message":"Update a [skip ci]","remainder":true}]}`),
			wantAccept:     false,
			wantProblems: []wantProblem{
				{code: PlanProblemMessageSkipDirective, group: 1},
			},
		},
		{
			name:           "spoofed trailer line",
			baseFiles:      map[string]string{"a.txt": "old\n"},
			workspaceFiles: map[string]string{"a.txt": "new\n"},
			plan:           []byte(`{"version":"freeside.commit-plan/v1","groups":[{"name":"trailer","message":"Update a\n\nCo-authored-by: Fake <fake@example.test>","remainder":true}]}`),
			wantAccept:     false,
			wantProblems: []wantProblem{
				{code: PlanProblemMessageSpoofedTrailer, group: 1},
			},
		},
		{
			name:           "control character in message",
			baseFiles:      map[string]string{"a.txt": "old\n"},
			workspaceFiles: map[string]string{"a.txt": "new\n"},
			plan:           []byte(`{"version":"freeside.commit-plan/v1","groups":[{"name":"ctrl","message":"Update a\u001b","remainder":true}]}`),
			wantAccept:     false,
			wantProblems: []wantProblem{
				{code: PlanProblemMessageControl, group: 1},
			},
		},
		{
			name:           "oversized message",
			baseFiles:      map[string]string{"a.txt": "old\n"},
			workspaceFiles: map[string]string{"a.txt": "new\n"},
			plan:           []byte(`{"version":"freeside.commit-plan/v1","groups":[{"name":"big","message":"` + overCapMsg + `","remainder":true}]}`),
			wantAccept:     false,
			wantProblems: []wantProblem{
				{code: PlanProblemMessageOverCap, group: 1},
			},
		},
		{
			name:           "secret-like string in plan",
			baseFiles:      map[string]string{"a.txt": "old\n"},
			workspaceFiles: map[string]string{"a.txt": "new\n"},
			plan:           []byte(`{"version":"freeside.commit-plan/v1","groups":[{"name":"sec","message":"Token ` + escapedSecret + `","remainder":true}]}`),
			wantAccept:     false,
			wantProblems: []wantProblem{
				{code: PlanProblemCommitPlanSecret, path: export.CommitPlanFilename},
			},
			forbidOutput: []string{secretToken, escapedSecret},
		},
		{
			name:           "secret-like string in changed file",
			baseFiles:      map[string]string{"a.txt": "old\n"},
			workspaceFiles: map[string]string{"a.txt": "TOKEN=" + secretToken + "\n"},
			plan:           []byte(`{"version":"freeside.commit-plan/v1","groups":[{"name":"all","message":"Update a","remainder":true}]}`),
			wantAccept:     false,
			wantProblems: []wantProblem{
				{code: PlanProblemSecret, path: "a.txt"},
			},
			forbidOutput: []string{secretToken},
		},
		{
			name:           "no plan file",
			baseFiles:      map[string]string{"a.txt": "old\n"},
			workspaceFiles: map[string]string{"a.txt": "new\n"},
			plan:           nil,
			wantAccept:     false,
			wantProblems: []wantProblem{
				{code: PlanProblemPlanMissing},
			},
		},
		{
			name:           "plan with no changes",
			baseFiles:      map[string]string{"a.txt": "same\n"},
			workspaceFiles: map[string]string{"a.txt": "same\n"},
			plan:           []byte(`{"version":"freeside.commit-plan/v1","groups":[{"name":"all","message":"Noop","remainder":true}]}`),
			wantAccept:     false,
			wantProblems: []wantProblem{
				{code: PlanProblemNoChanges},
			},
		},
		{
			name:           "no plan and no changes",
			baseFiles:      map[string]string{"a.txt": "same\n"},
			workspaceFiles: map[string]string{"a.txt": "same\n"},
			plan:           nil,
			wantAccept:     true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo, base := initBaseRepo(t, tc.baseFiles)
			workspace := writeWorkspaceFiles(t, tc.workspaceFiles)
			if tc.plan != nil {
				if err := os.WriteFile(filepath.Join(workspace, export.CommitPlanFilename), tc.plan, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			handoff := exportWorkspace(t, workspace)

			importClone := cloneAtBase(t, repo)
			importOpts := testImportOptions(base)
			importOpts.Policy.CommitPlan = domain.CommitPlanPlanPreferred
			importOpts.ImportRef = "refs/freeside/imports/parity-test"
			importRes, importErr := Import(t.Context(), handoff, importClone, importOpts)
			// Under publish-strict defaults, Import accepts a plan cleanly only
			// when it built a commit with no commit-plan fallback notice and no
			// publish-blocking (non-advisory) findings.
			importAccepted := importErr == nil &&
				importRes.CommitSHA != "" &&
				importRes.CommitPlanNotice == nil &&
				AllAdvisory(importRes.Findings, importOpts.Policy.CandidateAutomationChanges)

			checkClone := cloneAtBase(t, repo)
			checkOpts := testImportOptions(base)
			checkOpts.Policy.CommitPlan = domain.CommitPlanPlanPreferred
			checkRes, checkErr := CheckCommitPlan(t.Context(), handoff, checkClone, checkOpts)
			if checkErr != nil {
				t.Fatalf("CheckCommitPlan error: %v", checkErr)
			}
			if importAccepted != tc.wantAccept {
				t.Fatalf("Import accepted = %v, want %v (res=%+v, err=%v)", importAccepted, tc.wantAccept, importRes, importErr)
			}
			if checkRes.Honored != importAccepted {
				t.Fatalf("CheckCommitPlan honored = %v != Import accepted = %v (problems=%+v)", checkRes.Honored, importAccepted, checkRes.Problems)
			}
			if tc.wantAccept {
				if len(checkRes.Problems) != 0 {
					t.Fatalf("accepted check returned problems: %+v", checkRes.Problems)
				}
				return
			}
			if len(checkRes.Problems) != len(tc.wantProblems) {
				t.Fatalf("problems = %+v, want %+v", checkRes.Problems, tc.wantProblems)
			}
			for i, want := range tc.wantProblems {
				got := checkRes.Problems[i]
				if got.Code != want.code || got.Group != want.group || got.Path != want.path {
					t.Errorf("problem[%d] = %+v, want code=%q group=%d path=%q", i, got, want.code, want.group, want.path)
				}
				if got.Message == "" {
					t.Errorf("problem[%d] has empty message", i)
				}
			}
			raw, err := json.Marshal(checkRes)
			if err != nil {
				t.Fatalf("marshal check result: %v", err)
			}
			for _, forbidden := range tc.forbidOutput {
				if strings.Contains(string(raw), forbidden) {
					t.Errorf("CheckCommitPlan output leaked forbidden content")
				}
			}
		})
	}
}

// TestCheckCommitPlanUncoveredPathCap verifies that an exact-cover failure
// reports every uncovered path up to MaxReportedUncoveredPaths plus one
// omission note when more paths remain uncovered.
func TestCheckCommitPlanUncoveredPathCap(t *testing.T) {
	t.Parallel()
	total := MaxReportedUncoveredPaths + 5
	ws := make(map[string]string, total+1)
	ws["a.txt"] = "covered\n"
	for i := range total {
		ws[fmt.Sprintf("u%03d.txt", i)] = "uncovered\n"
	}
	repo, base := initBaseRepo(t, map[string]string{})
	workspace := writeWorkspaceFiles(t, ws)
	plan := []byte(`{"version":"freeside.commit-plan/v1","groups":[{"name":"first","message":"Add a","paths":["a.txt"]}]}`)
	if err := os.WriteFile(filepath.Join(workspace, export.CommitPlanFilename), plan, 0o600); err != nil {
		t.Fatal(err)
	}
	handoff := exportWorkspace(t, workspace)
	res, err := CheckCommitPlan(t.Context(), handoff, cloneAtBase(t, repo), Options{BaseSHA: base})
	if err != nil {
		t.Fatalf("CheckCommitPlan: %v", err)
	}
	if res.Honored {
		t.Fatal("expected Honored=false")
	}
	if len(res.Problems) != MaxReportedUncoveredPaths+1 {
		t.Fatalf("got %d problems, want %d (cap + summary)", len(res.Problems), MaxReportedUncoveredPaths+1)
	}
	var gotPaths []string
	for i := range MaxReportedUncoveredPaths {
		p := res.Problems[i]
		if p.Code != PlanProblemPathUncovered || p.Path == "" {
			t.Fatalf("problem[%d] = %+v", i, p)
		}
		gotPaths = append(gotPaths, p.Path)
	}
	if !slices.IsSorted(gotPaths) {
		t.Errorf("uncovered paths not sorted: %v", gotPaths)
	}
	last := res.Problems[MaxReportedUncoveredPaths]
	if last.Code != PlanProblemPathUncovered || last.Path != "" || !strings.Contains(last.Message, "5 more") {
		t.Fatalf("summary problem = %+v, want 5-more omission notice", last)
	}
}

// TestCheckCommitPlanAllowsHeadPastBaseAndRefusesIntakeErrors verifies that
// CheckCommitPlan works after the writer commits past BaseSHA and maps
// non-plan intake errors to PlanProblemHandoffRefused.
func TestCheckCommitPlanAllowsHeadPastBaseAndRefusesIntakeErrors(t *testing.T) {
	t.Parallel()
	repo, base := initBaseRepo(t, map[string]string{"a.txt": "old\n"})
	clone := cloneAtBase(t, repo)
	// Writer commits a change locally so HEAD != base.
	if err := os.WriteFile(filepath.Join(clone, "a.txt"), []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rungit(t, clone, "add", "a.txt")
	rungit(t, clone, "commit", "-m", "writer local commit")

	workspace := writeWorkspaceFiles(t, map[string]string{"a.txt": "new\n"})
	plan := []byte(`{"version":"freeside.commit-plan/v1","groups":[{"name":"all","message":"Update a","remainder":true}]}`)
	if err := os.WriteFile(filepath.Join(workspace, export.CommitPlanFilename), plan, 0o600); err != nil {
		t.Fatal(err)
	}
	handoff := exportWorkspace(t, workspace)

	res, err := CheckCommitPlan(t.Context(), handoff, clone, Options{BaseSHA: base})
	if err != nil || !res.Honored || len(res.Problems) != 0 {
		t.Fatalf("CheckCommitPlan after local commit = %+v, %v; want honored", res, err)
	}

	// Non-plan intake refusal (orphan entry in handoff) becomes handoff_refused.
	if err := os.WriteFile(filepath.Join(handoff, "stray.txt"), []byte("orphan"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err = CheckCommitPlan(t.Context(), handoff, clone, Options{BaseSHA: base})
	if err != nil || res.Honored || len(res.Problems) != 1 || res.Problems[0].Code != PlanProblemHandoffRefused {
		t.Fatalf("intake refusal = %+v, %v; want handoff_refused", res, err)
	}
}
