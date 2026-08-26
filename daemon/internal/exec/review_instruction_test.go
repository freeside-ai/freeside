package exec

import (
	"bytes"
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
		func(r *ReviewRequest) {
			r.Instructions.RepositorySources[0].Digest = domain.Digest("sha256:" + strings.Repeat("f", 64))
		},
		func(r *ReviewRequest) {
			r.Instructions.ResultDigest = domain.Digest("sha256:" + strings.Repeat("1", 64))
		},
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
