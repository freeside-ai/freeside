package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

// submittedTaskLines reads the lines and creation time of the task a CLI
// submission created. It opens and closes the store per read so the next
// submission opens it alone.
func submittedTaskLines(t *testing.T, dbPath string, result submitResult) ([]domain.TaskLine, domain.Task) {
	t.Helper()
	ctx := t.Context()
	st := storetest.Open(t, dbPath, store.Options{})
	defer func() { _ = st.Close() }()
	var (
		lines []domain.TaskLine
		task  domain.Task
	)
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		run, err := tx.GetRun(ctx, result.SpecificationRunID)
		if err != nil {
			return err
		}
		if task, err = tx.GetTask(ctx, run.TaskID); err != nil {
			return err
		}
		lines, err = tx.TaskLines(ctx, run.TaskID)
		return err
	}); err != nil {
		t.Fatalf("read task lines: %v", err)
	}
	return lines, task
}

// lineLessFingerprint is the fingerprint a CLI submission was digested over
// before the task-lines field existed.
func lineLessFingerprint(t *testing.T, project domain.ProjectID, result submitResult) []byte {
	t.Helper()
	fingerprint, err := json.Marshal(struct {
		Project                                            domain.ProjectID
		Source, Policy, Publication, WorkUnit, Composition domain.Digest
	}{project, result.SourceDigest, result.SpecificationPolicyDigest, result.PublicationDigest, "", ""})
	if err != nil {
		t.Fatal(err)
	}
	return fingerprint
}

func recordedRequestDigest(t *testing.T, dbPath, submissionID string) domain.Digest {
	t.Helper()
	ctx := t.Context()
	st := storetest.Open(t, dbPath, store.Options{})
	defer func() { _ = st.Close() }()
	var recorded domain.ManualSubmission
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		recorded, err = tx.GetManualSubmission(ctx, "cli:"+submissionID)
		return err
	}); err != nil {
		t.Fatalf("read manual submission: %v", err)
	}
	return recorded.RequestDigest
}

// TestSubmitCommandRecordsTaskLines covers the CLI's --task-line path: the
// submission that creates the task records its lines once, and no replay,
// reordered repeat, or manual retry appends a second version.
func TestSubmitCommandRecordsTaskLines(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	root := t.TempDir()
	taskPath, policyPath, publicationPath := writeSubmissionInputs(t, root)
	reviewer := domain.TaskLineChoice{Role: domain.RoleReviewer, Agent: "codex"}
	implementer := domain.TaskLineChoice{Role: domain.RoleImplementer, Agent: "claude-b"}
	cfg := submitCommandConfig{
		SubmissionID: "lines-test",
		DBPath:       filepath.Join(root, "freeside.db"),
		TaskPath:     taskPath, PolicyPath: policyPath, PublicationPath: publicationPath,
		ProjectID: "proj-submit",
		TaskLines: []domain.TaskLineChoice{reviewer, implementer},
	}
	first, err := runSubmitCommand(ctx, cfg)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	lines, task := submittedTaskLines(t, cfg.DBPath, first)
	agents := map[domain.RoleName]string{}
	for _, line := range lines {
		if line.Version != 1 || line.PredecessorID != nil || line.Source != domain.TaskLineSourceCLISubmit ||
			line.SetBy != "cli:lines-test" || !line.SetAt.Equal(task.CreatedAt) {
			t.Fatalf("line = %+v", line)
		}
		agents[line.Role] = line.Agent
	}
	if want := map[domain.RoleName]string{domain.RoleReviewer: "codex", domain.RoleImplementer: "claude-b"}; !reflect.DeepEqual(agents, want) {
		t.Fatalf("recorded lines = %v, want %v", agents, want)
	}
	// The fingerprint covers the lines in canonical role order under their
	// wire names; a change to either strands every recorded submission that
	// carries a line.
	lineLess := lineLessFingerprint(t, cfg.ProjectID, first)
	pinned := string(lineLess[:len(lineLess)-1]) +
		`,"TaskLines":[{"role":"implementer","agent":"claude-b"},{"role":"reviewer","agent":"codex"}]}`
	if got, want := recordedRequestDigest(t, cfg.DBPath, cfg.SubmissionID), domain.Digest(contentaddr.Sum([]byte(pinned))); got != want {
		t.Fatalf("request digest = %s, want %s", got, want)
	}

	reordered := cfg
	reordered.TaskLines = []domain.TaskLineChoice{implementer, reviewer}
	for name, repeat := range map[string]submitCommandConfig{
		"same request":  cfg,
		"another order": reordered,
		"manual retry":  {DBPath: cfg.DBPath, RetrySubmissionID: cfg.SubmissionID},
	} {
		replay, err := runSubmitCommand(ctx, repeat)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if replay != first {
			t.Fatalf("%s: result = %#v, want %#v", name, replay, first)
		}
		if after, _ := submittedTaskLines(t, cfg.DBPath, first); !reflect.DeepEqual(after, lines) {
			t.Fatalf("%s: lines = %+v, want %+v", name, after, lines)
		}
	}

	// The same identity with another choice is refused, by the saved journal
	// and, once that is gone, by the recorded submission's fingerprint.
	changed := cfg
	changed.TaskLines = []domain.TaskLineChoice{reviewer, {Role: domain.RoleImplementer, Agent: "codex"}}
	none := cfg
	none.TaskLines = nil
	for name, other := range map[string]submitCommandConfig{"changed agent": changed, "no lines": none} {
		if _, err := runSubmitCommand(ctx, other); !errors.Is(err, store.ErrImmutableConflict) {
			t.Fatalf("%s against the journal: error = %v, want ErrImmutableConflict", name, err)
		}
	}
	if err := os.Remove(submissionJournalPath(cfg.DBPath, cfg.SubmissionID)); err != nil {
		t.Fatal(err)
	}
	for name, other := range map[string]submitCommandConfig{"changed agent": changed, "no lines": none} {
		if _, err := runSubmitCommand(ctx, other); !errors.Is(err, store.ErrImmutableConflict) {
			t.Fatalf("%s against the record: error = %v, want ErrImmutableConflict", name, err)
		}
		// Each refused attempt saved its own journal; clear it for the next.
		if err := os.Remove(submissionJournalPath(cfg.DBPath, cfg.SubmissionID)); err != nil {
			t.Fatal(err)
		}
	}
	if after, _ := submittedTaskLines(t, cfg.DBPath, first); !reflect.DeepEqual(after, lines) {
		t.Fatalf("lines after refused changes = %+v, want %+v", after, lines)
	}
}

