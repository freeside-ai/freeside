package signet_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

// TestUnavailableOperatorHooksAnswer400 drives the three commands a daemon
// with no committed agent tree cannot apply, through POST /commands, with
// each hook answering as the daemon's no-tree wiring does
// (daemoncompose's late.Agents() == nil branch). Each answers 400 with a
// message alone, records no receipt or decision, leaves the card open, and
// moves no revision: the refusal is definite, so the client must not hold
// it as an ambiguous 500.
func TestUnavailableOperatorHooksAnswer400(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	wantRefused := func(t *testing.T, handler http.Handler, body string) {
		t.Helper()
		response := authenticatedRequest(t, handler, http.MethodPost, "/commands", bytes.NewReader([]byte(body)))
		var decoded map[string]any
		if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
			t.Fatalf("decode %q: %v", response.Body.String(), err)
		}
		if message, _ := decoded["message"].(string); response.Code != http.StatusBadRequest || message == "" || len(decoded) != 1 {
			t.Fatalf("POST /commands = %d %s, want 400 with a message alone", response.Code, response.Body.String())
		}
	}

	t.Run("operator commands", func(t *testing.T) {
		t.Parallel()
		// Counted, so the 400 is shown to come from the hook and not from
		// a refusal of the request ahead of it.
		reached := 0
		svc, st, task := newOperatorService(t,
			signet.WithRoutingPolicySetter(func(
				context.Context, *store.WriteTx, signet.RoutingPolicyChange, time.Time,
			) (signet.RoutingPolicyOutcome, error) {
				reached++
				return signet.RoutingPolicyOutcome{}, signet.ErrOperatorSettingUnavailable
			}),
			signet.WithParkedTaskMover(func(
				context.Context, *store.WriteTx, domain.ParkedTaskMoveRequest, time.Time,
			) (domain.ParkedTaskMoveReceipt, error) {
				reached++
				return domain.ParkedTaskMoveReceipt{}, signet.ErrOperatorSettingUnavailable
			}),
		)
		handler := signet.NewHTTPHandler(svc, testAuthorizer)
		before := serverRevision(t, st)
		commands := map[string]string{
			"policy-no-tree": `{"kind":"set_routing_policy","roles":[{"role":"implementer","enrollments":["` +
				string(codexPrimary) + `"]}],"expected_revision":null}`,
			"move-no-tree": `{"kind":"move_parked_task","task_id":"` + string(task) +
				`","choice":{"enrollment_id":"` + string(claudePrimary) + `"}}`,
		}
		for id, payload := range commands {
			wantRefused(t, handler, `{"command_id":"`+id+`","device_id":"device-1","payload":`+payload+`}`)
		}
		if reached != len(commands) {
			t.Fatalf("%d hooks reached, want %d", reached, len(commands))
		}
		if serverRevision(t, st) != before {
			t.Fatal("a refused operator command moved the server revision")
		}
		if err := st.Read(ctx, func(tx *store.ReadTx) error {
			for id := range commands {
				if _, _, err := tx.GetCommandReceipt(ctx, id); !errors.Is(err, store.ErrNotFound) {
					t.Errorf("receipt for %s: %v, want none", id, err)
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("retry_with_alternate_agent decision", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		const enrollment domain.ClientEnrollmentID = "anthropic-a/claude_code"
		item, err := domain.NewAttentionItem(domain.AttentionItemInput{
			ID: "item-alternate-no-tree", ProjectID: "proj-1", Subject: f.item.Subject,
			Type: domain.AttentionExecutionFailure, Priority: domain.PriorityHigh,
			Reason:            "implementation hit its quota",
			RequestedDecision: []domain.Action{domain.ActionRetryWithAlternateAgent, domain.ActionStop},
			ExecutionFailure: &domain.ExecutionFailureFacts{
				Outcome: domain.ExecutionOutcomeFailed, Cause: domain.FailureCauseQuota,
				Stage: domain.StageNameImplementation, InvocationID: "inv-implement-run-1",
			},
			AlternateAgents: []domain.AlternateAgentOffer{{
				Enrollment: enrollment, Pool: "identity:anthropic-a",
				Harness: domain.HarnessClientClaudeCode, Model: "claude-opus-5-5",
				Qualification: domain.AlternateAgentDifferentPool,
			}},
			ItemVersion: 1, InterruptionClass: domain.InterruptionExceptional, Status: domain.StatusOpen,
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		seedImplementationFailure(t, f, item.ExecutionFailure)
		if err := f.service.PutItem(ctx, item); err != nil {
			t.Fatal(err)
		}
		f.item = item
		_, placed := f.itemSnapshot(t)
		reached := false
		svc := signet.NewService(f.store, signet.WithClock(func() time.Time { return *f.now }),
			signet.WithAlternateAgentRetrier(func(
				context.Context, *store.WriteTx, domain.Command, domain.AttentionItem, time.Time,
			) (domain.AlternateAgentRetry, error) {
				reached = true
				return domain.AlternateAgentRetry{}, signet.ErrOperatorSettingUnavailable
			}))
		before := serverRevision(t, f.store)
		wantRefused(t, signet.NewHTTPHandler(svc, testAuthorizer), `{
			"command_id":"alternate-no-tree", "device_id":"device-1",
			"expected_entity_version":`+strconv.FormatInt(placed.EntityVersion, 10)+`, "expected_bindings":{},
			"payload":{"item_id":"item-alternate-no-tree", "action":"retry_with_alternate_agent",
				"item_version":1, "pr_head_sha":"", "artifact_digests":[],
				"alternate_agent_choice":{"enrollment_id":"`+string(enrollment)+`"}}}`)
		if !reached {
			t.Fatal("the decision was refused before its retrier")
		}
		if serverRevision(t, f.store) != before {
			t.Fatal("a refused decision moved the server revision")
		}
		if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
			_, _, err := tx.GetCommandSnapshot(ctx, "alternate-no-tree")
			return err
		}); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("decision record: %v, want none", err)
		}
		if after, snap := f.itemSnapshot(t); after.Status != domain.StatusOpen || after.DecidedAt != nil ||
			snap.EntityVersion != placed.EntityVersion {
			t.Fatalf("card after the refusal: %s decided %v at version %d, want open and undecided at %d",
				after.Status, after.DecidedAt, snap.EntityVersion, placed.EntityVersion)
		}
	})
}

// seedImplementationFailure records the run stage, admission, and failed
// outcome an execution failure card's facts cite, so the store accepts the
// card (putTestItem does the same for an agent question).
func seedImplementationFailure(t *testing.T, f fixture, facts *domain.ExecutionFailureFacts) {
	t.Helper()
	ctx := t.Context()
	runID := *f.item.Subject.RunID
	stageID := domain.StageID("stage-" + string(facts.InvocationID))
	attemptID := domain.AttemptID("attempt-" + string(facts.InvocationID))
	invocation, err := domain.NewAgentInvocation(
		facts.InvocationID, []domain.ArtifactID{domain.ArtifactID("input-" + facts.InvocationID)}, nil, 0,
	)
	if err != nil {
		t.Fatal(err)
	}
	inputDigest, err := invocation.ComputeInputDigest()
	if err != nil {
		t.Fatal(err)
	}
	var run domain.Run
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		run, err = tx.GetRun(ctx, runID)
		return err
	}); errors.Is(err, store.ErrNotFound) {
		run = domain.Run{ID: runID, ProjectID: f.item.ProjectID, SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy"}
	} else if err != nil {
		t.Fatal(err)
	}
	run.Stages = append(run.Stages, domain.Stage{
		ID: stageID, RunID: runID, Name: string(facts.Stage),
		Attempts: []domain.Attempt{{ID: attemptID, StageID: stageID, Number: 1, InvocationID: facts.InvocationID}},
	})
	identityID := domain.AuthIdentityID("failure-auth")
	admission, err := domain.NewExecutionAdmission(domain.ExecutionAdmissionInput{
		InvocationID: facts.InvocationID, RunID: runID, StageID: stageID, AttemptID: attemptID,
		Backend:       "fresh_vm_read_only_volume_handoff",
		Capabilities:  domain.NewCapabilitySnapshot(domain.CapPostExitExport),
		OperatingMode: domain.ModeAttendedDev, CredentialMode: domain.CredentialSubscriptionContained,
		EgressProfile: domain.EgressProviderOnly,
		ImageRef:      domain.ImageRef("agent@sha256:" + strings.Repeat("a", 64)),
		SpecDigest:    run.SpecDigest, PolicyDigest: run.PolicyDigest, InputDigest: inputDigest,
		Base:      domain.BaseRevision{Repo: "owner/repo", RepositoryID: 1, BaseRef: "refs/heads/main", BaseSHA: "deadbeef"},
		Workspace: "workspace-1", AuthIdentityID: &identityID, AdmittedAt: *f.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		if err := tx.PutAgentInvocation(ctx, invocation); err != nil {
			return err
		}
		if err := storetest.RecordIdentityOnPool(ctx, tx, domain.AuthIdentity{
			ID: identityID, Provider: domain.AuthProviderOpenAI, AuthStoreMutationLease: true,
			Interim: domain.InterimClientFacts{AuthStoreVolume: "provider-credentials", RefreshStrategy: domain.RefreshOnDemand},
		}, 8, *f.now); err != nil {
			return err
		}
		if err := tx.RecordExecutionAdmission(ctx, admission); err != nil {
			return err
		}
		return tx.RecordExecutionOutcome(ctx, domain.ExecutionOutcome{
			InvocationID: facts.InvocationID, AdmissionID: admission.ID,
			Status: facts.Outcome, Cause: facts.Cause, Summary: "quota exhausted", RecordedAt: f.now.Add(time.Second),
		})
	}); err != nil {
		t.Fatal(err)
	}
}
