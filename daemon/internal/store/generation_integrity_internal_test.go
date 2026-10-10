package store

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/migrations"
)

// Credential-integrity marks at the store boundary (plan §5.4 admission rule
// 4, issue #1624): recording is first-observation-wins, a mark row is never
// read back on its own authority, and re-enrollment's new generation is
// unmarked. The admitting transaction's use of the gate is pinned beside the
// agent-bound admission tests.

var integrityMarkObservedAt = time.Date(2026, 1, 2, 3, 30, 0, 0, time.UTC)

func integrityMarkFor(
	generation domain.EnrollmentGeneration, finding domain.CredentialIntegrityFinding,
) domain.GenerationIntegrityMark {
	return domain.GenerationIntegrityMark{
		EnrollmentID: generation.EnrollmentID, Ordinal: generation.Ordinal,
		Finding: finding, ObservedAt: integrityMarkObservedAt,
	}
}

func recordIntegrityMark(t *testing.T, s *Store, mark domain.GenerationIntegrityMark) domain.GenerationIntegrityMark {
	t.Helper()
	ctx := context.Background()
	var stored domain.GenerationIntegrityMark
	if err := s.WriteInternal(ctx, func(tx *InternalTx) error {
		var err error
		stored, err = tx.RecordGenerationIntegrityMark(ctx, mark)
		return err
	}); err != nil {
		t.Fatalf("record integrity mark: %v", err)
	}
	return stored
}

// requireUnmarked runs the gate in a read transaction, as role resolution
// does.
func requireUnmarked(t *testing.T, s *Store, id domain.ClientEnrollmentID, ordinal int) error {
	t.Helper()
	ctx := context.Background()
	return s.Read(ctx, func(tx *ReadTx) error {
		return tx.RequireGenerationUnmarked(ctx, id, ordinal)
	})
}

// assertMarkedRefusal checks the typed refusal: the class through the
// sentinel and the exact mark through the concrete type.
func assertMarkedRefusal(t *testing.T, err error, want domain.GenerationIntegrityMark) {
	t.Helper()
	if !errors.Is(err, domain.ErrGenerationIntegrityMarked) {
		t.Fatalf("error = %v, want %v", err, domain.ErrGenerationIntegrityMarked)
	}
	var marked *domain.GenerationIntegrityMarkedError
	if !errors.As(err, &marked) || marked.Mark != want {
		t.Fatalf("refusal mark = %+v, want %+v", marked, want)
	}
}

// appendSecondGeneration re-enrolls the fixture enrollment seedAgentClosure
// left at generation 1: release the consumed fence, re-acquire against the
// current state, and append generation 2.
func appendSecondGeneration(t *testing.T, s *Store) domain.EnrollmentGeneration {
	t.Helper()
	ctx := context.Background()
	leaseStart := time.Date(2026, 1, 2, 3, 2, 0, 0, time.UTC)
	var stamped domain.EnrollmentGeneration
	if err := s.WriteInternal(ctx, func(tx *InternalTx) error {
		if err := tx.ReleaseAuthStoreMutationLease(
			ctx, "auth-1", "inv-refresh", 1, leaseStart.Add(2*time.Minute)); err != nil {
			return err
		}
		if _, err := tx.AcquireAuthStoreMutationLeaseBound(
			ctx, "auth-1", "inv-refresh", &domain.LeaseGenerationBinding{
				EnrollmentID: "enroll-1", Generation: 1,
				AuthStoreVolume: "codex-store", StoreManifestDigest: enrollmentManifest,
			}, leaseStart.Add(3*time.Minute), leaseStart.Add(10*time.Minute)); err != nil {
			return err
		}
		var err error
		stamped, err = tx.AppendEnrollmentGeneration(ctx, enrollmentEntry(2), leaseStart.Add(4*time.Minute))
		return err
	}); err != nil {
		t.Fatalf("append second generation: %v", err)
	}
	if stamped.Ordinal != 2 {
		t.Fatalf("second generation stamped ordinal %d", stamped.Ordinal)
	}
	return stamped
}

