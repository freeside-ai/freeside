package signet_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/agenttree"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

var (
	operatorAt = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	// codex-primary runs on seedUsageEnrollment's enrollment, so pool-a is a
	// drain-first account the tree can name.
	operatorTree = agenttree.Tree{Agents: []domain.AgentSource{
		{Name: "claude-primary"}, {Name: "codex-primary", Enrollment: "enrollment-usage-1"},
	}}
	operatorTreeRev  = domain.Digest("sha256:" + strings.Repeat("a", 64))
	errOperatorFails = errors.New("operator failed")
)

// recordingUsageOperator records each call it receives, and fails every call
// when err is set. Every call, failed or not, returns an after-commit effect
// that counts its runs in afters.
type recordingUsageOperator struct {
	refreshed []domain.UsagePoolID
	reassigns []domain.ClientEnrollmentID
	afters    int
	err       error
}

func (o *recordingUsageOperator) RefreshUsage(
	_ context.Context, _ *store.WriteTx, pool domain.UsagePoolID, _ time.Time,
) (func(), error) {
	o.refreshed = append(o.refreshed, pool)
	return func() { o.afters++ }, o.err
}

func (o *recordingUsageOperator) ReassignCollector(
	_ context.Context, _ *store.WriteTx, _ domain.UsagePoolID, collector domain.ClientEnrollmentID, _ time.Time,
) (func(), error) {
	o.reassigns = append(o.reassigns, collector)
	return func() { o.afters++ }, o.err
}

// engineTaskLineChanger and engineRoutingPolicySetter bind the hooks the way
// the daemon does, so these tests drive the engine's real rules.
func engineTaskLineChanger(
	ctx context.Context, tx *store.WriteTx, change signet.TaskLineChange, now time.Time,
) (signet.TaskLineChangeOutcome, error) {
	result, err := engine.ChangeTaskLineTx(ctx, tx, engine.ChangeTaskLineRequest{
		TaskID: change.TaskID, Role: change.Role, Selection: change.Selection, Clear: change.Clear, Source: change.Source,
	}, now)
	return signet.TaskLineChangeOutcome{Recorded: result.Recorded, Line: result.Line}, err
}

func engineRoutingPolicySetter(
	ctx context.Context, tx *store.WriteTx, change signet.RoutingPolicyChange, now time.Time,
) (signet.RoutingPolicyOutcome, error) {
	result, err := engine.SetRoutingPolicyTx(ctx, tx, operatorTree, operatorTreeRev, engine.SetRoutingPolicyRequest{
		Roles: change.Roles, DrainFirstAccount: change.DrainFirstAccount, Source: change.Source, SetBy: change.SetBy,
	}, now)
	return signet.RoutingPolicyOutcome{Recorded: result.Recorded, Policy: result.Policy}, err
}

// newOperatorService opens a store with an active device-1, the usage pools
// seedUsageEnrollment declares, and one task, composed with opts.
func newOperatorService(t *testing.T, opts ...signet.Option) (*signet.Service, *store.Store, domain.TaskID) {
	t.Helper()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "store.db"), store.Options{
		ApprovedCredentialModes: []domain.CredentialMode{domain.CredentialSubscriptionContained},
	})
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Write(t.Context(), func(tx *store.WriteTx) error {
		return tx.PutDevice(t.Context(), domain.Device{
			ID: "device-1", DisplayName: "Test Mac", Status: domain.DeviceActive, PairedAt: operatorAt,
		})
	}); err != nil {
		t.Fatal(err)
	}
	seedUsageEnrollment(t, st, operatorAt.Add(-time.Hour))
	seedLineEnrollments(t, st)
	id := seedStopTask(t, st)
	opts = append([]signet.Option{signet.WithClock(func() time.Time { return operatorAt })}, opts...)
	return signet.NewService(st, opts...), st, id
}

func operatorCommand(id string, kind domain.CommandKind) signet.ClientCommand {
	return signet.ClientCommand{CommandID: id, DeviceID: "device-1", Kind: kind}
}

func refreshCommand(id string, pool domain.UsagePoolID) signet.ClientCommand {
	in := operatorCommand(id, domain.CommandKindRefreshUsage)
	in.RefreshUsage = signet.RefreshUsagePayload{Pool: pool}
	return in
}

