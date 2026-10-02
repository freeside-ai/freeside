package store_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

var diminishingFactsAt = time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)

// The drift item the tests write sits on its own run: the seeded decision's
// item already holds its round's id, and an audit is written only for a run's
// latest round, so the seeded run has no free audited round.
const driftFactsRun domain.RunID = "run-facts-drift"

const driftFactsRound = 1

// diminishingFactsFixture is a seeded run whose round 2 parked on a
// diminishing item (cause fixed_recurrence, no card facts) and was audited,
// beside driftFactsRun, which carries two audited rounds and no item.
type diminishingFactsFixture struct {
	st            *store.Store
	decision      store.ReviewDiminishingDecision
	audit         domain.DriftAudit // of driftFactsRun, driftFactsRound
	laterAudit    domain.DriftAudit // of driftFactsRun, the round after
	decisionAudit domain.DriftAudit // of the seeded run's parked round
}

func seedDiminishingFacts(
	t *testing.T, path string, runID domain.RunID, action domain.Action,
) diminishingFactsFixture {
	t.Helper()
	ctx := context.Background()
	st, decision := seedReviewDiminishingDecision(t, path, runID, diminishingFactsAt, action)
	input := driftAuditInputFor(runID, decision.Binding.Round, domain.DriftVerdictOverHardened, "finding-a")
	input.ResolvedPolicyDigest = decision.Binding.PolicyDigest
	fx := diminishingFactsFixture{
		st: st, decision: decision, decisionAudit: newDriftAudit(t, input),
		audit:      driftAuditFor(t, driftFactsRun, driftFactsRound, domain.DriftVerdictOverHardened),
		laterAudit: driftAuditFor(t, driftFactsRun, driftFactsRound+1, domain.DriftVerdictOverHardened),
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutDriftAudit(ctx, fx.decisionAudit); err != nil {
			return err
		}
		if err := tx.PutRun(ctx, domain.Run{
			ID: driftFactsRun, ProjectID: decision.Item.ProjectID,
			SpecDigest: adjSpecDigest, PolicyDigest: adjPolicyDigest,
		}); err != nil {
			return err
		}
		for _, audit := range []domain.DriftAudit{fx.audit, fx.laterAudit} {
			if err := putDriftRound(ctx, t, tx, driftFactsRun, audit.Round); err != nil {
				return err
			}
			if err := tx.PutDriftAudit(ctx, audit); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed drift audits: %v", err)
	}
	return fx
}

// driftFactsOf projects an audit the way a producer would.
func driftFactsOf(audit domain.DriftAudit) domain.ReviewDiminishingFacts {
	return domain.ReviewDiminishingFacts{
		Cause: domain.ReviewDiminishingDriftAudit,
		DriftAudit: &domain.DriftAuditFacts{
			AuditDigest: audit.Digest, Verdict: audit.Verdict, Confidence: audit.Confidence,
			Explanation: audit.Explanation,
			Reversals:   append([]domain.DriftReversal{}, audit.Reversals...),
		},
	}
}

func (fx diminishingFactsFixture) driftFacts() domain.ReviewDiminishingFacts {
	return driftFactsOf(fx.audit)
}

func (fx diminishingFactsFixture) driftItemID() domain.ItemID {
	return store.ReviewDiminishingItemID(driftFactsRun, driftFactsRound)
}

// driftBinding is the seeded reason binding re-bound to driftFactsRun's round
// and the drift_audit cause.
func (fx diminishingFactsFixture) driftBinding() store.ReviewDiminishingBinding {
	binding := fx.decision.Binding
	binding.RunID, binding.Round = driftFactsRun, driftFactsRound
	binding.ItemID = fx.driftItemID()
	binding.Cause = domain.ReviewDiminishingDriftAudit
	return binding
}

// driftItem builds driftFactsRound's diminishing item with the given reason
// binding and card facts.
func (fx diminishingFactsFixture) driftItem(
	t *testing.T, binding store.ReviewDiminishingBinding, facts domain.ReviewDiminishingFacts,
) domain.AttentionItem {
	t.Helper()
	reason, err := store.ReviewDiminishingReason(binding)
	if err != nil {
		t.Fatal(err)
	}
	seeded := fx.decision.Item
	run := driftFactsRun
	subject := seeded.Subject
	subject.ID, subject.RunID = domain.SubjectID(run), &run
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: fx.driftItemID(), ProjectID: seeded.ProjectID, Subject: subject,
		Type: domain.AttentionReviewDiminishing, Priority: domain.PriorityNormal, Reason: reason,
		RequestedDecision: seeded.RequestedDecision, PRHeadSHA: seeded.PRHeadSHA,
		YieldHistory: seeded.YieldHistory, ReviewDiminishing: &facts, ItemVersion: 1,
		InterruptionClass: domain.InterruptionPlannedGate, CreatedAt: seeded.CreatedAt,
		Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return item
}

func tryPutItem(st *store.Store, item domain.AttentionItem) error {
	ctx := context.Background()
	return st.Write(ctx, func(tx *store.WriteTx) error { return tx.PutAttentionItem(ctx, item) })
}

// readItemBothTiers reads the item through the snapshot tier and the record
// tier, the two reconstructions that re-run the card-fact gate.
func readItemBothTiers(st *store.Store, id domain.ItemID) (snapshot, record domain.AttentionItem, err error) {
	ctx := context.Background()
	err = st.Read(ctx, func(tx *store.ReadTx) error {
		var readErr error
		if snapshot, readErr = tx.GetAttentionItem(ctx, id); readErr != nil {
			return readErr
		}
		record, readErr = tx.GetAttentionItemRecord(ctx, id)
		return readErr
	})
	return snapshot, record, err
}

func TestReviewDiminishingCauseFactsLoadThroughTheDecision(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store.db")
	fx := seedDiminishingFacts(t, path, "run-cause-facts", "")
	defer func() { _ = fx.st.Close() }()

	// A later version may attach the facts to an item stored without them.
	next := fx.decision.Item
	next.ReviewDiminishing = &domain.ReviewDiminishingFacts{Cause: fx.decision.Binding.Cause}
	next.ItemVersion++
	if err := tryPutItem(fx.st, next); err != nil {
		t.Fatalf("put cause facts: %v", err)
	}
	snapshot, record, err := readItemBothTiers(fx.st, next.ID)
	if err != nil {
		t.Fatalf("read cause facts: %v", err)
	}
	for tier, got := range map[string]domain.AttentionItem{"snapshot": snapshot, "record": record} {
		if !reflect.DeepEqual(got.ReviewDiminishing, next.ReviewDiminishing) {
			t.Fatalf("%s facts = %+v, want %+v", tier, got.ReviewDiminishing, next.ReviewDiminishing)
		}
	}
	if err := fx.st.Read(ctx, func(tx *store.ReadTx) error {
		decision, err := tx.ReviewDiminishingDecision(ctx, next.ID)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(decision.Item.ReviewDiminishing, next.ReviewDiminishing) {
			t.Fatalf("decision facts = %+v", decision.Item.ReviewDiminishing)
		}
		return nil
	}); err != nil {
		t.Fatalf("decision load with cause facts: %v", err)
	}
}

// TestReviewDiminishingFactsReplayOverAnOlderRowConverges is the upgrade
// case: a producer that starts stamping the facts replays an open item stored
// without them. Card facts never force a new version, so the replay converges
// and the stored item keeps none.
func TestReviewDiminishingFactsReplayOverAnOlderRowConverges(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "store.db")
	fx := seedDiminishingFacts(t, path, "run-facts-replay", "")
	defer func() { _ = fx.st.Close() }()

	replay := fx.decision.Item
	replay.ReviewDiminishing = &domain.ReviewDiminishingFacts{Cause: fx.decision.Binding.Cause}
	if err := tryPutItem(fx.st, replay); err != nil {
		t.Fatalf("same-version replay with facts: %v", err)
	}
	snapshot, _, err := readItemBothTiers(fx.st, replay.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ReviewDiminishing != nil || snapshot.ItemVersion != fx.decision.Item.ItemVersion {
		t.Fatalf("stored item = version %d facts %+v, want unchanged",
			snapshot.ItemVersion, snapshot.ReviewDiminishing)
	}
	// The replay is still gated: facts the reason does not bind are refused.
	replay.ReviewDiminishing = &domain.ReviewDiminishingFacts{Cause: domain.ReviewDiminishingLowValue}
	if err := tryPutItem(fx.st, replay); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("replay with an unbound cause = %v, want ErrParentKeyMismatch", err)
	}
}

