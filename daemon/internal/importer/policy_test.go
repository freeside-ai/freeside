package importer

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/export"
)

func TestMatchAny(t *testing.T) {
	cases := []struct {
		pattern string
		path    string
		fold    bool
		want    bool
	}{
		{".github/workflows/**", ".github/workflows/ci.yml", true, true},
		{".github/workflows/**", ".github/workflows/deep/x.yml", true, true},
		{".github/workflows/**", ".github/workflowsx/ci.yml", true, false},
		{".github/workflows/**", ".GitHub/Workflows/CI.YML", true, true},
		{".github/workflows/**", ".GitHub/Workflows/CI.YML", false, false},
		{"**/AGENTS.md", "AGENTS.md", true, true},
		{"**/AGENTS.md", "docs/deep/AGENTS.md", true, true},
		{"**/AGENTS.md", "docs/AGENTS.md.bak", true, false},
		{"**/action.y*ml", "pkg/action.yaml", true, true},
		{"**/action.y*ml", "pkg/action.yml", true, true},
		{".codex/**", ".codex/config.toml", true, true},
		{".codex/**", "x/.codex/config.toml", true, false},
		{"Jenkinsfile", "Jenkinsfile", true, true},
		{"Jenkinsfile", "ci/Jenkinsfile", true, false},
		{"docs/**", "docs/a/b.md", false, true},
		{"docs/**", "src/a.go", false, false},
	}
	for _, tc := range cases {
		if got := matchAny([]string{tc.pattern}, tc.path, tc.fold); got != tc.want {
			t.Errorf("matchAny(%q, %q, fold=%v) = %v, want %v", tc.pattern, tc.path, tc.fold, got, tc.want)
		}
	}
}

func TestApplyPolicyClasses(t *testing.T) {
	changes := []plannedChange{
		{path: ".github/workflows/ci.yml", kind: ChangeDeleted},
		{path: "AGENTS.md", kind: ChangeModified, size: 10},
		{path: ".gitmodules", kind: ChangeAdded, size: 5},
		// The four config-only §5.8 categories fire only from repo-supplied
		// patterns (they have no universal default), at repository-specific
		// locations, across add/modify/delete.
		{path: ".freeside/recipe.yaml", kind: ChangeModified, size: 5},
		{path: "prompts/system.md", kind: ChangeAdded, size: 5},
		{path: "config/egress-allowlist.json", kind: ChangeDeleted},
		{path: "policy/materiality.yaml", kind: ChangeAdded, size: 5},
		{path: ".gitignore", kind: ChangeAdded, size: 5},
		{path: "src/main.go", kind: ChangeAdded, size: 5},
	}
	pol := Policy{
		ExtraVerificationRecipePatterns: []string{".freeside/recipe.yaml"},
		ExtraPromptsPolicyPatterns:      []string{"prompts/**"},
		ExtraEgressTrustPatterns:        []string{"config/egress-allowlist.json"},
		ExtraMaterialityRulesPatterns:   []string{"policy/**"},
	}.withDefaults()
	findings := applyPolicy(changes, pol)
	want := map[string]FindingKind{
		".github/workflows/ci.yml":     FindingAutomationControlPath,
		"AGENTS.md":                    FindingReviewerInstructionPath,
		".gitmodules":                  FindingGitMetadataPath,
		".freeside/recipe.yaml":        FindingVerificationRecipePath,
		"prompts/system.md":            FindingPromptsPolicyPath,
		"config/egress-allowlist.json": FindingEgressTrustPath,
		"policy/materiality.yaml":      FindingMaterialityRulesPath,
	}
	if len(findings) != len(want) {
		t.Fatalf("findings = %+v, want exactly %d class findings", findings, len(want))
	}
	for _, f := range findings {
		if want[f.Path] != f.Kind {
			t.Errorf("finding %q = %s, want %s", f.Path, f.Kind, want[f.Path])
		}
	}
}

