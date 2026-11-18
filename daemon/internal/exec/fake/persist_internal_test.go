package fake

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
)

func TestMakeStateDirectorySyncsEveryPublishedEntryAndRetry(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "one", "two")
	var synced []string
	if err := makeStateDirectoryWithSync(target, func(path string) error {
		synced = append(synced, path)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Dir(root), root, filepath.Join(root, "one")}
	if !slices.Equal(synced, want) {
		t.Fatalf("synced directories = %v, want %v", synced, want)
	}

	synced = nil
	if err := makeStateDirectoryWithSync(target, func(path string) error {
		synced = append(synced, path)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want = []string{filepath.Dir(target)}
	if !slices.Equal(synced, want) {
		t.Fatalf("retry synced directories = %v, want %v", synced, want)
	}
}

// TestLoadReviewStateRefusesAnUnknownField: the review fake's state file
// decodes strictly, so an intent carrying a field the current request does
// not declare fails the load instead of reading as a current request.
func TestLoadReviewStateRefusesAnUnknownField(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := atomicWrite(dir, reviewStateFile, reviewState{
		Intents: map[domain.InvocationID]exec.ReviewRequest{"review-run-1-1": {RunID: "run-1", Round: 1}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := loadReviewState(dir); err != nil {
		t.Fatalf("current state: %v", err)
	}
	path := filepath.Join(dir, reviewStateFile)
	var file map[string]map[string]map[string]any
	body, err := os.ReadFile(path) //nolint:gosec // G304: test-owned temporary path
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body, &file); err != nil {
		t.Fatal(err)
	}
	file["intents"]["review-run-1-1"]["retired_field"] = "value"
	if body, err = json.Marshal(file); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadReviewState(dir); err == nil || !strings.Contains(err.Error(), "retired_field") {
		t.Fatalf("state with an unknown intent field = %v, want a refusal naming it", err)
	}
}
