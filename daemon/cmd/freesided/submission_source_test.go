package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

const (
	sourceTestIssue = "https://github.com/example/project/issues/82"
	// The record a client-composed task saves, written by hand for a CLI task.
	declaredSourcePublication = `{"recipe":"freeside.client-publication/v2","source_issue":"` + sourceTestIssue +
		`","commit_author":{"app_slug":"freeside-test","bot_user_id":12345}}`
	undeclaredSourcePublication = `{"recipe":"freeside.client-publication/v2","commit_author":{"app_slug":"freeside-test","bot_user_id":12345}}`
	literalSourcePublication    = `{"title":"Test the task","body":"## Why\n\nReviewer context.\n","commit_author":{"app_slug":"freeside-test","bot_user_id":12345}}`
)

type submissionSourceCase struct {
	name, task, publication string
	// wantIssue and wantOmitted are the accepted result; both empty means the
	// submission is refused.
	wantIssue, wantOmitted string
}

func (tc submissionSourceCase) refused() bool { return tc.wantIssue == "" && tc.wantOmitted == "" }

func submissionSourceCases() []submissionSourceCase {
	return []submissionSourceCase{
		{"declared source", sourceTestIssue + "\n", declaredSourcePublication, sourceTestIssue, ""},
		{"prose task with literal text", "# Task\n\nImplement the thing.\n", literalSourcePublication, "", sourceOmittedNotAnIssue},
		{"prose task with client record", "# Task\n\nImplement the thing.\n", undeclaredSourcePublication, "", sourceOmittedNotAnIssue},
		{"issue task with literal text", sourceTestIssue + "\n", literalSourcePublication, "", ""},
		{"issue task with undeclared source", sourceTestIssue, undeclaredSourcePublication, "", ""},
		{"prose-wrapped issue with declared source", "Please handle " + sourceTestIssue + "\n", declaredSourcePublication, "", ""},
		{"issue task declaring another issue", "https://github.com/example/project/issues/83\n", declaredSourcePublication, "", ""},
	}
}

func writeSubmissionSource(t *testing.T, root string, tc submissionSourceCase) (taskPath, policyPath, publicationPath string) {
	t.Helper()
	taskPath, policyPath, publicationPath = writeSubmissionInputs(t, root)
	for path, body := range map[string]string{taskPath: tc.task, publicationPath: tc.publication} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return taskPath, policyPath, publicationPath
}

func checkSourceRefusal(t *testing.T, err error) {
	t.Helper()
	var refusal sourceReferenceError
	if !errors.As(err, &refusal) || !strings.Contains(err.Error(), "source_issue") {
		t.Fatalf("error = %v, want a source reference refusal naming source_issue", err)
	}
	// The refusal is printed into the composition manifest, so it must not
	// repeat the submitted source.
	if strings.Contains(err.Error(), "example/project") {
		t.Fatalf("refusal repeats submitted input: %v", err)
	}
}

func TestSubmitCommandSourceReference(t *testing.T) {
	t.Parallel()
	for _, tc := range submissionSourceCases() {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			task, policy, publication := writeSubmissionSource(t, root, tc)
			cfg := submitCommandConfig{
				DBPath: filepath.Join(root, "state.db"), SubmissionID: "source", ProjectID: "proj-source",
				TaskPath: task, PolicyPath: policy, PublicationPath: publication,
			}
			result, err := runSubmitCommand(t.Context(), cfg)
			if tc.refused() {
				checkSourceRefusal(t, err)
				for _, path := range []string{cfg.DBPath, cfg.DBPath + ".blobs", cfg.DBPath + ".submissions"} {
					if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("refused submission left %s: %v", path, err)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("submit = %v", err)
			}
			if result.SourceIssue != tc.wantIssue || result.SourceReferenceOmitted != tc.wantOmitted {
				t.Fatalf("result source = %q, omitted = %q; want %q, %q",
					result.SourceIssue, result.SourceReferenceOmitted, tc.wantIssue, tc.wantOmitted)
			}
			// The saved record is the operator's file and nothing else: its
			// digest, which joins the run identity, is the file's own.
			var written engine.ProductionPublication
			if err := json.Unmarshal([]byte(tc.publication), &written); err != nil {
				t.Fatal(err)
			}
			canonical, err := json.Marshal(written)
			if err != nil {
				t.Fatal(err)
			}
			if result.PublicationDigest != submissionBytes(canonical).digest {
				t.Fatalf("saved publication digest = %s, want the file's own", result.PublicationDigest)
			}
			encoded, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			wantField, absentField := `"source_issue":`, `"source_reference_omitted":`
			if tc.wantIssue == "" {
				wantField, absentField = absentField, wantField
			}
			if !bytes.Contains(encoded, []byte(wantField)) || bytes.Contains(encoded, []byte(absentField)) {
				t.Fatalf("result JSON = %s, want %s and no %s", encoded, wantField, absentField)
			}
			replayed, err := runSubmitCommand(t.Context(), cfg)
			if err != nil || replayed != result {
				t.Fatalf("repeated submit = %+v, %v; want the first result", replayed, err)
			}
		})
	}
}

