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
) (ward.ModelReport, error)

func productionClaudeModelDiscoverer(
	runtime ward.Runtime, authorize ward.RuntimeResourceAuthorizer,
) claudeModelDiscoverer {
	return func(ctx context.Context, image string, token ward.SetupToken) (ward.ModelReport, error) {
		return ward.DiscoverClaudeModels(ctx, runtime, image, token, authorize)
	}
}

// codexModelDiscoverer runs one model discovery through an image under a
// Codex probe credential. Production binds ward.DiscoverCodexModels to the
// daemon's runtime and rig authorizer; tests replace it.
type codexModelDiscoverer func(
	ctx context.Context, image string, credential ward.LineupProbeCredential,
) (ward.ModelReport, error)

func productionCodexModelDiscoverer(
	runtime ward.Runtime, authorize ward.RuntimeResourceAuthorizer,
) codexModelDiscoverer {
	return func(ctx context.Context, image string, credential ward.LineupProbeCredential) (ward.ModelReport, error) {
		return ward.DiscoverCodexModels(ctx, runtime, image, credential, authorize)
	}
}

// modelCatalogRefresh refreshes the discovered model catalog of every
// enabled Claude setup-token enrollment (issue #2594) and, when the reviewer
// is Codex, every enabled Codex CLI OAuth enrollment (issue #2595).
type modelCatalogRefresh struct {
	store *store.Store
	// readToken reads an enrollment generation's setup token under the
	// identity's read hold: wardstore.CredentialReader.Read in production.
	readToken func(ctx context.Context, id domain.ClientEnrollmentID, ordinal int) (ward.SetupToken, error)
	discover  claudeModelDiscoverer
	image     string
	// codexImage is the review image Codex discovery runs in; empty skips
	// Codex discovery. readCodex reads an enrollment's current probe
	// credential under the identity's read hold:
	// wardstore.CodexProbeCredentialReader.Read in production.
	codexImage    string
	readCodex     func(ctx context.Context, id domain.ClientEnrollmentID) (ward.LineupProbeCredential, error)
	discoverCodex codexModelDiscoverer
	now           func() time.Time
	logger        *slog.Logger
}

// newModelCatalogRefresh binds the refresh to the Claude driver's runtime,
// images, and rig: tokens are read through the pinned exporter image under
// the identity's read hold, and discovery runs in the agent image. Codex
// credentials are read from the review input root under the same hold, and
// Codex discovery runs in the review image, only when the review auth mode
// is a Codex one: the driver composed, so resolveReviewSelection admitted
// that mode for the reviewer's harness. A nil discoverer is the production
// probe.
func newModelCatalogRefresh(
	st *store.Store, cfg claudeDriverConfig, containerBin string,
	discover claudeModelDiscoverer, discoverCodex codexModelDiscoverer, logger *slog.Logger,
) modelCatalogRefresh {
	runtime := ward.NewCLIRuntime(containerBin)
	authorize := productionRigRuntimeAuthorizer(cfg.StateDir, cfg.RigTokenFile)
	if discover == nil {
		discover = productionClaudeModelDiscoverer(runtime, authorize)
	}
	if discoverCodex == nil {
		discoverCodex = productionCodexModelDiscoverer(runtime, authorize)
	}
	utcNow := func() time.Time { return time.Now().UTC() }
	reader := wardstore.CredentialReader{
		Store: st, ReadSetupToken: productionSetupTokenReader(runtime, cfg.ExporterImage, authorize), Now: utcNow,
	}
	refresh := modelCatalogRefresh{
		store: st, readToken: reader.Read, discover: discover, image: string(cfg.AgentImage),
		discoverCodex: discoverCodex, now: utcNow, logger: logger,
	}
	if codexReviewerConfigured(cfg) {
		refresh.codexImage = cfg.ReviewImage
		refresh.readCodex = wardstore.CodexProbeCredentialReader{Store: st, InputRoot: cfg.ReviewInputRoot, Now: utcNow}.Read
	}
	return refresh
}

// codexReviewerConfigured reports whether the daemon's reviewer is a Codex
// subscription reviewer: an unattended review whose auth mode only the Codex
// provider takes. Its input root holds the OAuth stores discovery reads; an
// API-key reviewer's holds a key file, so it starts no Codex discovery.
func codexReviewerConfigured(cfg claudeDriverConfig) bool {
	return cfg.OperatingMode == domain.ModeUnattended && cfg.ReviewImage != "" && cfg.ReviewInputRoot != "" &&
		cfg.ReviewAuthMode == ward.CodexAuthSubscription
}

type modelCatalogTarget struct {
	harness    domain.HarnessClientKind
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
	if r.codexImage == "" {
		r.logger.Info("Codex model discovery skipped: no Codex reviewer is configured")
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
// a current generation, then, when Codex discovery runs, its Codex CLI OAuth
// enrollments with one.
func (r modelCatalogRefresh) targets(ctx context.Context) ([]modelCatalogTarget, error) {
	var claude, codex []modelCatalogTarget
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
				claudeTarget := enrollment.HarnessClient == domain.HarnessClientClaudeCode &&
					enrollment.AuthMethod == domain.AuthMethodSetupToken
				codexTarget := r.codexImage != "" && enrollment.HarnessClient == domain.HarnessClientCodexCLI &&
					enrollment.AuthMethod == domain.AuthMethodOAuth
				if !claudeTarget && !codexTarget {
					continue
				}
				generation, err := tx.CurrentEnrollmentGeneration(ctx, enrollment.ID)
				if errors.Is(err, store.ErrNotFound) {
					continue
				}
				if err != nil {
					return err
				}
				target := modelCatalogTarget{
					harness: enrollment.HarnessClient, enrollment: enrollment.ID, generation: generation.Ordinal,
				}
				if claudeTarget {
					claude = append(claude, target)
				} else {
					codex = append(codex, target)
				}
			}
		}
		return nil
	})
	return append(claude, codex...), err
}

func (r modelCatalogRefresh) discoverOne(ctx context.Context, target modelCatalogTarget) (domain.DiscoveredModelCatalog, error) {
	if target.harness == domain.HarnessClientCodexCLI {
		// The reader resolves the current generation itself, under its hold.
		credential, err := r.readCodex(ctx, target.enrollment)
		if err != nil {
			return domain.DiscoveredModelCatalog{}, err
		}
		report, err := r.discoverCodex(ctx, r.codexImage, credential)
		if err != nil {
			return domain.DiscoveredModelCatalog{}, err
		}
		return recordModelCatalog(ctx, r.store, report, target.enrollment, r.now)
	}
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
	return recordModelCatalog(ctx, st, report, enrollment, now)
}

// recordModelCatalog stores one enrollment's discovered catalog, replacing
// the last one.
func recordModelCatalog(
	ctx context.Context, st *store.Store, report ward.ModelReport, enrollment domain.ClientEnrollmentID,
	now func() time.Time,
) (domain.DiscoveredModelCatalog, error) {
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