func integrityMarkBodies(t *testing.T, s *Store) map[string]string {
	t.Helper()
	rows, err := s.db.QueryContext(context.Background(), `
SELECT enrollment_id || '/' || ordinal || '/' || finding, observed_at || ' ' || body
FROM generation_integrity_marks`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	bodies := map[string]string{}
	for rows.Next() {
		var key, body string
		if err := rows.Scan(&key, &body); err != nil {
			t.Fatal(err)
		}
		bodies[key] = body
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return bodies
}

func TestRecordGenerationIntegrityMark(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	for _, finding := range domain.AllCredentialIntegrityFindings {
		t.Run("records and reads back "+string(finding), func(t *testing.T) {
			s := openAgentAdmissionStore(t)
			generation := seedAgentClosure(t, s)
			if err := requireUnmarked(t, s, generation.EnrollmentID, generation.Ordinal); err != nil {
				t.Fatalf("unmarked generation = %v", err)
			}
			mark := integrityMarkFor(generation, finding)
			if stored := recordIntegrityMark(t, s, mark); stored != mark {
				t.Fatalf("recorded mark = %+v, want %+v", stored, mark)
			}
			var marks []domain.GenerationIntegrityMark
			if err := s.Read(ctx, func(tx *ReadTx) error {
				var err error
				marks, err = tx.GenerationIntegrityMarks(ctx, generation.EnrollmentID, generation.Ordinal)
				return err
			}); err != nil {
				t.Fatalf("read marks: %v", err)
			}
			if !reflect.DeepEqual(marks, []domain.GenerationIntegrityMark{mark}) {
				t.Fatalf("marks = %+v, want [%+v]", marks, mark)
			}
			assertMarkedRefusal(t, requireUnmarked(t, s, generation.EnrollmentID, generation.Ordinal), mark)
		})
	}

	t.Run("a repeat keeps the first observation", func(t *testing.T) {
		s := openAgentAdmissionStore(t)
		generation := seedAgentClosure(t, s)
		first := integrityMarkFor(generation, domain.CredentialIntegrityTruncation)
		recordIntegrityMark(t, s, first)
		before := integrityMarkBodies(t, s)
		later := first
		later.ObservedAt = first.ObservedAt.Add(time.Hour)
		if stored := recordIntegrityMark(t, s, later); stored != first {
			t.Fatalf("repeat returned %+v, want the first mark %+v", stored, first)
		}
		if after := integrityMarkBodies(t, s); !reflect.DeepEqual(after, before) || len(after) != 1 {
			t.Fatalf("repeat changed the stored rows: %v, was %v", after, before)
		}
	})

	t.Run("both findings mark one generation", func(t *testing.T) {
		s := openAgentAdmissionStore(t)
		generation := seedAgentClosure(t, s)
		truncation := integrityMarkFor(generation, domain.CredentialIntegrityTruncation)
		corruption := integrityMarkFor(generation, domain.CredentialIntegrityCorruption)
		recordIntegrityMark(t, s, truncation)
		recordIntegrityMark(t, s, corruption)
		// The refusal names the first mark in finding order, so it is
		// deterministic whichever the probe recorded first.
		assertMarkedRefusal(t, requireUnmarked(t, s, generation.EnrollmentID, generation.Ordinal), corruption)
	})

	refusals := []struct {
		name    string
		mutate  func(*domain.GenerationIntegrityMark)
		wantErr error
	}{
		{"a generation the enrollment never appended", func(m *domain.GenerationIntegrityMark) { m.Ordinal = 7 }, ErrNotFound},
		{"an enrollment that does not exist", func(m *domain.GenerationIntegrityMark) { m.EnrollmentID = "enroll-absent" }, ErrNotFound},
		{"an unknown finding", func(m *domain.GenerationIntegrityMark) { m.Finding = "expired" }, domain.ErrInvalidCredentialIntegrityFinding},
		{"an unpersisted ordinal", func(m *domain.GenerationIntegrityMark) { m.Ordinal = 0 }, domain.ErrNonPositive},
	}
	for _, tc := range refusals {
		t.Run("refuses "+tc.name, func(t *testing.T) {
			s := openAgentAdmissionStore(t)
			generation := seedAgentClosure(t, s)
			mark := integrityMarkFor(generation, domain.CredentialIntegrityTruncation)
			tc.mutate(&mark)
			err := s.WriteInternal(ctx, func(tx *InternalTx) error {
				_, err := tx.RecordGenerationIntegrityMark(ctx, mark)
				return err
			})
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("record = %v, want %v", err, tc.wantErr)
			}
			if rows := integrityMarkBodies(t, s); len(rows) != 0 {
				t.Fatalf("a refused mark left rows: %v", rows)
			}
		})
	}

	t.Run("a generation that does not exist is not unmarked", func(t *testing.T) {
		s := openAgentAdmissionStore(t)
		generation := seedAgentClosure(t, s)
		if err := requireUnmarked(t, s, generation.EnrollmentID, 7); !errors.Is(err, ErrNotFound) {
			t.Fatalf("require unmarked on an absent generation = %v, want %v", err, ErrNotFound)
		}
	})
}

// TestGenerationIntegrityMarkReconstructionFailsClosed pins the read side: a
// mark row that cannot be authenticated is an error, never an absent mark, so
// tampering with a row cannot reopen admission.
func TestGenerationIntegrityMarkReconstructionFailsClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const otherEnrollmentBody = `{"enrollment_id":"enroll-2","ordinal":1,` +
		`"finding":"truncation","observed_at":"2026-01-02T03:30:00Z"}`
	cases := []struct {
		name   string
		tamper string
		args   []any
		want   error
	}{
		{
			"observed_at column disagrees with the body",
			`UPDATE generation_integrity_marks SET observed_at = '2026-01-02T04:00:00Z'`, nil, errRowInconsistent,
		},
		{
			"finding column disagrees with the body",
			`UPDATE generation_integrity_marks SET finding = 'corruption'`, nil, errRowInconsistent,
		},
		{
			"body names another enrollment",
			`UPDATE generation_integrity_marks SET body = ?`,
			[]any{otherEnrollmentBody},
			errRowInconsistent,
		},
		{
			"body carries an unknown finding",
			`UPDATE generation_integrity_marks SET finding = 'expired', body = replace(body, 'truncation', 'expired')`,
			nil, domain.ErrInvalidCredentialIntegrityFinding,
		},
		{"body is not JSON", `UPDATE generation_integrity_marks SET body = 'not json'`, nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := openAgentAdmissionStore(t)
			generation := seedAgentClosure(t, s)
			recordIntegrityMark(t, s, integrityMarkFor(generation, domain.CredentialIntegrityTruncation))
			if _, err := s.db.ExecContext(ctx, tc.tamper, tc.args...); err != nil {
				t.Fatalf("tamper: %v", err)
			}
			readErr := s.Read(ctx, func(tx *ReadTx) error {
				_, err := tx.GenerationIntegrityMarks(ctx, generation.EnrollmentID, generation.Ordinal)
				return err
			})
			gateErr := requireUnmarked(t, s, generation.EnrollmentID, generation.Ordinal)
			for name, err := range map[string]error{"read": readErr, "gate": gateErr} {
				if err == nil {
					t.Fatalf("%s of a tampered mark row passed", name)
				}
				if tc.want != nil && !errors.Is(err, tc.want) {
					t.Fatalf("%s of a tampered mark row = %v, want %v", name, err, tc.want)
				}
			}
		})
	}
}