// sourceApplyRequest is the request a CLI prepares for one task and
// publication, with every digest and derived identity bound, so a refusal
// exercises the source rule and not a binding error.
func sourceApplyRequest(t *testing.T, tc submissionSourceCase) submitApplyRequest {
	t.Helper()
	req := egressApplyRequest(t, nil)
	req.Publication = mustDecodePublication(t, tc.publication)
	publicationBody, err := json.Marshal(req.Publication)
	if err != nil {
		t.Fatal(err)
	}
	spec := submissionBytes([]byte(tc.task))
	req.SubmissionID, req.SpecBody, req.SpecDigest = "source", spec.body, spec.digest
	req.PublicationDigest = submissionBytes(publicationBody).digest
	req.PublicationBodyDigest = req.PublicationDigest
	req.ImplementationRunID = engine.ManualSubmissionRunID(
		"cli:"+req.SubmissionID, req.ProjectID, req.SpecDigest, req.PolicyDigest, req.PublicationDigest, "")
	if req.SpecificationRunID, err = engine.SpecificationRunIDForImplementation(req.ImplementationRunID); err != nil {
		t.Fatal(err)
	}
	if req.CampaignID, err = engine.ProductionCampaignIDForImplementation(req.ImplementationRunID); err != nil {
		t.Fatal(err)
	}
	if req.ResolvedPolicy, err = domain.NewResolvedPolicy(req.SpecificationRunID, req.Keys); err != nil {
		t.Fatal(err)
	}
	if err := validateSubmitApply(req); err != nil {
		t.Fatalf("structural request validation = %v", err)
	}
	return req
}

