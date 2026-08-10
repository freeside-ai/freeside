package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/fakepublication"
)

// TestAuthenticateLegacyFakePRReferenceRefutesCorruptHistory enumerates the
// independent authority bindings the migration must prove before it can mint
// a PR-reference anchor from legacy fake-publication rows.
func TestAuthenticateLegacyFakePRReferenceRefutesCorruptHistory(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		mutate func(*legacyFakePRFixture)
		want   bool
	}{
		{name: "exact history", want: true},
		{name: "nondeterministic item id", mutate: func(f *legacyFakePRFixture) {
			f.item.ID = "fake-publication-ready-forged"
		}},
		{name: "foreign task project", mutate: func(f *legacyFakePRFixture) {
			f.task.ProjectID = "project-foreign"
		}},
		{name: "malformed task", mutate: func(f *legacyFakePRFixture) {
			f.taskPayload = []byte(`{"version":"unknown"}`)
		}},
		{name: "tampered terminal binding", mutate: func(f *legacyFakePRFixture) {
			f.tamperBinding = true
		}},
		{name: "foreign intent repository", mutate: func(f *legacyFakePRFixture) {
			f.intent.Repo = "other/repo"
		}},
		{name: "foreign outcome head", mutate: func(f *legacyFakePRFixture) {
			f.outcome.HeadSHA = "foreign-head"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			db := openRaw(t)
			migrateThrough(t, ctx, db, "0036_")
			fixture := newLegacyFakePRFixture(t)
			if tc.mutate != nil {
				tc.mutate(&fixture)
			}
			fixture.refresh(t)
			fixture.seed(t, ctx, db)

			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback() }()
			_, got, err := authenticateLegacyFakePRReference(ctx, tx, fixture.row(t))
			if err != nil {
				t.Fatalf("authenticate: %v", err)
			}
			if got != tc.want {
				t.Fatalf("authenticated = %t, want %t", got, tc.want)
			}
		})
	}
}

type legacyFakePRFixture struct {
	task          fakepublication.Task
	item          domain.AttentionItem
	intent        readyPublicationIntent
	outcome       readyPublicationOutcome
	taskPayload   []byte
	tamperBinding bool
}

func newLegacyFakePRFixture(t *testing.T) legacyFakePRFixture {
	t.Helper()
	runID := domain.RunID("run-refute-legacy-ready")
	fixedTime := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	task := fakepublication.Task{
		Version: fakepublication.TaskVersion, RunID: runID, ProjectID: "project-1",
		StoreEpoch: "epoch-refute", WorkspaceDir: "/tmp/refute-workspace",
		HandoffDir:    "/tmp/refute-handoff",
		HandoffDigest: domain.Digest("sha256:" + strings.Repeat("d", 64)),
		Repo:          "owner/repo", BaseRef: "main", BaseSHA: strings.Repeat("1", 40),
		AllowedPaths:             []string{"daemon/**"},
		RecipeDigest:             domain.Digest("sha256:" + strings.Repeat("e", 64)),
		RecipePath:               ".freeside/verification.yaml",
		TrustProfileDigest:       domain.Digest("sha256:" + strings.Repeat("f", 64)),
		VerificationInvocationID: "verify-refute", PublicationInvocationID: "publish-refute",
		Title: "Refute migration", Body: "Refute migration body",
		CommitDate: fixedTime, CommitDateExplicit: true, StartedAt: fixedTime,
		OperatingMode: fakepublication.OperatingModeAttended,
	}
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: fakepublication.ReadyItemID(runID), ProjectID: task.ProjectID,
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
		Type:    domain.AttentionReadyForFinalReview, Priority: domain.PriorityNormal,
		Reason: "legacy publication ready", RequestedDecision: []domain.Action{domain.ActionOpenPR},
		PRHeadSHA: "cafebabe", PRReference: &domain.PRReference{Repo: task.Repo, Number: 451},
		ItemVersion: 1, InterruptionClass: domain.InterruptionPlannedGate, Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	identity := domain.Digest("sha256:" + strings.Repeat("a", 64))
	return legacyFakePRFixture{
		task: task,
		item: item,
		intent: readyPublicationIntent{
			Identity: identity, InvocationID: task.PublicationInvocationID,
			Repo: task.Repo, BaseRef: task.BaseRef, SourceHeadSHA: item.PRHeadSHA,
			AuthorizationID: domain.Digest("sha256:" + strings.Repeat("c", 64)),
		},
		outcome: readyPublicationOutcome{
			Identity: identity, Repo: task.Repo, BaseRef: task.BaseRef, HeadSHA: item.PRHeadSHA,
			Branch: "freeside/publish/aaaaaaaaaaaaaaaa", PRNumber: 451, EvidenceEligible: true,
		},
	}
}

func (f *legacyFakePRFixture) refresh(t *testing.T) {
	t.Helper()
	if f.taskPayload == nil {
		payload, err := fakepublication.EncodeTask(f.task)
		if err != nil {
			t.Fatalf("encode task: %v", err)
		}
		f.taskPayload = payload
	}
	plainReason, _, bound := fakepublication.ParseTerminalReason(f.item.Reason)
	if bound {
		f.item.Reason = plainReason
	}
	digest, err := fakepublication.TerminalDigestBeforePRReference(f.task, f.item)
	if err != nil {
		t.Fatalf("terminal digest: %v", err)
	}
	f.item.Reason += "\n\n<!-- freeside:fake-publication-terminal=" + string(digest) + " -->"
	if f.tamperBinding {
		f.item.Reason += "tampered"
	}
}

func (f legacyFakePRFixture) seed(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	intentPayload, err := json.Marshal(f.intent)
	if err != nil {
		t.Fatal(err)
	}
	outcomePayload, err := json.Marshal(f.outcome)
	if err != nil {
		t.Fatal(err)
	}
	for _, seed := range []struct {
		query string
		args  []any
	}{
		{
			`INSERT INTO outbox (idempotency_key, kind, payload, status, created_at)
		 VALUES (?, ?, ?, 'dispatched', '2026-08-09T12:00:00Z')`,
			[]any{fakepublication.TaskKey(f.task.RunID), fakepublication.TaskKind, f.taskPayload},
		},
		{
			`INSERT INTO outbox (idempotency_key, kind, payload, status, created_at)
		 VALUES (?, ?, ?, 'dispatched', '2026-08-09T12:01:00Z')`,
			[]any{
				"publish/" + string(f.task.PublicationInvocationID) + "/" + readyPublicationIntentKind,
				readyPublicationIntentKind, intentPayload,
			},
		},
		{
			`INSERT INTO inbox (idempotency_key, kind, payload, created_at)
		 VALUES (?, 'publish.outcome', ?, '2026-08-09T12:02:00Z')`,
			[]any{"publish.outcome/" + string(f.intent.Identity), outcomePayload},
		},
	} {
		if _, err := db.ExecContext(ctx, seed.query, seed.args...); err != nil {
			t.Fatal(err)
		}
	}
}

func (f legacyFakePRFixture) row(t *testing.T) legacyAttentionRow {
	t.Helper()
	item := f.item
	item.PRReference = nil
	body, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	return legacyAttentionRow{
		id: item.ID, projectID: item.ProjectID, entityVersion: item.ItemVersion,
		body: body, itemType: string(item.Type), status: string(item.Status),
	}
}
