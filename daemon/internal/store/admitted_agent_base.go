package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// ErrAdmittedAgentBaseUnrecorded is returned (wrapped) when a pointer advance
// names a base the daemon did not record for that harness.
var ErrAdmittedAgentBaseUnrecorded = errors.New("admitted agent base is not a recorded base for the harness")

const (
	admittedAgentBaseColumns = `id, harness, image_ref, harness_build, source_repository_id,
    source_repository, source_commit, source_tree_sha, admitted_at`
	insertAdmittedAgentBaseSQL = `
INSERT INTO admitted_agent_bases (` + admittedAgentBaseColumns + `)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`
	getAdmittedAgentBaseSQL = `SELECT ` + admittedAgentBaseColumns + `
FROM admitted_agent_bases WHERE id = ?`
	getAdmittedAgentBaseByImageSQL = `SELECT ` + admittedAgentBaseColumns + `
FROM admitted_agent_bases WHERE image_ref = ?
ORDER BY admitted_at DESC, id LIMIT 1`
	upsertAdmittedAgentBasePointerSQL = `
INSERT INTO admitted_agent_base_pointers (harness, base_id, advanced_at)
VALUES (?, ?, ?)
ON CONFLICT (harness) DO UPDATE SET
    base_id = excluded.base_id,
    advanced_at = excluded.advanced_at`
	getAdmittedAgentBasePointerSQL = `
SELECT base_id, advanced_at FROM admitted_agent_base_pointers WHERE harness = ?`
)

// RecordAdmittedAgentBase write-once records one admitted agent base and
// returns the stored record. The record is content-addressed, so recording
// the same base again converges on the first row, keeping its admitted_at.
// Like project images, bases are daemon-internal, so this belongs on
// InternalTx and does not bump the server revision.
func (tx *InternalTx) RecordAdmittedAgentBase(
	ctx context.Context, base domain.AdmittedAgentBase,
) (domain.AdmittedAgentBase, error) {
	if err := base.Validate(); err != nil {
		return domain.AdmittedAgentBase{}, fmt.Errorf("record admitted agent base: %w", err)
	}
	existing, err := tx.GetAdmittedAgentBase(ctx, base.ID)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return domain.AdmittedAgentBase{}, fmt.Errorf("record admitted agent base: %w", err)
	}
	if _, err := tx.tx.ExecContext(ctx, insertAdmittedAgentBaseSQL,
		base.ID, base.Harness, base.ImageRef, base.HarnessBuild, base.SourceRepoID,
		base.SourceRepository, base.SourceCommit, base.SourceTreeSHA, formatTime(base.AdmittedAt),
	); err != nil {
		return domain.AdmittedAgentBase{}, fmt.Errorf("record admitted agent base %s: %w", base.ID, err)
	}
	return base, nil
}

// GetAdmittedAgentBase reads one recorded base by ID. The ID is rebuilt from
// the columns, so a row whose facts no longer address it fails closed with
// errRowInconsistent.
func (tx *ReadTx) GetAdmittedAgentBase(ctx context.Context, id domain.Digest) (domain.AdmittedAgentBase, error) {
	base, err := scanAdmittedAgentBase(tx.tx.QueryRowContext(ctx, getAdmittedAgentBaseSQL, id))
	if err != nil {
		return domain.AdmittedAgentBase{}, fmt.Errorf("get admitted agent base %s: %w", id, err)
	}
	if base.ID != id {
		return domain.AdmittedAgentBase{}, fmt.Errorf("get admitted agent base %s: %w", id, errRowInconsistent)
	}
	return base, nil
}

// FindAdmittedAgentBaseByImage reads the recorded base whose published image
// is ref, reporting absence rather than erroring. A rebuild uses it to tell a
// base the daemon admitted, which it builds FROM through the base's local
// tag, from a base the operator built. Sources that differ only outside
// what the build reads admit one image more than once; it reads the latest.
func (tx *ReadTx) FindAdmittedAgentBaseByImage(
	ctx context.Context, ref domain.ImageRef,
) (domain.AdmittedAgentBase, bool, error) {
	base, err := scanAdmittedAgentBase(tx.tx.QueryRowContext(ctx, getAdmittedAgentBaseByImageSQL, ref))
	if errors.Is(err, ErrNotFound) {
		return domain.AdmittedAgentBase{}, false, nil
	}
	if err != nil {
		return domain.AdmittedAgentBase{}, false, fmt.Errorf("find admitted agent base for %s: %w", ref, err)
	}
	if base.ImageRef != ref {
		return domain.AdmittedAgentBase{}, false, fmt.Errorf("find admitted agent base for %s: %w", ref, errRowInconsistent)
	}
	return base, true, nil
}