func reassignCommand(id string, pool domain.UsagePoolID, enrollment domain.ClientEnrollmentID) signet.ClientCommand {
	in := operatorCommand(id, domain.CommandKindReassignCollector)
	in.ReassignCollector = signet.ReassignCollectorPayload{Pool: pool, EnrollmentID: enrollment}
	return in
}

// lineCommand changes task's implementer line to the selection that runs
// name (lineSelectionOf), or clears it when name is nil.
func lineCommand(id string, task domain.TaskID, name *string) signet.ClientCommand {
	in := operatorCommand(id, domain.CommandKindChangeTaskLine)
	in.ChangeTaskLine = signet.ChangeTaskLinePayload{TaskID: task, Role: domain.RoleImplementer}
	if name != nil {
		selection := lineSelectionOf(*name)
		in.ChangeTaskLine.Selection = &selection
	}
	return in
}

func moveCommand(id string, task domain.TaskID, agent string) signet.ClientCommand {
	in := operatorCommand(id, domain.CommandKindMoveParkedTask)
	in.MoveParkedTask = signet.MoveParkedTaskPayload{TaskID: task, Agent: agent}
	return in
}

// fakeParkedTaskMover answers a move with the receipt a move to the request's
// agent on pool-b would record, writing nothing: the engine's move has its
// own tests, and these exercise only the command's envelope.
func fakeParkedTaskMover(
	_ context.Context, _ *store.WriteTx, request domain.ParkedTaskMoveRequest, now time.Time,
) (domain.ParkedTaskMoveReceipt, error) {
	record, err := domain.NewAlternateAgentRetry(domain.AlternateAgentRetry{
		TaskID: request.TaskID, Role: domain.RoleImplementer, FailedInvocationID: "inv-refused",
		CommandID: request.CommandID, Agent: request.Agent, Pool: "pool-b",
		Qualification: domain.AlternateAgentDifferentPool, RecordedAt: now,
	})
	if err != nil {
		return domain.ParkedTaskMoveReceipt{}, err
	}
	line, err := domain.NewTaskLine(domain.TaskLineInput{
		TaskID: request.TaskID, Role: domain.RoleImplementer, Agent: request.Agent,
		Source: domain.TaskLineSourceAlternateAgentMove, SetBy: string(record.ID),
		Provenance: &domain.TaskLineProvenance{
			PolicyRevision: 1, Tier: domain.TaskLineSelectionPolicyOrder, Pool: "pool-b",
			RankedBy: []domain.TaskLineRankingReading{},
		},
	}, 1, nil, now)
	if err != nil {
		return domain.ParkedTaskMoveReceipt{}, err
	}
	return domain.ParkedTaskMoveReceipt{ParkedTaskMoveRequest: request, Retry: record, Lines: []domain.TaskLine{line}}, nil
}

func policyCommand(id string, expected *int, agents ...string) signet.ClientCommand {
	in := operatorCommand(id, domain.CommandKindSetRoutingPolicy)
	in.SetRoutingPolicy = signet.SetRoutingPolicyPayload{
		Roles:            []domain.RoutingPolicyRole{{Role: domain.RoleImplementer, Agents: agents}},
		ExpectedRevision: expected,
	}
	return in
}

func serverRevision(t *testing.T, st *store.Store) int64 {
	t.Helper()
	state, err := st.ServerState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return state.Revision
}

