package ward

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/export"
)

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
	// interrupts. Defaults to WriterStopTimeout + ExporterTimeout + 5 minutes.
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
	// Sleep waits between state polls; tests inject a recording stub. Nil
	// defaults to a context-aware real sleep.
	Sleep func(context.Context, time.Duration) error
}

// withDefaults returns cfg with unset optional fields filled.
func (cfg Config) withDefaults() Config {
	if cfg.WorkspaceTarget == "" {
		cfg.WorkspaceTarget = export.HelperWorkspaceDir
	}
	if cfg.HandoffDir == "" {
		cfg.HandoffDir = export.HelperHandoffDir
	}
	if cfg.ProofPath == "" {
		cfg.ProofPath = "/handoff-proof.txt"
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
		// A wedge backstop, not an SLA, so size it generously above the two
		// observed-state waits it contains plus enough slack to stream,
		// extract, hash, and scan a near-max multi-GiB export without the
		// overall budget firing on a legitimately slow run. Operators facing
		// larger scans can raise it.
		cfg.HandoffTimeout = cfg.WriterStopTimeout + cfg.ExporterTimeout + 15*time.Minute
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
		cfg.MaxManifestBytes = 64 << 20
	}
	if cfg.Sleep == nil {
		cfg.Sleep = sleepContext
	}
	return cfg
}

// validate reports the first violation in a defaults-applied Config.
func (cfg Config) validate() error {
	switch {
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
	// The proof and handoff output are collected from the exporter's own root
	// filesystem and must be disjoint from the workspace, which the agent
	// writes and which is mounted (read-only) in the exporter. Were ProofPath
	// or HandoffDir nested in the workspace, agent-authored files could shadow
	// the exporter's own output and forge check 5's proof or supply a
	// self-consistent manifest; the gate leans on this disjointness, so it is
	// asserted here rather than left to depend on the default values.
	if err := disjointPaths(cfg.WorkspaceTarget, cfg.HandoffDir, cfg.ProofPath); err != nil {
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
				return fmt.Errorf("%w: path %q nests under %q; workspace, handoff, and proof paths must be disjoint",
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
