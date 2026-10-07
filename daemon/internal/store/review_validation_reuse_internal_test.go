package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// Seed several dispositions sharing one adjudication and review, the repeated
// joins that a filer pass used to reconstruct for every filing.
func seedReviewValidation(t *testing.T, count, stored int) (*Store, domain.FindingAdjudication, []domain.ReviewDispositionRecord) {
	t.Helper()
	ctx := t.Context()
	st := openTemplateStoreAt(t, t.TempDir()+"/store.db", Options{})
	at := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	run := domain.Run{ID: "reuse-run", ProjectID: "project-1", SpecDigest: migrationAdjudicationDigest("a"), PolicyDigest: migrationAdjudicationDigest("b")}
	var findings []domain.Finding
	var ids []domain.FindingID
	var entries []domain.FindingAdjudicationEntry
	for i := range count {
		id := domain.FindingID(fmt.Sprintf("reuse-finding-%d", i))
		findings = append(findings, domain.Finding{ID: id, RunID: run.ID, Source: "codex_local", Message: "finding", RawText: "finding", CreatedAt: at})
		ids = append(ids, id)
		entry, err := domain.NewModelAdjudicationEntry(id, domain.GoalAdjacent, nil, domain.RouteDefer,
			domain.ConfidenceHigh, "separate work", nil, nil, nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, entry)
	}
	record, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: "reuse-review", RunID: run.ID, Round: 1,
		Provider: "openai", ModelConfiguration: "gpt-codex/high", CostOwner: "owner",
		ConfigurationDigest: migrationAdjudicationDigest("c"), InstructionDigest: migrationAdjudicationDigest("d"),
		CompletionEvidence: migrationAdjudicationDigest("e"), BaseSHA: "base", HeadSHA: "head",
		CompletedAt: at, Outcome: domain.ReviewFindings, FindingIDs: ids,
	})
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := domain.NewFindingAdjudication(run.ID, 1, run.SpecDigest, record.InstructionDigest,
		run.PolicyDigest, entries, "", at.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	var dispositions []domain.ReviewDispositionRecord
	if err := st.Write(ctx, func(tx *WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		if err := tx.PutReviewRecord(ctx, record, findings); err != nil {
			return err
		}
		if err := tx.PutFindingAdjudication(ctx, artifact); err != nil {
			return err
		}
		for i, id := range ids {
			disposition := domain.ReviewDispositionRecord{
				FindingID: id, RunID: run.ID, Round: 1,
				Disposition: domain.ReviewDispositionDeferred, Reason: "separate work",
				AdjudicationDigest: artifact.Digest, CreatedAt: at.Add(2 * time.Minute),
			}
			if i < stored {
				if err := tx.PutFindingDisposition(ctx, disposition); err != nil {
					return err
				}
			}
			dispositions = append(dispositions, disposition)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return st, artifact, dispositions
}

func TestReviewValidationReuseBoundsWork(t *testing.T) {
	st, artifact, dispositions := seedReviewValidation(t, 5, 5)
	ctx := t.Context()
	if err := st.Read(ctx, func(tx *ReadTx) error {
		for _, d := range dispositions {
			if err := tx.gateFollowUpFilingSource(ctx, domain.WorkUnitDeclaration{RunID: artifact.RunID}, domain.FollowUpFilingSource{
				Kind: domain.FollowUpSourceDeferredDisposition, FindingID: d.FindingID, AdjudicationDigest: artifact.Digest,
			}); err != nil {
				return err
			}
		}
		if got := tx.reviewValidation; got.scopeChecks != len(dispositions) || got.adjudicationChecks != 1 || got.reviewRecordChecks != 1 {
			t.Fatalf("N filing gates executed scope/adjudication/review checks %d/%d/%d, want %d/1/1",
				got.scopeChecks, got.adjudicationChecks, got.reviewRecordChecks, len(dispositions))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"write", "internal"} {
		t.Run(mode, func(t *testing.T) {
			check := func(tx *ReadTx) error {
				for pass := 1; pass <= 2; pass++ {
					if _, err := tx.ListFindingDispositions(ctx, artifact.RunID); err != nil {
						return err
					}
					got := tx.reviewValidation
					if got.marks != nil || got.scopeChecks != pass*len(dispositions) || got.adjudicationChecks != pass || got.reviewRecordChecks != pass {
						t.Fatalf("load %d leaked marks or repeated joins: %+v", pass, got)
					}
				}
				return nil
			}
			if err := reviewValidationWrite(st, ctx, mode, check); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func reviewValidationWrite(st *Store, ctx context.Context, mode string, fn func(*ReadTx) error) error {
	if mode == "write" {
		return st.Write(ctx, func(tx *WriteTx) error { return fn(&tx.ReadTx) })
	}
	return st.WriteInternal(ctx, func(tx *InternalTx) error { return fn(&tx.ReadTx) })
}

func TestReviewValidationReuseStopsAtWritesAndTransactions(t *testing.T) {
	for _, mode := range []string{"read", "write", "internal"} {
		for name, sql := range map[string]string{
			"disposition":       `UPDATE finding_dispositions SET reason = 'forged'`,
			"adjudication":      `UPDATE finding_adjudications SET approved_spec_digest = 'forged'`,
			"review membership": `UPDATE review_record_findings SET ordinal = 99 WHERE finding_id = 'reuse-finding-0'`,
		} {
			t.Run(mode+"/"+name, func(t *testing.T) {
				st, artifact, _ := seedReviewValidation(t, 2, 2)
				ctx := t.Context()
				read := func(tx *ReadTx) error { _, err := tx.ListFindingDispositions(ctx, artifact.RunID); return err }
				refuse := func(tx *ReadTx) error {
					for range 2 {
						if err := read(tx); err == nil {
							t.Fatal("read reused authority from before the tamper")
						}
					}
					return nil
				}
				if mode == "read" {
					if err := st.Read(ctx, read); err != nil {
						t.Fatal(err)
					}
					if _, err := st.db.ExecContext(ctx, sql); err != nil {
						t.Fatal(err)
					}
					if err := st.Read(ctx, refuse); err != nil {
						t.Fatal(err)
					}
				} else if err := reviewValidationWrite(st, ctx, mode, func(tx *ReadTx) error {
					if err := read(tx); err != nil {
						return err
					}
					if _, err := tx.tx.ExecContext(ctx, sql); err != nil {
						return err
					}
					if err := refuse(tx); err != nil {
						return err
					}
					if tx.reviewValidation.marks != nil {
						t.Fatal("failed load leaked marks")
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestReviewValidationReuseSeesNewDisposition(t *testing.T) {
	st, artifact, dispositions := seedReviewValidation(t, 2, 1)
	ctx := t.Context()
	if err := st.Write(ctx, func(tx *WriteTx) error {
		before, err := tx.ListFindingDispositions(ctx, artifact.RunID)
		if err != nil {
			return err
		}
		if len(before) != 1 {
			t.Fatalf("before = %d", len(before))
		}
		if err := tx.PutFindingDisposition(ctx, dispositions[1]); err != nil {
			return err
		}
		after, err := tx.ListFindingDispositions(ctx, artifact.RunID)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(after, dispositions) {
			t.Fatalf("new disposition invisible: %v", after)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestReviewValidationReusePathCancellationAndCopies(t *testing.T) {
	st, artifact, _ := seedReviewValidation(t, 2, 2)
	ctx := t.Context()
	if err := st.Read(ctx, func(tx *ReadTx) error {
		first, err := tx.GetFindingAdjudication(ctx, artifact.Digest)
		if err != nil {
			return err
		}
		first.Entries[0].Rationale = "caller mutation"
		again, err := tx.GetFindingAdjudication(ctx, artifact.Digest)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(again, artifact) {
			t.Fatal("returned artifact shared mutable data")
		}
		pathCtx, err := publicationReadContext(ctx, "enclosing-read")
		if err != nil {
			return err
		}
		if _, err := tx.GetFindingAdjudication(pathCtx, artifact.Digest); err != nil {
			return err
		}
		if tx.reviewValidation.adjudicationChecks != 2 {
			t.Fatal("different path reused adjudication validation")
		}
		for _, readCtx := range []context.Context{ctx, pathCtx} {
			if _, err := tx.ListFindingDispositions(readCtx, artifact.RunID); err != nil {
				return err
			}
		}
		if tx.reviewValidation.scopeChecks != 4 {
			t.Fatal("different path reused disposition scope validation")
		}
		row := mustReviewValidationRow(t, ctx, tx, artifact.Digest)
		for name, mutate := range map[string]func(*findingAdjudicationRow){
			"body digest":   func(row *findingAdjudicationRow) { row.bodyDigest = "forged" },
			"copied column": func(row *findingAdjudicationRow) { row.approvedSpecDigest = "forged" },
			"invalid body": func(row *findingAdjudicationRow) {
				row.body = []byte(`{}`)
				row.bodyDigest = reviewBodyDigest(string(row.body))
			},
		} {
			changed := row
			mutate(&changed)
			if _, err := tx.reconstructFindingAdjudication(ctx, changed); err == nil {
				t.Fatalf("warm validation hid changed %s", name)
			}
		}
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := tx.reconstructFindingAdjudication(canceled, row); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled hit = %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func mustReviewValidationRow(t *testing.T, ctx context.Context, tx *ReadTx, digest domain.Digest) findingAdjudicationRow {
	t.Helper()
	row, err := scanFindingAdjudicationRow(tx.tx.QueryRowContext(ctx, `SELECT `+selectFindingAdjudicationColumns+` FROM finding_adjudications WHERE content_digest = ?`, digest))
	if err != nil {
		t.Fatal(err)
	}
	return row
}
