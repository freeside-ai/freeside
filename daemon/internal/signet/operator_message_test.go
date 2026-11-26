package signet_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

var writerMessageAt = time.Date(2026, 11, 26, 17, 0, 0, 0, time.UTC)

// fakeWriterSender answers each offer with answer and keeps every offer.
type fakeWriterSender struct {
	mu     sync.Mutex
	offers []domain.OperatorMessage
	answer func(domain.OperatorMessage) (*domain.OperatorMessageRefusal, error)
}

func (s *fakeWriterSender) OfferWriterMessage(
	_ context.Context, m domain.OperatorMessage,
) (*domain.OperatorMessageRefusal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.offers = append(s.offers, m)
	if s.answer == nil {
		return nil, nil
	}
	return s.answer(m)
}

func (s *fakeWriterSender) offered() []domain.OperatorMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]domain.OperatorMessage(nil), s.offers...)
}

// seedWriterPrerequisites records what a seeded writer's admission
// needs: the provider identity it names, and a cordoned launch cap.
func seedWriterPrerequisites(t *testing.T, st *store.Store) {
	t.Helper()
	capValue, err := json.Marshal(domain.CordonedLaunchSettings{MaxConcurrent: 4})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(t.Context(), func(tx *store.WriteTx) error {
		if _, err := tx.AppendInstanceSettingsRevision(t.Context(), domain.InstanceSettingsInput{
			Section: domain.InstanceSectionCordonedLaunches, Value: capValue, SetBy: "cli:operator",
		}, writerMessageAt); err != nil {
			return err
		}
		return tx.RecordAuthIdentity(t.Context(), domain.AuthIdentity{
			ID: "auth-1", Provider: "claude", AuthStoreMutationLease: true,
			Interim: domain.InterimClientFacts{AuthStoreVolume: "provider-cred", RefreshStrategy: domain.RefreshOnDemand},
		}, writerMessageAt)
	}); err != nil {
		t.Fatal(err)
	}
}

// seedWriter records invocation as the one attempt of runID's stage named
// stage, admitted to backend.
func seedWriter(
	t *testing.T, st *store.Store, runID domain.RunID, invocation domain.InvocationID, stage string, backend domain.RunnerBackendClass,
) {
	t.Helper()
	attempt := corpusAttempt(runID, invocation)
	run := corpusRun(runID, attempt)
	run.Stages[0].Name = stage
	input := corpusAdmissionInput(runID, corpusStageID(runID), attempt.ID, invocation, writerMessageAt)
	input.Backend = string(backend)
	if backend == domain.BackendCordonedHostProcess {
		// A cordoned writer has the host's network (validateEgressClass).
		input.EgressProfile = domain.EgressHostOpen
	}
	admission, err := domain.NewExecutionAdmission(input)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(t.Context(), func(tx *store.WriteTx) error {
		if err := tx.PutRun(t.Context(), run); err != nil {
			return err
		}
		return tx.RecordExecutionAdmission(t.Context(), admission)
	}); err != nil {
		t.Fatal(err)
	}
}

func seedCordonedWriter(t *testing.T, st *store.Store, runID domain.RunID, invocation domain.InvocationID) {
	t.Helper()
	seedWriter(t, st, runID, invocation, corpusRun(runID).Stages[0].Name, domain.BackendCordonedHostProcess)
}

func writerMessageCommand(commandID string, runID domain.RunID, invocation domain.InvocationID, text string) signet.ClientCommand {
	return signet.ClientCommand{
		CommandID: commandID, DeviceID: "device-1", Kind: domain.CommandKindSendWriterMessage,
		SendWriterMessage: signet.SendWriterMessagePayload{RunID: runID, InvocationID: invocation, Text: text},
	}
}

type writerMessageFixture struct {
	fixture
	sender  *fakeWriterSender
	handler http.Handler
}

// newWriterMessageFixture binds sender unless it is nil, and seeds a
// cordoned writer, writer-1 of run-writer.
func newWriterMessageFixture(t *testing.T, sender *fakeWriterSender) writerMessageFixture {
	t.Helper()
	f := newFixture(t)
	var options []signet.Option
	if sender != nil {
		options = append(options, signet.WithWriterMessages(func() signet.WriterMessageSender { return sender }))
	}
	options = append(options, signet.WithClock(func() time.Time { return writerMessageAt }))
	svc := signet.NewService(f.store, options...)
	seedWriterPrerequisites(t, f.store)
	seedCordonedWriter(t, f.store, "run-writer", "writer-1")
	return writerMessageFixture{fixture: f, sender: sender, handler: signet.NewHTTPHandler(svc, testAuthorizer)}
}

