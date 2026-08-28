package fakepublication

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func TestValidateCandidateBodyReservesPublisherOwnedSections(t *testing.T) {
	t.Parallel()
	want := maxPullRequestBodyBytes - 3*len("\n\n") - identityMarkerBytes -
		maxRenderedAdvisoriesBytes - minRenderedDispositionHistoryBytes
	if maxCandidateBodyBytes != want {
		t.Fatalf("candidate body budget = %d, want derived %d", maxCandidateBodyBytes, want)
	}
	if err := ValidateCandidateBody(strings.Repeat("x", maxCandidateBodyBytes)); err != nil {
		t.Fatalf("exact candidate body budget: %v", err)
	}
	if err := ValidateCandidateBody(strings.Repeat("x", maxCandidateBodyBytes+1)); err == nil {
		t.Fatal("candidate body consumed a publisher-owned section reserve")
	}

	task := Task{
		Version: TaskVersion, RunID: "run-body-budget", ProjectID: "project-body-budget",
		StoreEpoch: "epoch-body-budget", WorkspaceDir: "/workspace", HandoffDir: "/handoff",
		HandoffDigest: "sha256:" + domain.Digest(strings.Repeat("a", 64)),
		Repo:          "freeside-ai/repo", BaseRef: "main", BaseSHA: strings.Repeat("b", 40),
		AllowedPaths: []string{"src/**"}, RecipeDigest: "sha256:recipe", RecipePath: "recipe.yaml",
		TrustProfileDigest: "sha256:profile", VerificationInvocationID: "verify-body-budget",
		PublicationInvocationID: "publish-body-budget", Title: "Budget boundary", Body: strings.Repeat("x", maxCandidateBodyBytes),
		CommitDate: time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC),
		StartedAt:  time.Date(2026, 8, 28, 12, 1, 0, 0, time.UTC), OperatingMode: OperatingModeAttended,
	}
	payload, err := EncodeTask(task)
	if err != nil {
		t.Fatalf("encode exact-limit task: %v", err)
	}
	if _, err := DecodeTask(payload); err != nil {
		t.Fatalf("decode exact-limit task: %v", err)
	}
	task.Body += "x"
	payload, err = json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeTask(payload); err == nil {
		t.Fatal("decoded persisted task whose body consumed a publisher-owned section reserve")
	}
}

func TestTerminalDigestIgnoresAttentionCreationTime(t *testing.T) {
	first := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	second := first.Add(time.Hour)
	item := domain.AttentionItem{
		ID:        "item-terminal",
		ProjectID: "project-terminal",
		Reason:    "published and ready",
		CreatedAt: &first,
	}

	firstDigest, err := TerminalDigest(Task{RunID: "run-terminal"}, item)
	if err != nil {
		t.Fatal(err)
	}
	item.CreatedAt = &second
	secondDigest, err := TerminalDigest(Task{RunID: "run-terminal"}, item)
	if err != nil {
		t.Fatal(err)
	}
	if firstDigest != secondDigest {
		t.Fatalf("creation time changed terminal digest: %q != %q", firstDigest, secondDigest)
	}

	item.Reason = "different terminal fact"
	changedDigest, err := TerminalDigest(Task{RunID: "run-terminal"}, item)
	if err != nil {
		t.Fatal(err)
	}
	if changedDigest == firstDigest {
		t.Fatal("terminal digest ignored a derived publication fact")
	}
}
