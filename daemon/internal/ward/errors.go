package ward

import (
	"errors"
	"fmt"
)

// Check names one check of the workspace-handoff gate's required backend
// contract (docs/spikes/workspace-handoff.md, plan §5.7). The numbering
// follows the spike; check 6 (running the trusted export helper) is gauntlet
// logic shipped inside the pinned exporter image and has no ward check name.
// The zero value "" is invalid by design (domain enum convention).
type Check string

const (
	// CheckCredentialSeparation is spike check 1: the workspace is its own
	// named volume, every provider credential is a different mount, and no
	// credential appears in the container root filesystem or workspace.
	CheckCredentialSeparation Check = "credential_separation" //nolint:gosec // a check name, not a credential
	// CheckControlPlaneIsolation is spike check 2: container CLI/XPC control
	// stays outside the agent VM; no host CLI, runtime socket, daemon state,
	// SSH agent, home directory, or registry credential is mounted in.
	CheckControlPlaneIsolation Check = "control_plane_isolation"
	// CheckWriterTermination is spike check 3: the writer's termination is
	// observed state (state: stopped, then deleted, then absent from the
	// full container list), never scheduling intent.
	CheckWriterTermination Check = "writer_termination"
	// CheckExporterAllowlist is spike check 4: the exporter, inspected before
	// execution, carries exactly one persistent mount (the expected workspace
	// volume, read-only, at the expected target) and nothing else.
	CheckExporterAllowlist Check = "exporter_allowlist"
	// CheckInExporterVerification is spike check 5: inside the exporter,
	// /proc/mounts says ro, a write probe fails, and expected credential and
	// host paths are absent, proven by the exported proof file.
	CheckInExporterVerification Check = "in_exporter_verification"
	// CheckExportVerification is spike check 7: exported digests verify
	// against the manifest and the output passes §5.4 scanning before it
	// reaches the out-of-process gauntlet worker.
	CheckExportVerification Check = "export_verification"
	// CheckTeardown is the spike's teardown requirement: the exporter
	// container is destroyed and no handoff volume is left behind.
	CheckTeardown Check = "teardown"
)

// AllChecks lists every valid Check; it drives table-driven tests and is the
// single place a new check is registered.
var AllChecks = []Check{
	CheckCredentialSeparation,
	CheckControlPlaneIsolation,
	CheckWriterTermination,
	CheckExporterAllowlist,
	CheckInExporterVerification,
	CheckExportVerification,
	CheckTeardown,
}

func (c Check) valid() bool {
	switch c {
	case CheckCredentialSeparation, CheckControlPlaneIsolation,
		CheckWriterTermination, CheckExporterAllowlist,
		CheckInExporterVerification, CheckExportVerification, CheckTeardown:
		return true
	default:
		return false
	}
}

// ErrConformance is the class sentinel for handoff conformance failures;
// ConformanceFailure unwraps to it so errors.Is matches the class while
// errors.As reaches the details (the exec package's typed-refusal
// convention).
var ErrConformance = errors.New("workspace-handoff conformance check failed")

// ConformanceFailure is the typed failure of one gate check. The gate fails
// closed: any ConformanceFailure means the handoff produced no trusted
// export. Reason carries observed facts (states, mount targets, digests),
// never credential material.
type ConformanceFailure struct {
	// Backend is the failing backend's name, as it appears in audit records.
	Backend string
	// Check is the contract check that failed.
	Check Check
	// Reason states the observed violation.
	Reason string
}

func (e *ConformanceFailure) Error() string {
	return fmt.Sprintf("backend %q failed handoff check %q: %s",
		e.Backend, e.Check, e.Reason)
}

// Unwrap makes errors.Is(err, ErrConformance) match the failure class.
func (e *ConformanceFailure) Unwrap() error { return ErrConformance }

// failf builds a ConformanceFailure for check c with a formatted reason.
func failf(c Check, format string, args ...any) error {
	return &ConformanceFailure{
		Backend: BackendName,
		Check:   c,
		Reason:  fmt.Sprintf(format, args...),
	}
}
