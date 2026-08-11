package ward

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/export"
)

// AuthStoreLeaser is ward's seam to the store's per-identity auth-store
// mutation lease (plan §5.4). The gate acquires, re-verifies, and releases
// the lease itself, so the mutation window brackets the writer's actual
// runtime rather than a caller's estimate of it. The store-backed
// implementation is wired by the daemon (the ConformanceRecorder posture:
// domain and store must not import ward, so ward declares the interface it
// needs); the signatures mirror the store's lease methods one-for-one, so
// that adapter is a transaction wrapper and nothing else.
// ErrLeaseWindowEnded reports a release refused because the window is
// already over: released, expired past its bound, or taken over by a later
// holder with a bumped fence. AuthStoreLeaser implementations map the
// store's not-held and outside-window release refusals to this sentinel
// (errors.Is), so the gate can treat an already-ended window as nothing left
// to release — the case every crash recovery that outlives the window hits —
// while any other release failure stays a loud teardown problem.
var ErrLeaseWindowEnded = errors.New("auth store mutation lease window already ended")

type AuthStoreLeaser interface {
	// GetIdentity reconstructs the current trusted identity declaration. A
	// refresh transaction re-gates the lease requirement, refresh strategy,
	// and read-only snapshot support instead of trusting launch configuration.
	GetIdentity(ctx context.Context, id domain.AuthIdentityID) (domain.AuthIdentity, error)
	// AuthStoreVolume returns the immutable trusted volume binding for this
	// identity. Handoff compares it with the one writable credential mount
	// before opening a mutation window.
	AuthStoreVolume(ctx context.Context, id domain.AuthIdentityID) (string, error)
	// Acquire takes or converges on the identity's lease for holder, with
	// the given window. A live lease held by anyone else refuses; re-acquire
	// by the same holder returns the existing lease unchanged, without
	// extending it.
	Acquire(ctx context.Context, id domain.AuthIdentityID, holder domain.InvocationID,
		now, expiresAt time.Time) (domain.AuthStoreMutationLease, error)
	// Get reconstructs the identity's current lease row; liveness is the
	// caller's HeldAt question, never the row's claim.
	Get(ctx context.Context, id domain.AuthIdentityID) (domain.AuthStoreMutationLease, error)
	// Renew extends the exact live holder/fence window. It must refuse an
	// expired or superseded generation rather than reacquiring it.
	Renew(ctx context.Context, id domain.AuthIdentityID, holder domain.InvocationID,
		fence int64, now, expiresAt time.Time) (domain.AuthStoreMutationLease, error)
	// Release ends the lease the caller holds, identified by holder and
	// fence; a non-holder or a left-behind fence is refused.
	Release(ctx context.Context, id domain.AuthIdentityID, holder domain.InvocationID,
		fence int64, releasedAt time.Time) error
}

// OutputScanner is check 7's §5.4 scanning hook: it inspects the verified
// export directory and returns an error to block the handoff. Scanning
// policy (what counts as a leak) is gauntlet territory; the gate only
// guarantees the hook runs on every export and that any error fails closed,
// so no output reaches the gauntlet worker unscanned.
type OutputScanner interface {
	// Scan examines dir (the extracted, digest-verified handoff output). A
	// non-nil error fails the handoff's export_verification check.
	Scan(ctx context.Context, dir string) error
}

// ErrInvalidConfig is the class sentinel for a Config that cannot gate
// anything; New wraps it with the specific violation.
var ErrInvalidConfig = errors.New("invalid ward backend config")

