package ward

import (
	"fmt"
	"slices"

	"github.com/freeside-ai/freeside/daemon/internal/exec"
)

// Backend realizes the fresh_vm_read_only_volume_handoff isolation class on
// a Runtime. Construct it with New; the zero value declares nothing and
// gates nothing.
type Backend struct {
	rt          Runtime
	cfg         Config
	initialized bool
}

// Compile-time contract assertion (exec package convention).
var _ exec.RunnerBackend = (*Backend)(nil)

// New builds a Backend over rt, applying cfg defaults and refusing an
// invalid configuration.
func New(rt Runtime, cfg Config) (*Backend, error) {
	if rt == nil {
		return nil, fmt.Errorf("%w: Runtime is required", ErrInvalidConfig)
	}
	cfg = cfg.withDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	// Config is caller-owned. Freeze every reference field before it becomes
	// the expected allowlist that runtime-observed state is compared against.
	cfg.ExporterCommand = slices.Clone(cfg.ExporterCommand)
	return &Backend{rt: rt, cfg: cfg, initialized: true}, nil
}

// Name identifies the backend in policy, refusals, and audit records.
func (b *Backend) Name() string { return BackendName }

// Capabilities returns the backend's declared capability set, freshly built
// on every call so no caller can mutate the declaration (§5.3 fixed at
// spawn; exec.CheckCapabilities snapshots it at admission).
//
// The declaration is exactly what the spike proved on Apple container 1.1.0:
//
//   - supports_detachable_workspace: a named volume survives writer exit and
//     deletion and reattaches to a different VM.
//   - supports_post_exit_export: composed, narrow reading only — the runtime
//     exports a stopped root filesystem, never a mounted workspace volume
//     directly; the exporter copies the read-only workspace into its rootfs
//     first (the accepted workspace-copy cost).
//   - supports_read_only_remount: the same volume mounts rw in the writer
//     and ro in the exporter.
//
// supports_credential_volume_detach and supports_workspace_snapshot are
// refuted on this runtime and are never declared: the spike proved a guest
// unmount is not a credential-device detach (the refuted same-VM fallback
// class), and volume snapshotting has no public support.
func (b *Backend) Capabilities() exec.CapabilitySet {
	if b == nil || !b.initialized {
		return exec.NewCapabilitySet()
	}
	return exec.NewCapabilitySet(
		exec.CapDetachableWorkspace,
		exec.CapPostExitExport,
		exec.CapReadOnlyRemount,
	)
}