// TestSubmitFingerprintWithoutLinesIsUnchanged pins the recorded request
// digest of a line-less CLI submission to the fingerprint taken before the
// task-lines field existed. A recorded submission replays only while a retry
// reproduces that digest.
func TestSubmitFingerprintWithoutLinesIsUnchanged(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	root := t.TempDir()
	taskPath, policyPath, publicationPath := writeSubmissionInputs(t, root)
	cfg := submitCommandConfig{
		SubmissionID: "plain-test",
		DBPath:       filepath.Join(root, "freeside.db"),
		TaskPath:     taskPath, PolicyPath: policyPath, PublicationPath: publicationPath,
		ProjectID: "proj-submit",
	}
	result, err := runSubmitCommand(ctx, cfg)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if lines, _ := submittedTaskLines(t, cfg.DBPath, result); len(lines) != 0 {
		t.Fatalf("line-less submission recorded %+v", lines)
	}
	want := domain.Digest(contentaddr.Sum(lineLessFingerprint(t, cfg.ProjectID, result)))
	if got := recordedRequestDigest(t, cfg.DBPath, cfg.SubmissionID); got != want {
		t.Fatalf("request digest = %s, want %s", got, want)
	}
	if journal, err := os.ReadFile(submissionJournalPath(cfg.DBPath, cfg.SubmissionID)); err != nil {
		t.Fatal(err)
	} else if strings.Contains(string(journal), "TaskLines") {
		t.Fatalf("line-less journal names the task-lines field: %s", journal)
	}
}

