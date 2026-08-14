package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

// CodexReenrollmentOutcome is the credential-free journal state.
type CodexReenrollmentOutcome string

const (
	CodexReenrollmentFailed   CodexReenrollmentOutcome = "failed"
	CodexReenrollmentVerified CodexReenrollmentOutcome = "verified"
)

// AllCodexReenrollmentOutcomes lists every valid CodexReenrollmentOutcome.
var AllCodexReenrollmentOutcomes = []CodexReenrollmentOutcome{
	CodexReenrollmentFailed,
	CodexReenrollmentVerified,
}

func (o CodexReenrollmentOutcome) valid() bool {
	switch o {
	case CodexReenrollmentFailed, CodexReenrollmentVerified:
		return true
	default:
		return false
	}
}

// CodexReenrollmentFailureClass is deliberately closed and credential-free.
type CodexReenrollmentFailureClass string

const (
	CodexReenrollmentAuthStoreReplacementFailed CodexReenrollmentFailureClass = "auth_store_replacement_failed"
	CodexReenrollmentVerificationFailed         CodexReenrollmentFailureClass = "verification_failed"
	CodexReenrollmentLeaseLost                  CodexReenrollmentFailureClass = "lease_lost"
)

// AllCodexReenrollmentFailureClasses lists every valid failure class.
var AllCodexReenrollmentFailureClasses = []CodexReenrollmentFailureClass{
	CodexReenrollmentAuthStoreReplacementFailed,
	CodexReenrollmentVerificationFailed,
	CodexReenrollmentLeaseLost,
}

func (c CodexReenrollmentFailureClass) valid() bool {
	switch c {
	case CodexReenrollmentAuthStoreReplacementFailed,
		CodexReenrollmentVerificationFailed,
		CodexReenrollmentLeaseLost:
		return true
	default:
		return false
	}
}

// CodexReenrollmentTerminal is the operation's single terminal outcome.
type CodexReenrollmentTerminal struct {
	Outcome              CodexReenrollmentOutcome       `json:"outcome"`
	FailureClass         *CodexReenrollmentFailureClass `json:"failure_class"`
	AuthStoreDigest      *domain.Digest                 `json:"auth_store_digest"`
	AccessTokenExpiresAt *time.Time                     `json:"access_token_expires_at"`
	CompletedAt          time.Time                      `json:"completed_at"`
}

func (o CodexReenrollmentTerminal) Validate() error {
	if !o.Outcome.valid() {
		return fmt.Errorf("codex re-enrollment outcome %q is invalid", o.Outcome)
	}
	if o.CompletedAt.IsZero() || o.CompletedAt.Location() != time.UTC {
		return errors.New("codex re-enrollment completion time must be nonzero UTC")
	}
	switch o.Outcome {
	case CodexReenrollmentFailed:
		if o.FailureClass == nil || !o.FailureClass.valid() ||
			o.AuthStoreDigest != nil || o.AccessTokenExpiresAt != nil {
			return errors.New("failed codex re-enrollment outcome has inconsistent fields")
		}
	case CodexReenrollmentVerified:
		if o.FailureClass != nil || o.AuthStoreDigest == nil || *o.AuthStoreDigest == "" ||
			o.AccessTokenExpiresAt == nil || o.AccessTokenExpiresAt.IsZero() ||
			o.AccessTokenExpiresAt.Location() != time.UTC ||
			!o.AccessTokenExpiresAt.After(o.CompletedAt) {
			return errors.New("verified codex re-enrollment outcome has inconsistent fields")
		}
	}
	return nil
}

// CodexReenrollmentJournal is one immutable operation header plus at most one
// terminal outcome. Absence of Terminal is the durable pending state.
type CodexReenrollmentJournal struct {
	AuthIdentityID domain.AuthIdentityID      `json:"auth_identity_id"`
	LeaseFence     int64                      `json:"lease_fence"`
	MarkerItemID   domain.ItemID              `json:"marker_item_id"`
	Holder         domain.InvocationID        `json:"holder"`
	OpenedAt       time.Time                  `json:"opened_at"`
	Terminal       *CodexReenrollmentTerminal `json:"terminal"`
}