// Config parameterizes the backend. The exporter image is the unit's seam
// with the gauntlet lane: the pinned image carries the trusted export helper
// (check 6), while everything the gate enforces about the exporter (checks 4,
// 5, 7) comes from here.
type Config struct {
	// AgentImage is the digest-pinned writer image whose realized topology
	// Suite.Full proves and whose liveness PreJob checks. It is optional for
	// one-shot library users that never restore or run production
	// conformance, but production composition must set it. When set,
	// NewSuite refuses a fixture for any other image.
	AgentImage string
	// ProviderEndpoints is the exact CONNECT authority allowlist for
	// provider_only writer egress. Each entry is a canonical lowercase
	// TLS DNS-name:port pair; no wildcard, IP literal, or implicit port is
	// allowed. Suite.Full requires matching TLS SNI and an HTTP response from
	// every entry, plus rejection of its alternate-Host fronting witness.
	ProviderEndpoints []string
	// EgressProxyTimeout bounds CONNECT request parsing and upstream dialing.
	// Defaults to 15 seconds.
	EgressProxyTimeout time.Duration
	// EgressDialContext is an injectable upstream dialer for conformance tests.
	// Nil uses net.Dialer; production callers leave it nil.
	EgressDialContext dialContextFunc
	// ExporterImage is the digest-pinned exporter image reference
	// ("repo/name@sha256:..."). A tag-only reference is refused: the exporter
	// is trusted compute, and trust binds to bytes, not a movable tag.
	ExporterImage string
	// ExporterCommand is the argv the exporter runs: the in-exporter
	// verification probes plus the trusted export helper. It must write the
	// check-5 proof file to ProofPath and the §5.6 manifest and blobs under
	// HandoffDir on the exporter's root filesystem.
	ExporterCommand []string
	// WorkspaceTarget is where the workspace volume mounts in both the agent
	// (read-write) and the exporter (read-only). Defaults to "/workspace",
	// the export helper's default input.
	WorkspaceTarget string
	// HandoffDir is where the exporter leaves the manifest and blobs on its
	// root filesystem. Defaults to "/handoff", the export helper's default
	// output.
	HandoffDir string
	// ProofPath is where the exporter writes the check-5 proof file on its
	// root filesystem. Defaults to "/handoff-proof.txt".
	ProofPath string
	// SeedRoot is the daemon-owned directory every seed source must resolve
	// under. It has no default: a backend that was never told where the
	// daemon's checkouts live cannot seed at all, which is the correct state
	// for a backend nobody configured for real work. An arbitrary host path is
	// never accepted, because a seed source is copied into a volume the writer
	// then holds read-write.
	SeedRoot string
	// ExportRoot is the daemon-owned durable directory for verified handoff
	// payloads after the exporter VM stops. Production must place it outside
	// volatile temporary storage so a completed journal remains recoverable
	// across a host reboot. Tests and one-shot callers default to os.TempDir.
	ExportRoot string
	// SeedStageDir is where the seeder receives the staged checkout on its own
	// root filesystem, before it copies the tree onto the workspace volume.
	// Defaults to "/seed".
	//
	// It must be disjoint from WorkspaceTarget, and that is a correctness
	// requirement rather than tidiness: on Apple container 1.1.0 a
	// `container copy` whose destination lies inside a mounted volume writes
	// nothing and still reports success, so staging into the mount would seed
	// an empty workspace with no error anywhere. validate() enforces the
	// disjointness; the observer catches the result if it ever slips.
	SeedStageDir string
	// SeedReadyDir is where the seeder receives the sentinel that says the
	// staged checkout is complete. Defaults to "/seed-ready".
	//
	// It is a second, separate copy rather than a marker inside the tree
	// because a directory copy is not atomic: a marker written as part of the
	// same copy could become visible before the rest of the tree, and the
	// seeder would copy a partial checkout onto the workspace.
	SeedReadyDir string
	// BaseProofPath is where the observer writes the seeded-base proof file on
	// its root filesystem. Defaults to "/handoff-base.txt".
	BaseProofPath string
	// CredProofPath is where the credential-store observer writes its digest
	// proof file on its own root filesystem. Defaults to "/handoff-cred.txt".
	// It is distinct from BaseProofPath so the two observer roles stay
	// distinguishable by the proof they produce.
	CredProofPath string
	// SeedTimeout bounds each seeding runtime call and the wait for the seeder
	// and observer to reach observed state stopped. Defaults to 5 minutes.
	SeedTimeout time.Duration
	// MaxSeedBytes and MaxSeedEntries cap the host checkout the gate is willing
	// to stage. The seed lands twice (the seeder's root filesystem, then the
	// workspace volume), so an unbounded source would exhaust both. Default to
	// 512 MiB and 100,000 entries.
	MaxSeedBytes   int64
	MaxSeedEntries int
	// WriterStopTimeout bounds the wait for the agent container to reach
	// observed state stopped. Defaults to 10 minutes.
	WriterStopTimeout time.Duration
	// ExporterTimeout bounds the wait for the exporter container to reach
	// observed state stopped. Defaults to 5 minutes.
	ExporterTimeout time.Duration
	// TeardownTimeout bounds teardown, which runs detached from the caller's
	// cancellation. Without its own deadline a wedged runtime call could keep
	// a cancelled Handoff from ever returning. Defaults to 2 minutes.
	TeardownTimeout time.Duration
	// HandoffTimeout is the overall wall-clock budget for one Handoff. The
	// per-operation timeouts above only start once their own runtime call
	// returns, so a runtime that wedges inside a side-effecting call (for
	// example after launching the credential-bearing agent VM but before
	// StartContainer returns) would otherwise leave the gate blocked and the VM
	// live indefinitely. This bounds every side-effecting call from one place;
	// teardown detaches from it (context.WithoutCancel) and reaps what it
	// interrupts. Defaults to WriterStopTimeout + ExporterTimeout +
	// 6*SeedTimeout + 15 minutes: the four seed operations plus the two
	// credential observers a leased run adds, each riding the SeedTimeout
	// per-operation bound.
	HandoffTimeout time.Duration
	// PollInterval is the state-poll spacing. Defaults to 500ms.
	PollInterval time.Duration
	// MaxExportBytes caps the bytes extracted from the exported archive's
	// handoff output (a tar-bomb guard on the daemon host; the export
	// helper's own blob limits bound the honest case well below this).
	// Defaults to 2 GiB.
	MaxExportBytes int64
	// MaxArchiveBytes caps the stopped container's full rootfs tar while the
	// Runtime streams it onto the host. It is distinct from MaxExportBytes
	// because the pinned exporter's base image is present in the archive too.
	// Defaults to 4 GiB.
	MaxArchiveBytes int64
	// MaxExportEntries caps filesystem objects under HandoffDir before any
	// archive-derived path is created on the host. Defaults to 10,000.
	MaxExportEntries int
	// MaxManifestBytes caps the manifest.json read into the daemon heap
	// during verification. The per-file extraction budget (MaxExportBytes)
	// alone lets a hostile manifest grow to the full export budget, so an
	// unbounded read would load it all before JSON validation can reject it;
	// blobless entries (symlinks, submodules) evade MaxExportEntries yet each
	// still occupies a manifest record, so the manifest is not otherwise
	// bounded. Sits far above any honest §5.6 manifest and far below
	// MaxExportBytes. Defaults to 64 MiB.
	MaxManifestBytes int64
	// Scanner is the required check-7 scanning hook.
	Scanner OutputScanner
	// AuthStoreLeaser acquires and releases §5.4 auth-store mutation leases.
	// It is required only by handoffs whose spec carries an AuthStoreLease
	// claim; a leased handoff under a nil leaser fails closed.
	AuthStoreLeaser AuthStoreLeaser
	// Journal durably records each handoff for restart-safe recovery. Nil
	// preserves the one-shot semantics exactly: nothing is recorded, a
	// crash strands objects as before, and Recover has nothing to work
	// from. Requiring a journal is the caller's operating-mode policy, not
	// the gate's.
	Journal HandoffJournal
	// Now supplies the current instant for lease windows and release stamps;
	// tests inject a fixed clock. Nil defaults to time.Now.
	Now func() time.Time
	// Sleep waits between state polls; tests inject a recording stub. Nil
	// defaults to a context-aware real sleep.
	Sleep func(context.Context, time.Duration) error
}

