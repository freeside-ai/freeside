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

// The durable backend-conformance record (plan §5.7; issues #327, #320,
// #3211): the append-only log of what each backend's completed full
// conformance passes proved, bound to the normalized configuration each pass
// exercised. The row id is the proof generation, mirroring ward's in-memory
// generation guard: a newer append (any outcome) supersedes the older record
// for the same configuration, and a failed append invalidates that
// configuration's declaration. Ward holds one declaration per configuration
// it proves (one per writer image), so a ward admission is judged against the
// newest row for its own configuration digest, and a row for another
// configuration neither admits nor refuses it. The macOS verification
// executor keeps one declaration per backend: its newest row is its current
// declaration. Rows are daemon-internal and written by the suite runner
// through InternalTx, so they carry no entity_version/as_of_revision and never
// bump the server revision.
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
	// The newest row for one backend and configuration digest: a ward
	// configuration's current declaration. Ordered by id for the same reason
	// as latestBackendConformanceSQL.
	latestConfigurationConformanceSQL = `
SELECT id, outcome, configuration_digest, capabilities, proved_at
FROM backend_conformance_records
WHERE backend = ? AND configuration_digest = ?
ORDER BY id DESC LIMIT 1`
	// The row immediately preceding a given generation for one backend and
	// configuration digest, across every outcome, ordered by id for the same
	// reason as latestBackendConformanceSQL. In the append-only log this row
	// is, by construction, exactly the declaration of that configuration a
	// supersession marker at that generation superseded: whatever was that
	// configuration's newest row the instant the marker was written. The
	// authenticate re-gate tolerates only when that declaration passed, so
	// this must not filter by outcome. Skipping an intervening marker or
	// failure to reach an older pass would resurrect a declaration the
	// backend already cleared: a failed pass in between, or a stacked marker
	// from a recheck that never concluded (a restart mid-recheck, or a
	// retirement followed by a new proof), means no completed pass currently
	// authorizes the configuration, and the immediately-preceding row makes
	// that refuse. Rows for other configurations are not in this
	// configuration's history and do not matter.
	latestConfigurationConformanceBeforeSQL = `
SELECT id, outcome, configuration_digest, capabilities, proved_at
FROM backend_conformance_records
WHERE backend = ? AND configuration_digest = ? AND id < ?
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

// LatestConfigurationConformance reconstructs one configuration's current
// declaration: the newest record a backend appended for digest. Presence and
// validation follow LatestBackendConformance. Ward startup reads it to
// restore each image's proof, and the doctor to judge each configuration
// ward tracks.
func (tx *ReadTx) LatestConfigurationConformance(
	ctx context.Context, backend domain.RunnerBackendClass, digest domain.Digest,
) (domain.BackendConformance, bool, error) {
	row := tx.tx.QueryRowContext(ctx, latestConfigurationConformanceSQL,
		string(backend), string(digest))
	return scanBackendConformance(backend, row)
}

// latestConfigurationConformanceBefore recovers the row immediately
// preceding generation for one backend and configuration digest: in the
// append-only log, exactly the declaration of that configuration a
// supersession marker at that generation superseded. It deliberately returns
// a marker or a failure too, since the authenticate re-gate re-binds only
// when that declaration passed and must refuse otherwise. It shares the
// reconstruction-time validation of LatestBackendConformance: a row that no
// longer validates against the class ceiling fails closed rather than
// restoring a wider declaration.
func (tx *ReadTx) latestConfigurationConformanceBefore(
	ctx context.Context, backend domain.RunnerBackendClass, digest domain.Digest, generation uint64,
) (domain.BackendConformance, bool, error) {
	row := tx.tx.QueryRowContext(ctx, latestConfigurationConformanceBeforeSQL,
		string(backend), string(digest), generation)
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
// the current durable declaration of the admission's own backend
// configuration must exist, record a pass, and cover the admission's
// capability snapshot. A ward backend holds one declaration per configuration
// it proves (#3211), so a newer row for another configuration neither admits
// nor refuses this one. Like RequireUnattendedAdmissible it is a
// recording-time precondition on new unattended operation, not part of a
// record's meaning, so reconstruction (scanExecutionAdmission) deliberately
// does not re-run it: a lapsed or superseded conformance must stop what
// happens next, not make recorded history unreadable. attended_dev is exempt
// by the owner-ratified reading of #320: §5.7 admits a weaker, unproven
// runner class there on purpose.
func (tx *ReadTx) RequireBackendConformant(
	ctx context.Context, admission domain.ExecutionAdmission,
) error {
	return tx.requireBackendConformant(ctx, admission, backendConformanceMint)
}

// AuthenticateBackendConformant re-gates an already-recorded admission against
// its configuration's current conformance state, tolerating one thing the mint
// gate does not: a supersession marker for that configuration. The startup
// suite (and the scheduled doctor re-proof) writes that marker the instant a
// recheck of the configuration begins, before the marker's own passed outcome
// lands. A re-proof is a proof refresh, not a policy change, so an invocation
// already admitted against the proof the marker superseded stays authenticated
// across the window; without this, a re-proof racing an in-flight invocation's
// re-gate makes the invocation permanently unauthenticatable and durable-stops
// the daemon (issue #761). A failed proof, or a second marker, for the
// configuration is a real change and stays a refusal. Rows for other
// configurations do not matter. Whether this process could still run the
// admission's configuration at all is the caller's check
// (admissionauth.Authority).
func (tx *ReadTx) AuthenticateBackendConformant(
	ctx context.Context, admission domain.ExecutionAdmission,
) error {
	return tx.requireBackendConformant(ctx, admission, backendConformanceAuthenticate)
}

// backendConformanceGate selects how requireBackendConformant treats a latest
// row that is not itself a current passed proof. The mint gate refuses
// anything but a current passed row (work not yet admitted loses nothing by
// holding out a recheck window); the authenticate gate additionally recovers
// the passed proof a supersession marker for the same configuration
// superseded, so a persisted admission survives the window.
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
	// An unattended admission that names no configuration has no
	// per-configuration proof to check. Validate refuses one; this keeps the
	// exported gate closed for a caller that passes one unvalidated.
	if admission.BackendConfigurationDigest == "" {
		return fmt.Errorf("admission %q backend %q names no configuration: %w",
			admission.InvocationID, admission.Backend, domain.ErrAdmissionConfigurationMismatch)
	}
	return tx.requireConformantDeclaration(ctx, conformanceSubject{
		label:            fmt.Sprintf("admission %q", admission.InvocationID),
		class:            domain.RunnerBackendClass(admission.Backend),
		digest:           admission.BackendConfigurationDigest,
		capabilities:     admission.Capabilities,
		perConfiguration: true,
	}, gate)
}

// RequireHostVerificationConformant is the conformance half of §5.7's
// unattended condition for the credential-free macOS verification executor:
// the executor class's current durable declaration must exist, record a pass,
// be bound to the executor's configuration digest, and cover the capabilities
// the executor declares now. Unlike a ward admission it reads the newest row
// for the class, whatever its configuration: the executor holds one
// declaration, and a proof of any other configuration replaces it. The class
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
// refusal so the caller's record is named. perConfiguration judges it by the
// newest row for digest (a ward admission); otherwise by the newest row for
// the class (the executor).
type conformanceSubject struct {
	label            string
	class            domain.RunnerBackendClass
	digest           domain.Digest
	capabilities     domain.CapabilitySnapshot
	perConfiguration bool
}

func (tx *ReadTx) requireConformantDeclaration(
	ctx context.Context, subject conformanceSubject, gate backendConformanceGate,
) error {
	var (
		record domain.BackendConformance
		found  bool
		err    error
	)
	if subject.perConfiguration {
		record, found, err = tx.LatestConfigurationConformance(ctx, subject.class, subject.digest)
	} else {
		record, found, err = tx.LatestBackendConformance(ctx, subject.class)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", subject.label, err)
	}
	if !found {
		return fmt.Errorf("%s backend %q configuration %s: %w",
			subject.label, subject.class, subject.digest, ErrBackendNotConformant)
	}
	if record.Outcome != domain.ConformancePassed {
		// A supersession marker for the subject's configuration, in the
		// authenticate gate only, re-binds to the passed proof it superseded.
		// Every other non-passed latest row (a failure, or a marker for a
		// different configuration) refuses exactly as the mint gate does. The
		// recovered proof carries the same-digest capability ceiling; the
		// shared checks below then run against it unchanged.
		passed, recovered, err := tx.supersededSameConfigurationProof(ctx, subject, record, gate)
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
// admission to when its configuration's latest row is a supersession marker.
// recovered is false (refuse) unless every condition holds: the gate
// authenticates, the subject is judged per configuration, the latest row is a
// supersession marker (not a failure), the marker's configuration is bound
// and equals the admission's bound digest, and the exact declaration of that
// configuration the marker superseded passed. Re-binding to the superseded
// declaration, the configuration's immediately-preceding row, is what keeps
// every non-refresh history fatal: an already-admitted invocation is
// authenticated only against the state the configuration actually held when
// this re-proof began, never an older pass its log moved past (a fail-open the
// mint gate refuses). A failure or another marker (a recheck that never
// concluded) in that position means no completed pass currently authorizes
// the configuration, so tolerance closes. The marker's digest equality is
// checked here even though the per-configuration read already filters by it,
// so the rule does not rest on which query found the row.
func (tx *ReadTx) supersededSameConfigurationProof(
	ctx context.Context,
	subject conformanceSubject,
	latest domain.BackendConformance,
	gate backendConformanceGate,
) (domain.BackendConformance, bool, error) {
	if gate != backendConformanceAuthenticate || !subject.perConfiguration {
		return domain.BackendConformance{}, false, nil
	}
	if latest.Outcome != domain.ConformanceSuperseded {
		return domain.BackendConformance{}, false, nil
	}
	if !latest.ConfigurationBound() ||
		latest.ConfigurationDigest != subject.digest {
		return domain.BackendConformance{}, false, nil
	}
	superseded, found, err := tx.latestConfigurationConformanceBefore(
		ctx, subject.class, subject.digest, latest.Generation)
	if err != nil {
		return domain.BackendConformance{}, false, err
	}
	if !found || superseded.Outcome != domain.ConformancePassed ||
		superseded.ConfigurationDigest != subject.digest {
		return domain.BackendConformance{}, false, nil
	}
	return superseded, true, nil
}
