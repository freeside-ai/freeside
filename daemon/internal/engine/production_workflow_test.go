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
	"github.com/freeside-ai/freeside/daemon/internal/signet"
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
		name       string
		runID      domain.RunID
		payload    string
		wantReason string
	}{
		{
			name:  "canonical",
			runID: "run-owned",
			payload: productionRequestJSON(`"invocation_id":"inv-implement-run-owned",` +
				`"run_id":"run-owned","stage_id":"implement-run-owned"`),
		},
		{
			name:       "malformed",
			runID:      "run-malformed",
			payload:    `{"run_id":"run-malformed"}`,
			wantReason: productionQuarantineUnreadable,
		},
		{
			name:  "retargeted",
			runID: "run-retargeted",
			payload: productionRequestJSON(`"invocation_id":"inv-implement-run-other",` +
				`"run_id":"run-other","stage_id":"implement-run-other"`),
			wantReason: productionQuarantineUnreadable,
		},
		{
			name:  "future version",
			runID: "run-future",
			payload: productionRequestJSON(`"version":"freeside.production-invocation/v3",` +
				`"invocation_id":"inv-implement-run-future",` +
				`"run_id":"run-future","stage_id":"implement-run-future"`),
			wantReason: productionQuarantineUnsupportedVersion,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e, st := newQuarantineEngine(t, ctx)
			seedProductionMarker(t, ctx, st, tc.runID, tc.payload)
			run := domain.Run{ID: tc.runID, ProjectID: "project-1"}

			owned, err := e.ownsProductionRun(ctx, run)
			if err != nil {
				t.Fatalf("ownership = %v, %v", owned, err)
			}
			if tc.wantReason == "" {
				if !owned {
					t.Fatal("canonical marker did not report ownership")
				}
				requireNoQuarantineItem(t, ctx, st, tc.runID)
				return
			}
			if owned {
				t.Fatal("unauthentic marker reported ownership")
			}
			item := requireQuarantineItem(t, ctx, st, tc.runID)
			if item.Reason != tc.wantReason {
				t.Fatalf("quarantine reason = %q, want %q", item.Reason, tc.wantReason)
			}
			if item.Type != domain.AttentionExecutionFailure || item.Status != domain.StatusOpen ||
				item.Subject.ID != domain.SubjectID(tc.runID) || item.ProjectID != run.ProjectID {
				t.Fatalf("quarantine item = %#v", item)
			}

			// A second pass (the restart case) converges on the one notice
			// instead of failing or duplicating it.
			owned, err = e.ownsProductionRun(ctx, run)
			if owned || err != nil {
				t.Fatalf("replayed ownership = %v, %v", owned, err)
			}
			requireQuarantineItem(t, ctx, st, tc.runID)
		})
	}
}

// TestProductionQuarantineLeavesADecidedNoticeAlone pins the property that
// makes the notice durable rather than merely repeated: a pass that finds the
// item already recorded writes nothing, so an operator's decision on it
// survives every later scan.
func TestProductionQuarantineLeavesADecidedNoticeAlone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e, st := newQuarantineEngine(t, ctx)
	runID := domain.RunID("run-decided")
	seedProductionMarker(t, ctx, st, runID, `{"run_id":"run-decided"}`)
	run := domain.Run{ID: runID, ProjectID: "project-1"}

	if owned, err := e.ownsProductionRun(ctx, run); owned || err != nil {
		t.Fatalf("ownership = %v, %v", owned, err)
	}
	decided := requireQuarantineItem(t, ctx, st, runID)
	decided.Status = domain.StatusResolved
	decided.ItemVersion = 2
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutAttentionItem(ctx, decided)
	}); err != nil {
		t.Fatalf("record operator decision: %v", err)
	}

	if owned, err := e.ownsProductionRun(ctx, run); owned || err != nil {
		t.Fatalf("ownership after decision = %v, %v", owned, err)
	}
	current := requireQuarantineItem(t, ctx, st, runID)
	if current.Status != domain.StatusResolved || current.ItemVersion != 2 {
		t.Fatalf("decided notice was rewritten: %#v", current)
	}
}

