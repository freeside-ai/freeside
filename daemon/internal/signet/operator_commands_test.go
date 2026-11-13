package signet_test

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"reflect"
	"slices"
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
	// operatorLineup is the implementer's lineup line: the model and effort
	// a policy's enrollments run on.
	operatorLineup = domain.TaskLineSelection{EnrollmentID: "line-other/codex_cli", ModelID: "model-lineup", Effort: "high"}
	operatorTree   = agenttree.Tree{Lineup: []agenttree.LineupLine{{
		Key: string(domain.RoleImplementer),
		Selection: domain.LineupSelection{
			Names: domain.AgentSelectionNames{
				Enrollment: string(operatorLineup.EnrollmentID), Model: operatorLineup.ModelID, Effort: operatorLineup.Effort,
			},
			PromptName: "implementer", PromptDigest: domain.Digest("sha256:" + strings.Repeat("b", 64)),
		},
	}}}
	operatorTreeRev = domain.Digest("sha256:" + strings.Repeat("a", 64))
	// operatorCatalog is operatorTree's catalog.
	operatorCatalog = signet.AgentCatalog{
		LineupRevision: &operatorTreeRev,
		Lineup:         []signet.AgentCatalogLineupLine{{Role: domain.RoleImplementer, Selection: operatorLineup}},
	}
	withOperatorCatalog = signet.WithAgentCatalog(func() *signet.AgentCatalog { return &operatorCatalog })
	errOperatorFails    = errors.New("operator failed")
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
	return newOperatorServiceAt(t, filepath.Join(t.TempDir(), "store.db"), opts...)
}

