package store

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// rewindDiscoveredModelCatalogs returns a store at head to the shape a daemon
// at 0126 holds.
func rewindDiscoveredModelCatalogs(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	for _, statement := range []string{
		`DROP TABLE discovered_model_catalogs`,
		`DELETE FROM schema_migrations WHERE name = '0127_discovered_model_catalogs.sql'`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
}

func discoveredCatalog(enrollment domain.ClientEnrollmentID, at time.Time, ids ...string) domain.DiscoveredModelCatalog {
	catalog := domain.DiscoveredModelCatalog{
		EnrollmentID: enrollment, HarnessBuild: "2.1.282 (Claude Code)", ObservedAt: at,
	}
	for _, id := range ids {
		catalog.Models = append(catalog.Models, domain.DiscoveredModel{
			ID: id, ResolvedModel: new("claude-" + id), DisplayName: id, EffortLevels: []string{"low", "high"},
		})
	}
	return catalog
}

func recordCatalog(t *testing.T, st *Store, catalog domain.DiscoveredModelCatalog) error {
	t.Helper()
	return st.WriteInternal(t.Context(), func(tx *InternalTx) error {
		return tx.RecordDiscoveredModelCatalog(t.Context(), catalog)
	})
}

func readCatalog(t *testing.T, st *Store, id domain.ClientEnrollmentID) (domain.DiscoveredModelCatalog, error) {
	t.Helper()
	var catalog domain.DiscoveredModelCatalog
	err := st.Read(t.Context(), func(tx *ReadTx) error {
		var err error
		catalog, err = tx.GetDiscoveredModelCatalog(t.Context(), id)
		return err
	})
	return catalog, err
}

func TestDiscoveredModelCatalogRoundTripsAndReplaces(t *testing.T) {
	st := openEnrollmentStore(t)
	recordEnrollmentFixtures(t, st)
	recordPoolEnrollment(t, st, "auth-2", "enroll-2", "", observationAt)

	if _, err := readCatalog(t, st, "enroll-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("catalog before discovery: err = %v, want ErrNotFound", err)
	}
	first := discoveredCatalog("enroll-1", observationAt, "opus", "sonnet")
	other := discoveredCatalog("enroll-2", observationAt, "haiku")
	for _, catalog := range []domain.DiscoveredModelCatalog{first, other} {
		if err := recordCatalog(t, st, catalog); err != nil {
			t.Fatal(err)
		}
	}
	got, err := readCatalog(t, st, "enroll-1")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, first) {
		t.Fatalf("catalog = %+v, want %+v", got, first)
	}

	second := discoveredCatalog("enroll-1", observationAt.Add(time.Hour), "opus")
	second.HarnessBuild = "2.1.318 (Claude Code)"
	if err := recordCatalog(t, st, second); err != nil {
		t.Fatal(err)
	}
	var listed []domain.DiscoveredModelCatalog
	if err := st.Read(t.Context(), func(tx *ReadTx) error {
		var err error
		listed, err = tx.ListDiscoveredModelCatalogs(t.Context())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if want := []domain.DiscoveredModelCatalog{second, other}; !reflect.DeepEqual(listed, want) {
		t.Fatalf("listed = %+v, want %+v", listed, want)
	}
}

func TestDiscoveredModelCatalogRefusals(t *testing.T) {
	st := openEnrollmentStore(t)
	recordEnrollmentFixtures(t, st)
	if err := recordCatalog(t, st, discoveredCatalog("enroll-unknown", observationAt, "opus")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown enrollment: err = %v, want ErrNotFound", err)
	}
	if err := recordCatalog(t, st, discoveredCatalog("enroll-1", observationAt)); !errors.Is(err, domain.ErrNoDiscoveredModels) {
		t.Fatalf("empty catalog: err = %v, want ErrNoDiscoveredModels", err)
	}
	if _, err := readCatalog(t, st, "enroll-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a refused write left a row: err = %v", err)
	}
}

func TestDiscoveredModelCatalogReadFailsOnABadRow(t *testing.T) {
	st := openEnrollmentStore(t)
	recordEnrollmentFixtures(t, st)
	if err := recordCatalog(t, st, discoveredCatalog("enroll-1", observationAt, "opus")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(t.Context(),
		`UPDATE discovered_model_catalogs SET body = json_set(body, '$.models', json('[]'))`); err != nil {
		t.Fatal(err)
	}
	if _, err := readCatalog(t, st, "enroll-1"); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("read of a bad row: err = %v, want a loud failure", err)
	}
}
