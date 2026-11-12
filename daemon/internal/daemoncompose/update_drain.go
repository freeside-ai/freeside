package daemoncompose

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// ExecutableBuild returns the build this process runs: the sha256 digest of
// its executable, the form the update drain log records (issue #3103's
// decision note). An update replaces the executable, so a new build reads a
// new digest.
func ExecutableBuild() (domain.Digest, error) {
	path, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate the daemon executable: %w", err)
	}
	file, err := os.Open(path) //nolint:gosec // G304: the running executable's own path, from os.Executable
	if err != nil {
		return "", fmt.Errorf("open the daemon executable: %w", err)
	}
	defer func() { _ = file.Close() }() // read-only; a close error loses nothing
	sum := sha256.New()
	if _, err := io.Copy(sum, file); err != nil {
		return "", fmt.Errorf("read the daemon executable: %w", err)
	}
	return domain.Digest(contentaddr.Format(sum.Sum(nil))), nil
}

// liftUpdateDrainAtStart lifts a drain a different build requested. Start
// calls it after the stored-state steps and before any loop that can launch
// work. A drain this build requested stays in force, and is logged so the
// operator sees why nothing launches.
func liftUpdateDrainAtStart(ctx context.Context, drain engine.UpdateDrain, logger *slog.Logger) error {
	lifted, err := drain.LiftForBuild(ctx)
	if err != nil {
		return fmt.Errorf("lift the update drain: %w", err)
	}
	if lifted {
		logger.Info("lifted the update drain a previous build requested", "build", drain.Build)
		return nil
	}
	var inForce *domain.UpdateDrainTransition
	if err := drain.Store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		inForce, err = tx.UpdateDrainInForce(ctx)
		return err
	}); err != nil {
		return fmt.Errorf("read the update drain: %w", err)
	}
	if inForce != nil {
		logger.Warn("an update drain this build requested is still in force; nothing launches until freesided drain -cancel",
			"since", inForce.OccurredAt)
	}
	return nil
}

// ProbeUnlessDraining returns the credential-integrity probe one doctor pass
// may run, and the end of its running-probe count. The scheduled doctor and
// the operator's doctor command both call it. While an update drain is
// in force it returns a nil probe, so the pass reports the marks earlier
// passes recorded and starts no observer container; a non-nil probe that
// observed nothing would read as a pass that found nothing. The caller calls
// end once the probe has run. A nil logger logs nothing.
func ProbeUnlessDraining(
	ctx context.Context, st *store.Store, running *engine.RunningWork, probe CredentialIntegrityProbe,
	logger *slog.Logger,
) (CredentialIntegrityProbe, func(), error) {
	if probe == nil {
		return nil, func() {}, nil
	}
	end, err := running.BeginUnlessDraining(ctx, st, domain.RestartBlockerProbe)
	if errors.Is(err, store.ErrUpdateDrainInForce) {
		if logger != nil {
			logger.Info("credential-integrity probe skipped while an update drain is in force")
		}
		return nil, func() {}, nil
	}
	if err != nil {
		return nil, nil, err
	}
	return probe, end, nil
}
