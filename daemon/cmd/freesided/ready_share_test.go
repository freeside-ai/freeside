package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/observe/causecount"
	"github.com/freeside-ai/freeside/daemon/internal/observe/readyshare"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

// readyShareEnd is the fixture's -end. With the default 168h period, the
// period is [Nov 9, Nov 16).
var readyShareEnd = time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)

// readyShareFixture writes items through the store's item writer, so the read
// sees the body and subject_task_id the store stores. Each run is its own
// task: run-a, run-b, run-c, and run-d in proj-1 and run-e in proj-2. One
// project-scoped item names no task, and one undecodable row is inserted
// directly. It returns the database and each run's task.
func readyShareFixture(t *testing.T) (string, map[domain.RunID]domain.TaskID) {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	st := storetest.Open(t, dbPath, store.Options{})
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RegisterProject(ctx, domain.Project{ID: "proj-1", Repo: "owner/repo", RepositoryID: 7})
	}); err != nil {
		t.Fatal(err)
	}
	ready, failure := domain.AttentionReadyForFinalReview, domain.AttentionExecutionFailure
	planned, exceptional := domain.InterruptionPlannedGate, domain.InterruptionExceptional
	tasks := map[domain.RunID]domain.TaskID{}
	for i, seed := range []struct {
		run       domain.RunID
		project   domain.ProjectID
		itemType  domain.AttentionType
		class     domain.InterruptionClass
		createdAt time.Time
	}{
		// run-a raised nothing before ready; its later failure doesn't count.
		{"run-a", "proj-1", ready, planned, *day(12)},
		{"run-a", "proj-1", failure, exceptional, *day(13)},
		// run-b raised two items, one of each class, then reached ready twice.
		{"run-b", "proj-1", failure, exceptional, *day(10)},
		{"run-b", "proj-1", failure, planned, *day(11)},
		{"run-b", "proj-1", ready, planned, *day(14)},
		{"run-b", "proj-1", ready, planned, *day(15)},
		// run-c raised one item.
		{"run-c", "proj-1", failure, exceptional, *day(10)},
		{"run-c", "proj-1", ready, planned, *day(11)},
		// run-d first reached ready before the period.
		{"run-d", "proj-1", ready, planned, *day(5)},
		{"run-d", "proj-1", ready, planned, *day(12)},
		// run-e is in another project.
		{"run-e", "proj-2", failure, exceptional, *day(10)},
		{"run-e", "proj-2", ready, planned, *day(12)},
		// No task: the subject is the project.
		{"", "proj-1", failure, exceptional, *day(12)},
	} {
		id := domain.ItemID(fmt.Sprintf("item-%02d", i))
		task := putReadyShareItem(t, ctx, st, id, seed.run, seed.project, seed.itemType, seed.class, seed.createdAt)
		if seed.run != "" {
			tasks[seed.run] = task
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close() //nolint:errcheck // test cleanup
	if _, err := raw.ExecContext(ctx, `
INSERT INTO attention_items (id, project_id, conversation_id, item_type, entity_version, as_of_revision, body)
VALUES ('item-bad', 'proj-1', NULL, 'execution_failure', 1, 1, '{')`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return dbPath, tasks
}

// putReadyShareItem writes one open item and returns the task its run is
// bound to. An empty runID makes a project-scoped item with no task.
func putReadyShareItem(
	t *testing.T, ctx context.Context, st *store.Store, id domain.ItemID, runID domain.RunID,
	project domain.ProjectID, itemType domain.AttentionType, class domain.InterruptionClass, createdAt time.Time,
) domain.TaskID {
	t.Helper()
	input := domain.AttentionItemInput{
		ID: id, ProjectID: project,
		Subject:           domain.Subject{Type: domain.SubjectProject, ID: domain.SubjectID(project)},
		Type:              itemType,
		Priority:          domain.PriorityNormal,
		Reason:            "waiting on the operator",
		RequestedDecision: []domain.Action{},
		ItemVersion:       1,
		InterruptionClass: class,
		Status:            domain.StatusOpen,
		CreatedAt:         &createdAt,
	}
	if runID != "" {
		input.Subject = domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID}
	}
	if itemType == domain.AttentionReadyForFinalReview {
		input.RequestedDecision = []domain.Action{domain.ActionOpenPR, domain.ActionStop, domain.ActionDismiss}
		input.PRHeadSHA, input.PRReference = "cafebabe", &domain.PRReference{Repo: "owner/repo", Number: 123}
	}
	item, err := domain.NewAttentionItem(input, nil)
	if err != nil {
		t.Fatalf("new %s: %v", id, err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := storetest.BindSubject(ctx, tx, &item); err != nil {
			return err
		}
		return tx.PutAttentionItem(ctx, item)
	}); err != nil {
		t.Fatalf("put %s: %v", id, err)
	}
	if item.Subject.TaskID == nil {
		return ""
	}
	return *item.Subject.TaskID
}

func runReadyShare(t *testing.T, args ...string) readyshare.Report {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if err := runReadyShareCommand(context.Background(), args, &stdout, &stderr); err != nil {
		t.Fatalf("ready-share %v: %v; stderr=%s", args, err, stderr.String())
	}
	var report readyshare.Report
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode report: %v\n%s", err, stdout.String())
	}
	return report
}

