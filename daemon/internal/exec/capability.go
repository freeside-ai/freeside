package exec

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// Capability is an isolation property a runner backend either has or lacks
// (plan §5.7). Backends declare capabilities, policy states minimums, and an
// unmet minimum is a typed refusal, never a silent downgrade: a weaker
// isolation class is a different risk posture, not a degraded mode.
//
// The vocabulary itself lives in domain, because an admitted declaration is
// persisted in the execution record and re-gated when the store reconstructs
// it, and neither domain nor store may import exec. This alias keeps exec the
// place drivers and backends name capabilities from, with one registration
// point behind it.
type Capability = domain.RunnerCapability

// The named §5.7 capabilities.
const (
	CapDetachableWorkspace    = domain.CapDetachableWorkspace
	CapPostExitExport         = domain.CapPostExitExport
	CapReadOnlyRemount        = domain.CapReadOnlyRemount
	CapCredentialVolumeDetach = domain.CapCredentialVolumeDetach
	CapWorkspaceSnapshot      = domain.CapWorkspaceSnapshot
	CapNetworklessExport      = domain.CapNetworklessExport
)

// AllCapabilities lists every valid Capability; it drives table-driven tests
// and aliases the single place a new capability is registered.
var AllCapabilities = domain.AllRunnerCapabilities

// CapabilitySet is the set of capabilities a backend declares. Sets are built
// with NewCapabilitySet and queried with Has; the map form keeps membership
// checks O(1) and JSON out of scope. A live declaration stays a runtime fact:
// what persists is Snapshot's canonical slice, which is a different type with
// a single byte form.
type CapabilitySet map[Capability]struct{}

// NewCapabilitySet builds a set from the given capabilities.
func NewCapabilitySet(caps ...Capability) CapabilitySet {
	s := make(CapabilitySet, len(caps))
	for _, c := range caps {
		s[c] = struct{}{}
	}
	return s
}

// Has reports whether the set contains c.
func (s CapabilitySet) Has(c Capability) bool {
	_, ok := s[c]
	return ok
}

// Snapshot renders the set in the canonical form the domain persists: sorted,
// deduplicated, and detached from the map, so one declaration cannot serialize
// two ways depending on map iteration order.
func (s CapabilitySet) Snapshot() domain.CapabilitySnapshot {
	if len(s) == 0 {
		return nil
	}
	return domain.NewCapabilitySnapshot(slices.Collect(maps.Keys(s))...)
}

// Clone returns an independent copy of the set. CapabilitySet is a map, so
// handing one back by reference lets a holder mutate a backend's declaration
// after it was read; returning a clone at every boundary keeps the returned
// value detached from the source. maps.Clone preserves nil (a nil set clones
// to nil), so membership semantics are unchanged.
func (s CapabilitySet) Clone() CapabilitySet { return maps.Clone(s) }

// RunnerBackend is an execution environment provider (plan §5.7). Phase 1
// needs only the declaring side of the capability model: a backend names
// itself and states what it supports, fixed at conformance time, and policy
// checks minimums against that declaration before any job is placed. Runner
// lifecycle operations land with the ward lane's first backend.
type RunnerBackend interface {
	// Name identifies the backend in policy, refusals, and audit records.
	Name() string
	// Capabilities returns the backend's declared capability set as an
	// independent copy (see CapabilitySet.Clone): a caller must not be able to
	// mutate the backend's declaration through the returned set, so the class
	// stays fixed at spawn (§5.3) no matter which backend implements this.
	Capabilities() CapabilitySet
}

// ErrCapabilityRefused is the class sentinel for capability refusals;
// CapabilityRefusal unwraps to it so errors.Is matches the class while
// errors.As reaches the details.
var ErrCapabilityRefused = errors.New("backend capabilities below policy minimum")

// CapabilityRefusal is the typed refusal for an unmet policy minimum (§5.7):
// it names the backend and every missing capability so the caller can record
// or render the refusal without parsing an error string.
type CapabilityRefusal struct {
	Backend string
	// Missing lists the required capabilities the backend does not declare,
	// sorted for deterministic rendering.
	Missing []Capability
}

func (e *CapabilityRefusal) Error() string {
	missing := make([]string, len(e.Missing))
	for i, c := range e.Missing {
		missing[i] = string(c)
	}
	return fmt.Sprintf("backend %q missing required capabilities: %s",
		e.Backend, strings.Join(missing, ", "))
}

// Unwrap makes errors.Is(err, ErrCapabilityRefused) match the refusal class.
func (e *CapabilityRefusal) Unwrap() error { return ErrCapabilityRefused }

// Admission is the immutable capability declaration that admitted a run: the
// spawn-time snapshot policy and audit bind to (§5.3). Declared is a frozen
// clone taken at the admission decision, independent of the live backend, so a
// later mutation of the backend's capabilities cannot silently widen or narrow
// the admitted class (§5.7). The zero Admission is returned on refusal.
type Admission struct {
	// Backend is the admitting backend's name, as it appears in audit records.
	Backend string
	// Declared is the frozen set of capabilities the backend declared at
	// admission, not the backend's live map.
	Declared CapabilitySet
}

// CheckCapabilities checks a backend's declared capabilities against a policy
// minimum and, on success, returns the admitted-capability snapshot. It
// returns a nil error only when every required capability is declared;
// otherwise the zero Admission and a *CapabilityRefusal listing all missing
// capabilities. There is no partial success and no substitution: no silent
// downgrade (§5.7). An invalid capability name in the minimum is refused too,
// so a policy typo can never widen into an accidental pass.
//
// The backend's declaration is read once here and the returned snapshot is a
// frozen clone of it, so the gate decision and the audited snapshot come from
// the same read and cannot diverge. This is the single admission entry point:
// a second function that re-read Capabilities() would reopen the
// observe-a-different-set race this snapshot exists to close.
func CheckCapabilities(backend RunnerBackend, minimum []Capability) (Admission, error) {
	declared := backend.Capabilities()
	// domain.MissingCapabilities is the same predicate the store re-runs when
	// it reconstructs a persisted admission, so a snapshot cannot pass the
	// spawn gate under one reading of the floor and fail re-admission under
	// another.
	if missing := domain.MissingCapabilities(declared.Snapshot(), minimum); len(missing) > 0 {
		return Admission{}, &CapabilityRefusal{Backend: backend.Name(), Missing: missing}
	}
	return Admission{Backend: backend.Name(), Declared: declared.Clone()}, nil
}
