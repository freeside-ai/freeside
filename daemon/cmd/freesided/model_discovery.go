package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
	"github.com/freeside-ai/freeside/daemon/internal/wardstore"
)

// claudeModelDiscoverer runs one model discovery through an image under a
// setup token. Production binds ward.DiscoverClaudeModels to the daemon's
// runtime and rig authorizer; tests replace it.
type claudeModelDiscoverer func(
	ctx context.Context, image string, token ward.SetupToken,
) (ward.ClaudeModelReport, error)

func productionClaudeModelDiscoverer(
	runtime ward.Runtime, authorize ward.RuntimeResourceAuthorizer,
) claudeModelDiscoverer {
	return func(ctx context.Context, image string, token ward.SetupToken) (ward.ClaudeModelReport, error) {
		return ward.DiscoverClaudeModels(ctx, runtime, image, token, authorize)
	}
}

// modelCatalogRefresh refreshes the discovered model catalog of every
// enabled Claude setup-token enrollment (issue #2594).
type modelCatalogRefresh struct {
	store *store.Store
	// readToken reads an enrollment generation's setup token under the
	// identity's read hold: wardstore.CredentialReader.Read in production.
	readToken func(ctx context.Context, id domain.ClientEnrollmentID, ordinal int) (ward.SetupToken, error)
	discover  claudeModelDiscoverer
	image     string
	now       func() time.Time
	logger    *slog.Logger
}

// newModelCatalogRefresh binds the refresh to the Claude driver's runtime,
// images, and rig: tokens are read through the pinned exporter image under
// the identity's read hold, and discovery runs in the agent image. A nil
// discover is the production probe.
func newModelCatalogRefresh(
	st *store.Store, cfg claudeDriverConfig, containerBin string, discover claudeModelDiscoverer, logger *slog.Logger,
) modelCatalogRefresh {
	runtime := ward.NewCLIRuntime(containerBin)
	authorize := productionRigRuntimeAuthorizer(cfg.StateDir, cfg.RigTokenFile)
	if discover == nil {
		discover = productionClaudeModelDiscoverer(runtime, authorize)
	}
	utcNow := func() time.Time { return time.Now().UTC() }
	reader := wardstore.CredentialReader{
		Store: st, ReadSetupToken: productionSetupTokenReader(runtime, cfg.ExporterImage, authorize), Now: utcNow,
	}
	return modelCatalogRefresh{
		store: st, readToken: reader.Read, discover: discover, image: string(cfg.AgentImage),
		now: utcNow, logger: logger,
	}
}

type modelCatalogTarget struct {
	enrollment domain.ClientEnrollmentID
	generation int
}

// run discovers each enrollment in turn, one container at a time. A failure
// keeps the enrollment's last catalog and logs one warning: a missing
// catalog blocks a selection, never the daemon, so run returns nothing.
func (r modelCatalogRefresh) run(ctx context.Context) {
	targets, err := r.targets(ctx)
	if err != nil {
		r.logger.Warn("model discovery skipped: enrollments not read", "error", err)
		return
	}
	for _, target := range targets {
		if ctx.Err() != nil {
			return
		}
		catalog, err := r.discoverOne(ctx, target)
		if err != nil {
			r.logger.Warn("model discovery failed; the last catalog stands",
				"enrollment", target.enrollment, "generation", target.generation, "error", err)
			continue
		}
		r.logger.Info("model catalog discovered",
			"enrollment", target.enrollment, "harness_build", catalog.HarnessBuild, "models", len(catalog.Models))
	}
}

// targets lists every enabled identity's Claude setup-token enrollment with
// a current generation.
func (r modelCatalogRefresh) targets(ctx context.Context) ([]modelCatalogTarget, error) {
	var targets []modelCatalogTarget
	err := r.store.Read(ctx, func(tx *store.ReadTx) error {
		identities, err := tx.ListAuthIdentities(ctx)
		if err != nil {
			return err
		}
		for _, identity := range identities {
			if !identity.Enabled {
				continue
			}
			enrollments, err := tx.ListClientEnrollments(ctx, identity.ID)
			if err != nil {
				return err
			}
			for _, enrollment := range enrollments {
				if enrollment.HarnessClient != domain.HarnessClientClaudeCode ||
					enrollment.AuthMethod != domain.AuthMethodSetupToken {
					continue
				}
				generation, err := tx.CurrentEnrollmentGeneration(ctx, enrollment.ID)
				if errors.Is(err, store.ErrNotFound) {
					continue
				}
				if err != nil {
					return err
				}
				targets = append(targets, modelCatalogTarget{enrollment: enrollment.ID, generation: generation.Ordinal})
			}
		}
		return nil
	})
	return targets, err
}

func (r modelCatalogRefresh) discoverOne(ctx context.Context, target modelCatalogTarget) (domain.DiscoveredModelCatalog, error) {
	token, err := r.readToken(ctx, target.enrollment, target.generation)
	if err != nil {
		return domain.DiscoveredModelCatalog{}, err
	}
	return recordClaudeModelCatalog(ctx, r.store, r.discover, r.image, token, target.enrollment, r.now)
}

// recordClaudeModelCatalog discovers one enrollment's models under its token
// and stores the catalog, replacing the last one only on success.
func recordClaudeModelCatalog(
	ctx context.Context, st *store.Store, discover claudeModelDiscoverer, image string, token ward.SetupToken,
	enrollment domain.ClientEnrollmentID, now func() time.Time,
) (domain.DiscoveredModelCatalog, error) {
	report, err := discover(ctx, image, token)
	if err != nil {
		return domain.DiscoveredModelCatalog{}, err
	}
	catalog, err := report.Catalog(enrollment, now())
	if err != nil {
		return domain.DiscoveredModelCatalog{}, err
	}
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordDiscoveredModelCatalog(ctx, catalog)
	}); err != nil {
		return domain.DiscoveredModelCatalog{}, err
	}
	return catalog, nil
}