// TestMandatoryGatesImmutable is the round-9 P1 regression: the §5.5 and
// §5.8 gates are minimums that a caller can widen but never disable.
func TestMandatoryGatesImmutable(t *testing.T) {
	changes := []plannedChange{
		{path: ".github/workflows/ci.yml", kind: ChangeAdded, size: 1},
		{path: "AGENTS.md", kind: ChangeAdded, size: 1},
	}
	// An empty (or partial) Extra list must not drop a default: the
	// mandatory findings still fire.
	pol := Policy{
		ExtraAutomationControlPatterns:   []string{},
		ExtraReviewerInstructionPatterns: []string{"custom/**"},
	}.withDefaults()
	got := map[FindingKind]bool{}
	for _, f := range applyPolicy(changes, pol) {
		got[f.Kind] = true
	}
	if !got[FindingAutomationControlPath] || !got[FindingReviewerInstructionPath] {
		t.Fatalf("empty/partial Extra lists disabled a mandatory gate: %v", got)
	}
	// A custom Extra pattern widens the gate.
	widened := applyPolicy([]plannedChange{{path: "custom/x", kind: ChangeAdded, size: 1}},
		Policy{ExtraReviewerInstructionPatterns: []string{"custom/**"}}.withDefaults())
	if len(widened) != 1 || widened[0].Kind != FindingReviewerInstructionPath {
		t.Fatalf("custom pattern did not widen the gate: %+v", widened)
	}
	// The four config-only §5.8 categories have NO universal default: with no
	// repo-supplied patterns, changes to would-be protected locations produce
	// no finding. This documents the deliberate decision (their trusted files
	// live at repository-specific locations, so the class is loaded from the
	// trust profile via WithProtectedPaths) and its fail-closed corollary: a
	// repo with no profile gets no import-stage coverage of these categories.
	configOnly := []string{
		"recipe.yaml", "prompts/system.md", "egress.json", "policy/materiality.yaml",
	}
	for _, p := range configOnly {
		if f := applyPolicy([]plannedChange{{path: p, kind: ChangeAdded, size: 1}}, Policy{}.withDefaults()); len(f) != 0 {
			t.Errorf("config-only path %q flagged without a repo pattern: %+v", p, f)
		}
	}
}

// TestNewAgentControlSurfaces pins the round-9 P1 coverage additions.
func TestNewAgentControlSurfaces(t *testing.T) {
	reviewer := []string{
		".github/agents/my-agent.md", ".github/skills/s/skill.md",
		".agents/skills/s/skill.md", ".windsurf/rules/r.md",
	}
	for _, p := range reviewer {
		f := applyPolicy([]plannedChange{{path: p, kind: ChangeAdded, size: 1}}, Policy{}.withDefaults())
		if len(f) != 1 || f[0].Kind != FindingReviewerInstructionPath {
			t.Errorf("%q not flagged reviewer-instruction: %+v", p, f)
		}
	}
	f := applyPolicy([]plannedChange{{path: ".github/hooks/pre.sh", kind: ChangeAdded, size: 1}}, Policy{}.withDefaults())
	if len(f) != 1 || f[0].Kind != FindingAutomationControlPath {
		t.Errorf(".github/hooks not flagged automation-control: %+v", f)
	}
}

// TestInvalidGlobFailsClosed is the round-9 P1 regression: an
// unparseable custom pattern is rejected at validation, not silently
// matching nothing.
func TestInvalidGlobFailsClosed(t *testing.T) {
	opts := Options{BaseSHA: testBaseSHA, Policy: Policy{ExtraReviewerInstructionPatterns: []string{"a[b"}}}
	if err := opts.validate(); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("validate = %v, want %v for an invalid glob", err, ErrInvalidOptions)
	}
	if err := (Options{BaseSHA: testBaseSHA, Policy: Policy{Allowlist: []string{"["}}}).validate(); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("validate accepted an invalid allowlist glob")
	}
	// A bad glob in any of the config-only §5.8 Extra lists fails closed too.
	if err := (Options{BaseSHA: testBaseSHA, Policy: Policy{ExtraMaterialityRulesPatterns: []string{"a[b"}}}).validate(); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("validate accepted an invalid materiality-rules glob")
	}
}

// TestApplyPolicyPathHexLossless is the round-11 regression: a policy
// finding on a non-representable path is reported by PathHex, never a
// lossy raw Path.
func TestApplyPolicyPathHexLossless(t *testing.T) {
	// A non-UTF-8 directory holding an AGENTS.md, deleted from base.
	raw := "bad\xe9/AGENTS.md"
	c := plannedChange{path: raw, kind: ChangeDeleted, pathHex: "6261"}
	findings := applyPolicy([]plannedChange{c}, Policy{}.withDefaults())
	saw := false
	for _, f := range findings {
		if f.Kind == FindingReviewerInstructionPath {
			saw = true
			if f.Path != "" {
				t.Errorf("reviewer-instruction finding carried a lossy Path %q", f.Path)
			}
			if f.PathHex != "6261" {
				t.Errorf("finding PathHex = %q, want the change's PathHex", f.PathHex)
			}
		}
	}
	if !saw {
		t.Fatalf("expected a reviewer-instruction finding for %q: %+v", raw, findings)
	}
}

