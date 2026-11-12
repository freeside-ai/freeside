package importer

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// fixtureTrustProfile builds a valid, digest-authentic AutomationTrustProfile
// whose ProtectedPaths widen all seven categories, for the import-stage
// control-plane tests.
func fixtureTrustProfile(t *testing.T) domain.AutomationTrustProfile {
	t.Helper()
	return fixtureTrustProfileProtecting(t, domain.ProtectedPathConfig{
		ExtraAutomationControlPatterns:   []string{"ci/**"},
		ExtraReviewerInstructionPatterns: []string{"REVIEW.md"},
		ExtraGitMetadataPatterns:         []string{"sub/.gitattributes"},
		ExtraVerificationControlPatterns: []string{".freeside/recipe.yaml"},
		ExtraPromptsAndPolicyPatterns:    []string{"prompts/**"},
		ExtraEgressAndTrustPatterns:      []string{"config/egress-allowlist.json"},
		ExtraMaterialityRulesPatterns:    []string{"policy/**"},
	})
}

// fixtureTrustProfileProtecting is the fixture profile with a caller-chosen
// protected-path configuration.
func fixtureTrustProfileProtecting(
	t *testing.T, paths domain.ProtectedPathConfig,
) domain.AutomationTrustProfile {
	t.Helper()
	return fixtureTrustProfileWithStance(t, paths, domain.AutomationChangesBlocked)
}

