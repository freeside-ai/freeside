package ward

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

const (
	// MinSetupTokenBytes and MaxSetupTokenBytes bound an operator-entered
	// setup token. A real token is roughly a hundred bytes; the bounds refuse
	// an empty or truncated paste and a mistaken file, not a vendor format.
	MinSetupTokenBytes = 32
	MaxSetupTokenBytes = 4096
	// setupTokenPrefix is the prefix every Claude setup token carries. It
	// refuses the likeliest wrong paste, an API key (sk-ant-api...), whose
	// scope is not the inference-only one subscription_contained assumes.
	setupTokenPrefix = "sk-ant-oat" //nolint:gosec // G101: a public token-format prefix, not a credential

	defaultClaudeAuthEnrollmentLeaseDuration = 15 * time.Minute
	defaultClaudeAuthEnrollmentTeardown      = 30 * time.Second
	claudeCredentialVolumeSizeMB             = 2
	claudeCredentialVolumeTarget             = "/credential-volume" //nolint:gosec // G101: a mount path
	claudeCredentialStageDir                 = "/credential-input"  //nolint:gosec // G101: a mount path
	claudeCredentialReadyDir                 = "/credential-ready"  //nolint:gosec // G101: a mount path
)

// SetupToken is a Claude setup token on its way into a credential volume.
// Every implicit rendering (fmt verbs, JSON) yields a fixed placeholder, so a
// value that reaches a log line or an error by mistake shows nothing; the
// bytes leave only through the staged file the seeder copies.
type SetupToken struct{ value []byte }

// NewSetupToken validates and wraps an operator-entered token: printable
// ASCII with no whitespace, within the byte bounds, carrying the setup-token
// prefix. Errors are fixed strings that never echo the input.
func NewSetupToken(value []byte) (SetupToken, error) {
	if len(value) < MinSetupTokenBytes || len(value) > MaxSetupTokenBytes {
		return SetupToken{}, fmt.Errorf("setup token must be %d to %d bytes",
			MinSetupTokenBytes, MaxSetupTokenBytes)
	}
	for _, c := range value {
		if c < 0x21 || c > 0x7e {
			return SetupToken{}, errors.New("setup token must be one line of printable ASCII")
		}
	}
	if !bytes.HasPrefix(value, []byte(setupTokenPrefix)) {
		return SetupToken{}, errors.New("input is not a Claude setup token (claude setup-token prints one)")
	}
	return SetupToken{value: bytes.Clone(value)}, nil
}

const setupTokenPlaceholder = "[setup token]"

func (SetupToken) String() string   { return setupTokenPlaceholder }
func (SetupToken) GoString() string { return setupTokenPlaceholder }

// Format covers every fmt verb, so %x cannot hex-dump the bytes.
func (SetupToken) Format(f fmt.State, _ rune) {
	io.WriteString(f, setupTokenPlaceholder) //nolint:errcheck,gosec // fmt.State writes cannot be usefully handled
}

func (SetupToken) MarshalText() ([]byte, error) { return []byte(setupTokenPlaceholder), nil }

// ClaudeAuthEnrollmentStore is ward's persistence port for a Claude
// enrollment. Begin binds the identity (recording it when new), records the
// enrollment, and takes the lease bound to the enrollment's first generation
// in one transaction; AppendGeneration records the authored store under that
// live lease. There is no journal or recovery item: authoring a fresh volume
// spends nothing, so a failed attempt is simply retried.
type ClaudeAuthEnrollmentStore interface {
	Begin(
		ctx context.Context,
		identity domain.AuthIdentity,
		bootstrap EnrollmentBootstrap,
		holder domain.InvocationID,
		now, expiresAt time.Time,
	) (domain.AuthStoreMutationLease, error)
	AppendGeneration(
		ctx context.Context, generation domain.EnrollmentGeneration, now time.Time,
	) (domain.EnrollmentGeneration, error)
}

