package engine

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func productionEntry(payload string) store.QueueEntry {
	return store.QueueEntry{
		IdempotencyKey: "inv-implement-run-1",
		Kind:           KindProductionInvocationRequested,
		Payload:        []byte(payload),
	}
}

func TestProductionHoldRetryPruning(t *testing.T) {
	w := productionPublicationWorkflow{holdRetryAfter: map[domain.RunID]time.Time{
		"run-pending":  time.Unix(1, 0),
		"run-finished": time.Unix(2, 0),
	}}
	w.pruneHeldTaskRetries([]store.QueueEntry{{
		IdempotencyKey: productionPublicationTaskKey("run-pending"),
	}})
	if _, found := w.holdRetryAfter["run-pending"]; !found {
		t.Fatal("pending task retry deadline was pruned")
	}
	if _, found := w.holdRetryAfter["run-finished"]; found {
		t.Fatal("finished task retry deadline was retained")
	}
}

func TestProductionPublicationErrorClassification(t *testing.T) {
	t.Parallel()
	for _, err := range []error{
		domain.ErrParentKeyMismatch,
		domain.ErrImmutableTransition,
		domain.ErrInvalidOperatingMode,
		domain.ErrPathBoundaryMismatch,
		store.ErrNotFound,
		store.ErrImmutableConflict,
		store.ErrStaleWrite,
	} {
		if !productionPublicationStateContradiction(fmt.Errorf("reconcile task: %w", err)) {
			t.Errorf("%v was not classified as a durable contradiction", err)
		}
		if productionPublicationRetryableFailure(fmt.Errorf("reconcile task: %w", err)) {
			t.Errorf("%v was classified as retryable", err)
		}
	}
	for _, err := range []error{
		productionPublicationRetryableError(errors.New("container exited -1")),
		&net.DNSError{Err: "temporary", Name: "github.com"},
		&publish.APIError{Status: 503, RequestPath: "/repos/o/r"},
		&publish.TransportGitError{
			Args: []string{"fetch"}, ExitCode: -1,
			Refusal: publish.RefusalUnknown, Err: context.DeadlineExceeded,
		},
		publish.ErrJanitorInactive,
		publish.ErrInstallationGrantUntrusted,
		&os.PathError{Op: "write", Path: "/state", Err: os.ErrPermission},
		context.DeadlineExceeded,
	} {
		if !productionPublicationRetryableFailure(err) {
			t.Errorf("%v was not classified as retryable", err)
		}
	}
	for _, err := range []error{
		errors.New("malformed durable checkpoint"),
		publish.ErrGitTransport,
	} {
		if productionPublicationRetryableFailure(err) {
			t.Errorf("durable or untyped error %v was classified as retryable", err)
		}
	}
	if !productionPublicationPermanentExternalFailure(&publish.APIError{Status: 401, RequestPath: "/repos/o/r"}) {
		t.Fatal("a permanent forge refusal was not classified for durable hold")
	}
	for _, status := range []int{403, 429, 503} {
		if productionPublicationPermanentExternalFailure(&publish.APIError{
			Status: status, RequestPath: "/repos/o/r",
		}) {
			t.Errorf("retryable forge status %d was classified for durable hold", status)
		}
	}
	for _, err := range []error{
		publish.ErrRemoteMissingBase,
		publish.ErrAmbiguousInstallation,
		publish.ErrInstallationResolution,
		publish.ErrGrantMismatch,
	} {
		if !productionPublicationPermanentExternalFailure(err) {
			t.Errorf("permanent external refusal %v was not classified for durable hold", err)
		}
	}
	if !productionPublicationPermanentExternalFailure(&publish.TransportGitError{
		Args: []string{"push"}, ExitCode: 128, Refusal: publish.RefusalAuth,
	}) {
		t.Fatal("a permanent transport authentication refusal was not classified for durable hold")
	}
}

const testProductionPublicationJSON = `"publication":{"title":"Test production work item","body":"Reviewer context.","commit_author":{"app_slug":"freeside-test","bot_user_id":12345}}`