// TestProductionDispatchQuarantinesUnreadableMarkers covers the dispatch half
// of #424: a pending marker no daemon pass can decode leaves the loop instead
// of ending it, while a row this lane could not have filed stays loud because
// it names no run to quarantine.
func TestProductionDispatchQuarantinesUnreadableMarkers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	t.Run("attributable row is quarantined", func(t *testing.T) {
		e, st := newQuarantineEngine(t, ctx)
		runID := domain.RunID("run-pending-future")
		run := domain.Run{
			ID: runID, ProjectID: "project-1",
			SpecDigest:   domain.Digest("sha256:" + strings.Repeat("a", 64)),
			PolicyDigest: domain.Digest("sha256:" + strings.Repeat("c", 64)),
			Stages: []domain.Stage{{
				ID: productionStageID(runID), RunID: runID,
				Name: productionStageName, Attempts: []domain.Attempt{},
			}},
		}
		if err := st.Write(ctx, func(tx *store.WriteTx) error {
			return tx.PutRun(ctx, run)
		}); err != nil {
			t.Fatalf("seed run: %v", err)
		}
		seedProductionMarker(t, ctx, st, runID, productionRequestJSON(
			`"version":"freeside.production-invocation/v3",`+
				`"invocation_id":"inv-implement-run-pending-future",`+
				`"run_id":"run-pending-future","stage_id":"implement-run-pending-future"`,
		))

		started, err := e.dispatchPendingInvocations(ctx)
		if err != nil || started != 0 {
			t.Fatalf("dispatch = %d, %v", started, err)
		}
		item := requireQuarantineItem(t, ctx, st, runID)
		if item.Reason != productionQuarantineUnsupportedVersion {
			t.Fatalf("quarantine reason = %q", item.Reason)
		}
	})

	t.Run("unattributable row stays loud", func(t *testing.T) {
		e, st := newQuarantineEngine(t, ctx)
		if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
			_, _, err := tx.EnqueueOutbox(
				ctx, "not-a-production-key",
				KindProductionInvocationRequested, []byte(`{"run_id":"run-orphan"}`),
			)
			return err
		}); err != nil {
			t.Fatalf("seed marker: %v", err)
		}
		if _, err := e.dispatchPendingInvocations(ctx); err == nil {
			t.Fatal("unattributable production row dispatched without error")
		}
	})
}

// TestProductionQuarantineSkipsUnrelatedFailures keeps the classification
// narrow: only a marker reconstruction failure quarantines, so a store fault
// or any other cause still reaches the caller.
func TestProductionQuarantineSkipsUnrelatedFailures(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e, st := newQuarantineEngine(t, ctx)
	quarantined, err := quarantineProductionMarker(
		ctx, st, e.signet, "run-unrelated", "project-1", store.ErrNotFound)
	if quarantined || err != nil {
		t.Fatalf("quarantined unrelated cause = %v, %v", quarantined, err)
	}
	requireNoQuarantineItem(t, ctx, st, "run-unrelated")
}

func newQuarantineEngine(t *testing.T, ctx context.Context) (*Engine, *store.Store) {
	t.Helper()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return &Engine{store: st, signet: signet.NewService(st)}, st
}

func seedProductionMarker(
	t *testing.T, ctx context.Context, st *store.Store, runID domain.RunID, payload string,
) {
	t.Helper()
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, _, err := tx.EnqueueOutbox(
			ctx, string(productionInvocationID(runID)),
			KindProductionInvocationRequested, []byte(payload),
		)
		return err
	}); err != nil {
		t.Fatalf("seed marker: %v", err)
	}
}

func requireQuarantineItem(
	t *testing.T, ctx context.Context, st *store.Store, runID domain.RunID,
) domain.AttentionItem {
	t.Helper()
	var item domain.AttentionItem
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		item, err = tx.GetAttentionItemRecord(ctx, productionQuarantineItemID(runID))
		return err
	}); err != nil {
		t.Fatalf("read quarantine item for %q: %v", runID, err)
	}
	return item
}

func requireNoQuarantineItem(
	t *testing.T, ctx context.Context, st *store.Store, runID domain.RunID,
) {
	t.Helper()
	err := st.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.GetAttentionItemRecord(ctx, productionQuarantineItemID(runID))
		return err
	})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("quarantine item for %q = %v, want none", runID, err)
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

