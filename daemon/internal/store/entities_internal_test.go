package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/migrations"
)

// TestGetRejectsInconsistentRow: a row whose JSON body disagrees with its
// extracted key columns must fail the read, whether the mismatch is on the
// queried key or on a join column the lookup did not filter by; the columns
// are what foreign keys and lookups enforce, so a divergent body is corrupt,
// not trusted data. Internal test: writing the corrupt row requires raw SQL
// past the Put boundary.
func TestGetRejectsInconsistentRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openRaw(t)
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := seedEpoch(ctx, db); err != nil {
		t.Fatalf("seedEpoch: %v", err)
	}
	s := &Store{db: db}

	body, err := encode(domain.Run{
		ID: "run-1", ProjectID: "proj-1",
		SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	cases := []struct {
		name   string
		id     string // the id column (and Get key); the body always says run-1
		proj   string // the project_id column; the body always says proj-1
		policy string // the policy_digest column; the body always says sha256:policy
		getID  domain.RunID
	}{
		{"queried key differs from body identity", "run-2", "proj-1", "sha256:policy", "run-2"},
		{"join column differs from body", "run-1", "proj-other", "sha256:policy", "run-1"},
		{"digest column differs from body", "run-1", "proj-1", "sha256:other", "run-1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := db.ExecContext(ctx, `DELETE FROM runs`); err != nil {
				t.Fatalf("reset: %v", err)
			}
			if _, err := db.ExecContext(ctx,
				`INSERT INTO runs (id, project_id, policy_digest, entity_version, as_of_revision, body) VALUES (?, ?, ?, 1, 1, ?)`,
				tc.id, tc.proj, tc.policy, body); err != nil {
				t.Fatalf("insert corrupt row: %v", err)
			}
			err := s.Read(ctx, func(tx *ReadTx) error {
				_, err := tx.GetRun(ctx, tc.getID)
				return err
			})
			if !errors.Is(err, errRowInconsistent) {
				t.Fatalf("GetRun error = %v, want errRowInconsistent", err)
			}
		})
	}
}

// TestGetCommandRejectsInconsistentRow: a commands row whose JSON body disagrees
// with an extracted binding column (the bound version, action, and the rest are
// the authoritative decision record) must fail the read. The columns are what
// the store and its foreign keys act on, so a divergent body is a forged
// decision record, not trusted data (#32 acceptance 4). Internal test: the
// corrupt row is written past the Put boundary as raw SQL.
func TestGetCommandRejectsInconsistentRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openRaw(t)
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := seedEpoch(ctx, db); err != nil {
		t.Fatalf("seedEpoch: %v", err)
	}
	s := &Store{db: db}

	// A parent attention item to satisfy the commands foreign key; GetCommand
	// never reads it, so a minimal raw row suffices.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO attention_items (id, project_id, conversation_id, entity_version, as_of_revision, body) VALUES ('item-1', 'proj-1', NULL, 1, 1, '{}')`); err != nil {
		t.Fatalf("insert parent item: %v", err)
	}

	// A well-formed command body (passes domain Validate): the columns below
	// diverge from it one at a time.
	const body = `{"command_id":"cmd-1","device_id":"device-1","item_id":"item-1","item_version":1,"pr_head_sha":"cafebabe","artifact_digests":["sha256:img","sha256:log"],"action":"open_pr"}`

	cases := []struct {
		name    string
		version int
		action  string
	}{
		{"version column differs from body", 2, "open_pr"},
		{"action column differs from body", 1, "stop"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := db.ExecContext(ctx, `DELETE FROM commands`); err != nil {
				t.Fatalf("reset: %v", err)
			}
			if _, err := db.ExecContext(ctx,
				`INSERT INTO commands (command_id, item_id, item_version, pr_head_sha, device_id, action, entity_version, as_of_revision, body) VALUES ('cmd-1', 'item-1', ?, 'cafebabe', 'device-1', ?, 1, 1, ?)`,
				tc.version, tc.action, body); err != nil {
				t.Fatalf("insert corrupt row: %v", err)
			}
			err := s.Read(ctx, func(tx *ReadTx) error {
				_, err := tx.GetCommand(ctx, "cmd-1")
				return err
			})
			if !errors.Is(err, errRowInconsistent) {
				t.Fatalf("GetCommand error = %v, want errRowInconsistent", err)
			}
		})
	}
}

// TestGetResolvedPolicyRejectsForgedDigest: a stored resolved_policies body
// whose digest is internally consistent with its digest column (so the
// row-consistency check passes) but does not address the keys it carries must
// fail the read. decode re-validates every body, and Validate recomputes the
// content digest, so a forged digest is refused on read as well as on write
// (#33 acceptance 2, the "stored digest" half). Internal test: encode would
// reject the body, so it is written past the Put boundary as raw JSON.
func TestGetResolvedPolicyRejectsForgedDigest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openRaw(t)
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := seedEpoch(ctx, db); err != nil {
		t.Fatalf("seedEpoch: %v", err)
	}
	s := &Store{db: db}

	// A run to satisfy the resolved_policies foreign key.
	runBody, err := encode(domain.Run{
		ID: "run-1", ProjectID: "proj-1",
		SpecDigest: "sha256:spec", PolicyDigest: "sha256:forged",
	})
	if err != nil {
		t.Fatalf("encode run: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO runs (id, project_id, policy_digest, entity_version, as_of_revision, body) VALUES ('run-1', 'proj-1', 'sha256:forged', 1, 1, ?)`,
		runBody); err != nil {
		t.Fatalf("insert run: %v", err)
	}

	// digest column == body.digest (row-consistent) but neither addresses the
	// keys: their authentic content digest is something else entirely.
	const forgedBody = `{"run_id":"run-1","digest":"sha256:forged","keys":[{"key":"rein","value":"tight","provenance":{"source":"preset","digest":"sha256:preset"}}]}`
	if _, err := db.ExecContext(ctx,
		`INSERT INTO resolved_policies (run_id, digest, entity_version, as_of_revision, body) VALUES ('run-1', 'sha256:forged', 1, 1, ?)`,
		forgedBody); err != nil {
		t.Fatalf("insert forged policy: %v", err)
	}

	err = s.Read(ctx, func(tx *ReadTx) error {
		_, err := tx.GetResolvedPolicy(ctx, "run-1")
		return err
	})
	if !errors.Is(err, domain.ErrPolicyDigestMismatch) {
		t.Fatalf("GetResolvedPolicy error = %v, want ErrPolicyDigestMismatch", err)
	}
}

