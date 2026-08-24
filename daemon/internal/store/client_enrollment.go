package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// Client enrollments and their append-only store generations (plan §5.4,
// issue #894). Daemon-internal records like the identities they hang off:
// never synchronized, writes on InternalTx with non-Put names, no
// entity_version/as_of_revision. Dormant until the #867 cutover creates the
// first enrollment; the accessors and their gates land with the contract so
// adoption has a proven boundary to write through.
//
// Trust posture: an enrollment or generation row is never read back on its
// own authority. Reconstruction re-runs the identity binding (the enrollment
// must still belong to an identity carrying its exact account binding) and
// the generation binding (parent key, account, expiry-per-method), so a row
// that would re-home a credential onto a different account fails closed at
// every read, not only at the write that tried to create it.

// ErrEnrollmentOrdinalSupplied is returned when a generation entry arrives
// carrying a nonzero ordinal: the ordinal is this store's append identity,
// and accepting a caller-supplied one would let a writer forge where its
// entry sits in the store history.
var ErrEnrollmentOrdinalSupplied = errors.New(
	"enrollment generation ordinal is store-assigned; an entry must arrive with ordinal zero")

// ErrEnrollmentLeaseNotHeld is returned when a generation append is not
// covered by the identity's live mutation lease at the caller's instant, or
// names a fence the lease has left behind. §5.4: the one lease fences every
// enrollment's store, so an append outside it would record a mutation nothing
// serialized.
var ErrEnrollmentLeaseNotHeld = errors.New(
	"enrollment generation append is not covered by the identity's live mutation lease")

// ErrEnrollmentLeaseBindingMismatch is returned when the live lease's fence
// does not name exactly the enrollment store being appended to: it carries no
// generation binding at all (an unbound interim fence guards no enrollment
// store), or a binding for a different enrollment or locator. The fence was
// taken to guard one exact store, and an append against another under the
// same fence would launder the exclusion (§5.4: every fence names enrollment
// id, generation, exact locator, and store manifest digest).
var ErrEnrollmentLeaseBindingMismatch = errors.New(
	"enrollment generation append is not covered by the lease's generation binding")

// ErrEnrollmentLeaseBindingStale is returned when the live lease's fence
// names the right enrollment store but not its current state: the binding's
// generation is not the store's newest ordinal, or its manifest digest is
// not the newest generation's. The mutation was computed from bytes the
// history has moved past, and stamping it MAX(ordinal)+1 would install a
// rollback as the current generation. The holder's path back is to release,
// re-read the store, and re-acquire against the state it actually mutates.
var ErrEnrollmentLeaseBindingStale = errors.New(
	"enrollment generation append starts from a store state the history has moved past")

// clientEnrollmentRecord is the persisted shape of an enrollment: the record
// plus the instant it was recorded, carried in the validated body and
// cross-checked against the column like every other extracted field.
// Package-private: a persistence format, not a contract shape the goldens pin.
type clientEnrollmentRecord struct {
	Enrollment domain.ClientEnrollment `json:"enrollment"`
	RecordedAt time.Time               `json:"recorded_at"`
}

func (r clientEnrollmentRecord) Validate() error {
	if err := r.Enrollment.Validate(); err != nil {
		return err
	}
	if r.RecordedAt.IsZero() {
		return fmt.Errorf("client enrollment %s recorded_at: %w", r.Enrollment.ID, domain.ErrMissingTimestamp)
	}
	if r.RecordedAt.Location() != time.UTC {
		return fmt.Errorf("client enrollment %s recorded_at: %w", r.Enrollment.ID, domain.ErrTimestampNotUTC)
	}
	return nil
}

