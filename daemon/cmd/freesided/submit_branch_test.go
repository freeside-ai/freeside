package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/engine"
)

func TestSubmitDeclaredBranchPersistsAndChangesRunIdentity(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	work, policy, publicationPath := writeSubmissionInputs(t, root)
	cfg := submitCommandConfig{DBPath: filepath.Join(root, "freeside.db"), WorkItemPath: work, PolicyPath: policy, PublicationPath: publicationPath, ProjectID: "proj-submit"}
	initial, err := runSubmitCommand(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	input, err := readSubmissionFile(publicationPath)
	if err != nil {
		t.Fatal(err)
	}
	var publication engine.ProductionPublication
	if err := json.Unmarshal(input.body, &publication); err != nil {
		t.Fatal(err)
	}
	publication.Branch = "feat/meaningful-task"
	body, err := json.Marshal(publication)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(publicationPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	declared, err := runSubmitCommand(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if initial.RunID == declared.RunID || initial.PublicationDigest == declared.PublicationDigest {
		t.Fatal("branch did not change submission identity")
	}
	retry, err := runSubmitCommand(t.Context(), cfg)
	if err != nil || retry.RunID != declared.RunID {
		t.Fatalf("retry = %+v, %v", retry, err)
	}
}
