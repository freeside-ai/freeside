package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func testImageUpkeepRefusal(t *testing.T, recordedAt time.Time) domain.ImageUpkeepRefusal {
	t.Helper()
	refusal, err := domain.NewImageUpkeepRefusal(domain.ImageUpkeepRefusalInput{
		RepositoryID: 1278475858, Repository: "freeasinbird/gh-imgup",
		Harness: domain.HarnessClientClaudeCode, TipSHA: strings.Repeat("e", 40),
		MeasuredFromImageID: domain.Digest("sha256:" + strings.Repeat("ab", 32)),
		Class:               domain.ImageUpkeepRefusalRepair,
		Findings: []domain.ImageUpkeepClauseFinding{{
			Clause: domain.RebuildRefusalLockfileInconsistent, Detail: "lockfile root disagrees",
		}},
		RecordedAt: recordedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	return refusal
}

func TestImageUpkeepRefusalRecordConverges(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openProjectImageStore(t)
	first := testImageUpkeepRefusal(t, time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC))
	before, err := s.ServerState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	record := func(refusal domain.ImageUpkeepRefusal) domain.ImageUpkeepRefusal {
		t.Helper()
		var stored domain.ImageUpkeepRefusal
		if err := s.WriteInternal(ctx, func(tx *InternalTx) error {
			var err error
			stored, err = tx.RecordImageUpkeepRefusal(ctx, refusal)
			return err
		}); err != nil {
			t.Fatalf("record: %v", err)
		}
		return stored
	}
	if stored := record(first); stored.ID != first.ID || !stored.RecordedAt.Equal(first.RecordedAt) {
		t.Fatalf("first record = %+v, want %+v", stored, first)
	}
	// A rerun that refuses the same way later converges on the first row and
	// keeps its recording time.
	rerun := testImageUpkeepRefusal(t, first.RecordedAt.Add(time.Hour))
	if stored := record(rerun); stored.ID != first.ID || !stored.RecordedAt.Equal(first.RecordedAt) {
		t.Fatalf("rerun record = %+v, want the first row", stored)
	}
	after, err := s.ServerState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision != before.Revision {
		t.Fatalf("revision %d -> %d, want daemon-internal write", before.Revision, after.Revision)
	}

	otherRecord, err := domain.NewImageUpkeepRefusal(domain.ImageUpkeepRefusalInput{
		RepositoryID: first.RepositoryID, Repository: first.Repository, Harness: first.Harness,
		TipSHA: first.TipSHA, MeasuredFromImageID: first.MeasuredFromImageID,
		Class: domain.ImageUpkeepRefusalHostFault, RecordedAt: first.RecordedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	record(otherRecord)

	var (
		got     domain.ImageUpkeepRefusal
		listed  []domain.ImageUpkeepRefusal
		missing error
	)
	if err := s.Read(ctx, func(tx *ReadTx) error {
		var err error
		if got, err = tx.GetImageUpkeepRefusal(ctx, first.ID); err != nil {
			return err
		}
		if listed, err = tx.ListImageUpkeepRefusals(ctx, first.RepositoryID); err != nil {
			return err
		}
		_, missing = tx.GetImageUpkeepRefusal(ctx, domain.Digest("sha256:"+strings.Repeat("0", 64)))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got.ID != first.ID || len(got.Findings) != 1 {
		t.Fatalf("get = %+v, want %+v", got, first)
	}
	if len(listed) != 2 || listed[0].ID != first.ID || listed[1].ID != otherRecord.ID {
		t.Fatalf("list = %+v, want the two records in recording order", listed)
	}
	if !errors.Is(missing, ErrNotFound) {
		t.Fatalf("get missing = %v, want ErrNotFound", missing)
	}
}

func TestImageUpkeepRefusalReadFailsClosed(t *testing.T) {
	t.Parallel()
	for name, tamper := range map[string]string{
		"class column edited":  `UPDATE image_upkeep_refusals SET class = 'policy'`,
		"tip column edited":    `UPDATE image_upkeep_refusals SET tip_sha = '` + strings.Repeat("f", 40) + `'`,
		"body class edited":    `UPDATE image_upkeep_refusals SET body = replace(body, '"repair"', '"policy"'), class = 'policy'`,
		"recorded_at edited":   `UPDATE image_upkeep_refusals SET recorded_at = '2026-10-27T00:00:00Z'`,
		"repository id edited": `UPDATE image_upkeep_refusals SET repository_id = 7`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			s := openProjectImageStore(t)
			refusal := testImageUpkeepRefusal(t, time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC))
			if err := s.WriteInternal(ctx, func(tx *InternalTx) error {
				_, err := tx.RecordImageUpkeepRefusal(ctx, refusal)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.ExecContext(ctx, tamper); err != nil {
				t.Fatalf("tamper: %v", err)
			}
			err := s.Read(ctx, func(tx *ReadTx) error {
				_, err := tx.GetImageUpkeepRefusal(ctx, refusal.ID)
				return err
			})
			if err == nil || errors.Is(err, ErrNotFound) {
				t.Fatalf("get after tamper: err = %v, want a fail-closed decode", err)
			}
		})
	}
}

func TestRepositoryTipObservationUpsertAndReadBack(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openProjectImageStore(t)
	read := func() (domain.RepositoryTipObservation, bool, error) {
		t.Helper()
		var (
			observation domain.RepositoryTipObservation
			found       bool
		)
		err := s.Read(ctx, func(tx *ReadTx) error {
			var err error
			observation, found, err = tx.GetRepositoryTipObservation(ctx, 1278475858)
			return err
		})
		return observation, found, err
	}
	if _, found, err := read(); err != nil || found {
		t.Fatalf("empty store: found=%v err=%v, want absent", found, err)
	}
	observation := domain.RepositoryTipObservation{
		RepositoryID: 1278475858, Repository: "freeasinbird/gh-imgup", Branch: "main",
		TipSHA: strings.Repeat("e", 40), ObservedAt: time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC),
	}
	put := func(o domain.RepositoryTipObservation) {
		t.Helper()
		if err := s.WriteInternal(ctx, func(tx *InternalTx) error {
			return tx.RecordRepositoryTipObservation(ctx, o)
		}); err != nil {
			t.Fatalf("put: %v", err)
		}
	}
	put(observation)
	if got, found, err := read(); err != nil || !found || got != observation {
		t.Fatalf("read = %+v found=%v err=%v, want %+v", got, found, err, observation)
	}
	advanced := observation
	advanced.TipSHA, advanced.UpkeepTipSHA = strings.Repeat("f", 40), strings.Repeat("e", 40)
	advanced.ObservedAt = observation.ObservedAt.Add(time.Hour)
	put(advanced)
	if got, found, err := read(); err != nil || !found || got != advanced {
		t.Fatalf("read after upsert = %+v found=%v err=%v, want %+v", got, found, err, advanced)
	}

	if _, err := s.db.ExecContext(ctx,
		`UPDATE repository_tip_observations SET upkeep_tip_sha = NULL`); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	if _, found, err := read(); !errors.Is(err, ErrRowInconsistent) || found {
		t.Fatalf("read after tamper: found=%v err=%v, want ErrRowInconsistent", found, err)
	}
}