// Each operator command commits its receipt at a new revision; a replay of
// its command_id returns that result unchanged and writes nothing, and the
// same command_id with another body is an immutable conflict.
func TestOperatorCommandsAcceptAndReplay(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		kind            domain.CommandKind
		command, change func(domain.TaskID) signet.ClientCommand
	}{
		{
			domain.CommandKindRefreshUsage,
			func(domain.TaskID) signet.ClientCommand { return refreshCommand("cmd", "pool-a") },
			func(domain.TaskID) signet.ClientCommand { return refreshCommand("cmd", "pool-idle") },
		},
		{
			domain.CommandKindReassignCollector,
			func(domain.TaskID) signet.ClientCommand {
				return reassignCommand("cmd", "pool-a", "enrollment-usage-1")
			},
			func(domain.TaskID) signet.ClientCommand { return refreshCommand("cmd", "pool-a") },
		},
		{
			domain.CommandKindChangeTaskLine,
			func(id domain.TaskID) signet.ClientCommand { return lineCommand("cmd", id, new("codex-primary")) },
			func(id domain.TaskID) signet.ClientCommand { return lineCommand("cmd", id, nil) },
		},
		{
			domain.CommandKindSetRoutingPolicy,
			func(domain.TaskID) signet.ClientCommand { return policyCommand("cmd", nil, "codex-primary") },
			func(domain.TaskID) signet.ClientCommand { return policyCommand("cmd", nil, "claude-primary") },
		},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			svc, st, id := newOperatorService(t, signet.WithUsageOperator(&recordingUsageOperator{}),
				signet.WithTaskLineChanger(engineTaskLineChanger), signet.WithRoutingPolicySetter(engineRoutingPolicySetter))
			before := serverRevision(t, st)
			result, err := svc.Submit(t.Context(), tc.command(id))
			if err != nil {
				t.Fatal(err)
			}
			if result.Receipt == nil || result.Receipt.Kind() != tc.kind || result.Revision != before+1 {
				t.Fatalf("result = %+v at %d, want a %s receipt at %d", result.Receipt, result.Revision, tc.kind, before+1)
			}
			replay, err := svc.Submit(t.Context(), tc.command(id))
			if err != nil || !reflect.DeepEqual(replay, result) {
				t.Fatalf("replay = %+v, %v; want %+v", replay, err, result)
			}
			if _, err := svc.Submit(t.Context(), tc.change(id)); !errors.Is(err, store.ErrImmutableConflict) {
				t.Fatalf("another body under the command_id = %v, want ErrImmutableConflict", err)
			}
			if after := serverRevision(t, st); after != before+1 {
				t.Fatalf("revision %d after replay and conflict, want %d", after, before+1)
			}
		})
	}
}

func TestUsageCommandsCheckThePoolAndEnrollment(t *testing.T) {
	t.Parallel()
	operator := &recordingUsageOperator{}
	svc, st, _ := newOperatorService(t, signet.WithUsageOperator(operator))
	before := serverRevision(t, st)
	for _, tc := range []struct {
		name    string
		command signet.ClientCommand
		want    error
	}{
		{"unknown pool refresh", refreshCommand("a", "pool-missing"), store.ErrNotFound},
		{"unknown pool reassign", reassignCommand("b", "pool-missing", "enrollment-usage-1"), store.ErrNotFound},
		{"enrollment on another pool", reassignCommand("c", "pool-idle", "enrollment-usage-1"), store.ErrEnrollmentNotOnPool},
		{"empty pool", refreshCommand("d", ""), signet.ErrInvalidOperatorCommandPayload},
		{"decision envelope", func() signet.ClientCommand {
			in := refreshCommand("e", "pool-a")
			in.ExpectedEntityVersion = 1
			return in
		}(), signet.ErrInvalidOperatorCommandPayload},
	} {
		if _, err := svc.Submit(t.Context(), tc.command); !errors.Is(err, tc.want) {
			t.Errorf("%s = %v, want %v", tc.name, err, tc.want)
		}
	}
	if len(operator.refreshed)+len(operator.reassigns) != 0 || serverRevision(t, st) != before {
		t.Fatalf("refused commands reached the operator (%v %v) or moved the revision", operator.refreshed, operator.reassigns)
	}

	if _, err := svc.Submit(t.Context(), reassignCommand("f", "pool-a", "enrollment-usage-1")); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(operator.reassigns, []domain.ClientEnrollmentID{"enrollment-usage-1"}) {
		t.Fatalf("reassignments = %v", operator.reassigns)
	}
	// An operator failure rolls the receipt back with it.
	operator.err = errOperatorFails
	if _, err := svc.Submit(t.Context(), refreshCommand("g", "pool-a")); !errors.Is(err, errOperatorFails) {
		t.Fatalf("failed refresh = %v", err)
	}
	if serverRevision(t, st) != before+1 {
		t.Fatal("a failed refresh committed")
	}
}

