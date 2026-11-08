package integration_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/agentbaseline"
	"github.com/freeside-ai/freeside/daemon/internal/agenttree"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/specify"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

// The issue-#869 scenario: an implementation fails on its agent's expired
// credential, the engine's failure card offers a second enrollment on its
// own identity, which the wire still names by its tree agent (#2735), the
// operator picks it, and the retry attempt runs that agent. The
// proof is the stored admission of the retry under the second agent's
// enrollment, and an attempt that cites any record but the operator's is
// refused where it is written.

const (
	alternateRetryCommandID = "command-alternate-agent"
	alternateRetryAgent     = "claude-second"
	alternateRetryModel     = "claude-alternate-test"
)

// alternateAgentIdentity is one adopted Claude identity: its own pool, one
// claude_code enrollment, and a current generation.
type alternateAgentIdentity struct {
	identity   domain.AuthIdentity
	enrollment domain.ClientEnrollment
}

func newAlternateAgentIdentity(id domain.AuthIdentityID, account string) alternateAgentIdentity {
	identity := domain.AuthIdentity{
		ID: id, Provider: "claude", AccountBinding: account,
		AuthStoreMutationLease: true, Enabled: true, CostOwner: "operator",
		Interim: domain.InterimClientFacts{
			AuthStoreVolume: string(id) + "-interim", RefreshStrategy: domain.RefreshExternal,
		},
	}
	return alternateAgentIdentity{identity: identity, enrollment: domain.ClientEnrollment{
		ID: domain.ClientEnrollmentID(string(id) + "/claude_code"), AuthIdentityID: id,
		HarnessClient: domain.HarnessClientClaudeCode, Route: "anthropic-subscription",
		AuthMethod: domain.AuthMethodSetupToken, CredentialMode: domain.CredentialSubscriptionContained,
		RefreshStrategy: domain.RefreshExternal, AccountBinding: account,
	}}
}

func (a alternateAgentIdentity) seed(t *testing.T, st *store.Store) {
	t.Helper()
	ctx := context.Background()
	manifest := productionDigest([]byte(string(a.identity.ID) + " auth store"))
	volume := string(a.identity.ID) + "-generation"
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := storetest.RecordIdentityOnPool(ctx, tx, a.identity, testPoolLimit, admittedAt.Add(-4*time.Minute)); err != nil {
			return err
		}
		if err := tx.RecordClientEnrollment(ctx, a.enrollment, admittedAt.Add(-3*time.Minute)); err != nil {
			return err
		}
		lease, err := tx.AcquireAuthStoreMutationLeaseBound(ctx, a.identity.ID, domain.InvocationID("inv-adopt-"+string(a.identity.ID)),
			&domain.LeaseGenerationBinding{
				EnrollmentID: a.enrollment.ID, AuthStoreVolume: volume, StoreManifestDigest: manifest,
			}, admittedAt.Add(-2*time.Minute), admittedAt.Add(10*time.Minute))
		if err != nil {
			return err
		}
		_, err = tx.AppendEnrollmentGeneration(ctx, domain.EnrollmentGeneration{
			EnrollmentID: a.enrollment.ID, AuthStoreVolume: volume,
			StoreManifestDigest: manifest, LeaseFence: lease.Fence,
			AccountBinding: a.identity.AccountBinding, RecordedAt: admittedAt.Add(-time.Minute),
		}, admittedAt.Add(-time.Minute))
		return err
	}); err != nil {
		t.Fatalf("seed identity %s: %v", a.identity.ID, err)
	}
}