// ClaudeAuthEnrollmentConfig identifies one setup-token enrollment and the
// runtime that authors its credential volume.
type ClaudeAuthEnrollmentConfig struct {
	Token          SetupToken
	AuthIdentityID domain.AuthIdentityID
	EnrollmentID   domain.ClientEnrollmentID
	Route          string
	CostOwner      string
	// AccountBinding is the operator's attestation of the subscription the
	// token belongs to: the pinned CLI exposes no account identity to read.
	AccountBinding string
	// Volume names the credential volume to author. It must not exist yet;
	// replacing an enrolled volume is re-enroll.
	Volume        string
	Runtime       Runtime
	ExporterImage string
	// Authorize, when set, durably records each helper container's name
	// before it is created, as preflight's credential inspection does.
	Authorize RuntimeResourceAuthorizer

	Store           ClaudeAuthEnrollmentStore
	AuthStoreLeaser AuthStoreLeaser
	Now             func() time.Time

	LeaseDuration   time.Duration
	TeardownTimeout time.Duration
}

// ClaudeAuthEnrollmentResult carries only non-secret coordinates.
type ClaudeAuthEnrollmentResult struct {
	AuthIdentityID      domain.AuthIdentityID     `json:"auth_identity_id"`
	EnrollmentID        domain.ClientEnrollmentID `json:"enrollment_id"`
	AuthStoreVolume     string                    `json:"auth_store_volume"`
	StoreManifestDigest domain.Digest             `json:"store_manifest_digest"`
	Generation          int                       `json:"generation"`
}

// ErrCredentialVolumeExists refuses to author over a volume this command did
// not create: it may hold another identity's live credential.
var ErrCredentialVolumeExists = errors.New("credential volume already exists")

// EnrollClaudeSetupToken enrolls one Claude setup token: it records the
// identity and enrollment under the bound bootstrap lease, authors the
// credential volume as the single root-owned 0400 token file the setup_token
// manifest policy requires, proves that shape with the same observer
// preflight uses, and appends generation one. Any failure after the volume
// exists deletes it, so a failed enrollment leaves no store and no
// generation; the recorded enrollment stays and a retry reuses it.
//
// The token is not checked against the provider: the pinned CLI's auth status
// reports only local state, and a bad token fails closed at its first use.
func EnrollClaudeSetupToken(
	ctx context.Context, cfg ClaudeAuthEnrollmentConfig,
) (_ ClaudeAuthEnrollmentResult, retErr error) {
	if err := normalizeClaudeAuthEnrollmentConfig(&cfg); err != nil {
		return ClaudeAuthEnrollmentResult{}, err
	}
	digest := domain.Digest(contentaddr.Sum(cfg.Token.value))
	identity := domain.AuthIdentity{
		ID: cfg.AuthIdentityID, Provider: "claude", AccountBinding: cfg.AccountBinding,
		AuthStoreMutationLease: true, MaxParallelExecutions: 1, Enabled: true,
		CostOwner: cfg.CostOwner,
		// The interim facts the flag path reads today (-auth-identity with
		// -auth-volume), so the daemon runs this identity unchanged until
		// the #867 cutover moves them onto the enrollment.
		Interim: domain.InterimClientFacts{
			AuthStoreVolume: cfg.Volume, RefreshStrategy: domain.RefreshOnDemand,
		},
	}
	bootstrap := EnrollmentBootstrap{
		Enrollment: domain.ClientEnrollment{
			ID: cfg.EnrollmentID, AuthIdentityID: cfg.AuthIdentityID,
			HarnessClient: domain.HarnessClientClaudeCode, Route: cfg.Route,
			AuthMethod:     domain.AuthMethodSetupToken,
			CredentialMode: domain.CredentialSubscriptionContained,
			// No daemon refresh exists for a setup token; the operator
			// re-mints it outside the daemon.
			RefreshStrategy: domain.RefreshExternal, SupportsReadOnlyAuthSnapshot: true,
			AccountBinding: cfg.AccountBinding,
		},
		Binding: domain.LeaseGenerationBinding{
			EnrollmentID: cfg.EnrollmentID, AuthStoreVolume: cfg.Volume,
			StoreManifestDigest: digest,
		},
	}
	owner, err := newOwnershipLabel()
	if err != nil {
		return ClaudeAuthEnrollmentResult{}, errors.New("mint Claude auth enrollment holder")
	}
	holder := domain.InvocationID("claude-auth-enrollment-" + owner.Value)
	author := newCredentialVolumeAuthor(cfg, owner)
	// Refuse an existing volume before recording anything: the identity's
	// interim volume is a fixed binding, so recording it first would leave
	// an identity permanently bound to a volume it does not own. create
	// checks again under the lease.
	if err := author.refuseExisting(ctx); err != nil {
		return ClaudeAuthEnrollmentResult{}, err
	}
	now := cfg.Now()
	lease, err := cfg.Store.Begin(ctx, identity, bootstrap, holder, now, now.Add(cfg.LeaseDuration))
	if err != nil {
		return ClaudeAuthEnrollmentResult{}, fmt.Errorf("begin Claude auth enrollment: %w", err)
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.TeardownTimeout)
		defer cancel()
		releaseErr := cfg.AuthStoreLeaser.Release(releaseCtx, cfg.AuthIdentityID, holder, lease.Fence, cfg.Now())
		if releaseErr != nil && !errors.Is(releaseErr, ErrLeaseWindowEnded) {
			retErr = errors.Join(retErr, fmt.Errorf("release Claude auth enrollment lease: %w", releaseErr))
		}
	}()

	created, err := author.create(ctx)
	removeVolume := func(cause error) (ClaudeAuthEnrollmentResult, error) {
		if !created {
			return ClaudeAuthEnrollmentResult{}, cause
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.TeardownTimeout)
		defer cancel()
		if err := cfg.Runtime.DeleteVolume(cleanupCtx, cfg.Volume); err != nil {
			return ClaudeAuthEnrollmentResult{}, errors.Join(cause, fmt.Errorf(
				"delete credential volume %q after failed enrollment (delete it by hand, "+
					"then rerun with the same -auth-volume): %w", cfg.Volume, err))
		}
		return ClaudeAuthEnrollmentResult{}, cause
	}
	if err != nil {
		return removeVolume(err)
	}
	if err := author.seed(ctx, cfg.Token); err != nil {
		return removeVolume(err)
	}
	if err := InspectCredentialVolumeManifest(
		ctx, cfg.Runtime, cfg.ExporterImage, cfg.Volume, CredentialManifestSetupToken, cfg.Authorize,
	); err != nil {
		return removeVolume(fmt.Errorf("authored credential volume failed its manifest proof: %w", err))
	}
	generation, err := cfg.Store.AppendGeneration(ctx, domain.EnrollmentGeneration{
		EnrollmentID: cfg.EnrollmentID, AuthStoreVolume: cfg.Volume,
		StoreManifestDigest: digest, LeaseFence: lease.Fence,
		AccountBinding: cfg.AccountBinding, RecordedAt: cfg.Now(),
	}, cfg.Now())
	if err != nil {
		return removeVolume(fmt.Errorf("record Claude enrollment generation: %w", err))
	}
	return ClaudeAuthEnrollmentResult{
		AuthIdentityID: cfg.AuthIdentityID, EnrollmentID: cfg.EnrollmentID,
		AuthStoreVolume: cfg.Volume, StoreManifestDigest: digest,
		Generation: generation.Ordinal,
	}, nil
}