func (j CodexReenrollmentJournal) Validate() error {
	if j.AuthIdentityID == "" || j.MarkerItemID == "" || j.Holder == "" {
		return errors.New("codex re-enrollment identity, marker, and holder are required")
	}
	if j.LeaseFence < 1 {
		return fmt.Errorf("codex re-enrollment lease fence %d is not positive", j.LeaseFence)
	}
	if j.OpenedAt.IsZero() || j.OpenedAt.Location() != time.UTC {
		return errors.New("codex re-enrollment opened_at must be nonzero UTC")
	}
	if j.Terminal != nil {
		if err := j.Terminal.Validate(); err != nil {
			return err
		}
		if j.Terminal.CompletedAt.Before(j.OpenedAt) {
			return errors.New("codex re-enrollment completion precedes opening")
		}
	}
	return nil
}

// RecoveryBinding returns verified coordinates, refusing pending and failed
// operations so callers cannot project an unverified resolution action.
func (j CodexReenrollmentJournal) RecoveryBinding() (domain.CodexReenrollmentRecoveryBinding, error) {
	if err := j.Validate(); err != nil {
		return domain.CodexReenrollmentRecoveryBinding{}, err
	}
	if j.Terminal == nil || j.Terminal.Outcome != CodexReenrollmentVerified {
		return domain.CodexReenrollmentRecoveryBinding{}, ErrCodexReenrollmentNotVerified
	}
	return domain.CodexReenrollmentRecoveryBinding{
		AuthIdentityID:       j.AuthIdentityID,
		LeaseFence:           j.LeaseFence,
		AuthStoreDigest:      *j.Terminal.AuthStoreDigest,
		AccessTokenExpiresAt: *j.Terminal.AccessTokenExpiresAt,
	}, nil
}

var (
	ErrCodexReenrollmentOutcomeConflict = errors.New("codex re-enrollment already has a different terminal outcome")
	ErrCodexReenrollmentLeaseMismatch   = errors.New("codex re-enrollment lease state does not authorize terminal outcome")
	ErrCodexReenrollmentNotVerified     = errors.New("codex re-enrollment is not verified")
)

// CodexReenrollmentMarkerPrefix returns the identity-bound namespace for one
// revoked-identity marker history.
func CodexReenrollmentMarkerPrefix(id domain.AuthIdentityID) string {
	digest := contentaddr.Hex(contentaddr.Sum([]byte(id)))
	return "system-health-codex-auth-" + digest + "-"
}

// CodexReenrollmentMarkerID returns the sole canonical marker identifier for
// one positive occurrence in an identity's history.
func CodexReenrollmentMarkerID(
	id domain.AuthIdentityID, occurrence int,
) (domain.ItemID, error) {
	if occurrence < 1 {
		return "", fmt.Errorf("codex auth re-enrollment occurrence must be positive: %w",
			domain.ErrCodexReenrollmentMarkerMismatch)
	}
	return domain.ItemID(CodexReenrollmentMarkerPrefix(id) + strconv.Itoa(occurrence)), nil
}

// NewCodexReenrollmentMarker constructs the exact persisted marker shape used
// by both ward admission and the re-enrollment journal trust boundary.
func NewCodexReenrollmentMarker(
	id domain.AuthIdentityID,
	occurrence int,
	projectID domain.ProjectID,
	version int,
	status domain.ItemStatus,
	binding *domain.CodexReenrollmentRecoveryBinding,
) (domain.AttentionItem, error) {
	itemID, err := CodexReenrollmentMarkerID(id, occurrence)
	if err != nil {
		return domain.AttentionItem{}, err
	}
	posture := domain.HealthPostureAdvisory
	actions := []domain.Action{domain.ActionAcknowledge}
	if binding != nil {
		if binding.AuthIdentityID != id {
			return domain.AttentionItem{}, domain.ErrCodexReenrollmentBindingMismatch
		}
		actions = append(actions, domain.ActionResolveReenrollment)
	}
	return domain.NewAttentionItem(domain.AttentionItemInput{
		ID: itemID, ProjectID: projectID,
		Subject: domain.Subject{Type: domain.SubjectSystem, ID: "daemon"},
		Type:    domain.AttentionSystemHealth, Priority: domain.PriorityHigh,
		Reason: fmt.Sprintf(
			"Codex auth identity %q can no longer refresh. Complete verified re-enrollment to make the recovery action available.",
			id,
		),
		RequestedDecision:                actions,
		CodexReenrollmentRecoveryBinding: binding,
		ItemVersion:                      version,
		InterruptionClass:                domain.InterruptionExceptional,
		CreatedAt:                        nil,
		Posture:                          &posture,
		Status:                           status,
	}, nil)
}

