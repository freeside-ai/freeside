package importer

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// fixtureTrustProfile builds a valid, digest-authentic AutomationTrustProfile
// whose ProtectedPaths widen all seven categories, for the import-stage
// control-plane tests.
func fixtureTrustProfile(t *testing.T) domain.AutomationTrustProfile {
	t.Helper()
	profile, err := domain.NewAutomationTrustProfile(domain.AutomationTrustProfileInput{
		Repo:                       "freeside-ai/demo",
		PRExecution:                domain.PRExecutionAuditedSameRepo,
		CandidateAutomationChanges: domain.AutomationChangesBlocked,
		PRGitHubTokenPermissions:   domain.TokenPermissionsReadOnly,
		WorkflowAuditDigest:        "sha256:workflow-audit",
		Review: domain.ReviewSettings{
			Mode: domain.ReviewAuto, ConfigDigest: "sha256:review-config",
		},
		ProtectedPaths: domain.ProtectedPathConfig{
			ExtraAutomationControlPatterns:   []string{"ci/**"},
			ExtraReviewerInstructionPatterns: []string{"REVIEW.md"},
			ExtraGitMetadataPatterns:         []string{"sub/.gitattributes"},
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

// TestControlPlaneCategoryCoverage asserts the controlPlaneClasses table covers
// every §5.8 category exactly once: the runtime completeness check the
// exhaustive linter cannot give for a table literal. A new domain category with
// no importer gate would fail here.
func TestControlPlaneCategoryCoverage(t *testing.T) {
	seen := map[domain.ControlPlaneCategory]int{}
	for _, cl := range controlPlaneClasses {
		seen[cl.category]++
	}
	for _, cat := range domain.AllControlPlaneCategories {
		if seen[cat] != 1 {
			t.Errorf("category %q appears in %d table rows, want exactly 1", cat, seen[cat])
		}
	}
	if len(controlPlaneClasses) != len(domain.AllControlPlaneCategories) {
		t.Errorf("table has %d rows, want %d (one per category)",
			len(controlPlaneClasses), len(domain.AllControlPlaneCategories))
	}
}

// TestFindingCandidateLift pins the FindingKind → CandidateFinding{Class,
// Category} mapping: every import kind lifts to a valid, blocking,
// import-origin CandidateFinding of the expected class, control-plane kinds
// carry their §5.8 category and the rest carry none.
func TestFindingCandidateLift(t *testing.T) {
	wantClass := map[FindingKind]domain.CandidateFindingClass{
		FindingNonRegularChange:        domain.FindingClassImportIntegrity,
		FindingInvalidPathEntry:        domain.FindingClassImportIntegrity,
		FindingBlobOmitted:             domain.FindingClassImportIntegrity,
		FindingAllowlistViolation:      domain.FindingClassRepoChangePolicy,
		FindingSizeViolation:           domain.FindingClassRepoChangePolicy,
		FindingPathCollision:           domain.FindingClassRepoChangePolicy,
		FindingGitMetadataPath:         domain.FindingClassRepoChangePolicy,
		FindingSecret:                  domain.FindingClassSecret,
		FindingSecretScanSkipped:       domain.FindingClassSecret,
		FindingAutomationControlPath:   domain.FindingClassControlPlane,
		FindingReviewerInstructionPath: domain.FindingClassControlPlane,
		FindingVerificationRecipePath:  domain.FindingClassControlPlane,
		FindingPromptsPolicyPath:       domain.FindingClassControlPlane,
		FindingEgressTrustPath:         domain.FindingClassControlPlane,
		FindingMaterialityRulesPath:    domain.FindingClassControlPlane,
	}
	// The map must cover every kind: a new kind added without a lift class
	// fails here before it can reach the gate unclassed.
	if len(wantClass) != len(AllFindingKinds) {
		t.Fatalf("wantClass covers %d kinds, AllFindingKinds has %d", len(wantClass), len(AllFindingKinds))
	}
	for _, k := range AllFindingKinds {
		cf := Finding{Kind: k, Path: "p/x", Detail: "added"}.Candidate()
		if err := cf.Validate(); err != nil {
			t.Errorf("%s: Candidate().Validate() = %v", k, err)
		}
		if cf.Class != wantClass[k] {
			t.Errorf("%s: class = %q, want %q", k, cf.Class, wantClass[k])
		}
		if cf.Origin != domain.FindingOriginImport {
			t.Errorf("%s: origin = %q, want import", k, cf.Origin)
		}
		if cf.Disposition != domain.DispositionBlocking {
			t.Errorf("%s: disposition = %q, want blocking", k, cf.Disposition)
		}
		if cf.Kind != string(k) {
			t.Errorf("%s: kind token = %q", k, cf.Kind)
		}
		if wantClass[k] == domain.FindingClassControlPlane {
			cat, ok := categoryFor(k)
			if !ok {
				t.Errorf("%s: control-plane kind not in the category table", k)
			} else if cf.Category == nil || *cf.Category != cat {
				t.Errorf("%s: category = %v, want %q", k, cf.Category, cat)
			}
		} else if cf.Category != nil {
			t.Errorf("%s: non-control-plane finding carries category %q", k, *cf.Category)
		}
	}
}

// TestFindingCandidateLiftPathHex: a lifted finding on a non-representable path
// carries PathHex losslessly and never a Path (mirrors the manifest's
// mutual-exclusion, which CandidateFinding.Validate enforces).
func TestFindingCandidateLiftPathHex(t *testing.T) {
	cf := Finding{Kind: FindingReviewerInstructionPath, PathHex: "6261", Detail: "deleted"}.Candidate()
	if err := cf.Validate(); err != nil {
		t.Fatalf("Validate = %v", err)
	}
	if cf.Path != "" || cf.PathHex != "6261" {
		t.Errorf("PathHex not carried losslessly: path=%q hex=%q", cf.Path, cf.PathHex)
	}
}

// TestFindingCandidateLiftSecretsDistinct is the Codex round-1 P1 regression:
// two secret matches in one file differ only by rule/line — fields the domain
// CandidateFinding lacks — so the lift must fold them into Detail or both lift
// to identical findings that NewCandidateAuthorization rejects as duplicates,
// sinking the authorization instead of recording the blocking secrets.
func TestFindingCandidateLiftSecretsDistinct(t *testing.T) {
	a := Finding{Kind: FindingSecret, Path: "conf.env", Rule: "aws_key", Line: 3}.Candidate()
	b := Finding{Kind: FindingSecret, Path: "conf.env", Rule: "gh_token", Line: 7}.Candidate()
	if a.Detail == b.Detail {
		t.Fatalf("two secret matches in one file lifted to the same detail %q", a.Detail)
	}
	// The two lifted findings must ride into one authorization without tripping
	// the domain's exact-duplicate rejection.
	if _, err := domain.NewCandidateAuthorization(domain.CandidateAuthorizationInput{
		Repo:                     "freeside-ai/demo",
		BaseSHA:                  "beefcafe",
		HeadSHA:                  "cafebabe",
		ImportResultDigest:       "sha256:import-result",
		VerificationRecipeDigest: "sha256:recipe",
		VerificationOutcome:      domain.VerificationFailed,
		Findings:                 []domain.CandidateFinding{a, b},
		TrustProfileDigest:       "sha256:profile",
		InvocationID:             "inv-1",
		CreatedAt:                time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}); err != nil {
		t.Fatalf("distinct secret findings rejected by authorization: %v", err)
	}
}

// TestProtectedPathPolicyFailClosed pins the trust boundary: WithProtectedPaths
// refuses an absent or tampered profile (fail closed) and, for a valid one,
// widens all seven Extra lists (including the verification_control →
// verification_recipes crosswalk) without disturbing the mandatory defaults.
func TestProtectedPathPolicyFailClosed(t *testing.T) {
	// (a) An absent (zero-value) profile fails closed.
	if _, err := (Policy{}).WithProtectedPaths(domain.AutomationTrustProfile{}); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("zero profile: err = %v, want ErrInvalidOptions", err)
	}
	// (b) A profile whose bound digest was forged fails closed.
	tampered := fixtureTrustProfile(t)
	tampered.ProfileDigest = "sha256:forged"
	if _, err := (Policy{}).WithProtectedPaths(tampered); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("tampered digest: err = %v, want ErrInvalidOptions", err)
	}
	// (c) A valid profile widens every category from its config.
	got, err := (Policy{}).WithProtectedPaths(fixtureTrustProfile(t))
	if err != nil {
		t.Fatalf("valid profile: %v", err)
	}
	for _, tc := range []struct {
		name string
		got  []string
		want []string
	}{
		{"automation", got.ExtraAutomationControlPatterns, []string{"ci/**"}},
		{"reviewer", got.ExtraReviewerInstructionPatterns, []string{"REVIEW.md"}},
		{"gitmeta", got.ExtraGitMetadataPatterns, []string{"sub/.gitattributes"}},
		{"verification (crosswalk)", got.ExtraVerificationRecipePatterns, []string{".freeside/recipe.yaml"}},
		{"prompts", got.ExtraPromptsPolicyPatterns, []string{"prompts/**"}},
		{"egress", got.ExtraEgressTrustPatterns, []string{"config/egress-allowlist.json"}},
		{"materiality", got.ExtraMaterialityRulesPatterns, []string{"policy/**"}},
	} {
		if !slices.Equal(tc.got, tc.want) {
			t.Errorf("%s widening = %v, want %v", tc.name, tc.got, tc.want)
		}
	}
	// The mandatory defaults survive the widening (config widens, never narrows).
	auto := got.automationControl()
	if len(auto) == 0 || !slices.Equal(auto[:len(DefaultAutomationControlPatterns)], DefaultAutomationControlPatterns) {
		t.Errorf("mandatory automation defaults dropped: %v", auto)
	}
	// The crosswalk actually fires the verification_recipes gate at import.
	f := applyPolicy([]plannedChange{{path: ".freeside/recipe.yaml", kind: ChangeModified, size: 1}}, got.withDefaults())
	if len(f) != 1 || f[0].Kind != FindingVerificationRecipePath {
		t.Fatalf("verification-recipe gate not wired from profile: %+v", f)
	}
}

// TestWithProtectedPathsSnapshots is the Codex round-2 P2 regression: the
// returned policy must hold a snapshot of the validated profile, not a live
// alias of its backing arrays. A caller mutating the profile in place after the
// boundary (a valid glob edit re-runs no digest check) must not narrow or
// redirect control-plane coverage.
func TestWithProtectedPathsSnapshots(t *testing.T) {
	profile := fixtureTrustProfile(t)
	got, err := (Policy{}).WithProtectedPaths(profile)
	if err != nil {
		t.Fatalf("WithProtectedPaths: %v", err)
	}
	profile.ProtectedPaths.ExtraPromptsAndPolicyPatterns[0] = "other/**"
	if got.ExtraPromptsPolicyPatterns[0] != "prompts/**" {
		t.Fatalf("policy aliases the profile's backing array: %v", got.ExtraPromptsPolicyPatterns)
	}
}

// TestControlPlaneConfigAliasNormalization: a config-only protected path added
// under an NTFS/HFS alias still gets its finding, so config-driven patterns run
// the same normalizeAliases path as the defaulted classes.
func TestControlPlaneConfigAliasNormalization(t *testing.T) {
	pol := Policy{
		ExtraPromptsPolicyPatterns:    []string{"prompts/system.md"},
		ExtraMaterialityRulesPatterns: []string{"policy/materiality.yaml"},
	}.withDefaults()
	cases := []struct {
		path string
		want FindingKind
	}{
		{"prompts/system.md ", FindingPromptsPolicyPath},          // NTFS trailing space
		{"prompts/system.md::$DATA", FindingPromptsPolicyPath},    // NTFS unnamed data stream
		{"policy/materiality.yaml.", FindingMaterialityRulesPath}, // NTFS trailing dot
	}
	for _, tc := range cases {
		f := applyPolicy([]plannedChange{{path: tc.path, kind: ChangeAdded, mode: "100644", size: 1}}, pol)
		found := false
		for _, x := range f {
			if x.Kind == tc.want {
				found = true
			}
		}
		if !found {
			t.Errorf("config alias %q did not get a %s finding: %+v", tc.path, tc.want, f)
		}
	}
}