func normalizeClaudeAuthEnrollmentConfig(cfg *ClaudeAuthEnrollmentConfig) error {
	if cfg == nil || len(cfg.Token.value) == 0 || cfg.AuthIdentityID == "" ||
		cfg.EnrollmentID == "" || cfg.Route == "" || cfg.AccountBinding == "" ||
		cfg.Volume == "" || cfg.Runtime == nil || cfg.ExporterImage == "" ||
		cfg.Store == nil || cfg.AuthStoreLeaser == nil {
		return errors.New("claude auth enrollment configuration is incomplete")
	}
	if !cliSafe(cfg.Volume) {
		return errors.New("credential volume name is not CLI-safe")
	}
	// Check the runtime's name limit here, not at create: Begin records the
	// volume as the identity's fixed interim binding, so a name the runtime
	// then refuses would leave the identity bound to a volume that can never
	// exist.
	if err := validateRuntimeResourceName(cfg.Volume); err != nil {
		return fmt.Errorf("credential volume name: %w", err)
	}
	// The seeder and manifest observer run this image with the token's
	// volume mounted, so it is trusted compute under the same pin rule as
	// ward.Config, checked before anything is recorded.
	if !digestPinnedImagePattern.MatchString(cfg.ExporterImage) {
		return errors.New("exporter image is not digest-pinned")
	}
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().UTC() }
	}
	if cfg.LeaseDuration == 0 {
		cfg.LeaseDuration = defaultClaudeAuthEnrollmentLeaseDuration
	}
	if cfg.TeardownTimeout == 0 {
		cfg.TeardownTimeout = defaultClaudeAuthEnrollmentTeardown
	}
	if cfg.LeaseDuration <= 0 || cfg.TeardownTimeout <= 0 {
		return errors.New("claude auth enrollment durations are invalid")
	}
	if now := cfg.Now(); now.IsZero() || now.Location() != time.UTC {
		return errors.New("claude auth enrollment clock must return nonzero UTC instants")
	}
	return nil
}