const (
	insertClientEnrollmentSQL = `
INSERT INTO client_enrollments
    (id, auth_identity_id, harness_client, route, auth_method, credential_mode,
     refresh_strategy, supports_read_only_auth_snapshot, account_binding,
     recorded_at, body)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (id) DO NOTHING`
	selectClientEnrollmentBodySQL = `
SELECT body FROM client_enrollments WHERE id = ?`
	getClientEnrollmentSQL = `
SELECT auth_identity_id, harness_client, route, auth_method, credential_mode,
       refresh_strategy, supports_read_only_auth_snapshot, account_binding,
       recorded_at, body
FROM client_enrollments WHERE id = ?`

	maxEnrollmentGenerationOrdinalSQL = `
SELECT COALESCE(MAX(ordinal), 0) FROM client_enrollment_generations WHERE enrollment_id = ?`
	insertEnrollmentGenerationSQL = `
INSERT INTO client_enrollment_generations
    (enrollment_id, ordinal, auth_store_volume, store_manifest_digest,
     lease_fence, account_binding, token_expiry, recorded_at, body)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`
	getEnrollmentGenerationSQL = `
SELECT ordinal, auth_store_volume, store_manifest_digest, lease_fence,
       account_binding, token_expiry, recorded_at, body
FROM client_enrollment_generations
WHERE enrollment_id = ? AND ordinal = ?`
	latestEnrollmentGenerationSQL = `
SELECT ordinal, auth_store_volume, store_manifest_digest, lease_fence,
       account_binding, token_expiry, recorded_at, body
FROM client_enrollment_generations
WHERE enrollment_id = ? ORDER BY ordinal DESC LIMIT 1`
)

// RecordClientEnrollment persists one enrollment as a write-once record: a
// byte-identical replay converges, a same-id rewrite fails. The identity
// binding is gated on the write — the identity must exist, carry an account
// binding, and the enrollment must carry exactly that one — so an enrollment
// can never create the second-identity-per-account state §5.4 forbids.
func (tx *InternalTx) RecordClientEnrollment(
	ctx context.Context, enrollment domain.ClientEnrollment, recordedAt time.Time,
) error {
	if recordedAt.IsZero() {
		return fmt.Errorf("record client enrollment %q: zero recorded_at", enrollment.ID)
	}
	identity, err := tx.GetAuthIdentity(ctx, enrollment.AuthIdentityID)
	if err != nil {
		return fmt.Errorf("record client enrollment %q: %w", enrollment.ID, err)
	}
	if err := domain.ValidateEnrollmentIdentityBinding(identity, enrollment); err != nil {
		return fmt.Errorf("record client enrollment %q: %w", enrollment.ID, err)
	}
	body, err := encode(clientEnrollmentRecord{Enrollment: enrollment, RecordedAt: recordedAt.UTC()})
	if err != nil {
		return fmt.Errorf("record client enrollment %q: %w", enrollment.ID, err)
	}
	if err := tx.putImmutable(ctx, insertClientEnrollmentSQL,
		[]any{
			enrollment.ID, enrollment.AuthIdentityID, enrollment.HarnessClient,
			enrollment.Route, enrollment.AuthMethod, enrollment.CredentialMode,
			enrollment.RefreshStrategy, enrollment.SupportsReadOnlyAuthSnapshot,
			enrollment.AccountBinding, formatTime(recordedAt), body,
		},
		selectClientEnrollmentBodySQL, []any{enrollment.ID}, body); err != nil {
		return fmt.Errorf("record client enrollment %q: %w", enrollment.ID, err)
	}
	return nil
}

// GetClientEnrollment reconstructs one enrollment, cross-checking every
// extracted column against the decoded body and re-running the identity
// binding against the identity's current declaration. A row whose identity no
// longer carries its account binding fails closed rather than reconstructing
// a credential the account rule would refuse to create.
func (tx *ReadTx) GetClientEnrollment(
	ctx context.Context, id domain.ClientEnrollmentID,
) (domain.ClientEnrollment, error) {
	var (
		identityID     string
		harnessClient  string
		route          string
		authMethod     string
		credentialMode string
		refresh        string
		snapshots      bool
		accountBinding string
		recordedAt     string
		body           []byte
	)
	err := tx.tx.QueryRowContext(ctx, getClientEnrollmentSQL, id).
		Scan(&identityID, &harnessClient, &route, &authMethod, &credentialMode,
			&refresh, &snapshots, &accountBinding, &recordedAt, &body)
	if err != nil {
		return domain.ClientEnrollment{}, fmt.Errorf("get client enrollment %q: %w", id, notFoundOr(err))
	}
	record, err := decode[clientEnrollmentRecord](body)
	if err != nil {
		return domain.ClientEnrollment{}, fmt.Errorf("get client enrollment %q: %w", id, err)
	}
	enrollment := record.Enrollment
	if enrollment.ID != id ||
		string(enrollment.AuthIdentityID) != identityID ||
		string(enrollment.HarnessClient) != harnessClient ||
		enrollment.Route != route ||
		string(enrollment.AuthMethod) != authMethod ||
		string(enrollment.CredentialMode) != credentialMode ||
		string(enrollment.RefreshStrategy) != refresh ||
		enrollment.SupportsReadOnlyAuthSnapshot != snapshots ||
		enrollment.AccountBinding != accountBinding ||
		!timeColumnEqual(recordedAt, record.RecordedAt) {
		return domain.ClientEnrollment{}, fmt.Errorf("get client enrollment %q: %w", id, errRowInconsistent)
	}
	identity, err := tx.GetAuthIdentity(ctx, enrollment.AuthIdentityID)
	if err != nil {
		return domain.ClientEnrollment{}, fmt.Errorf("get client enrollment %q: %w", id, err)
	}
	if err := domain.ValidateEnrollmentIdentityBinding(identity, enrollment); err != nil {
		return domain.ClientEnrollment{}, fmt.Errorf("get client enrollment %q: %w", id, err)
	}
	return enrollment, nil
}

