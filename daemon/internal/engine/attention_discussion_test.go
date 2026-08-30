package engine

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/advisory"
	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	execfake "github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	inferencefake "github.com/freeside-ai/freeside/daemon/internal/inference/fake"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

type attentionDiscussionFixture struct {
	dbPath        string
	store         *store.Store
	signet        *signet.Service
	advisory      *advisory.Store
	inference     *inference.Client
	inferenceFake *inferencefake.Driver
	item          domain.AttentionItem
	now           time.Time
}

func newAttentionDiscussionFixture(
	t *testing.T, itemType domain.AttentionType, inferenceDriver inference.Driver,
) *attentionDiscussionFixture {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	st, err := store.Open(t.Context(), dbPath, store.Options{
		AdmissionFloors: map[domain.OperatingMode]domain.CapabilitySnapshot{
			domain.ModeAttendedDev: domain.NewCapabilitySnapshot(domain.CapPostExitExport),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	t.Cleanup(func() {
		if !closed {
			_ = st.Close()
		}
	})
	now := time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)
	service := signet.NewService(st, signet.WithClock(func() time.Time { return now }))
	runID := domain.RunID("run-discussion")
	attempt := domain.Attempt{
		ID: "attempt-failed", StageID: "stage-implementation", Number: 1,
		InvocationID: "inv-failed",
	}
	run := domain.Run{
		ID: runID, ProjectID: "project-discussion",
		SpecDigest:   domain.Digest(contentaddr.Sum([]byte("specification"))),
		PolicyDigest: domain.Digest(contentaddr.Sum([]byte("policy"))),
		Stages: []domain.Stage{{
			ID: attempt.StageID, RunID: runID, Name: "implementation",
			Attempts: []domain.Attempt{attempt},
		}},
	}
	admission, err := domain.NewExecutionAdmission(domain.ExecutionAdmissionInput{
		InvocationID: attempt.InvocationID, RunID: run.ID,
		StageID: attempt.StageID, AttemptID: attempt.ID,
		Backend:       "fresh_vm_read_only_volume_handoff",
		Capabilities:  domain.NewCapabilitySnapshot(domain.CapPostExitExport),
		OperatingMode: domain.ModeAttendedDev, CredentialMode: domain.CredentialLocalTrusted,
		EgressProfile: domain.EgressCleanVerification,
		ImageRef:      domain.ImageRef("ghcr.io/freeside-ai/agent@sha256:" + strings.Repeat("ab", 32)),
		SpecDigest:    run.SpecDigest, PolicyDigest: run.PolicyDigest,
		InputDigest: domain.Digest(contentaddr.Sum([]byte("discussion input"))),
		Base: domain.BaseRevision{
			Repo: "owner/repo", RepositoryID: 1, BaseRef: "refs/heads/main", BaseSHA: "deadbeef",
		},
		Workspace: "workspace-discussion", AdmittedAt: now.Add(-time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(t.Context(), func(tx *store.WriteTx) error {
		if err := tx.PutDevice(t.Context(), domain.Device{
			ID: "device-1", DisplayName: "Operator", Status: domain.DeviceActive, PairedAt: now,
		}); err != nil {
			return err
		}
		if err := tx.PutRun(t.Context(), run); err != nil {
			return err
		}
		if err := tx.RecordExecutionAdmission(t.Context(), admission); err != nil {
			return err
		}
		return tx.RecordExecutionOutcome(t.Context(), domain.ExecutionOutcome{
			InvocationID: attempt.InvocationID, AdmissionID: admission.ID,
			Status: domain.ExecutionOutcomeFailed, Summary: "discussion fixture failure",
			RecordedAt: now,
		})
	}); err != nil {
		t.Fatal(err)
	}

	head := strings.Repeat("a", 40)
	input := domain.AttentionItemInput{
		ID: domain.ItemID("item-" + string(itemType)), ProjectID: "project-discussion",
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
		Type:    itemType, Priority: domain.PriorityHigh, Reason: "The item needs an operator decision.",
		ItemVersion: 1, InterruptionClass: domain.InterruptionPlannedGate,
		CreatedAt: &now, Status: domain.StatusOpen,
	}
	switch itemType {
	case domain.AttentionExecutionFailure:
		input.RequestedDecision = []domain.Action{domain.ActionDiscuss, domain.ActionStop}
		input.ExecutionFailure = &domain.ExecutionFailureFacts{
			Outcome: domain.ExecutionOutcomeFailed, Stage: domain.StageNameImplementation,
			InvocationID: "inv-failed",
		}
	case domain.AttentionReviewConfiguration:
		input.RequestedDecision = []domain.Action{
			domain.ActionAdoptReviewConfiguration, domain.ActionDiscuss, domain.ActionStop,
		}
		input.PRHeadSHA = head
		input.ReviewConfigurationRecovery = &domain.ReviewConfigurationRecoveryBinding{
			RunID: runID, InvocationID: "review-1", Round: 1,
			BaseSHA: strings.Repeat("b", 40), HeadSHA: head,
			FailureDigest: domain.Digest(contentaddr.Sum([]byte("failure"))),
			Repo:          "owner/repo", RepositoryID: 1,
			SupersededProfileDigest: domain.Digest(contentaddr.Sum([]byte("profile"))),
		}
	case domain.AttentionReviewDispute:
		input.RequestedDecision = []domain.Action{domain.ActionDiscuss, domain.ActionStop}
		input.PRHeadSHA = head
		claimText := domain.ClaimText{
			MediaType: domain.MediaTypeTextPlain,
			Content:   "The remediator disputes the finding because the cited path is unreachable.",
		}
		input.AgentClaims = []domain.AgentClaim{{
			Label: "remediator_position", Artifact: "remediator-position",
			Digest: claimText.ComputeDigest(), Text: &claimText,
			Provenance: domain.Provenance{
				ProducerClass: domain.ProducerAgent, ProducerInvocationID: "remediation-1",
				HeadBinding: domain.HeadBound, SourceHeadSHA: head,
				SensitivityClass: domain.SensitivityNormal,
			},
		}}
	case domain.AttentionSpecApproval, domain.AttentionAgentQuestion,
		domain.AttentionReviewDiminishing, domain.AttentionReviewContradiction,
		domain.AttentionFindingAdjudication, domain.AttentionReadyForFinalReview,
		domain.AttentionPublishBlocked, domain.AttentionRunProposal,
		domain.AttentionSystemHealth, domain.AttentionBlocked:
		t.Fatalf("unsupported fixture type %q", itemType)
	}
	item, err := domain.NewAttentionItem(input, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.PutItem(t.Context(), item); err != nil {
		t.Fatal(err)
	}

	advisoryStore, err := advisory.Open(
		filepath.Join(dir, "advisory.json"), 20, 16<<10,
		advisory.WithClock(func() time.Time { return now }),
	)
	if err != nil {
		t.Fatal(err)
	}
	limits := inference.Limits{
		Calls: 20, ComputeUnits: 100_000, AttentionItems: 20, Starvation: time.Hour,
	}
	budget := inference.Budget{
		Window: time.Hour, Site: limits, Project: limits, Global: limits,
		MaxCallsPerRoot: 20, MaxStarvationPerRoot: time.Hour,
	}
	client, err := inference.New(inference.Config{
		StatePath: filepath.Join(dir, "inference.json"),
		Binding: inference.Binding{
			Provider: "fake", Model: "test", Driver: inferenceDriver,
		},
		Sites: []inference.Site{inference.DiscussionSite(budget)}, Advisory: advisoryStore,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture := &attentionDiscussionFixture{
		dbPath: dbPath, store: st, signet: service, advisory: advisoryStore,
		inference: client, item: item, now: now,
	}
	if fake, ok := inferenceDriver.(*inferencefake.Driver); ok {
		fixture.inferenceFake = fake
	}
	fixtureClose := func() {
		if !closed {
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			closed = true
		}
	}
	t.Cleanup(fixtureClose)
	return fixture
}

func (f *attentionDiscussionFixture) submit(t *testing.T) {
	t.Helper()
	var item domain.AttentionItem
	var snapshot store.Snapshot
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		item, snapshot, err = tx.GetAttentionItemSnapshot(t.Context(), f.item.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.signet.Submit(t.Context(), signet.ClientCommand{
		CommandID: "discuss-1", DeviceID: "device-1", ExpectedEntityVersion: snapshot.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: item.ID, ItemVersion: item.ItemVersion, PRHeadSHA: item.PRHeadSHA,
			ArtifactDigests: item.ArtifactDigests, Action: domain.ActionDiscuss,
			Message: "Explain what I need to decide.",
		},
	}); err != nil {
		t.Fatal(err)
	}
}

func (f *attentionDiscussionFixture) blockUnattended(t *testing.T) {
	t.Helper()
	posture := domain.HealthPostureBlocking
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "health-block-discussion", ProjectID: f.item.ProjectID,
		Subject: domain.Subject{Type: domain.SubjectSystem, ID: "daemon"},
		Type:    domain.AttentionSystemHealth, Priority: domain.PriorityNormal,
		Reason:            "unattended operation is blocked for the test",
		RequestedDecision: []domain.Action{domain.ActionAcknowledge, domain.ActionStopUnattended},
		ItemVersion:       1, InterruptionClass: domain.InterruptionExceptional,
		Posture: &posture, Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.signet.PutItem(t.Context(), item); err != nil {
		t.Fatal(err)
	}
}

func (f *attentionDiscussionFixture) engine(t *testing.T, withInference bool) *Engine {
	t.Helper()
	options := []Option{}
	if withInference {
		options = append(options, WithInference(f.inference))
	}
	engine, err := New(f.store, f.signet, execfake.NewStageDriver(), options...)
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

func (f *attentionDiscussionFixture) assertReply(
	t *testing.T, wantReply string, wantActions []domain.Action,
) {
	t.Helper()
	var item domain.AttentionItem
	var conversation domain.Conversation
	var intent store.QueueEntry
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		item, err = tx.GetAttentionItem(t.Context(), f.item.ID)
		if err != nil {
			return err
		}
		conversation, err = tx.GetConversation(t.Context(), *item.ConversationID)
		if err != nil {
			return err
		}
		intent, err = tx.GetOutbox(t.Context(), "inv-discuss-1")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if item.ItemVersion != 3 || !slices.Equal(item.RequestedDecision, wantActions) {
		t.Fatalf("item version/actions = %d/%v", item.ItemVersion, item.RequestedDecision)
	}
	if conversation.Status != domain.ConversationIdle || len(conversation.Messages) != 2 ||
		conversation.Messages[1].Body != wantReply ||
		conversation.Messages[1].ID != "msg-agent-inv-discuss-1" {
		t.Fatalf("conversation = %#v", conversation)
	}
	if !intent.Dispatched() {
		t.Fatalf("discussion intent status = %q", intent.Status)
	}
}

func TestAttentionDiscussionRepliesPreserveEveryDecisionSetAndReplay(t *testing.T) {
	types := []struct {
		itemType domain.AttentionType
		actions  []domain.Action
	}{
		{domain.AttentionExecutionFailure, []domain.Action{domain.ActionDiscuss, domain.ActionStop}},
		{domain.AttentionReviewConfiguration, []domain.Action{
			domain.ActionAdoptReviewConfiguration, domain.ActionDiscuss, domain.ActionStop,
		}},
		{domain.AttentionReviewDispute, []domain.Action{domain.ActionDiscuss, domain.ActionStop}},
	}
	for _, tc := range types {
		t.Run(string(tc.itemType), func(t *testing.T) {
			driver := inferencefake.New()
			driver.Script(inference.AttentionDiscussionSiteID, inferencefake.Script{Response: inference.Response{
				Output: []byte(`{"reply":"The decision set is unchanged."}`), ComputeUnits: 1,
			}})
			f := newAttentionDiscussionFixture(t, tc.itemType, driver)
			f.submit(t)
			engine := f.engine(t, true)
			if result, err := engine.Reconcile(t.Context()); err != nil || result.ResultsAccepted != 1 {
				t.Fatalf("Reconcile = %#v, %v", result, err)
			}
			f.assertReply(t, "The decision set is unchanged.", tc.actions)
			if result, err := engine.Reconcile(t.Context()); err != nil || result.ResultsAccepted != 0 {
				t.Fatalf("replay Reconcile = %#v, %v", result, err)
			}
			f.assertReply(t, "The decision set is unchanged.", tc.actions)
			entries, err := f.advisory.List(t.Context())
			discussionEntries := slices.DeleteFunc(entries, func(entry advisory.Entry) bool {
				return entry.Kind != "discussion_reply"
			})
			if err != nil || len(discussionEntries) != 1 || discussionEntries[0].Producer != "fake/test" {
				t.Fatalf("advisory entries = %#v, %v", entries, err)
			}
		})
	}
}

func TestAttentionDiscussionFallbackReturnsConversationToIdle(t *testing.T) {
	f := newAttentionDiscussionFixture(t, domain.AttentionExecutionFailure, nil)
	f.submit(t)
	if result, err := f.engine(t, true).Reconcile(t.Context()); err != nil || result.ResultsAccepted != 1 {
		t.Fatalf("Reconcile = %#v, %v", result, err)
	}
	f.assertReply(t, unavailableAttentionDiscussionReply, []domain.Action{domain.ActionDiscuss, domain.ActionStop})
}

func TestAttentionDiscussionSecretInputUsesFallbackWithoutProvider(t *testing.T) {
	driver := inferencefake.New()
	f := newAttentionDiscussionFixture(t, domain.AttentionExecutionFailure, driver)
	var snapshot store.Snapshot
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		_, got, err := tx.GetAttentionItemSnapshot(t.Context(), f.item.ID)
		snapshot = got
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.signet.Submit(t.Context(), signet.ClientCommand{
		CommandID: "discuss-1", DeviceID: "device-1", ExpectedEntityVersion: snapshot.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: f.item.ID, ItemVersion: f.item.ItemVersion, Action: domain.ActionDiscuss,
			Message: "Explain ghp_" + strings.Repeat("A", 36),
		},
	}); err != nil {
		t.Fatal(err)
	}
	if result, err := f.engine(t, true).Reconcile(t.Context()); err != nil || result.ResultsAccepted != 1 {
		t.Fatalf("Reconcile = %#v, %v", result, err)
	}
	f.assertReply(t, unavailableAttentionDiscussionReply, []domain.Action{domain.ActionDiscuss, domain.ActionStop})
	if requests := driver.Requests(); len(requests) != 0 {
		t.Fatalf("secret-bearing discussion reached provider: %#v", requests)
	}
}

func TestAttentionDiscussionHoldBlocksNewReplyButRetiresCompletion(t *testing.T) {
	t.Run("pending reply", func(t *testing.T) {
		driver := inferencefake.New()
		driver.Script(inference.AttentionDiscussionSiteID, inferencefake.Script{Response: inference.Response{
			Output: []byte(`{"reply":"must not run"}`), ComputeUnits: 1,
		}})
		f := newAttentionDiscussionFixture(t, domain.AttentionExecutionFailure, driver)
		f.submit(t)
		f.blockUnattended(t)
		if result, err := f.engine(t, true).Reconcile(t.Context()); err != nil || result.ResultsAccepted != 0 {
			t.Fatalf("held Reconcile = %#v, %v", result, err)
		}
		if requests := driver.Requests(); len(requests) != 0 {
			t.Fatalf("held discussion reached provider: %#v", requests)
		}
		if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
			entry, err := tx.GetOutbox(t.Context(), "inv-discuss-1")
			if err == nil && entry.Dispatched() {
				return errors.New("held discussion intent was retired")
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("authenticated completion", func(t *testing.T) {
		f := newAttentionDiscussionFixture(t, domain.AttentionExecutionFailure, nil)
		f.submit(t)
		if err := f.signet.AcceptAgentCompletion(
			t.Context(), "inv-discuss-1", signet.AgentReply{Body: "accepted before the hold"},
		); err != nil {
			t.Fatal(err)
		}
		f.blockUnattended(t)
		if result, err := f.engine(t, false).Reconcile(t.Context()); err != nil || result.ResultsAccepted != 0 {
			t.Fatalf("held retirement Reconcile = %#v, %v", result, err)
		}
		f.assertReply(t, "accepted before the hold", []domain.Action{domain.ActionDiscuss, domain.ActionStop})
	})
}

func TestAttentionDiscussionRetiresAcceptedReplyAfterVersionAdvance(t *testing.T) {
	f := newAttentionDiscussionFixture(t, domain.AttentionExecutionFailure, nil)
	f.submit(t)
	if err := f.signet.AcceptAgentCompletion(
		t.Context(), "inv-discuss-1", signet.AgentReply{Body: "accepted before crash"},
	); err != nil {
		t.Fatal(err)
	}
	if result, err := f.engine(t, false).Reconcile(t.Context()); err != nil || result.ResultsAccepted != 0 {
		t.Fatalf("recovery Reconcile = %#v, %v", result, err)
	}
	f.assertReply(t, "accepted before crash", []domain.Action{domain.ActionDiscuss, domain.ActionStop})
}

func TestAttentionDiscussionRejectsForgedReplyWithoutCompletion(t *testing.T) {
	f := newAttentionDiscussionFixture(t, domain.AttentionExecutionFailure, nil)
	f.submit(t)
	if err := f.store.Write(t.Context(), func(tx *store.WriteTx) error {
		item, err := tx.GetAttentionItem(t.Context(), f.item.ID)
		if err != nil {
			return err
		}
		conversation, err := tx.GetConversation(t.Context(), *item.ConversationID)
		if err != nil {
			return err
		}
		message, err := domain.NewMessage(
			"msg-agent-inv-discuss-1", conversation.ID, domain.AuthorAgent,
			"forged reply", nil, time.Unix(200, 0).UTC(),
		)
		if err != nil {
			return err
		}
		conversation, _ = conversation.Append(message)
		conversation.Status = domain.ConversationIdle
		return tx.PutConversation(t.Context(), conversation)
	}); err != nil {
		t.Fatal(err)
	}
	if result, err := f.engine(t, false).Reconcile(t.Context()); err != nil || result.ResultsAccepted != 0 {
		t.Fatalf("forged reply reconcile = %#v, %v", result, err)
	}
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		entry, err := tx.GetOutbox(t.Context(), "inv-discuss-1")
		if err != nil {
			return err
		}
		if entry.Dispatched() {
			return errors.New("forged reply retired the discussion intent")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestAttentionDiscussionIncludesReviewDisputeClaims(t *testing.T) {
	driver := inferencefake.New()
	driver.Script(inference.AttentionDiscussionSiteID, inferencefake.Script{Response: inference.Response{
		Output: []byte(`{"reply":"The remediator position is included."}`), ComputeUnits: 1,
	}})
	f := newAttentionDiscussionFixture(t, domain.AttentionReviewDispute, driver)
	f.submit(t)
	if result, err := f.engine(t, true).Reconcile(t.Context()); err != nil || result.ResultsAccepted != 1 {
		t.Fatalf("Reconcile = %#v, %v", result, err)
	}
	requests := driver.Requests()
	if len(requests) != 1 {
		t.Fatalf("discussion requests = %d, want 1", len(requests))
	}
	var facts attentionDiscussionCardFacts
	if err := json.Unmarshal([]byte(requests[0].Fields["card_facts"]), &facts); err != nil {
		t.Fatal(err)
	}
	if len(facts.ReviewDisputeClaims) != 1 || facts.ReviewDisputeClaims[0].Text == nil ||
		facts.ReviewDisputeClaims[0].Text.Content != f.item.AgentClaims[0].Text.Content {
		t.Fatalf("review dispute facts = %+v", facts)
	}
}

func TestAttentionDiscussionReplyLandsAfterTerminalDecision(t *testing.T) {
	driver := inferencefake.New()
	driver.Script(inference.AttentionDiscussionSiteID, inferencefake.Script{Response: inference.Response{
		Output: []byte(`{"reply":"This late answer remains durable."}`), ComputeUnits: 1,
	}})
	f := newAttentionDiscussionFixture(t, domain.AttentionExecutionFailure, driver)
	f.submit(t)
	var item domain.AttentionItem
	var snapshot store.Snapshot
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		item, snapshot, err = tx.GetAttentionItemSnapshot(t.Context(), f.item.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.signet.Submit(t.Context(), signet.ClientCommand{
		CommandID: "stop-before-reply", DeviceID: "device-1",
		ExpectedEntityVersion: snapshot.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: item.ID, ItemVersion: item.ItemVersion, Action: domain.ActionStop,
			ArtifactDigests: item.ArtifactDigests,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if result, err := f.engine(t, true).Reconcile(t.Context()); err != nil || result.ResultsAccepted != 1 {
		t.Fatalf("Reconcile = %#v, %v", result, err)
	}
	f.assertReply(t, "This late answer remains durable.", []domain.Action{domain.ActionDiscuss, domain.ActionStop})
	if item, err := f.signet.GetAttentionItem(t.Context(), f.item.ID); err != nil || item.Item.Status != domain.StatusResolved {
		t.Fatalf("terminal item = %+v, error = %v", item, err)
	}
}

func TestAttentionDiscussionWithoutConfiguredSitePauses(t *testing.T) {
	driver := inferencefake.New()
	f := newAttentionDiscussionFixture(t, domain.AttentionExecutionFailure, driver)
	f.submit(t)
	if result, err := f.engine(t, false).Reconcile(t.Context()); err != nil || result.ResultsAccepted != 0 {
		t.Fatalf("Reconcile = %#v, %v", result, err)
	}
	var item domain.AttentionItem
	var conversation domain.Conversation
	var intent store.QueueEntry
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		item, err = tx.GetAttentionItem(t.Context(), f.item.ID)
		if err != nil {
			return err
		}
		conversation, err = tx.GetConversation(t.Context(), *item.ConversationID)
		if err != nil {
			return err
		}
		intent, err = tx.GetOutbox(t.Context(), "inv-discuss-1")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if item.ItemVersion != 2 || conversation.Status != domain.ConversationAwaitingAgent ||
		len(conversation.Messages) != 1 || intent.Dispatched() {
		t.Fatalf("paused item/conversation/intent = %#v / %#v / %#v", item, conversation, intent)
	}
}

func TestAttentionDiscussionRecoversAfterStoreRestart(t *testing.T) {
	driver := inferencefake.New()
	driver.Script(inference.AttentionDiscussionSiteID, inferencefake.Script{Response: inference.Response{
		Output: []byte(`{"reply":"Recovered after restart."}`), ComputeUnits: 1,
	}})
	f := newAttentionDiscussionFixture(t, domain.AttentionReviewConfiguration, driver)
	f.submit(t)
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(t.Context(), f.dbPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	f.store = reopened
	f.signet = signet.NewService(reopened, signet.WithClock(func() time.Time { return f.now }))
	engine := f.engine(t, true)
	if result, err := engine.Reconcile(t.Context()); err != nil || result.ResultsAccepted != 1 {
		t.Fatalf("restart Reconcile = %#v, %v", result, err)
	}
	f.assertReply(t, "Recovered after restart.", []domain.Action{
		domain.ActionAdoptReviewConfiguration, domain.ActionDiscuss, domain.ActionStop,
	})
	if result, err := engine.Reconcile(t.Context()); err != nil || result.ResultsAccepted != 0 {
		t.Fatalf("restart replay Reconcile = %#v, %v", result, err)
	}
}
