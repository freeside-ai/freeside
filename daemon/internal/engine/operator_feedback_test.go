package engine

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func TestOperatorFeedbackRequestRejectsUntrustedIdentityChanges(t *testing.T) {
	request := operatorFeedbackRequest{
		Version:      operatorFeedbackRequestVersion,
		InvocationID: operatorFeedbackInvocationID("command-1"),
		RunID:        "run-1",
		StageID:      operatorFeedbackStageID(operatorFeedbackInvocationID("command-1")),
		CommandID:    "command-1", ItemID: "item-1",
		SourceInvocationID:  "inv-implement-run-1",
		InputArtifactID:     operatorFeedbackArtifactID("command-1"),
		InputArtifactDigest: "sha256:1111111111111111111111111111111111111111111111111111111111111111",
	}
	payload, err := encodeOperatorFeedbackRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	valid := store.QueueEntry{
		IdempotencyKey: string(request.InvocationID),
		Kind:           KindOperatorFeedbackInvocationRequested,
		Payload:        payload,
	}
	if _, err := decodeOperatorFeedbackRequest(valid); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}

	for _, test := range []struct {
		name   string
		mutate func(store.QueueEntry) store.QueueEntry
	}{
		{"foreign kind", func(entry store.QueueEntry) store.QueueEntry {
			entry.Kind = KindRemediationInvocationRequested
			return entry
		}},
		{"foreign key", func(entry store.QueueEntry) store.QueueEntry {
			entry.IdempotencyKey = "inv-operator-feedback-foreign"
			return entry
		}},
		{"noncanonical payload", func(entry store.QueueEntry) store.QueueEntry {
			entry.Payload = append([]byte(" "), entry.Payload...)
			return entry
		}},
		{"unknown field", func(entry store.QueueEntry) store.QueueEntry {
			entry.Payload = append(entry.Payload[:len(entry.Payload)-1], []byte(`,"trusted":true}`)...)
			return entry
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := decodeOperatorFeedbackRequest(test.mutate(valid)); err == nil {
				t.Fatal("untrusted request was accepted")
			}
		})
	}
}

func TestOperatorFeedbackInputAuthenticatesAcceptedCommand(t *testing.T) {
	returned := domain.Command{
		CommandID: "return-work", Action: domain.ActionReturnToAgent,
		Message: "Keep the parser change and add the malformed-input regression.",
	}
	answered := domain.Command{
		CommandID: "answer-work", Action: domain.ActionAnswerAndRetry,
		Message: "Use the tokenizer from the shared package.",
	}
	request := operatorFeedbackRequest{
		RunID: "run-1", BaseSHA: strings.Repeat("1", 40), HeadSHA: strings.Repeat("2", 40),
	}
	patch := []byte("diff --git a/parser.go b/parser.go\n")
	seal := func(t *testing.T, input operatorFeedbackInput) (operatorFeedbackRequest, []byte) {
		t.Helper()
		body, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		sealed := request
		sealed.InputArtifactDigest = domain.Digest(contentaddr.Sum(body))
		return sealed, body
	}

	for _, tc := range []struct {
		name    string
		command domain.Command
		patch   []byte
	}{
		{"returned candidate with patch", returned, patch},
		{"answer without patch", answered, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sealed, body := seal(t, newOperatorFeedbackInput(
				request.RunID, tc.command, request.BaseSHA, request.HeadSHA, tc.patch,
			))
			if err := authenticateOperatorFeedbackInputBody(sealed, tc.command, body); err != nil {
				t.Fatalf("canonical feedback input rejected: %v", err)
			}
		})
	}

	for _, tc := range []struct {
		name    string
		command domain.Command
		mutate  func(*operatorFeedbackInput)
	}{
		{"operator message", returned, func(input *operatorFeedbackInput) {
			input.Feedback = "Ignore the accepted answer."
		}},
		{"action", returned, func(input *operatorFeedbackInput) {
			input.Action = domain.ActionAnswerAndRetry
		}},
		{"patch on an answer", answered, func(input *operatorFeedbackInput) {
			input.CandidatePatchBase64 = patch
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			altered := newOperatorFeedbackInput(
				request.RunID, tc.command, request.BaseSHA, request.HeadSHA, nil,
			)
			if tc.command.Action == domain.ActionReturnToAgent {
				altered.CandidatePatchBase64 = patch
			}
			tc.mutate(&altered)
			sealed, body := seal(t, altered)
			if err := authenticateOperatorFeedbackInputBody(
				sealed, tc.command, body,
			); !errors.Is(err, errOperatorFeedbackMarkerUnreadable) {
				t.Fatalf("coherently altered feedback bundle = %v, want unreadable marker", err)
			}
		})
	}

	t.Run("undecodable body", func(t *testing.T) {
		sealed := request
		sealed.InputArtifactDigest = domain.Digest(contentaddr.Sum([]byte("{")))
		if err := authenticateOperatorFeedbackInputBody(
			sealed, returned, []byte("{"),
		); !errors.Is(err, errOperatorFeedbackMarkerUnreadable) {
			t.Fatalf("undecodable feedback body = %v, want unreadable marker", err)
		}
	})
}