func TestSubmitCommandRefusesBadTaskLines(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	root := t.TempDir()
	taskPath, policyPath, publicationPath := writeSubmissionInputs(t, root)
	base := submitCommandConfig{
		SubmissionID: "bad-lines",
		DBPath:       filepath.Join(root, "freeside.db"),
		TaskPath:     taskPath, PolicyPath: policyPath, PublicationPath: publicationPath,
		ProjectID: "proj-submit",
	}
	for name, tc := range map[string]struct {
		lines []domain.TaskLineChoice
		want  error
	}{
		"shadow reviewer": {[]domain.TaskLineChoice{{Role: domain.RoleShadowReviewer, Agent: "codex"}}, domain.ErrTaskLineRoleIneligible},
		"unknown role":    {[]domain.TaskLineChoice{{Role: "researcher", Agent: "codex"}}, domain.ErrTaskLineRoleIneligible},
		"duplicate role": {[]domain.TaskLineChoice{
			{Role: domain.RoleReviewer, Agent: "codex"}, {Role: domain.RoleReviewer, Agent: "claude"},
		}, domain.ErrDuplicateTaskLineRole},
		"malformed agent": {[]domain.TaskLineChoice{{Role: domain.RoleReviewer, Agent: "Codex"}}, domain.ErrInvalidAgentName},
	} {
		cfg := base
		cfg.TaskLines = tc.lines
		if _, err := runSubmitCommand(ctx, cfg); !errors.Is(err, tc.want) {
			t.Fatalf("%s: error = %v, want %v", name, err, tc.want)
		}
	}
	// A refused set saves no journal and creates no database, so the identity
	// is still free for a corrected request.
	if _, err := os.Stat(submissionJournalPath(base.DBPath, base.SubmissionID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal after refused lines: %v, want absent", err)
	}
	if _, err := os.Stat(base.DBPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("database after refused lines: %v, want absent", err)
	}

	chosen := []domain.TaskLineChoice{{Role: domain.RoleReviewer, Agent: "codex"}}
	if _, err := runSubmitCommand(ctx, submitCommandConfig{DBPath: base.DBPath, RunID: "run-legacy", TaskLines: chosen}); err == nil ||
		!strings.Contains(err.Error(), "--run-id is lookup-only") {
		t.Fatalf("lookup with lines: error = %v, want the lookup-only refusal", err)
	}
	if _, err := runSubmitCommand(ctx, submitCommandConfig{DBPath: base.DBPath, RetrySubmissionID: "saved", TaskLines: chosen}); err == nil ||
		!strings.Contains(err.Error(), "manual retry takes only") {
		t.Fatalf("retry with lines: error = %v, want the saved-inputs refusal", err)
	}
}

func TestTaskLineFlag(t *testing.T) {
	t.Parallel()
	var lines taskLineFlag
	for _, value := range []string{"implementer=codex", "reviewer=claude-b"} {
		if err := lines.Set(value); err != nil {
			t.Fatalf("Set(%q): %v", value, err)
		}
	}
	want := taskLineFlag{{Role: domain.RoleImplementer, Agent: "codex"}, {Role: domain.RoleReviewer, Agent: "claude-b"}}
	if !reflect.DeepEqual(lines, want) {
		t.Fatalf("lines = %+v, want %+v", lines, want)
	}
	for _, value := range []string{"", "implementer", "=codex", "implementer="} {
		if err := new(taskLineFlag).Set(value); err == nil {
			t.Fatalf("Set(%q) accepted a value that is not role=agent", value)
		}
	}
}

// TestValidateSubmitApplyChecksTaskLines covers the daemon side of the
// control socket: a prepared request's lines are validated again, must be in
// the canonical order the fingerprint digests, and never ride a legacy
// lookup.
func TestValidateSubmitApplyChecksTaskLines(t *testing.T) {
	t.Parallel()
	reviewer := domain.TaskLineChoice{Role: domain.RoleReviewer, Agent: "codex"}
	implementer := domain.TaskLineChoice{Role: domain.RoleImplementer, Agent: "claude-b"}
	if err := validateSubmitApply(submitApplyRequest{
		TaskLines: []domain.TaskLineChoice{{Role: domain.RoleShadowReviewer, Agent: "codex"}},
	}); !errors.Is(err, domain.ErrTaskLineRoleIneligible) {
		t.Fatalf("ineligible role: error = %v, want ErrTaskLineRoleIneligible", err)
	}
	if err := validateSubmitApply(submitApplyRequest{
		TaskLines: []domain.TaskLineChoice{reviewer, implementer},
	}); err == nil || !strings.Contains(err.Error(), "canonical role order") {
		t.Fatalf("unordered lines: error = %v, want the canonical-order refusal", err)
	}
	if err := validateSubmitApply(submitApplyRequest{
		LegacyRunID: "run-legacy", TaskLines: []domain.TaskLineChoice{implementer, reviewer},
	}); err == nil || !strings.Contains(err.Error(), "legacy run lookup") {
		t.Fatalf("legacy lookup with lines: error = %v, want the legacy refusal", err)
	}
}