// CodexReenrollmentMarkerOccurrence authenticates an identity-bound marker and
// returns its canonical positive occurrence, or zero for an unrelated item.
func CodexReenrollmentMarkerOccurrence(
	item domain.AttentionItem, id domain.AuthIdentityID,
) (int, error) {
	prefix := CodexReenrollmentMarkerPrefix(id)
	suffix := strings.TrimPrefix(string(item.ID), prefix)
	if suffix == string(item.ID) {
		return 0, nil
	}
	occurrence, err := strconv.Atoi(suffix)
	if err != nil || occurrence < 1 || suffix != strconv.Itoa(occurrence) {
		return 0, fmt.Errorf("codex auth re-enrollment item has an invalid occurrence: %w",
			domain.ErrCodexReenrollmentMarkerMismatch)
	}
	expected, err := NewCodexReenrollmentMarker(
		id, occurrence, item.ProjectID, item.ItemVersion, item.Status,
		item.CodexReenrollmentRecoveryBinding,
	)
	if err != nil {
		return 0, err
	}
	expected.Timing = item.Timing
	expected.CreatedAt = item.CreatedAt
	expected.DecidedAt = item.DecidedAt
	if !reflect.DeepEqual(expected, item) {
		return 0, fmt.Errorf("codex auth re-enrollment item diverges from its identity binding: %w",
			domain.ErrCodexReenrollmentMarkerMismatch)
	}
	return occurrence, nil
}

// NextCodexReenrollmentMarkerOccurrence advances a non-negative occurrence
// without allowing integer wrap to reopen an existing namespace.
func NextCodexReenrollmentMarkerOccurrence(latest int) (int, error) {
	if latest < 0 || latest == math.MaxInt {
		return 0, fmt.Errorf("codex auth re-enrollment occurrence exhausted: %w",
			domain.ErrCodexReenrollmentMarkerMismatch)
	}
	return latest + 1, nil
}

const (
	insertCodexReenrollmentSQL = `
INSERT INTO codex_reenrollment_operations
    (auth_identity_id, lease_fence, marker_item_id, holder, opened_at, outcome, failure_class,
     auth_store_digest, access_token_expires_at, completed_at, body)
VALUES (?, ?, ?, ?, ?, NULL, NULL, NULL, NULL, NULL, ?)`
	getCodexReenrollmentSQL = `
SELECT marker_item_id, holder, opened_at, outcome, failure_class, auth_store_digest,
       access_token_expires_at, completed_at, body
FROM codex_reenrollment_operations
WHERE auth_identity_id = ? AND lease_fence = ?`
	latestCodexReenrollmentSQL = `
SELECT lease_fence, marker_item_id, holder, opened_at, outcome, failure_class, auth_store_digest,
       access_token_expires_at, completed_at, body
FROM codex_reenrollment_operations
WHERE auth_identity_id = ? ORDER BY lease_fence DESC LIMIT 1`
	completeCodexReenrollmentSQL = `
UPDATE codex_reenrollment_operations
SET outcome = ?, failure_class = ?, auth_store_digest = ?,
    access_token_expires_at = ?, completed_at = ?, body = ?
WHERE auth_identity_id = ? AND lease_fence = ? AND outcome IS NULL`
)

