package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

// Client-target mode: the operator submits the real work item from a Freeside
// client, and the harness follows and verifies exactly that task instead of the
// CLI visibility seed. These helpers resolve the target by trusted task and run
// identity read from the store, never by matching source text. The daemon owns
// task and run creation; this is a read-only verifier over what it recorded.

const (
	realRunClientTargetEnv           = "FREESIDE_REAL_RUN_CLIENT_TARGET"
	realRunTargetTaskIDEnv           = "FREESIDE_REAL_RUN_TARGET_TASK_ID"
	realRunProjectEnv                = "FREESIDE_REAL_RUN_PROJECT"
	realRunSeedSpecificationRunIDEnv = "FREESIDE_REAL_RUN_SEED_SPECIFICATION_RUN_ID"
	realRunDaemonStartedAtEnv        = "FREESIDE_REAL_RUN_DAEMON_STARTED_AT"
	realRunTargetPathEnv             = "FREESIDE_REAL_RUN_TARGET_PATH"
)

// productionImplementationInvocationPrefix mirrors engine.productionInvocationID
// (daemon/internal/engine/production_workflow.go) and the same literal spelled
// at daemon/cmd/freesided/submit.go:540. The implementation invocation ID is
// the prefix followed by the implementation run ID.
const productionImplementationInvocationPrefix = "inv-implement-"

// clientTargetSelection is the durable binding a client-target session records
// before it follows anything: the operator-chosen task, its project, and the
// task's specification run (the first run in its recorded run list).
type clientTargetSelection struct {
	TaskID             domain.TaskID
	ProjectID          domain.ProjectID
	SpecificationRunID domain.RunID
}

// selectClientTarget validates the operator-chosen task against the live store
// and returns its specification run. It refuses, with a reason, a task that is
// absent, in another project, cancelled or stopped, without runs, created
// before this session's daemon started, or the visibility seed itself (a task
// whose runs include the seed's specification run). Selection is by trusted
// task and run identity only; it never inspects source text.
func selectClientTarget(
	ctx context.Context, tx *store.ReadTx,
	taskID domain.TaskID, project domain.ProjectID,
	seedSpecificationRunID domain.RunID, daemonStartedAt time.Time,
) (clientTargetSelection, error) {
	task, err := tx.GetTask(ctx, taskID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return clientTargetSelection{}, fmt.Errorf("task %q does not exist", taskID)
		}
		return clientTargetSelection{}, err
	}
	if task.ProjectID != project {
		return clientTargetSelection{}, fmt.Errorf(
			"task %q belongs to project %q, not the run's project %q", taskID, task.ProjectID, project)
	}
	// A stop request, confirmed or not, means the operator does not want this
	// task run; refuse rather than follow it toward a verified publication.
	if task.Cancellation != nil {
		return clientTargetSelection{}, fmt.Errorf("task %q is cancelled or stopped", taskID)
	}
	// The client target is submitted while this session's daemon runs; the
	// visibility seed and any pre-existing task predate it. This is the only
	// available proxy for "came from the client", because the store keeps no
	// task-to-submission-origin link (see the size/scope note in the plan).
	if task.CreatedAt.Before(daemonStartedAt) {
		return clientTargetSelection{}, fmt.Errorf(
			"task %q was created before this session's daemon started; it cannot be the client target", taskID)
	}
	runs, err := tx.TaskRunIDs(ctx, taskID)
	if err != nil {
		return clientTargetSelection{}, err
	}
	if len(runs) == 0 {
		return clientTargetSelection{}, fmt.Errorf("task %q has no runs yet", taskID)
	}
	for _, run := range runs {
		if seedSpecificationRunID != "" && run == seedSpecificationRunID {
			return clientTargetSelection{}, fmt.Errorf(
				"task %q is the visibility seed; select the task submitted from the client", taskID)
		}
	}
	specificationRunID := runs[0]
	run, err := tx.GetRun(ctx, specificationRunID)
	if err != nil {
		return clientTargetSelection{}, fmt.Errorf("read specification run %q: %w", specificationRunID, err)
	}
	if run.TaskID != taskID || run.ProjectID != project {
		return clientTargetSelection{}, fmt.Errorf(
			"specification run %q does not belong to task %q in project %q", specificationRunID, taskID, project)
	}
	return clientTargetSelection{TaskID: taskID, ProjectID: project, SpecificationRunID: specificationRunID}, nil
}

