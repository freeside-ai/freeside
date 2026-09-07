package ward

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
)

func TestCodexReviewArchiveReadFailuresStayOperational(t *testing.T) {
	archive := buildTar(t, []tarEntry{{name: "proof", body: []byte("proof")}})
	archiveRoot, err := os.OpenRoot(filepath.Dir(archive))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := archiveRoot.Close(); err != nil {
			t.Error(err)
		}
	})
	body, err := archiveRoot.ReadFile(filepath.Base(archive))
	if err != nil {
		t.Fatal(err)
	}
	readFailure := errors.New("transient backing-file read failure")
	for _, tc := range []struct {
		name   string
		reader io.Reader
		want   error
	}{
		{"truncated header", bytes.NewReader(body[:511]), io.ErrUnexpectedEOF},
		{"header I/O", iotest.ErrReader(readFailure), readFailure},
		{"content I/O", io.MultiReader(bytes.NewReader(body[:512]), iotest.ErrReader(readFailure)), readFailure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := extractArchiveRegularFile(tc.reader, "/proof", 10)
			if !errors.Is(err, tc.want) || errors.Is(err, errArchiveRegularFileInvalid) {
				t.Fatalf("archive read error lost its operational cause: %v", err)
			}
		})
	}
	for _, tc := range []struct {
		name    string
		entries []tarEntry
	}{
		{"oversize", []tarEntry{{name: "proof", body: []byte("too large")}}},
		{"duplicate", []tarEntry{{name: "proof"}, {name: "proof"}}},
		{"type", []tarEntry{{name: "proof", typeflag: tar.TypeSymlink, linkname: "other"}}},
		{"path escape", []tarEntry{{name: "../proof"}}},
		{"long path", []tarEntry{{name: strings.Repeat("p", maxArchivePathBytes+1)}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := os.ReadFile(buildTar(t, tc.entries))
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = extractArchiveRegularFile(bytes.NewReader(body), "/proof", 4)
			if !errors.Is(err, errArchiveRegularFileInvalid) {
				t.Fatalf("structural archive violation was not invalid output: %v", err)
			}
		})
	}
}

func TestCodexReviewTruncatedCollectionRetriesWithoutCleanup(t *testing.T) {
	fx := newHandoffFixture(t)
	seed := fx.seed(t)
	lc := fx.codexReviewLifecycle(t)
	cfg, spec := testCodexReview(t)
	journal := &fakeCodexReviewJournal{}
	sourceCfg := codexReviewSourceConfigForTest(t, lc, cfg, spec, journal)
	source, err := NewCodexReviewSource(sourceCfg)
	if err != nil {
		t.Fatal(err)
	}
	id := domain.InvocationID("review-retry-collection")
	req := exec.ReviewRequest{
		RunID: "run-1", Round: 1, Repo: seed.Seed.Base.Repo, RepositoryID: seed.Seed.Base.RepositoryID,
		BaseRef: seed.Seed.Base.BaseRef, BaseSHA: strings.Repeat("a", 40), HeadSHA: seed.Seed.Base.BaseSHA,
		Workspace: seed.Seed.SourceDir, Verification: testReviewVerificationEvidence(),
		Instructions: testReviewInstructionBinding(), RequestedAt: codexReviewEpoch,
	}
	if err := source.RequestReview(t.Context(), id, req); err != nil {
		t.Fatal(err)
	}
	archive := buildTar(t, []tarEntry{
		{name: strings.TrimPrefix(codexReviewStatusPath, "/"), body: []byte("0")},
		{name: strings.TrimPrefix(codexReviewEventsPath, "/"), body: []byte(`{"type":"turn.completed"}`)},
		{name: strings.TrimPrefix(codexReviewResultPath, "/"), body: []byte(`{"findings":[]}`)},
	})
	archiveRoot, err := os.OpenRoot(filepath.Dir(archive))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := archiveRoot.Close(); err != nil {
			t.Error(err)
		}
	})
	archiveName := filepath.Base(archive)
	body, err := archiveRoot.ReadFile(archiveName)
	if err != nil {
		t.Fatal(err)
	}
	if err := archiveRoot.WriteFile(archiveName, body[:511], 0o600); err != nil {
		t.Fatal(err)
	}
	fx.rt.exportTarPath = archive
	status, err := source.Inspect(t.Context(), id)
	if err == nil && status == exec.StatusRunning {
		_, err = source.Inspect(t.Context(), id)
	}
	if !errors.Is(err, ErrCodexReviewOperational) || errors.Is(err, ErrCodexReviewOutputInvalid) {
		t.Fatalf("truncated export was not retryable: %v", err)
	}
	if _, _, err := journal.GetCodexReviewOutcome(t.Context(), string(id)); !errors.Is(err, ErrCodexReviewOutcomeNotFound) {
		t.Fatalf("read failure persisted a terminal outcome: %v", err)
	}
	if _, exists := fx.rt.ctrs[codexReviewContainerName(string(id))]; !exists {
		t.Fatal("read failure cleaned up the retryable container")
	}
	if err := archiveRoot.WriteFile(archiveName, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if status, err := source.Inspect(t.Context(), id); err != nil || status != exec.StatusCompleted {
		t.Fatalf("healthy re-export did not complete: %s, %v", status, err)
	}
	if _, err := source.Poll(t.Context(), id); err != nil {
		t.Fatalf("healthy re-export lost its review result: %v", err)
	}
	if _, exists := fx.rt.ctrs[codexReviewContainerName(string(id))]; exists {
		t.Fatal("successful retry did not clean up the container")
	}
}