// BeginCodexReenrollmentJournal acquires the exact mutation lease and opens
// the durable pending operation in this same SQLite transaction.
func (tx *InternalTx) BeginCodexReenrollmentJournal(
	ctx context.Context, id domain.AuthIdentityID, markerItemID domain.ItemID, holder domain.InvocationID,
	now, expiresAt time.Time,
) (CodexReenrollmentJournal, domain.AuthStoreMutationLease, error) {
	items, err := tx.ListAttentionItems(ctx)
	if err != nil {
		return CodexReenrollmentJournal{}, domain.AuthStoreMutationLease{}, err
	}
	var marker domain.AttentionItem
	latestOccurrence := 0
	open := 0
	for _, snapshot := range items {
		item := snapshot.Value
		occurrence, err := CodexReenrollmentMarkerOccurrence(item, id)
		if err != nil {
			return CodexReenrollmentJournal{}, domain.AuthStoreMutationLease{}, err
		}
		if occurrence == 0 {
			continue
		}
		if item.Status == domain.StatusOpen {
			open++
		}
		if occurrence > latestOccurrence {
			marker = item
			latestOccurrence = occurrence
		}
	}
	if latestOccurrence == 0 || open != 1 || marker.ID != markerItemID ||
		marker.Status != domain.StatusOpen ||
		marker.CodexReenrollmentRecoveryBinding != nil || marker.Offers(domain.ActionResolveReenrollment) {
		return CodexReenrollmentJournal{}, domain.AuthStoreMutationLease{},
			domain.ErrCodexReenrollmentMarkerMismatch
	}
	lease, err := tx.AcquireAuthStoreMutationLease(ctx, id, holder, now, expiresAt)
	if err != nil {
		return CodexReenrollmentJournal{}, domain.AuthStoreMutationLease{}, err
	}
	if !lease.AcquiredAt.Equal(now) || !lease.ExpiresAt.Equal(expiresAt) {
		return CodexReenrollmentJournal{}, domain.AuthStoreMutationLease{}, errors.New(
			"begin codex re-enrollment: acquisition converged on an existing lease window")
	}
	rec := CodexReenrollmentJournal{
		AuthIdentityID: id, LeaseFence: lease.Fence, MarkerItemID: markerItemID,
		Holder: holder, OpenedAt: now.UTC(),
	}
	body, err := encode(rec)
	if err != nil {
		return CodexReenrollmentJournal{}, domain.AuthStoreMutationLease{}, err
	}
	if _, err := tx.tx.ExecContext(ctx, insertCodexReenrollmentSQL,
		rec.AuthIdentityID, rec.LeaseFence, rec.MarkerItemID, rec.Holder, formatTime(rec.OpenedAt), body); err != nil {
		return CodexReenrollmentJournal{}, domain.AuthStoreMutationLease{},
			fmt.Errorf("begin codex re-enrollment %q fence %d: %w", id, lease.Fence, err)
	}
	return rec, lease, nil
}

// FailCodexReenrollment records one safe terminal failure class.
func (tx *InternalTx) FailCodexReenrollment(
	ctx context.Context, id domain.AuthIdentityID, holder domain.InvocationID,
	fence int64, class CodexReenrollmentFailureClass, at time.Time,
) error {
	terminal := CodexReenrollmentTerminal{
		Outcome: CodexReenrollmentFailed, FailureClass: &class, CompletedAt: at.UTC(),
	}
	return tx.completeCodexReenrollment(ctx, id, holder, fence, terminal, at)
}

// VerifyCodexReenrollment records the verified replacement digest and expiry.
func (tx *InternalTx) VerifyCodexReenrollment(
	ctx context.Context, id domain.AuthIdentityID, holder domain.InvocationID,
	fence int64, digest domain.Digest, expiresAt, verifiedAt time.Time,
) error {
	expiry := expiresAt.UTC()
	terminal := CodexReenrollmentTerminal{
		Outcome: CodexReenrollmentVerified, AuthStoreDigest: &digest,
		AccessTokenExpiresAt: &expiry, CompletedAt: verifiedAt.UTC(),
	}
	return tx.completeCodexReenrollment(ctx, id, holder, fence, terminal, verifiedAt)
}