// AppendEnrollmentGeneration appends one immutable store-history entry and
// returns it with the store-assigned ordinal. The append is fenced by the
// identity's live mutation lease (§5.4): the lease must be held at the
// caller's instant, the entry must name the live fence, and the fence must
// carry a generation binding naming exactly this enrollment and locator — an
// unbound fence (the pre-enrollment interim shape) guards no enrollment
// store and refuses, so a store mutation can never ride a fence that does
// not name the store it mutated. The binding must also name the store's
// current state: its generation is the newest ordinal (zero for the
// bootstrap append that creates generation one) and, past bootstrap, its
// manifest digest is the newest generation's — a binding taken against
// superseded bytes, or reused after its own append, refuses instead of
// installing a rollback as the current generation. The entry must arrive
// unpersisted (ordinal zero); the store stamps the next contiguous ordinal,
// so the history is gap-free by construction.
func (tx *InternalTx) AppendEnrollmentGeneration(
	ctx context.Context, entry domain.EnrollmentGeneration, now time.Time,
) (domain.EnrollmentGeneration, error) {
	fail := func(err error) (domain.EnrollmentGeneration, error) {
		return domain.EnrollmentGeneration{}, fmt.Errorf(
			"append enrollment generation %q: %w", entry.EnrollmentID, err)
	}
	if entry.Ordinal != 0 {
		return fail(fmt.Errorf("ordinal %d: %w", entry.Ordinal, ErrEnrollmentOrdinalSupplied))
	}
	enrollment, err := tx.GetClientEnrollment(ctx, entry.EnrollmentID)
	if err != nil {
		return fail(err)
	}
	lease, err := tx.GetAuthStoreMutationLease(ctx, enrollment.AuthIdentityID)
	if err != nil {
		return fail(err)
	}
	if !lease.HeldAt(now) || lease.Fence != entry.LeaseFence {
		return fail(fmt.Errorf("lease fence %d, entry fence %d: %w",
			lease.Fence, entry.LeaseFence, ErrEnrollmentLeaseNotHeld))
	}
	binding := lease.GenerationBinding
	if binding == nil ||
		binding.EnrollmentID != entry.EnrollmentID ||
		binding.AuthStoreVolume != entry.AuthStoreVolume {
		return fail(ErrEnrollmentLeaseBindingMismatch)
	}
	var current int
	if err := tx.tx.QueryRowContext(ctx, maxEnrollmentGenerationOrdinalSQL, entry.EnrollmentID).
		Scan(&current); err != nil {
		return fail(err)
	}
	if binding.Generation != current {
		return fail(fmt.Errorf("binding generation %d, store at %d: %w",
			binding.Generation, current, ErrEnrollmentLeaseBindingStale))
	}
	if current > 0 {
		row := tx.tx.QueryRowContext(ctx, getEnrollmentGenerationSQL, entry.EnrollmentID, current)
		base, err := tx.scanEnrollmentGeneration(row, enrollment, current)
		if err != nil {
			return fail(err)
		}
		if binding.StoreManifestDigest != base.StoreManifestDigest {
			return fail(fmt.Errorf("binding manifest %s, current generation manifest %s: %w",
				binding.StoreManifestDigest, base.StoreManifestDigest, ErrEnrollmentLeaseBindingStale))
		}
	}
	stamped := entry
	stamped.Ordinal = current + 1
	if err := domain.ValidateEnrollmentGenerationBinding(enrollment, stamped); err != nil {
		return fail(err)
	}
	body, err := encode(stamped)
	if err != nil {
		return fail(err)
	}
	if _, err := tx.tx.ExecContext(ctx, insertEnrollmentGenerationSQL,
		stamped.EnrollmentID, stamped.Ordinal, stamped.AuthStoreVolume,
		stamped.StoreManifestDigest, stamped.LeaseFence, stamped.AccountBinding,
		formatTimePtr(stamped.TokenExpiry), formatTime(stamped.RecordedAt), body); err != nil {
		// A primary-key collision means a concurrent append won the ordinal;
		// single-winner, fail closed like the lease paths.
		return fail(err)
	}
	return stamped, nil
}