func TestMigrateLegacyTrustProfileRunPolicyIsExact(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db := openRaw(t)
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatal(err)
	}
	if err := seedEpoch(ctx, db); err != nil {
		t.Fatal(err)
	}
	st := &Store{db: db}
	legacy := domain.Run{
		ID: "run-legacy", ProjectID: "project-1",
		SpecDigest: "sha256:spec", PolicyDigest: "sha256:legacy-policy",
	}
	policy, err := domain.NewResolvedPolicy(legacy.ID, []domain.PolicyKey{{
		Key: "trust_profile_digest", Value: string(legacy.PolicyDigest),
		Provenance: domain.KeyProvenance{
			Source: domain.ProvenanceOverride, Digest: legacy.PolicyDigest,
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	updated := legacy
	updated.PolicyDigest = policy.Digest
	itemID, itemVersion := domain.ItemID("item-legacy"), 1
	fireAt := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	deadline := func(id domain.ScheduleID) domain.Schedule {
		schedule, err := domain.NewSchedule(domain.ScheduleInput{
			ID: id, ProjectID: legacy.ProjectID,
			Kind: domain.SchedulePRChecksDeadline,
			Subject: domain.ScheduleSubject{
				Type:   domain.ScheduleSubjectAttentionItem,
				ItemID: &itemID, ItemVersion: &itemVersion,
			},
			RunID: &legacy.ID, PolicyDigest: &legacy.PolicyDigest,
			CreatedAt: fireAt.Add(-time.Minute), FireAt: &fireAt,
		})
		if err != nil {
			t.Fatal(err)
		}
		return schedule
	}
	armed := deadline("schedule-armed")
	fired, err := deadline("schedule-fired").Concluded(
		domain.ScheduleFired, domain.ResolutionDeadlineElapsed, fireAt,
	)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := deadline("schedule-resolved").Concluded(
		domain.ScheduleResolved, domain.ResolutionSubjectConcluded, fireAt,
	)
	if err != nil {
		t.Fatal(err)
	}
	interval := int64(60)
	expiresAt := fireAt.Add(time.Hour)
	watch, err := domain.NewSchedule(domain.ScheduleInput{
		ID: "schedule-expired", ProjectID: legacy.ProjectID,
		Kind: domain.ScheduleBaseAdvanceWatch,
		Subject: domain.ScheduleSubject{
			Type:   domain.ScheduleSubjectAttentionItem,
			ItemID: &itemID, ItemVersion: &itemVersion,
		},
		RunID: &legacy.ID, PolicyDigest: &legacy.PolicyDigest,
		CreatedAt: fireAt.Add(-time.Minute), IntervalSeconds: &interval,
		ExpiresAt: &expiresAt,
		BaseWatch: &domain.ScheduleBaseWatch{
			Repo: "freeside-ai/freeside", BaseRef: "main", AdmittedBaseSHA: "base-sha",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	expired, err := watch.Concluded(
		domain.ScheduleExpired, domain.ResolutionIntentExpired, expiresAt,
	)
	if err != nil {
		t.Fatal(err)
	}
	schedules := []domain.Schedule{armed, fired, resolved, expired}
	if err := st.Write(ctx, func(tx *WriteTx) error {
		if err := tx.PutRun(ctx, legacy); err != nil {
			return err
		}
		for _, schedule := range schedules {
			if err := tx.PutSchedule(ctx, schedule); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	changedSpec := updated
	changedSpec.SpecDigest = "sha256:changed-spec"
	if err := st.Write(ctx, func(tx *WriteTx) error {
		return tx.MigrateLegacyTrustProfileRunPolicy(ctx, legacy, changedSpec, policy)
	}); !errors.Is(err, domain.ErrImmutableTransition) {
		t.Fatalf("migration with changed spec = %v, want ErrImmutableTransition", err)
	}
	if err := st.Read(ctx, func(tx *ReadTx) error {
		run, err := tx.GetRun(ctx, legacy.ID)
		if err != nil {
			return err
		}
		if run.PolicyDigest != legacy.PolicyDigest {
			t.Fatalf("failed migration changed run digest to %q", run.PolicyDigest)
		}
		stored, err := tx.ListSchedules(ctx)
		if err != nil {
			return err
		}
		if len(stored) != len(schedules) {
			t.Fatalf("failed migration left %d schedules, want %d", len(stored), len(schedules))
		}
		for _, snapshot := range stored {
			if snapshot.Value.PolicyDigest == nil ||
				*snapshot.Value.PolicyDigest != legacy.PolicyDigest {
				t.Fatalf("failed migration changed schedule %q policy to %v",
					snapshot.Value.ID, snapshot.Value.PolicyDigest)
			}
		}
		_, err = tx.GetResolvedPolicy(ctx, legacy.ID)
		return err
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed migration policy read = %v, want ErrNotFound", err)
	}
	unrelated, err := domain.NewResolvedPolicy(legacy.ID, []domain.PolicyKey{{
		Key: "driver", Value: "claude",
		Provenance: domain.KeyProvenance{
			Source: domain.ProvenanceOverride, Digest: legacy.PolicyDigest,
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	unrelatedRun := legacy
	unrelatedRun.PolicyDigest = unrelated.Digest
	if err := st.Write(ctx, func(tx *WriteTx) error {
		return tx.MigrateLegacyTrustProfileRunPolicy(ctx, legacy, unrelatedRun, unrelated)
	}); !errors.Is(err, domain.ErrImmutableTransition) {
		t.Fatalf("migration to unrelated policy = %v, want ErrImmutableTransition", err)
	}

	conflicting := schedules[0]
	conflictingDigest := domain.Digest("sha256:conflicting-policy")
	conflicting.PolicyDigest = &conflictingDigest
	conflictingBody, err := encode(conflicting)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
UPDATE schedules SET policy_digest = ?, body = ? WHERE id = ?`,
		conflictingDigest, conflictingBody, conflicting.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *WriteTx) error {
		return tx.MigrateLegacyTrustProfileRunPolicy(ctx, legacy, updated, policy)
	}); !errors.Is(err, domain.ErrImmutableTransition) {
		t.Fatalf("migration with conflicting schedule = %v, want ErrImmutableTransition", err)
	}
	if err := st.Read(ctx, func(tx *ReadTx) error {
		run, err := tx.GetRun(ctx, legacy.ID)
		if err != nil {
			return err
		}
		if run.PolicyDigest != legacy.PolicyDigest {
			t.Fatalf("rolled-back migration left run policy %q", run.PolicyDigest)
		}
		got, err := tx.GetSchedule(ctx, conflicting.ID)
		if err != nil {
			return err
		}
		if got.PolicyDigest == nil || *got.PolicyDigest != conflictingDigest {
			t.Fatalf("rolled-back migration changed conflicting schedule to %v", got.PolicyDigest)
		}
		_, err = tx.GetResolvedPolicy(ctx, legacy.ID)
		return err
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rolled-back migration policy read = %v, want ErrNotFound", err)
	}
	legacyBody, err := encode(schedules[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
UPDATE schedules SET policy_digest = ?, body = ? WHERE id = ?`,
		legacy.PolicyDigest, legacyBody, conflicting.ID); err != nil {
		t.Fatal(err)
	}

	if err := st.Write(ctx, func(tx *WriteTx) error {
		return tx.MigrateLegacyTrustProfileRunPolicy(ctx, legacy, updated, policy)
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Read(ctx, func(tx *ReadTx) error {
		run, err := tx.GetRun(ctx, legacy.ID)
		if err != nil {
			return err
		}
		gotPolicy, err := tx.GetResolvedPolicy(ctx, legacy.ID)
		if err != nil {
			return err
		}
		if run.PolicyDigest != policy.Digest || gotPolicy.Digest != policy.Digest {
			t.Fatalf("migrated binding = run %q policy %q", run.PolicyDigest, gotPolicy.Digest)
		}
		stored, err := tx.ListSchedules(ctx)
		if err != nil {
			return err
		}
		if len(stored) != len(schedules) {
			t.Fatalf("migrated schedules = %d, want %d", len(stored), len(schedules))
		}
		wantStatuses := map[domain.ScheduleStatus]bool{
			domain.ScheduleArmed: false, domain.ScheduleFired: false,
			domain.ScheduleResolved: false, domain.ScheduleExpired: false,
		}
		for _, snapshot := range stored {
			schedule := snapshot.Value
			if schedule.PolicyDigest == nil || *schedule.PolicyDigest != policy.Digest {
				t.Fatalf("migrated schedule %q policy = %v, want %q",
					schedule.ID, schedule.PolicyDigest, policy.Digest)
			}
			if snapshot.Snapshot.EntityVersion != 2 {
				t.Fatalf("migrated schedule %q entity version = %d, want 2",
					schedule.ID, snapshot.Snapshot.EntityVersion)
			}
			wantStatuses[schedule.Status] = true
		}
		for status, found := range wantStatuses {
			if !found {
				t.Fatalf("migration did not exercise %s schedule", status)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestGetRejectsForgedMetadata: runs, conversations, and deliveries share the
// items' reconstruction convention (#91, extended by #98's shared scan
// functions), so a store-impossible entity_version or as_of_revision fails
// their Gets closed too. Internal test: the Put boundary always stamps valid
// values, so the bad rows are written as raw SQL.
func TestGetRejectsForgedMetadata(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openRaw(t)
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := seedEpoch(ctx, db); err != nil {
		t.Fatalf("seedEpoch: %v", err)
	}
	s := &Store{db: db}

	runBody, err := encode(domain.Run{
		ID: "run-1", ProjectID: "proj-1",
		SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
	})
	if err != nil {
		t.Fatalf("encode run: %v", err)
	}
	conversationBody, err := encode(domain.Conversation{ID: "conv-1", Status: domain.ConversationIdle})
	if err != nil {
		t.Fatalf("encode conversation: %v", err)
	}
	deliveryBody, err := encode(domain.AttentionDelivery{
		ItemID: "item-1", DeviceID: "device-1", Channel: "ntfy", Attempt: 1,
		SubmittedAt: time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC),
		Status:      domain.DeliverySubmitted,
	})
	if err != nil {
		t.Fatalf("encode delivery: %v", err)
	}
	// A parent attention item to satisfy the deliveries foreign key; the
	// delivery Get never reads it, so a minimal raw row suffices.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO attention_items (id, project_id, conversation_id, entity_version, as_of_revision, body) VALUES ('item-1', 'proj-1', NULL, 1, 1, '{}')`); err != nil {
		t.Fatalf("insert parent item: %v", err)
	}

	seeds := map[string]func(entityVersion, asOfRevision int64){
		"run": func(ev, rev int64) {
			t.Helper()
			if _, err := db.ExecContext(ctx, `DELETE FROM runs`); err != nil {
				t.Fatalf("reset runs: %v", err)
			}
			if _, err := db.ExecContext(ctx,
				`INSERT INTO runs (id, project_id, policy_digest, entity_version, as_of_revision, body) VALUES ('run-1', 'proj-1', 'sha256:policy', ?, ?, ?)`,
				ev, rev, runBody); err != nil {
				t.Fatalf("insert run: %v", err)
			}
		},
		"conversation": func(ev, rev int64) {
			t.Helper()
			if _, err := db.ExecContext(ctx, `DELETE FROM conversations`); err != nil {
				t.Fatalf("reset conversations: %v", err)
			}
			if _, err := db.ExecContext(ctx,
				`INSERT INTO conversations (id, entity_version, as_of_revision, body) VALUES ('conv-1', ?, ?, ?)`,
				ev, rev, conversationBody); err != nil {
				t.Fatalf("insert conversation: %v", err)
			}
		},
		"delivery": func(ev, rev int64) {
			t.Helper()
			if _, err := db.ExecContext(ctx, `DELETE FROM attention_deliveries`); err != nil {
				t.Fatalf("reset deliveries: %v", err)
			}
			if _, err := db.ExecContext(ctx,
				`INSERT INTO attention_deliveries (item_id, device_id, channel, attempt, entity_version, as_of_revision, body) VALUES ('item-1', 'device-1', 'ntfy', 1, ?, ?, ?)`,
				ev, rev, deliveryBody); err != nil {
				t.Fatalf("insert delivery: %v", err)
			}
		},
	}
	reads := map[string]func() error{
		"run": func() error {
			return s.Read(ctx, func(tx *ReadTx) error {
				_, err := tx.GetRun(ctx, "run-1")
				return err
			})
		},
		"conversation": func() error {
			return s.Read(ctx, func(tx *ReadTx) error {
				_, err := tx.GetConversation(ctx, "conv-1")
				return err
			})
		},
		"delivery": func() error {
			return s.Read(ctx, func(tx *ReadTx) error {
				_, err := tx.GetAttentionDelivery(ctx, "item-1", "device-1", "ntfy", 1)
				return err
			})
		},
	}

	for entity, seed := range seeds {
		read := reads[entity]
		// Control rows prove the fixtures themselves reconstruct, so the forged
		// cases below fail on the metadata alone.
		seed(1, 1)
		if err := read(); err != nil {
			t.Fatalf("control %s read: %v", entity, err)
		}
		forged := []struct {
			name    string
			ev, rev int64
		}{
			{"entity_version zero", 0, 1},
			{"entity_version negative", -1, 1},
			{"as_of_revision zero", 1, 0},
			{"as_of_revision negative", 1, -1},
		}
		for _, tc := range forged {
			t.Run(entity+" "+tc.name, func(t *testing.T) {
				seed(tc.ev, tc.rev)
				if err := read(); !errors.Is(err, errRowInconsistent) {
					t.Fatalf("error = %v, want errRowInconsistent", err)
				}
			})
		}
	}
}

// TestListRejectsForgedMetadata: the collection reads reconstruct through
// the same scan functions as the Gets (#98 acceptance 2), so one row with
// store-impossible metadata fails the whole list closed, even beside a valid
// sibling row. Internal test: the bad rows are written as raw SQL.
func TestListRejectsForgedMetadata(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openRaw(t)
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := seedEpoch(ctx, db); err != nil {
		t.Fatalf("seedEpoch: %v", err)
	}
	s := &Store{db: db}

	// One valid sibling row per table proves the list fails because of the
	// forged row, not the fixtures, and that a corrupt row is never skipped.
	runID := domain.RunID("run-b")
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "item-b", ProjectID: "proj-1",
		Subject: domain.Subject{Type: domain.SubjectRun, ID: "run-b", RunID: &runID},
		Type:    domain.AttentionReadyForFinalReview, Priority: domain.PriorityNormal,
		Reason:            "forged-metadata sibling fixture",
		RequestedDecision: []domain.Action{domain.ActionOpenPR},
		PRHeadSHA:         "cafebabe",
		PRReference:       &domain.PRReference{Repo: "owner/repo", Number: 123},
		ItemVersion:       1,
		InterruptionClass: domain.InterruptionPlannedGate,
		Status:            domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatalf("NewAttentionItem: %v", err)
	}
	bodies := map[string]string{}
	for entity, v := range map[string]validator{
		"run-a":  domain.Run{ID: "run-a", ProjectID: "proj-1", SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy"},
		"run-b":  domain.Run{ID: "run-b", ProjectID: "proj-1", SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy"},
		"conv-a": domain.Conversation{ID: "conv-a", Status: domain.ConversationIdle},
		"conv-b": domain.Conversation{ID: "conv-b", Status: domain.ConversationIdle},
		"item-b": item,
		"delivery-a": domain.AttentionDelivery{
			ItemID: "item-b", DeviceID: "device-1", Channel: "ntfy", Attempt: 1,
			SubmittedAt: time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC), Status: domain.DeliverySubmitted,
		},
		"delivery-b": domain.AttentionDelivery{
			ItemID: "item-b", DeviceID: "device-1", Channel: "ntfy", Attempt: 2,
			SubmittedAt: time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC), Status: domain.DeliverySubmitted,
		},
	} {
		body, err := encode(v)
		if err != nil {
			t.Fatalf("encode %s: %v", entity, err)
		}
		bodies[entity] = body
	}
	prReferenceBody, err := encode(*item.PRReference)
	if err != nil {
		t.Fatalf("encode pr reference: %v", err)
	}
	// The forged item body reuses the sibling's shape under its own id.
	forgedItem := item
	forgedItem.ID = "item-a"
	forgedItem.Subject.ID = "run-a"
	forgedRunID := domain.RunID("run-a")
	forgedItem.Subject.RunID = &forgedRunID
	forgedItemBody, err := encode(forgedItem)
	if err != nil {
		t.Fatalf("encode forged item: %v", err)
	}
	bodies["item-a"] = forgedItemBody

	// Valid siblings ("-b") at metadata (1, 1); the "-a" row of each table
	// carries the forged metadata per case.
	seed := func(ev, rev int64) {
		t.Helper()
		for _, reset := range []string{
			`DELETE FROM attention_deliveries`,
			`DELETE FROM attention_item_pr_references`,
			`DELETE FROM attention_items`,
			`DELETE FROM conversations`,
			`DELETE FROM runs`,
		} {
			if _, err := db.ExecContext(ctx, reset); err != nil {
				t.Fatalf("%s: %v", reset, err)
			}
		}
		stmts := []struct {
			sql  string
			args []any
		}{
			{`INSERT INTO runs (id, project_id, policy_digest, entity_version, as_of_revision, body) VALUES ('run-a', 'proj-1', 'sha256:policy', ?, ?, ?)`, []any{ev, rev, bodies["run-a"]}},
			{`INSERT INTO runs (id, project_id, policy_digest, entity_version, as_of_revision, body) VALUES ('run-b', 'proj-1', 'sha256:policy', 1, 1, ?)`, []any{bodies["run-b"]}},
			{`INSERT INTO conversations (id, entity_version, as_of_revision, body) VALUES ('conv-a', ?, ?, ?)`, []any{ev, rev, bodies["conv-a"]}},
			{`INSERT INTO conversations (id, entity_version, as_of_revision, body) VALUES ('conv-b', 1, 1, ?)`, []any{bodies["conv-b"]}},
			{`INSERT INTO attention_items (id, project_id, conversation_id, item_type, status, entity_version, as_of_revision, body) VALUES ('item-a', 'proj-1', NULL, ?, ?, ?, ?, ?)`, []any{item.Type, item.Status, ev, rev, bodies["item-a"]}},
			{`INSERT INTO attention_items (id, project_id, conversation_id, item_type, status, entity_version, as_of_revision, body) VALUES ('item-b', 'proj-1', NULL, ?, ?, 1, 1, ?)`, []any{item.Type, item.Status, bodies["item-b"]}},
			{`INSERT INTO attention_item_pr_references (item_id, repo, pr_number, body) VALUES ('item-a', 'owner/repo', 123, ?)`, []any{prReferenceBody}},
			{`INSERT INTO attention_item_pr_references (item_id, repo, pr_number, body) VALUES ('item-b', 'owner/repo', 123, ?)`, []any{prReferenceBody}},
			{`INSERT INTO attention_deliveries (item_id, device_id, channel, attempt, entity_version, as_of_revision, body) VALUES ('item-b', 'device-1', 'ntfy', 1, ?, ?, ?)`, []any{ev, rev, bodies["delivery-a"]}},
			{`INSERT INTO attention_deliveries (item_id, device_id, channel, attempt, entity_version, as_of_revision, body) VALUES ('item-b', 'device-1', 'ntfy', 2, 1, 1, ?)`, []any{bodies["delivery-b"]}},
		}
		for _, stmt := range stmts {
			if _, err := db.ExecContext(ctx, stmt.sql, stmt.args...); err != nil {
				t.Fatalf("insert: %v", err)
			}
		}
	}
	lists := map[string]func(tx *ReadTx) error{
		"runs": func(tx *ReadTx) error {
			_, err := tx.ListRuns(ctx)
			return err
		},
		"conversations": func(tx *ReadTx) error {
			_, err := tx.ListConversations(ctx)
			return err
		},
		"items": func(tx *ReadTx) error {
			_, err := tx.ListAttentionItems(ctx)
			return err
		},
		"deliveries": func(tx *ReadTx) error {
			_, err := tx.ListAttentionDeliveries(ctx)
			return err
		},
	}

	// Control: with valid metadata everywhere, every list reconstructs both
	// rows, so the forged cases below fail on the metadata alone.
	seed(1, 1)
	for name, list := range lists {
		if err := s.Read(ctx, func(tx *ReadTx) error { return list(tx) }); err != nil {
			t.Fatalf("control %s list: %v", name, err)
		}
	}

	forged := []struct {
		name    string
		ev, rev int64
	}{
		{"entity_version zero", 0, 1},
		{"entity_version negative", -1, 1},
		{"as_of_revision zero", 1, 0},
		{"as_of_revision negative", 1, -1},
	}
	for _, tc := range forged {
		seed(tc.ev, tc.rev)
		for name, list := range lists {
			t.Run(name+" "+tc.name, func(t *testing.T) {
				err := s.Read(ctx, func(tx *ReadTx) error { return list(tx) })
				if !errors.Is(err, errRowInconsistent) {
					t.Fatalf("error = %v, want errRowInconsistent", err)
				}
			})
		}
	}
}

// TestListRejectsInconsistentRow: a listed row whose JSON body disagrees with
// an extracted key column fails the whole list closed, exactly like the
// single Get (#98 acceptance 2), including both directions of the item's
// nullable conversation_id column. Internal test: the corrupt rows are
// written as raw SQL.
func TestListRejectsInconsistentRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openRaw(t)
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := seedEpoch(ctx, db); err != nil {
		t.Fatalf("seedEpoch: %v", err)
	}
	s := &Store{db: db}

	runBody, err := encode(domain.Run{
		ID: "run-1", ProjectID: "proj-1",
		SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
	})
	if err != nil {
		t.Fatalf("encode run: %v", err)
	}
	conversationBody, err := encode(domain.Conversation{ID: "conv-1", Status: domain.ConversationIdle})
	if err != nil {
		t.Fatalf("encode conversation: %v", err)
	}
	deliveryBody, err := encode(domain.AttentionDelivery{
		ItemID: "item-1", DeviceID: "device-1", Channel: "ntfy", Attempt: 1,
		SubmittedAt: time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC),
		Status:      domain.DeliverySubmitted,
	})
	if err != nil {
		t.Fatalf("encode delivery: %v", err)
	}
	runID := domain.RunID("run-1")
	convID := domain.ConversationID("conv-1")
	newItem := func(bind *domain.ConversationID) domain.AttentionItem {
		t.Helper()
		item, err := domain.NewAttentionItem(domain.AttentionItemInput{
			ID: "item-1", ProjectID: "proj-1",
			Subject: domain.Subject{Type: domain.SubjectRun, ID: "run-1", RunID: &runID},
			Type:    domain.AttentionReadyForFinalReview, Priority: domain.PriorityNormal,
			Reason:            "inconsistent-row fixture",
			RequestedDecision: []domain.Action{domain.ActionOpenPR},
			PRHeadSHA:         "cafebabe",
			PRReference:       &domain.PRReference{Repo: "owner/repo", Number: 123},
			ItemVersion:       1,
			InterruptionClass: domain.InterruptionPlannedGate,
			ConversationID:    bind, Status: domain.StatusOpen,
		}, nil)
		if err != nil {
			t.Fatalf("NewAttentionItem: %v", err)
		}
		return item
	}
	unboundItemBody, err := encode(newItem(nil))
	if err != nil {
		t.Fatalf("encode unbound item: %v", err)
	}
	boundItemBody, err := encode(newItem(&convID))
	if err != nil {
		t.Fatalf("encode bound item: %v", err)
	}
	// The conversation row satisfies the items' conversation_id foreign key
	// and stands as every list's valid sibling alongside the corrupt row.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO conversations (id, entity_version, as_of_revision, body) VALUES ('conv-1', 1, 1, ?)`,
		conversationBody); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}

	cases := []struct {
		name string
		seed func()
		list func(tx *ReadTx) error
	}{
		{
			"run project_id column differs from body",
			func() {
				if _, err := db.ExecContext(ctx,
					`INSERT INTO runs (id, project_id, policy_digest, entity_version, as_of_revision, body) VALUES ('run-1', 'proj-other', 'sha256:policy', 1, 1, ?)`,
					runBody); err != nil {
					t.Fatalf("insert run: %v", err)
				}
			},
			func(tx *ReadTx) error {
				_, err := tx.ListRuns(ctx)
				return err
			},
		},
		{
			"conversation id column differs from body",
			func() {
				if _, err := db.ExecContext(ctx,
					`INSERT INTO conversations (id, entity_version, as_of_revision, body) VALUES ('conv-other', 1, 1, ?)`,
					conversationBody); err != nil {
					t.Fatalf("insert conversation: %v", err)
				}
			},
			func(tx *ReadTx) error {
				_, err := tx.ListConversations(ctx)
				return err
			},
		},
		{
			"item conversation_id column NULL, body bound",
			func() {
				if _, err := db.ExecContext(ctx,
					`INSERT INTO attention_items (id, project_id, conversation_id, entity_version, as_of_revision, body) VALUES ('item-1', 'proj-1', NULL, 1, 1, ?)`,
					boundItemBody); err != nil {
					t.Fatalf("insert item: %v", err)
				}
			},
			func(tx *ReadTx) error {
				_, err := tx.ListAttentionItems(ctx)
				return err
			},
		},
		{
			"item conversation_id column set, body unbound",
			func() {
				if _, err := db.ExecContext(ctx,
					`INSERT INTO attention_items (id, project_id, conversation_id, entity_version, as_of_revision, body) VALUES ('item-1', 'proj-1', 'conv-1', 1, 1, ?)`,
					unboundItemBody); err != nil {
					t.Fatalf("insert item: %v", err)
				}
			},
			func(tx *ReadTx) error {
				_, err := tx.ListAttentionItems(ctx)
				return err
			},
		},
		{
			"delivery attempt column differs from body",
			func() {
				if _, err := db.ExecContext(ctx,
					`INSERT INTO attention_items (id, project_id, conversation_id, entity_version, as_of_revision, body) VALUES ('item-1', 'proj-1', NULL, 1, 1, ?)`,
					unboundItemBody); err != nil {
					t.Fatalf("insert parent item: %v", err)
				}
				if _, err := db.ExecContext(ctx,
					`INSERT INTO attention_deliveries (item_id, device_id, channel, attempt, entity_version, as_of_revision, body) VALUES ('item-1', 'device-1', 'ntfy', 2, 1, 1, ?)`,
					deliveryBody); err != nil {
					t.Fatalf("insert delivery: %v", err)
				}
			},
			func(tx *ReadTx) error {
				_, err := tx.ListAttentionDeliveries(ctx)
				return err
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, reset := range []string{
				`DELETE FROM attention_deliveries`,
				`DELETE FROM attention_items`,
				`DELETE FROM runs`,
				`DELETE FROM conversations WHERE id != 'conv-1'`,
			} {
				if _, err := db.ExecContext(ctx, reset); err != nil {
					t.Fatalf("%s: %v", reset, err)
				}
			}
			tc.seed()
			err := s.Read(ctx, func(tx *ReadTx) error { return tc.list(tx) })
			if !errors.Is(err, errRowInconsistent) {
				t.Fatalf("error = %v, want errRowInconsistent", err)
			}
		})
	}
}

// TestSnapshotRejectsForgedMetadata: the snapshot metadata is store-stamped,
// so values the Puts cannot produce (a zero or negative version or revision,
// or a command row past its write-once entity_version 1) mark a forged or
// corrupt row and must fail the read like any diverging column (#91
// acceptance 3). Internal test: the Put boundary always stamps valid values,
// so the bad rows are written as raw SQL.
func TestSnapshotRejectsForgedMetadata(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openRaw(t)
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := seedEpoch(ctx, db); err != nil {
		t.Fatalf("seedEpoch: %v", err)
	}
	s := &Store{db: db}

	// A minimal valid item with no evidence, so reconstruction succeeds with no
	// approved-recipe set and only the metadata is at fault.
	runID := domain.RunID("run-1")
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "item-1", ProjectID: "proj-1",
		Subject: domain.Subject{Type: domain.SubjectRun, ID: "run-1", RunID: &runID},
		Type:    domain.AttentionReadyForFinalReview, Priority: domain.PriorityNormal,
		Reason:            "forged-metadata fixture",
		RequestedDecision: []domain.Action{domain.ActionOpenPR},
		PRHeadSHA:         "cafebabe",
		PRReference:       &domain.PRReference{Repo: "owner/repo", Number: 123},
		ItemVersion:       1,
		InterruptionClass: domain.InterruptionPlannedGate,
		Status:            domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatalf("NewAttentionItem: %v", err)
	}
	itemBody, err := encode(item)
	if err != nil {
		t.Fatalf("encode item: %v", err)
	}
	prReferenceBody, err := encode(*item.PRReference)
	if err != nil {
		t.Fatalf("encode pr reference: %v", err)
	}
	command, err := domain.NewCommand(domain.CommandInput{
		CommandID: "cmd-1", DeviceID: "device-1", ItemID: item.ID,
		ItemVersion: item.ItemVersion, PRHeadSHA: item.PRHeadSHA,
		ArtifactDigests: item.ArtifactDigests, Action: domain.ActionOpenPR,
	})
	if err != nil {
		t.Fatalf("NewCommand: %v", err)
	}
	commandBody, err := encode(command)
	if err != nil {
		t.Fatalf("encode command: %v", err)
	}
	device := domain.Device{
		ID: "device-1", DisplayName: "Ben's iPhone", Status: domain.DeviceActive,
		PairedAt: time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC),
	}
	deviceBody, err := encode(device)
	if err != nil {
		t.Fatalf("encode device: %v", err)
	}

	seedItem := func(entityVersion, asOfRevision int64) {
		t.Helper()
		if _, err := db.ExecContext(ctx, `DELETE FROM commands`); err != nil {
			t.Fatalf("reset commands: %v", err)
		}
		if _, err := db.ExecContext(ctx, `DELETE FROM attention_item_pr_references`); err != nil {
			t.Fatalf("reset item pr references: %v", err)
		}
		if _, err := db.ExecContext(ctx, `DELETE FROM attention_items`); err != nil {
			t.Fatalf("reset items: %v", err)
		}
		if _, err := db.ExecContext(ctx,
			`INSERT INTO attention_items (id, project_id, conversation_id, item_type, status, entity_version, as_of_revision, body) VALUES ('item-1', 'proj-1', NULL, ?, ?, ?, ?, ?)`,
			item.Type, item.Status, entityVersion, asOfRevision, itemBody); err != nil {
			t.Fatalf("insert item: %v", err)
		}
		if _, err := db.ExecContext(ctx,
			`INSERT INTO attention_item_pr_references (item_id, repo, pr_number, body) VALUES ('item-1', 'owner/repo', 123, ?)`,
			prReferenceBody); err != nil {
			t.Fatalf("insert item pr reference: %v", err)
		}
	}
	seedCommand := func(entityVersion, asOfRevision int64) {
		t.Helper()
		seedItem(1, 1)
		if _, err := db.ExecContext(ctx,
			`INSERT INTO commands (command_id, item_id, item_version, pr_head_sha, device_id, action, entity_version, as_of_revision, body) VALUES ('cmd-1', 'item-1', 1, 'cafebabe', 'device-1', 'open_pr', ?, ?, ?)`,
			entityVersion, asOfRevision, commandBody); err != nil {
			t.Fatalf("insert command: %v", err)
		}
	}
	readItem := func() error {
		return s.Read(ctx, func(tx *ReadTx) error {
			_, _, err := tx.GetAttentionItemSnapshot(ctx, "item-1")
			return err
		})
	}
	readCommand := func() error {
		return s.Read(ctx, func(tx *ReadTx) error {
			_, _, err := tx.GetCommandSnapshot(ctx, "cmd-1")
			return err
		})
	}
	seedDevice := func(entityVersion, asOfRevision int64) {
		t.Helper()
		if _, err := db.ExecContext(ctx, `DELETE FROM devices`); err != nil {
			t.Fatalf("reset devices: %v", err)
		}
		if _, err := db.ExecContext(ctx,
			`INSERT INTO devices (id, status, entity_version, as_of_revision, body) VALUES ('device-1', 'active', ?, ?, ?)`,
			entityVersion, asOfRevision, deviceBody); err != nil {
			t.Fatalf("insert device: %v", err)
		}
	}
	readDevice := func() error {
		return s.Read(ctx, func(tx *ReadTx) error {
			_, _, err := tx.GetDeviceSnapshot(ctx, "device-1")
			return err
		})
	}

	// Control rows prove the fixtures themselves reconstruct, so the forged
	// cases below fail on the metadata alone.
	seedItem(1, 1)
	if err := readItem(); err != nil {
		t.Fatalf("control item read: %v", err)
	}
	seedCommand(1, 1)
	if err := readCommand(); err != nil {
		t.Fatalf("control command read: %v", err)
	}
	seedDevice(1, 1)
	if err := readDevice(); err != nil {
		t.Fatalf("control device read: %v", err)
	}

	cases := []struct {
		name string
		seed func()
		read func() error
	}{
		{"item entity_version zero", func() { seedItem(0, 1) }, readItem},
		{"item entity_version negative", func() { seedItem(-1, 1) }, readItem},
		{"item as_of_revision zero", func() { seedItem(1, 0) }, readItem},
		{"item as_of_revision negative", func() { seedItem(1, -1) }, readItem},
		{"command entity_version zero", func() { seedCommand(0, 1) }, readCommand},
		{"command entity_version past write-once", func() { seedCommand(2, 1) }, readCommand},
		{"command as_of_revision zero", func() { seedCommand(1, 0) }, readCommand},
		{"command as_of_revision negative", func() { seedCommand(1, -1) }, readCommand},
		{"device entity_version zero", func() { seedDevice(0, 1) }, readDevice},
		{"device entity_version negative", func() { seedDevice(-1, 1) }, readDevice},
		{"device as_of_revision zero", func() { seedDevice(1, 0) }, readDevice},
		{"device as_of_revision negative", func() { seedDevice(1, -1) }, readDevice},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.seed()
			if err := tc.read(); !errors.Is(err, errRowInconsistent) {
				t.Fatalf("error = %v, want errRowInconsistent", err)
			}
		})
	}
}

// TestPutAttentionItemPreNoticeRowConverges is the #222 upgrade-boundary
// guard for idempotent replay: a row persisted before commit_plan_notice
// existed carries this item's exact content in different bytes (the stored
// body has no such key, the fresh encoding renders an explicit null), so the
// raw-byte fast path misses and the canonical compare must converge the
// unchanged replay instead of rejecting it as a same-version rewrite.
// Internal test: the legacy row is written as raw SQL past the Put boundary.
func TestPutAttentionItemPreNoticeRowConverges(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openRaw(t)
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := seedEpoch(ctx, db); err != nil {
		t.Fatalf("seedEpoch: %v", err)
	}
	s := &Store{db: db}

	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "item-1", ProjectID: "proj-1",
		Subject:           domain.Subject{Type: domain.SubjectRun, ID: "run-1"},
		Type:              domain.AttentionReadyForFinalReview,
		Priority:          domain.PriorityNormal,
		Reason:            "checks are green and the diff is ready",
		RequestedDecision: []domain.Action{domain.ActionOpenPR},
		PRReference:       &domain.PRReference{Repo: "owner/repo", Number: 123},
		ItemVersion:       1,
		InterruptionClass: domain.InterruptionPlannedGate,
		Status:            domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatalf("NewAttentionItem: %v", err)
	}
	body, err := encode(item)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	legacy := strings.Replace(body, `"commit_plan_notice":null,`, "", 1)
	if legacy == body {
		t.Fatal("legacy strip did not apply")
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO attention_items (id, project_id, conversation_id, entity_version, as_of_revision, body) VALUES (?, ?, NULL, 1, 1, ?)`,
		item.ID, item.ProjectID, legacy); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}

	if err := s.Write(ctx, func(tx *WriteTx) error {
		return tx.PutAttentionItem(ctx, item)
	}); err != nil {
		t.Fatalf("idempotent replay against a pre-notice row: %v", err)
	}

	// Converged without churn: the version did not advance and the stored
	// bytes were not rewritten (only a real transition rewrites the row).
	var version int64
	var stored string
	if err := db.QueryRowContext(ctx,
		`SELECT entity_version, body FROM attention_items WHERE id = ?`, item.ID).
		Scan(&version, &stored); err != nil {
		t.Fatalf("read row back: %v", err)
	}
	if version != 1 {
		t.Fatalf("entity_version = %d, want 1 (no churn)", version)
	}
	if stored != legacy {
		t.Fatalf("stored body was rewritten:\ngot  %s\nwant %s", stored, legacy)
	}
}

// TestPutAttentionItemLegacyOffsetExpiresWhenConverges is the #553 read-path
// tolerance: a row the dev CLI persisted before expires_when carried a UTC
// check holds a valid instant in a host-local offset spelling. Read must
// canonicalize it to UTC (so callers see one spelling), an idempotent replay
// must still converge without entity_version churn (the offset and its UTC
// re-encode are the same instant, exactly the commit_plan_notice precedent
// above), and a real transition against the legacy row must validate through
// the canonicalized decode rather than being refused by the write-boundary UTC
// check. Internal test: the offset row is written as raw SQL past the Put
// boundary.
func TestPutAttentionItemLegacyOffsetExpiresWhenConverges(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openRaw(t)
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := seedEpoch(ctx, db); err != nil {
		t.Fatalf("seedEpoch: %v", err)
	}
	s := &Store{db: db}

	expiresUTC := time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC)
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "item-1", ProjectID: "proj-1",
		Subject:           domain.Subject{Type: domain.SubjectRun, ID: "run-1"},
		Type:              domain.AttentionReadyForFinalReview,
		Priority:          domain.PriorityNormal,
		Reason:            "checks are green and the diff is ready",
		RequestedDecision: []domain.Action{domain.ActionOpenPR},
		PRReference:       &domain.PRReference{Repo: "owner/repo", Number: 123},
		ItemVersion:       1,
		InterruptionClass: domain.InterruptionPlannedGate,
		Status:            domain.StatusOpen,
		ExpiresWhen:       &expiresUTC,
	}, nil)
	if err != nil {
		t.Fatalf("NewAttentionItem: %v", err)
	}
	body, err := encode(item)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	// Forge the legacy spelling by rewriting only the expires_when value in the
	// canonical body to the same instant carried in a +02:00 offset.
	loc := time.FixedZone("+0200", 2*60*60)
	utcSpelling := `"expires_when":"` + expiresUTC.Format(time.RFC3339Nano) + `"`
	offsetSpelling := `"expires_when":"` + expiresUTC.In(loc).Format(time.RFC3339Nano) + `"`
	legacy := strings.Replace(body, utcSpelling, offsetSpelling, 1)
	if legacy == body {
		t.Fatalf("offset rewrite did not apply; body = %s", body)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO attention_items (id, project_id, conversation_id, item_type, status, entity_version, as_of_revision, body) VALUES (?, ?, NULL, ?, ?, 1, 1, ?)`,
		item.ID, item.ProjectID, item.Type, item.Status, legacy); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	prReferenceBody, err := encode(*item.PRReference)
	if err != nil {
		t.Fatalf("encode pr reference: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO attention_item_pr_references (item_id, repo, pr_number, body) VALUES (?, ?, ?, ?)`,
		item.ID, item.PRReference.Repo, item.PRReference.Number, prReferenceBody); err != nil {
		t.Fatalf("insert pr reference: %v", err)
	}

	// Read canonicalizes to UTC on both the Get and List paths.
	assertUTC := func(where string, got *time.Time) {
		t.Helper()
		if got == nil {
			t.Fatalf("%s: expires_when is nil, want a value", where)
		}
		if got.Location() != time.UTC {
			t.Errorf("%s: expires_when location = %v, want UTC", where, got.Location())
		}
		if !got.Equal(expiresUTC) {
			t.Errorf("%s: expires_when = %v, want the same instant as %v", where, got, expiresUTC)
		}
	}
	var fromGet domain.AttentionItem
	if err := s.Read(ctx, func(tx *ReadTx) error {
		var err error
		fromGet, err = tx.GetAttentionItem(ctx, item.ID)
		return err
	}); err != nil {
		t.Fatalf("GetAttentionItem: %v", err)
	}
	assertUTC("Get", fromGet.ExpiresWhen)

	var fromList []Snapshotted[domain.AttentionItem]
	if err := s.Read(ctx, func(tx *ReadTx) error {
		var err error
		fromList, err = tx.ListAttentionItems(ctx)
		return err
	}); err != nil {
		t.Fatalf("ListAttentionItems: %v", err)
	}
	if len(fromList) != 1 {
		t.Fatalf("ListAttentionItems returned %d items, want 1", len(fromList))
	}
	assertUTC("List", fromList[0].Value.ExpiresWhen)

	// An idempotent replay converges against the offset row without churn: the
	// canonical re-encode of the decoded row equals the fresh body, so no write
	// and no entity_version advance.
	if err := s.Write(ctx, func(tx *WriteTx) error {
		return tx.PutAttentionItem(ctx, item)
	}); err != nil {
		t.Fatalf("idempotent replay against an offset row: %v", err)
	}
	var version int64
	var stored string
	if err := db.QueryRowContext(ctx,
		`SELECT entity_version, body FROM attention_items WHERE id = ?`, item.ID).
		Scan(&version, &stored); err != nil {
		t.Fatalf("read row back: %v", err)
	}
	if version != 1 {
		t.Fatalf("entity_version = %d, want 1 (no churn)", version)
	}
	if stored != legacy {
		t.Fatalf("an idempotent replay rewrote the stored body:\ngot  %s\nwant %s", stored, legacy)
	}

	// A real transition against the legacy row still validates: the decoded old
	// row is canonicalized before ValidateAttentionItemTransition, so the UTC
	// check the write boundary now enforces cannot refuse the read of a
	// pre-existing offset row.
	advanced := item
	advanced.ItemVersion = 2
	if err := s.Write(ctx, func(tx *WriteTx) error {
		return tx.PutAttentionItem(ctx, advanced)
	}); err != nil {
		t.Fatalf("real transition against an offset row: %v", err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT entity_version FROM attention_items WHERE id = ?`, item.ID).
		Scan(&version); err != nil {
		t.Fatalf("read row back after transition: %v", err)
	}
	if version != 2 {
		t.Fatalf("entity_version after transition = %d, want 2", version)
	}
}