// withDefaults returns cfg with unset optional fields filled.
func (cfg Config) withDefaults() Config {
	if cfg.ExportRoot == "" {
		cfg.ExportRoot = filepath.Clean(os.TempDir())
	}
	if cfg.WorkspaceTarget == "" {
		cfg.WorkspaceTarget = export.HelperWorkspaceDir
	}
	if cfg.HandoffDir == "" {
		cfg.HandoffDir = export.HelperHandoffDir
	}
	if cfg.ProofPath == "" {
		cfg.ProofPath = "/handoff-proof.txt"
	}
	if cfg.SeedStageDir == "" {
		cfg.SeedStageDir = "/seed"
	}
	if cfg.SeedReadyDir == "" {
		cfg.SeedReadyDir = "/seed-ready"
	}
	if cfg.BaseProofPath == "" {
		cfg.BaseProofPath = "/handoff-base.txt"
	}
	if cfg.CredProofPath == "" {
		cfg.CredProofPath = "/handoff-cred.txt"
	}
	if cfg.SeedTimeout == 0 {
		cfg.SeedTimeout = 5 * time.Minute
	}
	if cfg.EgressProxyTimeout == 0 {
		cfg.EgressProxyTimeout = 15 * time.Second
	}
	if cfg.MaxSeedBytes == 0 {
		cfg.MaxSeedBytes = 512 << 20
	}
	if cfg.MaxSeedEntries == 0 {
		cfg.MaxSeedEntries = 100_000
	}
	if cfg.WriterStopTimeout == 0 {
		cfg.WriterStopTimeout = 10 * time.Minute
	}
	if cfg.ExporterTimeout == 0 {
		cfg.ExporterTimeout = 5 * time.Minute
	}
	if cfg.TeardownTimeout == 0 {
		cfg.TeardownTimeout = 2 * time.Minute
	}
	if cfg.HandoffTimeout == 0 {
		// A wedge backstop, not an SLA, so size it generously above the
		// bounded operations it contains plus enough slack to stream,
		// extract, hash, and scan a near-max multi-GiB export without the
		// overall budget firing on a legitimately slow run. Operators facing
		// larger scans can raise it.
		//
		// Seeding contributes four SeedTimeout-bounded operations, not two:
		// the two staged copies as well as the seeder's and observer's waits.
		// Counting only the waits would quietly spend the export slack.
		cfg.HandoffTimeout = cfg.WriterStopTimeout + cfg.ExporterTimeout + 6*cfg.SeedTimeout + 15*time.Minute
	}
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 500 * time.Millisecond
	}
	if cfg.MaxExportBytes == 0 {
		cfg.MaxExportBytes = 2 << 30
	}
	if cfg.MaxArchiveBytes == 0 {
		cfg.MaxArchiveBytes = 4 << 30
	}
	if cfg.MaxExportEntries == 0 {
		cfg.MaxExportEntries = 10_000
	}
	if cfg.MaxManifestBytes == 0 {
		cfg.MaxManifestBytes = export.DefaultMaxCommitPlanBytes
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Sleep == nil {
		cfg.Sleep = sleepContext
	}
	return cfg
}

