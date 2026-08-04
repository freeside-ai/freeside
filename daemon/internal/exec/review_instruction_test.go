package exec

import (
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
