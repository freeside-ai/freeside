package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
)

func reentrySuccessorForTest(
	predecessor domain.ItemID, reason domain.ReadinessInvalidationReason,
) domain.PublicationSuccessor {
	return domain.PublicationSuccessor{
		Version:                 domain.PublicationReentryVersion,
		RunID:                   "run-reentry",
		PredecessorItemID:       predecessor,
		PriorReviewInvocationID: ProductionReviewInvocationID("run-reentry", 1),
		ReviewRound:             2,
		Origin:                  domain.PublicationSuccessorReadinessInvalidation,
		Reentry: &domain.PublicationSuccessorReentry{
			Reason:  reason,
			BaseSHA: strings.Repeat("b", 40),
			HeadSHA: strings.Repeat("c", 40),
		},
	}
}

// TestReentryTaskRoundTripsThroughTheLaneDecoder: the row the trigger writes
// decodes and validates as the lane reads it, and every coordinate of it is
// re-derived from the authority, so an edited row is refused.
func TestReentryTaskRoundTripsThroughTheLaneDecoder(t *testing.T) {
	t.Parallel()
	successor := reentrySuccessorForTest(
		productionReadyItemID("run-reentry"), domain.ReadinessInvalidationBaseAdvanced)
	task := newReentryTask("project-reentry", successor)
	if err := task.validate(); err != nil {
		t.Fatalf("new re-entry task: %v", err)
	}
	payload, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeProductionPublicationTask(store.QueueEntry{
		IdempotencyKey: successor.TaskKey(), Kind: KindProductionPublicationRequested,
		Payload: payload,
	})
	if err != nil {
		t.Fatalf("decode re-entry task: %v", err)
	}
	if !decoded.reentersInPlace() || decoded.intentKey() != successor.TaskKey() ||
		decoded.readyItemID() != successor.ReadyItemID() ||
		decoded.blockedItemID() != successor.BlockedItemID() ||
		decoded.HeadSHA != successor.Reentry.HeadSHA {
		t.Fatalf("decoded re-entry task = %#v", decoded)
	}
	if run, ok := productionRunIDFromPendingTaskKey(successor.TaskKey()); !ok || run != successor.RunID {
		t.Fatalf("task key %q attributed to run %q, %t", successor.TaskKey(), run, ok)
	}

	for name, edit := range map[string]func(*productionPublicationTask){
		"another head":      func(task *productionPublicationTask) { task.HeadSHA = strings.Repeat("d", 40) },
		"another run":       func(task *productionPublicationTask) { task.RunID = "run-other" },
		"no project":        func(task *productionPublicationTask) { task.ProjectID = "" },
		"publication id":    func(task *productionPublicationTask) { task.PublicationID = "publish-other" },
		"verification id":   func(task *productionPublicationTask) { task.VerificationID = "verify-other" },
		"summary":           func(task *productionPublicationTask) { task.Summary = "exported" },
		"replay":            func(task *productionPublicationTask) { task.Replay.HeadSHA = task.HeadSHA },
		"publication":       func(task *productionPublicationTask) { task.Publication.Title = "Title" },
		"artifacts":         func(task *productionPublicationTask) { task.Artifacts = []domain.Digest{"sha256:artifact"} },
		"remediation no-op": func(task *productionPublicationTask) { task.LegacyRemediationNoop = true },
		"short base": func(task *productionPublicationTask) {
			edited := *task.Successor
			reentry := *edited.Reentry
			reentry.BaseSHA = "abc"
			edited.Reentry = &reentry
			task.Successor = &edited
		},
	} {
		edited := task
		edit(&edited)
		if err := edited.validate(); !errors.Is(err, domain.ErrParentKeyMismatch) {
			t.Errorf("%s: validate = %v, want a parent-key mismatch", name, err)
		}
	}
}