// CurrentEnrollmentGeneration reconstructs the enrollment's newest generation
// entry: the store §5.4 admission step 4 reads. Presence is reported through
// ErrNotFound — an enrollment with no generation yet has no mountable store.
func (tx *ReadTx) CurrentEnrollmentGeneration(
	ctx context.Context, id domain.ClientEnrollmentID,
) (domain.EnrollmentGeneration, error) {
	enrollment, err := tx.GetClientEnrollment(ctx, id)
	if err != nil {
		return domain.EnrollmentGeneration{}, fmt.Errorf("current enrollment generation %q: %w", id, err)
	}
	row := tx.tx.QueryRowContext(ctx, latestEnrollmentGenerationSQL, id)
	entry, err := tx.scanEnrollmentGeneration(row, enrollment, 0)
	if err != nil {
		return domain.EnrollmentGeneration{}, fmt.Errorf("current enrollment generation %q: %w", id, err)
	}
	return entry, nil
}

// GetEnrollmentGeneration reconstructs one exact generation entry: the read
// an admission's recorded generation resolves through.
func (tx *ReadTx) GetEnrollmentGeneration(
	ctx context.Context, id domain.ClientEnrollmentID, ordinal int,
) (domain.EnrollmentGeneration, error) {
	enrollment, err := tx.GetClientEnrollment(ctx, id)
	if err != nil {
		return domain.EnrollmentGeneration{}, fmt.Errorf("get enrollment generation %q/%d: %w", id, ordinal, err)
	}
	row := tx.tx.QueryRowContext(ctx, getEnrollmentGenerationSQL, id, ordinal)
	entry, err := tx.scanEnrollmentGeneration(row, enrollment, ordinal)
	if err != nil {
		return domain.EnrollmentGeneration{}, fmt.Errorf("get enrollment generation %q/%d: %w", id, ordinal, err)
	}
	return entry, nil
}

// scanEnrollmentGeneration is the single reconstruction path for generation
// entries: scan, decode, cross-check every extracted column, and re-run the
// enrollment binding (parent key, account, expiry-per-method). wantOrdinal
// zero accepts whatever ordinal the row carries (the latest-row read).
func (tx *ReadTx) scanEnrollmentGeneration(
	row scanner, enrollment domain.ClientEnrollment, wantOrdinal int,
) (domain.EnrollmentGeneration, error) {
	var (
		ordinal        int
		volume         string
		manifestDigest string
		fence          int64
		accountBinding string
		expiry         sql.NullString
		recordedAt     string
		body           []byte
	)
	err := row.Scan(&ordinal, &volume, &manifestDigest, &fence, &accountBinding,
		&expiry, &recordedAt, &body)
	if err != nil {
		return domain.EnrollmentGeneration{}, notFoundOr(err)
	}
	entry, err := decode[domain.EnrollmentGeneration](body)
	if err != nil {
		return domain.EnrollmentGeneration{}, err
	}
	if entry.EnrollmentID != enrollment.ID ||
		entry.Ordinal != ordinal ||
		(wantOrdinal != 0 && entry.Ordinal != wantOrdinal) ||
		entry.Ordinal < 1 ||
		entry.AuthStoreVolume != volume ||
		string(entry.StoreManifestDigest) != manifestDigest ||
		entry.LeaseFence != fence ||
		entry.AccountBinding != accountBinding ||
		!optionalTimeColumnEqual(expiry, entry.TokenExpiry) ||
		!timeColumnEqual(recordedAt, entry.RecordedAt) {
		return domain.EnrollmentGeneration{}, errRowInconsistent
	}
	if err := domain.ValidateEnrollmentGenerationBinding(enrollment, entry); err != nil {
		return domain.EnrollmentGeneration{}, err
	}
	return entry, nil
}
