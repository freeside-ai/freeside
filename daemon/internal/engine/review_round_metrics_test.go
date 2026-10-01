package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

// reviewRoundMetricsRepo returns a checkout with a base commit and one head.
func reviewRoundMetricsRepo(t *testing.T) (repo, baseSHA, headSHA string) {
	t.Helper()
	repo = t.TempDir()
	runRemediationGit(t, repo, nil, "init", "-q", "-b", "main", "--object-format=sha1")
	commit := func(body, message string) string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		runRemediationGit(t, repo, nil, "add", "-A")
		runRemediationGit(t, repo, nil, "commit", "-q", "-m", message)
		return strings.TrimSpace(string(runRemediationGit(t, repo, nil, "rev-parse", "HEAD")))
	}
	return repo, commit("one\n", "base"), commit("one\ntwo\n", "head")
}

func TestReviewRoundDiffMetricsFirstRoundAndRetry(t *testing.T) {
	t.Parallel()
	repo, baseSHA, headSHA := reviewRoundMetricsRepo(t)
	st := storetest.Open(t, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	workflow := &productionPublicationWorkflow{store: st, workDir: t.TempDir()}
	task := productionPublicationTask{RunID: "run-1"}
	record := domain.ReviewRecord{RunID: task.RunID, Round: 1, BaseSHA: baseSHA, HeadSHA: headSHA}

	metrics, err := workflow.reviewRoundDiffMetrics(
		t.Context(), task, productionBinding{}, repo, record)
	if err != nil {
		t.Fatal(err)
	}
	want := domain.DiffStats{FilesChanged: 1, Additions: 1, BaseSHA: baseSHA, HeadSHA: headSHA}
	if metrics == nil || metrics.Cumulative != want || metrics.Round != want {
		t.Fatalf("first round metrics = %#v, want both pairs %#v", metrics, want)
	}

	// A head the checkout cannot resolve is a deterministic refusal: a gap.
	absent := record
	absent.HeadSHA = strings.Repeat("0", 40)
	if metrics, err := workflow.reviewRoundDiffMetrics(
		t.Context(), task, productionBinding{}, repo, absent,
	); err != nil || metrics != nil {
		t.Fatalf("unresolvable head = %#v, %v; want a gap", metrics, err)
	}

	// Metrics are never backfilled, so a failure a retry can clear must stop
	// the pass before the record is written instead of becoming a gap.
	unwritable := &productionPublicationWorkflow{
		store: st, workDir: filepath.Join(t.TempDir(), "missing"),
	}
	var pathError *os.PathError
	if _, err := unwritable.reviewRoundDiffMetrics(
		t.Context(), task, productionBinding{}, repo, record,
	); !errors.As(err, &pathError) {
		t.Fatalf("missing work directory = %v, want the path error", err)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := workflow.reviewRoundDiffMetrics(
		cancelled, task, productionBinding{}, repo, record,
	); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled context = %v", err)
	}
}

func TestDiffStatsSincePreviousRound(t *testing.T) {
	t.Parallel()
	repo, baseSHA, headSHA := reviewRoundMetricsRepo(t)
	workflow := &productionPublicationWorkflow{workDir: t.TempDir()}
	task := productionPublicationTask{RunID: "run-1"}
	record := domain.ReviewRecord{RunID: task.RunID, Round: 2, BaseSHA: baseSHA, HeadSHA: headSHA}
	otherHead := strings.Repeat("a", 40)
	remediated := func(base, head string) productionBinding {
		return productionBinding{remediation: &authenticatedRemediationTransition{
			request: remediationInvocationRequest{BaseSHA: base, HeadSHA: head},
		}}
	}

	// A round on an unchanged head counts an empty round pair directly.
	same, err := workflow.diffStatsSincePreviousRound(
		t.Context(), task, productionBinding{}, repo,
		domain.ReviewRecord{Round: 1, BaseSHA: baseSHA, HeadSHA: headSHA}, record)
	if err != nil || *same != (domain.DiffStats{BaseSHA: headSHA, HeadSHA: headSHA}) {
		t.Fatalf("same-head round pair = %#v, %v", same, err)
	}

	previous := domain.ReviewRecord{Round: 1, BaseSHA: baseSHA, HeadSHA: otherHead}
	for name, binding := range map[string]productionBinding{
		// An operator-feedback successor changes the head without a
		// remediation input.
		"no remediation":            {},
		"remediation of other head": remediated(baseSHA, strings.Repeat("b", 40)),
		"remediation on other base": remediated(strings.Repeat("c", 40), otherHead),
	} {
		stats, err := workflow.diffStatsSincePreviousRound(
			t.Context(), task, binding, repo, previous, record)
		if stats != nil || err == nil || reviewRoundMetricsGap(t.Context(), err) != nil {
			t.Fatalf("%s = %#v, %v; want a refusal recorded as a gap", name, stats, err)
		}
	}
}
