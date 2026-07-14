package exec

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Capability is an isolation property a runner backend either has or lacks
// (plan §5.7). Backends declare capabilities, policy states minimums, and an
// unmet minimum is a typed refusal, never a silent downgrade: a weaker
// isolation class is a different risk posture, not a degraded mode.
type Capability string

// The five named §5.7 capabilities.
const (
	CapDetachableWorkspace    Capability = "supports_detachable_workspace"
	CapPostExitExport         Capability = "supports_post_exit_export"
	CapReadOnlyRemount        Capability = "supports_read_only_remount"
	CapCredentialVolumeDetach Capability = "supports_credential_volume_detach"
	CapWorkspaceSnapshot      Capability = "supports_workspace_snapshot"
)

// AllCapabilities lists every valid Capability; it drives table-driven tests
// and is the single place a new capability is registered.
var AllCapabilities = []Capability{
	CapDetachableWorkspace,
	CapPostExitExport,
	CapReadOnlyRemount,
	CapCredentialVolumeDetach,
	CapWorkspaceSnapshot,
}

func (c Capability) valid() bool {
	switch c {
	case CapDetachableWorkspace, CapPostExitExport, CapReadOnlyRemount,
		CapCredentialVolumeDetach, CapWorkspaceSnapshot:
		return true
	default:
		return false
	}
}

// CapabilitySet is the set of capabilities a backend declares. Sets are built
// with NewCapabilitySet and queried with Has; the map form keeps membership
// checks O(1) and JSON out of scope (capability declarations are runtime
// facts, not persisted contracts).
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

// RunnerBackend is an execution environment provider (plan §5.7). Phase 1
// needs only the declaring side of the capability model: a backend names
// itself and states what it supports, fixed at conformance time, and policy
// checks minimums against that declaration before any job is placed. Runner
// lifecycle operations land with the ward lane's first backend.
type RunnerBackend interface {
	// Name identifies the backend in policy, refusals, and audit records.
	Name() string
	// Capabilities returns the backend's declared capability set.
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

// CheckCapabilities checks a backend's declared capabilities against a
// policy minimum. It returns nil only when every required capability is
// declared; otherwise a *CapabilityRefusal listing all missing capabilities.
// There is no partial success and no substitution: no silent downgrade
// (§5.7). An invalid capability name in the minimum is refused too, so a
// policy typo can never widen into an accidental pass.
func CheckCapabilities(backend RunnerBackend, minimum []Capability) error {
	declared := backend.Capabilities()
	var missing []Capability
	for _, c := range minimum {
		if !c.valid() || !declared.Has(c) {
			missing = append(missing, c)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	slices.Sort(missing)
	missing = slices.Compact(missing)
	return &CapabilityRefusal{Backend: backend.Name(), Missing: missing}
}