func assertNoSubmittedWork(t *testing.T, st *store.Store, req submitApplyRequest) {
	t.Helper()
	if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
		for _, runID := range []domain.RunID{req.ImplementationRunID, req.SpecificationRunID} {
			if _, err := tx.GetRun(t.Context(), runID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("run %s = %v, want absent", runID, err)
			}
		}
		if _, err := tx.GetManualSubmission(t.Context(), "cli:"+req.SubmissionID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("manual submission = %v, want absent", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// A CLI that predates the rule prepares a request the daemon must still
// refuse: the check runs on both sides of the control socket.
func TestSubmitApplySourceReference(t *testing.T) {
	t.Parallel()
	for _, tc := range submissionSourceCases() {
		t.Run(tc.name, func(t *testing.T) {
			req := sourceApplyRequest(t, tc)
			st := storetest.Open(t, filepath.Join(t.TempDir(), "state.db"), store.Options{})
			blobs, err := signet.NewBlobStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			result, err := applySubmission(t.Context(), st, blobs, req)
			if !tc.refused() {
				if err != nil || result.SourceIssue != tc.wantIssue || result.SourceReferenceOmitted != tc.wantOmitted {
					t.Fatalf("apply = %+v, %v; want source %q, omitted %q", result, err, tc.wantIssue, tc.wantOmitted)
				}
				return
			}
			checkSourceRefusal(t, err)
			assertNoSubmittedWork(t, st, req)
			if found, err := blobs.Has(req.SpecDigest); err != nil || found {
				t.Fatalf("refused submission stored its task: found=%t err=%v", found, err)
			}
		})
	}
}

func refusedSourceCases() []submissionSourceCase {
	var refused []submissionSourceCase
	for _, tc := range submissionSourceCases() {
		if tc.refused() {
			refused = append(refused, tc)
		}
	}
	return refused
}

// retainHistoricalSource saves the journal a CLI wrote before the rule
// existed, for a submission the rule would now refuse as new.
func retainHistoricalSource(t *testing.T, tc submissionSourceCase) (submitCommandConfig, submitApplyRequest) {
	t.Helper()
	req := sourceApplyRequest(t, tc)
	root := t.TempDir()
	paths := map[string]string{
		"task": filepath.Join(root, "task.md"), "policy": filepath.Join(root, "policy.json"),
		"publication": filepath.Join(root, "publication.json"),
	}
	for role, body := range map[string][]byte{"task": req.SpecBody, "policy": req.PolicyBody, "publication": []byte(tc.publication)} {
		if err := os.WriteFile(paths[role], body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := submitCommandConfig{
		DBPath: filepath.Join(root, "state.db"), SubmissionID: req.SubmissionID, ProjectID: req.ProjectID,
		TaskPath: paths["task"], PolicyPath: paths["policy"], PublicationPath: paths["publication"],
	}
	prepared, err := prepareSubmission(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := retainSubmission(prepared); err != nil {
		t.Fatal(err)
	}
	return cfg, req
}

func TestHistoricalSourceSubmissionStillReplays(t *testing.T) {
	t.Parallel()
	for _, tc := range refusedSourceCases() {
		t.Run(tc.name, func(t *testing.T) {
			cfg, req := retainHistoricalSource(t, tc)
			if _, err := loadOrCreateTopicKey(cfg.DBPath, false); err != nil {
				t.Fatal(err)
			}
			st := storetest.Open(t, cfg.DBPath, store.Options{})
			blobs, err := signet.NewBlobStore(cfg.DBPath + ".blobs")
			if err != nil {
				t.Fatal(err)
			}
			seedHistoricalManualSubmission(t, st, blobs, req)
			// The stored record is unchanged, so the result says what it does:
			// name the issue it declared, or none.
			wantIssue, wantOmitted := req.Publication.SourceIssue, ""
			if wantIssue == "" {
				wantOmitted = sourceOmittedBeforeRule
			}
			check := func(name string, replayed submitResult, err error) {
				t.Helper()
				if err != nil {
					t.Fatalf("%s = %v", name, err)
				}
				if replayed.ImplementationRunID != req.ImplementationRunID || replayed.SpecificationRunID != req.SpecificationRunID ||
					replayed.SourceIssue != wantIssue || replayed.SourceReferenceOmitted != wantOmitted {
					t.Fatalf("%s = %+v, want the seeded run reporting source %q, omitted %q", name, replayed, wantIssue, wantOmitted)
				}
			}
			replayed, err := runSubmitCommand(t.Context(), submitCommandConfig{DBPath: cfg.DBPath, RetrySubmissionID: cfg.SubmissionID})
			check("manual retry", replayed, err)
			replayed, err = runSubmitCommand(t.Context(), cfg)
			check("repeated identity", replayed, err)
			legacy := req
			legacy.LegacyRunID, legacy.SubmissionID = req.ImplementationRunID, ""
			replayed, err = applySubmission(t.Context(), st, blobs, legacy)
			check("legacy run lookup", replayed, err)
		})
	}
}

// A journal alone is not acceptance: one that never reached the database is
// a new submission, and the rule refuses it without creating work.
func TestRetainedSourceSubmissionDoesNotCreateWork(t *testing.T) {
	t.Parallel()
	for _, tc := range refusedSourceCases() {
		t.Run(tc.name, func(t *testing.T) {
			cfg, req := retainHistoricalSource(t, tc)
			_, err := runSubmitCommand(t.Context(), submitCommandConfig{DBPath: cfg.DBPath, RetrySubmissionID: cfg.SubmissionID})
			checkSourceRefusal(t, err)
			assertNoSubmittedWork(t, storetest.Open(t, cfg.DBPath, store.Options{}), req)
		})
	}
}

func runSourcePreflight(t *testing.T, args []string, env *fakePreflightEnvironment) (compositionManifest, error) {
	t.Helper()
	var stdout bytes.Buffer
	err := runPreflightCommandWithEnvironment(t.Context(), args, &stdout, &bytes.Buffer{}, env,
		time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC), "2dce6570ee23")
	var manifest compositionManifest
	if decodeErr := json.Unmarshal(stdout.Bytes(), &manifest); decodeErr != nil {
		t.Fatal(decodeErr)
	}
	return manifest, err
}

func sourceIdentityCheck(t *testing.T, manifest compositionManifest) compositionCheck {
	t.Helper()
	for _, check := range manifest.Checks {
		if check.Name == "source_implementation_identity" {
			return check
		}
	}
	t.Fatal("manifest has no source_implementation_identity check")
	return compositionCheck{}
}

func TestPreflightSourceReference(t *testing.T) {
	t.Parallel()
	for _, tc := range submissionSourceCases() {
		t.Run(tc.name, func(t *testing.T) {
			args, env := preflightFixture(t)
			args = append(args, "-submission-id", "source")
			cfg, err := parsePreflightConfig(args, &bytes.Buffer{})
			if err != nil {
				t.Fatal(err)
			}
			for path, body := range map[string]string{cfg.TaskPath: tc.task, cfg.PublicationPath: tc.publication} {
				if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			manifest, err := runSourcePreflight(t, args, env)
			check := sourceIdentityCheck(t, manifest)
			if !tc.refused() {
				if err != nil || check.Status != compositionPassed {
					t.Fatalf("preflight = %v, identity check %s, want passed", err, check.Status)
				}
				return
			}
			if !errors.Is(err, errCompositionPreflight) || manifest.Status != compositionFailed || check.Status != compositionFailed {
				t.Fatalf("preflight = %v, status %s, identity check %s; want a failed manifest", err, manifest.Status, check.Status)
			}
			// The operator reads the manifest, so the check carries the same
			// message submit prints.
			_, submitErr := submittedSourceReference([]byte(tc.task), mustDecodePublication(t, tc.publication))
			if submitErr == nil || check.Evidence != submitErr.Error() {
				t.Fatalf("identity check evidence = %q, want submit's refusal %v", check.Evidence, submitErr)
			}
			// With no submission identity preflight only inspects a retained
			// legacy run, which the rule does not reach.
			legacy, err := parsePreflightConfig(args[:len(args)-2], &bytes.Buffer{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := inspectCompositionIdentity(t.Context(), legacy); err != nil {
				t.Fatalf("legacy inspection = %v", err)
			}
		})
	}
}

func mustDecodePublication(t *testing.T, body string) engine.ProductionPublication {
	t.Helper()
	var publication engine.ProductionPublication
	if err := json.Unmarshal([]byte(body), &publication); err != nil {
		t.Fatal(err)
	}
	return publication
}

// run-real-work.sh --resume-session feeds a finished session's retained
// inputs back through preflight. A session that submitted before the rule
// must still resume, and only with the inputs its journal saved.
func TestPreflightResumesRetainedSourceSubmission(t *testing.T) {
	t.Parallel()
	for _, tc := range refusedSourceCases() {
		t.Run(tc.name, func(t *testing.T) {
			saved, _ := retainHistoricalSource(t, tc)
			args, env := preflightFixture(t)
			args = append(args, "-submission-id", saved.SubmissionID)
			cfg, err := parsePreflightConfig(args, &bytes.Buffer{})
			if err != nil {
				t.Fatal(err)
			}
			for path, body := range map[string]string{cfg.TaskPath: tc.task, cfg.PublicationPath: tc.publication} {
				if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			// The authenticated rig names the database whose journal counts.
			env.rig.Resources.DatabasePath = saved.DBPath
			manifest, err := runSourcePreflight(t, args, env)
			if check := sourceIdentityCheck(t, manifest); err != nil || check.Status != compositionPassed {
				t.Fatalf("resumed preflight = %v, identity check %s (%s), want passed", err, check.Status, check.Evidence)
			}

			// The same identity with a different task is not that submission.
			if err := os.WriteFile(cfg.TaskPath, []byte("https://github.com/example/project/issues/84\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			manifest, err = runSourcePreflight(t, args, env)
			if check := sourceIdentityCheck(t, manifest); !errors.Is(err, errCompositionPreflight) || check.Status != compositionFailed {
				t.Fatalf("preflight with a changed task = %v, identity check %s, want failed", err, check.Status)
			}

			// Without the rig there is no database path to find the journal
			// under, so the saved inputs fail too; the remediation says the
			// refusal may be the missing rig's.
			if err := os.WriteFile(cfg.TaskPath, []byte(tc.task), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(cfg.RigTokenFile); err != nil {
				t.Fatal(err)
			}
			manifest, err = runSourcePreflight(t, args, env)
			if check := sourceIdentityCheck(t, manifest); !errors.Is(err, errCompositionPreflight) || check.Status != compositionFailed ||
				!strings.Contains(check.Remediation, "once the rig is available") {
				t.Fatalf("preflight without a rig = %v, identity check %+v, want failed with the rig named", err, check)
			}
		})
	}
}
