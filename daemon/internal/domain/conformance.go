package domain

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
