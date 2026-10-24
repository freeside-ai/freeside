package exec

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func TestComposeCodexReviewInstructionsCanonicalizesSourcesAndPrecedence(t *testing.T) {
	host := ReviewHostInstructionInput{Present: true, Body: []byte("operator rule\n")}
	bundle, binding, err := ComposeCodexReviewInstructions(host, []ReviewInstructionSourceInput{
		{Path: "daemon/AGENTS.override.md", Body: []byte("daemon rule\n")},
		{Path: "AGENTS.md", Body: []byte("root rule\n")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := []string{binding.RepositorySources[0].Path, binding.RepositorySources[1].Path}; !slices.Equal(got, []string{"AGENTS.md", "daemon/AGENTS.override.md"}) {
		t.Fatalf("repository source order = %v", got)
	}
	text := string(bundle)
	root := strings.Index(text, "root rule")
	nested := strings.Index(text, "daemon rule")
	operator := strings.Index(text, "operator rule")
	if root < 0 || nested <= root || operator <= nested ||
		!strings.Contains(text, `### Scope "."`) ||
		!strings.Contains(text, `### Scope "daemon"`) {
		t.Fatalf("bundle lost scope or precedence:\n%s", text)
	}
	if err := binding.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestComposeCodexReviewInstructionsDistinguishesHostAbsenceFromEmptyFile(t *testing.T) {
	_, absent, err := ComposeCodexReviewInstructions(ReviewHostInstructionInput{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, present, err := ComposeCodexReviewInstructions(
		ReviewHostInstructionInput{Present: true, Body: []byte{}}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if absent.HostDigest != nil || present.HostDigest == nil ||
		absent.ResultDigest == present.ResultDigest {
		t.Fatalf("absence and present-empty collapsed: absent=%+v present=%+v", absent, present)
	}
}

func TestComposeCodexReviewInstructionsIsolatesMarkdownConstructs(t *testing.T) {
	root := []byte("root rule\n```go\nunclosed backtick fence\n")
	nested := []byte("nested rule\n```\n````\n`````\n``````\nwithout trailing newline")
	host := []byte("operator rule\n~~~yaml\nunclosed tilde fence\n")
	repository := []ReviewInstructionSourceInput{
		{Path: "daemon/AGENTS.md", Body: nested},
		{Path: "AGENTS.md", Body: root},
		{Path: "empty/AGENTS.md", Body: nil},
	}
	bundle, binding, err := ComposeCodexReviewInstructions(
		ReviewHostInstructionInput{Present: true, Body: host}, repository,
	)
	if err != nil {
		t.Fatal(err)
	}
	assertReviewInstructionBlocksIsolated(t, bundle, 4)
	if binding.RepositorySources[0].Digest != digestReviewInstruction(root) ||
		binding.RepositorySources[1].Digest != digestReviewInstruction(nested) ||
		binding.HostDigest == nil || *binding.HostDigest != digestReviewInstruction(host) {
		t.Fatalf("source digests no longer bind raw bodies: %+v", binding)
	}

	again, againBinding, err := ComposeCodexReviewInstructions(
		ReviewHostInstructionInput{Present: true, Body: host}, repository,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again, bundle) || againBinding.ResultDigest != binding.ResultDigest {
		t.Fatal("identical inputs produced a different bundle or result digest")
	}
}

func TestComposeCodexReviewInstructionsEnforcesByteCeiling(t *testing.T) {
	_, _, err := ComposeCodexReviewInstructions(
		ReviewHostInstructionInput{},
		[]ReviewInstructionSourceInput{{
			Path: "AGENTS.md", Body: bytes.Repeat([]byte("x"), int(domain.MaxVendorInstructionBytes)),
		}},
	)
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("oversized fenced bundle error = %v", err)
	}
}

func TestReviewInstructionBindingAcceptsPersistedCompositionVersions(t *testing.T) {
	_, binding, err := ComposeCodexReviewInstructions(ReviewHostInstructionInput{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{
		ReviewInstructionCompositionVersionV1,
		ReviewInstructionCompositionVersion,
	} {
		candidate := binding
		candidate.CompositionVersion = version
		if err := candidate.Validate(); err != nil {
			t.Errorf("Validate(%q): %v", version, err)
		}
	}
	binding.CompositionVersion = "codex_explicit_bundle_v3"
	if err := binding.Validate(); err == nil {
		t.Fatal("unknown composition version remained valid")
	}
}

func TestReviewInstructionBindingRejectsReorderedSources(t *testing.T) {
	_, binding, err := ComposeCodexReviewInstructions(
		ReviewHostInstructionInput{},
		[]ReviewInstructionSourceInput{
			{Path: "AGENTS.md", Body: []byte("root")},
			{Path: "z/AGENTS.md", Body: []byte("nested")},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	slices.Reverse(binding.RepositorySources)
	if err := binding.Validate(); err == nil {
		t.Fatal("reordered repository sources remained valid")
	}
}

func TestReviewRequestAuthorityBindsRepositoryAndInstructions(t *testing.T) {
	_, instructions, err := ComposeCodexReviewInstructions(
		ReviewHostInstructionInput{},
		[]ReviewInstructionSourceInput{{Path: "AGENTS.md", Body: []byte("base rules")}},
	)
	if err != nil {
		t.Fatal(err)
	}
	request := ReviewRequest{
		RunID: "run-1", Round: 1, Repo: "owner/repo", RepositoryID: 42,
		BaseRef: "main", BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40),
		Workspace: "/candidate", Instructions: instructions,
		Admission: testReviewAdmission("run-1"),
		Verification: ReviewVerificationEvidence{
			Outcome:                domain.VerificationPassed,
			RecipeDigest:           domain.Digest("sha256:" + strings.Repeat("c", 64)),
			EvidenceSnapshotDigest: domain.Digest("sha256:" + strings.Repeat("d", 64)),
		},
		RequestedAt: time.Date(2026, 8, 4, 0, 0, 0, 0, time.UTC),
	}
	want, err := request.AuthorityDigest()
	if err != nil {
		t.Fatal(err)
	}
	mutations := []func(*ReviewRequest){
		func(r *ReviewRequest) { r.Repo = "other/repo" },
		func(r *ReviewRequest) { r.BaseSHA = strings.Repeat("e", 40) },
		func(r *ReviewRequest) { r.EvaluatedSHA = strings.Repeat("9", 40) },
		func(r *ReviewRequest) {
			r.Instructions.RepositorySources[0].Digest = domain.Digest("sha256:" + strings.Repeat("f", 64))
		},
		func(r *ReviewRequest) {
			r.Instructions.ResultDigest = domain.Digest("sha256:" + strings.Repeat("1", 64))
		},
		// The admitted agent is authority too: a request rewritten to name
		// another agent, identity, or launch value is another request.
		func(r *ReviewRequest) { r.Admission.AuthIdentityID = "auth-other" },
		func(r *ReviewRequest) {
			r.Admission.AgentBinding.AgentDigest = domain.Digest("sha256:" + strings.Repeat("2", 64))
		},
		func(r *ReviewRequest) { r.Admission.AgentBinding.RouteModelID = "other-model" },
		func(r *ReviewRequest) { r.Admission.LineageGroup = "other" },
	}
	for i, mutate := range mutations {
		changed := request
		changed.Instructions.RepositorySources = slices.Clone(request.Instructions.RepositorySources)
		mutate(&changed)
		got, err := changed.AuthorityDigest()
		if err != nil {
			t.Fatalf("mutation %d invalid: %v", i, err)
		}
		if got == want {
			t.Fatalf("mutation %d did not change request authority", i)
		}
	}
}

// TestReviewRequestRequiresItsAdmission pins the request's second legacy
// shape: a request with no admission snapshot, or one admitted for another
// run, does not validate and has no authority digest.
func TestReviewRequestRequiresItsAdmission(t *testing.T) {
	_, instructions, err := ComposeCodexReviewInstructions(ReviewHostInstructionInput{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := ReviewRequest{
		RunID: "run-1", Round: 1, Repo: "owner/repo", RepositoryID: 42,
		BaseRef: "main", BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40),
		Workspace: "/candidate", Instructions: instructions,
		Admission: testReviewAdmission("run-1"),
		Verification: ReviewVerificationEvidence{
			Outcome:                domain.VerificationPassed,
			RecipeDigest:           domain.Digest("sha256:" + strings.Repeat("c", 64)),
			EvidenceSnapshotDigest: domain.Digest("sha256:" + strings.Repeat("d", 64)),
		},
		RequestedAt: time.Date(2026, 8, 4, 0, 0, 0, 0, time.UTC),
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("admitted request: %v", err)
	}
	for name, tc := range map[string]struct {
		mutate func(*ReviewRequest)
		want   error
	}{
		"no admission": {func(r *ReviewRequest) { r.Admission = domain.ReviewAdmission{} }, domain.ErrEmptyID},
		"another run":  {func(r *ReviewRequest) { r.Admission.RunID = "run-2" }, domain.ErrParentKeyMismatch},
	} {
		refused := request
		tc.mutate(&refused)
		if err := refused.Validate(); !errors.Is(err, tc.want) {
			t.Fatalf("%s: Validate = %v, want %v", name, err, tc.want)
		}
		if _, err := refused.AuthorityDigest(); err == nil {
			t.Fatalf("%s produced an authority digest", name)
		}
	}
}

// TestReviewResultRequiresAWellFormedAdmission: a result returns the
// admission snapshot it reviewed under, so one with no snapshot, such as a
// retained result from before review admission, or with a snapshot no review
// launch could have run, does not validate.
func TestReviewResultRequiresAWellFormedAdmission(t *testing.T) {
	admission := testReviewAdmission("run-1")
	result := ReviewResult{
		InvocationID: admission.InvocationID, BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40),
		Admission:           admission,
		ConfigurationDigest: domain.Digest("sha256:" + strings.Repeat("c", 64)),
		InstructionDigest:   domain.Digest("sha256:" + strings.Repeat("d", 64)),
		CompletedAt:         time.Date(2026, 8, 4, 0, 0, 0, 0, time.UTC),
		CompletionEvidence:  domain.Digest("sha256:" + strings.Repeat("e", 64)),
	}
	if err := result.Validate(); err != nil {
		t.Fatalf("admitted result: %v", err)
	}
	for name, tc := range map[string]struct {
		mutate func(*ReviewResult)
		want   error
	}{
		"no admission":   {func(r *ReviewResult) { r.Admission = domain.ReviewAdmission{} }, domain.ErrEmptyID},
		"no identity":    {func(r *ReviewResult) { r.Admission.AuthIdentityID = "" }, domain.ErrEmptyID},
		"a stage's role": {func(r *ReviewResult) { r.Admission.Role = domain.RoleImplementer }, domain.ErrInvalidRoleName},
	} {
		refused := result
		tc.mutate(&refused)
		if err := refused.Validate(); !errors.Is(err, tc.want) {
			t.Fatalf("%s: Validate = %v, want %v", name, err, tc.want)
		}
	}
}

// TestRetainedReviewRequestIsHeldToTheRuleItWasWrittenUnder: a reader of
// history accepts a request from before review admission, which no source
// would run, and holds a request that carries an admission to the whole rule.
func TestRetainedReviewRequestIsHeldToTheRuleItWasWrittenUnder(t *testing.T) {
	_, instructions, err := ComposeCodexReviewInstructions(ReviewHostInstructionInput{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := ReviewRequest{
		RunID: "run-1", Round: 1, Repo: "owner/repo", RepositoryID: 42,
		BaseRef: "main", BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40),
		Workspace: "/candidate", Instructions: instructions,
		Admission: testReviewAdmission("run-1"),
		Verification: ReviewVerificationEvidence{
			Outcome:                domain.VerificationPassed,
			RecipeDigest:           domain.Digest("sha256:" + strings.Repeat("c", 64)),
			EvidenceSnapshotDigest: domain.Digest("sha256:" + strings.Repeat("d", 64)),
		},
		RequestedAt: time.Date(2026, 8, 4, 0, 0, 0, 0, time.UTC),
	}
	for name, tc := range map[string]struct {
		mutate  func(map[string]any)
		refused bool
	}{
		"admitted":                       {func(map[string]any) {}, false},
		"before admission":               {func(f map[string]any) { delete(f, "admission") }, false},
		"before admission, no workspace": {func(f map[string]any) { delete(f, "admission"); f["workspace"] = "" }, true},
		"an admission for another run":   {func(f map[string]any) { f["admission"].(map[string]any)["run_id"] = "run-2" }, true},
		"an empty admission":             {func(f map[string]any) { f["admission"] = map[string]any{} }, true},
		"an admission under another case": {func(f map[string]any) {
			f["Admission"] = map[string]any{}
			delete(f, "admission")
		}, true},
	} {
		encoded, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]any
		if err := json.Unmarshal(encoded, &fields); err != nil {
			t.Fatal(err)
		}
		tc.mutate(fields)
		body, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		var retained ReviewRequest
		if err := json.Unmarshal(body, &retained); err != nil {
			t.Fatal(err)
		}
		if err := ValidateRetainedReviewRequest(body, retained); (err != nil) != tc.refused {
			t.Fatalf("%s: retained request = %v, want refused %t", name, err, tc.refused)
		}
		// Whatever history accepts, a source runs only an admitted request.
		if name != "admitted" && retained.Validate() == nil {
			t.Fatalf("%s: the request would run", name)
		}
	}
	if err := ValidateRetainedReviewRequest([]byte("not json"), request); err == nil {
		t.Fatal("an undecodable body was accepted")
	}
}

func testReviewAdmission(runID domain.RunID) domain.ReviewAdmission {
	digest := func(fill string) domain.Digest { return domain.Digest("sha256:" + strings.Repeat(fill, 64)) }
	return domain.ReviewAdmission{
		InvocationID: "review-" + domain.InvocationID(runID) + "-1", RunID: runID, Role: domain.RoleReviewer,
		AuthIdentityID: "auth-openai-A", LineageGroup: "openai",
		AgentBinding: domain.AdmissionAgentBinding{
			AgentDigest: digest("a"), LaunchDigest: digest("b"), TreatmentDigest: digest("d"),
			PricingRevision: "2026-08", LineupRevision: digest("c"),
			EnrollmentID: "enroll-openai-A-codex", EnrollmentGeneration: 2,
			StoreManifestDigest: digest("9"), EffectiveEgress: []string{"chatgpt.com:443"},
			RouteModelID: "gpt-5.2-codex", RequestedEffort: domain.EffortHigh, NativeEffort: "high",
			SelectionSource: domain.AgentSelectionSourceLineup,
		},
		AdmittedAt: time.Date(2026, 8, 3, 23, 59, 0, 0, time.UTC),
	}
}

func TestReviewRequestEvaluatedCommit(t *testing.T) {
	_, instructions, err := ComposeCodexReviewInstructions(ReviewHostInstructionInput{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	base, head, merge := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	request := ReviewRequest{
		RunID: "run-1", Round: 1, Repo: "owner/repo", RepositoryID: 42,
		BaseRef: "main", BaseSHA: base, HeadSHA: head,
		Workspace: "/candidate", Instructions: instructions,
		Admission: testReviewAdmission("run-1"),
		Verification: ReviewVerificationEvidence{
			Outcome:                domain.VerificationPassed,
			RecipeDigest:           domain.Digest("sha256:" + strings.Repeat("c", 64)),
			EvidenceSnapshotDigest: domain.Digest("sha256:" + strings.Repeat("d", 64)),
		},
		RequestedAt: time.Date(2026, 8, 4, 0, 0, 0, 0, time.UTC),
	}
	if got := request.WorkspaceSHA(); got != head {
		t.Fatalf("WorkspaceSHA without an evaluated commit = %q, want the head", got)
	}
	request.EvaluatedSHA = merge
	if err := request.Validate(); err != nil {
		t.Fatalf("request naming a distinct evaluated commit: %v", err)
	}
	if got := request.WorkspaceSHA(); got != merge {
		t.Fatalf("WorkspaceSHA with an evaluated commit = %q, want it", got)
	}
	// An evaluated commit equal to either parent is not their merge, so the
	// request and its authority digest both fail closed.
	for name, evaluated := range map[string]string{"head": head, "base": base} {
		refused := request
		refused.EvaluatedSHA = evaluated
		if err := refused.Validate(); !errors.Is(err, domain.ErrProspectiveMergeIdentityInconsistent) {
			t.Fatalf("evaluated commit equal to the %s: %v, want ErrProspectiveMergeIdentityInconsistent", name, err)
		}
		if _, err := refused.AuthorityDigest(); err == nil {
			t.Fatalf("evaluated commit equal to the %s produced an authority digest", name)
		}
	}
}

func assertReviewInstructionBlocksIsolated(t *testing.T, bundle []byte, wantBlocks int) {
	t.Helper()
	insideFence := false
	wantFence := false
	wantEnd := false
	fenceLength := 0
	blocks := 0
	for lineNumber, line := range strings.Split(string(bundle), "\n") {
		generatedHeading := line == "# Freeside Explicit Codex Review Instruction Bundle" ||
			line == "## Trusted-Base Repository Instructions" ||
			line == "## Operator-Host Instructions" || strings.HasPrefix(line, "### Scope ")
		begin := strings.HasPrefix(line, "--- BEGIN ")
		end := strings.HasPrefix(line, "--- END ")
		if (generatedHeading || begin || end) && insideFence {
			t.Fatalf("generated boundary at line %d was captured inside a fence: %q", lineNumber+1, line)
		}
		if begin {
			if wantFence || wantEnd {
				t.Fatalf("nested BEGIN boundary at line %d", lineNumber+1)
			}
			wantFence = true
			blocks++
			continue
		}
		run := testBacktickFenceRun(line)
		switch {
		case wantFence:
			if run < 3 {
				t.Fatalf("instruction body at line %d did not start with a wrapper fence", lineNumber+1)
			}
			insideFence = true
			wantFence = false
			fenceLength = run
			continue
		case insideFence && run >= fenceLength:
			insideFence = false
			wantEnd = true
			fenceLength = 0
			continue
		}
		if end {
			if !wantEnd {
				t.Fatalf("END boundary at line %d did not follow a closed wrapper", lineNumber+1)
			}
			wantEnd = false
			continue
		}
		if wantEnd {
			t.Fatalf("line %d intervened between wrapper and END boundary: %q", lineNumber+1, line)
		}
	}
	if insideFence || wantFence || wantEnd {
		t.Fatalf("unterminated generated block: inside=%v wantFence=%v wantEnd=%v", insideFence, wantFence, wantEnd)
	}
	if blocks != wantBlocks {
		t.Fatalf("generated blocks = %d, want %d", blocks, wantBlocks)
	}
}

func testBacktickFenceRun(line string) int {
	if len(line) < 3 || strings.Trim(line, "`") != "" {
		return 0
	}
	return len(line)
}