func TestReviewDiminishingDriftFactsRoundTrip(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "store.db")
	fx := seedDiminishingFacts(t, path, "run-drift-facts", "")
	defer func() { _ = fx.st.Close() }()

	item := fx.driftItem(t, fx.driftBinding(), fx.driftFacts())
	if err := tryPutItem(fx.st, item); err != nil {
		t.Fatalf("put drift facts: %v", err)
	}
	// An identical replay converges.
	if err := tryPutItem(fx.st, item); err != nil {
		t.Fatalf("replay drift facts: %v", err)
	}
	snapshot, record, err := readItemBothTiers(fx.st, item.ID)
	if err != nil {
		t.Fatalf("read drift facts: %v", err)
	}
	for tier, got := range map[string]domain.AttentionItem{"snapshot": snapshot, "record": record} {
		if !reflect.DeepEqual(got.ReviewDiminishing, item.ReviewDiminishing) {
			t.Fatalf("%s facts = %+v, want %+v", tier, got.ReviewDiminishing, item.ReviewDiminishing)
		}
	}
}

func TestReviewDiminishingFactsWriteRefusals(t *testing.T) {
	t.Parallel()
	drift := func(mutate func(*domain.DriftAuditFacts)) func(
		*testing.T, diminishingFactsFixture,
	) domain.AttentionItem {
		return func(t *testing.T, fx diminishingFactsFixture) domain.AttentionItem {
			t.Helper()
			facts := fx.driftFacts()
			mutate(facts.DriftAudit)
			return fx.driftItem(t, fx.driftBinding(), facts)
		}
	}
	rebound := func(mutate func(*store.ReviewDiminishingBinding)) func(
		*testing.T, diminishingFactsFixture,
	) domain.AttentionItem {
		return func(t *testing.T, fx diminishingFactsFixture) domain.AttentionItem {
			t.Helper()
			binding := fx.driftBinding()
			mutate(&binding)
			return fx.driftItem(t, binding, fx.driftFacts())
		}
	}
	copied := func(audit func(diminishingFactsFixture) domain.DriftAudit) func(
		*testing.T, diminishingFactsFixture,
	) domain.AttentionItem {
		return func(t *testing.T, fx diminishingFactsFixture) domain.AttentionItem {
			t.Helper()
			return fx.driftItem(t, fx.driftBinding(), driftFactsOf(audit(fx)))
		}
	}
	onItem := func(mutate func(*domain.AttentionItem)) func(
		*testing.T, diminishingFactsFixture,
	) domain.AttentionItem {
		return func(t *testing.T, fx diminishingFactsFixture) domain.AttentionItem {
			t.Helper()
			item := fx.driftItem(t, fx.driftBinding(), fx.driftFacts())
			mutate(&item)
			return item
		}
	}
	for _, tc := range []struct {
		name string
		item func(*testing.T, diminishingFactsFixture) domain.AttentionItem
		want error
	}{
		{
			name: "cause differs from the reason binding", want: domain.ErrParentKeyMismatch,
			item: func(_ *testing.T, fx diminishingFactsFixture) domain.AttentionItem {
				next := fx.decision.Item
				next.ReviewDiminishing = &domain.ReviewDiminishingFacts{Cause: domain.ReviewDiminishingLowValue}
				next.ItemVersion++
				return next
			},
		},
		{
			name: "drift cause on a reason bound to another cause", want: domain.ErrParentKeyMismatch,
			item: rebound(func(binding *store.ReviewDiminishingBinding) {
				binding.Cause = domain.ReviewDiminishingGrowthWithoutBlockers
			}),
		},
		{
			name: "reason binds another item", want: domain.ErrParentKeyMismatch,
			item: rebound(func(binding *store.ReviewDiminishingBinding) { binding.ItemID = "another-item" }),
		},
		{
			name: "reason carries no binding", want: domain.ErrParentKeyMismatch,
			item: onItem(func(item *domain.AttentionItem) {
				item.Reason = "review escalated before publication"
			}),
		},
		{
			name: "unknown audit digest", want: domain.ErrParentKeyMismatch,
			item: drift(func(facts *domain.DriftAuditFacts) {
				facts.AuditDigest = "sha256:5555555555555555555555555555555555555555555555555555555555555555"
			}),
		},
		{
			// A real audit, copied faithfully, but of a run the item is not on.
			name: "audit of another run", want: domain.ErrParentKeyMismatch,
			item: copied(func(fx diminishingFactsFixture) domain.DriftAudit { return fx.decisionAudit }),
		},
		{
			name: "audit of another round", want: domain.ErrParentKeyMismatch,
			item: copied(func(fx diminishingFactsFixture) domain.DriftAudit { return fx.laterAudit }),
		},
		{
			name: "reason binds another run", want: domain.ErrParentKeyMismatch,
			item: rebound(func(binding *store.ReviewDiminishingBinding) { binding.RunID = "run-facts-refusal" }),
		},
		{
			name: "reason binds another round", want: domain.ErrParentKeyMismatch,
			item: rebound(func(binding *store.ReviewDiminishingBinding) { binding.Round++ }),
		},
		{
			name: "reason binds another head", want: domain.ErrParentKeyMismatch,
			item: rebound(func(binding *store.ReviewDiminishingBinding) { binding.HeadSHA = "another-head" }),
		},
		{
			// The reason and facts agree with each other and with a real
			// audit, but the card belongs to another run.
			name: "item on another run", want: domain.ErrParentKeyMismatch,
			item: onItem(func(item *domain.AttentionItem) {
				run := domain.RunID("run-facts-refusal")
				item.Subject.ID, item.Subject.RunID = domain.SubjectID(run), &run
			}),
		},
		{
			name: "verdict differs", want: domain.ErrParentKeyMismatch,
			item: drift(func(facts *domain.DriftAuditFacts) {
				facts.Verdict = domain.DriftVerdictStuck
				facts.Reversals = []domain.DriftReversal{}
			}),
		},
		{
			name: "confidence differs", want: domain.ErrParentKeyMismatch,
			item: drift(func(facts *domain.DriftAuditFacts) { facts.Confidence = domain.ConfidenceMedium }),
		},
		{
			name: "explanation differs", want: domain.ErrParentKeyMismatch,
			item: drift(func(facts *domain.DriftAuditFacts) { facts.Explanation = "another explanation" }),
		},
		{
			name: "reversal differs", want: domain.ErrParentKeyMismatch,
			item: drift(func(facts *domain.DriftAuditFacts) { facts.Reversals[0].Undo = "undo something else" }),
		},
		{
			name: "reversal added", want: domain.ErrParentKeyMismatch,
			item: drift(func(facts *domain.DriftAuditFacts) {
				facts.Reversals = append(facts.Reversals, domain.DriftReversal{
					FindingID: facts.Reversals[0].FindingID + "-later", Undo: "undo another fix", Rationale: "not in the audit",
				})
			}),
		},
		{
			name: "simplification round promised", want: store.ErrReviewDiminishingSimplificationUnproven,
			item: drift(func(facts *domain.DriftAuditFacts) { facts.SimplificationOnContinue = true }),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "store.db")
			fx := seedDiminishingFacts(t, path, "run-facts-refusal", "")
			defer func() { _ = fx.st.Close() }()
			item := tc.item(t, fx)
			err := tryPutItem(fx.st, item)
			// A missing audit must not read as a missing item.
			if !errors.Is(err, tc.want) || errors.Is(err, store.ErrNotFound) {
				t.Fatalf("put = %v, want %v and not ErrNotFound", err, tc.want)
			}
			if item.ID != fx.driftItemID() {
				return
			}
			if _, _, err := readItemBothTiers(fx.st, item.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("refused item was stored: read = %v", err)
			}
		})
	}
}

