package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

// undeclaredSubmission is a task and publication the source rule accepts, so
// a refusal in these tests is the declaration rule's alone.
var undeclaredSubmission = submissionSourceCase{
	name: "undeclared", task: "# Task\n\nImplement the thing.\n", publication: literalSourcePublication,
	wantOmitted: sourceOmittedNotAnIssue,
}

func checkWorkUnitRefusal(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, errWorkUnitRequired) {
		t.Fatalf("error = %v, want the work-unit declaration refusal", err)
	}
	// The operator fixes the submission from this text alone, so it names
	// the flag and the smallest file that satisfies it.
	for _, want := range []string{"--work-unit", smallestWorkUnit} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal %q does not name %s", err, want)
		}
	}
}

func TestSubmitCommandRequiresWorkUnit(t *testing.T) {
	t.Parallel()
	for name, submissionID := range map[string]string{"generated identity": "", "prepared identity": "undeclared"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			task, policy, publication := writeSubmissionInputs(t, root)
			cfg := submitCommandConfig{
				DBPath: filepath.Join(root, "state.db"), SubmissionID: submissionID, ProjectID: "proj-undeclared",
				TaskPath: task, PolicyPath: policy, PublicationPath: publication,
			}
			_, err := runSubmitCommand(t.Context(), cfg)
			checkWorkUnitRefusal(t, err)
			// Refused before anything is saved: no database, no stored
			// input, and no journal a retry could pick up.
			for _, path := range []string{cfg.DBPath, cfg.DBPath + ".blobs", cfg.DBPath + ".submissions"} {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("refused submission left %s: %v", path, err)
				}
			}
			cfg.WorkUnitPath = declaredWorkUnit(t)
			declared, err := runSubmitCommand(t.Context(), cfg)
			if err != nil || declared.WorkUnitID != domain.WorkUnitIDForRun(declared.RunID) {
				t.Fatalf("declared submit = %+v, %v; want the run's work unit", declared, err)
			}
		})
	}
}

// A CLI that predates the rule prepares an undeclared request the daemon must
// still refuse: the check runs on both sides of the control socket.
func TestSubmitApplyRequiresWorkUnit(t *testing.T) {
	t.Parallel()
	req := sourceApplyRequest(t, undeclaredSubmission)
	st := storetest.Open(t, filepath.Join(t.TempDir(), "state.db"), store.Options{})
	blobs, err := signet.NewBlobStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, err = applySubmission(t.Context(), st, blobs, req)
	checkWorkUnitRefusal(t, err)
	assertNoSubmittedWork(t, st, req)
	if found, err := blobs.Has(req.SpecDigest); err != nil || found {
		t.Fatalf("refused submission stored its task: found=%t err=%v", found, err)
	}
	declared := declareApplyRequest(t, req)
	result, err := applySubmission(t.Context(), st, blobs, declared)
	if err != nil || result.WorkUnitID != domain.WorkUnitIDForRun(declared.ImplementationRunID) {
		t.Fatalf("declared apply = %+v, %v; want the run's work unit", result, err)
	}
}