func scanAdmittedAgentBase(row *sql.Row) (domain.AdmittedAgentBase, error) {
	var (
		base                                   domain.AdmittedAgentBase
		id, harness, imageRef, admitted        string
		harnessBuild, repository, commit, tree string
		repositoryID                           int64
	)
	err := row.Scan(&id, &harness, &imageRef, &harnessBuild, &repositoryID,
		&repository, &commit, &tree, &admitted)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.AdmittedAgentBase{}, ErrNotFound
	}
	if err != nil {
		return domain.AdmittedAgentBase{}, err
	}
	admittedAt, err := parseTime(admitted)
	if err != nil {
		return domain.AdmittedAgentBase{}, err
	}
	base = domain.AdmittedAgentBase{
		ID: domain.Digest(id), Harness: domain.HarnessClientKind(harness),
		ImageRef: domain.ImageRef(imageRef), HarnessBuild: harnessBuild,
		SourceRepoID: repositoryID, SourceRepository: repository,
		SourceCommit: commit, SourceTreeSHA: tree, AdmittedAt: admittedAt,
	}
	// Validate rebuilds the ID from the other columns.
	if err := base.Validate(); err != nil {
		return domain.AdmittedAgentBase{}, errors.Join(errRowInconsistent, err)
	}
	return base, nil
}

// AdvanceAdmittedAgentBasePointer points a harness at a recorded base,
// creating or replacing the pointer. The base row must exist in this
// transaction and be of the pointer's harness, or the advance fails with
// ErrAdmittedAgentBaseUnrecorded.
func (tx *InternalTx) AdvanceAdmittedAgentBasePointer(
	ctx context.Context, pointer domain.AdmittedAgentBasePointer,
) error {
	if err := pointer.Validate(); err != nil {
		return fmt.Errorf("advance admitted agent base pointer: %w", err)
	}
	base, err := tx.GetAdmittedAgentBase(ctx, pointer.BaseID)
	if errors.Is(err, ErrNotFound) {
		return fmt.Errorf("advance admitted agent base pointer to %s: %w", pointer.BaseID, ErrAdmittedAgentBaseUnrecorded)
	}
	if err != nil {
		return fmt.Errorf("advance admitted agent base pointer: %w", err)
	}
	if base.Harness != pointer.Harness {
		return fmt.Errorf("advance admitted agent base pointer for %s to a %s base: %w",
			pointer.Harness, base.Harness, ErrAdmittedAgentBaseUnrecorded)
	}
	if _, err := tx.tx.ExecContext(ctx, upsertAdmittedAgentBasePointerSQL,
		pointer.Harness, pointer.BaseID, formatTime(pointer.AdvancedAt),
	); err != nil {
		return fmt.Errorf("advance admitted agent base pointer: %w", err)
	}
	return nil
}

// GetAdmittedAgentBasePointer reads a harness's pointer together with the
// base it names, reporting absence rather than erroring. The base row is
// re-read and must be of the pointer's harness, or the read fails closed
// with errRowInconsistent.
func (tx *ReadTx) GetAdmittedAgentBasePointer(
	ctx context.Context, harness domain.HarnessClientKind,
) (domain.AdmittedAgentBasePointer, domain.AdmittedAgentBase, bool, error) {
	var baseID, advanced string
	err := tx.tx.QueryRowContext(ctx, getAdmittedAgentBasePointerSQL, harness).Scan(&baseID, &advanced)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.AdmittedAgentBasePointer{}, domain.AdmittedAgentBase{}, false, nil
	}
	fail := func(err error) (domain.AdmittedAgentBasePointer, domain.AdmittedAgentBase, bool, error) {
		return domain.AdmittedAgentBasePointer{}, domain.AdmittedAgentBase{}, false,
			fmt.Errorf("get admitted agent base pointer %s: %w", harness, err)
	}
	if err != nil {
		return fail(err)
	}
	advancedAt, err := parseTime(advanced)
	if err != nil {
		return fail(err)
	}
	pointer := domain.AdmittedAgentBasePointer{
		Harness: harness, BaseID: domain.Digest(baseID), AdvancedAt: advancedAt,
	}
	if err := pointer.Validate(); err != nil {
		return fail(errors.Join(errRowInconsistent, err))
	}
	base, err := tx.GetAdmittedAgentBase(ctx, pointer.BaseID)
	if errors.Is(err, ErrNotFound) {
		return fail(fmt.Errorf("base row missing: %w", errRowInconsistent))
	}
	if err != nil {
		return fail(err)
	}
	if base.Harness != harness {
		return fail(fmt.Errorf("base row is of harness %s: %w", base.Harness, errRowInconsistent))
	}
	return pointer, base, true, nil
}