func TestCodexReviewTerminalFailureOverridesEmptyFindings(t *testing.T) {
	for _, tc := range []struct {
		name, events string
		status       int
		failure      domain.ReviewFailureClass
	}{
		{"empty failure message", `{"type":"turn.failed","error":{"message":""}}`, 0, domain.ReviewFailureTransient},
		{"unfinished turn", `{"type":"turn.started"}`, 0, domain.ReviewFailureTransient},
		{"configuration failure", `{"type":"turn.failed","error":{"message":"configuration invalid"}}`, 0, domain.ReviewFailureConfiguration},
		{"failure after completion", "{\"type\":\"turn.completed\"}\n{\"type\":\"turn.failed\"}", 0, domain.ReviewFailureTransient},
		{"recovered failure", "{\"type\":\"turn.failed\"}\n{\"type\":\"turn.completed\"}", 0, ""},
		{"recovered tool error", "{\"type\":\"item.completed\",\"item\":{\"type\":\"command_execution\",\"exit_code\":128}}\n{\"type\":\"turn.completed\"}", 0, ""},
		{"clean completion", `{"type":"turn.completed"}`, 0, ""},
		{"nonzero beats completion", `{"type":"turn.completed"}`, 1, domain.ReviewFailureTransient},
		{"workspace preflight failure", "fatal: cannot change to bound workspace: Permission denied\n", codexReviewAccessFailureExitStatus, domain.ReviewFailureConfiguration},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := newEquivalenceReviewSource()
			req := exec.ReviewRequest{BaseSHA: testCodexReviewHead, HeadSHA: testCodexReviewHead, Instructions: testReviewInstructionBinding()}
			collection := CodexReviewCollection{ExitStatus: tc.status, Result: []byte(`{"findings":[]}`), Events: []byte(tc.events)}
			outcome := source.normalizeCollection("review-terminal", req, collection)
			if outcome.FailureClass != tc.failure || (outcome.Result == nil) != (tc.failure != "") {
				t.Fatalf("failure=%q result present=%t", outcome.FailureClass, outcome.Result != nil)
			}
			if outcome.Result != nil && len(outcome.Result.Findings) != 0 {
				t.Fatal("clean result invented findings")
			}
			body, err := json.Marshal(outcome)
			if err != nil {
				t.Fatal(err)
			}
			var restored CodexReviewSourceOutcome
			if err := json.Unmarshal(body, &restored); err != nil {
				t.Fatal(err)
			}
			if err := errors.Join(restored.Validate(), restored.verifyCompletionEvidence(codexReviewProvider{})); err != nil {
				t.Fatal(err)
			}
			if restored.Collection == nil || !bytes.Equal(restored.Collection.Events, collection.Events) ||
				!bytes.Equal(restored.Collection.Result, collection.Result) || restored.Collection.ExitStatus != tc.status {
				t.Fatal("persistence lost the authenticated completion bytes")
			}
			journal := &fakeCodexReviewJournal{}
			if err := journal.PutCodexReviewOutcome(t.Context(), "review-terminal", restored); err != nil {
				t.Fatal(err)
			}
			if err := journal.MarkCodexReviewOutcomeReady(t.Context(), "review-terminal"); err != nil {
				t.Fatal(err)
			}
			restarted := &CodexReviewSource{cfg: CodexReviewSourceConfig{Journal: journal}}
			_, err = restarted.Poll(t.Context(), "review-terminal")
			if (err != nil) != (tc.failure != "") {
				t.Fatalf("restart changed the outcome: %v", err)
			}
			restored.Collection.Events = append(restored.Collection.Events, '\n')
			if err := restored.verifyCompletionEvidence(codexReviewProvider{}); !errors.Is(err, domain.ErrInvalidReviewCompletionEvidence) {
				t.Fatalf("changed failure/success bytes accepted: %v", err)
			}
		})
	}
}

