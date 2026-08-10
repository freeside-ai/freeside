package claude

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec/stage"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// Compatibility sentinels preserve the existing Claude driver contract while
// the durable implementation lives in the provider-neutral stage package.
var (
	ErrUnsupportedStart  = stage.ErrUnsupportedStart
	ErrDriverClosed      = stage.ErrDriverClosed
	ErrSeedRetryable     = stage.ErrSeedRetryable
	ErrSeedRefused       = stage.ErrSeedRefused
	ErrRecoveryRetryable = stage.ErrRecoveryRetryable
)

// Driver is the Claude-configured provider-neutral stage driver.
type Driver = stage.Driver

// ExecutionReplay is the authenticated replay returned by the stage driver.
type ExecutionReplay = stage.ExecutionReplay

// AuthStoreVolumes resolves the trusted identity-to-credential-volume
// binding used by the Claude provider's handoff specification.
type AuthStoreVolumes interface {
	AuthStoreVolume(context.Context, domain.AuthIdentityID) (string, error)
}

// Config preserves the Claude driver's existing construction surface.
type Config struct {
	Lifetime    context.Context
	Dir         string
	SeedRoot    string
	ExportRoot  string
	Gate        stage.Gate
	Seeder      stage.Seeder
	Exports     stage.ExportRecorder
	Outcomes    stage.OutcomeRecorder
	Authority   stage.AdmissionAuthority
	Artifacts   stage.Artifacts
	Volumes     AuthStoreVolumes
	PreJob      func(context.Context, domain.InvocationID) error
	Import      importer.Options
	Preparation []string
	Now         func() time.Time
	Logger      *slog.Logger
}

// New constructs the Claude provider over the shared durable stage machine.
func New(cfg Config) (*Driver, error) {
	var providerConfigError error
	if cfg.Volumes == nil {
		providerConfigError = errors.New("nil auth store volumes")
	}
	return stage.New(stage.Config{
		ErrorPrefix: "claude driver",
		DisplayName: "Claude",
		Provider:    claudeProvider{volumes: cfg.Volumes},
		CredentialMount: stage.CredentialMountPolicy{
			Target: credentialMountTarget, Manifest: ward.CredentialManifestSetupToken,
		},
		ProviderConfigError: providerConfigError,
		Lifetime:            cfg.Lifetime,
		Dir:                 cfg.Dir,
		SeedRoot:            cfg.SeedRoot,
		ExportRoot:          cfg.ExportRoot,
		Gate:                cfg.Gate,
		Seeder:              cfg.Seeder,
		Exports:             cfg.Exports,
		Outcomes:            cfg.Outcomes,
		Authority:           cfg.Authority,
		Artifacts:           cfg.Artifacts,
		PreJob:              cfg.PreJob,
		Import:              cfg.Import,
		Preparation:         cfg.Preparation,
		Now:                 cfg.Now,
		Logger:              cfg.Logger,
	})
}
