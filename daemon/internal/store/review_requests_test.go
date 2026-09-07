package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func TestReviewRequestPersistenceAndTamper(t *testing.T) {
	ctx := context.Background()
	for _, corruption := range []string{"none", "run", "round", "head", "body", "time"} {
		t.Run(corruption, func(t *testing.T) {
			st := openTemplateStoreAt(t, t.TempDir()+"/requests.db", Options{})
			run := domain.Run{ID: "run-1", ProjectID: "project", SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy"}
			r := domain.ReviewRequestRecord{InvocationID: "review-1", RunID: run.ID, Round: 1, BaseSHA: "base", HeadSHA: "head", RequestedAt: time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)}
			if err := st.Write(ctx, func(tx *WriteTx) error {
				if err := tx.PutRun(ctx, run); err != nil {
					return err
				}
				other := run
				other.ID = "run-2"
				if err := tx.PutRun(ctx, other); err != nil {
					return err
				}
				return tx.PutReviewRequest(ctx, r)
			}); err != nil {
				t.Fatal(err)
			}
			if err := st.Write(ctx, func(tx *WriteTx) error { return tx.PutReviewRequest(ctx, r) }); err != nil {
				t.Fatal(err)
			}
			changed := r
			changed.RequestedAt = changed.RequestedAt.Add(time.Second)
			if err := st.Write(ctx, func(tx *WriteTx) error { return tx.PutReviewRequest(ctx, changed) }); !errors.Is(err, ErrImmutableConflict) {
				t.Fatalf("changed request: %v", err)
			}
			alias := r
			alias.InvocationID = "alias"
			if err := st.Write(ctx, func(tx *WriteTx) error { return tx.PutReviewRequest(ctx, alias) }); err == nil {
				t.Fatal("duplicate round accepted")
			}
			mutations := map[string]string{
				"run":   "UPDATE review_requests SET run_id='run-2'",
				"round": "UPDATE review_requests SET round=2",
				"head":  "UPDATE review_requests SET head_sha='other'",
				"body":  "UPDATE review_requests SET body='{}'",
				"time":  "UPDATE review_requests SET requested_at='other'",
			}
			if query, ok := mutations[corruption]; ok {
				if _, err := st.db.ExecContext(ctx, query); err != nil {
					t.Fatal(err)
				}
			}
			err := st.Read(ctx, func(tx *ReadTx) error {
				rows, err := tx.ListReviewRequests(ctx, run.ID)
				if corruption == "none" && err == nil && (len(rows) != 1 || rows[0] != r) {
					t.Fatalf("roundtrip: %#v", rows)
				}
				return err
			})
			if (err != nil) != (corruption != "none") {
				t.Fatalf("read: %v", err)
			}
		})
	}
}