func (tx *InternalTx) completeCodexReenrollment(
	ctx context.Context, id domain.AuthIdentityID, holder domain.InvocationID,
	fence int64, terminal CodexReenrollmentTerminal, at time.Time,
) error {
	if err := terminal.Validate(); err != nil {
		return err
	}
	rec, err := tx.GetCodexReenrollmentJournal(ctx, id, fence)
	if err != nil {
		return err
	}
	if rec.Holder != holder {
		return ErrCodexReenrollmentLeaseMismatch
	}
	if rec.Terminal != nil {
		if terminalsEqual(*rec.Terminal, terminal) {
			return nil
		}
		return ErrCodexReenrollmentOutcomeConflict
	}
	if at.Before(rec.OpenedAt) {
		return ErrCodexReenrollmentLeaseMismatch
	}
	lease, err := tx.GetAuthStoreMutationLease(ctx, id)
	if err != nil || !codexReenrollmentTerminalAuthorized(lease, holder, fence, terminal, at) {
		return ErrCodexReenrollmentLeaseMismatch
	}
	rec.Terminal = &terminal
	body, err := encode(rec)
	if err != nil {
		return err
	}
	var failure, digest, expiry any
	if terminal.FailureClass != nil {
		failure = *terminal.FailureClass
	}
	if terminal.AuthStoreDigest != nil {
		digest = *terminal.AuthStoreDigest
	}
	if terminal.AccessTokenExpiresAt != nil {
		expiry = formatTime(*terminal.AccessTokenExpiresAt)
	}
	res, err := tx.tx.ExecContext(ctx, completeCodexReenrollmentSQL,
		terminal.Outcome, failure, digest, expiry, formatTime(terminal.CompletedAt), body, id, fence)
	if err != nil {
		return fmt.Errorf("complete codex re-enrollment %q fence %d: %w", id, fence, err)
	}
	changed, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrCodexReenrollmentOutcomeConflict
	}
	return nil
}

func codexReenrollmentTerminalAuthorized(
	lease domain.AuthStoreMutationLease, holder domain.InvocationID, fence int64,
	terminal CodexReenrollmentTerminal, at time.Time,
) bool {
	if terminal.Outcome != CodexReenrollmentFailed || terminal.FailureClass == nil ||
		*terminal.FailureClass != CodexReenrollmentLeaseLost {
		return lease.Holder == holder && lease.Fence == fence && lease.HeldAt(at)
	}

	// A lease-lost terminal is safe because it grants no recovery authority,
	// but it must still be a fact about this operation's exact fence. The
	// current row proves loss only once that fence's release or expiry has
	// happened, or once a newer generation has actually been acquired.
	switch {
	case lease.Fence < fence:
		return false
	case lease.Fence > fence:
		return !at.Before(lease.AcquiredAt)
	case lease.Holder != holder:
		return false
	case lease.ReleasedAt != nil:
		return !at.Before(*lease.ReleasedAt)
	default:
		return !at.Before(lease.ExpiresAt)
	}
}

// GetCodexReenrollmentJournal reconstructs and cross-checks one operation.
func (tx *ReadTx) GetCodexReenrollmentJournal(
	ctx context.Context, id domain.AuthIdentityID, fence int64,
) (CodexReenrollmentJournal, error) {
	row := tx.tx.QueryRowContext(ctx, getCodexReenrollmentSQL, id, fence)
	return scanCodexReenrollment(row, id, fence)
}

