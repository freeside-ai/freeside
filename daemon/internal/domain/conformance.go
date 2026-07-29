package domain

import (
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
)

// RunnerBackendClass names an isolation class a runner backend realizes and
// declares as its identity (plan §5.7: "the name is the runner backend's
// declared identity"). The vocabulary lives here rather than in ward because
// a backend's proven conformance is persisted and re-gated by the store, and
// store may not import ward; ward derives its backend name from this
// registration point.
type RunnerBackendClass string

// BackendFreshVMReadOnlyVolumeHandoff is the strong class the
// workspace-handoff spike proved on Apple container 1.1.0 (§5.7): the writer
// and exporter run in separate VMs, and the workspace crosses between them on
// a detachable volume remounted read-only.
const BackendFreshVMReadOnlyVolumeHandoff RunnerBackendClass = "fresh_vm_read_only_volume_handoff"

// AllRunnerBackendClasses lists every valid RunnerBackendClass; it drives
// table-driven tests and is the single place a new class is registered.
var AllRunnerBackendClasses = []RunnerBackendClass{BackendFreshVMReadOnlyVolumeHandoff}

func (c RunnerBackendClass) valid() bool {
	switch c {
	case BackendFreshVMReadOnlyVolumeHandoff:
		return true
	default:
		return false
	}
}

// ProvableCapabilities returns the capability ceiling a backend class can
// ever earn: the set its conformance suite is able to prove, with the
// capabilities refuted on the class's runtime excluded. It reports false for
// an unregistered class, so an unknown class has no ceiling and every claim
// against it fails closed. The ceiling is what lets the store refuse an
// over-claiming conformance record mechanically (issue #320): the store
// cannot re-run a probe, but it can know that no probe for this class could
// have proven the claimed capability.
//
// The switch dispatches behaviour and omits default, so registering a new
// class forces a decision about its ceiling here.
func ProvableCapabilities(class RunnerBackendClass) (CapabilitySnapshot, bool) {
	switch class {
	case BackendFreshVMReadOnlyVolumeHandoff:
		// The handoff spike's proven set plus the two suite-earned proofs.
		// supports_credential_volume_detach and supports_workspace_snapshot
		// are refuted on Apple container 1.1.0 (§5.7: the same-VM fallback is
		// refuted by execution, and volume snapshotting has no public
		// support), so a record claiming either is an over-claim by
		// construction.
		return NewCapabilitySnapshot(
			CapDetachableWorkspace,
			CapPostExitExport,
			CapReadOnlyRemount,
			CapNetworklessExport,
			CapEnforcedProviderEgress,
		), true
	}
	return nil, false
}

// ConformanceOutcome is the durable state one conformance-log append records
// (§5.7): the outcome of a completed full pass, or the superseding marker a
// beginning recheck writes. The marker exists because the in-memory
// generation guard clears the declaration the instant a recheck begins, and
// the durable log must not lag it: an admission whose snapshot froze just
// before the recheck began would otherwise still be admitted against the
// previous pass's row for as long as the recheck runs.
type ConformanceOutcome string

const (
	ConformancePassed ConformanceOutcome = "passed"
	ConformanceFailed ConformanceOutcome = "failed"
	// ConformanceSuperseded is the durable form of beginConformanceProof: a
	// recheck has begun, so the previous declaration no longer admits. The
	// recheck's own completed outcome supersedes it in turn.
	ConformanceSuperseded ConformanceOutcome = "superseded"
)

// AllConformanceOutcomes lists every valid ConformanceOutcome.
var AllConformanceOutcomes = []ConformanceOutcome{
	ConformancePassed, ConformanceFailed, ConformanceSuperseded,
}

func (o ConformanceOutcome) valid() bool {
	switch o {
	case ConformancePassed, ConformanceFailed, ConformanceSuperseded:
		return true
	default:
		return false
	}
}

// BackendConformance is the durable record of what a named backend's last
// completed full conformance pass proved (§5.7, issues #327/#320): the class,
// exact normalized backend configuration, proven capability declaration, and
// completion time. The store appends one per completed pass and the newest
// record per backend is the backend's current declaration; an unattended
// admission is gated against it at the write boundary, so a writer's
// in-process claim is never the thing that admits.
//
// The record is a ceiling, never a grant: holding a passed record admits
// nothing by itself, because the live capability floor still has to be met by
// the backend's in-memory declaration, which an unfinished or failed recheck
// has already cleared.
type BackendConformance struct {
	Backend RunnerBackendClass `json:"backend"`
	Outcome ConformanceOutcome `json:"outcome"`
	// ConfigurationDigest binds the proof to the normalized runtime,
	// exporter, endpoint, and mount-path configuration the suite exercised.
	// The all-zero digest is reserved for rows migrated from the pre-binding
	// schema; those rows remain readable audit history but cannot admit or
	// restore a backend.
	ConfigurationDigest Digest `json:"configuration_digest"`
	// Capabilities is the proven declaration of a passed pass, and nil exactly
	// when the pass failed: a failed pass proves nothing, and letting it keep
	// a base set would let a broken backend keep admitting.
	Capabilities CapabilitySnapshot `json:"capabilities"`
	// Generation is the store-assigned, per-backend-monotonic proof
	// generation: zero on a record that has not been persisted yet, and the
	// row identity once it has. The store stamps it at append and range-checks
	// it at reconstruction; no caller-supplied value is ever trusted.
	Generation uint64 `json:"generation"`
	// ProvedAt is the UTC instant the pass completed. It is recorded for
	// audit; supersession is decided by Generation, never by comparing
	// timestamps.
	ProvedAt time.Time `json:"proved_at"`
}