// The operator's after-commit effect, such as starting a collection, runs
// once for each command that committed and for nothing else.
func TestUsageCommandsRunTheirEffectOnlyAfterCommit(t *testing.T) {
	t.Parallel()
	operator := &recordingUsageOperator{}
	svc, _, _ := newOperatorService(t, signet.WithUsageOperator(operator))
	submit := func(command signet.ClientCommand) error {
		_, err := svc.Submit(t.Context(), command)
		return err
	}
	for _, step := range []struct {
		name    string
		command signet.ClientCommand
		fails   bool
		afters  int
	}{
		{"committed refresh", refreshCommand("r", "pool-a"), false, 1},
		{"replayed refresh", refreshCommand("r", "pool-a"), false, 1},
		{"another body under the id", refreshCommand("r", "pool-idle"), true, 1},
		{"another kind under the id", reassignCommand("r", "pool-a", "enrollment-usage-1"), true, 1},
		{"committed reassignment", reassignCommand("a", "pool-a", "enrollment-usage-1"), false, 2},
		{"replayed reassignment", reassignCommand("a", "pool-a", "enrollment-usage-1"), false, 2},
		{"unknown pool", refreshCommand("u", "pool-missing"), true, 2},
		{"foreign enrollment", reassignCommand("f", "pool-idle", "enrollment-usage-1"), true, 2},
	} {
		if err := submit(step.command); (err != nil) != step.fails {
			t.Fatalf("%s = %v, want failure %v", step.name, err, step.fails)
		}
		if operator.afters != step.afters {
			t.Fatalf("after %s the effect ran %d times, want %d", step.name, operator.afters, step.afters)
		}
	}
	// An operator that fails rolls the transaction back, so its effect never
	// runs even though it returned one.
	operator.err = errOperatorFails
	for _, command := range []signet.ClientCommand{
		refreshCommand("x", "pool-a"), reassignCommand("y", "pool-a", "enrollment-usage-1"),
	} {
		if err := submit(command); !errors.Is(err, errOperatorFails) {
			t.Fatalf("failed %s = %v", command.Kind, err)
		}
	}
	if operator.afters != 2 {
		t.Fatalf("a rolled-back command ran its effect: %d runs, want 2", operator.afters)
	}
}

func TestChangeTaskLineRecordsRepeatsAndClears(t *testing.T) {
	t.Parallel()
	svc, st, id := newOperatorService(t, signet.WithTaskLineChanger(engineTaskLineChanger))
	steps := []struct {
		agent    *string
		recorded bool
	}{
		{new("codex-primary"), true},
		{new("codex-primary"), false},
		{nil, true},
		{nil, false},
	}
	for i, step := range steps {
		result, err := svc.Submit(t.Context(), lineCommand("line-"+string(rune('a'+i)), id, step.agent))
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		receipt := result.Receipt.TaskLineChange
		if receipt.Recorded != step.recorded {
			t.Fatalf("step %d recorded = %v, want %v", i, receipt.Recorded, step.recorded)
		}
		if step.agent == nil {
			if receipt.Line != nil {
				t.Fatalf("step %d line after a clear = %+v, want nil", i, receipt.Line)
			}
			continue
		}
		if receipt.Line == nil || receipt.Line.Selection == nil || *receipt.Line.Selection != lineSelectionOf(*step.agent) ||
			receipt.Line.Source != domain.TaskLineSourceClientLineChange || receipt.Line.Version != 1 {
			t.Fatalf("step %d line = %+v", i, receipt.Line)
		}
	}
	before := serverRevision(t, st)
	for name, command := range map[string]signet.ClientCommand{
		"unknown task": lineCommand("line-missing", "task-missing", new("codex-primary")),
	} {
		if _, err := svc.Submit(t.Context(), command); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("%s = %v, want ErrNotFound", name, err)
		}
	}
	ineligible := lineCommand("line-verifier", id, new("codex-primary"))
	ineligible.ChangeTaskLine.Role = "verifier"
	if _, err := svc.Submit(t.Context(), ineligible); !errors.Is(err, signet.ErrInvalidOperatorCommandPayload) ||
		!errors.Is(err, domain.ErrTaskLineRoleIneligible) {
		t.Errorf("ineligible role = %v", err)
	}
	if serverRevision(t, st) != before {
		t.Fatal("a refused line change committed")
	}
}