// bindClientTarget finds the task's implementation run paired with the saved
// specification run and returns it with its invocation ID. bound is false, with
// a nil error, when no implementation run has been recorded yet ("not bound
// yet"), so the caller keeps polling. More than one match is a fail-closed
// error at this trust boundary: the specification run ID is a one-way hash of a
// single implementation run ID, so two matches cannot arise from real
// derivations and silently picking one could bind the wrong run.
func bindClientTarget(
	ctx context.Context, tx *store.ReadTx, sel clientTargetSelection,
) (domain.RunID, domain.InvocationID, bool, error) {
	runs, err := tx.TaskRunIDs(ctx, sel.TaskID)
	if err != nil {
		return "", "", false, err
	}
	var matched domain.RunID
	count := 0
	for _, run := range runs {
		if domain.SpecificationRunIDMatchesImplementation(sel.SpecificationRunID, run) {
			matched = run
			count++
		}
	}
	if count == 0 {
		return "", "", false, nil
	}
	if count > 1 {
		return "", "", false, fmt.Errorf(
			"task %q has %d implementation runs paired with specification run %q", sel.TaskID, count, sel.SpecificationRunID)
	}
	run, err := tx.GetRun(ctx, matched)
	if err != nil {
		return "", "", false, fmt.Errorf("read implementation run %q: %w", matched, err)
	}
	if run.TaskID != sel.TaskID || run.ProjectID != sel.ProjectID {
		return "", "", false, fmt.Errorf(
			"implementation run %q does not belong to task %q in project %q", matched, sel.TaskID, sel.ProjectID)
	}
	return matched, domain.InvocationID(productionImplementationInvocationPrefix + string(matched)), true, nil
}

// clientTargetDecision is the JSON the resolver tool writes for the harness.
// Outcome is one of: selected, refused (select mode), bound, pending, error
// (bind mode). A refusal or a bind error is a clean decision, not a tool
// failure, so the tool still exits 0 and the shell reads the outcome.
type clientTargetDecision struct {
	Outcome                    string `json:"outcome"`
	Reason                     string `json:"reason,omitempty"`
	TaskID                     string `json:"task_id,omitempty"`
	Project                    string `json:"project,omitempty"`
	SpecificationRunID         string `json:"specification_run_id,omitempty"`
	ImplementationRunID        string `json:"implementation_run_id,omitempty"`
	ImplementationInvocationID string `json:"implementation_invocation_id,omitempty"`
}

// TestRealRunClientTarget is the resolver entry point the harness runs from the
// retained verifier binary, switched on by FREESIDE_REAL_RUN_CLIENT_TARGET. It
// opens the live production store read-only, resolves the selection or binding,
// and writes the decision JSON. It never writes to the database. An ordinary
// `go test ./internal/integration` leaves the variable unset and skips it.
func TestRealRunClientTarget(t *testing.T) {
	mode := os.Getenv(realRunClientTargetEnv)
	if mode != "select" && mode != "bind" {
		t.Skip("set " + realRunClientTargetEnv + "=select|bind to resolve a client target; scripts/run-real-work.sh sets it")
	}
	stateRoot := requireEnv(t, "FREESIDE_REAL_RUN_STATE_ROOT")
	project := domain.ProjectID(requireEnv(t, realRunProjectEnv))
	targetPath := requireEnv(t, realRunTargetPathEnv)
	approvedRecipe := domain.Digest(requireEnv(t, "FREESIDE_REAL_RUN_APPROVED_RECIPE"))
	taskID := domain.TaskID(requireEnv(t, realRunTargetTaskIDEnv))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Read-only, alongside the running daemon. The resolver reads only tasks
	// and runs, which do not re-run the admission policy gate, so it needs no
	// backup-health source; opening one before any checkpoint exists (client
	// selection happens well before publication) would be fragile. This is a
	// deliberate divergence from the final verifier's option set.
	dbPath := filepath.Join(stateRoot, "freeside.db")
	opts := store.Options{
		BusyTimeout: 5 * time.Second,
		AdmissionFloors: map[domain.OperatingMode]domain.CapabilitySnapshot{
			domain.ModeUnattended: {},
		},
		ApprovedCredentialModes: []domain.CredentialMode{domain.CredentialSubscriptionContained},
		ApprovedRecipes:         map[domain.Digest]bool{approvedRecipe: true},
	}
	st, err := store.OpenReadOnly(ctx, dbPath, opts)
	if err != nil {
		t.Fatalf("open real-run store read-only: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	var decision clientTargetDecision
	switch mode {
	case "select":
		seedSpecification := domain.RunID(os.Getenv(realRunSeedSpecificationRunIDEnv))
		startedAt, parseErr := parseDaemonStartedAt(os.Getenv(realRunDaemonStartedAtEnv))
		if parseErr != nil {
			t.Fatalf("parse %s: %v", realRunDaemonStartedAtEnv, parseErr)
		}
		if err := st.Read(ctx, func(tx *store.ReadTx) error {
			sel, selErr := selectClientTarget(ctx, tx, taskID, project, seedSpecification, startedAt)
			if selErr != nil {
				decision = clientTargetDecision{Outcome: "refused", Reason: selErr.Error()}
				return nil
			}
			decision = clientTargetDecision{
				Outcome:            "selected",
				TaskID:             string(sel.TaskID),
				Project:            string(sel.ProjectID),
				SpecificationRunID: string(sel.SpecificationRunID),
			}
			return nil
		}); err != nil {
			t.Fatalf("resolve client target selection: %v", err)
		}
	case "bind":
		sel := clientTargetSelection{
			TaskID:             taskID,
			ProjectID:          project,
			SpecificationRunID: domain.RunID(requireEnv(t, realRunSpecificationRunIDEnv)),
		}
		if err := st.Read(ctx, func(tx *store.ReadTx) error {
			runID, invocation, bound, bindErr := bindClientTarget(ctx, tx, sel)
			switch {
			case bindErr != nil:
				decision = clientTargetDecision{Outcome: "error", Reason: bindErr.Error()}
			case !bound:
				decision = clientTargetDecision{Outcome: "pending"}
			default:
				decision = clientTargetDecision{
					Outcome:                    "bound",
					ImplementationRunID:        string(runID),
					ImplementationInvocationID: string(invocation),
				}
			}
			return nil
		}); err != nil {
			t.Fatalf("resolve client target binding: %v", err)
		}
	}

	if err := writeClientTargetDecision(targetPath, decision); err != nil {
		t.Fatalf("write client-target decision: %v", err)
	}
	t.Logf("client-target %s: outcome=%s", mode, decision.Outcome)
}

// parseDaemonStartedAt reads the Unix-nanosecond instant the harness recorded
// just before the daemon launched.
func parseDaemonStartedAt(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, fmt.Errorf("empty daemon start instant")
	}
	nanos, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return time.Time{}, err
	}
	return time.Unix(0, nanos).UTC(), nil
}