// TestReentryVerificationCheckpointKeyNamesTheCycle: a base-advance cycle
// keeps its predecessor's head, so run plus head would collide with the
// earlier cycle's checkpoint.
func TestReentryVerificationCheckpointKeyNamesTheCycle(t *testing.T) {
	t.Parallel()
	first := reentrySuccessorForTest(
		productionReadyItemID("run-reentry"), domain.ReadinessInvalidationBaseAdvanced)
	second := reentrySuccessorForTest(first.ReadyItemID(), domain.ReadinessInvalidationBaseAdvanced)
	second.ReviewRound, second.PriorReviewInvocationID = 3, ProductionReviewInvocationID("run-reentry", 2)
	keys := []string{
		productionVerificationCheckpointKey("run-reentry", first.Reentry.HeadSHA, ""),
		newReentryTask("project-reentry", first).verificationCheckpointKey(),
		newReentryTask("project-reentry", second).verificationCheckpointKey(),
	}
	if keys[0] == keys[1] || keys[1] == keys[2] || keys[0] == keys[2] {
		t.Fatalf("checkpoint keys collide: %q", keys)
	}
}

func TestReentryCheckpointIsCleanOnlyWhenPassedWithNoFindings(t *testing.T) {
	t.Parallel()
	passed := productionReentryCheckpoint{Outcome: domain.VerificationPassed}
	flagged := passed
	flagged.Findings = []domain.CandidateFinding{{}}
	failed := productionReentryCheckpoint{Outcome: domain.VerificationFailed}
	if !passed.clean() || flagged.clean() || failed.clean() || (productionReentryCheckpoint{}).clean() {
		t.Fatal("clean() does not require a passed outcome with no findings")
	}
	checkpoint := productionReentryCheckpoint{
		BaseSHA: strings.Repeat("b", 40), HeadSHA: strings.Repeat("c", 40),
		EvaluatedSHA: strings.Repeat("e", 40), Outcome: domain.VerificationPassed,
	}
	view := checkpoint.view("freeside-ai/example")
	if view.Authorization.ID != "" || view.Authorization.BaseSHA != checkpoint.BaseSHA ||
		view.Authorization.HeadSHA != checkpoint.HeadSHA || view.Imported.CommitSHA != checkpoint.HeadSHA {
		t.Fatalf("checkpoint view = %#v", view)
	}
}