func TestAnswerAndRetryRecordsSpecificationInputAndEnqueuesNextIteration(t *testing.T) {
	f := newSpecificationFixture(t, false, 4)
	f.submit(t)
	driver := f.newDriver(t)
	engine := f.newEngine(t, driver)
	sourceID := specificationInvocationID("specification-run", 1)

	var run domain.Run
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		run, err = tx.GetRun(t.Context(), "specification-run")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	stage, ok := findSpecificationStage(run)
	if !ok {
		t.Fatal("submitted specification run has no specification stage")
	}
	stage.Attempts = append(stage.Attempts, domain.Attempt{
		ID: attemptIDFor(sourceID), StageID: stage.ID, Number: 1, InvocationID: sourceID,
	})
	for index := range run.Stages {
		if run.Stages[index].ID == stage.ID {
			run.Stages[index] = stage
		}
	}
	if err := f.store.Write(t.Context(), func(tx *store.WriteTx) error {
		return tx.PutRun(t.Context(), run)
	}); err != nil {
		t.Fatal(err)
	}

	runID := run.ID
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "question-specification", ProjectID: run.ProjectID,
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(run.ID), RunID: &runID},
		Type:    domain.AttentionAgentQuestion, Priority: domain.PriorityNormal,
		Reason:            "the specifier needs an operator answer",
		RequestedDecision: []domain.Action{domain.ActionAnswerAndRetry, domain.ActionAnswerWithoutRetry},
		AgentClaims:       []domain.AgentClaim{summaryClaimFixture(sourceID, "Which compatibility target applies?")},
		ItemVersion:       1, InterruptionClass: domain.InterruptionExceptional,
		Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.signet.PutItem(t.Context(), item); err != nil {
		t.Fatal(err)
	}
	snapshot, err := f.signet.GetAttentionItem(t.Context(), item.ID)
	if err != nil {
		t.Fatal(err)
	}
	const answer = "Target the current and immediately previous API versions."
	if _, err := f.signet.Submit(t.Context(), signet.ClientCommand{
		CommandID: "answer-specification", DeviceID: "device-1",
		ExpectedEntityVersion: snapshot.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: item.ID, Action: domain.ActionAnswerAndRetry,
			ItemVersion: item.ItemVersion, ArtifactDigests: item.ArtifactDigests,
			Message: answer,
		},
	}); err != nil {
		t.Fatal(err)
	}

	created, err := engine.reconcileOperatorFeedback(t.Context())
	if err != nil || created != 1 {
		t.Fatalf("reconcileOperatorFeedback = %d, %v", created, err)
	}
	nextID := specificationInvocationID(run.ID, 2)
	var (
		entry    store.QueueEntry
		artifact domain.Artifact
	)
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		entry, err = tx.GetOutbox(t.Context(), string(nextID))
		if err != nil {
			return err
		}
		artifact, err = tx.GetArtifact(t.Context(), "answer-answer-specification")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	request, _, err := engine.loadSpecificationBinding(t.Context(), entry)
	if err != nil {
		t.Fatal(err)
	}
	if request.Iteration != 2 || !slices.Contains(request.AnswerArtifactIDs, artifact.ID) ||
		!slices.Contains(request.InputArtifactIDs, artifact.ID) {
		t.Fatalf("next request = %#v, feedback artifact = %#v", request, artifact)
	}
	reader, err := f.blobs.Open(artifact.Digest)
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil {
		t.Fatal(errors.Join(readErr, closeErr))
	}
	if strings.TrimSpace(string(body)) != answer {
		t.Fatalf("recorded answer = %q, want %q", body, answer)
	}
	// Advance the clock so the replay stamps a different answer-artifact
	// created_at: the transition must stay idempotent against the
	// byte-different re-put of the content-addressed artifact (#922), not
	// conflict on it and wedge the reconcile loop.
	*f.now = f.now.Add(time.Hour)
	if replay, err := engine.reconcileOperatorFeedback(t.Context()); err != nil || replay != 0 {
		t.Fatalf("replayed feedback reconciliation = %d, %v", replay, err)
	}
}

