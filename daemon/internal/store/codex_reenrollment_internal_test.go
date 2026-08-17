package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/migrations"
)

func TestCodexReenrollmentEnumRegistration(t *testing.T) {
	outcomes := []CodexReenrollmentOutcome{
		CodexReenrollmentFailed,
		CodexReenrollmentVerified,
	}
	if !slices.Equal(AllCodexReenrollmentOutcomes, outcomes) {
		t.Fatalf("outcomes = %v, want %v", AllCodexReenrollmentOutcomes, outcomes)
	}
	for _, outcome := range AllCodexReenrollmentOutcomes {
		if !outcome.valid() {
			t.Errorf("registered outcome %q is invalid", outcome)
		}
	}
	if CodexReenrollmentOutcome("").valid() {
		t.Error("empty outcome is valid")
	}

	failureClasses := []CodexReenrollmentFailureClass{
		CodexReenrollmentAuthStoreReplacementFailed,
		CodexReenrollmentVerificationFailed,
		CodexReenrollmentLeaseLost,
	}
	if !slices.Equal(AllCodexReenrollmentFailureClasses, failureClasses) {
		t.Fatalf(
			"failure classes = %v, want %v",
			AllCodexReenrollmentFailureClasses,
			failureClasses,
		)
	}
	for _, failureClass := range AllCodexReenrollmentFailureClasses {
		if !failureClass.valid() {
			t.Errorf("registered failure class %q is invalid", failureClass)
		}
	}
	if CodexReenrollmentFailureClass("").valid() {
		t.Error("empty failure class is valid")
	}
}

func TestCodexReenrollmentMarkerBindingMatchesIdentity(t *testing.T) {
	binding := domain.CodexReenrollmentRecoveryBinding{
		AuthIdentityID: "codex-other", LeaseFence: 1,
		AuthStoreDigest:      "sha256:replacement",
		AccessTokenExpiresAt: time.Date(2026, 8, 12, 1, 2, 3, 0, time.UTC),
	}
	_, err := NewCodexReenrollmentMarker(
		"codex-primary", 1,
		"project-1", 1, domain.StatusOpen, &binding,
	)
	if !errors.Is(err, domain.ErrCodexReenrollmentBindingMismatch) {
		t.Fatalf("cross-identity marker binding = %v, want mismatch", err)
	}
}

func TestCodexReenrollmentMarkerRequiresPositiveOccurrence(t *testing.T) {
	for _, occurrence := range []int{-1, 0} {
		_, err := NewCodexReenrollmentMarker(
			"codex-primary", occurrence, "project-1", 1, domain.StatusOpen, nil,
		)
		if !errors.Is(err, domain.ErrCodexReenrollmentMarkerMismatch) {
			t.Fatalf("occurrence %d = %v, want marker mismatch", occurrence, err)
		}
	}
}

func TestCodexReenrollmentJournalRejectsCompletionBeforeOpening(t *testing.T) {
	openedAt := time.Date(2026, 8, 11, 1, 2, 3, 0, time.UTC)
	class := CodexReenrollmentLeaseLost
	rec := CodexReenrollmentJournal{
		AuthIdentityID: "codex-primary",
		LeaseFence:     1,
		MarkerItemID:   "system-health-codex-auth-internal-1",
		Holder:         "enroll-1",
		OpenedAt:       openedAt,
		Terminal: &CodexReenrollmentTerminal{
			Outcome:      CodexReenrollmentFailed,
			FailureClass: &class,
			CompletedAt:  openedAt.Add(-time.Nanosecond),
		},
	}
	if err := rec.Validate(); err == nil {
		t.Fatal("journal accepted a terminal that predates its opening")
	}
}

