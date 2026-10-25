package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

// TestSystemHealthGolden pins the read's shape. A change to these bytes is a
// change to systemHealthVersion.
func TestSystemHealthGolden(t *testing.T) {
	at := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	command := "command-stop"
	blocking, advisory := domain.HealthPostureBlocking, domain.HealthPostureAdvisory
	diagnostic := &domain.HealthDiagnostic{Code: "unattended.stopped", Impairs: domain.ImpairedCapabilityUnattendedAdmission}
	system := domain.Subject{Type: domain.SubjectSystem, ID: "daemon"}
	runID, taskID := domain.RunID("run-closed"), domain.TaskID("task-closed")
	holds, advisoryAdmission := systemHealthHolds, systemHealthAdvisory
	superseded, closed := systemHealthSuperseded, systemHealthWorkflowClosed
	reason := `item "system-health-unattended-stopped-command-stop": blocking system health`
	got, err := json.MarshalIndent(systemHealth{
		Version: systemHealthVersion, AsOfRevision: 41, AsOf: at,
		OperatorStop: &systemHealthStop{CommandID: &command, Reason: "operator stop", OccurredAt: at.Add(-time.Hour)},
		Items: []systemHealthItem{
			{
				ID: "system-health-unattended-stopped-command-stop", ProjectID: "project-system", Subject: system,
				Posture: blocking, HealthDiagnostic: diagnostic,
				RequestedDecision: []domain.Action{domain.ActionResumeUnattended}, CreatedAt: new(at.Add(-time.Hour)),
				Admission: &holds, HoldReason: &reason,
			},
			{
				ID: "system-health-advisory", ProjectID: "project-system", Subject: system, Posture: advisory,
				RequestedDecision: []domain.Action{domain.ActionAcknowledge}, CreatedAt: new(at.Add(-50 * time.Minute)),
				Admission: &advisoryAdmission,
			},
			{
				ID: "system-health-waived", ProjectID: "project-system", Subject: system, Posture: blocking,
				RequestedDecision: []domain.Action{domain.ActionAcknowledge}, CreatedAt: new(at.Add(-40 * time.Minute)),
				Supersession: &domain.BlockingSupersession{Kind: domain.SupersessionBackupEncryptionWaiver, RepositoryID: 1296269},
				Admission:    &superseded,
			},
			{
				ID: "system-health-closed-workflow", ProjectID: "freeside",
				Subject:   domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID, TaskID: &taskID},
				Posture:   blocking,
				CreatedAt: new(at.Add(-30 * time.Minute)), RequestedDecision: []domain.Action{domain.ActionAcknowledge},
				Admission: &closed,
			},
			{
				ID: "system-health-unexplained", ProjectID: "project-system", Subject: system, Posture: blocking,
				RequestedDecision: []domain.Action{domain.ActionAcknowledge}, CreatedAt: new(at.Add(-20 * time.Minute)),
				AdmissionError: &taskRecordError{Kind: taskRecordErrorIntegrity, Message: "item unexplained"},
			},
		},
		RefusedRows: []systemHealthRefusedRow{{
			ID:    "execution-failure-evidence",
			Error: &taskRecordError{Kind: taskRecordErrorUnapprovedRecipe, Message: "recipe not approved"},
		}},
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden.Assert(t, "system_health", append(got, '\n'))
}

// TestClassifySystemHealthItem walks the gate's exemptions in the order the
// classifier reads them.
func TestClassifySystemHealthItem(t *testing.T) {
	blocking, advisory := domain.HealthPostureBlocking, domain.HealthPostureAdvisory
	item := func(id domain.ItemID, posture domain.HealthPosture, supersession *domain.BlockingSupersession) domain.AttentionItem {
		return domain.AttentionItem{ID: id, Type: domain.AttentionSystemHealth, Posture: &posture, BlockingSupersession: supersession}
	}
	waiver := &domain.BlockingSupersession{Kind: domain.SupersessionBackupEncryptionWaiver, RepositoryID: 7}
	held := item("held", blocking, waiver)
	gate := domain.UnattendedOperationGate{Blocking: []domain.UnattendedBlockingItem{
		{Item: held, Err: errors.New("held: waiver not configured")},
	}}
	open := func() error { return nil }
	unread := func() error { t.Error("an item the gate listed, or an advisory one, read its workflow"); return nil }
	diskErr := errors.New("disk I/O error")
	for _, tc := range []struct {
		name         string
		item         domain.AttentionItem
		workflowOpen func() error
		want         systemHealthAdmission
		wantReason   string
		wantKind     taskRecordErrorKind
	}{
		{"listed by the gate", held, unread, systemHealthHolds, "held: waiver not configured", ""},
		{"advisory", item("advisory", advisory, nil), unread, systemHealthAdvisory, "", ""},
		{
			"closed workflow", item("closed", blocking, nil), func() error { return store.ErrTaskWorkflowClosed },
			systemHealthWorkflowClosed, "", "",
		},
		{"superseded", item("waived", blocking, waiver), open, systemHealthSuperseded, "", ""},
		{"workflow unreadable", item("unread", blocking, nil), func() error { return diskErr }, "", "", taskRecordErrorOther},
		{"unexplained", item("unexplained", blocking, nil), open, "", "", taskRecordErrorIntegrity},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, reason, err := classifySystemHealthItem(gate, tc.item, tc.workflowOpen)
			if tc.wantKind != "" {
				if err == nil || newTaskRecordError(err).Kind != tc.wantKind {
					t.Fatalf("err = %v, want kind %s", err, tc.wantKind)
				}
				return
			}
			if err != nil || got != tc.want || !got.valid() {
				t.Fatalf("admission = %q, err = %v, want %q", got, err, tc.want)
			}
			if (reason == nil) != (tc.wantReason == "") || (reason != nil && *reason != tc.wantReason) {
				t.Fatalf("reason = %v, want %q", reason, tc.wantReason)
			}
		})
	}
	for _, admission := range allSystemHealthAdmissions {
		if !admission.valid() {
			t.Errorf("%q is listed but not valid", admission)
		}
	}
	if systemHealthAdmission("").valid() {
		t.Error("the zero value is valid")
	}
}

