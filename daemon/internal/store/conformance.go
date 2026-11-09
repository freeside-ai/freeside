package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// ErrBackendNotConformant is returned when an unattended admission names a
// backend with no current passed conformance record: none was ever recorded,
// the newest one records a failure, or it predates configuration binding. It
// is deliberately distinct from ErrNotFound: the admission was refused,
// which is not the same as having no record to return.
var ErrBackendNotConformant = errors.New(
	"backend has no current passed conformance record")

// ErrConformanceGenerationSupplied is returned when a conformance record
// arrives carrying a nonzero generation: the generation is this store's
// append identity, and accepting a caller-supplied one would let a writer
// forge where its proof sits in the supersession order.
var ErrConformanceGenerationSupplied = errors.New(
	"conformance proof generation is store-assigned; a record must arrive with generation zero")

// The durable backend-conformance record (plan §5.7; issues #327, #320): the
// append-only log of what each backend's completed full conformance passes
// proved, bound to the normalized configuration each pass exercised. The
// newest row per backend is its current declaration and the row id is the
// proof generation, mirroring ward's in-memory generation guard: a newer
// append (any outcome) supersedes the older record, and a failed append
// invalidates the declaration. Rows are daemon-internal and written by the
// suite runner through InternalTx, so they carry no
// entity_version/as_of_revision and never bump the server revision.
const (
	recordBackendConformanceSQL = `
INSERT INTO backend_conformance_records
    (backend, outcome, configuration_digest, capabilities, proved_at)
VALUES (?, ?, ?, ?, ?)`
	// Newest row per backend by id (insertion order), never by the
	// RFC3339Nano proved_at column: trailing zeros are trimmed, so
	// sub-second instants misorder lexicographically.
	latestBackendConformanceSQL = `
SELECT id, outcome, configuration_digest, capabilities, proved_at
FROM backend_conformance_records WHERE backend = ? ORDER BY id DESC LIMIT 1`
	// The row immediately preceding a given generation for a backend, across
	// every outcome and configuration digest, ordered by id for the same reason
	// as latestBackendConformanceSQL. In the append-only log this row is, by
	// construction, exactly the declaration a supersession marker at that
	// generation superseded: whatever was latest the instant the marker was
	// written. The authenticate re-gate tolerates only when that declaration
	// both passed and matches the admission's digest, so this must not filter
	// by outcome or digest. Skipping any intervening marker or other-digest row
	// to reach an older same-digest pass would resurrect a declaration the
	// backend already cleared: a proof for another configuration in between (a
	// roll forward to B then back to A), or a stacked marker from a mid-recheck
	// restart after a reconfiguration, means no completed pass currently
	// authorizes the admission's configuration, and the immediately-preceding
	// row (a marker, a failure, or a different digest) makes that refuse.
	latestBackendConformanceBeforeSQL = `
SELECT id, outcome, configuration_digest, capabilities, proved_at
FROM backend_conformance_records
WHERE backend = ? AND id < ?
ORDER BY id DESC LIMIT 1`
)

// RecordBackendConformance appends one completed pass's record and returns
// the store-assigned proof generation. Append-only by design: the log is the
// audit trail of every completed pass, and a repeated outcome (a second
// failure after a failure) is a real recorded pass, not a conflict. The
// record must arrive unpersisted (Generation zero) and bound to a concrete
// configuration: the generation is this store's append identity, and
// accepting a caller-supplied one would let a writer forge where its proof
// sits in the supersession order. Migrated unbound rows are audit history,
// never valid new appends.
func (tx *InternalTx) RecordBackendConformance(
	ctx context.Context, record domain.BackendConformance,
) (uint64, error) {
	if err := record.Validate(); err != nil {
		return 0, fmt.Errorf("record backend conformance: %w", err)
	}
	if record.Generation != 0 {
		return 0, fmt.Errorf("record backend conformance for %q: generation %d: %w",
			record.Backend, record.Generation, ErrConformanceGenerationSupplied)
	}
	if !record.ConfigurationBound() {
		return 0, fmt.Errorf("record backend conformance for %q: %w",
			record.Backend, domain.ErrConformanceConfigurationUnbound)
	}
	capabilities, err := json.Marshal(record.Capabilities)
	if err != nil {
		return 0, fmt.Errorf("record backend conformance for %q: %w", record.Backend, err)
	}
	result, err := tx.tx.ExecContext(ctx, recordBackendConformanceSQL,
		string(record.Backend), string(record.Outcome), string(record.ConfigurationDigest),
		string(capabilities),
		formatTime(record.ProvedAt))
	if err != nil {
		return 0, fmt.Errorf("record backend conformance for %q: %w", record.Backend, err)
	}
	generation, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("record backend conformance for %q: %w", record.Backend, err)
	}
	if generation <= 0 {
		return 0, fmt.Errorf("record backend conformance for %q: assigned generation %d: %w",
			record.Backend, generation, domain.ErrNonPositive)
	}
	return uint64(generation), nil
}