// A role whose attempt runs refuses the change with the current task
// projection, so the client can replace its stale copy.
func TestChangeTaskLineRefusesARunningRole(t *testing.T) {
	t.Parallel()
	running := func(context.Context, *store.WriteTx, signet.TaskLineChange, time.Time) (signet.TaskLineChangeOutcome, error) {
		return signet.TaskLineChangeOutcome{}, domain.ErrTaskLineRoleRunning
	}
	svc, st, id := newOperatorService(t, signet.WithTaskLineChanger(running))
	state, err := st.ServerState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.Submit(t.Context(), lineCommand("line-running", id, new("codex-primary")))
	var rejection *signet.TaskLineChangeRejectionError
	if !errors.As(err, &rejection) {
		t.Fatalf("running role = %v, want TaskLineChangeRejectionError", err)
	}
	if rejection.Reason != "role_running" || rejection.ReplacementTask.Task.ID != id ||
		rejection.SyncEpoch != state.SyncEpoch || rejection.ReplacementTask.AsOfRevision != state.Revision {
		t.Fatalf("rejection = %+v", rejection)
	}
	if serverRevision(t, st) != state.Revision {
		t.Fatal("a refused line change committed")
	}
}

func TestSetRoutingPolicyHoldsTheExpectedRevision(t *testing.T) {
	t.Parallel()
	svc, st, _ := newOperatorService(t, signet.WithRoutingPolicySetter(engineRoutingPolicySetter))
	first, err := svc.Submit(t.Context(), policyCommand("policy-1", nil, "codex-primary", "claude-primary"))
	if err != nil {
		t.Fatal(err)
	}
	policy := first.Receipt.RoutingPolicyChange.Policy
	if !first.Receipt.RoutingPolicyChange.Recorded || policy.Revision != 1 ||
		policy.Source != domain.RoutingPolicySourceClientSet || policy.SetBy != "device:device-1" ||
		policy.LineupRevision != operatorTreeRev {
		t.Fatalf("first policy = %+v", first.Receipt.RoutingPolicyChange)
	}

	before := serverRevision(t, st)
	_, err = svc.Submit(t.Context(), policyCommand("policy-stale", nil, "claude-primary"))
	var rejection *signet.RoutingPolicyRejectionError
	if !errors.As(err, &rejection) || rejection.CurrentPolicy == nil || !reflect.DeepEqual(*rejection.CurrentPolicy, policy) {
		t.Fatalf("stale expectation = %v, want the current policy", err)
	}
	for name, tc := range map[string]struct {
		command signet.ClientCommand
		want    error
	}{
		"unknown agent": {policyCommand("policy-unknown", new(1), "gemini"), domain.ErrRoutingPolicyAgentUnknown},
		"no agents":     {policyCommand("policy-empty", new(1)), signet.ErrInvalidOperatorCommandPayload},
		"zero expected": {policyCommand("policy-zero", new(0), "codex-primary"), signet.ErrInvalidOperatorCommandPayload},
		"unknown account": {func() signet.ClientCommand {
			in := policyCommand("policy-unknown-account", new(1), "codex-primary")
			in.SetRoutingPolicy.DrainFirstAccount = new(domain.UsagePoolID("pool-idle"))
			return in
		}(), domain.ErrRoutingPolicyDrainFirstUnknown},
		"empty account": {func() signet.ClientCommand {
			in := policyCommand("policy-empty-account", new(1), "codex-primary")
			in.SetRoutingPolicy.DrainFirstAccount = new(domain.UsagePoolID(""))
			return in
		}(), signet.ErrInvalidOperatorCommandPayload},
		"another kind's body": {func() signet.ClientCommand {
			in := policyCommand("policy-mixed", new(1), "codex-primary")
			in.RefreshUsage = signet.RefreshUsagePayload{Pool: "pool-a"}
			return in
		}(), signet.ErrInvalidOperatorCommandPayload},
	} {
		if _, err := svc.Submit(t.Context(), tc.command); !errors.Is(err, tc.want) {
			t.Errorf("%s = %v, want %v", name, err, tc.want)
		}
	}
	if serverRevision(t, st) != before {
		t.Fatal("a refused policy change committed")
	}

	// The orders the current revision holds record no new revision; the
	// receipt still commits.
	repeat, err := svc.Submit(t.Context(), policyCommand("policy-repeat", new(1), "codex-primary", "claude-primary"))
	if err != nil {
		t.Fatal(err)
	}
	if repeat.Receipt.RoutingPolicyChange.Recorded || !reflect.DeepEqual(repeat.Receipt.RoutingPolicyChange.Policy, policy) {
		t.Fatalf("repeat = %+v", repeat.Receipt.RoutingPolicyChange)
	}
	next, err := svc.Submit(t.Context(), policyCommand("policy-2", new(1), "claude-primary"))
	if err != nil {
		t.Fatal(err)
	}
	if got := next.Receipt.RoutingPolicyChange.Policy; got.Revision != 2 || !got.SameRule(
		[]domain.RoutingPolicyRole{{Role: domain.RoleImplementer, Agents: []string{"claude-primary"}}}, nil) {
		t.Fatalf("second policy = %+v", got)
	}

	// The same orders with a drain-first account are a new revision. The
	// account is pool-a, codex-primary's pool, though the order names only
	// claude-primary: whether a role can use it is the router's question.
	drained := policyCommand("policy-3", new(2), "claude-primary")
	drained.SetRoutingPolicy.DrainFirstAccount = new(domain.UsagePoolID("pool-a"))
	third, err := svc.Submit(t.Context(), drained)
	if err != nil {
		t.Fatal(err)
	}
	if got := third.Receipt.RoutingPolicyChange; !got.Recorded || got.Policy.Revision != 3 ||
		got.Policy.DrainFirstAccount == nil || *got.Policy.DrainFirstAccount != "pool-a" {
		t.Fatalf("drain-first policy = %+v", got)
	}
	if replay, err := svc.Submit(t.Context(), drained); err != nil || !reflect.DeepEqual(replay.Receipt, third.Receipt) {
		t.Fatalf("replay = %+v, %v, want the recorded receipt", replay.Receipt, err)
	}
	// The account is part of the request, so the command id with another
	// account is another command.
	reused := drained
	reused.SetRoutingPolicy.DrainFirstAccount = nil
	if _, err := svc.Submit(t.Context(), reused); !errors.Is(err, store.ErrImmutableConflict) {
		t.Fatalf("reused id with no account = %v, want %v", err, store.ErrImmutableConflict)
	}
}