// tamperStore closes the store, runs the statement on the raw database, and
// reopens it.
func tamperStore(t *testing.T, path string, st *store.Store, query string, args ...any) *store.Store {
	t.Helper()
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	result, err := raw.Exec(query, args...)
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := result.RowsAffected(); err != nil || changed != 1 {
		t.Fatalf("tamper changed %d rows, %v; want 1", changed, err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	return storetest.Open(t, path, store.Options{})
}

func TestReviewDiminishingFactsReadsFailClosed(t *testing.T) {
	t.Parallel()
	setFacts := `UPDATE attention_items SET body = json_set(body, '$.review_diminishing', json(?)) WHERE id = ?`
	rewrite := func(mutate func(*domain.ReviewDiminishingFacts)) func(diminishingFactsFixture) (string, []any) {
		return func(fx diminishingFactsFixture) (string, []any) {
			facts := fx.driftFacts()
			mutate(&facts)
			body, err := json.Marshal(facts)
			if err != nil {
				panic(err)
			}
			return setFacts, []any{string(body), string(fx.driftItemID())}
		}
	}
	for _, tc := range []struct {
		name   string
		tamper func(diminishingFactsFixture) (string, []any)
		want   error
	}{
		{name: "cause rewritten", want: domain.ErrParentKeyMismatch, tamper: rewrite(
			func(facts *domain.ReviewDiminishingFacts) {
				facts.Cause, facts.DriftAudit = domain.ReviewDiminishingLowValue, nil
			})},
		{name: "verdict rewritten", want: domain.ErrParentKeyMismatch, tamper: rewrite(
			func(facts *domain.ReviewDiminishingFacts) {
				facts.DriftAudit.Verdict = domain.DriftVerdictConverged
				facts.DriftAudit.Reversals = []domain.DriftReversal{}
			})},
		{name: "explanation rewritten", want: domain.ErrParentKeyMismatch, tamper: rewrite(
			func(facts *domain.ReviewDiminishingFacts) { facts.DriftAudit.Explanation = "rewritten" })},
		{name: "reversal rewritten", want: domain.ErrParentKeyMismatch, tamper: rewrite(
			func(facts *domain.ReviewDiminishingFacts) { facts.DriftAudit.Reversals[0].Rationale = "rewritten" })},
		{
			name: "simplification round promised", want: store.ErrReviewDiminishingSimplificationUnproven,
			tamper: rewrite(func(facts *domain.ReviewDiminishingFacts) {
				facts.DriftAudit.SimplificationOnContinue = true
			}),
		},
		{name: "facts no longer valid", want: domain.ErrCardFactInconsistent, tamper: rewrite(
			func(facts *domain.ReviewDiminishingFacts) { facts.DriftAudit = nil })},
		{
			name: "audit removed", want: domain.ErrParentKeyMismatch,
			tamper: func(fx diminishingFactsFixture) (string, []any) {
				return `DELETE FROM drift_audits WHERE content_digest = ?`, []any{string(fx.audit.Digest)}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "store.db")
			fx := seedDiminishingFacts(t, path, "run-facts-read", "")
			if err := tryPutItem(fx.st, fx.driftItem(t, fx.driftBinding(), fx.driftFacts())); err != nil {
				t.Fatalf("put drift facts: %v", err)
			}
			itemID := fx.driftItemID()
			query, args := tc.tamper(fx)
			st := tamperStore(t, path, fx.st, query, args...)
			defer func() { _ = st.Close() }()

			ctx := context.Background()
			for tier, read := range map[string]func(*store.ReadTx) error{
				"snapshot": func(tx *store.ReadTx) error {
					_, err := tx.GetAttentionItem(ctx, itemID)
					return err
				},
				"record": func(tx *store.ReadTx) error {
					_, err := tx.GetAttentionItemRecord(ctx, itemID)
					return err
				},
			} {
				err := st.Read(ctx, read)
				// A missing audit must not read as a missing item.
				if !errors.Is(err, tc.want) || errors.Is(err, store.ErrNotFound) {
					t.Fatalf("%s read over a tampered row = %v, want %v and not ErrNotFound", tier, err, tc.want)
				}
			}
		})
	}
}

// TestReviewDiminishingDecisionRefusesADriftAuditItem shows the decision load
// fails closed on cause drift_audit even when the item's card facts and Reason
// agree with a stored audit: convergence evaluation cannot re-derive that
// cause until the load learns about the audit (#1051).
func TestReviewDiminishingDecisionRefusesADriftAuditItem(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store.db")
	fx := seedDiminishingFacts(t, path, "run-drift-decision", "")
	itemID := fx.decision.Item.ID
	binding := fx.decision.Binding
	binding.Cause = domain.ReviewDiminishingDriftAudit
	reason, err := store.ReviewDiminishingReason(binding)
	if err != nil {
		t.Fatal(err)
	}
	facts, err := json.Marshal(driftFactsOf(fx.decisionAudit))
	if err != nil {
		t.Fatal(err)
	}
	st := tamperStore(t, path, fx.st, `UPDATE attention_items
SET body = json_set(body, '$.reason', ?, '$.review_diminishing', json(?)) WHERE id = ?`,
		reason, string(facts), string(itemID))
	defer func() { _ = st.Close() }()

	snapshot, _, err := readItemBothTiers(st, itemID)
	if err != nil {
		t.Fatalf("coherent drift item no longer reads: %v", err)
	}
	if snapshot.ReviewDiminishing == nil || snapshot.ReviewDiminishing.Cause != domain.ReviewDiminishingDriftAudit {
		t.Fatalf("item facts = %+v, want cause drift_audit", snapshot.ReviewDiminishing)
	}
	err = st.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.ReviewDiminishingDecision(ctx, itemID)
		return err
	})
	if !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("drift_audit decision load = %v, want ErrParentKeyMismatch", err)
	}
}