func productionRequestJSON(fields string) string {
	return `{` + fields + `,` + testProductionPublicationJSON + `}`
}

func TestDecodeProductionRequestRejectsMalformedPayloads(t *testing.T) {
	t.Parallel()
	canonical := productionRequestJSON(
		`"invocation_id":"inv-implement-run-1","run_id":"run-1","stage_id":"implement-run-1"`,
	)
	tests := []struct {
		name    string
		entry   store.QueueEntry
		wantErr error
	}{
		{"empty", productionEntry(``), nil},
		{"trailing value", productionEntry(canonical + ` {}`), nil},
		{"unknown field", productionEntry(productionRequestJSON(`"invocation_id":"inv-implement-run-1","run_id":"run-1","stage_id":"implement-run-1","extra":1`)), nil},
		{"noncanonical legacy", productionEntry(`{ "invocation_id":"inv-implement-run-1","run_id":"run-1","stage_id":"implement-run-1"}`), domain.ErrParentKeyMismatch},
		{"null version", productionEntry(`{"version":null,"invocation_id":"inv-implement-run-1","run_id":"run-1","stage_id":"implement-run-1"}`), nil},
		{"unknown version", productionEntry(productionRequestJSON(`"version":"freeside.production-invocation/v3","invocation_id":"inv-implement-run-1","run_id":"run-1","stage_id":"implement-run-1"`)), nil},
		{"v2 missing publication", productionEntry(`{"version":"freeside.production-invocation/v2","invocation_id":"inv-implement-run-1","run_id":"run-1","stage_id":"implement-run-1"}`), nil},
		{"null publication", productionEntry(`{"invocation_id":"inv-implement-run-1","run_id":"run-1","stage_id":"implement-run-1","publication":null}`), nil},
		{"missing run", productionEntry(productionRequestJSON(`"invocation_id":"inv-implement-run-1","stage_id":"implement-run-1"`)), domain.ErrEmptyID},
		{"missing stage", productionEntry(productionRequestJSON(`"invocation_id":"inv-implement-run-1","run_id":"run-1"`)), domain.ErrEmptyID},
		{"key mismatch", func() store.QueueEntry {
			e := productionEntry(productionRequestJSON(`"invocation_id":"inv-implement-run-2","run_id":"run-2","stage_id":"implement-run-2"`))
			return e
		}(), domain.ErrParentKeyMismatch},
		{"foreign kind", func() store.QueueEntry {
			e := productionEntry(canonical)
			e.Kind = "agent_invocation_requested"
			return e
		}(), domain.ErrParentKeyMismatch},
		{"underived invocation id", func() store.QueueEntry {
			e := productionEntry(productionRequestJSON(`"invocation_id":"inv-custom","run_id":"run-1","stage_id":"implement-run-1"`))
			e.IdempotencyKey = "inv-custom"
			return e
		}(), domain.ErrParentKeyMismatch},
		{"underived stage id", func() store.QueueEntry {
			e := productionEntry(productionRequestJSON(`"invocation_id":"inv-implement-run-1","run_id":"run-1","stage_id":"feedback-run-1"`))
			return e
		}(), domain.ErrParentKeyMismatch},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeProductionRequest(tc.entry)
			if err == nil {
				t.Fatal("decode accepted malformed entry")
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("decode error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func terminalEntry(payload string) store.QueueEntry {
	return store.QueueEntry{
		IdempotencyKey: "inv-implement-run-1",
		Kind:           kindProductionStageTerminal,
		Payload:        []byte(payload),
	}
}

// TestDecodeProductionTerminalRejectsForgedRecords: a stored terminal record
// is a reconstruction boundary. Trusting one by kind alone lets a corrupted
// or fabricated row permanently suppress an attempt's collection, which
// means neither an accepted result nor the execution_failure item that would
// otherwise make the failure visible.
func TestDecodeProductionTerminalRejectsForgedRecords(t *testing.T) {
	t.Parallel()
	run := domain.Run{ID: "run-1", ProjectID: "proj-1"}
	canonical := `{"invocation_id":"inv-implement-run-1","run_id":"run-1",` +
		`"stage_id":"implement-run-1","status":"completed"}`
	tests := []struct {
		name    string
		entry   store.QueueEntry
		wantErr error
	}{
		{"empty", terminalEntry(``), nil},
		{"trailing value", terminalEntry(canonical + ` {}`), nil},
		{"unknown field", terminalEntry(`{"invocation_id":"inv-implement-run-1","run_id":"run-1",` +
			`"stage_id":"implement-run-1","status":"completed","extra":1}`), nil},
		{"foreign run", terminalEntry(`{"invocation_id":"inv-implement-run-1","run_id":"run-2",` +
			`"stage_id":"implement-run-2","status":"completed"}`), domain.ErrParentKeyMismatch},
		{"underived stage", terminalEntry(`{"invocation_id":"inv-implement-run-1","run_id":"run-1",` +
			`"stage_id":"feedback-run-1","status":"completed"}`), domain.ErrParentKeyMismatch},
		{"non-terminal status", terminalEntry(`{"invocation_id":"inv-implement-run-1","run_id":"run-1",` +
			`"stage_id":"implement-run-1","status":"running"}`), exec.ErrInvalidStatus},
		{"unknown status", terminalEntry(`{"invocation_id":"inv-implement-run-1","run_id":"run-1",` +
			`"stage_id":"implement-run-1","status":"finished"}`), exec.ErrInvalidStatus},
		{"foreign kind", func() store.QueueEntry {
			e := terminalEntry(canonical)
			e.Kind = "agent_completion"
			return e
		}(), domain.ErrParentKeyMismatch},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeProductionTerminal(tc.entry, run)
			if err == nil {
				t.Fatal("a forged terminal record was accepted as authoritative")
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("decode error = %v, want %v", err, tc.wantErr)
			}
		})
	}

	// The lane's own records still round-trip, including the gone outcome it
	// writes for a session lost without a result.
	for _, status := range []exec.Status{exec.StatusCompleted, exec.StatusFailed, exec.StatusCanceled, exec.StatusGone} {
		entry := terminalEntry(`{"invocation_id":"inv-implement-run-1","run_id":"run-1",` +
			`"stage_id":"implement-run-1","status":"` + string(status) + `"}`)
		if _, err := decodeProductionTerminal(entry, run); err != nil {
			t.Errorf("canonical %q record rejected: %v", status, err)
		}
	}
}

func TestDecodeProductionRequestAcceptsCanonicalPayload(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		payload     string
		wantLegacy  bool
		wantVersion string
	}{
		{
			name:       "released legacy v1",
			payload:    `{"invocation_id":"inv-implement-run-1","run_id":"run-1","stage_id":"implement-run-1"}`,
			wantLegacy: true,
		},
		{
			name:    "unversioned publication preview",
			payload: productionRequestJSON(`"invocation_id":"inv-implement-run-1","run_id":"run-1","stage_id":"implement-run-1"`),
		},
		{
			name:        "publication v2",
			payload:     productionRequestJSON(`"version":"freeside.production-invocation/v2","invocation_id":"inv-implement-run-1","run_id":"run-1","stage_id":"implement-run-1"`),
			wantVersion: productionInvocationRequestVersion,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeProductionRequest(productionEntry(tc.payload))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got.InvocationID != "inv-implement-run-1" || got.RunID != "run-1" ||
				got.StageID != "implement-run-1" || got.Legacy != tc.wantLegacy || got.Version != tc.wantVersion {
				t.Fatalf("decoded request = %#v", got)
			}
		})
	}
}