func TestAnswerThenRevisionKeepsCanonicalSpecificationInputOrder(t *testing.T) {
	prior := domain.ArtifactID("prior-spec-1")
	request := specificationRequest{
		SpecificationRunID: "run-answer-revision", Iteration: 2,
		InputArtifactIDs:    []domain.ArtifactID{"source", "research", prior, "feedback-1"},
		PriorSpecArtifactID: &prior,
		FeedbackArtifactIDs: []domain.ArtifactID{"feedback-1"},
	}
	answered := nextSpecificationAnswerRequest(request, "answer-1")
	revised := nextSpecificationRevisionRequest(answered, "prior-spec-2", "feedback-2")
	want := []domain.ArtifactID{
		"source", "research", "prior-spec-2", "feedback-1", "feedback-2", "answer-1",
	}
	if !slices.Equal(revised.InputArtifactIDs, want) {
		t.Fatalf("answer-then-revision inputs = %v, want %v", revised.InputArtifactIDs, want)
	}
}

func TestReturnToAgentRecordsFeedbackAndCandidatePatchForResumedWork(t *testing.T) {
	f := newSpecificationFixture(t, false, 4)
	runID := domain.RunID("run-return-to-agent")
	sourceID := productionInvocationID(runID)
	head := strings.Repeat("2", 40)
	base := strings.Repeat("1", 40)
	run := domain.Run{
		ID: runID, ProjectID: "project-1", SpecDigest: f.source.Digest, PolicyDigest: f.policyArt.Digest,
		Stages: []domain.Stage{{
			ID: productionStageID(runID), RunID: runID, Name: productionStageName,
			Attempts: []domain.Attempt{{
				ID: attemptIDFor(sourceID), StageID: productionStageID(runID), Number: 1,
				InvocationID: sourceID,
			}},
		}},
	}
	root, err := domain.NewAgentInvocation(sourceID, []domain.ArtifactID{f.source.ID}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.Write(t.Context(), func(tx *store.WriteTx) error {
		if err := tx.PutRun(t.Context(), run); err != nil {
			return err
		}
		return tx.PutAgentInvocation(t.Context(), root)
	}); err != nil {
		t.Fatal(err)
	}
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: domain.ProductionReadyItemID(runID), ProjectID: run.ProjectID,
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
		Type:    domain.AttentionReadyForFinalReview, Priority: domain.PriorityNormal,
		Reason:            "the candidate is ready for final review",
		RequestedDecision: []domain.Action{domain.ActionOpenPR, domain.ActionReturnToAgent, domain.ActionMarkSeen},
		AgentClaims:       []domain.AgentClaim{summaryClaimFixture(sourceID, "The candidate is ready.")},
		PRHeadSHA:         head, PRReference: &domain.PRReference{Repo: "owner/repo", Number: 42},
		ItemVersion: 1, InterruptionClass: domain.InterruptionPlannedGate,
		Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.signet.PutItem(t.Context(), item); err != nil {
		t.Fatal(err)
	}
	snapshot, err := f.signet.GetAttentionItem(t.Context(), item.ID)
	if err != nil {
		t.Fatal(err)
	}
	const feedback = "Keep the parser change, but add the malformed-input regression case."
	result, err := f.signet.Submit(t.Context(), signet.ClientCommand{
		CommandID: "return-ready-work", DeviceID: "device-1",
		ExpectedEntityVersion: snapshot.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: item.ID, Action: domain.ActionReturnToAgent,
			ItemVersion: item.ItemVersion, PRHeadSHA: item.PRHeadSHA,
			ArtifactDigests: item.ArtifactDigests, Message: feedback,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var concluded domain.AttentionItem
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		concluded, err = tx.GetAttentionItem(t.Context(), item.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	patch := []byte("diff --git a/parser.go b/parser.go\n")
	engine := &Engine{
		store: f.store, signet: f.signet,
		productionPublication: &productionPublicationWorkflow{
			store: f.store, attention: f.signet, artifacts: f.blobs,
			now: func() time.Time { return *f.now },
		},
	}
	created, err := engine.persistImplementationFeedback(
		t.Context(), concluded, result.Record, run, root, sourceID, base, head, patch,
	)
	if err != nil || !created {
		t.Fatalf("persistImplementationFeedback = %t, %v", created, err)
	}
	invocationID := operatorFeedbackInvocationID(result.Record.CommandID)
	var (
		entry    store.QueueEntry
		artifact domain.Artifact
	)
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		entry, err = tx.GetOutbox(t.Context(), string(invocationID))
		if err != nil {
			return err
		}
		artifact, err = tx.GetArtifact(t.Context(), operatorFeedbackArtifactID(result.Record.CommandID))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	request, err := decodeOperatorFeedbackRequest(entry)
	if err != nil || request.BaseSHA != base || request.HeadSHA != head ||
		request.SourceInvocationID != sourceID {
		t.Fatalf("feedback request = %#v, %v", request, err)
	}
	reader, err := f.blobs.Open(artifact.Digest)
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil {
		t.Fatal(errors.Join(readErr, closeErr))
	}
	var input operatorFeedbackInput
	if err := json.Unmarshal(body, &input); err != nil {
		t.Fatal(err)
	}
	if input.Action != domain.ActionReturnToAgent || input.Feedback != feedback ||
		!bytes.Equal(input.CandidatePatchBase64, patch) {
		t.Fatalf("operator feedback input = %#v", input)
	}
	if replay, err := engine.persistImplementationFeedback(
		t.Context(), concluded, result.Record, run, root, sourceID, base, head, patch,
	); err != nil || replay {
		t.Fatalf("replayed feedback persistence = %t, %v", replay, err)
	}
	feedbackInvocation, err := func() (domain.AgentInvocation, error) {
		var invocation domain.AgentInvocation
		err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
			var err error
			invocation, err = tx.GetAgentInvocation(t.Context(), invocationID)
			return err
		})
		return invocation, err
	}()
	if err != nil {
		t.Fatal(err)
	}
	feedbackInvocation.InputIDs[0] = f.policyArt.ID
	corruptBody, err := json.Marshal(feedbackInvocation)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close() //nolint:errcheck // test cleanup
	if _, err := db.ExecContext(t.Context(),
		`UPDATE agent_invocations SET body = ? WHERE id = ?`, string(corruptBody), invocationID); err != nil {
		t.Fatal(err)
	}
	authErr := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		_, _, err := authenticateOperatorFeedbackTransition(
			t.Context(), tx, entry, run.ID, operatorFeedbackStageID(invocationID),
		)
		return err
	})
	if !errors.Is(authErr, domain.ErrParentKeyMismatch) {
		t.Fatalf("corrupt feedback root input authentication = %v, want ErrParentKeyMismatch", authErr)
	}
	started, err := engine.dispatchPendingInvocations(t.Context())
	if err != nil || started != 0 {
		t.Fatalf("dispatch corrupt feedback marker = %d, %v", started, err)
	}
	quarantineID := productionQuarantineOccurrenceID(
		operatorFeedbackMarkerQuarantinePrefix, run.ID, 1)
	quarantine, err := f.signet.GetAttentionItem(t.Context(), quarantineID)
	if err != nil || quarantine.Item.Status != domain.StatusOpen ||
		quarantine.Item.Reason != operatorFeedbackQuarantineUnreadable {
		t.Fatalf("feedback quarantine = %#v, %v", quarantine, err)
	}
}