// TestItemStoredBeforeReviewDiminishingFactsStillLoads plants a body with no
// review_diminishing key, the shape every item stored before the field has,
// and shows it reconstructs, loads as a decision, and converges on a replay.
func TestItemStoredBeforeReviewDiminishingFactsStillLoads(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store.db")
	fx := seedDiminishingFacts(t, path, "run-before-facts", domain.ActionFinishNow)
	itemID := fx.decision.Item.ID
	st := tamperStore(t, path, fx.st,
		`UPDATE attention_items SET body = json_remove(body, '$.review_diminishing')
WHERE id = ? AND json_type(body, '$.review_diminishing') = 'null'`, string(itemID))
	defer func() { _ = st.Close() }()

	snapshot, record, err := readItemBothTiers(st, itemID)
	if err != nil {
		t.Fatalf("read item without the key: %v", err)
	}
	if snapshot.ReviewDiminishing != nil || record.ReviewDiminishing != nil {
		t.Fatalf("facts = %+v, %+v; want none", snapshot.ReviewDiminishing, record.ReviewDiminishing)
	}
	var decision store.ReviewDiminishingDecision
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var readErr error
		decision, readErr = tx.ReviewDiminishingDecision(ctx, itemID)
		return readErr
	}); err != nil {
		t.Fatalf("decision load without the key: %v", err)
	}
	if decision.Command == nil || decision.Command.Action != domain.ActionFinishNow {
		t.Fatalf("decision command = %+v, want finish_now", decision.Command)
	}
	// The stored bytes predate the null; an unchanged replay still converges.
	if err := tryPutItem(st, decision.Item); err != nil {
		t.Fatalf("replay over the older row: %v", err)
	}
}