// TestProductionQuarantineRecursAfterRelease: a concluded notice is history,
// not a record of the current hold. A second quarantine after a repair must
// raise its own open notice, or the run is held behind nothing an operator
// would read as current.
func TestProductionQuarantineRecursAfterRelease(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e, st := newQuarantineEngine(t, ctx)
	runID := domain.RunID("run-recurring")
	run := domain.Run{ID: runID, ProjectID: "project-1"}
	base := productionMarkerQuarantinePrefix

	if err := recordProductionQuarantine(
		ctx, st, e.signet, base, runID, run.ProjectID, productionQuarantineUnreadable,
	); err != nil {
		t.Fatalf("first quarantine: %v", err)
	}
	if err := releaseProductionQuarantine(ctx, st, e.signet, base, runID); err != nil {
		t.Fatalf("release: %v", err)
	}
	if first := requireQuarantineItem(t, ctx, st, runID); first.Status != domain.StatusSuperseded {
		t.Fatalf("released notice = %#v", first)
	}

	if err := recordProductionQuarantine(
		ctx, st, e.signet, base, runID, run.ProjectID, productionQuarantineUnsupportedVersion,
	); err != nil {
		t.Fatalf("second quarantine: %v", err)
	}
	second, found, err := readProductionQuarantineItem(
		ctx, st, productionQuarantineOccurrenceID(base, runID, 2))
	if err != nil || !found {
		t.Fatalf("second occurrence = %v, %v", found, err)
	}
	if second.Status != domain.StatusOpen || second.Reason != productionQuarantineUnsupportedVersion {
		t.Fatalf("second occurrence = %#v", second)
	}

	// A repeated pass converges on that open occurrence instead of opening a third.
	if err := recordProductionQuarantine(
		ctx, st, e.signet, base, runID, run.ProjectID, productionQuarantineUnsupportedVersion,
	); err != nil {
		t.Fatalf("replayed quarantine: %v", err)
	}
	if _, found, err := readProductionQuarantineItem(
		ctx, st, productionQuarantineOccurrenceID(base, runID, 3)); err != nil || found {
		t.Fatalf("replayed pass opened a third occurrence: %v, %v", found, err)
	}
}

// TestProductionQuarantineReleaseConvergesOnADecision: an operator concluding
// the notice while a pass releases it is a race the pass must absorb. Turning
// it into an error would end the reconcile loop this path exists to keep
// running.
func TestProductionQuarantineReleaseConvergesOnADecision(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e, st := newQuarantineEngine(t, ctx)
	runID := domain.RunID("run-decided-race")
	base := productionMarkerQuarantinePrefix
	if err := recordProductionQuarantine(
		ctx, st, e.signet, base, runID, "project-1", productionQuarantineUnreadable,
	); err != nil {
		t.Fatalf("quarantine: %v", err)
	}
	stale := requireQuarantineItem(t, ctx, st, runID)

	// The operator's decision commits first.
	decided := stale
	decided.Status = domain.StatusResolved
	decided.ItemVersion = 2
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutAttentionItem(ctx, decided)
	}); err != nil {
		t.Fatalf("record decision: %v", err)
	}

	// The release pass is now holding the stale open copy.
	if err := releaseProductionQuarantine(ctx, st, e.signet, base, runID); err != nil {
		t.Fatalf("release under a concurrent decision: %v", err)
	}
	current := requireQuarantineItem(t, ctx, st, runID)
	if current.Status != domain.StatusResolved || current.ItemVersion != 2 {
		t.Fatalf("decision was overwritten: %#v", current)
	}
}

// TestProductionQuarantineRejectsADivergentConcurrentItem: a lost create race
// is only converged when what is stored really is this run's notice.
func TestProductionQuarantineRejectsADivergentConcurrentItem(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, st := newQuarantineEngine(t, ctx)
	runID := domain.RunID("run-divergent")
	foreign, err := productionQuarantineItem(
		productionQuarantineItemID(runID), "run-other", "project-other", "Some other notice.")
	if err != nil {
		t.Fatalf("construct foreign item: %v", err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutAttentionItem(ctx, foreign)
	}); err != nil {
		t.Fatalf("seed foreign item: %v", err)
	}

	err = confirmProductionQuarantineItem(
		ctx, st, productionQuarantineItemID(runID),
		domain.AttentionItem{
			ProjectID: "project-1", Type: domain.AttentionExecutionFailure,
			Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID)},
		},
		store.ErrStaleWrite,
	)
	if !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("divergent concurrent item accepted: %v", err)
	}
}

