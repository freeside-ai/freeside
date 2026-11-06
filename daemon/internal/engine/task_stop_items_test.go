package engine

import (
	"bytes"
	"database/sql"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	execfake "github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// stoppedSpecification is a specification task whose first invocation was
// scripted to end with one outcome, plus the Stop requested on that task.
type stoppedSpecification struct {
	f          specificationFixture
	e          *Engine
	driver     *execfake.StageDriver
	invocation domain.InvocationID
	card       domain.ItemID
	stop       domain.TaskCancellation
}

// newStoppedSpecification submits a specification task and scripts its
// first invocation with script. The engine clock, which stamps the
// invocation's outcome and its card, reads outcomeAt. The caller collects
// and stops in whichever order its case needs.
func newStoppedSpecification(t *testing.T, script execfake.StageScript, outcomeAt time.Time) stoppedSpecification {
	t.Helper()
	f := newSpecificationFixture(t, true, 2)
	*f.now = outcomeAt
	driver := f.newDriver(t)
	id := specificationInvocationID("specification-run", 1)
	driver.Script(id, script)
	f.submit(t)
	return stoppedSpecification{
		f: f, e: f.newEngine(t, driver), driver: driver,
		invocation: id, card: specificationFailureItemID(id),
	}
}

func canceledSpecificationScript(runningInspects int) execfake.StageScript {
	return execfake.StageScript{
		RunningInspects: runningInspects, Outcome: execfake.OutcomeCancel,
		Result: exec.StageResult{Artifacts: []domain.Digest{}, Summary: "Claude invocation canceled by daemon request."},
	}
}

func (s *stoppedSpecification) reconcile(t *testing.T, passes int) {
	t.Helper()
	for range passes {
		if _, err := s.e.Reconcile(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
}

// requestStop commits a Stop on the specification task, requested at at.
func (s *stoppedSpecification) requestStop(t *testing.T, at time.Time) {
	t.Helper()
	run, err := s.f.run("specification-run")
	if err != nil {
		t.Fatal(err)
	}
	state, err := s.f.store.ServerState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.f.store.Write(t.Context(), func(tx *store.WriteTx) error {
		receipt, _, err := tx.StopTask(t.Context(), domain.StopTaskRequest{
			CommandID: "stop-specification", DeviceID: "device-1", TaskID: run.TaskID, ProjectID: run.ProjectID,
			ExpectedSyncEpoch: state.SyncEpoch, ExpectedEntityVersion: state.Revision,
		}, at)
		s.stop = receipt.Cancellation
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// acknowledge records the runtime's answer to the Stop the way
// ReconcileTaskCancellations does once its attempt returns.
func (s *stoppedSpecification) acknowledge(t *testing.T, stopped bool) {
	t.Helper()
	if err := s.e.acknowledgeTaskStop(t.Context(), &taskStopRetry{}, s.stop, stopped); err != nil {
		t.Fatal(err)
	}
}

func (s *stoppedSpecification) item(t *testing.T) domain.AttentionItem {
	t.Helper()
	item, _ := s.f.item(t, s.card)
	return item
}

// lookup reads the card, reporting false while it has not been raised.
func (s *stoppedSpecification) lookup(t *testing.T) (domain.AttentionItem, bool) {
	t.Helper()
	var item domain.AttentionItem
	err := s.f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		item, err = tx.GetAttentionItem(t.Context(), s.card)
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return domain.AttentionItem{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return item, true
}

func (s *stoppedSpecification) task(t *testing.T) domain.Task {
	t.Helper()
	return cancellationTask(t, s.f.store, s.stop.Target.TaskID)
}

// history is the record the expiry must leave untouched: the specification
// terminal and the invocation's execution outcome.
func (s *stoppedSpecification) history(t *testing.T) (store.QueueEntry, domain.ExecutionOutcome) {
	t.Helper()
	var terminal store.QueueEntry
	var outcome domain.ExecutionOutcome
	if err := s.f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		if terminal, err = tx.GetInbox(t.Context(), string(s.invocation)); err != nil {
			return err
		}
		outcome, err = tx.GetExecutionOutcomeRecord(t.Context(), s.invocation)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return terminal, outcome
}

// requireConverged runs further passes of both loops and fails if any of
// them writes: the card keeps its version and the server its revision.
func (s *stoppedSpecification) requireConverged(t *testing.T) {
	t.Helper()
	before := s.item(t)
	revision, err := s.f.store.ServerState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		s.reconcile(t, 1)
		if err := s.e.ReconcileTaskCancellations(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	after := s.item(t)
	current, err := s.f.store.ServerState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if after.ItemVersion != before.ItemVersion || after.Status != before.Status || current != revision {
		t.Fatalf("further passes wrote: item %d/%s -> %d/%s, state %+v -> %+v",
			before.ItemVersion, before.Status, after.ItemVersion, after.Status, revision, current)
	}
}

func requireStopExpired(t *testing.T, item domain.AttentionItem) {
	t.Helper()
	c := item.Conclusion
	if item.Status != domain.StatusExpired || c == nil || c.Cause != domain.ConcludedByExpiry ||
		c.Reason != stoppedTaskItemReason || item.DecidedAt != nil {
		t.Fatalf("card = %s, conclusion %+v, decided_at %v; want expired by the confirmed Stop",
			item.Status, c, item.DecidedAt)
	}
	if item.CreatedAt == nil || c.ConcludedAt.Before(*item.CreatedAt) {
		t.Fatalf("card concluded at %v before it was created at %v", c.ConcludedAt, item.CreatedAt)
	}
}

// TestStopConfirmationExpiresItsCanceledCard is the reproduction's
// ordering: the canceled specification result is collected, raising the
// card, before the Stop is confirmed.
func TestStopConfirmationExpiresItsCanceledCard(t *testing.T) {
	t.Parallel()
	requestedAt := time.Now().UTC().Add(-time.Minute)
	s := newStoppedSpecification(t, canceledSpecificationScript(0), requestedAt.Add(time.Second))
	s.reconcile(t, 1)
	s.requestStop(t, requestedAt)
	opened := s.item(t)
	if opened.Status != domain.StatusOpen || opened.ExecutionFailure == nil ||
		opened.ExecutionFailure.Outcome != domain.ExecutionOutcomeCanceled {
		t.Fatalf("collected card = %s, facts %+v; want an open canceled card", opened.Status, opened.ExecutionFailure)
	}
	terminal, outcome := s.history(t)

	s.acknowledge(t, true)

	if task := s.task(t); task.Cancellation.State != domain.TaskCancellationConfirmed || domain.TaskWIP(task) {
		t.Fatalf("task after confirmation = %+v", task.Cancellation)
	}
	expired := s.item(t)
	requireStopExpired(t, expired)
	if expired.ItemVersion != opened.ItemVersion+1 {
		t.Fatalf("item version %d -> %d, want one bump", opened.ItemVersion, expired.ItemVersion)
	}
	if !expired.Conclusion.ConcludedAt.Equal(s.task(t).Cancellation.Acknowledgement.RecordedAt) {
		t.Fatalf("concluded at %v, want the confirmation's %v",
			expired.Conclusion.ConcludedAt, s.task(t).Cancellation.Acknowledgement.RecordedAt)
	}
	if gotTerminal, gotOutcome := s.history(t); gotTerminal.Kind != terminal.Kind ||
		string(gotTerminal.Payload) != string(terminal.Payload) || gotOutcome != outcome {
		t.Fatal("expiry changed the specification terminal or execution outcome")
	}
	s.requireConverged(t)
}

// TestReconcileExpiresCanceledCardRaisedAfterConfirmation is the other
// ordering: the Stop is confirmed while the invocation is live, and the
// engine collects its canceled result afterwards.
func TestReconcileExpiresCanceledCardRaisedAfterConfirmation(t *testing.T) {
	t.Parallel()
	requestedAt := time.Now().UTC().Add(-time.Minute)
	// The card is stamped after the confirmation, so its expiry must take
	// the card's own creation time.
	s := newStoppedSpecification(t, canceledSpecificationScript(100), time.Now().UTC().Add(time.Hour))
	s.reconcile(t, 1)
	if live, err := s.driver.Inspect(t.Context(), s.invocation); err != nil || !live.Live {
		t.Fatalf("specification invocation not live at Stop: %+v, %v", live, err)
	}
	s.requestStop(t, requestedAt)
	// The runtime cancels the invocation and proves the Stop before the
	// engine collects the canceled result.
	if err := s.driver.Cancel(t.Context(), s.invocation); err != nil {
		t.Fatal(err)
	}
	s.acknowledge(t, true)

	// The sweep runs after the collectors, so no pass ends with the card
	// open: the pass that raises it also expires it.
	var expired domain.AttentionItem
	for pass := range 3 {
		s.reconcile(t, 1)
		item, found := s.lookup(t)
		if !found {
			continue
		}
		if item.Status != domain.StatusExpired {
			t.Fatalf("pass %d left the canceled card %s", pass, item.Status)
		}
		expired = item
		break
	}
	requireStopExpired(t, expired)
	if expired.CreatedAt == nil || !expired.Conclusion.ConcludedAt.Equal(*expired.CreatedAt) {
		t.Fatalf("concluded at %v, want the card's creation %v", expired.Conclusion.ConcludedAt, expired.CreatedAt)
	}
	if _, outcome := s.history(t); outcome.Status != domain.ExecutionOutcomeCanceled {
		t.Fatalf("execution outcome = %+v, want canceled", outcome)
	}
	s.requireConverged(t)
}

// TestStopConfirmationExpiresAnImplementationCard covers the production
// stage. A daemon re-run never answers a stopped task's canceled root
// implementation, so its terminal raises the card, and the confirmation
// must expire it as it does a specification card.
func TestStopConfirmationExpiresAnImplementationCard(t *testing.T) {
	t.Parallel()
	f := newRunningCampaignFixture(t)
	stopRunTask(t, f.store, f.run.ID)
	if err := f.driver.Cancel(t.Context(), f.attempt.InvocationID); err != nil {
		t.Fatal(err)
	}
	f.collect(t, f.run, f.attempt)
	read := func() domain.AttentionItem {
		t.Helper()
		item, _ := f.item(t, domain.ExecutionFailureItemID(f.attempt.InvocationID))
		return item
	}
	opened := read()
	if opened.Status != domain.StatusOpen || opened.ExecutionFailure == nil ||
		opened.ExecutionFailure.Outcome != domain.ExecutionOutcomeCanceled ||
		opened.ExecutionFailure.Stage != domain.StageNameImplementation {
		t.Fatalf("collected card = %s, facts %+v; want an open canceled implementation card",
			opened.Status, opened.ExecutionFailure)
	}

	stop := cancellationTask(t, f.store, f.run.TaskID).Cancellation
	if err := f.engine.acknowledgeTaskStop(t.Context(), &taskStopRetry{}, *stop, true); err != nil {
		t.Fatal(err)
	}

	requireStopExpired(t, read())
	if _, found := f.attemptRun(t, 2); found {
		t.Fatal("a stopped task was re-run")
	}
}

// TestStopConfirmationSurvivesAnUnreadableCard keeps card cleanup from
// holding back a proven Stop: the confirmation commits without retiring a
// card that fails its reads, logs that, and leaves the row for the sweep,
// which logs it again instead of failing the pass.
func TestStopConfirmationSurvivesAnUnreadableCard(t *testing.T) {
	t.Parallel()
	requestedAt := time.Now().UTC().Add(-time.Minute)
	s := newStoppedSpecification(t, canceledSpecificationScript(0), requestedAt.Add(time.Second))
	s.reconcile(t, 1)
	s.requestStop(t, requestedAt)
	db, err := sql.Open("sqlite", s.f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(t.Context(), `UPDATE attention_items SET body = '{' WHERE id = ?`, s.card); err != nil {
		t.Fatal(err)
	}
	type row struct {
		status  string
		version int64
		body    string
	}
	read := func() row {
		t.Helper()
		var r row
		if err := db.QueryRowContext(t.Context(), `SELECT status, entity_version, body FROM attention_items WHERE id = ?`,
			s.card).Scan(&r.status, &r.version, &r.body); err != nil {
			t.Fatal(err)
		}
		return r
	}
	before := read()
	var logs bytes.Buffer
	s.e.logger = slog.New(slog.NewTextHandler(&logs, nil))
	const warning = "stopped task item cleanup remains incomplete"

	s.acknowledge(t, true)

	if task := s.task(t); task.Cancellation.State != domain.TaskCancellationConfirmed || domain.TaskWIP(task) {
		t.Fatalf("unreadable card held back the Stop: %+v", task.Cancellation)
	}
	if n := strings.Count(logs.String(), warning); n != 1 {
		t.Fatalf("confirmation logged %d cleanup warnings, want 1:\n%s", n, logs.String())
	}
	if after := read(); after != before {
		t.Fatalf("unreadable card row changed: %+v -> %+v", before, after)
	}
	// The sweep logs the task and continues. A full Reconcile would trip
	// over the same row in unrelated store-wide reads.
	if err := s.e.reconcileStoppedTaskItems(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(logs.String(), warning); n != 2 {
		t.Fatalf("sweep did not log the unreadable card: %d warnings:\n%s", n, logs.String())
	}
	if after := read(); after != before {
		t.Fatalf("sweep changed the unreadable card row: %+v -> %+v", before, after)
	}
}

// TestStopRetirementKeepsOtherCards covers the cards a Stop must not hide.
func TestStopRetirementKeepsOtherCards(t *testing.T) {
	t.Parallel()
	failed := execfake.StageScript{
		Outcome: execfake.OutcomeFail,
		Result:  exec.StageResult{Artifacts: []domain.Digest{}, Summary: "Specifier exited with status 1."},
	}
	cases := []struct {
		name   string
		script execfake.StageScript
		// outcomeBeforeStop stamps the outcome a second before the Stop
		// was requested instead of a second after.
		outcomeBeforeStop bool
		// ack is the runtime's answer; nil leaves the Stop requested.
		ack *bool
		// want is the Stop state the case leaves.
		want domain.TaskCancellationState
	}{
		{name: "stop requested", script: canceledSpecificationScript(0), want: domain.TaskCancellationRequested},
		{name: "stop failed", script: canceledSpecificationScript(0), ack: new(false), want: domain.TaskCancellationFailed},
		{name: "failed outcome", script: failed, ack: new(true), want: domain.TaskCancellationConfirmed},
		{
			name: "lost outcome", script: execfake.StageScript{Outcome: execfake.OutcomeCrashBeforeResult},
			ack: new(true), want: domain.TaskCancellationConfirmed,
		},
		{
			name: "canceled before the stop", script: canceledSpecificationScript(0), outcomeBeforeStop: true,
			ack: new(true), want: domain.TaskCancellationConfirmed,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			requestedAt := time.Now().UTC().Add(-time.Minute)
			outcomeAt := requestedAt.Add(time.Second)
			if tc.outcomeBeforeStop {
				outcomeAt = requestedAt.Add(-time.Second)
			}
			s := newStoppedSpecification(t, tc.script, outcomeAt)
			s.reconcile(t, 1)
			s.requestStop(t, requestedAt)
			if tc.ack != nil {
				s.acknowledge(t, *tc.ack)
			}
			s.reconcile(t, 2)
			if state := s.task(t).Cancellation.State; state != tc.want {
				t.Fatalf("stop state = %s, want %s", state, tc.want)
			}
			if item := s.item(t); item.Status != domain.StatusOpen || item.Conclusion != nil {
				t.Fatalf("card = %s, conclusion %+v; want it kept open", item.Status, item.Conclusion)
			}
			if tc.want != domain.TaskCancellationRequested {
				s.requireConverged(t)
			}
		})
	}
}

// TestStopCanceledCandidate covers the card-only half of the retirement
// rule, including cards the task-scoped store read cannot hand it today.
func TestStopCanceledCandidate(t *testing.T) {
	t.Parallel()
	run := domain.RunID("run-stopped")
	stop := domain.TaskCancellation{Target: domain.TaskCancellationTarget{
		TaskID: "task-stopped", Runs: []domain.TaskCancellationRun{{RunID: run}},
	}}
	card := func(edit func(*domain.AttentionItem)) domain.AttentionItem {
		item := domain.AttentionItem{
			Type:    domain.AttentionExecutionFailure,
			Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(run), RunID: &run},
			ExecutionFailure: &domain.ExecutionFailureFacts{
				Outcome: domain.ExecutionOutcomeCanceled, Stage: domain.StageNameSpecification, InvocationID: "inv-stopped",
			},
		}
		if edit != nil {
			edit(&item)
		}
		return item
	}
	other := domain.RunID("run-other-task")
	cases := []struct {
		name string
		item domain.AttentionItem
		want bool
	}{
		{name: "canceled on a target run", item: card(nil), want: true},
		{name: "another type", item: card(func(i *domain.AttentionItem) { i.Type = domain.AttentionAgentQuestion })},
		{name: "no execution facts", item: card(func(i *domain.AttentionItem) { i.ExecutionFailure = nil })},
		{name: "failed outcome", item: card(func(i *domain.AttentionItem) { i.ExecutionFailure.Outcome = domain.ExecutionOutcomeFailed })},
		{name: "lost outcome", item: card(func(i *domain.AttentionItem) { i.ExecutionFailure.Outcome = domain.ExecutionOutcomeLost })},
		{name: "run outside the target", item: card(func(i *domain.AttentionItem) { i.Subject.RunID = &other })},
		{name: "no subject run", item: card(func(i *domain.AttentionItem) { i.Subject.RunID = nil })},
	}
	for _, tc := range cases {
		if got := stopCanceledCandidate(stop, tc.item); got != tc.want {
			t.Errorf("%s: candidate = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestCanceledByStop covers the store-backed half of the retirement rule,
// including an owner or episode the fixtures cannot reach through a fenced
// task.
func TestCanceledByStop(t *testing.T) {
	t.Parallel()
	requestedAt := time.Date(2026, 9, 23, 19, 4, 0, 0, time.UTC)
	stop := domain.TaskCancellation{
		Target:      domain.TaskCancellationTarget{TaskID: "task-stopped", EpisodeOrdinal: 1},
		RequestedAt: requestedAt,
	}
	canceled := domain.ExecutionOutcome{Status: domain.ExecutionOutcomeCanceled, RecordedAt: requestedAt}
	cases := []struct {
		name    string
		owner   domain.TaskID
		episode int
		outcome domain.ExecutionOutcome
		want    bool
	}{
		{name: "canceled at the request", owner: "task-stopped", episode: 1, outcome: canceled, want: true},
		{name: "another task", owner: "task-other", episode: 1, outcome: canceled},
		{name: "another episode", owner: "task-stopped", episode: 2, outcome: canceled},
		{name: "no owner", owner: "", episode: 0, outcome: canceled},
		{
			name: "failed outcome", owner: "task-stopped", episode: 1,
			outcome: domain.ExecutionOutcome{Status: domain.ExecutionOutcomeFailed, RecordedAt: requestedAt},
		},
		{
			name: "canceled before the request", owner: "task-stopped", episode: 1,
			outcome: domain.ExecutionOutcome{Status: domain.ExecutionOutcomeCanceled, RecordedAt: requestedAt.Add(-time.Nanosecond)},
		},
	}
	for _, tc := range cases {
		if got := canceledByStop(stop, tc.owner, tc.episode, tc.outcome); got != tc.want {
			t.Errorf("%s: canceledByStop = %v, want %v", tc.name, got, tc.want)
		}
	}
}