// A service composed without a hook fails closed: the command records nothing,
// and the request's own refusals still come first.
func TestOperatorCommandsFailClosedWithoutTheirHook(t *testing.T) {
	t.Parallel()
	svc, st, id := newOperatorService(t)
	before := serverRevision(t, st)
	for _, tc := range []struct {
		command signet.ClientCommand
		want    error
	}{
		{refreshCommand("a", "pool-a"), signet.ErrUsageOperationUnavailable},
		{reassignCommand("b", "pool-a", "enrollment-usage-1"), signet.ErrUsageOperationUnavailable},
		{reassignCommand("c", "pool-idle", "enrollment-usage-1"), store.ErrEnrollmentNotOnPool},
		{lineCommand("d", id, new("codex-primary")), signet.ErrOperatorSettingUnavailable},
		{policyCommand("e", nil, "codex-primary"), signet.ErrOperatorSettingUnavailable},
	} {
		if _, err := svc.Submit(t.Context(), tc.command); !errors.Is(err, tc.want) {
			t.Errorf("%s %s = %v, want %v", tc.command.Kind, tc.command.CommandID, err, tc.want)
		}
	}
	if serverRevision(t, st) != before {
		t.Fatal("a command without its hook committed")
	}

	// An id another kind holds is refused before the hook is consulted, so
	// it answers as a reused id, not as an unavailable operation.
	if _, err := svc.Submit(t.Context(), stopCommand(t, st, id, "stopped")); err != nil {
		t.Fatal(err)
	}
	for _, command := range []signet.ClientCommand{
		refreshCommand("stopped", "pool-a"), lineCommand("stopped", id, new("codex-primary")),
		policyCommand("stopped", nil, "codex-primary"),
	} {
		if _, err := svc.Submit(t.Context(), command); !errors.Is(err, store.ErrImmutableConflict) {
			t.Errorf("%s under a Stop's command_id = %v, want ErrImmutableConflict", command.Kind, err)
		}
	}
}

