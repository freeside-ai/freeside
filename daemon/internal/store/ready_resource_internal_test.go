package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publicationrecord"
)

// TestReadyItemPRReferenceAnchorRegatesWithoutProductionBinding proves the
// fake-publication and development-fixture path: its creating Put stamps an
// immutable coordinate anchor even though no production binding exists, and
// both synchronized read shapes reject a later body-only retarget.
func TestReadyItemPRReferenceAnchorRegatesWithoutProductionBinding(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTemplateStoreAt(t, filepath.Join(t.TempDir(), "store.db"), Options{})
	runID := domain.RunID("run-fake-ready-anchor")
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "fake-ready-anchor", ProjectID: "project-1",
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
		Type:    domain.AttentionReadyForFinalReview, Priority: domain.PriorityNormal,
		Reason: "fake publication ready", RequestedDecision: []domain.Action{domain.ActionOpenPR},
		PRHeadSHA: "cafed00d", PRReference: &domain.PRReference{Repo: "owner/repo", Number: 123},
		ItemVersion: 1, InterruptionClass: domain.InterruptionPlannedGate, Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *WriteTx) error { return putTestAttentionItem(ctx, tx, &item) }); err != nil {
		t.Fatal(err)
	}
	if err := st.Read(ctx, func(tx *ReadTx) error {
		_, err := tx.GetReadyItemPRBinding(ctx, item.ID)
		return err
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("production binding error = %v, want ErrNotFound", err)
	}
	itemBody, err := encode(item)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		sql  string
	}{
		{"repo", `UPDATE attention_items SET body = json_set(body, '$.pr_reference.repo', 'other/repo') WHERE id = ?`},
		{"pr number", `UPDATE attention_items SET body = json_set(body, '$.pr_reference.number', 999) WHERE id = ?`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := st.db.ExecContext(ctx, tc.sql, item.ID); err != nil {
				t.Fatal(err)
			}
			for name, read := range map[string]func(*ReadTx) error{
				"get": func(tx *ReadTx) error {
					_, _, err := tx.GetAttentionItemSnapshot(ctx, item.ID)
					return err
				},
				"list": func(tx *ReadTx) error {
					_, err := tx.ListAttentionItems(ctx)
					return err
				},
			} {
				t.Run(name, func(t *testing.T) {
					err := st.Read(ctx, read)
					if !errors.Is(err, errRowInconsistent) {
						t.Fatalf("retargeted read error = %v, want errRowInconsistent", err)
					}
				})
			}
			if _, err := st.db.ExecContext(ctx,
				`UPDATE attention_items SET body = ? WHERE id = ?`, itemBody, item.ID); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestHeldItemPRReferenceAnchorIsWrittenOnceAndRegated covers a hold that
// outlives its run's publication: the version that first carries the pull
// request stamps the anchor, no later version can name another one, and both
// synchronized read shapes refuse a body that changed, dropped, or invented a
// reference the anchor does not back.
func TestHeldItemPRReferenceAnchorIsWrittenOnceAndRegated(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTemplateStoreAt(t, filepath.Join(t.TempDir(), "store.db"), Options{})
	runID := domain.RunID("run-held-anchor")
	hold := func(id domain.ItemID, version int, reference *domain.PRReference) domain.AttentionItem {
		t.Helper()
		item, err := domain.NewAttentionItem(domain.AttentionItemInput{
			ID: id, ProjectID: "project-1",
			Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
			Type:    domain.AttentionPublishBlocked, Priority: domain.PriorityHigh,
			Reason:            fmt.Sprintf("publication is held (version %d)", version),
			RequestedDecision: []domain.Action{domain.ActionInspectTrustFailure},
			PRHeadSHA:         "cafed00d", PRReference: reference,
			ItemVersion: version, InterruptionClass: domain.InterruptionExceptional, Status: domain.StatusOpen,
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return item
	}
	put := func(item domain.AttentionItem) error {
		return st.Write(ctx, func(tx *WriteTx) error { return putTestAttentionItem(ctx, tx, &item) })
	}
	anchors := func(id domain.ItemID) int {
		t.Helper()
		var count int
		if err := st.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM attention_item_pr_references WHERE item_id = ?`, id).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	reads := map[string]func(*ReadTx) error{
		"get": func(tx *ReadTx) error {
			_, _, err := tx.GetAttentionItemSnapshot(ctx, "held-anchor")
			return err
		},
		"list": func(tx *ReadTx) error {
			_, err := tx.ListAttentionItems(ctx)
			return err
		},
	}
	assertReads := func(t *testing.T, want error) {
		t.Helper()
		for name, read := range reads {
			if err := st.Read(ctx, read); !errors.Is(err, want) {
				t.Fatalf("%s error = %v, want %v", name, err, want)
			}
		}
	}

	reference := &domain.PRReference{Repo: "owner/repo", Number: 123}
	if err := put(hold("held-anchor", 1, nil)); err != nil {
		t.Fatal(err)
	}
	if got := anchors("held-anchor"); got != 0 {
		t.Fatalf("anchors before publication = %d, want 0", got)
	}
	assertReads(t, nil)

	if _, err := st.db.ExecContext(ctx, `UPDATE attention_items
		SET body = json_set(body, '$.pr_reference', json('{"repo":"owner/repo","number":123}'))
		WHERE id = 'held-anchor'`); err != nil {
		t.Fatal(err)
	}
	t.Run("body reference without an anchor", func(t *testing.T) { assertReads(t, errRowInconsistent) })
	if _, err := st.db.ExecContext(ctx, `UPDATE attention_items
		SET body = json_set(body, '$.pr_reference', json('null')) WHERE id = 'held-anchor'`); err != nil {
		t.Fatal(err)
	}

	if err := put(hold("held-anchor", 2, reference)); err != nil {
		t.Fatalf("attach reference: %v", err)
	}
	if got := anchors("held-anchor"); got != 1 {
		t.Fatalf("anchors after the attaching version = %d, want 1", got)
	}
	assertReads(t, nil)
	if err := put(hold("held-anchor", 3, reference)); err != nil {
		t.Fatalf("later version restating the reference: %v", err)
	}
	if err := put(hold("held-anchor", 4, &domain.PRReference{Repo: "owner/repo", Number: 124})); !errors.Is(err, ErrImmutableConflict) {
		t.Fatalf("retargeting version error = %v, want ErrImmutableConflict", err)
	}
	if err := put(hold("held-anchor", 4, nil)); !errors.Is(err, ErrImmutableConflict) {
		t.Fatalf("reference-dropping version error = %v, want ErrImmutableConflict", err)
	}

	var body string
	if err := st.db.QueryRowContext(ctx,
		`SELECT body FROM attention_items WHERE id = 'held-anchor'`).Scan(&body); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, sql string }{
		{"repo", `UPDATE attention_items SET body = json_set(body, '$.pr_reference.repo', 'other/repo') WHERE id = 'held-anchor'`},
		{"pr number", `UPDATE attention_items SET body = json_set(body, '$.pr_reference.number', 999) WHERE id = 'held-anchor'`},
		{"removed", `UPDATE attention_items SET body = json_set(body, '$.pr_reference', json('null')) WHERE id = 'held-anchor'`},
	} {
		t.Run("body "+tc.name, func(t *testing.T) {
			if _, err := st.db.ExecContext(ctx, tc.sql); err != nil {
				t.Fatal(err)
			}
			assertReads(t, errRowInconsistent)
			if _, err := st.db.ExecContext(ctx,
				`UPDATE attention_items SET body = ? WHERE id = 'held-anchor'`, body); err != nil {
				t.Fatal(err)
			}
		})
	}
	assertReads(t, nil)

	// A hold created after publication is anchored by its creating Put.
	if err := put(hold("held-anchor-created-published", 1, reference)); err != nil {
		t.Fatal(err)
	}
	if got := anchors("held-anchor-created-published"); got != 1 {
		t.Fatalf("anchors for a hold created with a reference = %d, want 1", got)
	}
}

// TestReadyItemBindingRegatesEveryResourceCoordinate corrupts the binding as
// though a reconstructed row or internal caller had retargeted it. Matching
// extracted columns are changed with the JSON, so only the immutable
// admission/export/outcome anchors can reject each forged coordinate.
func TestReadyItemBindingRegatesEveryResourceCoordinate(t *testing.T) {
	t.Parallel()
	for _, branch := range []string{"freeside/publish/" + strings.Repeat("a", 16), "feat/meaningful-task"} {
		t.Run(branch, func(t *testing.T) { testReadyItemBranchBinding(t, branch) })
	}
}

func testReadyItemBranchBinding(t *testing.T, branch string) {
	t.Helper()
	ctx := context.Background()
	fixture := seedReadyItemBinding(t, branch, "deadbeef", "cafed00d")
	st, item, admission, export := fixture.st, fixture.item, fixture.admission, fixture.export
	identity, binding := fixture.binding.PublicationIdentity, fixture.binding
	intentKey, intentPayload, payload := fixture.intentKey, fixture.intentPayload, fixture.outcomePayload

	assertAttentionReadRejected := func(t *testing.T) {
		t.Helper()
		for _, read := range []struct {
			name string
			fn   func(*ReadTx) error
		}{
			{"get", func(tx *ReadTx) error {
				_, _, err := tx.GetAttentionItemSnapshot(ctx, item.ID)
				return err
			}},
			{"list", func(tx *ReadTx) error {
				_, err := tx.ListAttentionItems(ctx)
				return err
			}},
		} {
			t.Run(read.name, func(t *testing.T) {
				err := st.Read(ctx, read.fn)
				if !errors.Is(err, errRowInconsistent) {
					t.Fatalf("retargeted attention item error = %v, want errRowInconsistent", err)
				}
			})
		}
	}
	t.Run("outcome branch binding", func(t *testing.T) {
		if _, err := st.db.ExecContext(ctx, `UPDATE inbox SET payload = CAST(json_set(payload, '$.branch', 'feat/other-task') AS BLOB) WHERE idempotency_key = ?`, "publish.outcome/"+string(identity)); err != nil {
			t.Fatal(err)
		}
		assertAttentionReadRejected(t)
		if _, err := st.db.ExecContext(ctx, `UPDATE inbox SET payload = ? WHERE idempotency_key = ?`, payload, "publish.outcome/"+string(identity)); err != nil {
			t.Fatal(err)
		}
	})
	itemBody, err := encode(item)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		sql  string
	}{
		{"repo", `UPDATE attention_items SET body = json_set(body, '$.pr_reference.repo', 'other/repo') WHERE id = ?`},
		{"pr number", `UPDATE attention_items SET body = json_set(body, '$.pr_reference.number', 451) WHERE id = ?`},
	} {
		t.Run("attention item "+tc.name, func(t *testing.T) {
			if _, err := st.db.ExecContext(ctx, tc.sql, item.ID); err != nil {
				t.Fatal(err)
			}
			assertAttentionReadRejected(t)
			if _, err := st.db.ExecContext(ctx, `UPDATE attention_items SET body = ? WHERE id = ?`, itemBody, item.ID); err != nil {
				t.Fatal(err)
			}
		})
	}
	originalBody, err := encode(binding)
	if err != nil {
		t.Fatal(err)
	}
	foreignIdentity := domain.Digest("sha256:" + strings.Repeat("b", 64))
	foreignPublicationInvocationID := domain.InvocationID("publish-production-run-ready-foreign")
	foreignIntent, err := json.Marshal(readyPublicationIntent{
		FormatVersion: publicationrecord.IntentFormatCurrent,
		Identity:      foreignIdentity, InvocationID: foreignPublicationInvocationID,
		Branch: publicationrecord.BranchName(foreignIdentity),
		Repo:   admission.Base.Repo, BaseRef: admission.Base.BaseRef,
		SourceHeadSHA:         export.HeadSHA,
		AuthorizationID:       domain.Digest("sha256:" + strings.Repeat("d", 64)),
		ProducingInvocationID: "inv-ready-foreign", ReservationRunID: "run-ready-foreign",
	})
	if err != nil {
		t.Fatal(err)
	}
	foreignOutcome, err := json.Marshal(readyPublicationOutcome{
		Identity: foreignIdentity, Repo: admission.Base.Repo, BaseRef: admission.Base.BaseRef,
		HeadSHA: export.HeadSHA, Branch: "freeside/publish/" + strings.Repeat("b", 16),
		PRNumber: 451, EvidenceEligible: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.WriteInternal(ctx, func(tx *InternalTx) error {
		intentKey := "publish/" + string(foreignPublicationInvocationID) + "/" + readyPublicationIntentKind
		if _, _, err := tx.EnqueueOutbox(ctx, intentKey, readyPublicationIntentKind, foreignIntent); err != nil {
			return err
		}
		if err := tx.MarkOutboxDispatched(ctx, intentKey); err != nil {
			return err
		}
		_, _, err := tx.RecordInbox(ctx, "publish.outcome/"+string(foreignIdentity), "publish.outcome", foreignOutcome)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		sql  string
		args []any
	}{
		{name: "repo", sql: `UPDATE ready_item_pr_bindings SET body = json_set(body, '$.repo', 'other/repo') WHERE item_id = ?`},
		{name: "repository id", sql: `UPDATE ready_item_pr_bindings SET repository_id = 515151, body = json_set(body, '$.repository_id', 515151) WHERE item_id = ?`},
		{name: "pr number", sql: `UPDATE ready_item_pr_bindings SET pr_number = 451, body = json_set(body, '$.pr_number', 451) WHERE item_id = ?`},
		{name: "base ref", sql: `UPDATE ready_item_pr_bindings SET body = json_set(body, '$.base_ref', 'release') WHERE item_id = ?`},
		{name: "publication identity", sql: `UPDATE ready_item_pr_bindings SET publication_identity = ?, body = json_set(body, '$.publication_identity', ?) WHERE item_id = ?`, args: []any{"sha256:" + strings.Repeat("b", 64), "sha256:" + strings.Repeat("b", 64)}},
		{name: "cross-run publication", sql: `UPDATE ready_item_pr_bindings
			SET publication_invocation_id = ?, publication_identity = ?, pr_number = ?,
			body = json_set(body, '$.publication_invocation_id', ?, '$.publication_identity', ?, '$.pr_number', ?)
			WHERE item_id = ?`, args: []any{
			foreignPublicationInvocationID, foreignIdentity, 451,
			foreignPublicationInvocationID, foreignIdentity, 451,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append(tc.args, item.ID)
			if _, err := st.db.ExecContext(ctx, tc.sql, args...); err != nil {
				t.Fatal(err)
			}
			err := st.Read(ctx, func(tx *ReadTx) error {
				_, err := tx.GetReadyItemPRBinding(ctx, item.ID)
				return err
			})
			if err == nil {
				t.Fatalf("retargeted binding read error = %v", err)
			}
			if _, err := st.db.ExecContext(ctx, `UPDATE ready_item_pr_bindings
				SET repository_id = ?, pr_number = ?, publication_invocation_id = ?,
				publication_identity = ?, body = ? WHERE item_id = ?`,
				binding.RepositoryID, binding.PRNumber, binding.PublicationInvocationID,
				binding.PublicationIdentity, originalBody, item.ID); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, tc := range []struct {
		name string
		sql  string
		args []any
	}{
		{name: "kind", sql: `UPDATE outbox SET kind = 'other.kind' WHERE idempotency_key = ?`},
		{name: "dispatch state", sql: `UPDATE outbox SET status = 'pending' WHERE idempotency_key = ?`},
		{name: "identity", sql: `UPDATE outbox SET payload = CAST(json_set(payload, '$.identity', ?) AS BLOB) WHERE idempotency_key = ?`, args: []any{foreignIdentity}},
		{name: "publication invocation", sql: `UPDATE outbox SET payload = CAST(json_set(payload, '$.invocation_id', ?) AS BLOB) WHERE idempotency_key = ?`, args: []any{foreignPublicationInvocationID}},
		{name: "repo", sql: `UPDATE outbox SET payload = CAST(json_set(payload, '$.repo', 'other/repo') AS BLOB) WHERE idempotency_key = ?`},
		{name: "base ref", sql: `UPDATE outbox SET payload = CAST(json_set(payload, '$.base_ref', 'release') AS BLOB) WHERE idempotency_key = ?`},
		{name: "head", sql: `UPDATE outbox SET payload = CAST(json_set(payload, '$.source_head_sha', 'feedface') AS BLOB) WHERE idempotency_key = ?`},
		{name: "authorization", sql: `UPDATE outbox SET payload = CAST(json_set(payload, '$.authorization_id', 'bad') AS BLOB) WHERE idempotency_key = ?`},
		{name: "producing invocation", sql: `UPDATE outbox SET payload = CAST(json_set(payload, '$.producing_invocation_id', 'inv-ready-foreign') AS BLOB) WHERE idempotency_key = ?`},
		{name: "reservation run", sql: `UPDATE outbox SET payload = CAST(json_set(payload, '$.reservation_run_id', 'run-ready-foreign') AS BLOB) WHERE idempotency_key = ?`},
		{name: "unknown field", sql: `UPDATE outbox SET payload = CAST(json_set(payload, '$.unexpected', true) AS BLOB) WHERE idempotency_key = ?`},
	} {
		t.Run("publication intent "+tc.name, func(t *testing.T) {
			args := append(tc.args, intentKey)
			if _, err := st.db.ExecContext(ctx, tc.sql, args...); err != nil {
				t.Fatal(err)
			}
			err := st.Read(ctx, func(tx *ReadTx) error {
				_, err := tx.GetReadyItemPRBinding(ctx, item.ID)
				return err
			})
			if err == nil {
				t.Fatal("corrupt publication intent was accepted")
			}
			if _, err := st.db.ExecContext(ctx, `UPDATE outbox
				SET kind = ?, status = ?, payload = ? WHERE idempotency_key = ?`,
				readyPublicationIntentKind, outboxStatusDispatched, intentPayload, intentKey); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// readyBindingFixture is one published ready item with every record its
// binding gate re-reads.
type readyBindingFixture struct {
	st             *Store
	run            domain.Run
	item           domain.AttentionItem
	admission      domain.ExecutionAdmission
	export         domain.ExecutionExport
	binding        domain.ReadyItemPRBinding
	intentKey      string
	intentPayload  []byte
	outcomePayload []byte
}

// readyAnchorSpecDigest is a well-formed content address, so an artifact
// that must restate the run's spec digest (a finding adjudication) can be
// written against this fixture.
var readyAnchorSpecDigest = domain.Digest("sha256:" + strings.Repeat("5", 64))

func seedReadyItemBinding(t *testing.T, branch, baseSHA, headSHA string) readyBindingFixture {
	t.Helper()
	return seedReadyItemBindingDeclaring(t, branch, baseSHA, headSHA, "")
}

// seedReadyItemBindingDeclaring is seedReadyItemBinding for a run whose
// policy declares paths, a comma-separated list; "" declares none.
func seedReadyItemBindingDeclaring(t *testing.T, branch, baseSHA, headSHA, paths string) readyBindingFixture {
	t.Helper()
	ctx := context.Background()
	st := openTemplateStoreAt(t, filepath.Join(t.TempDir(), "store.db"), Options{AdmissionFloors: map[domain.OperatingMode]domain.CapabilitySnapshot{
		domain.ModeAttendedDev: domain.NewCapabilitySnapshot(domain.CapPostExitExport),
	}})
	runID := domain.RunID("run-ready-anchor")
	invocationID := domain.InvocationID("inv-ready-anchor")
	stageID := domain.StageID("stage-ready-anchor")
	attemptID := domain.AttemptID("attempt-ready-anchor")
	keys := []domain.PolicyKey{{
		Key: "driver", Value: "claude", Provenance: domain.KeyProvenance{
			Source: domain.ProvenanceOverride,
			Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
	}}
	if paths != "" {
		keys = append(keys, domain.PolicyKey{Key: "paths", Value: paths, Provenance: keys[0].Provenance})
	}
	policy, err := domain.NewResolvedPolicy(runID, keys)
	if err != nil {
		t.Fatal(err)
	}
	run := domain.Run{
		ID: runID, ProjectID: "project-1", SpecDigest: readyAnchorSpecDigest, PolicyDigest: policy.Digest,
		Stages: []domain.Stage{{
			ID: stageID, RunID: runID, Name: "implementation",
			Attempts: []domain.Attempt{{ID: attemptID, StageID: stageID, Number: 1, InvocationID: invocationID}},
		}},
	}
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: domain.ProductionReadyItemID(runID), ProjectID: run.ProjectID,
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
		Type:    domain.AttentionReadyForFinalReview, Priority: domain.PriorityNormal,
		Reason: "published", RequestedDecision: []domain.Action{domain.ActionOpenPR},
		PRHeadSHA: headSHA, PRReference: &domain.PRReference{Repo: "owner/repo", Number: 450},
		ItemVersion:       1,
		InterruptionClass: domain.InterruptionPlannedGate, Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		if err := tx.PutResolvedPolicy(ctx, policy); err != nil {
			return err
		}
		return putTestAttentionItem(ctx, tx, &item)
	}); err != nil {
		t.Fatal(err)
	}
	admittedAt := time.Date(2026, 8, 2, 11, 0, 0, 0, time.UTC)
	admission, err := domain.NewExecutionAdmission(domain.ExecutionAdmissionInput{
		InvocationID: invocationID, RunID: runID, StageID: stageID, AttemptID: attemptID,
		Backend: "ready-anchor-test", Capabilities: domain.NewCapabilitySnapshot(domain.CapPostExitExport),
		OperatingMode: domain.ModeAttendedDev, CredentialMode: domain.CredentialSubscriptionContained,
		EgressProfile: domain.EgressCleanVerification,
		ImageRef:      domain.ImageRef("ghcr.io/freeside-ai/agent@sha256:" + strings.Repeat("a", 64)),
		SpecDigest:    run.SpecDigest, PolicyDigest: run.PolicyDigest, InputDigest: "sha256:input",
		Base:      domain.BaseRevision{Repo: "owner/repo", RepositoryID: 424242, BaseRef: "main", BaseSHA: baseSHA},
		Workspace: "workspace", AdmittedAt: admittedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	export, err := domain.NewExecutionExport(domain.ExecutionExportInput{
		InvocationID: invocationID, AdmissionID: admission.ID,
		ObservedBaseSHA: admission.Base.BaseSHA, HeadSHA: item.PRHeadSHA,
		ManifestDigest: "sha256:manifest", RecordedAt: admittedAt.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	identity := domain.Digest("sha256:" + strings.Repeat("a", 64))
	publicationInvocationID := domain.InvocationID("publish-production-" + string(runID))
	intentPayload, err := json.Marshal(readyPublicationIntent{
		FormatVersion: publicationrecord.IntentFormatCurrent,
		Identity:      identity, InvocationID: publicationInvocationID,
		Branch: branch,
		Repo:   admission.Base.Repo, BaseRef: admission.Base.BaseRef,
		SourceHeadSHA:         export.HeadSHA,
		AuthorizationID:       domain.Digest("sha256:" + strings.Repeat("c", 64)),
		ProducingInvocationID: invocationID, ReservationRunID: runID,
	})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(readyPublicationOutcome{
		Identity: identity, Repo: admission.Base.Repo, BaseRef: admission.Base.BaseRef,
		HeadSHA: export.HeadSHA, Branch: branch,
		PRNumber: 450, EvidenceEligible: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	binding := domain.ReadyItemPRBinding{
		ItemID: item.ID, RunID: runID, ProducingInvocationID: invocationID,
		PublicationInvocationID: publicationInvocationID,
		PublicationIdentity:     identity, Repo: admission.Base.Repo,
		RepositoryID: admission.Base.RepositoryID, PRNumber: 450,
		BaseRef: admission.Base.BaseRef, HeadSHA: export.HeadSHA,
		RecordedAt: admittedAt.Add(2 * time.Minute),
	}
	intentKey := "publish/" + string(publicationInvocationID) + "/" + readyPublicationIntentKind
	if err := st.Write(ctx, func(tx *WriteTx) error {
		if err := tx.RecordExecutionAdmission(ctx, admission); err != nil {
			return err
		}
		if err := tx.RecordExecutionExport(ctx, export); err != nil {
			return err
		}
		if _, _, err := tx.EnqueueOutbox(ctx, intentKey, readyPublicationIntentKind, intentPayload); err != nil {
			return err
		}
		if err := tx.MarkOutboxDispatched(ctx, intentKey); err != nil {
			return err
		}
		if _, _, err := tx.RecordInbox(ctx, "publish.outcome/"+string(identity), "publish.outcome", payload); err != nil {
			return err
		}
		return tx.RecordReadyItemPRBinding(ctx, binding)
	}); err != nil {
		t.Fatal(err)
	}
	return readyBindingFixture{
		st: st, run: run, item: item, admission: admission, export: export, binding: binding,
		intentKey: intentKey, intentPayload: intentPayload, outcomePayload: payload,
	}
}
