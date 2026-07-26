package domain

import (
	"fmt"
	"slices"
)

// RunnerCapability is an isolation property a runner backend either has or
// lacks (plan §5.7). Backends declare capabilities, policy states minimums,
// and an unmet minimum is a typed refusal, never a silent downgrade: a weaker
// isolation class is a different risk posture, not a degraded mode.
//
// The vocabulary lives here rather than in exec because an admitted
// declaration is persisted (see CapabilitySnapshot and ExecutionAdmission) and
// re-gated at reconstruction, and neither domain nor store may import exec.
// exec aliases this type, so there is still exactly one registration point.
type RunnerCapability string

// The named §5.7 capabilities.
const (
	CapDetachableWorkspace    RunnerCapability = "supports_detachable_workspace"
	CapPostExitExport         RunnerCapability = "supports_post_exit_export"
	CapReadOnlyRemount        RunnerCapability = "supports_read_only_remount"
	CapCredentialVolumeDetach RunnerCapability = "supports_credential_volume_detach"
	CapWorkspaceSnapshot      RunnerCapability = "supports_workspace_snapshot"
	CapNetworklessExport      RunnerCapability = "supports_networkless_export"
)

// AllRunnerCapabilities lists every valid RunnerCapability; it drives
// table-driven tests and is the single place a new capability is registered.
var AllRunnerCapabilities = []RunnerCapability{
	CapDetachableWorkspace,
	CapPostExitExport,
	CapReadOnlyRemount,
	CapCredentialVolumeDetach,
	CapWorkspaceSnapshot,
	CapNetworklessExport,
}

func (c RunnerCapability) valid() bool {
	switch c {
	case CapDetachableWorkspace, CapPostExitExport, CapReadOnlyRemount,
		CapCredentialVolumeDetach, CapWorkspaceSnapshot, CapNetworklessExport:
		return true
	default:
		return false
	}
}

// CapabilitySnapshot is the persisted form of a spawn-time capability
// declaration: a canonical (sorted, deduplicated) slice, not the runtime map
// exec declares them in. One admission therefore has exactly one byte form,
// which is what a content-addressed identity and write-once replay
// convergence both depend on.
type CapabilitySnapshot []RunnerCapability

// NewCapabilitySnapshot returns the canonical snapshot of the given
// capabilities: sorted, deduplicated, and detached from the caller's backing
// array. An empty input collapses to nil, so "declared nothing" has one
// representation. It does not reject unknown members; Validate does, at the
// boundary where an admission is built or decoded.
func NewCapabilitySnapshot(caps ...RunnerCapability) CapabilitySnapshot {
	if len(caps) == 0 {
		return nil
	}
	out := slices.Clone(caps)
	slices.Sort(out)
	out = slices.Compact(out)
	return out
}

// Validate reports whether the snapshot is canonical and every member
// registered. A decoded snapshot that is unsorted or carries a duplicate is
// rejected rather than normalized: two byte-forms for one declaration would
// give one admission two identities.
func (s CapabilitySnapshot) Validate() error {
	if s != nil && len(s) == 0 {
		return fmt.Errorf("capability snapshot: empty list must be nil: %w", ErrCapabilitiesNotCanonical)
	}
	for i, c := range s {
		if !c.valid() {
			return fmt.Errorf("capability %q: %w", c, ErrInvalidRunnerCapability)
		}
		if i == 0 {
			continue
		}
		switch {
		case c == s[i-1]:
			return fmt.Errorf("capability %q: %w", c, ErrDuplicate)
		case c < s[i-1]:
			return fmt.Errorf("capability %q after %q: %w", c, s[i-1], ErrCapabilitiesNotCanonical)
		}
	}
	return nil
}

// Has reports whether the snapshot declares c.
func (s CapabilitySnapshot) Has(c RunnerCapability) bool { return slices.Contains(s, c) }

// Clone returns an independent copy; nil clones to nil.
func (s CapabilitySnapshot) Clone() CapabilitySnapshot { return slices.Clone(s) }

// MissingCapabilities returns the floor members a declaration does not cover,
// sorted and deduplicated for deterministic rendering, or nil when the
// declaration satisfies the floor. An invalid capability name in the floor
// counts as missing, so a policy typo can never widen into an accidental pass.
//
// It is the shared predicate behind both admission (exec.CheckCapabilities,
// which gates a spawn against the live declaration) and re-admission (the
// store's reconstruction gate, which re-checks a persisted snapshot against
// the floor current policy states). One implementation means the two cannot
// disagree about what "satisfies the floor" means.
func MissingCapabilities(declared CapabilitySnapshot, floor []RunnerCapability) []RunnerCapability {
	var missing []RunnerCapability
	for _, c := range floor {
		if !c.valid() || !declared.Has(c) {
			missing = append(missing, c)
		}
	}
	slices.Sort(missing)
	return slices.Compact(missing)
}