// A submission the database accepted before the rule is not new work: every
// way of reaching it again returns the run it created, still undeclared.
func TestHistoricalUndeclaredSubmissionStillReplays(t *testing.T) {
	t.Parallel()
	cfg, req := retainHistoricalSource(t, undeclaredSubmission)
	if _, err := loadOrCreateTopicKey(cfg.DBPath, false); err != nil {
		t.Fatal(err)
	}
	st := storetest.Open(t, cfg.DBPath, store.Options{})
	blobs, err := signet.NewBlobStore(cfg.DBPath + ".blobs")
	if err != nil {
		t.Fatal(err)
	}
	seedHistoricalManualSubmission(t, st, blobs, req)
	check := func(name string, replayed submitResult, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s = %v", name, err)
		}
		if replayed.ImplementationRunID != req.ImplementationRunID || replayed.SpecificationRunID != req.SpecificationRunID ||
			replayed.WorkUnitID != "" {
			t.Fatalf("%s = %+v, want the seeded run with no work unit", name, replayed)
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

	// The accepted identity names its undeclared inputs and nothing else, so
	// a different task under it is still refused, as a conflict.
	changed := sourceApplyRequest(t, submissionSourceCase{
		task: "# Task\n\nImplement another thing.\n", publication: undeclaredSubmission.publication,
	})
	if _, err := applySubmission(t.Context(), st, blobs, changed); !errors.Is(err, store.ErrImmutableConflict) {
		t.Fatalf("changed task under the accepted identity = %v, want an immutable conflict", err)
	}
}

// A journal alone is not acceptance: one that never reached the database is
// a new submission, and the rule refuses it without creating work.
func TestRetainedUndeclaredSubmissionDoesNotCreateWork(t *testing.T) {
	t.Parallel()
	for name, retry := range map[string]func(submitCommandConfig) submitCommandConfig{
		"manual retry": func(cfg submitCommandConfig) submitCommandConfig {
			return submitCommandConfig{DBPath: cfg.DBPath, RetrySubmissionID: cfg.SubmissionID}
		},
		"repeated identity": func(cfg submitCommandConfig) submitCommandConfig { return cfg },
	} {
		t.Run(name, func(t *testing.T) {
			cfg, req := retainHistoricalSource(t, undeclaredSubmission)
			_, err := runSubmitCommand(t.Context(), retry(cfg))
			checkWorkUnitRefusal(t, err)
			assertNoSubmittedWork(t, storetest.Open(t, cfg.DBPath, store.Options{}), req)
		})
	}
}

// undeclaredPreflight is the preflight invocation for undeclaredSubmission
// under a submission identity, with no -work-unit.
func undeclaredPreflight(t *testing.T, submissionID string) ([]string, *fakePreflightEnvironment, preflightConfig) {
	t.Helper()
	args, env := preflightFixture(t)
	args = append(args, "-submission-id", submissionID)
	cfg, err := parsePreflightConfig(args, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	for path, body := range map[string]string{
		cfg.TaskPath: undeclaredSubmission.task, cfg.PublicationPath: undeclaredSubmission.publication,
	} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return args, env, cfg
}

func checkWorkUnitPreflightRefusal(t *testing.T, manifest compositionManifest, err error) compositionCheck {
	t.Helper()
	check := sourceIdentityCheck(t, manifest)
	if !errors.Is(err, errCompositionPreflight) || manifest.Status != compositionFailed || check.Status != compositionFailed {
		t.Fatalf("preflight = %v, status %s, identity check %s; want a failed manifest", err, manifest.Status, check.Status)
	}
	// The operator reads the manifest, so the check carries the same message
	// submit prints.
	if check.Evidence != errWorkUnitRequired.Error() || !strings.Contains(check.Remediation, "work-unit declaration") {
		t.Fatalf("identity check = %+v, want submit's refusal and the declaration named", check)
	}
	return check
}

func TestPreflightRequiresWorkUnit(t *testing.T) {
	t.Parallel()
	args, env, _ := undeclaredPreflight(t, "undeclared")
	manifest, err := runSourcePreflight(t, args, env)
	checkWorkUnitPreflightRefusal(t, manifest, err)

	declared := append(append([]string{}, args...), "-work-unit", declaredWorkUnit(t))
	manifest, err = runSourcePreflight(t, declared, env)
	if check := sourceIdentityCheck(t, manifest); err != nil || check.Status != compositionPassed {
		t.Fatalf("declared preflight = %v, identity check %s (%s), want passed", err, check.Status, check.Evidence)
	}

	// With no submission identity preflight only inspects a retained legacy
	// run, which the rule does not reach.
	legacy, err := parsePreflightConfig(args[:len(args)-2], &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inspectCompositionIdentity(t.Context(), legacy); err != nil {
		t.Fatalf("legacy inspection = %v", err)
	}
}

// run-real-work.sh --resume-session feeds a finished session's retained
// inputs back through preflight. A session that submitted without a
// declaration before the rule must still resume, and only with the inputs
// its journal saved.
func TestPreflightResumesRetainedUndeclaredSubmission(t *testing.T) {
	t.Parallel()
	saved, _ := retainHistoricalSource(t, undeclaredSubmission)
	args, env, cfg := undeclaredPreflight(t, saved.SubmissionID)
	// The authenticated rig names the database whose journal counts.
	env.rig.Resources.DatabasePath = saved.DBPath
	manifest, err := runSourcePreflight(t, args, env)
	if check := sourceIdentityCheck(t, manifest); err != nil || check.Status != compositionPassed {
		t.Fatalf("resumed preflight = %v, identity check %s (%s), want passed", err, check.Status, check.Evidence)
	}

	// The journal counts for the identity it was saved under and no other:
	// a copy filed under another identity resumes nothing.
	journal, err := os.ReadFile(submissionJournalPath(saved.DBPath, saved.SubmissionID))
	if err != nil {
		t.Fatal(err)
	}
	copiedJournal := submissionJournalPath(saved.DBPath, "copied")
	if err := os.WriteFile(copiedJournal, journal, 0o600); err != nil { //nolint:gosec // test-owned path under TempDir
		t.Fatal(err)
	}
	copied := append(append([]string{}, args[:len(args)-1]...), "copied")
	manifest, err = runSourcePreflight(t, copied, env)
	checkWorkUnitPreflightRefusal(t, manifest, err)

	// The same identity with a different task is not that submission.
	if err := os.WriteFile(cfg.TaskPath, []byte("# Task\n\nImplement another thing.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest, err = runSourcePreflight(t, args, env)
	checkWorkUnitPreflightRefusal(t, manifest, err)

	// Without the rig there is no database path to find the journal under,
	// so the saved inputs fail too; the remediation says the refusal may be
	// the missing rig's.
	if err := os.WriteFile(cfg.TaskPath, []byte(undeclaredSubmission.task), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(cfg.RigTokenFile); err != nil {
		t.Fatal(err)
	}
	manifest, err = runSourcePreflight(t, args, env)
	if check := checkWorkUnitPreflightRefusal(t, manifest, err); !strings.Contains(check.Remediation, "once the rig is available") {
		t.Fatalf("preflight without a rig: remediation %q does not name the rig", check.Remediation)
	}
}

// A journal that saved a declaration belongs to a declared submission, whose
// run identity includes it: resuming without the file is not that submission.
func TestPreflightRefusesDeclaredJournalWithoutWorkUnit(t *testing.T) {
	t.Parallel()
	args, env, cfg := undeclaredPreflight(t, "declared")
	dbPath := filepath.Join(t.TempDir(), "state.db")
	prepared, err := prepareSubmission(submitCommandConfig{
		DBPath: dbPath, SubmissionID: "declared", ProjectID: cfg.ProjectID,
		TaskPath: cfg.TaskPath, PolicyPath: cfg.PolicyPath, PublicationPath: cfg.PublicationPath,
		WorkUnitPath: declaredWorkUnit(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := retainSubmission(prepared); err != nil {
		t.Fatal(err)
	}
	env.rig.Resources.DatabasePath = dbPath
	manifest, err := runSourcePreflight(t, args, env)
	checkWorkUnitPreflightRefusal(t, manifest, err)
}