// TestInspectHealthServedEqualsDirect reads the root's health items through
// a daemon, stops it, and expects the same items from the file: the seed's
// blocking notice holds, an advisory notice does not, and a closed notice is
// not listed. The daemon raises notices of its own at start, so items are
// found by id.
func TestInspectHealthServedEqualsDirect(t *testing.T) {
	t.Parallel()
	h, dbPath := startSeededDaemon(t)
	ctx := t.Context()
	putSystemHealthItem(t, h.store, "system-health-advisory", domain.HealthPostureAdvisory, nil)
	decided := putSystemHealthItem(t, h.store, "system-health-decided", domain.HealthPostureBlocking, nil)
	closed, err := decided.ConcludeByDaemon(domain.ConcludedByDaemon, time.Now().UTC(), "Cleared.", nil)
	if err != nil {
		t.Fatal(err)
	}
	closed.ItemVersion++
	if err := h.store.Write(ctx, func(tx *store.WriteTx) error { return tx.PutAttentionItem(ctx, closed) }); err != nil {
		t.Fatal(err)
	}
	served := inspect[systemHealth](t, ctx, "health", "-db", dbPath)
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	direct := inspect[systemHealth](t, ctx, "health", "-db", dbPath)
	if !bytes.Equal(comparableHealth(t, direct), comparableHealth(t, served)) {
		t.Fatalf("direct read differs from the served one\ndirect: %s\nserved: %s",
			comparableHealth(t, direct), comparableHealth(t, served))
	}
	if direct.Version != systemHealthVersion || direct.OperatorStop != nil || direct.AdmissionError != nil ||
		len(direct.RefusedRows) != 0 || healthItem(direct, "system-health-decided") != nil {
		t.Fatalf("health = %s", comparableHealth(t, direct))
	}
	seeded := healthItem(direct, "system-health-seed-fixture")
	if seeded == nil || !equalPointers(seeded.Admission, new(systemHealthHolds)) ||
		seeded.HoldReason == nil || !strings.Contains(*seeded.HoldReason, domain.ErrBlockingSystemHealth.Error()) {
		t.Errorf("seeded item = %+v", seeded)
	}
	advisory := healthItem(direct, "system-health-advisory")
	if advisory == nil || !equalPointers(advisory.Admission, new(systemHealthAdvisory)) ||
		advisory.HoldReason != nil || advisory.HealthDiagnostic == nil {
		t.Errorf("advisory item = %+v", advisory)
	}
	// The reads above moved nothing.
	if after := inspect[systemHealth](t, ctx, "health", "-db", dbPath); after.AsOfRevision != direct.AsOfRevision {
		t.Fatalf("revision moved from %d to %d across reads", direct.AsOfRevision, after.AsOfRevision)
	}
}

