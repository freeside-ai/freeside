package store_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

func TestFinishReviewDiminishingDefersDisplayedBatchAndSurvivesRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store.db")
	runID := domain.RunID("run-diminishing")
	at := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	st, decision := seedReviewDiminishingDecision(t, path, runID, at, domain.ActionFinishNow)

	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.FinishReviewDiminishing(ctx, decision.Item.ID)
	}); err != nil {
		t.Fatalf("finish diminishing review: %v", err)
	}
	assertDiminishingFinish(t, st, runID, decision)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := storetest.Open(t, path, store.Options{})
	assertDiminishingFinish(t, reopened, runID, decision)
}

func TestReviewDiminishingAuthorityFailsClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	at := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	t.Run("wrong action cannot finish", func(t *testing.T) {
		st, decision := seedReviewDiminishingDecision(
			t, filepath.Join(t.TempDir(), "store.db"), "run-apply", at,
			domain.ActionApplyThenFinish,
		)
		t.Cleanup(func() { _ = st.Close() })
		if err := st.Write(ctx, func(tx *store.WriteTx) error {
			return tx.FinishReviewDiminishing(ctx, decision.Item.ID)
		}); !errors.Is(err, domain.ErrTransitionCommandMismatch) {
			t.Fatalf("finish error = %v, want ErrTransitionCommandMismatch", err)
		}
	})

	t.Run("ordinary disposition route remains strict", func(t *testing.T) {
		st, decision := seedReviewDiminishingDecision(
			t, filepath.Join(t.TempDir(), "store.db"), "run-strict", at,
			domain.ActionFinishNow,
		)
		t.Cleanup(func() { _ = st.Close() })
		disposition := domain.ReviewDispositionRecord{
			FindingID: decision.Binding.FindingIDs[0],
			RunID:     decision.Binding.RunID, Round: decision.Binding.Round,
			Disposition: domain.ReviewDispositionDeferred, Reason: "caller-selected deferral",
			AdjudicationDigest: decision.Binding.AdjudicationDigest, CreatedAt: at.Add(time.Minute),
		}
		if err := st.Write(ctx, func(tx *store.WriteTx) error {
			return tx.PutFindingDisposition(ctx, disposition)
		}); !errors.Is(err, domain.ErrInvalidDispositionAdjudication) {
			t.Fatalf("ordinary deferred disposition error = %v, want ErrInvalidDispositionAdjudication", err)
		}
	})
}

func TestReviewDiminishingHardLimitOffersOnlyFinishNow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store.db")
	at := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	st, decision := seedReviewDiminishingDecisionWithHardLimit(
		t, path, "run-hard-limit", at, domain.ActionFinishNow, 2)
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		got, err := tx.ReviewDiminishingDecision(ctx, decision.Item.ID)
		if err != nil {
			return err
		}
		if !slices.Equal(got.Item.RequestedDecision, []domain.Action{domain.ActionFinishNow}) {
			t.Fatalf("requested decisions = %v", got.Item.RequestedDecision)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE attention_items
SET body = json_set(body, '$.requested_decision', json_array(?, ?, ?))
WHERE id = ?`, domain.ActionFinishNow, domain.ActionApplyThenFinish,
		domain.ActionContinueUnderPolicy, decision.Item.ID); err != nil {
		t.Fatal(err)
	}
	forged := decision.Item
	forged.RequestedDecision = []domain.Action{
		domain.ActionFinishNow, domain.ActionApplyThenFinish, domain.ActionContinueUnderPolicy,
	}
	forgeDecisionSurface(t, db, forged)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := storetest.Open(t, path, store.Options{})
	err = reopened.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.ReviewDiminishingDecision(ctx, decision.Item.ID)
		return err
	})
	if !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("forged hard-limit choices = %v, want ErrParentKeyMismatch", err)
	}
}

func TestReviewDiminishingHardLimitRejectsUnofferedConcludedAction(t *testing.T) {
	t.Parallel()
	for _, action := range []domain.Action{
		domain.ActionApplyThenFinish,
		domain.ActionContinueUnderPolicy,
	} {
		t.Run(string(action), func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "store.db")
			at := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
			st, decision := seedReviewDiminishingDecisionWithHardLimit(
				t, path, domain.RunID("run-hard-limit-"+action), at, domain.ActionFinishNow, 2)
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`UPDATE commands
SET action = ?, body = json_set(body, '$.command.action', ?)
WHERE command_id = ?`, action, action, decision.Command.CommandID); err != nil {
				t.Fatal(err)
			}
			var body []byte
			if err := db.QueryRow(`SELECT body FROM commands WHERE command_id = ?`,
				decision.Command.CommandID).Scan(&body); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`UPDATE commands SET backup_binding_digest = ? WHERE command_id = ?`,
				contentaddr.Sum(body), decision.Command.CommandID); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			reopened := storetest.Open(t, path, store.Options{})
			err = reopened.Read(ctx, func(tx *store.ReadTx) error {
				_, err := tx.ReviewDiminishingDecision(ctx, decision.Item.ID)
				return err
			})
			if !errors.Is(err, domain.ErrParentKeyMismatch) {
				t.Fatalf("unoffered concluded action %q = %v, want ErrParentKeyMismatch", action, err)
			}
		})
	}
}

func TestListReviewDiminishingDecisionsAuthenticatesBeforeRunSelection(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store.db")
	runID := domain.RunID("run-decision-list")
	otherRunID := domain.RunID("run-decision-list-other")
	at := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	st, decision := seedReviewDiminishingDecision(
		t, path, runID, at, domain.ActionApplyThenFinish)
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutRun(ctx, domain.Run{
			ID: otherRunID, ProjectID: "project-1",
			SpecDigest: adjSpecDigest, PolicyDigest: "sha256:policy-other",
		})
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE attention_items
SET subject_run_id = ?, body = json_set(body,
    '$.subject.subject_id', ?, '$.subject.run_id', ?)
WHERE id = ?`, otherRunID, otherRunID, otherRunID, decision.Item.ID); err != nil {
		t.Fatal(err)
	}
	moved := decision.Item
	moved.Subject = domain.Subject{Type: moved.Subject.Type, ID: domain.SubjectID(otherRunID), RunID: &otherRunID}
	forgeDecisionSurface(t, db, moved)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := storetest.Open(t, path, store.Options{})
	err = reopened.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.ListReviewDiminishingDecisions(ctx, runID)
		return err
	})
	if !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("decision list after coherent run move = %v, want ErrParentKeyMismatch", err)
	}
}