// TestReenrollmentClearsIntegrityMark pins how the refusal ends: generation
// N+1 has no mark, and generation N's mark is history that nothing edits.
func TestReenrollmentClearsIntegrityMark(t *testing.T) {
	t.Parallel()
	s := openAgentAdmissionStore(t)
	first := seedAgentClosure(t, s)
	mark := recordIntegrityMark(t, s, integrityMarkFor(first, domain.CredentialIntegrityTruncation))
	before := integrityMarkBodies(t, s)

	second := appendSecondGeneration(t, s)
	if err := requireUnmarked(t, s, second.EnrollmentID, second.Ordinal); err != nil {
		t.Fatalf("re-enrolled generation = %v, want unmarked", err)
	}
	assertMarkedRefusal(t, requireUnmarked(t, s, first.EnrollmentID, first.Ordinal), mark)
	if after := integrityMarkBodies(t, s); !reflect.DeepEqual(after, before) {
		t.Fatalf("re-enrollment changed the mark rows: %v, was %v", after, before)
	}
}

// TestGenerationIntegrityMarksMigration proves 0087 applies to a store at the
// previous schema head that already holds a generation, and leaves that
// generation's row untouched and unmarked.
func TestGenerationIntegrityMarksMigration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const selectGeneration = `
SELECT enrollment_id || '/' || ordinal || ' ' || auth_store_volume || ' ' || store_manifest_digest || ' ' ||
       lease_fence || ' ' || account_binding || ' ' || COALESCE(token_expiry, '') || ' ' || recorded_at || ' ' || body
FROM client_enrollment_generations`

	// The generation is written through the store's own accessors in a head
	// store, then copied row for row into a database stopped one migration
	// short: the three tables' shapes are the same at both schema versions.
	sourcePath := filepath.Join(t.TempDir(), "source.db")
	source := openTemplateStoreAt(t, sourcePath, Options{})
	generation := seedAgentClosure(t, source)
	if err := source.Close(); err != nil {
		t.Fatalf("close source store: %v", err)
	}

	path := filepath.Join(t.TempDir(), "store.db")
	db, err := openDB(path, Options{})
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	prior := map[string]string{}
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() == "0087_generation_integrity_marks.sql" ||
			entry.Name() == "0088_follow_up_filing_kind.sql" ||
			entry.Name() == "0089_follow_up_filing_ledger.sql" ||
			entry.Name() == "0090_external_finding_dispositions.sql" ||
			entry.Name() == "0091_project_image_environment.sql" ||
			entry.Name() == "0092_publish_audit_issues.sql" ||
			entry.Name() == "0093_follow_up_filing_dispatch_identity.sql" ||
			entry.Name() == "0094_device_activity.sql" ||
			entry.Name() == "0095_task_lines.sql" || entry.IsDir() {
			continue
		}
		body, err := fs.ReadFile(migrations.FS, entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		prior[entry.Name()] = string(body)
	}
	if err := migrate(ctx, db, mapFS(prior)); err != nil {
		t.Fatalf("migrate to 0086: %v", err)
	}
	if got := rawVersion(t, db); got != 86 {
		t.Fatalf("prior schema version = %d, want 86", got)
	}
	// ATTACH is per connection, so the copy runs on one pinned connection.
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`ATTACH DATABASE '` + sourcePath + `' AS source`,
		`INSERT INTO auth_identities SELECT * FROM source.auth_identities`,
		`INSERT INTO client_enrollments SELECT * FROM source.client_enrollments`,
		`INSERT INTO client_enrollment_generations SELECT * FROM source.client_enrollment_generations`,
		`DETACH DATABASE source`,
	} {
		if _, err := conn.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	var before string
	if err := db.QueryRowContext(ctx, selectGeneration).Scan(&before); err != nil {
		t.Fatalf("read the generation row before the migration: %v", err)
	}
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate to head: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	s, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	var after string
	if err := s.db.QueryRowContext(ctx, selectGeneration).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("the migration changed the generation row:\n got %s\nwant %s", after, before)
	}
	var current domain.EnrollmentGeneration
	if err := s.Read(ctx, func(tx *ReadTx) error {
		var err error
		if current, err = tx.CurrentEnrollmentGeneration(ctx, generation.EnrollmentID); err != nil {
			return err
		}
		return tx.RequireGenerationUnmarked(ctx, current.EnrollmentID, current.Ordinal)
	}); err != nil {
		t.Fatalf("a generation written before the migration: %v", err)
	}
	if !reflect.DeepEqual(current, generation) {
		t.Fatalf("migrated generation = %+v, want %+v", current, generation)
	}
}