func TestOperatorCommandsHTTP(t *testing.T) {
	t.Parallel()
	svc, st, id := newOperatorService(t, signet.WithUsageOperator(&recordingUsageOperator{}),
		signet.WithTaskLineChanger(engineTaskLineChanger), signet.WithRoutingPolicySetter(engineRoutingPolicySetter))
	handler := signet.NewHTTPHandler(svc, testAuthorizer)
	post := func(t *testing.T, body string) (int, map[string]any) {
		t.Helper()
		response := authenticatedRequest(t, handler, http.MethodPost, "/commands", bytes.NewReader([]byte(body)))
		var decoded map[string]any
		if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
			t.Fatalf("decode %q: %v", response.Body.String(), err)
		}
		return response.Code, decoded
	}
	command := func(commandID, payload string) string {
		return `{"command_id":"` + commandID + `","device_id":"device-1","payload":` + payload + `}`
	}
	task := string(id)

	before := serverRevision(t, st)
	for name, tc := range map[string]struct {
		body string
		want int
	}{
		"mistyped selection": {command("h1", `{"kind":"change_task_line","task_id":"`+task+`","role":"implementer","selection":7}`), 400},
		"agent name":         {command("h1a", `{"kind":"change_task_line","task_id":"`+task+`","role":"implementer","agent":"codex-primary"}`), 400},
		"missing roles":      {command("h2", `{"kind":"set_routing_policy","expected_revision":null}`), 400},
		"unknown member":     {command("h3", `{"kind":"refresh_usage","pool":"pool-a","force":true}`), 400},
		"envelope version": {`{"command_id":"h4","device_id":"device-1","expected_entity_version":1,` +
			`"payload":{"kind":"refresh_usage","pool":"pool-a"}}`, 400},
		"envelope bindings": {`{"command_id":"h5","device_id":"device-1","expected_bindings":{},` +
			`"payload":{"kind":"refresh_usage","pool":"pool-a"}}`, 400},
		"unknown pool":         {command("h6", `{"kind":"refresh_usage","pool":"pool-missing"}`), 404},
		"unknown task":         {command("h7", `{"kind":"change_task_line","task_id":"task-missing","role":"implementer","selection":null}`), 404},
		"foreign enrollment":   {command("h8", `{"kind":"reassign_collector","pool":"pool-idle","enrollment_id":"enrollment-usage-1"}`), 400},
		"unknown agent":        {command("h9", `{"kind":"set_routing_policy","roles":[{"role":"implementer","agents":["gemini"]}],"expected_revision":null}`), 400},
		"repeated agent":       {command("h10", `{"kind":"set_routing_policy","roles":[{"role":"implementer","agents":["codex-primary","codex-primary"]}],"expected_revision":null}`), 400},
		"ineligible line role": {command("h11", `{"kind":"change_task_line","task_id":"`+task+`","role":"verifier","selection":{"enrollment_id":"line-codex-primary/codex_cli","model_id":"model-codex-primary","effort":"high"}}`), 400},
	} {
		t.Run(name, func(t *testing.T) {
			if code, body := post(t, tc.body); code != tc.want {
				t.Fatalf("status %d want %d: %v", code, tc.want, body)
			}
		})
	}
	if serverRevision(t, st) != before {
		t.Fatal("a refused request committed")
	}

	code, body := post(t, command("h-line", `{"kind":"change_task_line","task_id":"`+task+`","role":"implementer","selection":{"enrollment_id":"line-codex-primary/codex_cli","model_id":"model-codex-primary","effort":"high"}}`))
	record, _ := body["record"].(map[string]any)
	line, _ := record["line"].(map[string]any)
	selection, _ := line["selection"].(map[string]any)
	if code != 200 || record["kind"] != "change_task_line" || record["recorded"] != true || record["device_id"] != "device-1" ||
		line["agent"] != nil || selection["model_id"] != "model-codex-primary" || line["source"] != "client_line_change" ||
		body["revision"] != float64(before+1) {
		t.Fatalf("line change: %d %v", code, body)
	}
	// The generated client omits a nil selection, so an absent selection clears.
	code, body = post(t, command("h-clear", `{"kind":"change_task_line","task_id":"`+task+`","role":"implementer"}`))
	if record, _ := body["record"].(map[string]any); code != 200 || record["selection"] != nil || record["line"] != nil {
		t.Fatalf("line clear: %d %v", code, body)
	}
	code, body = post(t, command("h-policy", `{"kind":"set_routing_policy","roles":[{"role":"implementer","agents":["codex-primary"]}]}`))
	if record, _ := body["record"].(map[string]any); code != 200 || record["kind"] != "set_routing_policy" || record["expected_revision"] != nil {
		t.Fatalf("policy set: %d %v", code, body)
	}
	code, body = post(t, command("h-stale", `{"kind":"set_routing_policy","roles":[{"role":"implementer","agents":["claude-primary"]}],"expected_revision":null}`))
	if current, _ := body["current_policy"].(map[string]any); code != 409 || body["message"] == "" || current["revision"] != float64(1) {
		t.Fatalf("stale policy: %d %v", code, body)
	}
	code, body = post(t, command("h-drain-first", `{"kind":"set_routing_policy","roles":[{"role":"implementer","agents":["codex-primary"]}],"drain_first_account":"pool-a","expected_revision":1}`))
	if record, _ := body["record"].(map[string]any); code != 200 || record["recorded"] != true {
		t.Fatalf("drain-first set: %d %v", code, body)
	} else if policy, _ := record["policy"].(map[string]any); policy["drain_first_account"] != "pool-a" {
		t.Fatalf("drain-first policy: %v", record)
	}
	code, body = post(t, command("h-drain-unknown", `{"kind":"set_routing_policy","roles":[{"role":"implementer","agents":["codex-primary"]}],"drain_first_account":"pool-idle","expected_revision":2}`))
	if code != 400 {
		t.Fatalf("unknown drain-first account: %d %v", code, body)
	}
	code, body = post(t, command("h-refresh", `{"kind":"refresh_usage","pool":"pool-a"}`))
	if record, _ := body["record"].(map[string]any); code != 200 || record["kind"] != "refresh_usage" || record["pool"] != "pool-a" ||
		record["requested_at"] != "2026-01-02T03:04:05Z" {
		t.Fatalf("refresh: %d %v", code, body)
	}

	running := func(context.Context, *store.WriteTx, signet.TaskLineChange, time.Time) (signet.TaskLineChangeOutcome, error) {
		return signet.TaskLineChangeOutcome{}, domain.ErrTaskLineRoleRunning
	}
	runningSvc, _, runningID := newOperatorService(t, signet.WithTaskLineChanger(running))
	response := authenticatedRequest(t, signet.NewHTTPHandler(runningSvc, testAuthorizer), http.MethodPost, "/commands",
		bytes.NewReader([]byte(command("h-running", `{"kind":"change_task_line","task_id":"`+string(runningID)+`","role":"implementer","selection":{"enrollment_id":"line-codex-primary/codex_cli","model_id":"model-codex-primary","effort":"high"}}`))))
	var rejection struct {
		Message         string              `json:"message"`
		Reason          string              `json:"reason"`
		ReplacementTask signet.TaskSnapshot `json:"replacement_task"`
		SyncEpoch       string              `json:"sync_epoch"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &rejection); err != nil {
		t.Fatal(err)
	}
	if response.Code != 409 || rejection.Message == "" || rejection.Reason != "role_running" ||
		rejection.ReplacementTask.Task.ID != runningID || rejection.SyncEpoch == "" {
		t.Fatalf("running role: %d %s", response.Code, response.Body.String())
	}
	unwired, _, _ := newOperatorService(t)
	response = authenticatedRequest(t, signet.NewHTTPHandler(unwired, testAuthorizer), http.MethodPost, "/commands",
		bytes.NewReader([]byte(command("h-unwired", `{"kind":"refresh_usage","pool":"pool-a"}`))))
	if response.Code != 500 {
		t.Fatalf("unwired refresh: %d %s", response.Code, response.Body.String())
	}
}