// TestProductionQuarantineRefreshesTheOpenNotice: the open notice must
// describe the hold that is current. A marker that changes class before the
// notice is concluded would otherwise leave an operator reading that an
// upgrade repairs a marker which has since become malformed instead.
func TestProductionQuarantineRefreshesTheOpenNotice(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e, st := newQuarantineEngine(t, ctx)
	runID := domain.RunID("run-refreshed")
	base := productionMarkerQuarantinePrefix
	if err := recordProductionQuarantine(
		ctx, st, e.signet, base, runID, "project-1", productionQuarantineUnsupportedVersion,
	); err != nil {
		t.Fatalf("first quarantine: %v", err)
	}
	if err := recordProductionQuarantine(
		ctx, st, e.signet, base, runID, "project-1", productionQuarantineUnreadable,
	); err != nil {
		t.Fatalf("reclassified quarantine: %v", err)
	}
	current := requireQuarantineItem(t, ctx, st, runID)
	if current.Reason != productionQuarantineUnreadable || current.ItemVersion != 2 ||
		current.Status != domain.StatusOpen {
		t.Fatalf("refreshed notice = %#v", current)
	}

	// An unchanged condition writes nothing.
	if err := recordProductionQuarantine(
		ctx, st, e.signet, base, runID, "project-1", productionQuarantineUnreadable,
	); err != nil {
		t.Fatalf("replayed quarantine: %v", err)
	}
	if replayed := requireQuarantineItem(t, ctx, st, runID); replayed.ItemVersion != 2 {
		t.Fatalf("replayed pass rewrote the notice: %#v", replayed)
	}
}

// TestProductionQuarantineRejectsADivergentOpenNotice: the replay path
// re-checks the stored row rather than trusting the identity it was found
// under.
func TestProductionQuarantineRejectsADivergentOpenNotice(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e, st := newQuarantineEngine(t, ctx)
	runID := domain.RunID("run-divergent-open")
	base := productionMarkerQuarantinePrefix
	foreign, err := productionQuarantineItem(
		productionQuarantineItemID(runID), "run-other", "project-1", "Some other notice.")
	if err != nil {
		t.Fatalf("construct foreign item: %v", err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutAttentionItem(ctx, foreign)
	}); err != nil {
		t.Fatalf("seed foreign item: %v", err)
	}
	err = recordProductionQuarantine(
		ctx, st, e.signet, base, runID, "project-1", productionQuarantineUnreadable)
	if !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("divergent open notice accepted: %v", err)
	}
}

// TestProductionQuarantineOccurrenceIDsNeverCollide: a run id is validated
// only as non-empty, so run "foo" and run "foo-2" are an ordinary pair. An
// occurrence appended after the run id would give run "foo"'s second notice
// run "foo-2"'s identity, and the mismatched subject that produces is an
// error on the path whose whole purpose is to keep the loop running.
func TestProductionQuarantineOccurrenceIDsNeverCollide(t *testing.T) {
	t.Parallel()
	seen := map[domain.ItemID]string{}
	for _, runID := range []domain.RunID{"foo", "foo-2", "2-foo", "1", "12"} {
		for occurrence := 1; occurrence <= 13; occurrence++ {
			id := productionQuarantineOccurrenceID(productionMarkerQuarantinePrefix, runID, occurrence)
			key := fmt.Sprintf("%s#%d", runID, occurrence)
			if prior, dup := seen[id]; dup {
				t.Fatalf("id %q is shared by %s and %s", id, prior, key)
			}
			seen[id] = key
			if task := productionQuarantineOccurrenceID(
				productionTaskQuarantinePrefix, runID, occurrence); task == id {
				t.Fatalf("marker and task notices share id %q", id)
			}
		}
	}
}

// TestProductionQuarantineSurvivesADeepNoticeHistory: a run repaired and
// re-quarantined many times keeps getting a current open notice. A bounded
// history would have to choose between erroring, which ends the loop, and
// holding the run behind nothing an operator would read as current.
func TestProductionQuarantineSurvivesADeepNoticeHistory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e, st := newQuarantineEngine(t, ctx)
	runID := domain.RunID("run-deep-history")
	for cycle := 1; cycle <= 40; cycle++ {
		if err := recordProductionQuarantine(
			ctx, st, e.signet, productionMarkerQuarantinePrefix,
			runID, "project-1", productionQuarantineUnreadable,
		); err != nil {
			t.Fatalf("quarantine cycle %d: %v", cycle, err)
		}
		current, found, err := readProductionQuarantineItem(
			ctx, st, productionQuarantineOccurrenceID(
				productionMarkerQuarantinePrefix, runID, cycle))
		if err != nil || !found || current.Status != domain.StatusOpen {
			t.Fatalf("cycle %d notice = %v, %v, %v", cycle, found, current.Status, err)
		}
		if err := releaseProductionQuarantine(
			ctx, st, e.signet, productionMarkerQuarantinePrefix, runID,
		); err != nil {
			t.Fatalf("release cycle %d: %v", cycle, err)
		}
	}
}