// validate reports the first violation in a defaults-applied Config.
func (cfg Config) validate() error {
	switch {
	case len(cfg.ProviderEndpoints) == 0:
		return fmt.Errorf("%w: ProviderEndpoints is required for provider_only egress", ErrInvalidConfig)
	case cfg.AgentImage != "" && !digestPinnedImagePattern.MatchString(cfg.AgentImage):
		return fmt.Errorf("%w: AgentImage %q is not digest-pinned", ErrInvalidConfig, cfg.AgentImage)
	case cfg.EgressProxyTimeout < 0:
		return fmt.Errorf("%w: EgressProxyTimeout %s is negative", ErrInvalidConfig, cfg.EgressProxyTimeout)
	case cfg.ExporterImage == "":
		return fmt.Errorf("%w: ExporterImage is required", ErrInvalidConfig)
	case !digestPinnedImagePattern.MatchString(cfg.ExporterImage):
		return fmt.Errorf("%w: ExporterImage %q is not digest-pinned", ErrInvalidConfig, cfg.ExporterImage)
	case len(cfg.ExporterCommand) == 0:
		return fmt.Errorf("%w: ExporterCommand is required", ErrInvalidConfig)
	case !cleanAbs(cfg.WorkspaceTarget):
		return fmt.Errorf("%w: WorkspaceTarget %q is not a clean absolute non-root path", ErrInvalidConfig, cfg.WorkspaceTarget)
	case !cleanAbs(cfg.HandoffDir):
		return fmt.Errorf("%w: HandoffDir %q is not a clean absolute non-root path", ErrInvalidConfig, cfg.HandoffDir)
	case !cleanAbs(cfg.ProofPath):
		return fmt.Errorf("%w: ProofPath %q is not a clean absolute non-root path", ErrInvalidConfig, cfg.ProofPath)
	case !cleanAbs(cfg.SeedStageDir):
		return fmt.Errorf("%w: SeedStageDir %q is not a clean absolute non-root path", ErrInvalidConfig, cfg.SeedStageDir)
	case !cleanAbs(cfg.SeedReadyDir):
		return fmt.Errorf("%w: SeedReadyDir %q is not a clean absolute non-root path", ErrInvalidConfig, cfg.SeedReadyDir)
	case !cleanAbs(cfg.BaseProofPath):
		return fmt.Errorf("%w: BaseProofPath %q is not a clean absolute non-root path", ErrInvalidConfig, cfg.BaseProofPath)
	case !cleanAbs(cfg.CredProofPath):
		return fmt.Errorf("%w: CredProofPath %q is not a clean absolute non-root path", ErrInvalidConfig, cfg.CredProofPath)
	case !copyPathSafe(cfg.SeedStageDir):
		return fmt.Errorf("%w: SeedStageDir %q carries a container-reference delimiter", ErrInvalidConfig, cfg.SeedStageDir)
	case !copyPathSafe(cfg.SeedReadyDir):
		return fmt.Errorf("%w: SeedReadyDir %q carries a container-reference delimiter", ErrInvalidConfig, cfg.SeedReadyDir)
	case cfg.SeedRoot != "" && !cleanAbs(cfg.SeedRoot):
		return fmt.Errorf("%w: SeedRoot %q is not a clean absolute non-root path", ErrInvalidConfig, cfg.SeedRoot)
	case !cleanAbs(cfg.ExportRoot):
		return fmt.Errorf("%w: ExportRoot %q is not a clean absolute non-root path", ErrInvalidConfig, cfg.ExportRoot)
	case cfg.SeedTimeout < 0:
		return fmt.Errorf("%w: SeedTimeout %s is negative", ErrInvalidConfig, cfg.SeedTimeout)
	case cfg.MaxSeedBytes < 0:
		return fmt.Errorf("%w: MaxSeedBytes %d is negative", ErrInvalidConfig, cfg.MaxSeedBytes)
	case cfg.MaxSeedEntries < 0:
		return fmt.Errorf("%w: MaxSeedEntries %d is negative", ErrInvalidConfig, cfg.MaxSeedEntries)
	case cfg.MaxExportBytes < 0:
		return fmt.Errorf("%w: MaxExportBytes %d is negative", ErrInvalidConfig, cfg.MaxExportBytes)
	case cfg.MaxArchiveBytes < 0:
		return fmt.Errorf("%w: MaxArchiveBytes %d is negative", ErrInvalidConfig, cfg.MaxArchiveBytes)
	case cfg.MaxExportEntries < 0:
		return fmt.Errorf("%w: MaxExportEntries %d is negative", ErrInvalidConfig, cfg.MaxExportEntries)
	case cfg.MaxManifestBytes < 0:
		return fmt.Errorf("%w: MaxManifestBytes %d is negative", ErrInvalidConfig, cfg.MaxManifestBytes)
	case cfg.WriterStopTimeout < 0:
		return fmt.Errorf("%w: WriterStopTimeout %s is negative", ErrInvalidConfig, cfg.WriterStopTimeout)
	case cfg.ExporterTimeout < 0:
		return fmt.Errorf("%w: ExporterTimeout %s is negative", ErrInvalidConfig, cfg.ExporterTimeout)
	case cfg.PollInterval < 0:
		return fmt.Errorf("%w: PollInterval %s is negative", ErrInvalidConfig, cfg.PollInterval)
	case cfg.TeardownTimeout < 0:
		return fmt.Errorf("%w: TeardownTimeout %s is negative", ErrInvalidConfig, cfg.TeardownTimeout)
	case cfg.HandoffTimeout < 0:
		return fmt.Errorf("%w: HandoffTimeout %s is negative", ErrInvalidConfig, cfg.HandoffTimeout)
	case cfg.Scanner == nil:
		return fmt.Errorf("%w: Scanner is required (check 7 scans every export)", ErrInvalidConfig)
	}
	seenEndpoints := make(map[string]struct{}, len(cfg.ProviderEndpoints))
	for _, endpoint := range cfg.ProviderEndpoints {
		canonical, err := canonicalAuthority(endpoint)
		if err != nil || canonical != endpoint {
			return fmt.Errorf("%w: ProviderEndpoints contains a non-canonical authority", ErrInvalidConfig)
		}
		if _, duplicate := seenEndpoints[endpoint]; duplicate {
			return fmt.Errorf("%w: ProviderEndpoints contains a duplicate authority", ErrInvalidConfig)
		}
		seenEndpoints[endpoint] = struct{}{}
	}
	// Every path the gate collects evidence from is on some container's own
	// root filesystem and must be disjoint from the workspace, which the agent
	// writes and which is mounted into the exporter and observer. Were
	// ProofPath, HandoffDir, or BaseProofPath nested in the workspace,
	// agent-authored files could shadow a container's own output and forge
	// check 5's proof, the seeded-base proof, or a self-consistent manifest.
	//
	// SeedStageDir and SeedReadyDir are in the same set for a second reason:
	// on Apple container 1.1.0 a `container copy` into a mounted volume writes
	// nothing and still exits 0, so a stage directory nested in the workspace
	// would silently seed nothing. The gate leans on all of this, so it is
	// asserted here rather than left to depend on the default values.
	if err := disjointPaths(cfg.WorkspaceTarget, cfg.HandoffDir, cfg.ProofPath,
		cfg.SeedStageDir, cfg.SeedReadyDir, cfg.BaseProofPath, cfg.CredProofPath); err != nil {
		return err
	}
	// WorkspaceTarget is phrased into the exporter's --mount value; a comma or
	// control character there would let the CLI parse an injected mount option.
	if !cliSafe(cfg.WorkspaceTarget) {
		return fmt.Errorf("%w: WorkspaceTarget %q carries a CLI mount-option delimiter", ErrInvalidConfig, cfg.WorkspaceTarget)
	}
	return nil
}

// disjointPaths reports the first pair among the given absolute paths where
// one equals or nests under another; the proof, handoff, and workspace paths
// must all be mutually exclusive subtrees.
func disjointPaths(paths ...string) error {
	for i := range paths {
		for j := range paths {
			if i == j {
				continue
			}
			if paths[i] == paths[j] || strings.HasPrefix(paths[j], paths[i]+"/") {
				return fmt.Errorf("%w: path %q nests under %q; workspace, handoff, proof, and seed paths must be disjoint",
					ErrInvalidConfig, paths[j], paths[i])
			}
		}
	}
	return nil
}

// sleepContext sleeps for d or until ctx is done, whichever comes first.
func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
