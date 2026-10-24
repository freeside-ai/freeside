package store

import (
	"reflect"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// TestReviewValidationEquivalence checks that a disposition load agrees with
// itself across repeated reads and between read and write transactions, on an
// intact store and under each forged row. Each mutation runs alone, then in a
// few combinations that forge two links of one binding, then all together.
func TestReviewValidationEquivalence(t *testing.T) {
	mutations := map[string]string{
		"disposition reason":       `UPDATE finding_dispositions SET reason = 'forged'`,
		"adjudication spec":        `UPDATE finding_adjudications SET approved_spec_digest = 'forged'`,
		"review record head":       `UPDATE review_records SET head_sha = 'forged'`,
		"finding ordinal":          `UPDATE review_record_findings SET ordinal = 99 WHERE finding_id = 'reuse-finding-0'`,
		"disposition body digest":  `UPDATE finding_dispositions SET body_digest = 'forged'`,
		"adjudication body digest": `UPDATE finding_adjudications SET body_digest = 'forged'`,
		"review record body":       `UPDATE review_records SET body_digest = 'forged'`,
	}
	cases := [][]string{
		nil,
		{"disposition reason", "disposition body digest"},
		{"review record head", "finding ordinal"},
		{"adjudication spec", "review record body"},
	}
	all := make([]string, 0, len(mutations))
	for name := range mutations {
		cases = append(cases, []string{name})
		all = append(all, name)
	}
	cases = append(cases, all)
	type result struct {
		Dispositions []domain.ReviewDispositionRecord
		Error        string
	}
	for _, applied := range cases {
		name := "none"
		if len(applied) == len(mutations) {
			name = "all"
		} else if len(applied) > 0 {
			name = strings.Join(applied, " + ")
		}
		t.Run(name, func(t *testing.T) {
			st, artifact, _ := seedReviewValidation(t, 2, 2)
			ctx := t.Context()
			for _, mutation := range applied {
				if _, err := st.db.ExecContext(ctx, mutations[mutation]); err != nil {
					t.Fatal(err)
				}
			}
			for _, path := range []string{"", "outer-publication"} {
				readCtx := ctx
				if path != "" {
					var err error
					readCtx, err = publicationReadContext(ctx, path)
					if err != nil {
						t.Fatal(err)
					}
				}
				for _, boundary := range []struct {
					run   domain.RunID
					round int
				}{
					{"", 0}, {artifact.RunID, 1}, {artifact.RunID, 2}, {"other-run", 2},
				} {
					read := func(tx *ReadTx) result {
						rows, err := tx.loadFindingDispositionsAtDecision(readCtx, boundary.run, boundary.round)
						out := result{Dispositions: rows}
						if err != nil {
							out.Error = err.Error()
						}
						return out
					}
					var first result
					if err := st.Read(ctx, func(tx *ReadTx) error {
						first = read(tx)
						if again := read(tx); !reflect.DeepEqual(first, again) {
							t.Fatalf("repeated read changed result: first %+v, again %+v", first, again)
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
					if err := st.WriteInternal(ctx, func(tx *InternalTx) error {
						if written := read(&tx.ReadTx); !reflect.DeepEqual(first, written) {
							t.Fatalf("write read changed result: read %+v, write %+v", first, written)
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				}
			}
		})
	}
}

func TestReviewValidationReuseDoesNotKeepFailedChecks(t *testing.T) {
	st, artifact, _ := seedReviewValidation(t, 2, 2)
	ctx := t.Context()
	if _, err := st.db.ExecContext(ctx, `UPDATE review_record_findings SET ordinal = 99 WHERE finding_id = 'reuse-finding-0'`); err != nil {
		t.Fatal(err)
	}
	if err := st.Read(ctx, func(tx *ReadTx) error {
		var previous string
		for pass := 1; pass <= 2; pass++ {
			_, err := tx.GetFindingAdjudication(ctx, artifact.Digest)
			if err == nil {
				t.Fatal("invalid binding accepted")
			}
			if pass == 2 && err.Error() != previous {
				t.Fatal("failed read changed error")
			}
			previous = err.Error()
			if tx.reviewValidation.adjudicationChecks != pass || tx.reviewValidation.reviewRecordChecks != pass {
				t.Fatal("failed cross-row validation was retained")
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// A boundary may authenticate scope without authorizing the disposition. A
// later unrestricted read must still check its final authority.
func TestReviewValidationReuseBoundaryDoesNotSkipBinding(t *testing.T) {
	st, artifact, _ := seedReviewValidation(t, 2, 2)
	ctx := t.Context()
	if err := st.Read(ctx, func(tx *ReadTx) error {
		rows, err := tx.loadFindingDispositionsAtDecision(ctx, artifact.RunID, 1)
		if err != nil {
			return err
		}
		if len(rows) != 0 || len(tx.reviewValidation.marks.bindings) != 0 {
			t.Fatal("boundary did not exclude bindings")
		}
		rows, err = tx.ListFindingDispositions(ctx, artifact.RunID)
		if err != nil {
			return err
		}
		if len(rows) != 2 || len(tx.reviewValidation.marks.bindings) != 2 || tx.reviewValidation.scopeChecks != 2 {
			t.Fatal("unrestricted read did not complete bindings with reused scope")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