func TestProductionOwnershipReGatesTheMarkerPayload(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tests := []struct {
		name    string
		runID   domain.RunID
		payload string
		wantErr bool
	}{
		{
			name:  "canonical",
			runID: "run-owned",
			payload: productionRequestJSON(`"invocation_id":"inv-implement-run-owned",` +
				`"run_id":"run-owned","stage_id":"implement-run-owned"`),
		},
		{
			name:    "malformed",
			runID:   "run-malformed",
			payload: `{"run_id":"run-malformed"}`,
			wantErr: true,
		},
		{
			name:  "retargeted",
			runID: "run-retargeted",
			payload: productionRequestJSON(`"invocation_id":"inv-implement-run-other",` +
				`"run_id":"run-other","stage_id":"implement-run-other"`),
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st, err := store.Open(
				ctx, filepath.Join(t.TempDir(), "freeside.db"), store.Options{},
			)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
				_, _, err := tx.EnqueueOutbox(
					ctx, string(productionInvocationID(tc.runID)),
					KindProductionInvocationRequested, []byte(tc.payload),
				)
				return err
			}); err != nil {
				t.Fatalf("seed marker: %v", err)
			}

			owned, err := (&Engine{store: st}).ownsProductionRun(
				ctx, domain.Run{ID: tc.runID},
			)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("malformed marker reported ownership = %v", owned)
				}
				return
			}
			if err != nil || !owned {
				t.Fatalf("canonical marker ownership = %v, %v", owned, err)
			}
		})
	}
}