func TestReviewDiminishingDecisionRejectsMismatchedAndTamperedAuthority(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		tamper func(*testing.T, *sql.DB, store.ReviewDiminishingDecision)
	}{
		{name: "wrong project", tamper: func(
			t *testing.T, db *sql.DB, decision store.ReviewDiminishingDecision,
		) {
			t.Helper()
			if _, err := db.Exec(`UPDATE attention_items
SET project_id = 'project-other', body = json_set(body, '$.project_id', 'project-other')
WHERE id = ?`, decision.Item.ID); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "wrong item", tamper: tamperDiminishingBinding(func(binding *store.ReviewDiminishingBinding) {
			binding.ItemID = "item-other"
		})},
		{name: "wrong run", tamper: tamperDiminishingBinding(func(binding *store.ReviewDiminishingBinding) {
			binding.RunID = "run-other"
		})},
		{name: "wrong round", tamper: tamperDiminishingBinding(func(binding *store.ReviewDiminishingBinding) {
			binding.Round++
		})},
		{name: "wrong head", tamper: tamperDiminishingBinding(func(binding *store.ReviewDiminishingBinding) {
			binding.HeadSHA = "head-other"
		})},
		{name: "wrong adjudication digest", tamper: tamperDiminishingBinding(func(binding *store.ReviewDiminishingBinding) {
			binding.AdjudicationDigest = "sha256:other-adjudication"
		})},
		{name: "wrong finding set", tamper: tamperDiminishingBinding(func(binding *store.ReviewDiminishingBinding) {
			binding.FindingBatchDigest = "sha256:other-batch"
		})},
		{name: "stale policy", tamper: tamperDiminishingBinding(func(binding *store.ReviewDiminishingBinding) {
			binding.PolicyDigest = "sha256:stale-policy"
		})},
		{name: "fabricated cause", tamper: tamperDiminishingBinding(func(binding *store.ReviewDiminishingBinding) {
			binding.Cause = domain.ReviewDiminishingLowValue
		})},
		{name: "fabricated growth cause", tamper: tamperDiminishingBinding(func(binding *store.ReviewDiminishingBinding) {
			binding.Cause = domain.ReviewDiminishingGrowthWithoutBlockers
		})},
		// Convergence evaluation never returns drift_audit, so a decision
		// bound to it loads only on a round no deterministic cause stopped and
		// that carries an audit. This round stopped on fixed_recurrence.
		{name: "drift audit cause", tamper: tamperDiminishingBinding(func(binding *store.ReviewDiminishingBinding) {
			binding.Cause = domain.ReviewDiminishingDriftAudit
		})},
		{name: "wrong version", tamper: func(t *testing.T, db *sql.DB, decision store.ReviewDiminishingDecision) {
			t.Helper()
			if _, err := db.Exec(`UPDATE commands
SET item_version = 2, body = json_set(body, '$.item_version', 2)
WHERE command_id = ?`, decision.Command.CommandID); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "decoded item row", tamper: func(t *testing.T, db *sql.DB, decision store.ReviewDiminishingDecision) {
			t.Helper()
			if _, err := db.Exec(`UPDATE attention_items
SET body = json_set(body, '$.yield_history.rounds[0].new_findings', 0)
WHERE id = ?`, decision.Item.ID); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "decoded finding set", tamper: func(t *testing.T, db *sql.DB, decision store.ReviewDiminishingDecision) {
			t.Helper()
			if _, err := db.Exec(`UPDATE review_records
SET body = json_set(body, '$.finding_ids', json_array('finding-other'))
WHERE run_id = ? AND round = ?`, decision.Binding.RunID, decision.Binding.Round); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "store.db")
			st, decision := seedReviewDiminishingDecision(
				t, path, domain.RunID("run-tamper-"+strings.ReplaceAll(tc.name, " ", "-")),
				at, domain.ActionFinishNow,
			)
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			raw, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			tc.tamper(t, raw, decision)
			if err := raw.Close(); err != nil {
				t.Fatal(err)
			}
			reopened := storetest.Open(t, path, store.Options{})
			defer func() { _ = reopened.Close() }()
			err = reopened.Read(context.Background(), func(tx *store.ReadTx) error {
				_, readErr := tx.ReviewDiminishingDecision(context.Background(), decision.Item.ID)
				return readErr
			})
			if err == nil {
				t.Fatal("tampered diminishing authority was accepted")
			}
		})
	}
}

func TestDecisionTimeDispositionFilteringAuthenticatesScope(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	at := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	reads := map[string]func(context.Context, *store.ReadTx, store.ReviewDiminishingDecision) error{
		"yield history": func(
			ctx context.Context, tx *store.ReadTx, decision store.ReviewDiminishingDecision,
		) error {
			_, err := tx.ReviewYieldHistoryAtDecision(
				ctx, decision.Binding.RunID, decision.Binding.Round)
			return err
		},
		"convergence state": func(
			ctx context.Context, tx *store.ReadTx, decision store.ReviewDiminishingDecision,
		) error {
			records, err := tx.ListReviewRecords(ctx, decision.Binding.RunID)
			if err != nil {
				return err
			}
			_, err = tx.ReviewConvergenceStateAtDecision(ctx, records[len(records)-1])
			return err
		},
	}
	for tamperName, tamper := range map[string]func(
		*testing.T, string, *store.Store, domain.ReviewDispositionRecord,
	) *store.Store{
		"run": func(
			t *testing.T, path string, st *store.Store, disposition domain.ReviewDispositionRecord,
		) *store.Store {
			t.Helper()
			otherRun := domain.Run{
				ID: "run-other", ProjectID: "project-1",
				SpecDigest: adjSpecDigest, PolicyDigest: "sha256:policy-other",
			}
			if err := st.Write(ctx, func(tx *store.WriteTx) error { return tx.PutRun(ctx, otherRun) }); err != nil {
				t.Fatal(err)
			}
			disposition.RunID = otherRun.ID
			return rewriteDispositionBodyAndColumns(t, path, st, disposition)
		},
		"round": func(
			t *testing.T, path string, st *store.Store, disposition domain.ReviewDispositionRecord,
		) *store.Store {
			t.Helper()
			disposition.Round++
			return rewriteDispositionBodyAndColumns(t, path, st, disposition)
		},
	} {
		for readName, read := range reads {
			t.Run(tamperName+"/"+readName, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "store.db")
				st, decision := seedReviewDiminishingDecision(
					t, path, domain.RunID("run-filter-"+tamperName+"-"+readName),
					at, domain.ActionFinishNow,
				)
				t.Cleanup(func() { _ = st.Close() })
				dispositions := []domain.ReviewDispositionRecord{}
				if err := st.Read(ctx, func(tx *store.ReadTx) error {
					var err error
					dispositions, err = tx.ListFindingDispositions(ctx, decision.Binding.RunID)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				st = tamper(t, path, st, dispositions[0])
				err := st.Read(ctx, func(tx *store.ReadTx) error { return read(ctx, tx, decision) })
				if !errors.Is(err, domain.ErrParentKeyMismatch) {
					t.Fatalf("decision-time read after coherent %s tamper = %v", tamperName, err)
				}
			})
		}
	}
}

func TestPriorDiminishingDecisionIsAuthenticatedBeforeRoundSelection(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store.db")
	runID := domain.RunID("run-prior-decision-tamper")
	at := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	st, decision := seedReviewDiminishingDecision(
		t, path, runID, at, domain.ActionApplyThenFinish)
	finding := adjudicationFinding("finding-later", runID, "daemon/later.go", at.Add(2*time.Minute))
	record := adjudicationReviewRecord(
		t, runID, 3, []domain.FindingID{finding.ID}, finding.CreatedAt)
	artifact, err := domain.NewFindingAdjudication(
		runID, record.Round, adjSpecDigest, record.InstructionDigest, decision.Binding.PolicyDigest,
		[]domain.FindingAdjudicationEntry{adjudicationEngineEntry(t, finding.ID)}, "",

		finding.CreatedAt.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutReviewRecord(ctx, record, []domain.Finding{finding}); err != nil {
			return err
		}
		return tx.PutFindingAdjudication(ctx, artifact)
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	tampered := decision.Binding
	tampered.Round = record.Round
	reason, err := store.ReviewDiminishingReason(tampered)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE attention_items
SET body = json_set(body, '$.reason', ?)
WHERE id = ?`, reason, decision.Item.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := storetest.Open(t, path, store.Options{})
	err = reopened.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.ReviewConvergenceStateAtDecision(ctx, record)
		return err
	})
	if !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("current state with hidden prior decision = %v, want ErrParentKeyMismatch", err)
	}
}

func rewriteDispositionBodyAndColumns(
	t *testing.T, path string, st *store.Store, disposition domain.ReviewDispositionRecord,
) *store.Store {
	t.Helper()
	body, err := json.Marshal(disposition)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE finding_dispositions
SET run_id = ?, round = ?, body_digest = ?, body = ?`,
		disposition.RunID, disposition.Round, contentaddr.Sum(body), string(body)); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := storetest.Open(t, path, store.Options{})
	return reopened
}

func tamperDiminishingBinding(
	mutate func(*store.ReviewDiminishingBinding),
) func(*testing.T, *sql.DB, store.ReviewDiminishingDecision) {
	return func(t *testing.T, db *sql.DB, decision store.ReviewDiminishingDecision) {
		t.Helper()
		binding := decision.Binding
		mutate(&binding)
		reason, err := store.ReviewDiminishingReason(binding)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE attention_items
SET body = json_set(body, '$.reason', ?)
WHERE id = ?`, reason, decision.Item.ID); err != nil {
			t.Fatal(err)
		}
	}
}

func seedReviewDiminishingDecision(
	t *testing.T, path string, runID domain.RunID, at time.Time, action domain.Action,
) (*store.Store, store.ReviewDiminishingDecision) {
	t.Helper()
	return seedReviewDiminishingDecisionWithHardLimit(t, path, runID, at, action, 25)
}

func seedReviewDiminishingDecisionWithHardLimit(
	t *testing.T,
	path string,
	runID domain.RunID,
	at time.Time,
	action domain.Action,
	hardRoundLimit int,
) (*store.Store, store.ReviewDiminishingDecision) {
	t.Helper()
	return seedReviewDiminishingDecisionWith(t, path, runID, at, action, hardRoundLimit, false)
}

// seedReviewDiminishingDecisionWith optionally records each round's diff
// metrics as the round is written, so the item snapshots a history that
// carries them.
func seedReviewDiminishingDecisionWith(
	t *testing.T,
	path string,
	runID domain.RunID,
	at time.Time,
	action domain.Action,
	hardRoundLimit int,
	withDiffMetrics bool,
) (*store.Store, store.ReviewDiminishingDecision) {
	t.Helper()
	st, decision, _ := seedDiminishingDecision(
		t, path, runID, at, action, hardRoundLimit, withDiffMetrics, nil)
	return st, decision
}

// driftSeed describes the drift-parked variant of the seeded run: round 2's
// findings are both new, so no deterministic cause stops it, its audit reverses
// round 1's fixed finding-a, and its item parks on drift_audit.
type driftSeed struct {
	// verdict defaults to over_hardened. Only that verdict carries reversals.
	verdict domain.DriftVerdict
	// extraReversals are reversed after finding-a.
	extraReversals []domain.FindingID
	// severity is finding-a's. Unset reads as high at the route gate.
	severity domain.FindingSeverity
	// policyKeys are added to the run's resolved policy.
	policyKeys map[string]string
	// hardRoundLimit defaults to 25.
	hardRoundLimit int
	// simplification stamps the item with the audit's card facts, promising a
	// simplification round on continue.
	simplification bool
}

func seedDriftDiminishingDecision(
	t *testing.T, path string, runID domain.RunID, at time.Time, action domain.Action, seed driftSeed,
) (*store.Store, store.ReviewDiminishingDecision, domain.DriftAudit) {
	t.Helper()
	if seed.verdict == "" {
		seed.verdict = domain.DriftVerdictOverHardened
	}
	if seed.hardRoundLimit == 0 {
		seed.hardRoundLimit = 25
	}
	st, decision, audit := seedDiminishingDecision(
		t, path, runID, at, action, seed.hardRoundLimit, false, &seed)
	return st, decision, *audit
}

// seedDiminishingDecision is the shared seed. A nil drift parks round 2 on
// fixed_recurrence and writes no audit.
func seedDiminishingDecision(
	t *testing.T,
	path string,
	runID domain.RunID,
	at time.Time,
	action domain.Action,
	hardRoundLimit int,
	withDiffMetrics bool,
	drift *driftSeed,
) (*store.Store, store.ReviewDiminishingDecision, *domain.DriftAudit) {
	t.Helper()
	ctx := context.Background()
	st := storetest.Open(t, path, store.Options{})
	policyKeys := []domain.PolicyKey{
		{
			Key: "paths", Value: "daemon/**", Provenance: domain.KeyProvenance{
				Source: domain.ProvenancePreset,
				Digest: domain.Digest("sha256:" + strings.Repeat("a", 64)),
			},
		},
		{
			Key: "review.continue_while", Value: store.ReviewContinueWhileNewMaterialFindings,
			Provenance: domain.KeyProvenance{
				Source: domain.ProvenancePreset,
				Digest: domain.Digest("sha256:" + strings.Repeat("b", 64)),
			},
		},
		{
			Key: "review.low_value_streak_before_attention", Value: "2",
			Provenance: domain.KeyProvenance{
				Source: domain.ProvenancePreset,
				Digest: domain.Digest("sha256:" + strings.Repeat("c", 64)),
			},
		},
		{
			Key: "review.hard_round_limit", Value: fmt.Sprint(hardRoundLimit),
			Provenance: domain.KeyProvenance{
				Source: domain.ProvenancePreset,
				Digest: domain.Digest("sha256:" + strings.Repeat("d", 64)),
			},
		},
	}
	var extraKeys map[string]string
	if drift != nil {
		extraKeys = drift.policyKeys
	}
	for _, key := range slices.Sorted(maps.Keys(extraKeys)) {
		policyKeys = append(policyKeys, domain.PolicyKey{
			Key: key, Value: extraKeys[key], Provenance: domain.KeyProvenance{
				Source: domain.ProvenancePreset,
				Digest: domain.Digest("sha256:" + strings.Repeat("e", 64)),
			},
		})
	}
	policy, err := domain.NewResolvedPolicy(runID, policyKeys)
	if err != nil {
		t.Fatal(err)
	}
	findings := []domain.Finding{
		adjudicationFinding("finding-a", runID, "daemon/a.go", at),
		adjudicationFinding("finding-b", runID, "daemon/a.go", at.Add(time.Minute)),
		adjudicationFinding("finding-c", runID, "daemon/c.go", at.Add(time.Minute)),
	}
	if drift == nil {
		// finding-b recurs the fixed finding-a.
		findings[1].Message = findings[0].Message
		findings[1].RawText = findings[0].RawText
	} else {
		findings[0].Severity = drift.severity
	}
	reviewFindings := [][]domain.Finding{{findings[0]}, {findings[1], findings[2]}}
	records := make([]domain.ReviewRecord, len(reviewFindings))
	artifacts := make([]domain.FindingAdjudication, len(reviewFindings))
	for index, batch := range reviewFindings {
		round := index + 1
		findingIDs := make([]domain.FindingID, len(batch))
		entries := make([]domain.FindingAdjudicationEntry, len(batch))
		for findingIndex, finding := range batch {
			findingIDs[findingIndex] = finding.ID
			entries[findingIndex] = adjudicationEngineEntry(t, finding.ID)
		}
		records[index] = adjudicationReviewRecord(t, runID, round, findingIDs, batch[0].CreatedAt)
		artifacts[index], err = domain.NewFindingAdjudication(
			runID, round, adjSpecDigest, records[index].InstructionDigest, policy.Digest,
			entries, "",

			batch[0].CreatedAt.Add(time.Second))
		if err != nil {
			t.Fatal(err)
		}
	}
	record := records[len(records)-1]
	artifact := artifacts[len(artifacts)-1]
	var audit *domain.DriftAudit
	cause := domain.ReviewDiminishingFixedRecurrence
	var facts *domain.ReviewDiminishingFacts
	if drift != nil {
		var reversed []domain.FindingID
		if drift.verdict == domain.DriftVerdictOverHardened {
			reversed = append([]domain.FindingID{findings[0].ID}, drift.extraReversals...)
		}
		input := driftAuditInputFor(runID, record.Round, drift.verdict, reversed...)
		input.ResolvedPolicyDigest = policy.Digest
		built := newDriftAudit(t, input)
		audit, cause = &built, domain.ReviewDiminishingDriftAudit
		if drift.simplification {
			stamped := driftFactsOf(built)
			stamped.DriftAudit.SimplificationOnContinue = true
			facts = &stamped
		}
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, domain.Run{
			ID: runID, ProjectID: "project-1", SpecDigest: adjSpecDigest, PolicyDigest: policy.Digest,
		}); err != nil {
			return err
		}
		if err := tx.PutResolvedPolicy(ctx, policy); err != nil {
			return err
		}
		for index := range records {
			if err := tx.PutReviewRecord(ctx, records[index], reviewFindings[index]); err != nil {
				return err
			}
			if withDiffMetrics {
				if err := tx.PutReviewRoundDiffMetrics(
					ctx, runID, records[index].Round, roundDiffMetrics(records[index].Round),
				); err != nil {
					return err
				}
			}
			if err := tx.PutFindingAdjudication(ctx, artifacts[index]); err != nil {
				return err
			}
		}
		if err := tx.PutFindingDisposition(ctx, domain.ReviewDispositionRecord{
			FindingID: findings[0].ID, RunID: runID, Round: records[0].Round,
			Disposition:             domain.ReviewDispositionFixed,
			Reason:                  "absent from the independent remediation review",
			RemediationInvocationID: records[1].InvocationID,
			CreatedAt:               records[1].CompletedAt,
		}); err != nil {
			return err
		}
		if audit != nil {
			return tx.PutDriftAudit(ctx, *audit)
		}
		return nil
	}); err != nil {
		t.Fatalf("seed diminishing review: %v", err)
	}
	var history domain.ReviewYieldHistory
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var readErr error
		history, readErr = tx.ReviewYieldHistoryAtDecision(ctx, runID, record.Round)
		return readErr
	}); err != nil {
		t.Fatal(err)
	}
	itemID := store.ReviewDiminishingItemID(runID, record.Round)
	binding := store.ReviewDiminishingBinding{
		ItemID: itemID, RunID: runID, Round: record.Round, HeadSHA: record.HeadSHA,
		FindingIDs:         append([]domain.FindingID(nil), record.FindingIDs...),
		AdjudicationDigest: artifact.Digest, FindingBatchDigest: artifact.FindingBatchDigest,
		PolicyDigest: policy.Digest, ContinueWhile: store.ReviewContinueWhileNewMaterialFindings,
		LowValueStreakBeforeAttention: 2, Cause: cause,
		HardRoundLimit: hardRoundLimit,
	}
	reason, err := store.ReviewDiminishingReason(binding)
	if err != nil {
		t.Fatal(err)
	}
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: itemID, ProjectID: "project-1",
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
		Type:    domain.AttentionReviewDiminishing, Priority: domain.PriorityNormal, Reason: reason,
		RequestedDecision: store.ReviewDiminishingRequestedActions(record.Round, hardRoundLimit),
		PRHeadSHA:         record.HeadSHA, YieldHistory: &history, ReviewDiminishing: facts, ItemVersion: 1,
		InterruptionClass: domain.InterruptionPlannedGate, CreatedAt: &at, Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// An empty action leaves the item open, with no command.
	if action == "" {
		if err := st.Write(ctx, func(tx *store.WriteTx) error {
			return tx.PutAttentionItem(ctx, item)
		}); err != nil {
			t.Fatalf("seed open diminishing item: %v", err)
		}
		return st, store.ReviewDiminishingDecision{Item: item, Binding: binding}, audit
	}
	command, err := domain.NewCommand(domain.CommandInput{
		CommandID: "command-" + string(action), DeviceID: "device-1",
		ItemID: item.ID, ItemVersion: item.ItemVersion, PRHeadSHA: item.PRHeadSHA,
		ArtifactDigests: item.ArtifactDigests, Action: action,
	})
	if err != nil {
		t.Fatal(err)
	}
	decidedAt := at.Add(time.Minute)
	concluded, err := item.WithDecidedAt(decidedAt)
	if err != nil {
		t.Fatal(err)
	}
	concluded.Status = domain.StatusResolved
	concluded.ItemVersion++
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutAttentionItem(ctx, item); err != nil {
			return err
		}
		if err := tx.PutCommand(ctx, command); err != nil {
			return err
		}
		return tx.PutAttentionItem(ctx, concluded)
	}); err != nil {
		t.Fatalf("seed diminishing decision: %v", err)
	}
	return st, store.ReviewDiminishingDecision{Item: concluded, Command: &command, Binding: binding}, audit
}

func assertDiminishingFinish(
	t *testing.T, st *store.Store, runID domain.RunID, want store.ReviewDiminishingDecision,
) {
	t.Helper()
	ctx := context.Background()
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		decision, err := tx.ReviewDiminishingDecision(ctx, want.Item.ID)
		if err != nil {
			return err
		}
		if decision.Command == nil || decision.Command.CommandID != want.Command.CommandID {
			t.Fatalf("decision command = %#v", decision.Command)
		}
		dispositions, err := tx.ListFindingDispositions(ctx, runID)
		if err != nil {
			return err
		}
		current := 0
		for index := range dispositions {
			if dispositions[index].Round == want.Binding.Round {
				current++
				if dispositions[index].Disposition != domain.ReviewDispositionDeferred ||
					dispositions[index].AdjudicationDigest != want.Binding.AdjudicationDigest ||
					!dispositions[index].CreatedAt.Equal(*want.Item.DecidedAt) {
					t.Fatalf("current disposition = %#v", dispositions[index])
				}
			}
		}
		if len(dispositions) != 3 || current != len(want.Binding.FindingIDs) {
			t.Fatalf("dispositions = %#v", dispositions)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// forgeDecisionSurface rewrites the item's persisted decision surface to
// describe a tampered body, so the test reaches the review-diminishing gate
// it exercises: the store's surface re-gate otherwise refuses a row whose
// structural fields no longer match its record (plan §4).
func forgeDecisionSurface(t *testing.T, db *sql.DB, item domain.AttentionItem) {
	t.Helper()
	var taskID domain.TaskID
	if err := db.QueryRow(`SELECT task_id FROM runs WHERE id = ?`, item.Subject.RunID).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	item.Subject.TaskID = &taskID
	surface, err := domain.NewDecisionSurface(item)
	if err != nil {
		t.Fatalf("NewDecisionSurface: %v", err)
	}
	body, err := json.Marshal(surface)
	if err != nil {
		t.Fatalf("encode decision surface: %v", err)
	}
	if _, err := db.Exec(`UPDATE attention_decision_surfaces SET epoch = ?, digest = ?, body = ? WHERE item_id = ?`,
		surface.Epoch, surface.Digest, string(body), surface.ItemID); err != nil {
		t.Fatalf("forge decision surface: %v", err)
	}
	if _, err := db.Exec(`UPDATE attention_items SET body = json_set(body,
'$.decision_surface.epoch', ?, '$.decision_surface.digest', ?, '$.subject.task_id', ?), subject_task_id = ? WHERE id = ?`,
		surface.Epoch, surface.Digest, taskID, taskID, surface.ItemID); err != nil {
		t.Fatalf("forge item decision-surface projection: %v", err)
	}
}

// TestReviewDiminishingReasonSummarizesGrowthWithoutBlockers pins that the new
// stop cause is a valid binding cause with its own summary sentence, so a
// growth item never renders an empty lead line.
func TestReviewDiminishingReasonSummarizesGrowthWithoutBlockers(t *testing.T) {
	t.Parallel()
	runID := domain.RunID("run-growth-reason")
	reason, err := store.ReviewDiminishingReason(store.ReviewDiminishingBinding{
		ItemID: store.ReviewDiminishingItemID(runID, 3), RunID: runID, Round: 3, HeadSHA: "head-3",
		FindingIDs:         []domain.FindingID{"finding-a"},
		AdjudicationDigest: "sha256:adjudication", FindingBatchDigest: "sha256:batch",
		PolicyDigest: "sha256:policy", ContinueWhile: store.ReviewContinueWhileNewMaterialFindings,
		LowValueStreakBeforeAttention: 2, HardRoundLimit: 25,
		Cause: domain.ReviewDiminishingGrowthWithoutBlockers,
	})
	if err != nil {
		t.Fatal(err)
	}
	summary, binding, ok := strings.Cut(reason, "\nBinding: ")
	if !ok || summary == "" || !strings.Contains(binding, `"cause":"growth_without_blockers"`) {
		t.Fatalf("growth reason = %q", reason)
	}
}

// legacyDiminishingYieldHistory is the stored yield_history of the seeded
// diminishing item exactly as a daemon wrote it before diff metrics existed.
const legacyDiminishingYieldHistory = `{"rounds":[` +
	`{"round":1,"findings_ingested":1,"new_findings":1,"recurring_findings":0,` +
	`"fixed":1,"declined":0,"deferred":0,"outcome":"findings"},` +
	`{"round":2,"findings_ingested":2,"new_findings":1,"recurring_findings":1,` +
	`"fixed":0,"declined":0,"deferred":0,"outcome":"findings"}],` +
	`"terminal_outcome":"findings"}`

// TestReviewDiminishingItemStoredBeforeDiffMetricsStillLoads proves the
// yield-history field is additive: a run with no metrics rows writes the same
// bytes a daemon wrote before the field existed, and that item still
// reconstructs and still equals its re-derived history after a restart.
func TestReviewDiminishingItemStoredBeforeDiffMetricsStillLoads(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	at := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "store.db")
	st, decision := seedReviewDiminishingDecision(t, path, "run-legacy-yield", at, domain.ActionFinishNow)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	var stored, inBody string
	if err := raw.QueryRow(`SELECT yield_history, json_extract(body, '$.yield_history')
FROM attention_items WHERE id = ?`, decision.Item.ID).Scan(&stored, &inBody); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	if stored != legacyDiminishingYieldHistory || inBody != legacyDiminishingYieldHistory {
		t.Fatalf("stored yield history is not the legacy bytes:\ncolumn %s\nbody   %s", stored, inBody)
	}
	reopened := storetest.Open(t, path, store.Options{})
	defer func() { _ = reopened.Close() }()
	if err := reopened.Read(ctx, func(tx *store.ReadTx) error {
		got, err := tx.ReviewDiminishingDecision(ctx, decision.Item.ID)
		if err != nil {
			return err
		}
		for _, round := range got.Item.YieldHistory.Rounds {
			if round.DiffMetrics != nil {
				t.Fatalf("legacy round %d gained metrics: %+v", round.Round, round.DiffMetrics)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("legacy item no longer loads: %v", err)
	}
}

// TestReviewDiminishingItemLoadsAfterItsRoundIsAudited proves a drift audit
// written for the round an item already snapshotted does not change that
// item's history: the stored bytes stay the legacy bytes and the item still
// equals its re-derived history after a restart.
func TestReviewDiminishingItemLoadsAfterItsRoundIsAudited(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	at := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	runID := domain.RunID("run-audited-yield")
	path := filepath.Join(t.TempDir(), "store.db")
	st, decision := seedReviewDiminishingDecision(t, path, runID, at, domain.ActionFinishNow)
	input := driftAuditInputFor(
		runID, decision.Binding.Round, domain.DriftVerdictOverHardened, decision.Binding.FindingIDs[0])
	input.ResolvedPolicyDigest = decision.Binding.PolicyDigest
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutDriftAudit(ctx, newDriftAudit(t, input))
	}); err != nil {
		t.Fatalf("audit the snapshotted round: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := storetest.Open(t, path, store.Options{})
	defer func() { _ = reopened.Close() }()
	if err := reopened.Read(ctx, func(tx *store.ReadTx) error {
		got, err := tx.ReviewDiminishingDecision(ctx, decision.Item.ID)
		if err != nil {
			return err
		}
		body, err := json.Marshal(got.Item.YieldHistory)
		if err != nil {
			return err
		}
		if string(body) != legacyDiminishingYieldHistory {
			t.Fatalf("audited item's yield history changed: %s", body)
		}
		return nil
	}); err != nil {
		t.Fatalf("item no longer loads after its round was audited: %v", err)
	}
}

// TestReviewDiminishingItemWithDiffMetricsReconstructs proves an item that
// snapshotted metrics re-proves them against the stored rows after a restart.
func TestReviewDiminishingItemWithDiffMetricsReconstructs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	at := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "store.db")
	st, decision := seedReviewDiminishingDecisionWith(
		t, path, "run-metrics-yield", at, domain.ActionFinishNow, 25, true)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := storetest.Open(t, path, store.Options{})
	defer func() { _ = reopened.Close() }()
	if err := reopened.Read(ctx, func(tx *store.ReadTx) error {
		got, err := tx.ReviewDiminishingDecision(ctx, decision.Item.ID)
		if err != nil {
			return err
		}
		for _, round := range got.Item.YieldHistory.Rounds {
			if round.DiffMetrics == nil || *round.DiffMetrics != roundDiffMetrics(round.Round) {
				t.Fatalf("round %d metrics = %+v", round.Round, round.DiffMetrics)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("item with metrics: %v", err)
	}
}

// TestLateDiffMetricsFailTheSnapshottedItemClosed shows why metrics are
// written with the round and never afterwards: the latest round has no
// successor to refuse the write, and the item that already snapshotted that
// round then no longer equals its re-derived history.
func TestLateDiffMetricsFailTheSnapshottedItemClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	at := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	runID := domain.RunID("run-late-metrics")
	st, decision := seedReviewDiminishingDecision(
		t, filepath.Join(t.TempDir(), "store.db"), runID, at, domain.ActionFinishNow)
	defer func() { _ = st.Close() }()
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutReviewRoundDiffMetrics(
			ctx, runID, decision.Binding.Round, roundDiffMetrics(decision.Binding.Round))
	}); err != nil {
		t.Fatal(err)
	}
	err := st.Read(ctx, func(tx *store.ReadTx) error {
		_, readErr := tx.ReviewDiminishingDecision(ctx, decision.Item.ID)
		return readErr
	})
	if !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("item after late metrics = %v, want ErrParentKeyMismatch", err)
	}
}