// credentialVolumeAuthor creates one credential volume and seeds its token
// through the controlled rootfs-copy handshake the prompt volume uses: a
// networkless exporter-image container waits for a ready sentinel, copies
// the staged file onto the volume, and sets root-owned modes. The token
// never enters a command, environment, or label.
type credentialVolumeAuthor struct {
	backend *Backend
	cfg     ClaudeAuthEnrollmentConfig
	owner   Label
	runID   string
	seeder  string
}

func newCredentialVolumeAuthor(cfg ClaudeAuthEnrollmentConfig, owner Label) *credentialVolumeAuthor {
	wardCfg := (Config{ExporterImage: cfg.ExporterImage}).withDefaults()
	return &credentialVolumeAuthor{
		backend: &Backend{
			rt: cfg.Runtime, cfg: wardCfg, runtimeOps: newRuntimeOps(cfg.Runtime, wardCfg), initialized: true,
		},
		cfg: cfg, owner: owner,
		runID:  "enroll-" + owner.Value[:12],
		seeder: "freeside-enroll-credential-" + owner.Value[:12],
	}
}

// create makes the volume after proving no volume of that name exists. It
// reports whether it created one, so a failure deletes only what this
// command made. The volume carries no handoff labels: it outlives this
// command, and run-scoped recovery must never mistake it for a leftover.
func (a *credentialVolumeAuthor) create(ctx context.Context) (bool, error) {
	if err := a.refuseExisting(ctx); err != nil {
		return false, err
	}
	if err := a.cfg.Runtime.CreateVolume(ctx, a.cfg.Volume, claudeCredentialVolumeSizeMB, nil); err != nil {
		return false, fmt.Errorf("create credential volume %q: %w", a.cfg.Volume, err)
	}
	view, err := a.cfg.Runtime.InspectVolume(ctx, a.cfg.Volume)
	if err != nil || view.Name != a.cfg.Volume {
		return true, errors.Join(errors.New("created credential volume failed inspection"), err)
	}
	return true, nil
}

// refuseExisting fails when a volume of the configured name exists.
func (a *credentialVolumeAuthor) refuseExisting(ctx context.Context) error {
	volumes, err := a.cfg.Runtime.ListVolumes(ctx)
	if err != nil {
		return fmt.Errorf("list volumes: %w", err)
	}
	for _, volume := range volumes {
		if volume.Name == a.cfg.Volume {
			return fmt.Errorf("%q: %w", a.cfg.Volume, ErrCredentialVolumeExists)
		}
	}
	return nil
}

func (a *credentialVolumeAuthor) seed(ctx context.Context, token SetupToken) (retErr error) {
	stage, err := os.MkdirTemp("", "freeside-credential-")
	if err != nil {
		return errors.New("create credential staging directory")
	}
	defer func() {
		if err := os.RemoveAll(stage); err != nil {
			retErr = errors.Join(retErr, errors.New("remove credential staging directory"))
		}
	}()
	input, ready := filepath.Join(stage, "input"), filepath.Join(stage, "ready")
	for _, dir := range []string{input, ready} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			return errors.New("create credential staging directory")
		}
	}
	if err := os.WriteFile(filepath.Join(input, "token"), token.value, 0o600); err != nil {
		return errors.New("stage setup token")
	}
	if err := os.WriteFile(filepath.Join(ready, seedReadyFile), []byte("ready\n"), 0o600); err != nil {
		return errors.New("stage credential ready sentinel")
	}
	if a.cfg.Authorize != nil {
		if err := a.cfg.Authorize(ctx, RuntimeResourceNames{Containers: []string{a.seeder}}); err != nil {
			return err
		}
	}
	spec := buildCredentialSeederSpec(a.backend.cfg, a.runID, a.seeder, a.cfg.Volume, a.owner)
	var claim objectClaim
	if err := a.runSeeder(ctx, spec, input, ready, &claim); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), a.cfg.TeardownTimeout)
		defer cancel()
		return errors.Join(err, a.backend.runtimeOps.reapUnlistedContainer(cleanupCtx, a.seeder, claim, a.owner))
	}
	return nil
}