func TestCodexReviewReplayCannotTurnTerminalFailureIntoCleanResult(t *testing.T) {
	outcome := codexGoldenReviewOutcome(`{"type":"turn.completed"}`)
	outcome.Collection.Events = []byte(`{"type":"turn.failed","error":{"message":""}}`)
	// Even internally consistent content hashes cannot make a known failure
	// into successful completion during reconstruction.
	outcome.CollectionEvidence = collectionEvidence(codexReviewProvider{}, *outcome.Collection)
	outcome.Result.CompletionEvidence, _ = reviewResultEvidence(codexReviewProvider{}, *outcome.Result, outcome.CollectionEvidence)
	if err := outcome.verifyCompletionEvidence(codexReviewProvider{}); !errors.Is(err, domain.ErrInvalidReviewCompletionEvidence) {
		t.Fatalf("terminal failure accepted as a clean replay: %v", err)
	}
}

func TestCodexReviewFailedCollectionSurvivesCleanupAndRestart(t *testing.T) {
	for _, tc := range []struct {
		name, status, result, events string
		wantEvidence                 bool
	}{
		{"terminal contradiction", "0", `{"findings":[]}`, `{"type":"turn.failed","error":{"message":""}}`, true},
		{"workspace failure with result", "78", `{"findings":[]}`, "workspace inaccessible\n", true},
		{"workspace failure without result", "78", "", "workspace inaccessible\n", true},
		{"malformed result", "0", `{`, `{"type":"turn.completed"}`, true},
		{"missing result", "0", "", `{"type":"turn.completed"}`, true},
		{"missing transcript", "0", `{"findings":[]}`, "", false},
		{"missing status", "", `{"findings":[]}`, `{"type":"turn.completed"}`, false},
		{"oversized result", "78", strings.Repeat("x", maxCodexReviewResultBytes+1), "workspace inaccessible\n", true},
		{"oversized transcript", "78", `{"findings":[]}`, strings.Repeat("x", maxCodexReviewEventsBytes+1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newHandoffFixture(t)
			seed := fx.seed(t)
			lc := fx.codexReviewLifecycle(t)
			cfg, spec := testCodexReview(t)
			journal := &fakeCodexReviewJournal{}
			sourceCfg := codexReviewSourceConfigForTest(t, lc, cfg, spec, journal)
			source, err := NewCodexReviewSource(sourceCfg)
			if err != nil {
				t.Fatal(err)
			}
			id := domain.InvocationID("review-failed-collection")
			req := exec.ReviewRequest{
				RunID: "run-1", Round: 1, Repo: seed.Seed.Base.Repo, RepositoryID: seed.Seed.Base.RepositoryID,
				BaseRef: seed.Seed.Base.BaseRef, BaseSHA: strings.Repeat("a", 40), HeadSHA: seed.Seed.Base.BaseSHA,
				Workspace: seed.Seed.SourceDir, Verification: testReviewVerificationEvidence(),
				Instructions: testReviewInstructionBinding(), RequestedAt: codexReviewEpoch,
			}
			if err := source.RequestReview(t.Context(), id, req); err != nil {
				t.Fatal(err)
			}
			var entries []tarEntry
			for path, body := range map[string]string{codexReviewStatusPath: tc.status, codexReviewEventsPath: tc.events, codexReviewResultPath: tc.result} {
				if body != "" {
					entries = append(entries, tarEntry{name: strings.TrimPrefix(path, "/"), body: []byte(body)})
				}
			}
			fx.rt.exportTarPath = buildTar(t, entries)
			status, err := source.Inspect(t.Context(), id)
			for err == nil && status == exec.StatusRunning {
				status, err = source.Inspect(t.Context(), id)
			}
			if err != nil || status != exec.StatusFailed {
				t.Fatalf("failed collection status=%s err=%v", status, err)
			}
			outcome, ready, err := journal.GetCodexReviewOutcome(t.Context(), string(id))
			if err != nil || !ready || outcome.Result != nil || (outcome.Collection != nil) != tc.wantEvidence {
				t.Fatalf("failed collection ready=%t retained=%t err=%v", ready, outcome.Collection != nil, err)
			}
			if err := outcome.verifyCompletionEvidence(codexReviewProvider{}); err != nil {
				t.Fatal(err)
			}
			wantResult := []byte(tc.result)
			if len(wantResult) > maxCodexReviewResultBytes {
				wantResult = nil
			}
			if tc.wantEvidence && (!bytes.Equal(outcome.Collection.Events, []byte(tc.events)) ||
				!bytes.Equal(outcome.Collection.Result, wantResult)) {
				t.Fatal("failed collection discarded bounded authenticated bytes")
			}
			restarted, err := NewCodexReviewSource(sourceCfg)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := restarted.Poll(t.Context(), id); !errors.Is(err, exec.ErrNoResult) {
				t.Fatalf("failed collection became publishable after restart: %v", err)
			}
			if _, exists := fx.rt.ctrs[codexReviewContainerName(string(id))]; exists {
				t.Fatal("review container survived cleanup")
			}
		})
	}
}