func (f writerMessageFixture) post(t *testing.T, commandID string, payload map[string]any) (int, []byte) {
	t.Helper()
	payload["kind"] = "send_writer_message"
	body, err := json.Marshal(map[string]any{"command_id": commandID, "device_id": "device-1", "payload": payload})
	if err != nil {
		t.Fatal(err)
	}
	response := authenticatedRequest(t, f.handler, http.MethodPost, "/commands", bytes.NewReader(body))
	return response.Code, response.Body.Bytes()
}

func (f writerMessageFixture) messages(t *testing.T, invocation domain.InvocationID) []domain.OperatorMessage {
	t.Helper()
	var out []domain.OperatorMessage
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		out, err = tx.OperatorMessages(t.Context(), invocation)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

type writerMessageResponse struct {
	Record struct {
		Kind domain.CommandKind `json:"kind"`
		domain.OperatorMessage
	} `json:"record"`
}

func decodeWriterMessage(t *testing.T, body []byte) domain.OperatorMessage {
	t.Helper()
	var response writerMessageResponse
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	if response.Record.Kind != domain.CommandKindSendWriterMessage {
		t.Fatalf("record kind %q", response.Record.Kind)
	}
	return response.Record.OperatorMessage
}

// TestSendWriterMessageRecordsOffersAndSettles sends a message to a
// cordoned writer: it is recorded, offered once, settled delivered, and
// served on the transcript read; a retry of its command_id offers nothing
// again and answers with the same record.
func TestSendWriterMessageRecordsOffersAndSettles(t *testing.T) {
	t.Parallel()
	f := newWriterMessageFixture(t, &fakeWriterSender{})
	payload := map[string]any{"run_id": "run-writer", "invocation_id": "writer-1", "text": "the bug is in the lexer"}
	code, body := f.post(t, "cmd-1", payload)
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, body)
	}
	got := decodeWriterMessage(t, body)
	if got.State != domain.OperatorMessageDelivered || got.Ordinal != 1 || got.Sender != "device:device-1" {
		t.Fatalf("record = %+v", got)
	}
	offers := f.sender.offered()
	if len(offers) != 1 || offers[0].CommandID != "cmd-1" || offers[0].State != domain.OperatorMessageRecorded {
		t.Fatalf("offers = %+v; want one, of the recorded message", offers)
	}
	retry, again := f.post(t, "cmd-1", payload)
	if retry != http.StatusOK || !bytes.Equal(again, body) || len(f.sender.offered()) != 1 {
		t.Fatalf("retry = %d %s, %d offers", retry, again, len(f.sender.offered()))
	}
	payload["text"] = "something else"
	if code, body := f.post(t, "cmd-1", payload); code != http.StatusBadRequest {
		t.Fatalf("another request under cmd-1 = %d %s, want 400", code, body)
	}
	code, body = f.post(t, "cmd-2", payload)
	if code != http.StatusOK || decodeWriterMessage(t, body).Ordinal != 2 {
		t.Fatalf("second message = %d %s", code, body)
	}

	response := authenticatedRequest(t, f.handler, http.MethodGet,
		"/runs/run-writer/invocations/writer-1/transcript", nil)
	var transcript signet.InvocationTranscript
	if err := json.Unmarshal(response.Body.Bytes(), &transcript); err != nil || response.Code != http.StatusOK {
		t.Fatalf("transcript = %d %s, %v", response.Code, response.Body, err)
	}
	if len(transcript.OperatorMessages) != 2 || transcript.OperatorMessages[0].CommandID != "cmd-1" ||
		transcript.OperatorMessages[1].CommandID != "cmd-2" {
		t.Fatalf("transcript messages = %+v", transcript.OperatorMessages)
	}
}

