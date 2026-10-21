package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/domain/domaintest"
	"github.com/freeside-ai/freeside/daemon/migrations"
)

func subsetDisputeFixture(t *testing.T, withPrior bool) (externalCycleFixture, domain.AttentionItem, domain.FindingAdjudication) {
	t.Helper()
	f := seedExternalCycle(t)
	prior := f.adjudication(t, externalCycleRound, map[domain.FindingID]domain.AdjudicationRoute{
		f.internal.ID: domain.RouteDecline, f.finding.ID: domain.RouteDecline,
	})
	if withPrior {
		if err := f.putAdjudication(t, prior); err != nil {
			t.Fatal(err)
		}
	}
	var item domain.AttentionItem
	if err := f.st.Read(t.Context(), func(tx *ReadTx) error {
		record, err := tx.reviewRecordForRound(t.Context(), f.run.ID, externalCycleRound)
		if err != nil {
			return err
		}
		policy, err := tx.GetResolvedPolicy(t.Context(), f.run.ID)
		if err != nil {
			return err
		}
		binding := &domain.ReviewDisputeBinding{
			RunID: f.run.ID, Round: externalCycleRound,
			FindingIDs: []domain.FindingID{f.internal.ID, f.finding.ID}, CompletionEvidence: record.CompletionEvidence,
		}
		binding.Findings = []domain.DisputedFinding{{
			FindingID: f.internal.ID, Message: f.internal.Message,
			Location: f.internal.Location, Severity: f.internal.Severity,
			OfferedRoutes: domain.ReviewDisputeRoutes(f.internal, domain.CanonicalDeclaredPaths(policy)),
		}}
		item, err = domain.NewAttentionItem(domain.AttentionItemInput{
			ID:        domain.ProductionFindingAdjudicationItemID(f.run.ID, externalCycleRound, 1),
			ProjectID: f.run.ProjectID, Subject: f.item.Subject,
			Type: domain.AttentionReviewDispute, Priority: domain.PriorityNormal, Reason: "choose routes",
			RequestedDecision: []domain.Action{domain.ActionAdjudicateFindings, domain.ActionStop},
			PRHeadSHA:         record.HeadSHA, ItemVersion: 1, InterruptionClass: domain.InterruptionPlannedGate,
			Status: domain.StatusOpen, ReviewDispute: binding,
		}, nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return f, item, prior
}

func fullDisputeRows(f externalCycleFixture, item domain.AttentionItem) domain.AttentionItem {
	item.ReviewDispute.Findings = append(item.ReviewDispute.Findings, domain.DisputedFinding{
		FindingID: f.finding.ID, Message: f.finding.Message, Location: f.finding.Location, Severity: f.finding.Severity,
		External:      &domain.DisputedFindingExternal{ReviewerLogin: f.finding.External.ReviewerLogin, ThreadID: f.finding.External.ThreadID},
		OfferedRoutes: []domain.AdjudicationRoute{domain.RouteDecline, domain.RouteDefer},
	})
	return item
}

func putSubsetDispute(t *testing.T, f externalCycleFixture, item *domain.AttentionItem) {
	t.Helper()
	if err := f.st.Write(t.Context(), func(tx *WriteTx) error { return putTestAttentionItem(t.Context(), tx, item) }); err != nil {
		t.Fatal(err)
	}
}

func TestReviewDisputeSubsetAuthenticatesOmittedFindings(t *testing.T) {
	for _, name := range []string{"valid", "no prior", "missing external entry", "round", "finding", "evidence", "head", "message", "routes"} {
		t.Run(name, func(t *testing.T) {
			f, item, _ := subsetDisputeFixture(t, name != "no prior")
			switch name {
			case "missing external entry":
				item.ReviewDispute.FindingIDs = append(item.ReviewDispute.FindingIDs, f.second.ID)
			case "round":
				item.ReviewDispute.Round++
			case "finding":
				item.ReviewDispute.FindingIDs[1] = "invented"
			case "evidence":
				item.ReviewDispute.CompletionEvidence = "sha256:wrong"
			case "head":
				item.PRHeadSHA = "wrong"
			case "message":
				item.ReviewDispute.Findings[0].Message = "forged"
			case "routes":
				item.ReviewDispute.Findings[0].OfferedRoutes = []domain.AdjudicationRoute{domain.RouteRemediate}
			}
			err := f.st.Write(t.Context(), func(tx *WriteTx) error { return tx.PutAttentionItem(t.Context(), item) })
			if name == "valid" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil {
				t.Fatal("forged subset accepted")
			}
			if (name == "no prior" || name == "missing external entry") && !errors.Is(err, domain.ErrParentKeyMismatch) {
				t.Fatalf("wrong error: %v", err)
			}
		})
	}
}

func decideSubsetDispute(t *testing.T, f externalCycleFixture, item domain.AttentionItem, message string) domain.AdjudicationDecision {
	t.Helper()
	command, err := domain.NewCommand(domain.CommandInput{
		CommandID: "subset-answer", DeviceID: "device-test", ItemID: item.ID, ItemVersion: item.ItemVersion,
		PRHeadSHA: item.PRHeadSHA, ArtifactDigests: item.ArtifactDigests,
		Action: domain.ActionAdjudicateFindings, Message: message,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.Write(t.Context(), func(tx *WriteTx) error {
		if err := tx.PutCommand(t.Context(), command); err != nil {
			return err
		}
		item.Status = domain.StatusResolved
		item.ItemVersion++
		item, err = item.WithDecidedAt(reentryAt.Add(2 * time.Hour))
		if err != nil {
			return err
		}
		return tx.PutAttentionItem(t.Context(), domaintest.MustConclude(item))
	}); err != nil {
		t.Fatal(err)
	}
	return domain.AdjudicationDecision{CommandID: command.CommandID, ItemID: item.ID, ItemVersion: command.ItemVersion}
}

func TestReviewDisputeSubsetHumanAnswerPreservesPriorAndReadsBack(t *testing.T) {
	for _, name := range []string{"valid", "missing choice", "omitted choice", "changed omitted", "dropped omitted"} {
		t.Run(name, func(t *testing.T) {
			f, item, prior := subsetDisputeFixture(t, true)
			putSubsetDispute(t, f, &item)
			message := `[{"finding_id":"` + string(f.internal.ID) + `","route":"defer"}]`
			if name == "missing choice" {
				message = `[]`
			}
			if name == "omitted choice" {
				message = `[{"finding_id":"` + string(f.finding.ID) + `","route":"defer"}]`
			}
			decision := decideSubsetDispute(t, f, item, message)
			entries := slices.Clone(prior.Entries)
			for index, entry := range entries {
				if entry.FindingID == f.internal.ID || name == "changed omitted" {
					var err error
					entries[index], err = domain.NewHumanAdjudicationEntry(entry.FindingID, domain.RouteDefer)
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			if name == "dropped omitted" {
				entries = slices.DeleteFunc(entries, func(e domain.FindingAdjudicationEntry) bool { return e.FindingID == f.finding.ID })
			}
			human, err := domain.NewHumanSuccessorFindingAdjudication(prior, entries, decision, reentryAt.Add(2*time.Hour))
			if name == "dropped omitted" {
				if err == nil {
					t.Fatal("changed batch accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			err = f.putAdjudication(t, human)
			if name != "valid" {
				if !errors.Is(err, domain.ErrParentKeyMismatch) {
					t.Fatalf("invalid answer: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := f.st.Read(t.Context(), func(tx *ReadTx) error {
				if _, err := tx.GetAttentionItem(t.Context(), item.ID); err != nil {
					return err
				}
				if _, err := tx.GetAttentionItemRecord(t.Context(), item.ID); err != nil {
					return err
				}
				got, err := tx.GetFindingAdjudicationForRound(t.Context(), f.run.ID, externalCycleRound)
				if err == nil && !reflect.DeepEqual(got, human) {
					t.Fatal("human round trip differs")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestReviewDisputeSubsetRefusesHumanFirstRevisionWithoutRecursion(t *testing.T) {
	f, item, _ := subsetDisputeFixture(t, false)
	item = fullDisputeRows(f, item)
	putSubsetDispute(t, f, &item)
	ids := slices.Clone(item.ReviewDispute.FindingIDs)
	slices.Sort(ids)
	choices := make([]struct {
		FindingID domain.FindingID         `json:"finding_id"`
		Route     domain.AdjudicationRoute `json:"route"`
	}, 0, len(ids))
	var entries []domain.FindingAdjudicationEntry
	for _, id := range ids {
		entry, err := domain.NewHumanAdjudicationEntry(id, domain.RouteDecline)
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, entry)
		choices = append(choices, struct {
			FindingID domain.FindingID         `json:"finding_id"`
			Route     domain.AdjudicationRoute `json:"route"`
		}{id, domain.RouteDecline})
	}
	body, err := json.Marshal(choices)
	if err != nil {
		t.Fatal(err)
	}
	decision := decideSubsetDispute(t, f, item, string(body))
	human, err := domain.NewHumanFindingAdjudication(f.run.ID, externalCycleRound, f.run.SpecDigest,
		f.adjudication(t, externalCycleRound, map[domain.FindingID]domain.AdjudicationRoute{f.internal.ID: domain.RouteDecline}).InstructionSnapshotDigest,
		f.run.PolicyDigest, entries, decision, reentryAt.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.putAdjudication(t, human); err != nil {
		t.Fatal(err)
	}
	item.ReviewDispute.Findings = item.ReviewDispute.Findings[:1]
	err = f.st.Read(t.Context(), func(tx *ReadTx) error { return tx.gateExecutableReviewDispute(t.Context(), item) })
	if !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("human first revision: %v", err)
	}
}

func TestReviewDisputeDisplayedSetRefusesTampering(t *testing.T) {
	for _, name := range []string{"delete row", "add row", "substitute row", "missing record", "corrupt record"} {
		t.Run(name, func(t *testing.T) {
			f, item, _ := subsetDisputeFixture(t, true)
			if name == "delete row" {
				item = fullDisputeRows(f, item)
			}
			putSubsetDispute(t, f, &item)
			var original domain.AttentionItem
			if err := f.st.Read(t.Context(), func(tx *ReadTx) error {
				var err error
				original, err = tx.GetAttentionItem(t.Context(), item.ID)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			item = original
			if name == "delete row" {
				item.ReviewDispute.Findings = item.ReviewDispute.Findings[:1]
			}
			if name == "add row" || name == "substitute row" {
				item = fullDisputeRows(f, item)
			}
			if name == "substitute row" {
				item.ReviewDispute.Findings = item.ReviewDispute.Findings[1:]
			}
			if name != "missing record" && name != "corrupt record" {
				item.ItemVersion++
				err := f.st.Write(t.Context(), func(tx *WriteTx) error { return tx.PutAttentionItem(t.Context(), item) })
				if !errors.Is(err, ErrImmutableConflict) {
					t.Fatalf("changed set write: %v", err)
				}
				item.ItemVersion--
			}
			if err := f.st.Write(t.Context(), func(tx *WriteTx) error {
				if name == "missing record" {
					_, err := tx.tx.ExecContext(t.Context(), `DELETE FROM review_dispute_rows WHERE item_id = ?`, item.ID)
					return err
				}
				if name == "corrupt record" {
					_, err := tx.tx.ExecContext(t.Context(), `UPDATE review_dispute_rows SET finding_ids = 'invalid' WHERE item_id = ?`, item.ID)
					return err
				}
				body, err := json.Marshal(item)
				if err != nil {
					return err
				}
				_, err = tx.tx.ExecContext(t.Context(), `UPDATE attention_items SET body = ? WHERE id = ?`, string(body), item.ID)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			for _, history := range []bool{false, true} {
				err := f.st.Read(t.Context(), func(tx *ReadTx) error {
					if history {
						_, err := tx.GetAttentionItemRecord(t.Context(), item.ID)
						return err
					}
					_, err := tx.GetAttentionItem(t.Context(), item.ID)
					return err
				})
				if !errors.Is(err, errRowInconsistent) {
					t.Fatalf("history=%v: %v", history, err)
				}
			}
			err := f.st.Write(t.Context(), func(tx *WriteTx) error { return tx.PutAttentionItem(t.Context(), item) })
			if !errors.Is(err, errRowInconsistent) {
				t.Fatalf("tampered replay: %v", err)
			}
		})
	}
}

func TestReviewDisputeFirstRoutedTransitionRecordsSet(t *testing.T) {
	f, routed, _ := subsetDisputeFixture(t, true)
	// A legacy card without a binding can gain its first routed binding.
	legacy := routed
	legacy.ReviewDispute = nil
	legacy.RequestedDecision = []domain.Action{domain.ActionStop}
	putSubsetDispute(t, f, &legacy)
	routed.ItemVersion++
	putSubsetDispute(t, f, &routed)
	// Both a replay and a later delivery update must retain the same record.
	putSubsetDispute(t, f, &routed)
	if err := f.st.Read(t.Context(), func(tx *ReadTx) error { _, err := tx.GetAttentionItem(t.Context(), routed.ID); return err }); err != nil {
		t.Fatal(err)
	}
}

func TestReviewDisputeRowsMigration(t *testing.T) {
	f, item, _ := subsetDisputeFixture(t, true)
	item = fullDisputeRows(f, item)
	for _, name := range []string{"valid", "non routed", "partial", "duplicate", "corrupt"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			db := openRaw(t)
			migrateThrough(t, ctx, db, "0098_")
			candidate := item
			binding := *item.ReviewDispute
			binding.Findings = slices.Clone(binding.Findings)
			candidate.ReviewDispute = &binding
			switch name {
			case "non routed":
				binding.Findings = nil
				candidate.RequestedDecision = []domain.Action{domain.ActionStop}
			case "partial":
				binding.Findings = binding.Findings[:1]
			case "duplicate":
				binding.Findings[1] = binding.Findings[0]
			}
			body, err := json.Marshal(candidate)
			if err != nil {
				t.Fatal(err)
			}
			if name == "corrupt" {
				body = []byte("{")
			}
			if _, err := db.ExecContext(ctx, `INSERT INTO attention_items (id, project_id, item_type, status, entity_version, as_of_revision, body) VALUES (?, ?, 'review_dispute', 'open', 1, 1, ?)`, candidate.ID, candidate.ProjectID, string(body)); err != nil {
				t.Fatal(err)
			}
			err = migrate(ctx, db, migrations.FS)
			if name == "partial" || name == "duplicate" || name == "corrupt" {
				if err == nil {
					t.Fatal("invalid legacy row authenticated")
				}
				var count int
				if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE name = 'review_dispute_rows'`).Scan(&count); err != nil {
					t.Fatal(err)
				}
				if count != 0 {
					t.Fatal("migration did not roll back table")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := migrate(ctx, db, migrations.FS); err != nil {
				t.Fatal(err)
			}
			var ids string
			err = db.QueryRowContext(ctx, `SELECT finding_ids FROM review_dispute_rows WHERE item_id = ?`, candidate.ID).Scan(&ids)
			if name == "non routed" {
				if err == nil {
					t.Fatal("non-routed card gained a record")
				}
				return
			}
			if err != nil || ids != displayedDisputeIDs(candidate) {
				t.Fatalf("backfill %q: %v", ids, err)
			}
		})
	}
}