// BackendConformanceInput carries the caller-supplied fields of a
// BackendConformance. It has no Generation field: the generation is the
// store's append identity, so no input path can set it.
type BackendConformanceInput struct {
	Backend             RunnerBackendClass
	Outcome             ConformanceOutcome
	ConfigurationDigest Digest
	Capabilities        CapabilitySnapshot
	ProvedAt            time.Time
}

// UnboundBackendConfigurationDigest marks conformance rows migrated from the
// schema that predated configuration binding. It is a valid content-address
// shape so old audit rows remain reconstructable, but constructors and write
// boundaries refuse it as authority.
const UnboundBackendConfigurationDigest Digest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"

// NewBackendConformance builds a validated record in canonical form: the
// capability snapshot is canonicalized and detached from the caller's slice,
// the configuration must be concretely bound, and the timestamp is normalized
// to UTC.
func NewBackendConformance(in BackendConformanceInput) (BackendConformance, error) {
	c := BackendConformance{
		Backend:             in.Backend,
		Outcome:             in.Outcome,
		ConfigurationDigest: in.ConfigurationDigest,
		Capabilities:        NewCapabilitySnapshot(in.Capabilities...),
		ProvedAt:            in.ProvedAt.UTC(),
	}
	if err := c.Validate(); err != nil {
		return BackendConformance{}, err
	}
	if !c.ConfigurationBound() {
		return BackendConformance{}, fmt.Errorf(
			"backend conformance configuration: %w", ErrConformanceConfigurationUnbound)
	}
	return c, nil
}

// ConfigurationBound distinguishes a proof produced for a concrete active
// backend configuration from migrated pre-binding audit history.
func (c BackendConformance) ConfigurationBound() bool {
	return c.ConfigurationDigest != "" &&
		c.ConfigurationDigest != UnboundBackendConfigurationDigest
}

// Validate checks structure, configuration-digest shape, and the class
// ceiling. The ceiling lives here rather than in a transaction-scoped gate
// because it is compiled-in vocabulary, not live policy: no store state can
// change what a class's suite could ever prove, so every boundary that decodes
// or accepts a record enforces it identically. Generation is deliberately
// unconstrained: zero means not yet persisted, and the store stamps and
// range-checks the persisted value itself.
func (c BackendConformance) Validate() error {
	if !c.Backend.valid() {
		return fmt.Errorf("backend conformance class %q: %w", c.Backend, ErrInvalidRunnerBackendClass)
	}
	if !c.Outcome.valid() {
		return fmt.Errorf("backend conformance outcome %q: %w", c.Outcome, ErrInvalidConformanceOutcome)
	}
	if !contentaddr.Valid(string(c.ConfigurationDigest)) {
		return fmt.Errorf("backend conformance configuration digest %q: %w",
			c.ConfigurationDigest, ErrConformanceConfigurationUnbound)
	}
	if err := c.Capabilities.Validate(); err != nil {
		return fmt.Errorf("backend conformance capabilities: %w", err)
	}
	if c.Outcome != ConformancePassed && c.Capabilities != nil {
		return fmt.Errorf("backend conformance for %q: %w", c.Backend, ErrConformanceCapabilitiesWithoutPass)
	}
	ceiling, ok := ProvableCapabilities(c.Backend)
	if !ok {
		// valid() passed but the class has no registered ceiling: a drift
		// between the two registration points fails closed rather than
		// admitting an unbounded claim.
		return fmt.Errorf("backend conformance class %q has no registered ceiling: %w",
			c.Backend, ErrInvalidRunnerBackendClass)
	}
	if excess := ExcessCapabilities(c.Capabilities, ceiling); len(excess) > 0 {
		return fmt.Errorf("backend conformance for %q claims %v beyond the class ceiling: %w",
			c.Backend, excess, ErrConformanceOverclaim)
	}
	if c.ProvedAt.IsZero() {
		return fmt.Errorf("backend conformance proved_at: %w", ErrMissingTimestamp)
	}
	return nil
}

// ExcessCapabilities returns the members of claimed that allowed does not
// cover, sorted and deduplicated for deterministic rendering, or nil when
// claimed is within allowed. It is MissingCapabilities's dual: the floor
// predicate asks "does the declaration reach the minimum", this one asks
// "does the claim exceed the ceiling". An invalid capability name in the
// claim counts as excess, so an unregistered name can never ride inside an
// otherwise-plausible claim.
func ExcessCapabilities(claimed CapabilitySnapshot, allowed CapabilitySnapshot) []RunnerCapability {
	var excess []RunnerCapability
	for _, c := range claimed {
		if !c.valid() || !allowed.Has(c) {
			excess = append(excess, c)
		}
	}
	return NewCapabilitySnapshot(excess...)
}