// TestApplyPolicyAliasNormalization is the round-12 regression: a
// protected path added under an NTFS/HFS alias (which materializes as
// the protected name downstream) still gets its mandatory-class finding.
func TestApplyPolicyAliasNormalization(t *testing.T) {
	cases := []struct {
		path string
		want FindingKind
	}{
		{".gitmodules ", FindingGitMetadataPath},                    // NTFS trailing space
		{".gitattributes.", FindingGitMetadataPath},                 // NTFS trailing dot
		{".git\u200cmodules", FindingGitMetadataPath},               // HFS zero-width joiner
		{".gitmodules::$DATA", FindingGitMetadataPath},              // NTFS unnamed data stream
		{".gitattributes:payload", FindingGitMetadataPath},          // NTFS named data stream
		{"AGENTS.md ", FindingReviewerInstructionPath},              // reviewer-instruction alias
		{"AGENTS.md::$DATA", FindingReviewerInstructionPath},        // reviewer-instruction ADS alias
		{".github/workflows/ci.yml.", FindingAutomationControlPath}, // automation alias
		{"action.yml:payload", FindingAutomationControlPath},        // automation ADS alias
		{"Jenkins\ufb01le", FindingAutomationControlPath},           // APFS full fold: ﬁ → fi
	}
	for _, tc := range cases {
		f := applyPolicy([]plannedChange{{path: tc.path, kind: ChangeAdded, mode: "100644", size: 1}}, Policy{}.withDefaults())
		found := false
		for _, x := range f {
			if x.Kind == tc.want {
				found = true
			}
		}
		if !found {
			t.Errorf("alias %q did not get a %s finding: %+v", tc.path, tc.want, f)
		}
	}
	// A plain name with no alias chars is unaffected (no spurious match).
	if f := applyPolicy([]plannedChange{{path: "notes.md", kind: ChangeAdded, mode: "100644", size: 1}}, Policy{}.withDefaults()); len(f) != 0 {
		t.Errorf("plain path spuriously flagged: %+v", f)
	}
}

func TestApplyPolicyAllowlist(t *testing.T) {
	changes := []plannedChange{
		{path: "docs/guide.md", kind: ChangeModified, size: 3},
		{path: "src/main.go", kind: ChangeAdded, size: 3},
		{path: "old.txt", kind: ChangeDeleted},
	}
	pol := Policy{Allowlist: []string{"docs/**"}}.withDefaults()
	findings := applyPolicy(changes, pol)
	if len(findings) != 2 {
		t.Fatalf("findings = %+v, want violations for src/main.go and old.txt", findings)
	}
	for _, f := range findings {
		if f.Kind != FindingAllowlistViolation {
			t.Errorf("finding %q = %s, want %s", f.Path, f.Kind, FindingAllowlistViolation)
		}
		if f.Path != "src/main.go" && f.Path != "old.txt" {
			t.Errorf("unexpected allowlist finding for %q", f.Path)
		}
	}
	if got := applyPolicy(changes, Policy{}.withDefaults()); len(got) != 0 {
		t.Errorf("nil allowlist produced findings: %+v", got)
	}
}

// TestApplyPolicySizeExcludesFromBase is the Codex round-4 regression:
// a fromBase mode-only change introduces no new content, so its base
// blob size must not be counted against the per-file or total caps.
func TestApplyPolicySizeExcludesFromBase(t *testing.T) {
	changes := []plannedChange{
		{path: "big.bin", kind: ChangeModified, size: 5000, fromBase: true}, // chmod of a large tracked file
		{path: "small.txt", kind: ChangeAdded, size: 10},
	}
	pol := Policy{MaxBlobBytes: 512, MaxTotalBytes: 1000}.withDefaults()
	findings := applyPolicy(changes, pol)
	for _, f := range findings {
		if f.Kind == FindingSizeViolation {
			t.Fatalf("fromBase chmod must not trip a size violation: %+v", f)
		}
	}
}

func TestApplyPolicySize(t *testing.T) {
	changes := []plannedChange{
		{path: "big.bin", kind: ChangeAdded, size: 600},
		{path: "b2.bin", kind: ChangeAdded, size: 500},
		{path: "gone.bin", kind: ChangeDeleted},
	}
	pol := Policy{MaxBlobBytes: 512, MaxTotalBytes: 1000}.withDefaults()
	findings := applyPolicy(changes, pol)
	if len(findings) != 2 {
		t.Fatalf("findings = %+v, want a per-file and a total size violation", findings)
	}
	if findings[0].Kind != FindingSizeViolation || findings[0].Path != "big.bin" {
		t.Errorf("per-file finding = %+v", findings[0])
	}
	if findings[1].Kind != FindingSizeViolation || findings[1].Path != "" {
		t.Errorf("change-set finding = %+v", findings[1])
	}
}