// TestSendWriterMessageSettlesWhatTheRunAnswers settles a refusal with its
// reason, and leaves a message the offer couldn't settle recorded.
func TestSendWriterMessageSettlesWhatTheRunAnswers(t *testing.T) {
	t.Parallel()
	notTaken := domain.OperatorMessageRefusalNotTaken
	f := newWriterMessageFixture(t, &fakeWriterSender{answer: func(m domain.OperatorMessage) (*domain.OperatorMessageRefusal, error) {
		if m.CommandID == "cmd-refused" {
			return &notTaken, nil
		}
		return nil, errors.New("the driver is not running")
	}})
	for command, want := range map[string]domain.OperatorMessageState{
		"cmd-refused": domain.OperatorMessageRefused, "cmd-unknown": domain.OperatorMessageRecorded,
	} {
		code, body := f.post(t, command, map[string]any{"run_id": "run-writer", "invocation_id": "writer-1", "text": "hi"})
		if code != http.StatusOK {
			t.Fatalf("%s: status %d: %s", command, code, body)
		}
		if got := decodeWriterMessage(t, body); got.State != want {
			t.Fatalf("%s: record = %+v, want %s", command, got, want)
		}
	}
	for _, m := range f.messages(t, "writer-1") {
		if m.CommandID == "cmd-refused" && (m.Refusal == nil || *m.Refusal != notTaken) {
			t.Fatalf("stored refusal = %+v", m)
		}
	}
}

// TestSendWriterMessageRefusesBeforeRecording refuses each request the
// daemon can judge alone, recording nothing and offering nothing.
func TestSendWriterMessageRefusesBeforeRecording(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		unbound bool
		payload map[string]any
		want    int
	}{
		"empty text":         {payload: map[string]any{"text": ""}, want: http.StatusBadRequest},
		"only whitespace":    {payload: map[string]any{"text": " \n"}, want: http.StatusBadRequest},
		"text over limit":    {payload: map[string]any{"text": strings.Repeat("x", domain.MaxOperatorMessageBytes+1)}, want: http.StatusBadRequest},
		"a warded writer":    {payload: map[string]any{"run_id": "run-warded", "invocation_id": "writer-warded"}, want: http.StatusBadRequest},
		"a reviewer":         {payload: map[string]any{"run_id": "run-review", "invocation_id": "reviewer-1"}, want: http.StatusBadRequest},
		"another run":        {payload: map[string]any{"run_id": "run-warded"}, want: http.StatusNotFound},
		"an unknown run":     {payload: map[string]any{"run_id": "run-none"}, want: http.StatusNotFound},
		"no bound sender":    {unbound: true, payload: map[string]any{}, want: http.StatusBadRequest},
		"a decision version": {payload: map[string]any{"text": "hi"}, want: http.StatusBadRequest},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			sender := &fakeWriterSender{}
			bound := sender
			if tc.unbound {
				bound = nil
			}
			f := newWriterMessageFixture(t, bound)
			seedWriter(t, f.store, "run-warded", "writer-warded", corpusRun("run-warded").Stages[0].Name,
				domain.BackendFreshVMReadOnlyVolumeHandoff)
			seedWriter(t, f.store, "run-review", "reviewer-1", "review", domain.BackendCordonedHostProcess)
			payload := map[string]any{"run_id": "run-writer", "invocation_id": "writer-1", "text": "hi"}
			for key, value := range tc.payload {
				payload[key] = value
			}
			before, _ := f.store.ServerState(t.Context())
			var code int
			var body []byte
			if name == "a decision version" {
				payload["kind"] = "send_writer_message"
				raw, _ := json.Marshal(map[string]any{
					"command_id": "cmd-1", "device_id": "device-1", "expected_entity_version": 1, "payload": payload,
				})
				response := authenticatedRequest(t, f.handler, http.MethodPost, "/commands", bytes.NewReader(raw))
				code, body = response.Code, response.Body.Bytes()
			} else {
				code, body = f.post(t, "cmd-1", payload)
			}
			if code != tc.want {
				t.Fatalf("status %d want %d: %s", code, tc.want, body)
			}
			if after, _ := f.store.ServerState(t.Context()); after != before {
				t.Fatal("a refused message committed a write")
			}
			for _, invocation := range []domain.InvocationID{"writer-1", "writer-warded", "reviewer-1"} {
				if got := f.messages(t, invocation); len(got) != 0 {
					t.Fatalf("%s holds %+v", invocation, got)
				}
			}
			if len(sender.offered()) != 0 {
				t.Fatal("a refused message was offered")
			}
		})
	}
}