// LatestCodexReenrollmentJournal returns the highest-fence operation.
func (tx *ReadTx) LatestCodexReenrollmentJournal(
	ctx context.Context, id domain.AuthIdentityID,
) (CodexReenrollmentJournal, bool, error) {
	var fence int64
	var markerItemID, holder, opened string
	var outcome, failure, digest, expiry, completed sql.NullString
	var body []byte
	err := tx.tx.QueryRowContext(ctx, latestCodexReenrollmentSQL, id).Scan(
		&fence, &markerItemID, &holder, &opened, &outcome, &failure, &digest, &expiry, &completed, &body)
	if errors.Is(err, sql.ErrNoRows) {
		return CodexReenrollmentJournal{}, false, nil
	}
	if err != nil {
		return CodexReenrollmentJournal{}, false, err
	}
	rec, err := reconstructCodexReenrollment(
		id, fence, markerItemID, holder, opened, outcome, failure, digest, expiry, completed, body)
	return rec, true, err
}

type codexReenrollmentScanner interface{ Scan(...any) error }

func scanCodexReenrollment(row codexReenrollmentScanner, id domain.AuthIdentityID, fence int64) (CodexReenrollmentJournal, error) {
	var markerItemID, holder, opened string
	var outcome, failure, digest, expiry, completed sql.NullString
	var body []byte
	if err := row.Scan(&markerItemID, &holder, &opened, &outcome, &failure, &digest, &expiry, &completed, &body); err != nil {
		return CodexReenrollmentJournal{}, fmt.Errorf("get codex re-enrollment %q fence %d: %w", id, fence, notFoundOr(err))
	}
	return reconstructCodexReenrollment(
		id, fence, markerItemID, holder, opened, outcome, failure, digest, expiry, completed, body)
}

func reconstructCodexReenrollment(
	id domain.AuthIdentityID, fence int64, markerItemID, holder, opened string,
	outcome, failure, digest, expiry, completed sql.NullString, body []byte,
) (CodexReenrollmentJournal, error) {
	rec, err := decodeCodexReenrollment(body)
	if err != nil {
		return CodexReenrollmentJournal{}, fmt.Errorf("get codex re-enrollment %q fence %d: %w", id, fence, err)
	}
	if rec.AuthIdentityID != id || rec.LeaseFence != fence || string(rec.MarkerItemID) != markerItemID ||
		string(rec.Holder) != holder ||
		!timeColumnEqual(opened, rec.OpenedAt) || !codexTerminalColumnsEqual(rec.Terminal, outcome, failure, digest, expiry, completed) {
		return CodexReenrollmentJournal{}, fmt.Errorf("get codex re-enrollment %q fence %d: %w", id, fence, errRowInconsistent)
	}
	return rec, nil
}

func decodeCodexReenrollment(body []byte) (CodexReenrollmentJournal, error) {
	var rec CodexReenrollmentJournal
	if err := strictjson.Decode(
		body, &rec, strictjson.RejectInvalidUTF8, strictjson.NoLimit,
	); err != nil {
		return rec, err
	}
	if err := rec.Validate(); err != nil {
		return rec, fmt.Errorf("stored row invalid: %w", err)
	}
	return rec, nil
}

func terminalsEqual(a, b CodexReenrollmentTerminal) bool {
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return bytes.Equal(ab, bb)
}

func codexTerminalColumnsEqual(
	terminal *CodexReenrollmentTerminal,
	outcome, failure, digest, expiry, completed sql.NullString,
) bool {
	if terminal == nil {
		return !outcome.Valid && !failure.Valid && !digest.Valid && !expiry.Valid && !completed.Valid
	}
	if !outcome.Valid || outcome.String != string(terminal.Outcome) ||
		!completed.Valid || !timeColumnEqual(completed.String, terminal.CompletedAt) {
		return false
	}
	switch terminal.Outcome {
	case CodexReenrollmentFailed:
		return failure.Valid && terminal.FailureClass != nil && failure.String == string(*terminal.FailureClass) &&
			!digest.Valid && !expiry.Valid
	case CodexReenrollmentVerified:
		return !failure.Valid && digest.Valid && terminal.AuthStoreDigest != nil && digest.String == string(*terminal.AuthStoreDigest) &&
			expiry.Valid && terminal.AccessTokenExpiresAt != nil && timeColumnEqual(expiry.String, *terminal.AccessTokenExpiresAt)
	}
	return false
}