// fixtureTrustProfileWithStance is fixtureTrustProfileProtecting under the
// given candidate_automation_changes stance.
func fixtureTrustProfileWithStance(
	t *testing.T, paths domain.ProtectedPathConfig, stance domain.AutomationChangePolicy,
) domain.AutomationTrustProfile {
	t.Helper()
	profile, err := domain.NewAutomationTrustProfile(domain.AutomationTrustProfileInput{
		Repo:                       "freeside-ai/demo",
		RepositoryID:               123456789,
		PRExecution:                domain.PRExecutionAuditedSameRepo,
		CandidateAutomationChanges: stance,
		PRGitHubTokenPermissions:   domain.TokenPermissionsReadOnly,
		CommitPlan:                 domain.CommitPlanSingleCommit,
		MessageRuleset:             domain.MessageRulesetGitHub1,
		WorkflowAuditDigest:        "sha256:workflow-audit",
		Review: domain.ReviewSettings{
			Mode: domain.ReviewFreesideInvoked, ConfigDigest: "sha256:review-config",
		},
		ProtectedPaths: paths,
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
	t.Parallel()
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
	t.Parallel()
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
		FindingCommitPlanSecret:        domain.FindingClassSecret,
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
		cf := Finding{Kind: k, Path: "p/x", Detail: "added"}.Candidate(domain.AutomationChangesBlocked)
		if err := cf.Validate(); err != nil {
			t.Errorf("%s: Candidate().Validate() = %v", k, err)
		}
		if cf.Class != wantClass[k] {
			t.Errorf("%s: class = %q, want %q", k, cf.Class, wantClass[k])
		}
		if cf.Origin != domain.FindingOriginImport {
			t.Errorf("%s: origin = %q, want import", k, cf.Origin)
		}
		// Under block, reviewer instructions are the one advisory kind (plan
		// §5.8, revision 42); every other kind lifts blocking. The advisory
		// stance adds automation-control paths and nothing else.
		wantDisposition := domain.DispositionBlocking
		if k == FindingReviewerInstructionPath {
			wantDisposition = domain.DispositionAdvisory
		}
		if cf.Disposition != wantDisposition {
			t.Errorf("%s: disposition = %q, want %q", k, cf.Disposition, wantDisposition)
		}
		advisory := Finding{Kind: k, Path: "p/x", Detail: "added"}.Candidate(domain.AutomationChangesAdvisory)
		if err := advisory.Validate(); err != nil {
			t.Errorf("%s: Candidate(advisory).Validate() = %v", k, err)
		}
		if k == FindingAutomationControlPath {
			wantDisposition = domain.DispositionAdvisory
		}
		if advisory.Disposition != wantDisposition {
			t.Errorf("%s: disposition under advisory = %q, want %q", k, advisory.Disposition, wantDisposition)
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

// TestFindingAdvisoryStance: under the block stance (and its zero value)
// exactly the reviewer-instruction kind is advisory (plan §5.8, revision 42);
// the advisory stance adds the automation-control kind (§5.5) and nothing
// else. An advisory finding is non-fatal under the default and
// publish-strict profiles and still fatal under the specification profile,
// and AllAdvisory is the clean bar an import meets.
func TestFindingAdvisoryStance(t *testing.T) {
	t.Parallel()
	strict := FindingProfilePublishStrict
	spec := FindingProfileSpecification
	stances := map[domain.AutomationChangePolicy][]FindingKind{
		"":                               {FindingReviewerInstructionPath},
		domain.AutomationChangesBlocked:  {FindingReviewerInstructionPath},
		domain.AutomationChangesAdvisory: {FindingReviewerInstructionPath, FindingAutomationControlPath},
	}
	for stance, advisoryKinds := range stances {
		for _, k := range AllFindingKinds {
			f := Finding{Kind: k, Path: "p/x"}
			wantAdvisory := slices.Contains(advisoryKinds, k)
			if f.Advisory(stance) != wantAdvisory {
				t.Errorf("%q %s: Advisory() = %v, want %v", stance, k, f.Advisory(stance), wantAdvisory)
			}
			if f.Fatal(nil, stance) != !wantAdvisory || f.Fatal(&strict, stance) != !wantAdvisory {
				t.Errorf("%q %s: Fatal(nil)=%v Fatal(strict)=%v, want %v",
					stance, k, f.Fatal(nil, stance), f.Fatal(&strict, stance), !wantAdvisory)
			}
			if wantAdvisory && !f.Fatal(&spec, stance) {
				t.Errorf("%q %s: specification profile must keep the path fatal", stance, k)
			}
		}
	}
	advisory := Finding{Kind: FindingReviewerInstructionPath, Path: "AGENTS.md"}
	workflow := Finding{Kind: FindingAutomationControlPath, Path: ".github/workflows/ci.yml"}
	if !AllAdvisory(nil, "") || !AllAdvisory([]Finding{advisory}, "") ||
		AllAdvisory([]Finding{advisory, {Kind: FindingSizeViolation, Path: "big.bin"}}, "") {
		t.Error("AllAdvisory: empty and advisory-only sets qualify, a blocking sibling does not")
	}
	if AllAdvisory([]Finding{advisory, workflow}, domain.AutomationChangesBlocked) ||
		!AllAdvisory([]Finding{advisory, workflow}, domain.AutomationChangesAdvisory) {
		t.Error("AllAdvisory: a workflow edit qualifies under advisory only")
	}
}

// TestFindingCandidateLiftPathHex: a lifted finding on a non-representable path
// carries PathHex losslessly and never a Path (mirrors the manifest's
// mutual-exclusion, which CandidateFinding.Validate enforces).
func TestFindingCandidateLiftPathHex(t *testing.T) {
	t.Parallel()
	cf := Finding{Kind: FindingReviewerInstructionPath, PathHex: "6261", Detail: "deleted"}.Candidate("")
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
	t.Parallel()
	a := Finding{Kind: FindingSecret, Path: "conf.env", Rule: "aws_key", Line: 3}.Candidate("")
	b := Finding{Kind: FindingSecret, Path: "conf.env", Rule: "gh_token", Line: 7}.Candidate("")
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
		EvidenceSnapshotDigest:   "sha256:evidence-snapshot",
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
	t.Parallel()
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
	if got.CommitPlan != domain.CommitPlanSingleCommit || got.MessageRuleset != domain.MessageRulesetGitHub1 {
		t.Fatalf("profile commit policy not copied: %+v", got)
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
	t.Parallel()
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

// TestWithProtectedPathsStance: the automation-change stance crosses the
// boundary from the digest-verified profile. Advisory is recorded; block is
// the policy's zero value, so a block profile's options keep the bytes they
// had before the field existed, and a prior advisory value on the policy is
// replaced rather than merged.
func TestWithProtectedPathsStance(t *testing.T) {
	t.Parallel()
	advisory, err := (Policy{}).WithProtectedPaths(
		fixtureTrustProfileWithStance(t, domain.ProtectedPathConfig{}, domain.AutomationChangesAdvisory))
	if err != nil {
		t.Fatal(err)
	}
	if advisory.CandidateAutomationChanges != domain.AutomationChangesAdvisory {
		t.Fatalf("advisory profile stance = %q, want advisory", advisory.CandidateAutomationChanges)
	}
	blocked, err := advisory.WithProtectedPaths(fixtureTrustProfileProtecting(t, domain.ProtectedPathConfig{}))
	if err != nil {
		t.Fatal(err)
	}
	if blocked.CandidateAutomationChanges != "" {
		t.Fatalf("block profile stance = %q, want the zero value", blocked.CandidateAutomationChanges)
	}
	encoded, err := json.Marshal(blocked)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "CandidateAutomationChanges") {
		t.Fatalf("block policy encodes the stance key: %s", encoded)
	}
	// A profile whose stance was flipped after its digest was computed never
	// reaches the policy.
	tampered := fixtureTrustProfileProtecting(t, domain.ProtectedPathConfig{})
	tampered.CandidateAutomationChanges = domain.AutomationChangesAdvisory
	if _, err := (Policy{}).WithProtectedPaths(tampered); !errors.Is(err, domain.ErrProfileDigestMismatch) {
		t.Fatalf("tampered stance error = %v, want ErrProfileDigestMismatch", err)
	}
}

// TestControlPlaneConfigAliasNormalization: a config-only protected path added
// under an NTFS/HFS alias still gets its finding, so config-driven patterns run
// the same normalizeAliases path as the defaulted classes.
func TestControlPlaneConfigAliasNormalization(t *testing.T) {
	t.Parallel()
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