func TestLoadProductionBindingAuthenticatesTheSpecificationInput(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, err := store.Open(
		ctx, filepath.Join(t.TempDir(), "freeside.db"), store.Options{},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	specification := domain.Artifact{
		ID: "artifact-specification", Type: productionSpecificationArtifactType,
		Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Provenance: domain.Provenance{
			ProducerClass:        domain.ProducerDaemon,
			ProducerInvocationID: "submit-specification",
			HeadBinding:          domain.HeadIndependent,
			SensitivityClass:     domain.SensitivityNormal,
		},
	}
	extra := specification
	extra.ID = "artifact-extra"
	extra.Type = "evidence"
	extra.Digest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	extra.Provenance.ProducerInvocationID = "foreign-producer"
	run := domain.Run{
		ID: "run-binding", ProjectID: "project-binding",
		SpecDigest:   specification.Digest,
		PolicyDigest: "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		Stages: []domain.Stage{{
			ID: productionStageID("run-binding"), RunID: "run-binding",
			Name: productionStageName, Attempts: []domain.Attempt{},
		}},
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutArtifact(ctx, specification); err != nil {
			return err
		}
		if err := tx.PutArtifact(ctx, extra); err != nil {
			return err
		}
		return tx.PutRun(ctx, run)
	}); err != nil {
		t.Fatalf("seed binding state: %v", err)
	}

	tests := []struct {
		name    string
		inputs  []domain.ArtifactID
		wantErr bool
	}{
		{"canonical", []domain.ArtifactID{specification.ID}, false},
		{"extra input", []domain.ArtifactID{specification.ID, extra.ID}, true},
		{"foreign input", []domain.ArtifactID{extra.ID}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			invocationID := domain.InvocationID(
				"inv-implement-run-binding-" + strings.ReplaceAll(tc.name, " ", "-"),
			)
			invocation, err := domain.NewAgentInvocation(
				invocationID, tc.inputs, nil, 0,
			)
			if err != nil {
				t.Fatalf("new invocation: %v", err)
			}
			if err := st.Write(ctx, func(tx *store.WriteTx) error {
				return tx.PutAgentInvocation(ctx, invocation)
			}); err != nil {
				t.Fatalf("seed invocation: %v", err)
			}
			_, err = (&Engine{store: st}).loadProductionBinding(
				ctx, productionInvocationRequest{
					InvocationID: invocationID, RunID: run.ID,
					StageID: productionStageID(run.ID),
				},
			)
			if tc.wantErr {
				if !errors.Is(err, domain.ErrParentKeyMismatch) {
					t.Fatalf("binding error = %v, want ErrParentKeyMismatch", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("canonical binding: %v", err)
			}
		})
	}
}

func TestProductionAcceptanceRequiresDurableAdmission(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	e := &Engine{store: st}
	if err := e.requireProductionAdmissible(ctx, "inv-implement-run-1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing production admission error = %v, want store.ErrNotFound", err)
	}
}