func (a *credentialVolumeAuthor) runSeeder(
	ctx context.Context, spec ContainerSpec, input, ready string, claim *objectClaim,
) error {
	b := a.backend
	claim.attempted = true
	if err := b.rt.CreateContainer(ctx, cloneContainerSpec(spec)); err != nil {
		return failf(CheckControlPlaneIsolation, "create credential seeder: %v", err)
	}
	claim.owned = true
	rep, err := b.rt.Inspect(ctx, spec.Name)
	if err != nil {
		return failf(CheckControlPlaneIsolation, "inspect credential seeder: %v", err)
	}
	if err := verifySeedRoleAllowlist(rep, spec, a.cfg.Volume, claudeCredentialVolumeTarget,
		CheckControlPlaneIsolation); err != nil {
		return err
	}
	claim.fingerprint, err = ownedFingerprint(rep.CreationDate, rep.Labels, rep.LabelsObserved, a.owner)
	if err != nil {
		return failf(CheckControlPlaneIsolation, "credential seeder ownership: %v", err)
	}
	if err := b.rt.StartContainer(ctx, spec.Name); err != nil {
		return failf(CheckControlPlaneIsolation, "start credential seeder: %v", err)
	}
	for _, copy := range []struct{ source, target string }{
		{input, claudeCredentialStageDir}, {ready, claudeCredentialReadyDir},
	} {
		copyCtx, cancel := context.WithTimeout(ctx, b.cfg.SeedTimeout)
		err := b.rt.CopyIntoContainer(copyCtx, spec.Name, copy.source, copy.target)
		cancel()
		if err != nil {
			// The runtime's error names paths, never file contents.
			return failf(CheckControlPlaneIsolation, "copy credential input: %v", err)
		}
	}
	if err := b.waitStopped(ctx, spec.Name, *claim, a.owner, b.cfg.SeedTimeout); err != nil {
		return failf(CheckControlPlaneIsolation, "credential seeder: %v", err)
	}
	if err := b.helperStopped(ctx, LaunchConformance, spec.Size, spec.Name); err != nil {
		return err
	}
	if err := b.rt.DeleteContainer(ctx, spec.Name); err != nil {
		return failf(CheckControlPlaneIsolation, "delete credential seeder: %v", err)
	}
	if err := b.verifyContainerAbsent(ctx, spec.Name, *claim, a.owner, CheckControlPlaneIsolation); err != nil {
		return err
	}
	*claim = objectClaim{}
	return nil
}

// buildCredentialSeederSpec is the seeder for a setup-token volume: it
// removes an empty lost+found (the manifest admits only the token), waits a
// bounded time for the host's ready sentinel, copies the staged token, and
// leaves it root-owned 0400 under a root-owned 0755 volume root.
func buildCredentialSeederSpec(cfg Config, runID, name, volume string, owner Label) ContainerSpec {
	root := shellQuote(claudeCredentialVolumeTarget)
	token := shellQuote(claudeCredentialVolumeTarget + "/token")
	script := stateSeederScript(claudeCredentialVolumeTarget, stateManifestEmpty) +
		"; n=0; while [ ! -f " + shellQuote(claudeCredentialReadyDir+"/"+seedReadyFile) +
		" ]; do n=$((n+1)); [ \"$n\" -le " + strconv.Itoa(seederScriptTicks(cfg)) + " ] || exit 1; sleep 1; done; " +
		"cp " + shellQuote(claudeCredentialStageDir+"/token") + " " + token + "; " +
		"chown 0:0 " + root + " " + token + "; chmod 0755 " + root + "; chmod 0400 " + token + "; sync"
	return ContainerSpec{
		Size: DefaultLaunchSize(LaunchConformance),
		Name: name, Image: cfg.ExporterImage, Command: []string{"sh", "-c", script},
		NetworkDisabled: true,
		Mounts:          []Mount{{Type: MountVolume, Source: volume, Target: claudeCredentialVolumeTarget}},
		Labels:          append(runLabels(runID), owner),
	}
}