// LatestBackendConformance reconstructs a backend's current declaration: the
// newest appended record, with presence reported separately because an empty
// log is the legitimate "never proved anything" state, not an error (the
// LookupExecutionAdmission shape). Gets validate after reading: the decoded
// record re-runs domain validation, including the class capability ceiling,
// so a tampered row claiming beyond what the class's suite could ever prove
// fails closed instead of reconstructing as a wider declaration.
func (tx *ReadTx) LatestBackendConformance(
	ctx context.Context, backend domain.RunnerBackendClass,
) (domain.BackendConformance, bool, error) {
	row := tx.tx.QueryRowContext(ctx, latestBackendConformanceSQL, string(backend))
	return scanBackendConformance(backend, row)
}

// latestBackendConformanceBefore recovers the row immediately preceding
// generation for a backend: in the append-only log, exactly the declaration a
// supersession marker at that generation superseded. It deliberately returns a
// marker, a failure, or an other-digest row too, since the authenticate re-gate
// re-binds only when that declaration both passed and matches the admission's
// digest and must refuse otherwise. It shares the reconstruction-time
// validation of LatestBackendConformance: a row that no longer validates
// against the class ceiling fails closed rather than restoring a wider
// declaration.
func (tx *ReadTx) latestBackendConformanceBefore(
	ctx context.Context, backend domain.RunnerBackendClass, generation uint64,
) (domain.BackendConformance, bool, error) {
	row := tx.tx.QueryRowContext(ctx, latestBackendConformanceBeforeSQL,
		string(backend), generation)
	return scanBackendConformance(backend, row)
}

// scanBackendConformance reconstructs one conformance record from a scanned
// row and re-runs domain validation, so a tampered row claiming beyond what
// the class's suite could ever prove fails closed instead of reconstructing as
// a wider declaration. Presence is reported separately because an empty result
// is a legitimate "no such record" state, not an error. It is the single
// reconstruction point shared by every conformance read.
func scanBackendConformance(
	backend domain.RunnerBackendClass, row *sql.Row,
) (domain.BackendConformance, bool, error) {
	var (
		id                  int64
		outcome             string
		configurationDigest string
		capabilities        string
		provedAt            string
	)
	err := row.Scan(&id, &outcome, &configurationDigest, &capabilities, &provedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return domain.BackendConformance{}, false, nil
	case err != nil:
		return domain.BackendConformance{}, false,
			fmt.Errorf("backend conformance for %q: %w", backend, err)
	}
	if id <= 0 {
		return domain.BackendConformance{}, false,
			fmt.Errorf("backend conformance for %q: row id %d: %w",
				backend, id, domain.ErrNonPositive)
	}
	var caps domain.CapabilitySnapshot
	if err := json.Unmarshal([]byte(capabilities), &caps); err != nil {
		return domain.BackendConformance{}, false,
			fmt.Errorf("backend conformance for %q capabilities: %w", backend, err)
	}
	at, err := parseTime(provedAt)
	if err != nil {
		return domain.BackendConformance{}, false,
			fmt.Errorf("backend conformance for %q proved_at %q: %w", backend, provedAt, err)
	}
	record := domain.BackendConformance{
		Backend:             backend,
		Outcome:             domain.ConformanceOutcome(outcome),
		ConfigurationDigest: domain.Digest(configurationDigest),
		Capabilities:        caps,
		Generation:          uint64(id),
		ProvedAt:            at,
	}
	if err := record.Validate(); err != nil {
		return domain.BackendConformance{}, false,
			fmt.Errorf("backend conformance for %q: %w", backend, err)
	}
	return record, true, nil
}