func TestReadyShareCommandCountsItemsBeforeReady(t *testing.T) {
	t.Parallel()
	dbPath, tasks := readyShareFixture(t)
	end := readyShareEnd.Format(time.RFC3339)
	failure, planned, exceptional := "execution_failure", "planned_gate", "exceptional"
	ref := func(id, class string, d int) readyshare.ItemRef {
		return readyshare.ItemRef{ItemID: domain.ItemID(id), Type: failure, Class: class, CreatedAt: *day(d)}
	}
	share := func(f float64) *float64 { return &f }
	skipped := []causecount.Skipped{{Table: "attention_items", Row: "item-bad", Error: "body: unexpected end of JSON input"}}

	want := readyshare.Report{
		Period:     causecount.Window{Start: *day(9), End: readyShareEnd},
		TasksReady: 4, TasksReadyWithoutItems: 1, ShareWithoutItems: share(0.25),
		ByClass: []readyshare.ClassCount{
			{Class: planned, Known: true, Items: 1, Tasks: 1},
			{Class: exceptional, Known: true, Items: 3, Tasks: 3},
		},
		ByType: []readyshare.TypeCount{
			{Type: failure, Class: exceptional, Known: true, Items: 3, Tasks: 3},
			{Type: failure, Class: planned, Known: true, Items: 1, Tasks: 1},
		},
		Tasks: []readyshare.Task{
			{TaskID: tasks["run-c"], ReadyItemID: "item-07", ReadyAt: *day(11), Items: []readyshare.ItemRef{
				ref("item-06", exceptional, 10),
			}},
			{TaskID: tasks["run-a"], ReadyItemID: "item-00", ReadyAt: *day(12), Items: []readyshare.ItemRef{}},
			{TaskID: tasks["run-e"], ReadyItemID: "item-11", ReadyAt: *day(12), Items: []readyshare.ItemRef{
				ref("item-10", exceptional, 10),
			}},
			{TaskID: tasks["run-b"], ReadyItemID: "item-04", ReadyAt: *day(14), Items: []readyshare.ItemRef{
				ref("item-02", exceptional, 10), ref("item-03", planned, 11),
			}},
		},
		TaskLess: []readyshare.TaskLessCount{{Type: failure, Class: exceptional, Known: true, Items: 1}},
		Skipped:  skipped,
	}
	// run-a and run-e reach ready at the same instant; the task ID breaks
	// the tie, and the IDs are generated.
	if tasks["run-e"] < tasks["run-a"] {
		want.Tasks[1], want.Tasks[2] = want.Tasks[2], want.Tasks[1]
	}
	assertReadyShare(t, "report", runReadyShare(t, "-db", dbPath, "-end", end), want)

	// -project narrows every count: run-e's task and item drop out.
	project := domain.ProjectID("proj-1")
	scoped := want
	scoped.ProjectID = &project
	scoped.TasksReady, scoped.ShareWithoutItems = 3, share(1.0/3)
	scoped.ByClass = []readyshare.ClassCount{
		{Class: planned, Known: true, Items: 1, Tasks: 1},
		{Class: exceptional, Known: true, Items: 2, Tasks: 2},
	}
	scoped.ByType = []readyshare.TypeCount{
		{Type: failure, Class: exceptional, Known: true, Items: 2, Tasks: 2},
		{Type: failure, Class: planned, Known: true, Items: 1, Tasks: 1},
	}
	scoped.Tasks = nil
	for _, task := range want.Tasks {
		if task.TaskID != tasks["run-e"] {
			scoped.Tasks = append(scoped.Tasks, task)
		}
	}
	assertReadyShare(t, "scoped report", runReadyShare(t, "-db", dbPath, "-end", end, "-project", "proj-1"), scoped)

	// -period moves the window: the last 24h holds no first ready item and
	// no task-less item, so the share is null.
	assertReadyShare(t, "24h report", runReadyShare(t, "-db", dbPath, "-end", end, "-period", "24h"), readyshare.Report{
		Period: causecount.Window{Start: *day(15), End: readyShareEnd},
		ByClass: []readyshare.ClassCount{
			{Class: planned, Known: true},
			{Class: exceptional, Known: true},
		},
		ByType: []readyshare.TypeCount{}, Tasks: []readyshare.Task{}, TaskLess: []readyshare.TaskLessCount{},
		Skipped: skipped,
	})
}

func assertReadyShare(t *testing.T, name string, got, want readyshare.Report) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		gotJSON, _ := json.MarshalIndent(got, "", "  ")
		wantJSON, _ := json.MarshalIndent(want, "", "  ")
		t.Fatalf("%s =\n%s\nwant\n%s", name, gotJSON, wantJSON)
	}
}

func TestReadyShareCommandRefusesBadArguments(t *testing.T) {
	t.Parallel()
	dbPath, _ := readyShareFixture(t)
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"unknown project": {[]string{"-db", dbPath, "-project", "proj-2"}, `unknown project "proj-2"`},
		"missing db":      {[]string{}, "-db is required"},
		"zero period":     {[]string{"-db", dbPath, "-period", "0s"}, "-period must be positive"},
		"bad end":         {[]string{"-db", dbPath, "-end", "tomorrow"}, "-end:"},
		"positional":      {[]string{"-db", dbPath, "extra"}, "unexpected positional arguments"},
	} {
		var stdout bytes.Buffer
		err := runReadyShareCommand(context.Background(), tc.args, &stdout, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error = %v, want %q", name, err, tc.want)
		}
		if name == "unknown project" && !errors.Is(err, store.ErrNotFound) {
			t.Errorf("unknown project error %v does not wrap store.ErrNotFound", err)
		}
		if stdout.Len() != 0 {
			t.Errorf("%s: printed %q", name, stdout.String())
		}
	}
}