// alternateAgentSelection is the baseline lineup on main's enrollment plus a
// second agent on second's, at the same offer and route: only the identity,
// and so the pool and the credential, differs. The offer outlives the real
// clock the card is written under.
func alternateAgentSelection(
	t *testing.T, main, second alternateAgentIdentity, implementerPrompt domain.Digest,
) engine.AgentSelection {
	t.Helper()
	model := agentbaseline.Selection{Model: alternateRetryModel, Effort: domain.EffortHigh}
	tree, err := agentbaseline.Tree(agentbaseline.TreeInput{
		ClaudeEnrollment: main.enrollment,
		Writers: map[domain.RoleName]agentbaseline.Selection{
			// The specifier asks its own model, so the implementer's agent is
			// named for the implementer (agentbaseline.WriterAgentName).
			domain.RoleSpecifier:   {Model: "claude-specifier-test", Effort: domain.EffortHigh},
			domain.RoleImplementer: model, domain.RoleRemediator: model,
		},
		TermsBasisDate: "2026-07-01", PricingRevision: "pricing-2026-07",
		OfferNotAfter: admittedAt.Add(5 * 365 * 24 * time.Hour),
		Prompts: map[domain.RoleName]agentbaseline.Prompt{
			domain.RoleSpecifier:   {Name: "specifier", Digest: capabilityRetrySpecificationPrompt},
			domain.RoleImplementer: {Name: "implementer", Digest: implementerPrompt},
			domain.RoleRemediator:  {Name: "remediator", Digest: productionDigest([]byte("remediation prompt"))},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	implementer, ok := tree.Agent(agentbaseline.WriterAgentName(domain.RoleImplementer))
	if !ok {
		t.Fatal("the baseline tree has no implementer agent")
	}
	alternate := implementer
	alternate.Name, alternate.Enrollment = alternateRetryAgent, string(second.enrollment.ID)
	tree.Agents = append(tree.Agents, alternate)
	tree.Sort()
	files, err := agenttree.Render(tree)
	if err != nil {
		t.Fatal(err)
	}
	revision, err := agenttree.Revision(files)
	if err != nil {
		t.Fatal(err)
	}
	if tree, err = agenttree.Parse(files); err != nil {
		t.Fatal(err)
	}
	return engine.AgentSelection{
		Tree: tree, LineupRevision: revision, Launch: agentbaseline.RoleLaunch,
		EffectiveEgress: []string{"api.anthropic.com:443"}, AttemptBudget: time.Hour, ExpiryMargin: 5 * time.Minute,
	}
}

// openAlternateAgentFixture is the capability-retry composition admitting
// through selection instead of one identity, with both identities, the Claude
// adapter's conformance, and a routing policy that orders both agents for the
// implementer seeded.
func openAlternateAgentFixture(
	t *testing.T, main, second alternateAgentIdentity,
) (*workflowFixture, engine.AgentSelection) {
	t.Helper()
	ctx := context.Background()
	var selection engine.AgentSelection
	f := openUnattendedFixtureWith(t, t.TempDir(), true, testIdentity, testPoolLimit,
		func(env *engine.AdmissionEnvironment) {
			selection = alternateAgentSelection(t, main, second, env.PromptPackageDigest)
			env.AuthIdentityID, env.Agents = nil, &selection
			env.EnforceableEgressProfiles = []domain.EgressProfile{domain.EgressProviderOnly}
		},
		func(st *store.Store, blobs *signet.BlobStore) []engine.Option {
			fetcher, err := specify.NewFetcher(st, blobs, nil)
			if err != nil {
				t.Fatalf("specify.NewFetcher: %v", err)
			}
			return []engine.Option{engine.WithSpecification(engine.SpecificationConfig{
				Fetcher: fetcher, Blobs: blobs, Now: func() time.Time { return admittedAt },
				PromptPackageDigest: capabilityRetrySpecificationPrompt,
			})}
		})
	main.seed(t, f.store)
	second.seed(t, f.store)
	// The card is read as the wire carries it, which names offers by tree
	// agent until #2808 (#2735), so the read service translates through the
	// tree the engine admits by.
	f.signet = signet.NewService(f.store, signet.WithBlobStore(f.blobs), signet.WithAgentTree(treeOf(selection)))
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		adapter, err := agentbaseline.ClaudeWardAdapter()
		if err != nil {
			return err
		}
		record, err := domain.NewAdapterConformance(domain.AdapterConformanceInput{
			AdapterDigest: adapter.Digest, Outcome: domain.ConformancePassed,
			ProvedCapabilities: domain.NewLaunchCapabilitySet(domain.LaunchCapReadTools, domain.LaunchCapMutationTools,
				domain.LaunchCapInstructionDelivery, domain.LaunchCapStructuredOutput, domain.LaunchCapContextSeverance,
				domain.LaunchCapRouteStoreContract),
			ProvedAt: admittedAt.Add(-time.Minute),
		})
		if err != nil {
			return err
		}
		if _, err := tx.RecordAdapterConformance(ctx, record); err != nil {
			return err
		}
		_, err = tx.AppendRoutingPolicy(ctx, domain.RoutingPolicyInput{
			Roles: []domain.RoutingPolicyRole{{
				Role:        domain.RoleImplementer,
				Enrollments: []domain.ClientEnrollmentID{main.enrollment.ID, second.enrollment.ID},
			}},
			Source: domain.RoutingPolicySourceCLISet, SetBy: "operator", LineupRevision: selection.LineupRevision,
		}, admittedAt.Add(-time.Hour))
		return err
	}); err != nil {
		t.Fatalf("seed conformance and routing policy: %v", err)
	}
	return f, selection
}

// treeOf is the agent tree signet translates the wire's agent names through.
func treeOf(selection engine.AgentSelection) func() *agenttree.Tree {
	return func() *agenttree.Tree { return &selection.Tree }
}

// TestAlternateAgentRetryAdmitsTheChosenAgent is the acceptance proof: the
// engine's own card offers the second agent under valid_generation, the
// operator's decision records the choice, and the retry attempt is admitted
// under the second agent's enrollment and identity. Before the engine
// allocates it, an attempt citing a record the decision did not make, or
// retrying another invocation, is refused where it is written.
func TestAlternateAgentRetryAdmitsTheChosenAgent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	main := newAlternateAgentIdentity("claude-main", "acct-alternate-0001")
	second := newAlternateAgentIdentity("claude-second", "acct-alternate-0002")
	// Its own cost owner shows the retry's admission records the chosen
	// agent's identity, not the failed one's.
	second.identity.CostOwner = "second-owner"
	f, selection := openAlternateAgentFixture(t, main, second)
	failure := driveImplementationFailure(t, f, capabilityRetryManifest(t), fake.StageScript{
		Outcome: fake.OutcomeFail,
		Result: exec.StageResult{
			Summary: "The provider refused the agent's credential.", Cause: domain.FailureCauseCredentialExpired,
		},
	})
	card := failure.card
	want := []domain.AlternateAgentOffer{{
		Agent: alternateRetryAgent, Pool: second.identity.EffectiveUsagePool(),
		Harness: domain.HarnessClientClaudeCode, Model: alternateRetryModel,
		Qualification: domain.AlternateAgentValidGeneration,
	}}
	if !card.Item.Offers(domain.ActionRetryWithAlternateAgent) || !reflect.DeepEqual(card.Item.AlternateAgents, want) {
		t.Fatalf("failure card requests %v offering %+v, want retry_with_alternate_agent offering %+v",
			card.Item.RequestedDecision, card.Item.AlternateAgents, want)
	}

	operator := signet.NewService(f.store, signet.WithBlobStore(f.blobs),
		signet.WithClock(func() time.Time { return admittedAt.Add(time.Minute) }),
		signet.WithAlternateAgentRetrier(selection.AcceptAlternateAgentRetryTx), signet.WithAgentTree(treeOf(selection)))
	agent := alternateRetryAgent
	if _, err := operator.Submit(ctx, signet.ClientCommand{
		CommandID: alternateRetryCommandID, DeviceID: deviceA, ExpectedEntityVersion: card.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: card.Item.ID, Action: domain.ActionRetryWithAlternateAgent,
			ItemVersion: card.Item.ItemVersion, AlternateAgent: &agent,
		},
	}); err != nil {
		t.Fatalf("submit retry_with_alternate_agent: %v", err)
	}
	var record domain.AlternateAgentRetry
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		record, err = tx.GetAlternateAgentRetryByCommand(ctx, alternateRetryCommandID)
		return err
	}); err != nil {
		t.Fatalf("read the operator's record: %v", err)
	}
	chosen, err := selection.Tree.SelectionOfAgent(alternateRetryAgent)
	if err != nil {
		t.Fatal(err)
	}
	if record.Selection == nil || *record.Selection != chosen || record.ItemID != card.Item.ID ||
		record.FailedInvocationID != failure.invocation || record.Pool != second.identity.EffectiveUsagePool() {
		t.Fatalf("record = %+v", record)
	}

	assertForgedRetriesRefused(t, f, failure, record)

	retryInvocation := productionInvocationForRun(failure.retryRunID)
	f.driver.Script(retryInvocation, fake.StageScript{
		Outcome: fake.OutcomeFail,
		Result:  exec.StageResult{Summary: "The retry ran under the chosen agent."},
	})
	pass := reconcileCapabilityRetryPass(t, f, "alternate agent retry")
	if pass.RunTransitions != 1 || pass.InvocationsStarted != 1 {
		t.Fatalf("alternate agent retry pass = %+v, want one transition and one start", pass)
	}
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		attempt, err := tx.GetProductionAttemptByRun(ctx, failure.retryRunID)
		if err != nil {
			return err
		}
		if attempt.AlternateAgentRetryID == nil || *attempt.AlternateAgentRetryID != record.ID {
			t.Errorf("retry attempt cites %v, want record %s", attempt.AlternateAgentRetryID, record.ID)
		}
		admission, err := tx.GetExecutionAdmissionRecord(ctx, retryInvocation)
		if err != nil {
			return err
		}
		if admission.AgentBinding == nil || admission.AgentBinding.EnrollmentID != second.enrollment.ID ||
			admission.AuthIdentityID == nil || *admission.AuthIdentityID != second.identity.ID {
			t.Fatalf("retry admission binds %+v under %v, want %s under %s",
				admission.AgentBinding, admission.AuthIdentityID, second.enrollment.ID, second.identity.ID)
		}
		failed, err := tx.GetExecutionAdmissionRecord(ctx, failure.invocation)
		if err != nil {
			return err
		}
		// Both agents share one offer, so one lineage group.
		if binding := admission.AgentBinding; binding.SelectionSource != domain.AgentSelectionSourceCard ||
			binding.SelectionRecordID != record.ID || binding.CostOwner != "second-owner" ||
			failed.AgentBinding == nil || binding.LineageGroup != failed.AgentBinding.LineageGroup {
			t.Errorf("retry admission selected by %s %s, cost owner %q, lineage group %q; "+
				"want card %s, second-owner, and the offer's lineage group",
				binding.SelectionSource, binding.SelectionRecordID, binding.CostOwner, binding.LineageGroup, record.ID)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// assertForgedRetriesRefused writes, in turn, the retry attempt the
// operator's command asked for, but citing a record its decision did not
// make or retrying another invocation. Each is refused as a parent-key
// mismatch, and none allocates the retry run.
func assertForgedRetriesRefused(
	t *testing.T, f *workflowFixture, failure implementationFailure, record domain.AlternateAgentRetry,
) {
	t.Helper()
	ctx := context.Background()
	forge := func(edit func(*domain.AlternateAgentRetry)) domain.AlternateAgentRetry {
		forged := record
		edit(&forged)
		forged, err := domain.NewAlternateAgentRetry(forged)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
			return tx.RecordAlternateAgentRetry(ctx, forged)
		}); err != nil {
			t.Fatalf("store the forged record: %v", err)
		}
		return forged
	}
	parkedMove := forge(func(r *domain.AlternateAgentRetry) { r.ItemID, r.CommandID = "", "command-forged-move" })
	otherCommand := forge(func(r *domain.AlternateAgentRetry) { r.CommandID = "command-forged-other" })
	for name, spec := range map[string]engine.ProductionReattemptSpec{
		"a parked move's record":        {AlternateAgentRetryID: parkedMove.ID, RetryOfInvocationID: failure.invocation},
		"another command's record":      {AlternateAgentRetryID: otherCommand.ID, RetryOfInvocationID: failure.invocation},
		"a retry of another invocation": {AlternateAgentRetryID: record.ID, RetryOfInvocationID: "inv-implement-other"},
	} {
		spec.ParentRunID = capabilityRetryRunID
		spec.Reason = "operator alternate agent retry " + alternateRetryCommandID
		spec.OperatorCommandID = alternateRetryCommandID
		if retry, err := engine.ReattemptProductionRun(ctx, f.store, spec); !errors.Is(err, domain.ErrParentKeyMismatch) {
			t.Errorf("%s: reattempt = %+v, %v; want a parent-key mismatch", name, retry, err)
		}
	}
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.GetRun(ctx, failure.retryRunID)
		return err
	}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a forged retry allocated the retry run: %v", err)
	}
}