func writeClientTargetDecision(path string, decision clientTargetDecision) error {
	body, err := json.Marshal(decision)
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(body, '\n'), 0o600)
}

// --- hermetic table tests --------------------------------------------------

// putClientTargetRun records one run and returns its assigned task ID. A run
// with an empty TaskID mints a fresh task; passing the returned ID puts a
// second run under the same task, in recorded (ordinal) order.
func putClientTargetRun(t *testing.T, ctx context.Context, st *store.Store, run domain.Run) domain.TaskID {
	t.Helper()
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutRun(ctx, run)
	}); err != nil {
		t.Fatalf("put run %q: %v", run.ID, err)
	}
	var taskID domain.TaskID
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		stored, err := tx.GetRun(ctx, run.ID)
		if err != nil {
			return err
		}
		taskID = stored.TaskID
		return nil
	}); err != nil {
		t.Fatalf("read task for run %q: %v", run.ID, err)
	}
	return taskID
}

func openClientTargetStore(t *testing.T, ctx context.Context) *store.Store {
	t.Helper()
	return storetest.Open(t, filepath.Join(t.TempDir(), "freeside.db"), store.Options{
		AdmissionFloors: map[domain.OperatingMode]domain.CapabilitySnapshot{
			domain.ModeAttendedDev: {},
		},
	})
}

// seedClientTargetTask records a specification run and, when implementation is
// true, its paired implementation run under the same task. It returns the task
// ID and specification run ID.
func seedClientTargetTask(
	t *testing.T, ctx context.Context, st *store.Store,
	project domain.ProjectID, implementationRunID domain.RunID, implementation bool,
) (domain.TaskID, domain.RunID) {
	t.Helper()
	specificationRunID := domain.SpecificationRunIDForImplementation(implementationRunID)
	digest := domain.Digest("sha256:" + strings.Repeat("a", 64))
	taskID := putClientTargetRun(t, ctx, st, domain.Run{
		ID: specificationRunID, ProjectID: project, SpecDigest: digest, PolicyDigest: digest,
	})
	if implementation {
		putClientTargetRun(t, ctx, st, domain.Run{
			ID: implementationRunID, ProjectID: project, TaskID: taskID, SpecDigest: digest, PolicyDigest: digest,
		})
	}
	return taskID, specificationRunID
}

