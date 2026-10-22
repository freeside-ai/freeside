package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
	"github.com/freeside-ai/freeside/daemon/internal/observe/observedb"
	"github.com/freeside-ai/freeside/daemon/internal/seedfixture"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

var taskRecordAsOf = time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)

// A run of the representative fixture's richest task: a decided
// specification, a failed attempt, and the attempt that replaced it.
const fixtureActiveRunID = "run-freeside-656"

// openSeededStore opens a store holding the representative fixture, with
// the attended_dev floor its admissions were recorded under.
func openSeededStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	st := storetest.Open(t, dbPath, store.Options{
		AdmissionFloors: map[domain.OperatingMode]domain.CapabilitySnapshot{
			domain.ModeAttendedDev: seedfixture.AdmissionCapabilities,
		},
	})
	if _, err := seedfixture.Seed(t.Context(), st, seedfixture.Representative); err != nil {
		t.Fatal(err)
	}
	return st, dbPath
}

func taskOfRun(t *testing.T, st *store.Store, runID domain.RunID) domain.TaskID {
	t.Helper()
	var id domain.TaskID
	if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
		run, err := tx.GetRun(t.Context(), runID)
		id = run.TaskID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

func readRecord(t *testing.T, st *store.Store, id domain.TaskID) taskRecord {
	t.Helper()
	var record taskRecord
	if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		record, err = readTaskRecord(t.Context(), tx, id, taskRecordAsOf)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return record
}

func recordItem(record taskRecord, id domain.ItemID) *taskRecordItem {
	index := slices.IndexFunc(record.Items, func(item taskRecordItem) bool { return item.ID == id })
	if index < 0 {
		return nil
	}
	return &record.Items[index]
}

func TestTaskRecordConclusionDuringExternalReview(t *testing.T) {
	st := schedTestStore(t)
	item := reentryReadyItem(t, st, "run-record-external-review")
	runID := *item.Subject.RunID
	armActiveTestSchedules(t, st, item)
	activateExternalTestProfile(t, st, activeResourceTestTime.Add(-time.Hour), externalTestMaintainer())
	reconciler := externalReentryReconciler(st, staticReview(externalReentryActivity()))
	reconcileClean(t, &reconciler, "start cycle")
	successor, queued := readReentry(t, st, runID)
	if successor == nil || !queued {
		t.Fatal("external review cycle was not queued")
	}
	for _, finished := range []bool{false, true} {
		if finished {
			if err := st.WriteInternal(t.Context(), func(tx *store.InternalTx) error {
				return tx.MarkOutboxDispatched(t.Context(), successor.TaskKey())
			}); err != nil {
				t.Fatal(err)
			}
		}
		record := readRecord(t, st, taskOfRun(t, st, runID))
		if len(record.Runs) != 1 {
			t.Fatalf("runs = %d, want 1", len(record.Runs))
		}
		run := record.Runs[0]
		if run.Conclusion == nil || len(run.Errors) != 0 {
			t.Errorf("finished=%t: conclusion=%#v, errors=%#v", finished, run.Conclusion, run.Errors)
		}
	}
}

// TestTaskRecordGolden pins the record's shape. A change to these bytes is a
// change to taskRecordVersion. The store stamps a random task id and the wall
// clock on every task, so the test replaces those two values before
// comparing; everything else in the fixture is fixed.
func TestTaskRecordGolden(t *testing.T) {
	st, _ := openSeededStore(t)
	ctx := t.Context()
	id := taskOfRun(t, st, fixtureActiveRunID)
	var invocation domain.InvocationID
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		admissions, err := tx.ListRunExecutionAdmissionRecords(ctx, fixtureActiveRunID)
		if err != nil {
			return err
		}
		invocation = admissions[0].InvocationID
		notice, err := invocationStalledItem(ctx, tx, invocation, 5*time.Minute, taskRecordAsOf)
		if err != nil {
			return err
		}
		if err := tx.PutAttentionItem(ctx, notice); err != nil {
			return err
		}
		return tx.PutAttentionDelivery(ctx, domain.AttentionDelivery{
			ItemID: "execution-failure-" + fixtureActiveRunID, DeviceID: "device-golden", Channel: "ntfy",
			Attempt: 1, SubmittedAt: taskRecordAsOf, Status: domain.DeliverySubmitted,
		})
	}); err != nil {
		t.Fatal(err)
	}
	record := readRecord(t, st, id)
	if record.Version != taskRecordVersion {
		t.Fatalf("version = %q", record.Version)
	}
	got, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	created, err := json.Marshal(record.Task.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	got = bytes.ReplaceAll(got, []byte(id), []byte("task-GOLDEN"))
	got = bytes.ReplaceAll(got, created, []byte(`"2026-09-01T00:00:00Z"`))
	golden.Assert(t, "task_record", append(got, '\n'))
}