// TestParkedTaskMovesToTheChosenAgent is the parked path's acceptance proof:
// a quota refusal with a reset parks the retry attempt with no card, the
// hold offers the second agent on its own pool, the operator's move re-pins
// the implementer there, naming the agent it left, and releases the hold,
// and the parked attempt then starts under the second agent's enrollment
// without waiting for the refused window's reset.
func TestParkedTaskMovesToTheChosenAgent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	main := newAlternateAgentIdentity("claude-main", "acct-alternate-0001")
	second := newAlternateAgentIdentity("claude-second", "acct-alternate-0002")
	f, selection := openAlternateAgentFixture(t, main, second)
	failure, taskID := driveParkedImplementer(t, f, second)

	const moveCommandID = "command-move-parked-task"
	if _, err := parkedTaskMover(f, selection).Submit(ctx, moveParkedTaskCommand(moveCommandID, taskID)); err != nil {
		t.Fatalf("submit move_parked_task: %v", err)
	}
	var moved domain.TaskLine
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		heads, err := tx.TaskLineHeads(ctx, taskID)
		if err != nil {
			return err
		}
		moved = heads[domain.RoleImplementer]
		movedAgent, err := selection.Tree.LineAgent(moved)
		if err != nil {
			return err
		}
		if movedAgent != alternateRetryAgent || moved.Source != domain.TaskLineSourceAlternateAgentMove ||
			moved.Provenance == nil || moved.Provenance.Pool != second.identity.EffectiveUsagePool() ||
			moved.Provenance.PassedOver == nil ||
			moved.Provenance.PassedOver.Enrollment != main.enrollment.ID {
			t.Fatalf("implementer line after the move = %+v (provenance %+v)", moved, moved.Provenance)
		}
		if _, found, err := tx.GetRunHold(ctx, failure.retryRunID); err != nil || found {
			t.Fatalf("retry run hold after the move: found %v, err %v; want released", found, err)
		}
		return nil
	}); err != nil {
		t.Fatalf("read the moved task: %v", err)
	}

	retryInvocation := productionInvocationForRun(failure.retryRunID)
	f.driver.Script(retryInvocation, fake.StageScript{
		Outcome: fake.OutcomeFail,
		Result:  exec.StageResult{Summary: "The parked attempt ran under the chosen agent."},
	})
	pass := reconcileCapabilityRetryPass(t, f, "moved attempt")
	if pass.InvocationsStarted != 1 {
		t.Fatalf("moved attempt pass = %+v, want one start", pass)
	}
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		admission, err := tx.GetExecutionAdmissionRecord(ctx, retryInvocation)
		if err != nil {
			return err
		}
		if binding := admission.AgentBinding; binding == nil || binding.EnrollmentID != second.enrollment.ID ||
			binding.SelectionSource != domain.AgentSelectionSourceTaskLine || binding.SelectionRecordID != moved.ID {
			t.Errorf("moved attempt admission binds %+v, want %s selected by task line %s",
				binding, second.enrollment.ID, moved.ID)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestParkedTaskMoveRefusesAStoppedTask: once Stop is recorded, a move of the
// still-parked attempt is refused as not parked, before its workflow closes,
// and records nothing: no line, and the hold stays.
func TestParkedTaskMoveRefusesAStoppedTask(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	main := newAlternateAgentIdentity("claude-main", "acct-alternate-0001")
	second := newAlternateAgentIdentity("claude-second", "acct-alternate-0002")
	f, selection := openAlternateAgentFixture(t, main, second)
	failure, taskID := driveParkedImplementer(t, f, second)
	state, err := f.store.ServerState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		task, err := tx.GetTask(ctx, taskID)
		if err != nil {
			return err
		}
		_, _, err = tx.StopTask(ctx, domain.StopTaskRequest{
			CommandID: "command-stop-parked-task", DeviceID: deviceA, TaskID: taskID, ProjectID: task.ProjectID,
			ExpectedSyncEpoch: state.SyncEpoch, ExpectedEntityVersion: state.Revision,
		}, time.Now().UTC())
		return err
	}); err != nil {
		t.Fatalf("stop the parked task: %v", err)
	}

	_, err = parkedTaskMover(f, selection).Submit(ctx, moveParkedTaskCommand("command-move-stopped-task", taskID))
	if !errors.Is(err, signet.ErrTaskNotParked) {
		t.Fatalf("move of a stopped task = %v, want ErrTaskNotParked", err)
	}
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		heads, err := tx.TaskLineHeads(ctx, taskID)
		if err != nil {
			return err
		}
		if line, found := heads[domain.RoleImplementer]; found && line.Source == domain.TaskLineSourceAlternateAgentMove {
			t.Errorf("a refused move wrote line %+v", line)
		}
		if _, found, err := tx.GetRunHold(ctx, failure.retryRunID); err != nil || !found {
			t.Errorf("retry run hold after a refused move: found %v, err %v; want kept", found, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// parkedTaskMover is the operator service with the engine's move wired, on
// the real clock the parked hold's reset is read against.
func parkedTaskMover(f *workflowFixture, selection engine.AgentSelection) *signet.Service {
	return signet.NewService(f.store, signet.WithBlobStore(f.blobs),
		signet.WithClock(func() time.Time { return time.Now().UTC() }),
		signet.WithParkedTaskMover(selection.MoveParkedTaskTx), signet.WithAgentTree(treeOf(selection)))
}

func moveParkedTaskCommand(commandID string, taskID domain.TaskID) signet.ClientCommand {
	return signet.ClientCommand{
		CommandID: commandID, DeviceID: deviceA, Kind: domain.CommandKindMoveParkedTask,
		MoveParkedTask: signet.MoveParkedTaskPayload{TaskID: taskID, Agent: alternateRetryAgent},
	}
}

// driveParkedImplementer fails the implementation attempt on a quota refusal
// with a known reset and checks the engine parked its retry with no card,
// under a hold offering the second agent on its own pool.
func driveParkedImplementer(
	t *testing.T, f *workflowFixture, second alternateAgentIdentity,
) (implementationFailure, domain.TaskID) {
	t.Helper()
	ctx := context.Background()
	// The park reads the reset against the engine's real clock.
	resetsAt := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	failure := driveImplementationTerminal(t, f, capabilityRetryManifest(t), fake.StageScript{
		Outcome: fake.OutcomeFail,
		Result: exec.StageResult{
			Summary: "The provider refused the five-hour window.", Cause: domain.FailureCauseQuota,
			QuotaRefusal: &domain.QuotaRefusal{Window: "five_hour", ResetsAt: &resetsAt},
		},
	})
	if _, err := f.signet.GetAttentionItem(ctx,
		domain.ItemID("execution-failure-"+string(failure.invocation))); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("parked refusal raised a failure card (err %v), want none", err)
	}

	var taskID domain.TaskID
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		run, err := tx.GetRun(ctx, failure.retryRunID)
		if err != nil {
			return err
		}
		taskID = run.TaskID
		hold, found, err := tx.GetRunHold(ctx, failure.retryRunID)
		if err != nil {
			return err
		}
		// The hold stores the enrollment; the wire names it by agent.
		want := []domain.AlternateAgentOffer{{
			Enrollment: second.enrollment.ID, Pool: second.identity.EffectiveUsagePool(),
			Harness: domain.HarnessClientClaudeCode, Model: alternateRetryModel,
			Qualification: domain.AlternateAgentDifferentPool,
		}}
		if !found || hold.Reason != domain.HoldQuotaWindowExhausted || hold.QuotaWindow == nil ||
			!reflect.DeepEqual(hold.QuotaWindow.AlternateAgents, want) {
			t.Fatalf("retry run hold = %+v (found %v), want quota_window_exhausted offering %+v", hold, found, want)
		}
		return nil
	}); err != nil {
		t.Fatalf("read the parked run: %v", err)
	}
	// Both timelines name the hold's offer by its tree agent, the shape the
	// API's AlternateAgentOffer still requires.
	wire := []domain.AlternateAgentOffer{{
		Agent: alternateRetryAgent, Pool: second.identity.EffectiveUsagePool(),
		Harness: domain.HarnessClientClaudeCode, Model: alternateRetryModel,
		Qualification: domain.AlternateAgentDifferentPool,
	}}
	runTimeline, err := f.signet.GetRunTimeline(ctx, failure.retryRunID)
	if err != nil {
		t.Fatal(err)
	}
	if runTimeline.Hold == nil || runTimeline.Hold.QuotaWindow == nil ||
		!reflect.DeepEqual(runTimeline.Hold.QuotaWindow.AlternateAgents, wire) {
		t.Fatalf("run timeline hold = %+v, want offers %+v", runTimeline.Hold, wire)
	}
	taskTimeline, err := f.signet.GetTaskTimeline(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	var held int
	for _, section := range taskTimeline.Sections {
		for _, run := range section.Runs {
			if run.Hold == nil || run.Hold.QuotaWindow == nil {
				continue
			}
			held++
			if !reflect.DeepEqual(run.Hold.QuotaWindow.AlternateAgents, wire) {
				t.Fatalf("task timeline run %s hold offers = %+v, want %+v",
					run.RunID, run.Hold.QuotaWindow.AlternateAgents, wire)
			}
		}
	}
	if held != 1 {
		t.Fatalf("task timeline shows %d quota holds, want 1", held)
	}
	return failure, taskID
}