func TestCodexReenrollmentRecoveryBindingValidatesCallerConstructedJournal(t *testing.T) {
	completedAt := time.Date(2026, 8, 11, 1, 2, 4, 0, time.UTC)
	digest := domain.Digest("sha256:replacement")
	expiresAt := completedAt.Add(time.Hour)
	for name, journal := range map[string]CodexReenrollmentJournal{
		"nil verified evidence": {
			AuthIdentityID: "codex-primary", LeaseFence: 1,
			MarkerItemID: "marker-1", Holder: "enroll-1",
			OpenedAt: completedAt.Add(-time.Second),
			Terminal: &CodexReenrollmentTerminal{
				Outcome: CodexReenrollmentVerified, CompletedAt: completedAt,
			},
		},
		"invalid journal coordinates": {
			LeaseFence: 1, MarkerItemID: "marker-1", Holder: "enroll-1",
			OpenedAt: completedAt.Add(-time.Second),
			Terminal: &CodexReenrollmentTerminal{
				Outcome: CodexReenrollmentVerified, AuthStoreDigest: &digest,
				AccessTokenExpiresAt: &expiresAt, CompletedAt: completedAt,
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := journal.RecoveryBinding(); err == nil {
				t.Fatal("invalid caller-constructed journal yielded a recovery binding")
			}
		})
	}
}

func TestCodexReenrollmentRecoveryCarrierValidatesCallerConstructedTransition(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, t.TempDir()+"/freeside.db", Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	at := time.Date(2026, 8, 11, 1, 2, 4, 0, time.UTC)
	commandID := "command-1"
	valid := domain.CodexReenrollmentRecoveryTransition{
		AuthIdentityID: "codex-primary", LeaseFence: 1,
		AuthStoreDigest: "sha256:replacement", AccessTokenExpiresAt: at.Add(time.Hour),
		CommandID: &commandID, Reason: "verified re-enrollment", OccurredAt: at,
	}
	for name, transition := range map[string]domain.CodexReenrollmentRecoveryTransition{
		"nil command": func() domain.CodexReenrollmentRecoveryTransition {
			got := valid
			got.CommandID = nil
			return got
		}(),
		"invalid binding coordinates": func() domain.CodexReenrollmentRecoveryTransition {
			got := valid
			got.AuthIdentityID = ""
			return got
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			if err := st.Read(ctx, func(tx *ReadTx) error {
				_, err := tx.CodexReenrollmentRecoveryCarrier(ctx, transition)
				return err
			}); err == nil {
				t.Fatal("invalid caller-constructed transition yielded a recovery carrier")
			}
		})
	}
}

func TestCodexReenrollmentRecoveryCarrierRequiresExactDecisionInstant(t *testing.T) {
	at := time.Date(2026, 8, 11, 1, 2, 4, 0, time.UTC)
	binding := domain.CodexReenrollmentRecoveryBinding{
		AuthIdentityID: "codex-primary", LeaseFence: 1,
		AuthStoreDigest: "sha256:replacement", AccessTokenExpiresAt: at.Add(time.Hour),
	}
	item, err := NewCodexReenrollmentMarker(
		binding.AuthIdentityID, 1, "project-1", 2, domain.StatusResolved, &binding,
	)
	if err != nil {
		t.Fatal(err)
	}
	item.DecidedAt = &at
	commandID := "command-1"
	transition := domain.CodexReenrollmentRecoveryTransition{
		AuthIdentityID: binding.AuthIdentityID, LeaseFence: binding.LeaseFence,
		AuthStoreDigest: binding.AuthStoreDigest, AccessTokenExpiresAt: binding.AccessTokenExpiresAt,
		CommandID: &commandID, Reason: "verified re-enrollment", OccurredAt: at,
	}
	if err := validateCodexReenrollmentRecoveryCarrier(item, transition); err != nil {
		t.Fatalf("exact decision instant: %v", err)
	}
	for name, mutate := range map[string]func(*domain.AttentionItem, *domain.CodexReenrollmentRecoveryTransition){
		"missing decision stamp": func(item *domain.AttentionItem, _ *domain.CodexReenrollmentRecoveryTransition) {
			item.DecidedAt = nil
		},
		"non-resolved carrier": func(item *domain.AttentionItem, _ *domain.CodexReenrollmentRecoveryTransition) {
			item.Status = domain.StatusOpen
		},
		"transition before decision": func(_ *domain.AttentionItem, transition *domain.CodexReenrollmentRecoveryTransition) {
			transition.OccurredAt = transition.OccurredAt.Add(-time.Nanosecond)
		},
		"transition after decision": func(_ *domain.AttentionItem, transition *domain.CodexReenrollmentRecoveryTransition) {
			transition.OccurredAt = transition.OccurredAt.Add(time.Nanosecond)
		},
	} {
		t.Run(name, func(t *testing.T) {
			gotItem := item
			gotTransition := transition
			mutate(&gotItem, &gotTransition)
			if err := validateCodexReenrollmentRecoveryCarrier(gotItem, gotTransition); !errors.Is(err, domain.ErrCodexReenrollmentMarkerMismatch) {
				t.Fatalf("carrier validation = %v, want marker mismatch", err)
			}
		})
	}
}