func TestParseNameStatus(t *testing.T) {
	t.Parallel()
	got, err := parseNameStatus([]byte("A\x00new.txt\x00M\x00changed.txt\x00T\x00retyped\x00D\x00gone.txt\x00M\x00bad\xff\x00"))
	if err != nil {
		t.Fatal(err)
	}
	want := []importer.Change{
		{Kind: importer.ChangeAdded, Path: "new.txt"},
		{Kind: importer.ChangeModified, Path: "changed.txt"},
		{Kind: importer.ChangeModified, Path: "retyped"},
		{Kind: importer.ChangeDeleted, Path: "gone.txt"},
		{Kind: importer.ChangeModified, PathHex: "626164ff"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("changes = %#v, want %#v", got, want)
	}
	if got, err := parseNameStatus(nil); err != nil || len(got) != 0 {
		t.Fatalf("empty diff = %#v, %v", got, err)
	}
	for name, out := range map[string]string{
		"rename":       "R100\x00old\x00",
		"missing path": "M\x00",
		"empty path":   "M\x00\x00",
		"odd fields":   "M\x00a\x00D\x00",
	} {
		if _, err := parseNameStatus([]byte(out)); !errors.Is(err, domain.ErrCardFactInconsistent) {
			t.Errorf("%s: err = %v, want a refusal", name, err)
		}
	}
}

// TestReentryStopReasonsNameTheCommits: the stop text is what a person acts
// on, so it names both commits, quotes repository-controlled names, and stays
// bounded however many paths conflict.
func TestReentryStopReasonsNameTheCommits(t *testing.T) {
	t.Parallel()
	task := newReentryTask("project-reentry", reentrySuccessorForTest(
		productionReadyItemID("run-reentry"), domain.ReadinessInvalidationBaseAdvanced))
	base := task.Successor.Reentry.BaseSHA
	paths := make([]string, 0, reentryConflictPathLimit+5)
	for i := range reentryConflictPathLimit + 5 {
		paths = append(paths, "dir/file-"+string(rune('a'+i))+".txt")
	}
	paths[0] = "line\nbreak.txt"
	reason := reentryConflictReason(task, base, &publish.MergeConflictError{
		BaseSHA: base, HeadSHA: task.HeadSHA, Paths: paths,
	})
	if !strings.Contains(reason, base) || !strings.Contains(reason, task.HeadSHA) ||
		!strings.Contains(reason, `"line\nbreak.txt"`) || strings.Contains(reason, "\n") ||
		!strings.Contains(reason, paths[reentryConflictPathLimit-1]) ||
		strings.Contains(reason, paths[reentryConflictPathLimit]) ||
		!strings.Contains(reason, "and more") {
		t.Fatalf("conflict reason = %q", reason)
	}
	few := reentryConflictReason(task, base, &publish.MergeConflictError{
		BaseSHA: base, HeadSHA: task.HeadSHA, Paths: paths[1:3],
	})
	if strings.Contains(few, "and more") {
		t.Fatalf("a complete listing claims more paths: %q", few)
	}
	truncated := reentryConflictReason(task, base, &publish.MergeConflictError{
		BaseSHA: base, HeadSHA: task.HeadSHA, Paths: paths[1:3], Truncated: true,
	})
	if !strings.Contains(truncated, "and more") {
		t.Fatalf("a truncated listing reads as complete: %q", truncated)
	}
	// A path is as long as its pusher makes it; the reason is stored and
	// synced, so each one is cut, on a rune boundary.
	long := reentryConflictReason(task, base, &publish.MergeConflictError{
		BaseSHA: base, HeadSHA: task.HeadSHA, Paths: []string{"x" + strings.Repeat("é", 4096)},
	})
	if len(long) > 4*reentryQuotedTextLimit || !utf8.ValidString(long) || !strings.Contains(long, `é..."`) {
		t.Fatalf("long conflict path reason (%d bytes) = %q", len(long), long)
	}
	missing := reentryMissingHeadReason(task, "feature\nbranch")
	if !strings.Contains(missing, task.HeadSHA) || strings.Contains(missing, "\n") {
		t.Fatalf("missing-head reason = %q", missing)
	}
}

// TestReentryStopsOnlyOnRefusalsTheTreeCauses: a tree the verifier cannot
// evaluate ends the cycle with a reason, because its pusher controls it. A
// refusal that contradicts Freeside's own state stays an error.
func TestReentryStopsOnlyOnRefusalsTheTreeCauses(t *testing.T) {
	t.Parallel()
	task := newReentryTask("project-reentry", reentrySuccessorForTest(
		productionReadyItemID("run-reentry"), domain.ReadinessInvalidationHeadChanged))
	base := task.Successor.Reentry.BaseSHA
	for _, cause := range []error{
		verify.ErrSymlinkEntrypoint, verify.ErrMalformedTree, verify.ErrWorkspaceMismatch,
	} {
		err := fmt.Errorf("clean re-entry verification: path %q\n%s: %w",
			"scripts/verify.sh", strings.Repeat("p", 4096), cause)
		reason, stop := reentryUnverifiableTreeReason(task, base, err)
		if !stop || !strings.Contains(reason, task.HeadSHA) || !strings.Contains(reason, base) ||
			strings.Contains(reason, "\n") || len(reason) > 4*reentryQuotedTextLimit {
			t.Errorf("%v: stop = %t, reason (%d bytes) = %q", cause, stop, len(reason), reason)
		}
	}
	for _, cause := range []error{
		nil, verify.ErrGitPlumbing, verify.ErrRecipeInvalid, verify.ErrHeadMismatch,
		verify.ErrBaseMismatch, context.Canceled,
	} {
		if reason, stop := reentryUnverifiableTreeReason(task, base, cause); stop || reason != "" {
			t.Errorf("%v: stop = %t, reason = %q; want an error the lane sees", cause, stop, reason)
		}
	}
}

// TestReentryStopsOnBaseTheImageCannotServe: an image that cannot serve the
// re-entry base ends the one cycle, and the reason still states the cause when
// the refusal is longer than the excerpt the item quotes. Any other failure
// stays an error.
func TestReentryStopsOnBaseTheImageCannotServe(t *testing.T) {
	t.Parallel()
	task := newReentryTask("project-reentry", reentrySuccessorForTest(
		productionReadyItemID("run-reentry"), domain.ReadinessInvalidationBaseAdvanced))
	base := task.Successor.Reentry.BaseSHA
	image, err := domain.NewProjectImage(domain.ProjectImageInput{
		Repository: "freeside-ai/fixture", RepositoryID: 1,
		CommitSHA:          strings.Repeat("a", 40),
		RecipeDigest:       domain.Digest("sha256:" + strings.Repeat("b", 64)),
		PreparationCommand: []string{"/usr/local/bin/freeside-project-prepare"},
		BaseImageRef:       domain.ImageRef("127.0.0.1:5100/agent@sha256:" + strings.Repeat("c", 64)),
		ImageRef:           domain.ImageRef("127.0.0.1:5100/project@sha256:" + strings.Repeat("d", 64)),
	})
	if err != nil {
		t.Fatal(err)
	}
	refusal := image.AdmissibleAt(domain.ProjectImageBaseInputs{CommitSHA: base}, "")
	reason, stop := reentryUnservedBaseReason(task, base, fmt.Errorf("verify: %w", refusal))
	if !stop || !strings.Contains(reason, task.HeadSHA) || !strings.Contains(reason, base) ||
		!strings.Contains(reason, "rebuild the image") || strings.Contains(reason, "\n") ||
		len(reason) > 4*reentryQuotedTextLimit {
		t.Errorf("stop = %t, reason (%d bytes) = %q", stop, len(reason), reason)
	}
	for _, cause := range []error{
		nil, domain.ErrParentKeyMismatch, verify.ErrGitPlumbing, verify.ErrCommitFileUnreadable,
		context.Canceled,
	} {
		if reason, stop := reentryUnservedBaseReason(task, base, cause); stop || reason != "" {
			t.Errorf("%v: stop = %t, reason = %q; want an error the lane sees", cause, stop, reason)
		}
	}
}

// TestReentryDiffDescribesTheEvaluatedCommit: for a base advance the changes
// are the ones the prospective merge makes to the new base, which excludes
// what the base gained on its own, and the stats are labelled with the pull
// request's head, the commit the ready item presents.
func TestReentryDiffDescribesTheEvaluatedCommit(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		return strings.TrimSpace(string(runRemediationGit(t, repo, nil, args...)))
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		git("add", "-A")
	}
	git("init", "-q", "-b", "main", "--object-format=sha1")
	write("file.txt", "one\n")
	git("commit", "-q", "-m", "base")
	git("checkout", "-q", "-b", "feature")
	write("feature.txt", "feature\nlines\n")
	git("commit", "-q", "-m", "head")
	head := git("rev-parse", "HEAD")
	git("checkout", "-q", "main")
	write("upstream.txt", "upstream\n")
	git("commit", "-q", "-m", "advance")
	base := git("rev-parse", "HEAD")
	git("merge", "-q", "--no-ff", "-m", "merge", "feature")
	merge := git("rev-parse", "HEAD")

	workflow := &productionPublicationWorkflow{workDir: t.TempDir()}
	changes, stats, err := workflow.reentryDiff(t.Context(), repo, base, merge, head)
	if err != nil {
		t.Fatal(err)
	}
	if want := []importer.Change{{Kind: importer.ChangeAdded, Path: "feature.txt"}}; !slices.Equal(changes, want) {
		t.Fatalf("changes = %#v, want %#v", changes, want)
	}
	if stats.BaseSHA != base || stats.HeadSHA != head || stats.FilesChanged != 1 ||
		stats.Additions != 2 || stats.Deletions != 0 {
		t.Fatalf("diff stats = %#v", stats)
	}
}