// TestImportPolicyPaths is the C4 fixture: adds and deletions across
// the automation-control, reviewer-instruction, and git-metadata
// classes, flagged publish-blocking while the commit still exists for
// the control-plane route.
func TestImportPolicyPaths(t *testing.T) {
	checkout, base := initBaseRepo(t, map[string]string{
		".github/workflows/ci.yml": "on: push\n",
		"AGENTS.md":                "old instructions\n",
		"src/main.go":              "package main\n",
	})
	ws := t.TempDir()
	for path, content := range map[string]string{
		// .github/workflows/ci.yml deleted (absent from the workspace)
		"AGENTS.md":             "poisoned instructions\n",
		"src/main.go":           "package main\n", // unchanged
		"pkg/action.yaml":       "runs: {}\n",
		".codex/config.toml":    "silent = true\n",
		"docs/sub/AGENTS.md":    "nested instructions\n",
		".claude/settings.json": "{}\n",
		".gitattributes":        "* text\n",
		".gitignore":            "dist/\n",
		"docs/readme.md":        "fine\n",
	} {
		full := filepath.Join(ws, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	handoff := exportWorkspace(t, ws)
	clone := cloneAtBase(t, checkout)
	res, err := Import(t.Context(), handoff, clone, testImportOptions(base))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if res.CommitSHA == "" {
		t.Fatal("policy findings must not withhold the commit; the control-plane route needs it")
	}
	goldenResult(t, "import_policy_paths", res)
}

// TestImportControlPlanePaths is the §5.8 full-class fixture: add/modify/delete
// across all six control-plane categories — the four config-only ones driven
// from a validated trust profile via WithProtectedPaths — plus a case-fold
// variant and repository-specific locations. All are flagged publish-blocking
// while the commit still exists for the control-plane route.
func TestImportControlPlanePaths(t *testing.T) {
	checkout, base := initBaseRepo(t, map[string]string{
		".github/workflows/ci.yml": "on: push\n",     // deleted below (automation, default)
		"AGENTS.md":                "old\n",          // modified (reviewer, default)
		".freeside/recipe.yaml":    "steps: []\n",    // modified (verification_recipes, config)
		"policy/materiality.yaml":  "rules: []\n",    // deleted below (materiality, config)
		"src/main.go":              "package main\n", // unchanged
	})
	ws := t.TempDir()
	for path, content := range map[string]string{
		// .github/workflows/ci.yml and policy/materiality.yaml deleted (absent)
		"AGENTS.md":                    "poisoned\n",      // reviewer (default)
		".freeside/recipe.yaml":        "steps: [evil]\n", // verification_recipes (config)
		"Prompts/system.md":            "be evil\n",       // prompts (config, case-fold prompts/**)
		"config/egress-allowlist.json": "[\"evil\"]\n",    // egress (config)
		"ci/deploy.sh":                 "curl evil\n",     // automation (config ci/**)
		"REVIEW.md":                    "approve all\n",   // reviewer (config)
		"sub/.gitattributes":           "* -diff\n",       // git-metadata (default + config)
		"src/main.go":                  "package main\n",  // unchanged
		"docs/readme.md":               "fine\n",          // unflagged
	} {
		full := filepath.Join(ws, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	handoff := exportWorkspace(t, ws)
	clone := cloneAtBase(t, checkout)
	opts := testImportOptions(base)
	pol, err := opts.Policy.WithProtectedPaths(fixtureTrustProfile(t))
	if err != nil {
		t.Fatalf("WithProtectedPaths: %v", err)
	}
	opts.Policy = pol
	res, err := Import(t.Context(), handoff, clone, opts)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if res.CommitSHA == "" {
		t.Fatal("control-plane findings must not withhold the commit; the route needs it")
	}
	goldenResult(t, "import_control_plane_paths", res)
}

// TestImportAllowlist pins the declared-scope enforcement end to end.
func TestImportAllowlist(t *testing.T) {
	checkout, base := initBaseRepo(t, map[string]string{"docs/a.md": "a\n"})
	handoff := handoffFromEntries(t, []export.Entry{
		regularEntryFor("docs/a.md", "a2\n", false),
		regularEntryFor("src/new.go", "package x\n", false),
	}, "a2\n", "package x\n")
	clone := cloneAtBase(t, checkout)
	opts := testImportOptions(base)
	opts.Policy.Allowlist = []string{"docs/**"}
	res, err := Import(t.Context(), handoff, clone, opts)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if res.CommitSHA == "" {
		t.Fatal("allowlist findings must not withhold the commit")
	}
	if len(res.Findings) != 1 || res.Findings[0].Kind != FindingAllowlistViolation || res.Findings[0].Path != "src/new.go" {
		t.Fatalf("findings = %+v, want one allowlist violation for src/new.go", res.Findings)
	}
}