// TestProductionQuarantineReleaseLeavesForeignItemsAlone: the release path
// concludes only this hold's own notice. An unrelated item under the same
// predictable id is left untouched, and left alone rather than turned into an
// error, since failing to retire a notice this lane does not own is harmless
// while erroring would end the reconcile loop.
func TestProductionQuarantineReleaseLeavesForeignItemsAlone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e, st := newQuarantineEngine(t, ctx)
	for _, tc := range []struct {
		name   string
		runID  domain.RunID
		prefix string
		reason string
	}{
		{
			"unrelated reason", "run-foreign-reason", productionMarkerQuarantinePrefix,
			"An operator item that is not a quarantine.",
		},
		{
			"other row class", "run-foreign-class", productionMarkerQuarantinePrefix,
			productionQuarantineUnreadableTask,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runID := tc.runID
			id := productionQuarantineOccurrenceID(tc.prefix, runID, 1)
			foreign, err := productionQuarantineItem(id, runID, "project-1", tc.reason)
			if err != nil {
				t.Fatalf("construct item: %v", err)
			}
			if err := st.Write(ctx, func(tx *store.WriteTx) error {
				return tx.PutAttentionItem(ctx, foreign)
			}); err != nil {
				t.Fatalf("seed item: %v", err)
			}
			if err := releaseProductionQuarantine(ctx, st, e.signet, tc.prefix, runID); err != nil {
				t.Fatalf("release over a foreign item: %v", err)
			}
			current, found, err := readProductionQuarantineItem(ctx, st, id)
			if err != nil || !found {
				t.Fatalf("read back: %v, %v", found, err)
			}
			if current.Status != domain.StatusOpen || current.ItemVersion != 1 {
				t.Fatalf("foreign item was concluded: %#v", current)
			}
		})
	}
}

// TestProductionQuarantineRepairsADriftedNotice: the stored row is a
// reconstruction, so every operator-facing field is re-derived from the
// current hold. A row carrying this run's bindings and reason but a drifted
// priority, action set, or interruption class is repaired, not accepted: a
// subset check can only authenticate the fields someone thought to list.
func TestProductionQuarantineRepairsADriftedNotice(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		runID domain.RunID
		drift func(*domain.AttentionItem)
	}{
		{"priority", "run-drift-priority", func(i *domain.AttentionItem) {
			i.Priority = domain.PriorityLow
		}},
		{"requested decision", "run-drift-actions", func(i *domain.AttentionItem) {
			i.RequestedDecision = []domain.Action{domain.ActionDiscuss}
		}},
		{"interruption class", "run-drift-class", func(i *domain.AttentionItem) {
			i.InterruptionClass = domain.InterruptionPlannedGate
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, st := newQuarantineEngine(t, ctx)
			drifted, err := productionQuarantineItem(
				productionQuarantineItemID(tc.runID), tc.runID, "project-1",
				productionQuarantineUnreadable)
			if err != nil {
				t.Fatalf("construct item: %v", err)
			}
			tc.drift(&drifted)
			if err := st.Write(ctx, func(tx *store.WriteTx) error {
				return tx.PutAttentionItem(ctx, drifted)
			}); err != nil {
				t.Fatalf("seed drifted item: %v", err)
			}

			if err := recordProductionQuarantine(
				ctx, st, e.signet, productionMarkerQuarantinePrefix,
				tc.runID, "project-1", productionQuarantineUnreadable,
			); err != nil {
				t.Fatalf("record over a drifted notice: %v", err)
			}
			canonical, err := productionQuarantineItem(
				productionQuarantineItemID(tc.runID), tc.runID, "project-1",
				productionQuarantineUnreadable)
			if err != nil {
				t.Fatalf("construct canonical item: %v", err)
			}
			current := requireQuarantineItem(t, ctx, st, tc.runID)
			if !sameProductionQuarantineNotice(current, canonical) {
				t.Fatalf("drifted notice was accepted: %#v", current)
			}
			if current.Status != domain.StatusOpen || current.ItemVersion != 2 {
				t.Fatalf("repaired notice lifecycle = %#v", current)
			}
		})
	}
}