func TestSelectClientTarget(t *testing.T) {
	ctx := context.Background()
	st := openClientTargetStore(t, ctx)
	const project = domain.ProjectID("project-1")
	past := time.Now().Add(-time.Hour)
	future := time.Now().Add(time.Hour)

	// The visibility seed: a task created before the daemon start line, with
	// its own specification run the resolver must refuse to reselect.
	seedTask, seedSpec := seedClientTargetTask(t, ctx, st, project, "run-impl-seed", false)

	// The client target: a task created after the daemon start line.
	targetTask, targetSpec := seedClientTargetTask(t, ctx, st, project, "run-impl-target", true)

	otherProjectTask, _ := seedClientTargetTask(t, ctx, st, "project-2", "run-impl-other-project", false)

	t.Run("selects the client target's specification run", func(t *testing.T) {
		var sel clientTargetSelection
		if err := st.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			sel, err = selectClientTarget(ctx, tx, targetTask, project, seedSpec, past)
			return err
		}); err != nil {
			t.Fatalf("select: %v", err)
		}
		if sel.TaskID != targetTask || sel.ProjectID != project || sel.SpecificationRunID != targetSpec {
			t.Fatalf("selection = %+v, want task %q spec %q", sel, targetTask, targetSpec)
		}
	})

	refusals := []struct {
		name    string
		task    domain.TaskID
		project domain.ProjectID
		started time.Time
	}{
		{"absent task", "task-missing", project, past},
		{"wrong project", targetTask, "project-9", past},
		{"the visibility seed", seedTask, project, past},
		{"a task in another project by id", otherProjectTask, project, past},
		{"created before the daemon started", targetTask, project, future},
	}
	for _, tc := range refusals {
		t.Run("refuses "+tc.name, func(t *testing.T) {
			if err := st.Read(ctx, func(tx *store.ReadTx) error {
				_, selErr := selectClientTarget(ctx, tx, tc.task, tc.project, seedSpec, tc.started)
				if selErr == nil {
					return fmt.Errorf("selection accepted %q", tc.task)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}

	t.Run("refuses a cancelled or stopped task", func(t *testing.T) {
		cancelledTask, _ := seedClientTargetTask(t, ctx, st, project, "run-impl-cancelled", true)
		now := time.Now().UTC()
		if err := st.Write(ctx, func(tx *store.WriteTx) error {
			return tx.RecordTaskStart(ctx, "run-impl-cancelled", now)
		}); err != nil {
			t.Fatalf("start task: %v", err)
		}
		state, err := st.ServerState(ctx)
		if err != nil {
			t.Fatalf("server state: %v", err)
		}
		if err := st.Write(ctx, func(tx *store.WriteTx) error {
			_, _, stopErr := tx.StopTask(ctx, domain.StopTaskRequest{
				CommandID: "stop", DeviceID: "device", TaskID: cancelledTask, ProjectID: project,
				ExpectedSyncEpoch: state.SyncEpoch, ExpectedEntityVersion: state.Revision,
			}, now)
			return stopErr
		}); err != nil {
			t.Fatalf("stop task: %v", err)
		}
		if err := st.Read(ctx, func(tx *store.ReadTx) error {
			if _, selErr := selectClientTarget(ctx, tx, cancelledTask, project, seedSpec, past); selErr == nil {
				return fmt.Errorf("selection accepted a cancelled task")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
}

func TestBindClientTarget(t *testing.T) {
	ctx := context.Background()
	st := openClientTargetStore(t, ctx)
	const project = domain.ProjectID("project-1")

	t.Run("pending before the implementation run is recorded", func(t *testing.T) {
		taskID, spec := seedClientTargetTask(t, ctx, st, project, "run-impl-pending", false)
		sel := clientTargetSelection{TaskID: taskID, ProjectID: project, SpecificationRunID: spec}
		if err := st.Read(ctx, func(tx *store.ReadTx) error {
			runID, invocation, bound, bindErr := bindClientTarget(ctx, tx, sel)
			if bindErr != nil {
				return bindErr
			}
			if bound || runID != "" || invocation != "" {
				return fmt.Errorf("bound before the implementation run existed: run=%q bound=%t", runID, bound)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("binds the one paired implementation run", func(t *testing.T) {
		const implRun = domain.RunID("run-impl-bound")
		taskID, spec := seedClientTargetTask(t, ctx, st, project, implRun, true)
		sel := clientTargetSelection{TaskID: taskID, ProjectID: project, SpecificationRunID: spec}
		if err := st.Read(ctx, func(tx *store.ReadTx) error {
			runID, invocation, bound, bindErr := bindClientTarget(ctx, tx, sel)
			if bindErr != nil {
				return bindErr
			}
			if !bound || runID != implRun ||
				invocation != domain.InvocationID(productionImplementationInvocationPrefix+string(implRun)) {
				return fmt.Errorf("binding = run %q invocation %q bound %t", runID, invocation, bound)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
}