func TestAnswerAtSpecificationIterationLimitRecordsFailure(t *testing.T) {
	f := newSpecificationFixture(t, false, 1)
	f.submit(t)
	sourceID := specificationInvocationID("specification-run", 1)
	var (
		run   domain.Run
		entry store.QueueEntry
	)
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		run, err = tx.GetRun(t.Context(), "specification-run")
		if err != nil {
			return err
		}
		entry, err = tx.GetOutbox(t.Context(), string(sourceID))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	request, err := decodeSpecificationRequest(entry)
	if err != nil {
		t.Fatal(err)
	}
	stage, ok := findSpecificationStage(run)
	if !ok {
		t.Fatal("submitted specification run has no specification stage")
	}
	stage.Attempts = append(stage.Attempts, domain.Attempt{
		ID: attemptIDFor(sourceID), StageID: stage.ID, Number: 1, InvocationID: sourceID,
	})
	for index := range run.Stages {
		if run.Stages[index].ID == stage.ID {
			run.Stages[index] = stage
		}
	}
	if err := f.store.Write(t.Context(), func(tx *store.WriteTx) error {
		return tx.PutRun(t.Context(), run)
	}); err != nil {
		t.Fatal(err)
	}
	runID := run.ID
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "question-specification-limit", ProjectID: run.ProjectID,
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(run.ID), RunID: &runID},
		Type:    domain.AttentionAgentQuestion, Priority: domain.PriorityNormal,
		Reason:            "the specifier needs an operator answer",
		RequestedDecision: []domain.Action{domain.ActionAnswerAndRetry},
		AgentClaims:       []domain.AgentClaim{summaryClaimFixture(sourceID, "Which target applies?")},
		ItemVersion:       1, InterruptionClass: domain.InterruptionExceptional, Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.signet.PutItem(t.Context(), item); err != nil {
		t.Fatal(err)
	}
	snapshot, err := f.signet.GetAttentionItem(t.Context(), item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.signet.Submit(t.Context(), signet.ClientCommand{
		CommandID: "answer-at-limit", DeviceID: "device-1", ExpectedEntityVersion: snapshot.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: item.ID, Action: domain.ActionAnswerAndRetry,
			ItemVersion: item.ItemVersion, ArtifactDigests: item.ArtifactDigests,
			Message: "Target the current API version.",
		},
	}); err != nil {
		t.Fatal(err)
	}
	engine := f.newEngine(t, f.newDriver(t))
	if transitions, err := engine.reconcileOperatorFeedback(t.Context()); err != nil || transitions != 0 {
		t.Fatalf("limited answer reconciliation = %d, %v", transitions, err)
	}
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		if _, err := tx.GetOutbox(t.Context(), string(specificationInvocationID(run.ID, 2))); !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("next iteration marker = %w, want ErrNotFound", err)
		}
		failure, err := tx.GetAttentionItem(t.Context(), specificationRevisionFailureItemID(request))
		if err != nil {
			return err
		}
		if failure.Type != domain.AttentionExecutionFailure || failure.Status != domain.StatusOpen {
			return fmt.Errorf("iteration-limit failure = %#v", failure)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestOversizedOperatorFeedbackInputParksOnlyItsRun(t *testing.T) {
	f := newSpecificationFixture(t, false, 4)
	runID := domain.RunID("run-oversized-operator-feedback")
	sourceID := productionInvocationID(runID)
	head := strings.Repeat("2", 40)
	run := domain.Run{
		ID: runID, ProjectID: "project-1", SpecDigest: f.source.Digest, PolicyDigest: f.policyArt.Digest,
		Stages: []domain.Stage{{
			ID: productionStageID(runID), RunID: runID, Name: productionStageName,
			Attempts: []domain.Attempt{{
				ID: attemptIDFor(sourceID), StageID: productionStageID(runID), Number: 1,
				InvocationID: sourceID,
			}},
		}},
	}
	root, err := domain.NewAgentInvocation(sourceID, []domain.ArtifactID{f.source.ID}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.Write(t.Context(), func(tx *store.WriteTx) error {
		if err := tx.PutRun(t.Context(), run); err != nil {
			return err
		}
		return tx.PutAgentInvocation(t.Context(), root)
	}); err != nil {
		t.Fatal(err)
	}
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: domain.ProductionReadyItemID(runID), ProjectID: run.ProjectID,
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
		Type:    domain.AttentionReadyForFinalReview, Priority: domain.PriorityNormal,
		Reason:            "the candidate is ready for final review",
		RequestedDecision: []domain.Action{domain.ActionOpenPR, domain.ActionReturnToAgent},
		AgentClaims:       []domain.AgentClaim{summaryClaimFixture(sourceID, "The candidate is ready.")},
		PRHeadSHA:         head, PRReference: &domain.PRReference{Repo: "owner/repo", Number: 42},
		ItemVersion: 1, InterruptionClass: domain.InterruptionPlannedGate, Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.signet.PutItem(t.Context(), item); err != nil {
		t.Fatal(err)
	}
	snapshot, err := f.signet.GetAttentionItem(t.Context(), item.ID)
	if err != nil {
		t.Fatal(err)
	}
	result, err := f.signet.Submit(t.Context(), signet.ClientCommand{
		CommandID: "return-oversized-work", DeviceID: "device-1",
		ExpectedEntityVersion: snapshot.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: item.ID, Action: domain.ActionReturnToAgent,
			ItemVersion: item.ItemVersion, PRHeadSHA: item.PRHeadSHA,
			ArtifactDigests: item.ArtifactDigests, Message: "Keep the candidate and revise the parser.",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var concluded domain.AttentionItem
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		var readErr error
		concluded, readErr = tx.GetAttentionItem(t.Context(), item.ID)
		return readErr
	}); err != nil {
		t.Fatal(err)
	}
	engine := &Engine{
		store: f.store, signet: f.signet,
		productionPublication: &productionPublicationWorkflow{
			store: f.store, attention: f.signet, artifacts: f.blobs,
			now: func() time.Time { return *f.now },
		},
	}
	if transitions, err := engine.reconcileOperatorFeedback(t.Context()); err != nil || transitions != 0 {
		t.Fatalf("global feedback reconciliation = %d, %v", transitions, err)
	}
	patch := bytes.Repeat([]byte("x"), int(exec.ProductionMaxInputBytes))
	created, err := engine.persistImplementationFeedback(
		t.Context(), concluded, result.Record, run, root, sourceID,
		strings.Repeat("1", 40), head, patch,
	)
	if err != nil || !created {
		t.Fatalf("oversized feedback persistence = %t, %v", created, err)
	}

	failureID := operatorFeedbackUndeliverableItemID(result.Record.CommandID)
	var failure domain.AttentionItem
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		var readErr error
		failure, readErr = tx.GetAttentionItem(t.Context(), failureID)
		return readErr
	}); err != nil {
		t.Fatal(err)
	}
	if failure.Type != domain.AttentionExecutionFailure || failure.Status != domain.StatusOpen ||
		!slices.Equal(failure.RequestedDecision, []domain.Action{domain.ActionAcknowledge}) {
		t.Fatalf("oversized feedback failure = %#v", failure)
	}
	var markerErr error
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		_, markerErr = tx.GetOutbox(t.Context(), string(operatorFeedbackInvocationID(result.Record.CommandID)))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(markerErr, store.ErrNotFound) {
		t.Fatalf("feedback invocation marker = %v, want ErrNotFound", markerErr)
	}
	parked, err := engine.operatorFeedbackUndeliverableRecorded(t.Context(), concluded, result.Record)
	if err != nil || !parked {
		t.Fatalf("operatorFeedbackUndeliverableRecorded = %t, %v", parked, err)
	}
	if transitions, err := engine.productionPublication.reconcileOperatorFeedback(
		t.Context(),
	); err != nil || transitions != 0 {
		t.Fatalf("publication feedback reconciliation = %d, %v", transitions, err)
	}
	if replay, err := engine.persistImplementationFeedback(
		t.Context(), concluded, result.Record, run, root, sourceID,
		strings.Repeat("1", 40), head, patch,
	); err != nil || replay {
		t.Fatalf("replayed oversized feedback persistence = %t, %v", replay, err)
	}
}