// TestProductionMarkerVersionClassifiedBeforeStrictDecode: a newer version
// normally adds a field, and the strict decode would reject that before the
// version was read, reporting the downgrade this lane exists to survive as a
// malformed marker. The classifier runs first, and only ever changes which
// refusal an operator reads.
func TestProductionMarkerVersionClassifiedBeforeStrictDecode(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		payload string
		want    string
	}{
		{
			name: "future version adding a field",
			payload: `{"version":"freeside.production-invocation/v3",` +
				`"invocation_id":"inv-implement-run-1","run_id":"run-1",` +
				`"stage_id":"implement-run-1","review":{"source":"codex"}}`,
			want: "freeside.production-invocation/v3",
		},
		{
			name: "future version renaming a field",
			payload: `{"version":"freeside.production-invocation/v4",` +
				`"invocation":"inv-implement-run-1","run_id":"run-1"}`,
			want: "freeside.production-invocation/v4",
		},
		{"released version", productionRequestJSON(
			`"version":"freeside.production-invocation/v2","invocation_id":"inv-implement-run-1",` +
				`"run_id":"run-1","stage_id":"implement-run-1"`), ""},
		{"unversioned preview", productionRequestJSON(
			`"invocation_id":"inv-implement-run-1","run_id":"run-1","stage_id":"implement-run-1"`), ""},
		{"released legacy v1", `{"invocation_id":"inv-implement-run-1","run_id":"run-1","stage_id":"implement-run-1"}`, ""},
		{"obsolete namespace member", `{"version":"freeside.production-invocation/v1","run_id":"run-1"}`, ""},
		{"corrupt version", `{"version":"garbage","run_id":"run-1"}`, ""},
		{"foreign namespace", `{"version":"other.product/v9","run_id":"run-1"}`, ""},
		{"non-canonical number", `{"version":"freeside.production-invocation/v007","run_id":"run-1"}`, ""},
		{"suffixed version", `{"version":"freeside.production-invocation/v3-beta","run_id":"run-1"}`, ""},
		{"malformed json", `{"version":`, ""},
		{"non-string version", `{"version":9,"run_id":"run-1"}`, ""},
		{"empty version", `{"version":"","run_id":"run-1"}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := unsupportedProductionMarkerVersion([]byte(tc.payload)); got != tc.want {
				t.Fatalf("classified version = %q, want %q", got, tc.want)
			}
			_, err := decodeProductionRequest(productionEntry(tc.payload))
			if err == nil {
				return
			}
			if tc.want == "" && errors.Is(err, errProductionMarkerUnsupportedVersion) {
				t.Fatalf("released or malformed payload classified as a future version: %v", err)
			}
			if tc.want != "" && !errors.Is(err, errProductionMarkerUnsupportedVersion) {
				t.Fatalf("future version classified as unreadable: %v", err)
			}
		})
	}
}

// TestProductionMarkerVersionConstantsCompose pins the namespace, the release
// number, and the released version string to one another: the classifier
// decides the downgrade diagnosis from the first two, so a drift between them
// would silently change which markers this binary claims to implement.
func TestProductionMarkerVersionConstantsCompose(t *testing.T) {
	t.Parallel()
	composed := fmt.Sprintf("%s%d",
		productionInvocationVersionNamespace, productionInvocationRequestVersionNumber)
	if composed != productionInvocationRequestVersion {
		t.Fatalf("composed version = %q, want %q", composed, productionInvocationRequestVersion)
	}
}

// TestProductionQuarantineDiagnosesADowngrade ties the classification to what
// the operator actually reads: the notice for a newer marker names the
// upgrade that repairs the hold.
func TestProductionQuarantineDiagnosesADowngrade(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e, st := newQuarantineEngine(t, ctx)
	runID := domain.RunID("run-downgraded")
	seedProductionMarker(t, ctx, st, runID, `{"version":"freeside.production-invocation/v3",`+
		`"invocation_id":"inv-implement-run-downgraded","run_id":"run-downgraded",`+
		`"stage_id":"implement-run-downgraded","review":{"source":"codex"}}`)

	owned, err := e.ownsProductionRun(ctx, domain.Run{ID: runID, ProjectID: "project-1"})
	if owned || err != nil {
		t.Fatalf("ownership = %v, %v", owned, err)
	}
	if item := requireQuarantineItem(t, ctx, st, runID); item.Reason != productionQuarantineUnsupportedVersion {
		t.Fatalf("quarantine reason = %q", item.Reason)
	}
}