// TestTaskRecordStampsMatchTimeline checks the stamps only. That one
// transaction reads every section is a property of readTaskRecord's
// signature: it and its callees hold a *store.ReadTx and no *store.Store.
func TestTaskRecordStampsMatchTimeline(t *testing.T) {
	st, _ := openSeededStore(t)
	record := readRecord(t, st, taskOfRun(t, st, fixtureActiveRunID))
	if record.Timeline == nil || record.TimelineError != nil {
		t.Fatalf("timeline = %v, error = %+v", record.Timeline, record.TimelineError)
	}
	if record.AsOfRevision == 0 || record.AsOfRevision != record.Timeline.AsOfRevision {
		t.Fatalf("record revision %d, timeline revision %d", record.AsOfRevision, record.Timeline.AsOfRevision)
	}
	if !record.AsOf.Equal(taskRecordAsOf) || !record.Timeline.AsOf.Equal(taskRecordAsOf) {
		t.Fatalf("as_of = %s, timeline as_of = %s", record.AsOf, record.Timeline.AsOf)
	}
}

// TestTaskRecordConclusionMatchesFollow holds the record's conclusion to the
// one `freesided follow` reads, for every fixture run: the same outcome, or
// the same refusal.
func TestTaskRecordConclusionMatchesFollow(t *testing.T) {
	st, _ := openSeededStore(t)
	ctx := t.Context()
	var tasks []store.Snapshotted[domain.Task]
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		tasks, err = tx.ListTasks(ctx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	concluded, refused := 0, 0
	for _, task := range tasks {
		for _, run := range readRecord(t, st, task.Value.ID).Runs {
			_, want, err := observedb.Borrow(st).ObserveConclusion(ctx, run.RunID)
			if err != nil {
				refused++
				if run.Conclusion != nil || !slices.Equal(runErrorSections(run), []taskRecordRunSection{taskRecordRunConclusion}) ||
					run.Errors[0].Kind != taskRecordErrorIntegrity {
					t.Errorf("run %s: follow refuses (%v); record conclusion = %+v, errors = %+v",
						run.RunID, err, run.Conclusion, run.Errors)
				}
				continue
			}
			concluded++
			got := run.Conclusion
			if len(run.Errors) != 0 || got == nil || got.Outcome != want.Outcome || got.Final != want.Final ||
				!equalPointers(got.HoldReason, want.Reason) || !equalPointers(got.TerminalStatus, want.Terminal) {
				t.Errorf("run %s: record conclusion = %+v (errors %+v), follow = %+v", run.RunID, got, run.Errors, want)
			}
		}
	}
	// The fixture holds both cases: runs follow concludes, and a completed
	// run nothing authenticates, which follow refuses.
	if concluded+refused != 8 || concluded == 0 || refused == 0 {
		t.Fatalf("%d concluded and %d refused, want the fixture's 8 runs with both cases", concluded, refused)
	}
}

func runErrorSections(run taskRecordRun) []taskRecordRunSection {
	sections := make([]taskRecordRunSection, 0, len(run.Errors))
	for _, runError := range run.Errors {
		sections = append(sections, runError.Section)
	}
	return sections
}

func equalPointers[T comparable](a, b *T) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// TestTaskRecordReadsBrokenTimeline stores a task creation time the timeline's
// integrity check refuses and the task's own read accepts, and expects the
// rest of the record.
func TestTaskRecordReadsBrokenTimeline(t *testing.T) {
	st, dbPath := openSeededStore(t)
	id := taskOfRun(t, st, fixtureActiveRunID)
	execStoreSQL(t, dbPath,
		`UPDATE tasks SET body = json_set(body, '$.created_at', '2026-09-12T08:00:00-04:00') WHERE id = '`+string(id)+`'`)
	record := readRecord(t, st, id)
	if record.Timeline != nil || record.TimelineError == nil || record.TimelineError.Kind != taskRecordErrorIntegrity {
		t.Fatalf("timeline = %v, error = %+v", record.Timeline, record.TimelineError)
	}
	if !strings.Contains(record.TimelineError.Message, signet.ErrRunObservationIntegrity.Error()) {
		t.Fatalf("timeline error message = %q", record.TimelineError.Message)
	}
	if len(record.Runs) != 3 || record.Items == nil || record.ItemsError != nil {
		t.Fatalf("runs = %d, items = %v, items error = %+v", len(record.Runs), record.Items, record.ItemsError)
	}
	if recordItem(record, "execution-failure-"+fixtureActiveRunID) == nil {
		t.Fatal("the failed attempt's item is missing")
	}
}

// TestTaskRecordReportsUnreadableRun removes one run's row. Every read that
// needs the row is named in the run's errors, the reads that do not need it
// stay, and the task's other runs are untouched.
func TestTaskRecordReportsUnreadableRun(t *testing.T) {
	st, dbPath := openSeededStore(t)
	id := taskOfRun(t, st, fixtureActiveRunID)
	// The task's newest run: no other run's rows depend on it.
	runs := readRecord(t, st, id).Runs
	broken := runs[len(runs)-1].RunID
	execStoreSQL(t, dbPath, `DELETE FROM runs WHERE id = '`+string(broken)+`'`)
	record := readRecord(t, st, id)
	if len(record.Runs) != len(runs) {
		t.Fatalf("runs = %d, want %d", len(record.Runs), len(runs))
	}
	for _, run := range record.Runs {
		if run.RunID != broken {
			if len(run.Errors) != 0 || run.Conclusion == nil {
				t.Errorf("run %s: conclusion = %+v, errors = %+v", run.RunID, run.Conclusion, run.Errors)
			}
			continue
		}
		sections := runErrorSections(run)
		if len(sections) == 0 || !slices.Contains(sections, taskRecordRunConclusion) || run.Conclusion != nil {
			t.Fatalf("run errors = %+v, conclusion = %+v", run.Errors, run.Conclusion)
		}
		for _, runError := range run.Errors {
			if !runError.Section.valid() || runError.Kind != taskRecordErrorIntegrity || runError.Message == "" {
				t.Errorf("run error = %+v", runError)
			}
		}
		// A field is null exactly when the errors name its section, or the
		// run has none of it.
		if (run.Admissions == nil) != slices.Contains(sections, taskRecordRunAdmissions) ||
			(run.Invocations == nil) != slices.Contains(sections, taskRecordRunObservation) {
			t.Errorf("admissions = %v, invocations = %v, errors = %+v", run.Admissions, run.Invocations, run.Errors)
		}
	}
}

// TestTaskRecordKeepsRunFactsPastAFailedRead breaks one admission row of
// another task. The admissions read scans every run's rows, so it fails for
// this task's runs too, and each run still reads its hold, invocations, and
// conclusion.
func TestTaskRecordKeepsRunFactsPastAFailedRead(t *testing.T) {
	st, dbPath := openSeededStore(t)
	id := taskOfRun(t, st, fixtureActiveRunID)
	before := readRecord(t, st, id)
	execStoreSQL(t, dbPath, `UPDATE execution_admissions SET run_id = 'run-elsewhere'
		WHERE invocation_id = (SELECT invocation_id FROM execution_admissions WHERE run_id NOT IN (`+
		quotedRunIDs(before.Runs)+`) LIMIT 1)`)
	record := readRecord(t, st, id)
	for index, run := range record.Runs {
		want := before.Runs[index]
		if !slices.Equal(runErrorSections(run), []taskRecordRunSection{taskRecordRunAdmissions}) ||
			run.Errors[0].Kind != taskRecordErrorIntegrity || run.Admissions != nil {
			t.Fatalf("run %s: errors = %+v, admissions = %v", run.RunID, run.Errors, run.Admissions)
		}
		// Everything but the admissions is as it was.
		run.Admissions, run.Errors = want.Admissions, want.Errors
		got, wantJSON := mustJSON(t, run), mustJSON(t, want)
		if run.Conclusion == nil || run.Invocations == nil || !bytes.Equal(got, wantJSON) {
			t.Errorf("run %s lost facts: %s, want %s", run.RunID, got, wantJSON)
		}
	}
	if record.Counts.RunsHeld != before.Counts.RunsHeld {
		t.Fatalf("runs_held = %d, want %d", record.Counts.RunsHeld, before.Counts.RunsHeld)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	out, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func quotedRunIDs(runs []taskRecordRun) string {
	ids := make([]string, 0, len(runs))
	for _, run := range runs {
		ids = append(ids, `'`+string(run.RunID)+`'`)
	}
	return strings.Join(ids, ", ")
}

// TestTaskRecordReportsUnreadableTask stores a task row the store refuses.
// The task section is null with its error. Each run's conclusion needs the
// task's rows and says so, and the reads that do not need them stay.
func TestTaskRecordReportsUnreadableTask(t *testing.T) {
	st, dbPath := openSeededStore(t)
	id := taskOfRun(t, st, fixtureActiveRunID)
	execStoreSQL(t, dbPath, `UPDATE tasks SET body = json_set(body, '$.project_id', 'project-elsewhere') WHERE id = '`+string(id)+`'`)
	record := readRecord(t, st, id)
	if record.Task != nil || record.TaskError == nil || record.TaskError.Kind != taskRecordErrorIntegrity ||
		record.Counts.FailedStops != nil || record.TaskID != id {
		t.Fatalf("task = %+v, error = %+v, failed_stops = %v", record.Task, record.TaskError, record.Counts.FailedStops)
	}
	if record.Timeline != nil || record.TimelineError == nil || len(record.Runs) != 3 {
		t.Fatalf("timeline = %v, error = %+v, runs = %d", record.Timeline, record.TimelineError, len(record.Runs))
	}
	for _, run := range record.Runs {
		if !slices.Equal(runErrorSections(run), []taskRecordRunSection{taskRecordRunConclusion}) ||
			run.Admissions == nil || run.Invocations == nil {
			t.Errorf("run %s: admissions = %v, invocations = %v, errors = %+v",
				run.RunID, run.Admissions, run.Invocations, run.Errors)
		}
	}
}

// TestTaskRecordReportsUnreadableItems removes the attempt history the
// task's item rows are bound to. Each refused row prints as its id and
// error with nothing else, the list itself reads, and the counts, which
// count read items only, are zero; the runs still read.
func TestTaskRecordReportsUnreadableItems(t *testing.T) {
	st, dbPath := openSeededStore(t)
	id := taskOfRun(t, st, fixtureActiveRunID)
	want := readRecord(t, st, id).Items
	execStoreSQL(t, dbPath, `DELETE FROM production_attempts`)
	record := readRecord(t, st, id)
	if record.ItemsError != nil || len(record.Items) != len(want) || len(want) == 0 {
		t.Fatalf("items = %d (want %d), items_error = %+v", len(record.Items), len(want), record.ItemsError)
	}
	for index, item := range record.Items {
		refused := taskRecordItem{ID: want[index].ID, Error: item.Error}
		if item.Error == nil || item.Error.Kind != taskRecordErrorIntegrity || !reflect.DeepEqual(item, refused) {
			t.Errorf("item %d = %+v, want only id %q and an integrity error", index, item, want[index].ID)
		}
	}
	for _, count := range record.Counts.ItemsByType {
		if count.Count != 0 {
			t.Errorf("counts = %+v, want zero: a refused item has no type", record.Counts.ItemsByType)
		}
	}
	if record.Task == nil || len(record.Runs) != 3 || record.Counts.FailedStops == nil {
		t.Fatalf("task = %v, runs = %d, failed_stops = %v", record.Task, len(record.Runs), record.Counts.FailedStops)
	}
}

// TestTaskRecordCountsUnattributedRefusedRows forges the subject columns of
// one of the task's item rows. The row is refused and its unverified columns
// no longer route it here, so it leaves the items, and the count of refused
// rows the record did not attribute says the store refused something.
func TestTaskRecordCountsUnattributedRefusedRows(t *testing.T) {
	st, dbPath := openSeededStore(t)
	id := taskOfRun(t, st, fixtureActiveRunID)
	const forged = "execution-failure-" + fixtureActiveRunID
	before := readRecord(t, st, id)
	if recordItem(before, forged) == nil || !equalPointers(before.Counts.UnattributedRefusedItems, new(0)) {
		t.Fatalf("before forging: item = %+v, unattributed = %v", recordItem(before, forged), before.Counts.UnattributedRefusedItems)
	}
	execStoreSQL(t, dbPath, `UPDATE attention_items SET subject_task_id = NULL, subject_run_id = NULL WHERE id = '`+forged+`'`)
	record := readRecord(t, st, id)
	if recordItem(record, forged) != nil || len(record.Items) != len(before.Items)-1 || record.ItemsError != nil {
		t.Fatalf("items = %d (before %d), items_error = %+v", len(record.Items), len(before.Items), record.ItemsError)
	}
	if !equalPointers(record.Counts.UnattributedRefusedItems, new(1)) {
		t.Fatalf("unattributed refused items = %v, want 1", record.Counts.UnattributedRefusedItems)
	}
}

// TestTaskRecordReportsFailedItemEnumeration: when the enumeration itself
// fails, the items and their counts are null with items_error.
func TestTaskRecordReportsFailedItemEnumeration(t *testing.T) {
	st, dbPath := openSeededStore(t)
	id := taskOfRun(t, st, fixtureActiveRunID)
	execStoreSQL(t, dbPath, `DROP TABLE attention_deliveries`)
	record := readRecord(t, st, id)
	if record.Items != nil || record.ItemsError == nil {
		t.Fatalf("items = %v, error = %+v", record.Items, record.ItemsError)
	}
	if record.Counts.ItemsByType != nil || record.Counts.HealthNoticesByCode != nil ||
		record.Counts.UnattributedRefusedItems != nil {
		t.Fatalf("counts = %+v, want null item counts", record.Counts)
	}
}

// TestTaskRecordLeavesOutContent checks the record against what the store
// holds for the record's items: no reason, no evidence reference, no claim,
// and no command message or attachment reaches the output. A digest alone is
// not checked: the record states the specification's digest, which an item's
// evidence can also cite.
func TestTaskRecordLeavesOutContent(t *testing.T) {
	st, _ := openSeededStore(t)
	ctx := t.Context()
	id := taskOfRun(t, st, fixtureActiveRunID)
	const message = "operator-message-that-must-not-leak"
	owned := readRecord(t, st, id).Items
	var private []string
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		for _, entry := range owned {
			item, err := tx.GetAttentionItem(ctx, entry.ID)
			if err != nil {
				return err
			}
			private = append(private, item.Reason)
			for _, artifact := range item.EvidenceSnapshot {
				private = append(private, string(artifact.ID))
			}
			for _, claim := range item.AgentClaims {
				private = append(private, string(claim.Artifact))
				if claim.Text != nil {
					private = append(private, claim.Text.Content)
				}
			}
		}
		target, err := tx.GetAttentionItem(ctx, "execution-failure-"+fixtureActiveRunID)
		if err != nil {
			return err
		}
		command, err := domain.NewCommand(domain.CommandInput{
			CommandID: "discuss-leak-check", DeviceID: "device-leak-check", ItemID: target.ID,
			ItemVersion: target.ItemVersion, PRHeadSHA: target.PRHeadSHA, ArtifactDigests: target.ArtifactDigests,
			Action: domain.ActionDiscuss, Message: message,
		})
		if err != nil {
			return err
		}
		return tx.PutCommand(ctx, command)
	}); err != nil {
		t.Fatal(err)
	}
	private = slices.DeleteFunc(private, func(text string) bool { return text == "" })
	// Every item has a reason; more strings than items means the evidence and
	// claim checks have something to find too.
	if len(owned) < 7 || len(private) <= len(owned) {
		t.Fatalf("%d items with %d private strings: the fixture no longer exercises this check", len(owned), len(private))
	}
	record := readRecord(t, st, id)
	item := recordItem(record, "execution-failure-"+fixtureActiveRunID)
	if item == nil || len(item.Commands) != 1 || item.Commands[0] != (taskRecordCommand{
		CommandID: "discuss-leak-check", Action: domain.ActionDiscuss, DeviceID: "device-leak-check",
	}) {
		t.Fatalf("item = %+v", item)
	}
	out, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range append(private, message) {
		if bytes.Contains(out, []byte(text)) {
			t.Errorf("the record contains %q", text)
		}
	}
}

func TestTaskRecordJoinsStallNotices(t *testing.T) {
	st, _ := openSeededStore(t)
	ctx := t.Context()
	id := taskOfRun(t, st, fixtureActiveRunID)
	var own, foreign, longer domain.ItemID
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		admissions, err := tx.ListRunExecutionAdmissionRecords(ctx, fixtureActiveRunID)
		if err != nil {
			return err
		}
		invocation := admissions[0].InvocationID
		// A notice for another task's invocation, and one whose invocation id
		// merely extends this task's, both stay off the record.
		for target, itemID := range map[domain.InvocationID]*domain.ItemID{
			invocation: &own, "inv-of-no-task": &foreign, invocation + "-b": &longer,
		} {
			notice, err := invocationStalledItem(ctx, tx, target, 5*time.Minute, taskRecordAsOf)
			if err != nil {
				return err
			}
			if err := tx.PutAttentionItem(ctx, notice); err != nil {
				return err
			}
			*itemID = notice.ID
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	record := readRecord(t, st, id)
	notice := recordItem(record, own)
	if notice == nil || notice.HealthDiagnostic == nil || notice.HealthDiagnostic.Code != "invocation_stalled" {
		t.Fatalf("stall notice = %+v", notice)
	}
	if recordItem(record, foreign) != nil || recordItem(record, longer) != nil {
		t.Fatal("the record carries another invocation's stall notice")
	}
	want := []taskRecordCodeCount{{Code: "invocation_stalled", Count: 1}}
	if !slices.Equal(record.Counts.HealthNoticesByCode, want) {
		t.Fatalf("health notices = %+v, want %+v", record.Counts.HealthNoticesByCode, want)
	}
}

func TestTaskRecordCounts(t *testing.T) {
	st, _ := openSeededStore(t)
	record := readRecord(t, st, taskOfRun(t, st, fixtureActiveRunID))
	counted := 0
	for index, count := range record.Counts.ItemsByType {
		if count.Type != domain.AllAttentionTypes[index] {
			t.Fatalf("items_by_type[%d] = %s, want %s", index, count.Type, domain.AllAttentionTypes[index])
		}
		counted += count.Count
	}
	if len(record.Counts.ItemsByType) != len(domain.AllAttentionTypes) || counted != len(record.Items) || counted == 0 {
		t.Fatalf("items_by_type lists %d types and counts %d of %d items",
			len(record.Counts.ItemsByType), counted, len(record.Items))
	}
	if record.Counts.RunsHeld != 1 || record.Counts.FailedStops == nil || *record.Counts.FailedStops != 0 {
		t.Fatalf("runs_held = %d, failed_stops = %v", record.Counts.RunsHeld, record.Counts.FailedStops)
	}
	if record.Counts.HealthNoticesByCode == nil || len(record.Counts.HealthNoticesByCode) != 0 {
		t.Fatalf("health notices = %+v, want an empty list", record.Counts.HealthNoticesByCode)
	}
}

func TestTaskRecordProjectsCancellation(t *testing.T) {
	requested := taskRecordAsOf.Add(time.Minute)
	recorded := taskRecordAsOf.Add(2 * time.Minute)
	for _, state := range domain.AllTaskCancellationStates {
		task := domain.Task{ProjectID: "freeside", CreatedAt: taskRecordAsOf, Cancellation: &domain.TaskCancellation{
			RequestID: "stop-1", State: state, RequestedAt: requested,
		}}
		if state != domain.TaskCancellationRequested {
			task.Cancellation.Acknowledgement = &domain.TaskCancellationAcknowledgement{
				ID: "ack-1", RequestID: "stop-1", State: state, EvidenceDigest: "sha256:evidence", RecordedAt: recorded,
			}
		}
		got := projectTaskRecordTask(task)
		want := taskRecordCancellation{RequestID: "stop-1", State: state, RequestedAt: requested}
		if state != domain.TaskCancellationRequested {
			want.Acknowledgement = &taskRecordAcknowledgement{State: state, EvidenceDigest: "sha256:evidence", RecordedAt: recorded}
		}
		if got.Cancellation == nil || got.Cancellation.RequestID != want.RequestID || got.Cancellation.State != want.State ||
			!equalPointers(got.Cancellation.Acknowledgement, want.Acknowledgement) || got.LifecycleFacts == nil {
			t.Errorf("%s: cancellation = %+v", state, got.Cancellation)
		}
	}
	if got := projectTaskRecordTask(domain.Task{}); got.Cancellation != nil {
		t.Fatalf("cancellation = %+v, want none", got.Cancellation)
	}
}

func TestTaskRecordUnknownTask(t *testing.T) {
	st, _ := openSeededStore(t)
	err := st.Read(t.Context(), func(tx *store.ReadTx) error {
		_, err := readTaskRecord(t.Context(), tx, "task-unknown", taskRecordAsOf)
		return err
	})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want not found", err)
	}
}

func TestTaskRecordErrorKinds(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want taskRecordErrorKind
	}{
		{signet.ErrRunObservationIntegrity, taskRecordErrorIntegrity},
		{domain.ErrParentKeyMismatch, taskRecordErrorIntegrity},
		{store.ErrNotFound, taskRecordErrorIntegrity},
		{domain.ErrUnapprovedRecipe, taskRecordErrorUnapprovedRecipe},
		{errors.New("disk I/O error"), taskRecordErrorOther},
	} {
		got := newTaskRecordError(tc.err)
		if got.Kind != tc.want || !got.Kind.valid() || got.Message != tc.err.Error() {
			t.Errorf("%v: %+v, want kind %s", tc.err, got, tc.want)
		}
	}
	for _, kind := range allTaskRecordErrorKinds {
		if !kind.valid() {
			t.Errorf("%q is listed but not valid", kind)
		}
	}
	for _, section := range allTaskRecordRunSections {
		if !section.valid() {
			t.Errorf("%q is listed but not valid", section)
		}
	}
	if taskRecordErrorKind("").valid() || taskRecordRunSection("").valid() {
		t.Error("a zero value is valid")
	}
}