// RequireBackendConformant is the backend-conformance half of §5.7's
// unattended conditions, checked in the admitting transaction (issue #320):
// the named backend's current durable declaration must exist, record a pass,
// and cover the admission's capability snapshot. Like
// RequireUnattendedAdmissible it is a recording-time precondition on new
// unattended operation, not part of a record's meaning, so reconstruction
// (scanExecutionAdmission) deliberately does not re-run it: a lapsed or
// superseded conformance must stop what happens next, not make recorded
// history unreadable. attended_dev is exempt by the owner-ratified reading
// of #320: §5.7 admits a weaker, unproven runner class there on purpose.
func (tx *ReadTx) RequireBackendConformant(
	ctx context.Context, admission domain.ExecutionAdmission,
) error {
	return tx.requireBackendConformant(ctx, admission, backendConformanceMint)
}

// AuthenticateBackendConformant re-gates an already-recorded admission against
// current conformance state, tolerating one thing the mint gate does not: a
// same-configuration supersession marker. The startup suite (and the scheduled
// doctor re-proof) writes that marker the instant a recheck of the current
// configuration begins, before the marker's own passed outcome lands. A
// same-configuration re-proof is a proof refresh, not a policy change, so an
// invocation already admitted against the proof the marker superseded stays
// authenticated across the window; without this, a re-proof racing an in-flight
// invocation's re-gate makes the invocation permanently unauthenticatable and
// durable-stops the daemon (issue #761). A failed proof, or a marker for a
// different configuration, is a real policy change and stays a refusal.
func (tx *ReadTx) AuthenticateBackendConformant(
	ctx context.Context, admission domain.ExecutionAdmission,
) error {
	return tx.requireBackendConformant(ctx, admission, backendConformanceAuthenticate)
}

// backendConformanceGate selects how requireBackendConformant treats a latest
// row that is not itself a current passed proof. The mint gate refuses
// anything but a current passed row (work not yet admitted loses nothing by
// holding out a recheck window); the authenticate gate additionally recovers
// the passed proof a same-configuration supersession marker superseded, so a
// persisted admission survives the window.
type backendConformanceGate int

const (
	backendConformanceMint backendConformanceGate = iota
	backendConformanceAuthenticate
)

func (tx *ReadTx) requireBackendConformant(
	ctx context.Context, admission domain.ExecutionAdmission, gate backendConformanceGate,
) error {
	if admission.OperatingMode != domain.ModeUnattended {
		return nil
	}
	return tx.requireConformantDeclaration(ctx, conformanceSubject{
		label:        fmt.Sprintf("admission %q", admission.InvocationID),
		class:        domain.RunnerBackendClass(admission.Backend),
		digest:       admission.BackendConfigurationDigest,
		capabilities: admission.Capabilities,
	}, gate)
}

// RequireHostVerificationConformant is the conformance half of §5.7's
// unattended condition for the credential-free macOS verification executor:
// the executor class's current durable declaration must exist, record a pass,
// be bound to the executor's configuration digest, and cover the capabilities
// the executor declares now. It applies the same checks and refusals as the
// mint gate for a ward admission, against the executor's own record. The class
// is fixed here rather than taken from the caller, so a host verification job
// can never be re-gated against the ward's record. There is no authenticate
// variant: a host verification admission is not yet persisted (#2882), so no
// recorded admission has to survive a recheck window.
func (tx *ReadTx) RequireHostVerificationConformant(
	ctx context.Context, digest domain.Digest, capabilities domain.CapabilitySnapshot,
) error {
	return tx.requireConformantDeclaration(ctx, conformanceSubject{
		label:        "host verification",
		class:        domain.BackendCredentialFreeHostVerification,
		digest:       digest,
		capabilities: capabilities,
	}, backendConformanceMint)
}

// conformanceSubject is what requireConformantDeclaration gates: a declaration
// a backend of class made under configuration digest. label prefixes every
// refusal so the caller's record is named.
type conformanceSubject struct {
	label        string
	class        domain.RunnerBackendClass
	digest       domain.Digest
	capabilities domain.CapabilitySnapshot
}

