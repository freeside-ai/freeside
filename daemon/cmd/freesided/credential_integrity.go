package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/operations"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
	"github.com/freeside-ai/freeside/daemon/internal/wardstore"
)

// credentialIntegrityProbe is one live probe pass, as the doctor consumes it.
type credentialIntegrityProbe func(context.Context) ([]operations.CredentialIntegrityOutcome, error)

// runOnce runs the probe now and returns a probe that replays that one
// result, so a caller can observe outside a lock and let the doctor consume
// the result inside it. A nil probe stays nil: no pass, recorded marks only.
func (p credentialIntegrityProbe) runOnce(ctx context.Context) credentialIntegrityProbe {
	if p == nil {
		return nil
	}
	outcomes, err := p(ctx)
	return func(context.Context) ([]operations.CredentialIntegrityOutcome, error) {
		return outcomes, err
	}
}

// credentialIntegrityObservers are the two store reads a probe pass makes.
type credentialIntegrityObservers struct {
	setupToken func(ctx context.Context, volume string) (ward.SetupTokenIntegrity, error)
	codexStore func(path string) (ward.CodexStoreIntegrity, error)
}

// productionCredentialIntegrityObservers reads setup-token volumes through
// the daemon's pinned exporter image and Codex stores under the root the
// review lifecycle reads them from. A Codex store outside that root, or any
// Codex store when no review root is configured, fails its observation and
// is reported as not checked.
func productionCredentialIntegrityObservers(
	cfg claudeDriverConfig, runtime ward.Runtime,
) credentialIntegrityObservers {
	authorize := productionRigRuntimeAuthorizer(cfg.StateDir, cfg.RigTokenFile)
	return credentialIntegrityObservers{
		setupToken: func(ctx context.Context, volume string) (ward.SetupTokenIntegrity, error) {
			return ward.ObserveSetupTokenIntegrity(ctx, runtime, cfg.ExporterImage, volume, authorize)
		},
		codexStore: func(path string) (ward.CodexStoreIntegrity, error) {
			return ward.ObserveCodexStoreIntegrity(cfg.ReviewInputRoot, path)
		},
	}
}

// newCredentialIntegrityProbe composes the probe and narrows its results to
// what the doctor reports: enrollment ids and fixed codes. A skipped store's
// cause goes to the log here and nowhere else.
func newCredentialIntegrityProbe(
	st *store.Store, observers credentialIntegrityObservers, now func() time.Time, logger *slog.Logger,
) credentialIntegrityProbe {
	probe := wardstore.IntegrityProbe{
		Store:             st,
		ObserveSetupToken: observers.setupToken,
		ObserveCodexStore: observers.codexStore,
		Now:               func() time.Time { return now().UTC() },
	}
	return func(ctx context.Context) ([]operations.CredentialIntegrityOutcome, error) {
		results, err := probe.Run(ctx)
		if err != nil {
			return nil, err
		}
		outcomes := make([]operations.CredentialIntegrityOutcome, 0, len(results))
		for _, result := range results {
			if result.Skipped != "" && logger != nil {
				logger.Warn("credential integrity: store not checked",
					"auth_identity", result.AuthIdentityID, "enrollment", result.EnrollmentID,
					"reason", result.Skipped, "error", result.Err)
			}
			if result.CorruptionUnconfirmed && logger != nil {
				logger.Warn("credential integrity: corruption finding not reproduced, no mark recorded",
					"auth_identity", result.AuthIdentityID, "enrollment", result.EnrollmentID)
			}
			outcomes = append(outcomes, operations.CredentialIntegrityOutcome{
				EnrollmentID:          result.EnrollmentID,
				NotChecked:            string(result.Skipped),
				CorruptionChecked:     result.CorruptionChecked,
				CorruptionUnconfirmed: result.CorruptionUnconfirmed,
			})
		}
		return outcomes, nil
	}
}

// credentialIdentityLabel is the label auth list shows for an identity.
func credentialIdentityLabel(identity domain.AuthIdentity) string {
	return maskAccountBinding(identity.AccountBinding)
}
