package daemoncompose

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/macverify"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// MacverifyExecutorConfig names the macOS verification executor the daemon
// places macos_host recipe commands on. An empty Root configures no
// executor: every such command then fails its verification by name.
type MacverifyExecutorConfig struct {
	// Root is the executor root `freesided macverify install` created.
	Root string
	// Slots are the slot account names the install created.
	Slots []string
	// XcodeBuild and Swift pin the host toolchain recipe commands run
	// under; a host whose toolchain differs refuses each job.
	XcodeBuild, Swift string
	// PortBase is the first reserved port, as the install wrote it.
	PortBase int
	// ListenPort is the daemon's listen port, 0 when the kernel picks one.
	// No slot may reserve it, or a job could reach the daemon's API.
	ListenPort int
}

// proveHostVerification runs the executor's full conformance pass and records
// its outcome.
func proveHostVerification(
	ctx context.Context, backend *macverify.Backend, recorder macverify.ConformanceRecorder,
) error {
	suite, err := macverify.NewSuite(backend, macverify.SuiteFixture{}, macverify.WithConformanceRecorder(recorder))
	if err != nil {
		return err
	}
	return suite.Full(ctx)
}

// ComposeHostVerificationExecutor builds the executor's backend, or returns
// nil when cfg.Root is empty. Under runConformance it runs the full pass
// through prove (proveHostVerification outside tests), and a failed pass
// fails startup, as the ward's does. Otherwise it restores
// the latest durable record when it passed for this exact configuration. A
// missing, failed, or stale record leaves the backend undeclared without
// stopping startup: unattended admission then refuses each macos_host job by
// name. Attended admission needs no declaration for this class
// (domain.RequiredCapabilities), so there only the pre-job probe confines a
// job.
//
// Tier A: it decides which durable proof the executor declares from. The
// restore re-checks the record's class, outcome, and configuration digest
// against the backend built from current flags (macverify.RestoreConformance),
// so a record for an older configuration or binary never declares.
func ComposeHostVerificationExecutor(
	ctx context.Context, st *store.Store, cfg MacverifyExecutorConfig, runConformance bool, logger *slog.Logger,
	prove func(context.Context, *macverify.Backend, macverify.ConformanceRecorder) error,
) (*macverify.Backend, error) {
	if cfg.Root == "" {
		return nil, nil
	}
	slots := slices.Sorted(slices.Values(cfg.Slots))
	config := macverify.Config{
		ExecutorRoot:      cfg.Root,
		SandboxProfile:    macverify.DefaultSandboxProfile(),
		PacketFilterRules: macverify.PacketFilterRules(slots),
		Slots:             slots,
		Toolchain:         macverify.Toolchain{XcodeBuild: cfg.XcodeBuild, Swift: cfg.Swift},
		PortBase:          cfg.PortBase,
	}
	if cfg.ListenPort != 0 && config.ReservesPort(cfg.ListenPort) {
		return nil, fmt.Errorf("configure the macOS verification executor: the daemon's listen port %d is a slot's reserved port; choose another -listen port or -macverify-port-base", cfg.ListenPort)
	}
	backend, err := macverify.New(config)
	if err != nil {
		return nil, fmt.Errorf("configure the macOS verification executor: %w", err)
	}
	if runConformance {
		if err := prove(ctx, backend, StoreConformanceRecorder{Store: st}); err != nil {
			return nil, fmt.Errorf("run macOS verification executor conformance: %w", err)
		}
		return backend, nil
	}
	var (
		record domain.BackendConformance
		found  bool
	)
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		record, found, err = tx.LatestBackendConformance(ctx, domain.BackendCredentialFreeHostVerification)
		return err
	}); err != nil {
		return nil, fmt.Errorf("read macOS verification executor conformance: %w", err)
	}
	digest := backend.ConfigurationDigest()
	switch {
	case !found:
		warnUndeclaredHostVerification(logger, digest, "no conformance record")
	case record.Outcome != domain.ConformancePassed || record.ConfigurationDigest != digest:
		warnUndeclaredHostVerification(logger, digest, fmt.Sprintf(
			"the latest conformance record is %s for configuration %s", record.Outcome, record.ConfigurationDigest))
	default:
		if err := backend.RestoreConformance(record); err != nil {
			if !errors.Is(err, macverify.ErrHostUnsupported) {
				return nil, fmt.Errorf("restore macOS verification executor conformance: %w", err)
			}
			warnUndeclaredHostVerification(logger, digest, err.Error())
		}
	}
	return backend, nil
}

func warnUndeclaredHostVerification(logger *slog.Logger, digest domain.Digest, why string) {
	if logger == nil {
		return
	}
	logger.Warn("macOS verification executor declares nothing; unattended admission refuses macos_host recipe commands until a pass under -run-conformance",
		"configuration_digest", digest, "reason", why)
}