func (tx *ReadTx) requireConformantDeclaration(
	ctx context.Context, subject conformanceSubject, gate backendConformanceGate,
) error {
	record, found, err := tx.LatestBackendConformance(ctx, subject.class)
	if err != nil {
		return fmt.Errorf("%s: %w", subject.label, err)
	}
	if !found {
		return fmt.Errorf("%s backend %q: %w",
			subject.label, subject.class, ErrBackendNotConformant)
	}
	if record.Outcome != domain.ConformancePassed {
		// A same-configuration supersession marker, in the authenticate gate
		// only, re-binds to the passed proof it superseded. Every other
		// non-passed latest row (a failure, or a marker for a different
		// configuration) refuses exactly as the mint gate does. The recovered
		// proof carries the same-digest capability ceiling; the shared checks
		// below then run against it unchanged.
		passed, recovered, err := tx.supersededSameConfigurationProof(
			ctx, subject.class, subject.digest, record, gate)
		if err != nil {
			return fmt.Errorf("%s: %w", subject.label, err)
		}
		if !recovered {
			return fmt.Errorf("%s backend %q conformance generation %d records %q: %w",
				subject.label, subject.class, record.Generation,
				record.Outcome, ErrBackendNotConformant)
		}
		record = passed
	}
	if !record.ConfigurationBound() {
		return fmt.Errorf("%s backend %q conformance generation %d: %w",
			subject.label, subject.class, record.Generation,
			domain.ErrConformanceConfigurationUnbound)
	}
	if subject.digest != record.ConfigurationDigest {
		return fmt.Errorf(
			"%s backend %q configuration %s differs from conformance generation %d configuration %s: %w",
			subject.label, subject.class, subject.digest,
			record.Generation, record.ConfigurationDigest,
			domain.ErrAdmissionConfigurationMismatch)
	}
	if excess := domain.ExcessCapabilities(subject.capabilities, record.Capabilities); len(excess) > 0 {
		return fmt.Errorf("%s backend %q claims %v beyond conformance generation %d: %w",
			subject.label, subject.class, excess, record.Generation,
			domain.ErrAdmissionExceedsConformance)
	}
	return nil
}

// supersededSameConfigurationProof reports the passed proof to re-bind an
// admission to when the backend's latest row is a supersession marker for the
// admission's own configuration. recovered is false (refuse) unless every
// condition holds: the gate authenticates, the latest row is a supersession
// marker (not a failure), the marker's configuration is bound and equals the
// admission's bound digest, and the exact declaration the marker superseded
// (the newest completed proof for that digest) passed. A marker for a
// different configuration means the operator reconfigured the backend, so the
// admission's configuration is no longer current and stays refused; the digest
// equality is checked here rather than left to the shared mismatch check so a
// reconfiguration refuses as "no current passed record" rather than silently
// recovering a stale proof. Re-binding to the *superseded* declaration, not
// the immediately-preceding row, is what keeps every non-refresh history
// fatal: that row is the exact declaration the marker superseded, so an
// already-admitted invocation is authenticated only against the state the
// backend actually held when this re-proof began, never an older pass the log
// moved past (a fail-open the mint gate refuses). A failure, an
// other-configuration proof, or another marker (a mid-recheck restart after a
// reconfiguration) in that position all mean no completed pass currently
// authorizes the admission's configuration, so tolerance closes. Two digest
// checks are required and distinct: the marker must be re-proving the
// admission's configuration (a refresh, not a reconfiguration), and the
// declaration it superseded must itself be a pass for that configuration.
func (tx *ReadTx) supersededSameConfigurationProof(
	ctx context.Context,
	class domain.RunnerBackendClass,
	digest domain.Digest,
	latest domain.BackendConformance,
	gate backendConformanceGate,
) (domain.BackendConformance, bool, error) {
	if gate != backendConformanceAuthenticate {
		return domain.BackendConformance{}, false, nil
	}
	if latest.Outcome != domain.ConformanceSuperseded {
		return domain.BackendConformance{}, false, nil
	}
	if !latest.ConfigurationBound() ||
		latest.ConfigurationDigest != digest {
		return domain.BackendConformance{}, false, nil
	}
	superseded, found, err := tx.latestBackendConformanceBefore(ctx, class, latest.Generation)
	if err != nil {
		return domain.BackendConformance{}, false, err
	}
	if !found || superseded.Outcome != domain.ConformancePassed ||
		superseded.ConfigurationDigest != digest {
		return domain.BackendConformance{}, false, nil
	}
	return superseded, true, nil
}
