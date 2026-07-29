package engine

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func productionEntry(payload string) store.QueueEntry {
	return store.QueueEntry{
		IdempotencyKey: "inv-implement-run-1",
		Kind:           KindProductionInvocationRequested,
		Payload:        []byte(payload),
	}
}

func TestDecodeProductionRequestRejectsMalformedPayloads(t *testing.T) {
	t.Parallel()
	canonical := `{"invocation_id":"inv-implement-run-1","run_id":"run-1","stage_id":"implement-run-1"}`
	tests := []struct {
		name    string
		entry   store.QueueEntry
		wantErr error
	}{
		{"empty", productionEntry(``), nil},
		{"trailing value", productionEntry(canonical + ` {}`), nil},
		{"unknown field", productionEntry(`{"invocation_id":"inv-implement-run-1","run_id":"run-1","stage_id":"implement-run-1","extra":1}`), nil},
		{"missing run", productionEntry(`{"invocation_id":"inv-implement-run-1","stage_id":"implement-run-1"}`), domain.ErrEmptyID},
		{"missing stage", productionEntry(`{"invocation_id":"inv-implement-run-1","run_id":"run-1"}`), domain.ErrEmptyID},
		{"key mismatch", func() store.QueueEntry {
			e := productionEntry(`{"invocation_id":"inv-implement-run-2","run_id":"run-2","stage_id":"implement-run-2"}`)
			return e
		}(), domain.ErrParentKeyMismatch},
		{"foreign kind", func() store.QueueEntry {
			e := productionEntry(canonical)
			e.Kind = "agent_invocation_requested"
			return e
		}(), domain.ErrParentKeyMismatch},
		{"underived invocation id", func() store.QueueEntry {
			e := productionEntry(`{"invocation_id":"inv-custom","run_id":"run-1","stage_id":"implement-run-1"}`)
			e.IdempotencyKey = "inv-custom"
			return e
		}(), domain.ErrParentKeyMismatch},
		{"underived stage id", func() store.QueueEntry {
			e := productionEntry(`{"invocation_id":"inv-implement-run-1","run_id":"run-1","stage_id":"feedback-run-1"}`)
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
	got, err := decodeProductionRequest(productionEntry(
		`{"invocation_id":"inv-implement-run-1","run_id":"run-1","stage_id":"implement-run-1"}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.InvocationID != "inv-implement-run-1" || got.RunID != "run-1" || got.StageID != "implement-run-1" {
		t.Fatalf("decoded request = %#v", got)
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
			payload: `{"invocation_id":"inv-implement-run-owned",` +
				`"run_id":"run-owned","stage_id":"implement-run-owned"}`,
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
			payload: `{"invocation_id":"inv-implement-run-other",` +
				`"run_id":"run-other","stage_id":"implement-run-other"}`,
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