// TestTaskRecordReportsPublication reads a published run's proof the way the
// real-run verifier checks it, then stores a completion its evidence does
// not support and expects only the completion section to fail.
func TestTaskRecordReportsPublication(t *testing.T) {
	st := schedTestStore(t)
	ctx := t.Context()
	runID := domain.RunID("run-published")
	capturedRunNamed(t, st, runID, domain.ProductionReadyItemID(runID))
	readRun := func() taskRecordRun {
		var run taskRecordRun
		if err := st.Read(ctx, func(tx *store.ReadTx) error {
			run = readTaskRecordRun(ctx, tx, runID)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return run
	}
	run := readRun()
	want := taskRecordPublication{
		ReadyItemID: domain.ProductionReadyItemID(runID), ExportHeadSHA: "cafed00d",
		Outcome: taskRecordPublicationOutcome{
			Repo: "owner/repo", BaseRef: "main", HeadSHA: "cafed00d", PRNumber: 450, EvidenceEligible: true,
		},
	}
	if run.Publication == nil || !reflect.DeepEqual(*run.Publication, want) {
		t.Fatalf("publication = %+v, want %+v", run.Publication, want)
	}
	if sections := runErrorSections(run); slices.Contains(sections, taskRecordRunPublication) ||
		slices.Contains(sections, taskRecordRunCompletion) {
		t.Fatalf("errors = %+v", run.Errors)
	}
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		ready, err := tx.GetReadyItemPRBinding(ctx, domain.ProductionReadyItemID(runID))
		if err != nil {
			return err
		}
		ready.RunID = "run-other"
		if _, err := readTaskRecordCompletion(ctx, tx, runID, ready); !errors.Is(err, domain.ErrParentKeyMismatch) {
			t.Errorf("a ready binding of another run: %v, want an integrity error", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	var unit domain.WorkUnitID
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		declaration, err := tx.GetWorkUnitDeclarationByRun(ctx, runID)
		unit = declaration.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// A lander's head the proof refused prints beside the publication.
	refused := domain.WorkUnitRebaseProof{
		UnitID: unit, RepositoryID: 424242, PRNumber: 450,
		ReviewedBaseSHA: "deadbeef", ReviewedHeadSHA: "cafed00d", ProvenHeadSHA: "beadfeed",
		Detail: "the head does not descend from the reviewed base", RecordedAt: taskRecordAsOf,
	}
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordWorkUnitRebaseProof(ctx, refused)
	}); err != nil {
		t.Fatal(err)
	}
	run = readRun()
	want.RebaseProofs = []taskRecordRebaseProof{{
		ReviewedBaseSHA: "deadbeef", ReviewedHeadSHA: "cafed00d", ProvenHeadSHA: "beadfeed",
		Detail: refused.Detail, RecordedAt: taskRecordAsOf,
	}}
	if run.Publication == nil || !reflect.DeepEqual(*run.Publication, want) {
		t.Fatalf("publication = %+v, want %+v", run.Publication, want)
	}

	boundIssue := 443
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordWorkUnitCompletion(ctx, domain.WorkUnitCompletion{
			UnitID: unit, Criterion: domain.CompletionBoundIssueClosedByMergedPR, PRNumber: 450,
			MergeCommitSHA: "feedface", BoundIssue: &boundIssue, RecordedAt: taskRecordAsOf,
		})
	}); err != nil {
		t.Fatal(err)
	}
	run = readRun()
	if run.Publication == nil || run.Publication.Completion != nil {
		t.Fatalf("publication = %+v, want it with no completion", run.Publication)
	}
	index := slices.IndexFunc(run.Errors, func(e taskRecordRunError) bool { return e.Section == taskRecordRunCompletion })
	if index < 0 || run.Errors[index].Kind != taskRecordErrorIntegrity {
		t.Fatalf("errors = %+v, want an integrity error on the completion", run.Errors)
	}
}
