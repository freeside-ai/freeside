package fakepublication

import (
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

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