func TestCodexReenrollmentVerifiedRequiresUnexpiredEvidence(t *testing.T) {
	completedAt := time.Date(2026, 8, 11, 1, 2, 3, 0, time.UTC)
	digest := domain.Digest("sha256:replacement")
	for _, expiresAt := range []time.Time{
		completedAt.Add(-time.Nanosecond),
		completedAt,
	} {
		terminal := CodexReenrollmentTerminal{
			Outcome:              CodexReenrollmentVerified,
			AuthStoreDigest:      &digest,
			AccessTokenExpiresAt: &expiresAt,
			CompletedAt:          completedAt,
		}
		if err := terminal.Validate(); err == nil {
			t.Errorf("verified terminal accepted expiry %s at completion %s", expiresAt, completedAt)
		}
	}
	expiresAt := completedAt.Add(time.Nanosecond)
	terminal := CodexReenrollmentTerminal{
		Outcome:              CodexReenrollmentVerified,
		AuthStoreDigest:      &digest,
		AccessTokenExpiresAt: &expiresAt,
		CompletedAt:          completedAt,
	}
	if err := terminal.Validate(); err != nil {
		t.Fatalf("verified terminal rejected live evidence: %v", err)
	}
}

func TestCodexReenrollmentMigrationAppliesFromHead(t *testing.T) {
	ctx := context.Background()
	db := openRaw(t)
	migrateThrough(t, ctx, db, "0039_")
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatal(err)
	}
	if got := rawVersion(t, db); got != 49 {
		t.Fatalf("schema version = %d, want 49", got)
	}
	for _, table := range []string{
		"codex_reenrollment_operations", "codex_reenrollment_recovery_transitions",
	} {
		var count int
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s rows = %d, want empty", table, count)
		}
	}
}

