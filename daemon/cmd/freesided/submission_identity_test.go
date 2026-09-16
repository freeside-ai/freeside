package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func TestSubmissionIdentityNewWorkAndSavedManualRetry(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	task, policy, publication := writeSubmissionInputs(t, root)
	cfg := submitCommandConfig{
		DBPath: filepath.Join(root, "state.db"), TaskPath: task, PolicyPath: policy,
		PublicationPath: publication, ProjectID: "project-submit",
	}
	first, err := runSubmitCommand(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	second, err := runSubmitCommand(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if first.SubmissionID == second.SubmissionID || first.RunID == second.RunID ||
		first.SpecificationRunID == second.SpecificationRunID || first.CampaignID == second.CampaignID {
		t.Fatal("new work reused identity")
	}
	// The original files can disappear after a lost stdout response. Retry
	// must use the retained inputs, including the original publication bytes.
	if err := os.Remove(task); err != nil {
		t.Fatal(err)
	}
	replayed, err := runSubmitCommand(t.Context(), submitCommandConfig{DBPath: cfg.DBPath, RetrySubmissionID: first.SubmissionID})
	if err != nil {
		t.Fatal(err)
	}
	if replayed != first {
		t.Fatal("saved manual retry changed original result")
	}
	if _, err := runSubmitCommand(t.Context(), submitCommandConfig{
		DBPath: cfg.DBPath, RetrySubmissionID: first.SubmissionID, RequireComposition: true,
	}); err == nil {
		t.Fatal("retry ignored a new composition requirement")
	}
}

func TestSubmissionIdentityRetainedBeforeFirstCommit(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	task, policy, publication := writeSubmissionInputs(t, root)
	cfg := submitCommandConfig{
		DBPath: filepath.Join(root, "state.db"), TaskPath: task, PolicyPath: policy,
		PublicationPath: publication, ProjectID: "project-submit", SubmissionID: "prepared-before-crash",
	}
	prepared, err := prepareSubmission(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := retainSubmission(prepared); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cfg.DBPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("retention unexpectedly opened the database")
	}
	first, err := runSubmitCommand(t.Context(), submitCommandConfig{DBPath: cfg.DBPath, RetrySubmissionID: cfg.SubmissionID})
	if err != nil {
		t.Fatal(err)
	}
	// Equivalent JSON formatting is the same request and keeps original output.
	body, err := os.ReadFile(publication) //nolint:gosec // fixture created under t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, body, "", "  "); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(publication, pretty.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	replayed, err := runSubmitCommand(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if replayed != first {
		t.Fatal("formatting changed result")
	}
	cfg.ProjectID = "other-project"
	if _, err := runSubmitCommand(t.Context(), cfg); !errors.Is(err, store.ErrImmutableConflict) {
		t.Fatalf("changed request = %v", err)
	}
}

func TestSubmissionIdentityInvalidInputsDoNotOccupyPreparedIdentity(t *testing.T) {
	t.Parallel()
	for _, invalid := range []string{
		"missing-project", "missing-task", "publication", "policy", "work-unit", "composition",
		"specification-policy", "work-unit-fields",
	} {
		t.Run(invalid, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			task, policy, publication := writeSubmissionInputs(t, root)
			cfg := submitCommandConfig{
				DBPath: filepath.Join(root, "state.db"), TaskPath: task, PolicyPath: policy,
				PublicationPath: publication, ProjectID: "project-submit", SubmissionID: "correctable",
			}
			bad := cfg
			switch invalid {
			case "missing-project":
				bad.ProjectID = ""
			case "missing-task":
				bad.TaskPath = ""
			default:
				path := filepath.Join(root, "invalid.json")
				body := []byte(`{"incomplete":`)
				if invalid == "work-unit-fields" {
					body = []byte(`{"completion_criterion":"unknown"}`)
				}
				if invalid == "specification-policy" {
					original, err := os.ReadFile(policy) //nolint:gosec // fixture created under t.TempDir
					if err != nil {
						t.Fatal(err)
					}
					body = bytes.Replace(original, []byte(`"specification.max_iterations","value":"4"`),
						[]byte(`"specification.max_iterations","value":"0"`), 1)
				}
				if err := os.WriteFile(path, body, 0o600); err != nil { //nolint:gosec // path is a fixed filename beneath t.TempDir; policy-file bytes affect only the contents
					t.Fatal(err)
				}
				switch invalid {
				case "publication":
					bad.PublicationPath = path
				case "policy", "specification-policy":
					bad.PolicyPath = path
				case "work-unit", "work-unit-fields":
					bad.WorkUnitPath = path
				case "composition":
					bad.CompositionPath = path
				}
			}
			if _, err := runSubmitCommand(t.Context(), bad); err == nil {
				t.Fatal("invalid request was accepted")
			}
			for _, path := range []string{cfg.DBPath, submissionJournalPath(cfg.DBPath, cfg.SubmissionID)} {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("invalid request created %s: %v", path, err)
				}
			}
			if _, err := runSubmitCommand(t.Context(), cfg); err != nil {
				t.Fatalf("corrected request could not use prepared identity: %v", err)
			}
		})
	}
}

func TestSubmissionIdentityJournalFailureDoesNotOpenDatabase(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	task, policy, publication := writeSubmissionInputs(t, root)
	cfg := submitCommandConfig{
		DBPath: filepath.Join(root, "state.db"), TaskPath: task, PolicyPath: policy,
		PublicationPath: publication, ProjectID: "project-submit", SubmissionID: "save-failed",
	}
	if err := os.WriteFile(cfg.DBPath+".submissions", []byte("blocked directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runSubmitCommand(t.Context(), cfg); err == nil {
		t.Fatal("submission continued without its recovery journal")
	}
	if _, err := os.Stat(cfg.DBPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal failure opened the database: %v", err)
	}
}

func TestSubmissionIdentityReplaysAcceptedInvalidUTF8(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	task, policy, publication := writeSubmissionInputs(t, root)
	body := []byte(`{"title":"Test TITLE task","body":"Closes #123.","commit_author":{"app_slug":"freeside-test","bot_user_id":12345}}`)
	body = bytes.Replace(body, []byte("TITLE"), []byte{0xff}, 1)
	if err := os.WriteFile(publication, body, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := submitCommandConfig{
		DBPath: filepath.Join(root, "state.db"), TaskPath: task, PolicyPath: policy,
		PublicationPath: publication, ProjectID: "project-submit", SubmissionID: "accepted-utf8",
	}
	first, err := runSubmitCommand(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := runSubmitCommand(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if replayed != first {
		t.Fatal("identical accepted input changed replay result")
	}
}