// TestInspectHealthReadsStoppedRootWithRecipeEvidence: with no
// -approved-recipe, a row whose evidence names the daemon's recipe is
// refused, listed by id beside the health items it does not hide, and the
// stderr note says what the read needs. With the digest the read is clean.
func TestInspectHealthReadsStoppedRootWithRecipeEvidence(t *testing.T) {
	t.Parallel()
	recipe := domain.Digest("sha256:" + strings.Repeat("e", 64))
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	h := startDaemon(t, config{
		DBPath: dbPath, ListenAddr: "127.0.0.1:0", SeedFixture: "representative",
		Environment: environmentEphemeral, ApprovedRecipes: map[domain.Digest]bool{recipe: true},
	})
	const evidence = domain.ItemID("execution-failure-evidence")
	putRecipeEvidenceItem(t, h.store, recipe, fixtureActiveRunID, evidence)
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	args := []string{"health", "-db", dbPath}
	var stdout, stderr bytes.Buffer
	if err := runInspectCommand(t.Context(), args, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	var refused systemHealth
	if err := strictjson.Decode(stdout.Bytes(), &refused, strictjson.RejectInvalidUTF8, strictjson.NoLimit); err != nil {
		t.Fatal(err)
	}
	if len(refused.RefusedRows) != 1 || refused.RefusedRows[0].ID != evidence ||
		refused.RefusedRows[0].Error.Kind != taskRecordErrorUnapprovedRecipe {
		t.Fatalf("refused rows = %+v", refused.RefusedRows)
	}
	if healthItem(refused, "system-health-seed-fixture") == nil {
		t.Fatalf("items = %+v", refused.Items)
	}
	if note := stderr.String(); !strings.Contains(note, "-approved-recipe") || !strings.Contains(note, "approved 0 ") {
		t.Fatalf("stderr = %q, want the approved-recipe note", note)
	}

	stdout.Reset()
	stderr.Reset()
	if err := runInspectCommand(t.Context(), append(args, "-approved-recipe", string(recipe)), &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	var approved systemHealth
	if err := strictjson.Decode(stdout.Bytes(), &approved, strictjson.RejectInvalidUTF8, strictjson.NoLimit); err != nil {
		t.Fatal(err)
	}
	if len(approved.RefusedRows) != 0 || len(approved.Items) != len(refused.Items) || stderr.Len() != 0 {
		t.Fatalf("approved read = %s, stderr = %q", comparableHealth(t, approved), stderr.String())
	}
}

// TestSystemHealthAgreesWithTheGate reads a store the way admission does:
// a supersession the store's waiver covers reads superseded, one it does not
// cover holds with the gate's reason, and a system_health row the store
// refuses fails the gate, so the read lists the refused row and leaves every
// item's admission null instead of guessing.
func TestSystemHealthAgreesWithTheGate(t *testing.T) {
	ctx := t.Context()
	recipe := domain.Digest("sha256:" + strings.Repeat("e", 64))
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	waived := int64(7)
	st, err := store.Open(ctx, dbPath, store.Options{
		BackupEncryptionWaiverRepositoryID: &waived, ApprovedRecipes: map[domain.Digest]bool{recipe: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	supersededBy := func(repository int64) func(*domain.AttentionItemInput) {
		return func(input *domain.AttentionItemInput) {
			input.BlockingSupersession = &domain.BlockingSupersession{
				Kind: domain.SupersessionBackupEncryptionWaiver, RepositoryID: repository,
			}
		}
	}
	putSystemHealthItem(t, st, "system-health-waived", domain.HealthPostureBlocking, supersededBy(waived))
	putSystemHealthItem(t, st, "system-health-unwaived", domain.HealthPostureBlocking, supersededBy(8))
	health, err := readSystemHealthFrom(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	if item := healthItem(health, "system-health-waived"); item == nil ||
		!equalPointers(item.Admission, new(systemHealthSuperseded)) || item.HoldReason != nil {
		t.Errorf("waived item = %+v", item)
	}
	if item := healthItem(health, "system-health-unwaived"); item == nil ||
		!equalPointers(item.Admission, new(systemHealthHolds)) || item.HoldReason == nil ||
		!strings.Contains(*item.HoldReason, domain.ErrWaiverNotConfigured.Error()) {
		t.Errorf("unwaived item = %+v", item)
	}

	putSystemHealthItem(t, st, "system-health-evidence", domain.HealthPostureBlocking,
		func(input *domain.AttentionItemInput) {
			input.EvidenceSnapshot = []domain.Artifact{recipeEvidenceArtifact(t, recipe, "system-health-evidence")}
		}, recipe)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = store.Open(ctx, dbPath, store.Options{BackupEncryptionWaiverRepositoryID: &waived})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Error(err)
		}
	})
	health, err = readSystemHealthFrom(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	if health.AdmissionError == nil || health.OperatorStop != nil || len(health.RefusedRows) != 1 ||
		health.RefusedRows[0].ID != "system-health-evidence" || len(health.Items) != 2 {
		t.Fatalf("health = %s", comparableHealth(t, health))
	}
	for _, item := range health.Items {
		if item.Admission != nil || item.HoldReason != nil || item.AdmissionError != nil {
			t.Errorf("item %s admission = %v, hold reason = %v, error = %+v",
				item.ID, item.Admission, item.HoldReason, item.AdmissionError)
		}
	}
}

// putSystemHealthItem writes a daemon-subject system_health item, as the
// daemon's stop notices are written; edit, when set, changes the input first.
// The store must approve any recipe the edited evidence names.
func putSystemHealthItem(
	t *testing.T, st *store.Store, id domain.ItemID, posture domain.HealthPosture,
	edit func(*domain.AttentionItemInput), approved ...domain.Digest,
) domain.AttentionItem {
	t.Helper()
	at := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	input := domain.AttentionItemInput{
		ID: id, ProjectID: "project-system", Subject: domain.Subject{Type: domain.SubjectSystem, ID: "daemon"},
		Type: domain.AttentionSystemHealth, Priority: domain.PriorityHigh, Reason: "Health notice.",
		RequestedDecision: []domain.Action{domain.ActionAcknowledge}, ItemVersion: 1, Status: domain.StatusOpen,
		InterruptionClass: domain.InterruptionExceptional, CreatedAt: &at, Posture: &posture,
		HealthDiagnostic: &domain.HealthDiagnostic{Code: "test.notice", Impairs: domain.ImpairedCapabilityNone},
	}
	if edit != nil {
		edit(&input)
	}
	recipes := map[domain.Digest]bool{}
	for _, recipe := range approved {
		recipes[recipe] = true
	}
	item, err := domain.NewAttentionItem(input, recipes)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(t.Context(), func(tx *store.WriteTx) error {
		return tx.PutAttentionItem(t.Context(), item)
	}); err != nil {
		t.Fatal(err)
	}
	return item
}

func healthItem(health systemHealth, id domain.ItemID) *systemHealthItem {
	for index := range health.Items {
		if health.Items[index].ID == id {
			return &health.Items[index]
		}
	}
	return nil
}

// comparableHealth is the read without its wall-clock stamp and revision,
// which differ between two reads of unchanged state.
func comparableHealth(t *testing.T, health systemHealth) []byte {
	t.Helper()
	health.AsOf, health.AsOfRevision = time.Time{}, 0
	out, err := json.Marshal(health)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