func TestCodexReenrollmentMigrationNormalizesOnlyAuthenticatedLegacyMarkers(t *testing.T) {
	ctx := context.Background()
	db := openRaw(t)
	migrateThrough(t, ctx, db, "0039_")
	identities := []domain.AuthIdentityID{
		"codex-legacy",
		"codex-\"slash\\control\nunicode-鳥",
	}
	for _, id := range identities {
		if _, err := db.ExecContext(ctx, `INSERT INTO auth_identities
			(id, provider, auth_store_mutation_lease, auth_store_volume,
			 max_parallel_executions, refresh_strategy,
			 supports_read_only_auth_snapshot, recorded_at, body)
			VALUES (?, 'codex', 1, ?, 1, 'refresh_on_demand', 0,
			        '2026-08-11T12:00:00Z', '{}')`, id, "volume-"+string(id)); err != nil {
			t.Fatalf("seed identity %q: %v", id, err)
		}
	}
	var beforeRevision int64
	if err := db.QueryRowContext(ctx, `SELECT revision FROM server_state WHERE id = 1`).Scan(&beforeRevision); err != nil {
		t.Fatal(err)
	}

	first, err := legacyCodexReenrollmentMarker(
		identities[0], codexReenrollmentMigrationTestItemID(identities[0], "1"),
		"project-1", 3, domain.StatusOpen,
	)
	if err != nil {
		t.Fatal(err)
	}
	second, err := legacyCodexReenrollmentMarker(
		identities[1], codexReenrollmentMigrationTestItemID(identities[1], "2"),
		"project-1", 7, domain.StatusResolved,
	)
	if err != nil {
		t.Fatal(err)
	}
	second, err = second.WithDecidedAt(time.Date(2026, 8, 11, 12, 1, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	wrongID, err := legacyCodexReenrollmentMarker(
		identities[0], "system-health-codex-auth-wrong-3", "project-1", 1, domain.StatusOpen,
	)
	if err != nil {
		t.Fatal(err)
	}
	current := first
	current.ID = codexReenrollmentMigrationTestItemID(identities[0], "4")
	current.Reason = fmt.Sprintf(
		"Codex auth identity %q can no longer refresh. Complete verified re-enrollment to make the recovery action available.",
		identities[0],
	)
	current.RequestedDecision = []domain.Action{domain.ActionAcknowledge}
	if err := current.Validate(); err != nil {
		t.Fatal(err)
	}

	legacyBodies := make(map[domain.ItemID][]byte)
	for _, item := range []domain.AttentionItem{first, second, wrongID} {
		body := codexReenrollmentMigrationTestBody(t, item, true)
		legacyBodies[item.ID] = body
		insertCodexReenrollmentMigrationTestItem(t, ctx, db, item, body, beforeRevision)
	}
	currentBody := codexReenrollmentMigrationTestBody(t, current, false)
	insertCodexReenrollmentMigrationTestItem(t, ctx, db, current, currentBody, beforeRevision)

	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatal(err)
	}
	var afterRevision int64
	if err := db.QueryRowContext(ctx, `SELECT revision FROM server_state WHERE id = 1`).Scan(&afterRevision); err != nil {
		t.Fatal(err)
	}
	if afterRevision != beforeRevision+1 {
		t.Fatalf("server revision = %d, want %d", afterRevision, beforeRevision+1)
	}
	for _, want := range []domain.AttentionItem{first, second} {
		var entityVersion, asOfRevision int64
		var body []byte
		if err := db.QueryRowContext(ctx, `SELECT entity_version, as_of_revision, body
			FROM attention_items WHERE id = ?`, want.ID).Scan(&entityVersion, &asOfRevision, &body); err != nil {
			t.Fatal(err)
		}
		if entityVersion != 2 || asOfRevision != afterRevision {
			t.Errorf("marker %s metadata = (%d, %d), want (2, %d)", want.ID, entityVersion, asOfRevision, afterRevision)
		}
		var got domain.AttentionItem
		if err := decodeMigrationJSON(body, &got); err != nil {
			t.Fatal(err)
		}
		sameDecidedAt := got.DecidedAt == nil && want.DecidedAt == nil ||
			got.DecidedAt != nil && want.DecidedAt != nil && got.DecidedAt.Equal(*want.DecidedAt)
		wantReason := fmt.Sprintf(
			"Codex auth identity %q can no longer refresh. Complete verified re-enrollment to make the recovery action available.",
			map[domain.ItemID]domain.AuthIdentityID{first.ID: identities[0], second.ID: identities[1]}[want.ID],
		)
		if got.ItemVersion != want.ItemVersion || got.Status != want.Status ||
			!sameDecidedAt ||
			got.Reason != wantReason ||
			!slices.Equal(got.RequestedDecision, []domain.Action{domain.ActionAcknowledge}) ||
			got.CodexReenrollmentRecoveryBinding != nil {
			t.Errorf("normalized marker %s = %+v", want.ID, got)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(body, &fields); err != nil {
			t.Fatal(err)
		}
		if value, ok := fields["codex_reenrollment_recovery_binding"]; !ok || string(value) != "null" {
			t.Errorf("marker %s binding JSON = %s, present=%t", want.ID, value, ok)
		}
	}
	for id, wantBody := range map[domain.ItemID][]byte{
		wrongID.ID: legacyBodies[wrongID.ID],
		current.ID: currentBody,
	} {
		var entityVersion, asOfRevision int64
		var body []byte
		if err := db.QueryRowContext(ctx, `SELECT entity_version, as_of_revision, body
			FROM attention_items WHERE id = ?`, id).Scan(&entityVersion, &asOfRevision, &body); err != nil {
			t.Fatal(err)
		}
		if entityVersion != 1 || asOfRevision != beforeRevision || !bytes.Equal(body, wantBody) {
			t.Errorf("near-match marker %s was rewritten", id)
		}
	}

	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatal(err)
	}
	var repeatedRevision int64
	if err := db.QueryRowContext(ctx, `SELECT revision FROM server_state WHERE id = 1`).Scan(&repeatedRevision); err != nil {
		t.Fatal(err)
	}
	if repeatedRevision != afterRevision {
		t.Fatalf("idempotent migrate revision = %d, want %d", repeatedRevision, afterRevision)
	}
}

func TestCodexReenrollmentMigrationNoMatchPreservesRevision(t *testing.T) {
	ctx := context.Background()
	db := openRaw(t)
	migrateThrough(t, ctx, db, "0039_")
	var before int64
	if err := db.QueryRowContext(ctx, `SELECT revision FROM server_state WHERE id = 1`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := rewriteLegacyCodexReenrollmentMarkers(ctx, tx); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var after int64
	if err := db.QueryRowContext(ctx, `SELECT revision FROM server_state WHERE id = 1`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("no-match migration revision = %d, want %d", after, before)
	}
}

func TestAuthenticateLegacyCodexReenrollmentMarkerRejectsNearMatches(t *testing.T) {
	id := domain.AuthIdentityID("codex-near-match")
	valid, err := legacyCodexReenrollmentMarker(
		id, codexReenrollmentMigrationTestItemID(id, "10"), "project-1", 1, domain.StatusOpen,
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		itemID domain.ItemID
		mutate func(*domain.AttentionItem)
	}{
		{name: "canonical numeric order", itemID: codexReenrollmentMigrationTestItemID(id, "10")},
		{name: "leading zero", itemID: codexReenrollmentMigrationTestItemID(id, "010")},
		{name: "explicit plus", itemID: codexReenrollmentMigrationTestItemID(id, "+10")},
		{name: "zero", itemID: codexReenrollmentMigrationTestItemID(id, "0")},
		{name: "wrong reason", itemID: valid.ID, mutate: func(item *domain.AttentionItem) { item.Reason += " " }},
		{name: "reversed actions", itemID: valid.ID, mutate: func(item *domain.AttentionItem) {
			item.RequestedDecision = []domain.Action{domain.ActionStopUnattended, domain.ActionAcknowledge}
		}},
		{name: "wrong subject", itemID: valid.ID, mutate: func(item *domain.AttentionItem) { item.Subject.ID = "other" }},
		{name: "wrong priority", itemID: valid.ID, mutate: func(item *domain.AttentionItem) { item.Priority = domain.PriorityNormal }},
	} {
		t.Run(test.name, func(t *testing.T) {
			item := valid
			item.ID = test.itemID
			if test.mutate != nil {
				test.mutate(&item)
			}
			body := codexReenrollmentMigrationTestBody(t, item, true)
			row := legacyCodexReenrollmentRow{
				id: item.ID, projectID: item.ProjectID, itemType: string(item.Type),
				status: string(item.Status), healthPosture: sql.NullString{String: string(*item.Posture), Valid: true},
				entityVersion: 1, body: body,
			}
			_, ok := authenticateLegacyCodexReenrollmentMarker(row, []domain.AuthIdentityID{id})
			want := test.name == "canonical numeric order"
			if ok != want {
				t.Fatalf("authenticated = %t, want %t", ok, want)
			}
		})
	}
	validBody := codexReenrollmentMigrationTestBody(t, valid, true)
	var unknownFields map[string]json.RawMessage
	if err := json.Unmarshal(validBody, &unknownFields); err != nil {
		t.Fatal(err)
	}
	unknownFields["unknown"] = json.RawMessage(`true`)
	unknownBody, err := json.Marshal(unknownFields)
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string][]byte{
		"unknown field": unknownBody,
		"trailing body": append(slices.Clone(validBody), []byte(` {}`)...),
	} {
		t.Run(name, func(t *testing.T) {
			row := legacyCodexReenrollmentRow{
				id: valid.ID, projectID: valid.ProjectID, itemType: string(valid.Type),
				status: string(valid.Status), healthPosture: sql.NullString{String: string(*valid.Posture), Valid: true},
				entityVersion: 1, body: body,
			}
			if _, ok := authenticateLegacyCodexReenrollmentMarker(row, []domain.AuthIdentityID{id}); ok {
				t.Fatal("malformed legacy body authenticated")
			}
		})
	}
}

func codexReenrollmentMigrationTestItemID(id domain.AuthIdentityID, suffix string) domain.ItemID {
	digest := contentaddr.Hex(contentaddr.Sum([]byte(id)))
	return domain.ItemID("system-health-codex-auth-" + digest + "-" + suffix)
}

func codexReenrollmentMigrationTestBody(
	t *testing.T, item domain.AttentionItem, omitBinding bool,
) []byte {
	t.Helper()
	body, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	if !omitBinding {
		return body
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "codex_reenrollment_recovery_binding")
	body, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func insertCodexReenrollmentMigrationTestItem(
	t *testing.T, ctx context.Context, db *sql.DB, item domain.AttentionItem,
	body []byte, asOfRevision int64,
) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `INSERT INTO attention_items
		(id, project_id, conversation_id, item_type, status, health_posture,
		 entity_version, as_of_revision, body)
		VALUES (?, ?, NULL, ?, ?, ?, 1, ?, ?)`,
		item.ID, item.ProjectID, item.Type, item.Status, item.Posture, asOfRevision, string(body)); err != nil {
		t.Fatalf("seed marker %s: %v", item.ID, err)
	}
}

