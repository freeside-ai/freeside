package store

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// seedManualSubmissionTarget stores the project and specification artifact a
// manual submission binds to, and returns a lineless record for them.
func seedManualSubmissionTarget(t *testing.T, ctx context.Context, st *Store) domain.ManualSubmission {
	t.Helper()
	digest := domain.Digest("sha256:" + strings.Repeat("ab", 32))
	artifact, err := domain.NewArtifact(domain.ArtifactInput{
		ID: "artifact-spec", Type: domain.ArtifactKindSpecification, Digest: digest,
		Provenance: domain.Provenance{
			ProducerClass: domain.ProducerDaemon, ProducerInvocationID: "submit-spec",
			HeadBinding: domain.HeadIndependent, SensitivityClass: domain.SensitivityNormal,
		},
		Metadata: domain.EvidenceMetadata{
			MediaType: domain.EvidenceMediaTextMarkdown, SizeBytes: 10, CreatedAt: time.Now().UTC(),
			Source: domain.EvidenceSourceRun, Availability: domain.EvidenceAvailable,
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *WriteTx) error {
		if err := tx.RegisterProject(ctx, domain.Project{
			ID: "proj-1", Repo: originTestRepository, RepositoryID: originTestRepositoryID,
		}); err != nil {
			return err
		}
		return tx.PutArtifact(ctx, artifact)
	}); err != nil {
		t.Fatal(err)
	}
	return domain.ManualSubmission{
		Identity: "cli:lines", ProjectID: "proj-1", SourceArtifactID: artifact.ID,
		SourceDigest: digest, RequestDigest: domain.Digest("sha256:" + strings.Repeat("cd", 32)),
		ImplementationRunID: "run-implementation",
	}
}

func acceptManualSubmission(t *testing.T, ctx context.Context, st *Store, s domain.ManualSubmission) error {
	t.Helper()
	return st.Write(ctx, func(tx *WriteTx) error {
		_, err := tx.AcceptManualSubmission(ctx, s)
		return err
	})
}

func readManualSubmission(t *testing.T, ctx context.Context, st *Store, identity string) (domain.ManualSubmission, error) {
	t.Helper()
	var s domain.ManualSubmission
	err := st.Read(ctx, func(tx *ReadTx) error {
		var err error
		s, err = tx.GetManualSubmission(ctx, identity)
		return err
	})
	return s, err
}

// TestManualSubmissionLinesRoundTrip: a request with lines stores them and a
// lineless one stores NULL. Another request under the identity conflicts.
func TestManualSubmissionLinesRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTemplateStoreAt(t, t.TempDir()+"/manual-submission-round-trip.db", Options{})
	plain := seedManualSubmissionTarget(t, ctx, st)
	if err := acceptManualSubmission(t, ctx, st, plain); err != nil {
		t.Fatal(err)
	}
	var stored sql.NullString
	if err := st.db.QueryRowContext(ctx, `SELECT task_lines FROM manual_submissions WHERE identity = ?`, plain.Identity).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored.Valid {
		t.Fatalf("a lineless request stored %q, want NULL", stored.String)
	}

	lined := plain
	lined.Identity = "client:lines"
	lined.ImplementationRunID = "run-implementation-lines"
	lined.TaskLines = []domain.TaskLineChoice{
		{Role: domain.RoleImplementer, Agent: "claude-b"}, {Role: domain.RoleReviewer, Agent: "codex"},
	}
	if err := acceptManualSubmission(t, ctx, st, lined); err != nil {
		t.Fatal(err)
	}
	got, err := readManualSubmission(t, ctx, st, lined.Identity)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, lined) {
		t.Fatalf("submission = %+v, want %+v", got, lined)
	}
	if err := acceptManualSubmission(t, ctx, st, lined); err != nil {
		t.Fatalf("idempotent retry: %v", err)
	}
	other := lined
	other.RequestDigest = lined.SourceDigest
	if err := acceptManualSubmission(t, ctx, st, other); !errors.Is(err, ErrImmutableConflict) {
		t.Fatalf("another request under the identity: error = %v, want ErrImmutableConflict", err)
	}
}

// TestManualSubmissionLinesOutOfFormFailClosed: the read re-runs the record's
// validation, so an edited column is an inconsistent row, not a set of lines
// admission would bind to. A task that names the submission fails with it.
func TestManualSubmissionLinesOutOfFormFailClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTemplateStoreAt(t, t.TempDir()+"/manual-submission-corrupt.db", Options{})
	s := seedManualSubmissionTarget(t, ctx, st)
	if err := acceptManualSubmission(t, ctx, st, s); err != nil {
		t.Fatal(err)
	}
	for name, column := range map[string]string{
		"not json":     `not json`,
		"empty":        `[]`,
		"out of order": `[{"role":"reviewer","agent":"codex"},{"role":"implementer","agent":"codex"}]`,
		"bad role":     `[{"role":"shadow_reviewer","agent":"codex"}]`,
		"bad agent":    `[{"role":"reviewer","agent":"Not An Agent"}]`,
	} {
		if _, err := st.db.ExecContext(ctx, `UPDATE manual_submissions SET task_lines = ? WHERE identity = ?`, column, s.Identity); err != nil {
			t.Fatal(err)
		}
		if _, err := readManualSubmission(t, ctx, st, s.Identity); !errors.Is(err, errRowInconsistent) {
			t.Errorf("%s: read = %v, want errRowInconsistent", name, err)
		}
	}
}