func TestProductionPublicationContinuesAfterReturnedFeedbackFailure(t *testing.T) {
	f := newSpecificationFixture(t, false, 4)
	runID := domain.RunID("run-return-isolation")
	sourceID := productionInvocationID(runID)
	head := strings.Repeat("2", 40)
	run := domain.Run{
		ID: runID, ProjectID: "project-1", SpecDigest: f.source.Digest, PolicyDigest: f.policyArt.Digest,
		Stages: []domain.Stage{{
			ID: productionStageID(runID), RunID: runID, Name: productionStageName,
			Attempts: []domain.Attempt{{
				ID: attemptIDFor(sourceID), StageID: productionStageID(runID), Number: 1,
				InvocationID: sourceID,
			}},
		}},
	}
	root, err := domain.NewAgentInvocation(sourceID, []domain.ArtifactID{f.source.ID}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.Write(t.Context(), func(tx *store.WriteTx) error {
		if err := tx.PutRun(t.Context(), run); err != nil {
			return err
		}
		return tx.PutAgentInvocation(t.Context(), root)
	}); err != nil {
		t.Fatal(err)
	}
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: domain.ProductionReadyItemID(runID), ProjectID: run.ProjectID,
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
		Type:    domain.AttentionReadyForFinalReview, Priority: domain.PriorityNormal,
		Reason:            "the candidate is ready for final review",
		RequestedDecision: []domain.Action{domain.ActionReturnToAgent},
		AgentClaims:       []domain.AgentClaim{summaryClaimFixture(sourceID, "The candidate is ready.")},
		PRHeadSHA:         head, PRReference: &domain.PRReference{Repo: "owner/repo", Number: 42},
		ItemVersion: 1, InterruptionClass: domain.InterruptionPlannedGate, Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.signet.PutItem(t.Context(), item); err != nil {
		t.Fatal(err)
	}
	snapshot, err := f.signet.GetAttentionItem(t.Context(), item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.signet.Submit(t.Context(), signet.ClientCommand{
		CommandID: "return-isolation", DeviceID: "device-1", ExpectedEntityVersion: snapshot.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: item.ID, Action: domain.ActionReturnToAgent,
			ItemVersion: item.ItemVersion, PRHeadSHA: item.PRHeadSHA,
			ArtifactDigests: item.ArtifactDigests, Message: "Add the malformed-input regression case.",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Write(t.Context(), func(tx *store.WriteTx) error {
		_, _, err := tx.EnqueueOutbox(
			t.Context(), "unattributable-task", KindProductionPublicationRequested, []byte(`{}`),
		)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	workflow := &productionPublicationWorkflow{
		store: f.store, attention: f.signet, signet: f.signet,
		artifacts: f.blobs, workDir: t.TempDir(), now: func() time.Time { return *f.now },
		holdRetryAfter: make(map[string]time.Time),
	}
	result, err := workflow.reconcile(t.Context())
	if err == nil || !strings.Contains(err.Error(), `operator feedback command "return-isolation"`) ||
		!strings.Contains(err.Error(), `task "unattributable-task" cannot be reconstructed`) {
		t.Fatalf("isolated feedback and publication errors = %#v, %v", result, err)
	}
}

func TestOperatorFeedbackRestartsAcrossDurableBoundaries(t *testing.T) {
	for _, transition := range []DurableTransition{
		DurableTransitionSpecificationAnswer,
		DurableTransitionOperatorFeedback,
	} {
		for _, side := range AllDurableTransitionSides {
			t.Run(string(transition)+"/"+string(side), func(t *testing.T) {
				injected := false
				hook := func(observed DurableTransition, observedSide DurableTransitionSide) error {
					if !injected && observed == transition && observedSide == side {
						injected = true
						return errors.New("injected process loss")
					}
					return nil
				}
				switch transition {
				case DurableTransitionSpecificationAnswer:
					assertSpecificationAnswerRestart(t, hook, side, &injected)
				case DurableTransitionOperatorFeedback:
					assertImplementationFeedbackRestart(t, hook, side, &injected)
				default:
					t.Fatalf("unregistered operator-feedback transition %q", transition)
				}
			})
		}
	}
}

func assertSpecificationAnswerRestart(
	t *testing.T,
	hook DurableTransitionHook,
	side DurableTransitionSide,
	injected *bool,
) {
	t.Helper()
	f := newSpecificationFixture(t, false, 4)
	f.submit(t)
	sourceID := specificationInvocationID("specification-run", 1)
	var run domain.Run
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		run, err = tx.GetRun(t.Context(), "specification-run")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	stage, ok := findSpecificationStage(run)
	if !ok {
		t.Fatal("submitted specification run has no specification stage")
	}
	stage.Attempts = append(stage.Attempts, domain.Attempt{
		ID: attemptIDFor(sourceID), StageID: stage.ID, Number: 1, InvocationID: sourceID,
	})
	for index := range run.Stages {
		if run.Stages[index].ID == stage.ID {
			run.Stages[index] = stage
		}
	}
	if err := f.store.Write(t.Context(), func(tx *store.WriteTx) error { return tx.PutRun(t.Context(), run) }); err != nil {
		t.Fatal(err)
	}
	runID := run.ID
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "question-answer-restart", ProjectID: run.ProjectID,
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(run.ID), RunID: &runID},
		Type:    domain.AttentionAgentQuestion, Priority: domain.PriorityNormal,
		Reason:            "the specifier needs an operator answer",
		RequestedDecision: []domain.Action{domain.ActionAnswerAndRetry},
		AgentClaims:       []domain.AgentClaim{summaryClaimFixture(sourceID, "Which compatibility target applies?")},
		ItemVersion:       1, InterruptionClass: domain.InterruptionExceptional, Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.signet.PutItem(t.Context(), item); err != nil {
		t.Fatal(err)
	}
	snapshot, err := f.signet.GetAttentionItem(t.Context(), item.ID)
	if err != nil {
		t.Fatal(err)
	}
	result, err := f.signet.Submit(t.Context(), signet.ClientCommand{
		CommandID: "answer-restart", DeviceID: "device-1", ExpectedEntityVersion: snapshot.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: item.ID, Action: domain.ActionAnswerAndRetry,
			ItemVersion: item.ItemVersion, ArtifactDigests: item.ArtifactDigests,
			Message: "Target the current and immediately previous API versions.",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		item, err = tx.GetAttentionItem(t.Context(), item.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	engine := f.newEngineWithTransitionHook(t, f.newDriver(t), hook)
	if _, err := engine.enqueueSpecificationAnswer(t.Context(), run, item, result.Record); err == nil || !*injected {
		t.Fatalf("injected %s crash = %v, reached %t", side, err, *injected)
	}
	f = f.reopen(t)
	engine = f.newEngine(t, f.newDriver(t))
	created, err := engine.enqueueSpecificationAnswer(t.Context(), run, item, result.Record)
	if err != nil {
		t.Fatal(err)
	}
	if want := side == DurableTransitionBefore; created != want {
		t.Fatalf("recovered specification answer created = %t, want %t", created, want)
	}
	nextID := specificationInvocationID(run.ID, 2)
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		if _, err := tx.GetOutbox(t.Context(), string(nextID)); err != nil {
			return err
		}
		if _, err := tx.GetAgentInvocation(t.Context(), nextID); err != nil {
			return err
		}
		_, err := tx.GetArtifact(t.Context(), "answer-answer-restart")
		return err
	}); err != nil {
		t.Fatalf("recovered specification answer identities: %v", err)
	}
}

func assertImplementationFeedbackRestart(
	t *testing.T,
	hook DurableTransitionHook,
	side DurableTransitionSide,
	injected *bool,
) {
	t.Helper()
	f := newSpecificationFixture(t, false, 4)
	runID := domain.RunID("run-feedback-restart")
	sourceID := productionInvocationID(runID)
	base := strings.Repeat("1", 40)
	head := strings.Repeat("2", 40)
	run := domain.Run{
		ID: runID, ProjectID: "project-1", SpecDigest: f.source.Digest, PolicyDigest: f.policyArt.Digest,
		Stages: []domain.Stage{{
			ID: productionStageID(runID), RunID: runID, Name: productionStageName,
			Attempts: []domain.Attempt{{
				ID: attemptIDFor(sourceID), StageID: productionStageID(runID), Number: 1,
				InvocationID: sourceID,
			}},
		}},
	}
	root, err := domain.NewAgentInvocation(sourceID, []domain.ArtifactID{f.source.ID}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.Write(t.Context(), func(tx *store.WriteTx) error {
		if err := tx.PutRun(t.Context(), run); err != nil {
			return err
		}
		return tx.PutAgentInvocation(t.Context(), root)
	}); err != nil {
		t.Fatal(err)
	}
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: domain.ProductionReadyItemID(runID), ProjectID: run.ProjectID,
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
		Type:    domain.AttentionReadyForFinalReview, Priority: domain.PriorityNormal,
		Reason:            "the candidate is ready for final review",
		RequestedDecision: []domain.Action{domain.ActionReturnToAgent},
		AgentClaims:       []domain.AgentClaim{summaryClaimFixture(sourceID, "The candidate is ready.")},
		PRHeadSHA:         head, PRReference: &domain.PRReference{Repo: "owner/repo", Number: 42},
		ItemVersion: 1, InterruptionClass: domain.InterruptionPlannedGate, Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.signet.PutItem(t.Context(), item); err != nil {
		t.Fatal(err)
	}
	snapshot, err := f.signet.GetAttentionItem(t.Context(), item.ID)
	if err != nil {
		t.Fatal(err)
	}
	result, err := f.signet.Submit(t.Context(), signet.ClientCommand{
		CommandID: "feedback-restart", DeviceID: "device-1", ExpectedEntityVersion: snapshot.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: item.ID, Action: domain.ActionReturnToAgent,
			ItemVersion: item.ItemVersion, PRHeadSHA: item.PRHeadSHA,
			ArtifactDigests: item.ArtifactDigests, Message: "Add the malformed-input regression case.",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		item, err = tx.GetAttentionItem(t.Context(), item.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	workflow := &productionPublicationWorkflow{
		store: f.store, attention: f.signet, artifacts: f.blobs, transitionHook: hook,
		now: func() time.Time { return *f.now },
	}
	engine := &Engine{store: f.store, signet: f.signet, productionPublication: workflow}
	patch := []byte("diff --git a/parser.go b/parser.go\n")
	if _, err := engine.persistImplementationFeedback(
		t.Context(), item, result.Record, run, root, sourceID, base, head, patch,
	); err == nil || !*injected {
		t.Fatalf("injected %s crash = %v, reached %t", side, err, *injected)
	}
	f = f.reopen(t)
	workflow = &productionPublicationWorkflow{
		store: f.store, attention: f.signet, artifacts: f.blobs,
		now: func() time.Time { return *f.now },
	}
	engine = &Engine{store: f.store, signet: f.signet, productionPublication: workflow}
	created, err := engine.persistImplementationFeedback(
		t.Context(), item, result.Record, run, root, sourceID, base, head, patch,
	)
	if err != nil {
		t.Fatal(err)
	}
	if want := side == DurableTransitionBefore; created != want {
		t.Fatalf("recovered implementation feedback created = %t, want %t", created, want)
	}
	invocationID := operatorFeedbackInvocationID(result.Record.CommandID)
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		if _, err := tx.GetOutbox(t.Context(), string(invocationID)); err != nil {
			return err
		}
		if _, err := tx.GetAgentInvocation(t.Context(), invocationID); err != nil {
			return err
		}
		if _, err := tx.GetArtifact(t.Context(), operatorFeedbackArtifactID(result.Record.CommandID)); err != nil {
			return err
		}
		recovered, err := tx.GetRun(t.Context(), run.ID)
		if err != nil {
			return err
		}
		stages := 0
		for _, stage := range recovered.Stages {
			if stage.ID == operatorFeedbackStageID(invocationID) {
				stages++
			}
		}
		if stages != 1 {
			return errors.New("recovered operator-feedback stage count is not one")
		}
		return nil
	}); err != nil {
		t.Fatalf("recovered implementation feedback identities: %v", err)
	}
}