func TestCodexReenrollmentReconstructionRejectsUnknownAndTrailingBody(t *testing.T) {
	for _, mutate := range []struct {
		name string
		body func(string) string
	}{
		{"unknown field", func(body string) string {
			return strings.TrimSuffix(body, "}") + `,"unknown":true}`
		}},
		{"trailing data", func(body string) string { return body + `{}` }},
	} {
		t.Run(mutate.name, func(t *testing.T) {
			ctx := context.Background()
			st, err := Open(ctx, t.TempDir()+"/freeside.db", Options{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			at := time.Date(2026, 8, 11, 1, 2, 3, 0, time.UTC)
			identity := domain.AuthIdentity{
				ID: "codex-primary", Provider: "codex", AuthStoreMutationLease: true,
				AuthStoreVolume: "codex-auth", MaxParallelExecutions: 1,
				RefreshStrategy: domain.RefreshOnDemand,
			}
			markerID := domain.ItemID(CodexReenrollmentMarkerPrefix(identity.ID) + "1")
			var rec CodexReenrollmentJournal
			if err := st.WriteInternal(ctx, func(tx *InternalTx) error {
				return tx.RecordAuthIdentity(ctx, identity, at)
			}); err != nil {
				t.Fatal(err)
			}
			marker, err := NewCodexReenrollmentMarker(
				identity.ID, 1, "project-1", 1, domain.StatusOpen, nil,
			)
			if err != nil {
				t.Fatal(err)
			}
			if err := st.Write(ctx, func(tx *WriteTx) error { return tx.PutAttentionItem(ctx, marker) }); err != nil {
				t.Fatal(err)
			}
			if err := st.WriteInternal(ctx, func(tx *InternalTx) error {
				var err error
				rec, _, err = tx.BeginCodexReenrollmentJournal(
					ctx, identity.ID, markerID, "enroll-1", at, at.Add(time.Minute))
				return err
			}); err != nil {
				t.Fatal(err)
			}
			var body string
			if err := st.db.QueryRowContext(ctx,
				`SELECT body FROM codex_reenrollment_operations WHERE auth_identity_id = ? AND lease_fence = ?`,
				identity.ID, rec.LeaseFence).Scan(&body); err != nil {
				t.Fatal(err)
			}
			if _, err := st.db.ExecContext(ctx,
				`UPDATE codex_reenrollment_operations SET body = ? WHERE auth_identity_id = ? AND lease_fence = ?`,
				mutate.body(body), identity.ID, rec.LeaseFence); err != nil {
				t.Fatal(err)
			}
			if err := st.Read(ctx, func(tx *ReadTx) error {
				_, _, err := tx.LatestCodexReenrollmentJournal(ctx, identity.ID)
				return err
			}); err == nil {
				t.Fatal("malformed body reconstructed")
			}
		})
	}
}