// newOperatorServiceAt is newOperatorService over the store at path, for a
// test that writes a row the store's own API no longer writes.
func newOperatorServiceAt(
	t *testing.T, path string, opts ...signet.Option,
) (*signet.Service, *store.Store, domain.TaskID) {
	t.Helper()
	st := storetest.Open(t, path, store.Options{
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

// moveCommand moves task to enrollment at the lineup's model and effort.
func moveCommand(id string, task domain.TaskID, enrollment domain.ClientEnrollmentID) signet.ClientCommand {
	in := operatorCommand(id, domain.CommandKindMoveParkedTask)
	in.MoveParkedTask = signet.MoveParkedTaskPayload{TaskID: task, Choice: &signet.AlternateAgentChoice{EnrollmentID: enrollment}}
	return in
}

// fakeParkedTaskMover answers a move with the receipt a move to the request's
// choice on pool-b would record, under a lineup of claude-move-test at high
// effort, writing nothing: the engine's move has its own tests, and these
// exercise only the command's envelope.
func fakeParkedTaskMover(
	_ context.Context, _ *store.WriteTx, request domain.ParkedTaskMoveRequest, now time.Time,
) (domain.ParkedTaskMoveReceipt, error) {
	selection := &domain.TaskLineSelection{
		EnrollmentID: request.Choice.EnrollmentID,
		ModelID:      cmp.Or(request.Choice.ModelID, "claude-move-test"),
		Effort:       cmp.Or(request.Choice.Effort, "high"),
	}
	record, err := domain.NewAlternateAgentRetry(domain.AlternateAgentRetry{
		TaskID: request.TaskID, Role: domain.RoleImplementer, FailedInvocationID: "inv-refused",
		CommandID: request.CommandID, Selection: selection, Pool: "pool-b",
		Qualification: domain.AlternateAgentDifferentPool, RecordedAt: now,
	})
	if err != nil {
		return domain.ParkedTaskMoveReceipt{}, err
	}
	line, err := domain.NewTaskLine(domain.TaskLineInput{
		TaskID: request.TaskID, Role: domain.RoleImplementer, Selection: selection,
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

// The enrollments operatorCatalog's agents run: codex-primary's on pool-a
// and claude-primary's on its identity's own pool.
const (
	codexPrimary  domain.ClientEnrollmentID = "enrollment-usage-1"
	claudePrimary domain.ClientEnrollmentID = "line-claude-primary/codex_cli"
)

func policyCommand(id string, expected *int, enrollments ...domain.ClientEnrollmentID) signet.ClientCommand {
	in := operatorCommand(id, domain.CommandKindSetRoutingPolicy)
	in.SetRoutingPolicy = signet.SetRoutingPolicyPayload{
		Roles:            []signet.RoutingPolicyRoleInput{{Role: domain.RoleImplementer, Enrollments: enrollments}},
		ExpectedRevision: expected,
	}
	return in
}

// wantEnrollments checks that a routing policy receipt renders the
// implementer's order on the wire as enrollments, with agents null.
func wantEnrollments(t *testing.T, result signet.CommandResult, enrollments ...domain.ClientEnrollmentID) {
	t.Helper()
	body, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Record struct {
			Policy signet.RoutingPolicyWire `json:"policy"`
		} `json:"record"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatal(err)
	}
	if roles := wire.Record.Policy.Roles; len(roles) != 1 || !slices.Equal(roles[0].Enrollments, enrollments) ||
		roles[0].Agents != nil || !bytes.Contains(body, []byte(`"agents":null`)) {
		t.Fatalf("receipt on the wire = %s, want enrollments %v", body, enrollments)
	}
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
			func(domain.TaskID) signet.ClientCommand { return policyCommand("cmd", nil, codexPrimary) },
			func(domain.TaskID) signet.ClientCommand { return policyCommand("cmd", nil, claudePrimary) },
		},
		{
			domain.CommandKindSetWorkflowPolicy,
			func(domain.TaskID) signet.ClientCommand {
				return workflowCommand("cmd", domain.WorkflowPolicyScopeGeneral, nil, domain.WorkflowGateIssueComment, new("true"), nil)
			},
			func(domain.TaskID) signet.ClientCommand {
				return workflowCommand("cmd", domain.WorkflowPolicyScopeGeneral, nil, domain.WorkflowGateIssueComment, new("false"), nil)
			},
		},
		{
			domain.CommandKindMoveParkedTask,
			func(id domain.TaskID) signet.ClientCommand { return moveCommand("cmd", id, "move-claude/claude_code") },
			func(id domain.TaskID) signet.ClientCommand { return moveCommand("cmd", id, "other-claude/claude_code") },
		},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			svc, st, id := newOperatorService(t, signet.WithUsageOperator(&recordingUsageOperator{}),
				signet.WithTaskLineChanger(engineTaskLineChanger), signet.WithRoutingPolicySetter(engineRoutingPolicySetter),
				withOperatorCatalog, signet.WithParkedTaskMover(fakeParkedTaskMover),
				signet.WithWorkflowPolicySetter(engineWorkflowPolicySetter))
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

// A move's choice is refused before the mover runs when it is missing or
// malformed; the refusal names the field and never echoes the value.
func TestMoveParkedTaskRefusesAMalformedChoice(t *testing.T) {
	t.Parallel()
	svc, st, id := newOperatorService(t, signet.WithParkedTaskMover(fakeParkedTaskMover))
	before := serverRevision(t, st)
	empty, secret := "", "secret text"
	for name, tc := range map[string]struct {
		choice *signet.AlternateAgentChoice
		field  string
	}{
		"no choice":        {nil, ""},
		"no enrollment":    {&signet.AlternateAgentChoice{}, "enrollment_id"},
		"an empty model":   {&signet.AlternateAgentChoice{EnrollmentID: "move-claude/claude_code", ModelID: &empty}, "model_id"},
		"an invalid model": {&signet.AlternateAgentChoice{EnrollmentID: "move-claude/claude_code", ModelID: &secret}, "model_id"},
		"an empty effort":  {&signet.AlternateAgentChoice{EnrollmentID: "move-claude/claude_code", Effort: &empty}, "effort"},
	} {
		command := moveCommand("cmd-"+strings.ReplaceAll(name, " ", "-"), id, "")
		command.MoveParkedTask.Choice = tc.choice
		_, err := svc.Submit(t.Context(), command)
		if !errors.Is(err, signet.ErrInvalidOperatorCommandPayload) || !strings.Contains(err.Error(), tc.field) ||
			strings.Contains(err.Error(), secret) {
			t.Errorf("%s: error = %v, want invalid naming %q and not the text", name, err, tc.field)
		}
	}
	if serverRevision(t, st) != before {
		t.Fatal("a malformed move committed")
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
	svc, st, _ := newOperatorService(t, signet.WithRoutingPolicySetter(engineRoutingPolicySetter), withOperatorCatalog)
	first, err := svc.Submit(t.Context(), policyCommand("policy-1", nil, codexPrimary, claudePrimary))
	if err != nil {
		t.Fatal(err)
	}
	policy := first.Receipt.RoutingPolicyChange.Policy
	if !first.Receipt.RoutingPolicyChange.Recorded || policy.Revision != 1 ||
		policy.Source != domain.RoutingPolicySourceClientSet || policy.SetBy != "device:device-1" ||
		policy.LineupRevision != operatorTreeRev || !policy.SameRule([]domain.RoutingPolicyRole{{
		Role: domain.RoleImplementer, Enrollments: []domain.ClientEnrollmentID{codexPrimary, claudePrimary},
	}}, nil) {
		t.Fatalf("first policy = %+v", first.Receipt.RoutingPolicyChange)
	}
	wantEnrollments(t, first, codexPrimary, claudePrimary)

	before := serverRevision(t, st)
	_, err = svc.Submit(t.Context(), policyCommand("policy-stale", nil, claudePrimary))
	var rejection *signet.RoutingPolicyRejectionError
	if !errors.As(err, &rejection) || rejection.CurrentPolicy == nil || rejection.CurrentPolicy.Revision != 1 ||
		!reflect.DeepEqual(rejection.CurrentPolicy.Roles, []signet.RoutingPolicyRoleWire{{
			Role: domain.RoleImplementer, Enrollments: []domain.ClientEnrollmentID{codexPrimary, claudePrimary},
		}}) {
		t.Fatalf("stale expectation = %v, want the current policy", err)
	}
	// The setter refuses an enrollment the store does not hold by the role
	// and the entry's position; the id, here a pasted credential, never
	// comes back.
	pasted := domain.ClientEnrollmentID(pastedRole)
	if _, err := svc.Submit(t.Context(), policyCommand("policy-unknown", new(1), codexPrimary, pasted)); !errors.Is(
		err, domain.ErrRoutingPolicyEnrollmentUnknown) || !strings.Contains(err.Error(), "role implementer enrollment 2") ||
		strings.Contains(err.Error(), pastedRole) {
		t.Errorf("unknown enrollment = %v, want %v by position", err, domain.ErrRoutingPolicyEnrollmentUnknown)
	}
	for name, tc := range map[string]struct {
		command signet.ClientCommand
		want    error
	}{
		"no enrollments": {policyCommand("policy-empty", new(1)), signet.ErrInvalidOperatorCommandPayload},
		"repeated enrollment": {
			policyCommand("policy-repeated", new(1), codexPrimary, codexPrimary), domain.ErrRoutingPolicyEnrollmentRepeated,
		},
		"zero expected": {policyCommand("policy-zero", new(0), codexPrimary), signet.ErrInvalidOperatorCommandPayload},
		"unknown account": {func() signet.ClientCommand {
			in := policyCommand("policy-unknown-account", new(1), codexPrimary)
			in.SetRoutingPolicy.DrainFirstAccount = new(domain.UsagePoolID("pool-idle"))
			return in
		}(), domain.ErrRoutingPolicyDrainFirstUnknown},
		"empty account": {func() signet.ClientCommand {
			in := policyCommand("policy-empty-account", new(1), codexPrimary)
			in.SetRoutingPolicy.DrainFirstAccount = new(domain.UsagePoolID(""))
			return in
		}(), signet.ErrInvalidOperatorCommandPayload},
		"another kind's body": {func() signet.ClientCommand {
			in := policyCommand("policy-mixed", new(1), codexPrimary)
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
	repeat, err := svc.Submit(t.Context(), policyCommand("policy-repeat", new(1), codexPrimary, claudePrimary))
	if err != nil {
		t.Fatal(err)
	}
	if repeat.Receipt.RoutingPolicyChange.Recorded || !reflect.DeepEqual(repeat.Receipt.RoutingPolicyChange.Policy, policy) {
		t.Fatalf("repeat = %+v", repeat.Receipt.RoutingPolicyChange)
	}
	next, err := svc.Submit(t.Context(), policyCommand("policy-2", new(1), claudePrimary))
	if err != nil {
		t.Fatal(err)
	}
	if got := next.Receipt.RoutingPolicyChange.Policy; got.Revision != 2 || !got.SameRule(
		[]domain.RoutingPolicyRole{{
			Role: domain.RoleImplementer, Enrollments: []domain.ClientEnrollmentID{claudePrimary},
		}}, nil) {
		t.Fatalf("second policy = %+v", got)
	}

	// The same orders with a drain-first account are a new revision. The
	// account is the pool of claude-primary's identity, the one enrollment
	// the order names.
	drained := policyCommand("policy-3", new(2), claudePrimary)
	drained.SetRoutingPolicy.DrainFirstAccount = new(domain.UsagePoolID("identity:line-claude-primary"))
	third, err := svc.Submit(t.Context(), drained)
	if err != nil {
		t.Fatal(err)
	}
	if got := third.Receipt.RoutingPolicyChange; !got.Recorded || got.Policy.Revision != 3 ||
		got.Policy.DrainFirstAccount == nil || *got.Policy.DrainFirstAccount != "identity:line-claude-primary" {
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

// TestRoutingPolicyRendersAPreEnrollmentRevision: a revision stored before
// routing policy ordered enrollments (#2736) holds agent names, and the wire
// carries them as stored, with enrollments null, in the bootstrap snapshot
// and in a stale-revision rejection. Nothing translates them through a tree.
func TestRoutingPolicyRendersAPreEnrollmentRevision(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "store.db")
	svc, _, _ := newOperatorServiceAt(t, path, signet.WithRoutingPolicySetter(engineRoutingPolicySetter))
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(t.Context(), `
INSERT INTO routing_policies (revision, source, set_by, set_at, lineup_revision, drain_first_account, body)
VALUES (1, 'cli_routing_set', 'cli:operator', '2026-01-02T03:04:05Z', ?, NULL, ?)`, string(operatorTreeRev),
		`{"revision":1,"roles":[{"role":"implementer","agents":["codex-primary","claude-primary"]}],`+
			`"source":"cli_routing_set","set_by":"cli:operator","set_at":"2026-01-02T03:04:05Z",`+
			`"lineup_revision":"`+string(operatorTreeRev)+`","drain_first_account":null}`); err != nil {
		t.Fatal(err)
	}
	want := []signet.RoutingPolicyRoleWire{{Role: domain.RoleImplementer, Agents: []string{"codex-primary", "claude-primary"}}}

	snapshot, err := svc.Bootstrap(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(snapshot.RoutingPolicy)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.RoutingPolicy == nil || !reflect.DeepEqual(snapshot.RoutingPolicy.Policy.Roles, want) ||
		!bytes.Contains(body, []byte(`"enrollments":null`)) {
		t.Fatalf("bootstrap routing policy = %s, want the agents form", body)
	}

	_, err = svc.Submit(t.Context(), policyCommand("policy-stale", nil, claudePrimary))
	var rejection *signet.RoutingPolicyRejectionError
	if !errors.As(err, &rejection) || rejection.CurrentPolicy == nil ||
		!reflect.DeepEqual(rejection.CurrentPolicy.Roles, want) {
		t.Fatalf("stale expectation = %v, want the agents-form policy", err)
	}

	// A change over it records enrollments, and the receipt renders them.
	next, err := svc.Submit(t.Context(), policyCommand("policy-2", new(1), claudePrimary))
	if err != nil {
		t.Fatal(err)
	}
	wantEnrollments(t, next, claudePrimary)
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
		{policyCommand("e", nil, codexPrimary), signet.ErrOperatorSettingUnavailable},
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
		policyCommand("stopped", nil, codexPrimary),
	} {
		if _, err := svc.Submit(t.Context(), command); !errors.Is(err, store.ErrImmutableConflict) {
			t.Errorf("%s under a Stop's command_id = %v, want ErrImmutableConflict", command.Kind, err)
		}
	}
}

// pastedRole reads like a credential pasted where a role belongs.
const pastedRole = "sk-ant-oat01-fixture-not-a-token"

func TestOperatorCommandsHTTP(t *testing.T) {
	t.Parallel()
	svc, st, id := newOperatorService(t, signet.WithUsageOperator(&recordingUsageOperator{}),
		signet.WithTaskLineChanger(engineTaskLineChanger), signet.WithRoutingPolicySetter(engineRoutingPolicySetter), withOperatorCatalog)
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

	// The setter refuses an enrollment whose identity is disabled, and one
	// whose discovery lacks the implementer's lineup model, with 400 by the
	// role and the entry's position, never the id.
	const disabled, unserved domain.ClientEnrollmentID = "line-codex-pro/codex_cli", "line-codex-reviewer/codex_cli"
	if err := st.Write(t.Context(), func(tx *store.WriteTx) error {
		identity, err := tx.GetAuthIdentity(t.Context(), "line-codex-pro")
		if err != nil {
			return err
		}
		identity.Enabled = false
		if err := tx.RecordAuthIdentity(t.Context(), identity, operatorAt.Add(time.Minute)); err != nil {
			return err
		}
		return tx.RecordDiscoveredModelCatalog(t.Context(), domain.DiscoveredModelCatalog{
			EnrollmentID: unserved, HarnessBuild: "codex-cli 0.50.0", ObservedAt: operatorAt.Add(-time.Minute),
			Models: []domain.DiscoveredModel{{ID: "model-other", DisplayName: "Other", EffortLevels: []string{"high"}}},
		})
	}); err != nil {
		t.Fatal(err)
	}
	for name, enrollment := range map[string]domain.ClientEnrollmentID{"disabled": disabled, "unserved": unserved} {
		code, body := post(t, command("h-"+name, `{"kind":"set_routing_policy","roles":[{"role":"implementer","enrollments":["enrollment-usage-1","`+string(enrollment)+`"]}],"expected_revision":null}`))
		if message, _ := body["message"].(string); code != 400 || !strings.Contains(message, "role implementer enrollment 2") ||
			strings.Contains(fmt.Sprint(body), string(enrollment)) {
			t.Errorf("%s enrollment: %d %v, want 400 by position", name, code, body)
		}
	}

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
		"unknown enrollment":   {command("h9", `{"kind":"set_routing_policy","roles":[{"role":"implementer","enrollments":["`+pastedRole+`"]}],"expected_revision":null}`), 400},
		"repeated enrollment":  {command("h10", `{"kind":"set_routing_policy","roles":[{"role":"implementer","enrollments":["enrollment-usage-1","enrollment-usage-1"]}],"expected_revision":null}`), 400},
		"ineligible line role": {command("h11", `{"kind":"change_task_line","task_id":"`+task+`","role":"verifier","selection":{"enrollment_id":"line-codex-primary/codex_cli","model_id":"model-codex-primary","effort":"high"}}`), 400},
		// A role orders enrollments only; the strict decoder refuses agents.
		"agents member":      {command("h12", `{"kind":"set_routing_policy","roles":[{"role":"implementer","agents":["codex-primary"]}],"expected_revision":null}`), 400},
		"pasted role":        {command("h13", `{"kind":"set_routing_policy","roles":[{"role":"`+pastedRole+`","enrollments":["enrollment-usage-1"]}],"expected_revision":null}`), 400},
		"pasted role repeat": {command("h14", `{"kind":"set_routing_policy","roles":[{"role":"`+pastedRole+`","enrollments":["enrollment-usage-1","enrollment-usage-1"]}],"expected_revision":null}`), 400},
	} {
		t.Run(name, func(t *testing.T) {
			code, body := post(t, tc.body)
			if code != tc.want {
				t.Fatalf("status %d want %d: %v", code, tc.want, body)
			}
			if strings.Contains(fmt.Sprint(body), pastedRole) {
				t.Fatalf("refusal repeats the request's role: %v", body)
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
	code, body = post(t, command("h-policy", `{"kind":"set_routing_policy","roles":[{"role":"implementer","enrollments":["enrollment-usage-1"]}]}`))
	if record, _ := body["record"].(map[string]any); code != 200 || record["kind"] != "set_routing_policy" || record["expected_revision"] != nil {
		t.Fatalf("policy set: %d %v", code, body)
	}
	code, body = post(t, command("h-stale", `{"kind":"set_routing_policy","roles":[{"role":"implementer","enrollments":["line-claude-primary/codex_cli"]}],"expected_revision":null}`))
	if current, _ := body["current_policy"].(map[string]any); code != 409 || body["message"] == "" || current["revision"] != float64(1) {
		t.Fatalf("stale policy: %d %v", code, body)
	}
	code, body = post(t, command("h-drain-first", `{"kind":"set_routing_policy","roles":[{"role":"implementer","enrollments":["enrollment-usage-1"]}],"drain_first_account":"pool-a","expected_revision":1}`))
	if record, _ := body["record"].(map[string]any); code != 200 || record["recorded"] != true {
		t.Fatalf("drain-first set: %d %v", code, body)
	} else if policy, _ := record["policy"].(map[string]any); policy["drain_first_account"] != "pool-a" {
		t.Fatalf("drain-first policy: %v", record)
	}
	code, body = post(t, command("h-drain-unknown", `{"kind":"set_routing_policy","roles":[{"role":"implementer","enrollments":["enrollment-usage-1"]}],"drain_first_account":"pool-idle","expected_revision":2}`))
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
