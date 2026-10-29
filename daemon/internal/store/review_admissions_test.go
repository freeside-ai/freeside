package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func storedReviewAdmission(t *testing.T, role domain.RoleName) domain.ReviewAdmission {
	t.Helper()
	digest := func(fill string) domain.Digest { return domain.Digest("sha256:" + strings.Repeat(fill, 64)) }
	admission, err := domain.NewReviewAdmission(domain.ReviewAdmission{
		InvocationID: "review-1", RunID: "run-1", Role: role,
		AuthIdentityID: "auth-1", LineageGroup: "openai",
		AgentBinding: domain.AdmissionAgentBinding{
			AgentDigest: digest("a"), LaunchDigest: digest("b"), TreatmentDigest: digest("d"),
			PricingRevision: "pricing-2026-01", LineupRevision: digest("c"),
			EnrollmentID: "enroll-1", EnrollmentGeneration: 1, StoreManifestDigest: digest("9"),
			EffectiveEgress: []string{"chatgpt.com:443"},
			RouteModelID:    "gpt-5.2-codex", RequestedEffort: domain.EffortHigh, NativeEffort: "high",
			SelectionSource: domain.AgentSelectionSourceLineup,
		},
		AdmittedAt: time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	return admission
}

// TestReviewAdmissionIsWrittenOnce pins the first-snapshot-wins rule at the
// row: the same snapshot replays, and a different one for the invocation is
// a conflict that leaves the first in place.
func TestReviewAdmissionIsWrittenOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTemplateStoreAt(t, t.TempDir()+"/admissions.db", Options{})
	run := domain.Run{ID: "run-1", ProjectID: "project", SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy"}
	first := storedReviewAdmission(t, domain.RoleReviewer)
	if err := st.Read(ctx, func(tx *ReadTx) error {
		_, err := tx.GetReviewAdmission(ctx, first.InvocationID)
		return err
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("read before admission = %v, want %v", err, ErrNotFound)
	}
	if err := st.Write(ctx, func(tx *WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		return tx.PutReviewAdmission(ctx, first)
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *WriteTx) error { return tx.PutReviewAdmission(ctx, first) }); err != nil {
		t.Fatalf("replay of the same snapshot = %v", err)
	}
	for name, mutate := range map[string]func(*domain.ReviewAdmission){
		"another agent":    func(a *domain.ReviewAdmission) { a.AgentBinding.AgentDigest = a.AgentBinding.LaunchDigest },
		"another identity": func(a *domain.ReviewAdmission) { a.AuthIdentityID = "auth-2" },
		"a later lineup":   func(a *domain.ReviewAdmission) { a.AgentBinding.LineupRevision = a.AgentBinding.LaunchDigest },
		"a later instant":  func(a *domain.ReviewAdmission) { a.AdmittedAt = a.AdmittedAt.Add(time.Second) },
		"the other arm":    func(a *domain.ReviewAdmission) { a.Role = domain.RoleShadowReviewer },
	} {
		second := first
		mutate(&second)
		if err := st.Write(ctx, func(tx *WriteTx) error {
			return tx.PutReviewAdmission(ctx, second)
		}); !errors.Is(err, ErrImmutableConflict) {
			t.Errorf("second snapshot naming %s = %v, want %v", name, err, ErrImmutableConflict)
		}
	}
	invalid := first
	// No card selects a review agent.
	invalid.AgentBinding.SelectionSource = domain.AgentSelectionSourceCard
	invalid.AgentBinding.SelectionRecordID = invalid.AgentBinding.AgentDigest
	if err := st.Write(ctx, func(tx *WriteTx) error {
		return tx.PutReviewAdmission(ctx, invalid)
	}); !errors.Is(err, domain.ErrReviewAdmissionInvalid) {
		t.Fatalf("invalid snapshot = %v, want %v", err, domain.ErrReviewAdmissionInvalid)
	}
	if err := st.Read(ctx, func(tx *ReadTx) error {
		got, err := tx.GetReviewAdmission(ctx, first.InvocationID)
		if err == nil && !got.Equal(first) {
			t.Fatalf("stored snapshot = %#v, want the first %#v", got, first)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// TestReviewAdmissionRowTamper proves a row edited in place does not read
// back: the body digest and every copied column are held to the decoded body.
func TestReviewAdmissionRowTamper(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	shadowBody := func(t *testing.T) string {
		t.Helper()
		shadow := storedReviewAdmission(t, domain.RoleShadowReviewer)
		body, err := encode(shadow)
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	for name, query := range map[string]func(*testing.T) (string, []any){
		"run":  func(*testing.T) (string, []any) { return "UPDATE review_admissions SET run_id='run-2'", nil },
		"role": func(*testing.T) (string, []any) { return "UPDATE review_admissions SET role='shadow_reviewer'", nil },
		"agent": func(*testing.T) (string, []any) {
			return "UPDATE review_admissions SET agent_digest='sha256:other'", nil
		},
		"time": func(*testing.T) (string, []any) { return "UPDATE review_admissions SET admitted_at='other'", nil },
		"body": func(t *testing.T) (string, []any) {
			return "UPDATE review_admissions SET body=?", []any{shadowBody(t)}
		},
		"invalid body": func(*testing.T) (string, []any) { return "UPDATE review_admissions SET body='{}'", nil },
		"key":          func(*testing.T) (string, []any) { return "UPDATE review_admissions SET invocation_id='review-2'", nil },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			st := openTemplateStoreAt(t, t.TempDir()+"/admissions.db", Options{})
			admission := storedReviewAdmission(t, domain.RoleReviewer)
			if err := st.Write(ctx, func(tx *WriteTx) error {
				for _, id := range []domain.RunID{"run-1", "run-2"} {
					if err := tx.PutRun(ctx, domain.Run{
						ID: id, ProjectID: "project", SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
					}); err != nil {
						return err
					}
				}
				return tx.PutReviewAdmission(ctx, admission)
			}); err != nil {
				t.Fatal(err)
			}
			statement, args := query(t)
			if _, err := st.db.ExecContext(ctx, statement, args...); err != nil {
				t.Fatal(err)
			}
			id := admission.InvocationID
			if name == "key" {
				id = "review-2"
			}
			if err := st.Read(ctx, func(tx *ReadTx) error {
				_, err := tx.GetReviewAdmission(ctx, id)
				return err
			}); err == nil || errors.Is(err, ErrNotFound) {
				t.Fatalf("tampered row read back: %v", err)
			}
		})
	}
}

// TestReviewAdmissionWithTheAttendedFlagReconstructs: a review admission
// recorded before plan revision 97 carries "attended": false. It still reads
// back, and a resumed invocation's replay of it is not a conflict, because
// the binding re-encodes to the same bytes (#2265).
func TestReviewAdmissionWithTheAttendedFlagReconstructs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTemplateStoreAt(t, t.TempDir()+"/admissions.db", Options{})
	run := domain.Run{ID: "run-1", ProjectID: "project", SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy"}
	legacy := storedReviewAdmission(t, domain.RoleReviewer)
	attended := false
	legacy.AgentBinding.LegacyAttended = &attended
	if err := st.Write(ctx, func(tx *WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		return tx.PutReviewAdmission(ctx, legacy)
	}); err != nil {
		t.Fatal(err)
	}
	var body string
	if err := st.db.QueryRowContext(ctx,
		`SELECT body FROM review_admissions WHERE invocation_id = ?`, legacy.InvocationID).Scan(&body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, `"attended":false`) {
		t.Fatalf("stored body %s does not carry the attended flag", body)
	}
	var got domain.ReviewAdmission
	if err := st.Read(ctx, func(tx *ReadTx) error {
		var err error
		got, err = tx.GetReviewAdmission(ctx, legacy.InvocationID)
		return err
	}); err != nil {
		t.Fatalf("read a review admission carrying the attended flag: %v", err)
	}
	if !got.Equal(legacy) {
		t.Fatalf("read back %+v, want %+v", got, legacy)
	}
	if err := st.Write(ctx, func(tx *WriteTx) error { return tx.PutReviewAdmission(ctx, got) }); err != nil {
		t.Fatalf("replay of the stored snapshot = %v", err)
	}
}
