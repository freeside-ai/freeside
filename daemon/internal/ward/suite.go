package ward

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/export"
)

// Suite runs the workspace-handoff backend's conformance checks at the plan
// §5.7 cadence points (startup, configuration change, and the doctor
// schedule for the full suite; a lightweight probe before each unattended
// job) without a real work item. Doctor scheduling is a downstream
// operations-unit concern; this package supplies the invocable library.
//
// The suite fails closed: every method returns nil (conformant) or a
// non-nil error (not conformant, so the caller must not proceed). A failed
// contract check or negative probe returns a *ConformanceFailure naming the
// failed check (the §3.1 non-waivable class; docs/plan.md). Failed runner
// conformance including the handoff gate never auto-promotes or offers a
// bypass, so absence of a nil result gates unattended operation.
//
// Full exercises the whole contract (spike checks 1-5 and 7, run together by
// one synthetic handoff) plus two of the three negative probes: the
// read-write-attach exclusion and credential-marker containment. The third
// probe (same-VM guest-unmount refutation) needs a CAP_SYS_ADMIN guest
// process, which the gate's ContainerSpec vocabulary deliberately cannot
// express (that minimality is checks 1-2's isolation argument); it is a
// permanent reference-runtime test that drives the runtime CLI directly, not
// a Full member. See the decision note.
type Suite struct {
	b  *Backend
	fx SuiteFixture
	// defaultWriter records that NewSuite generated the writer command (the
	// caller supplied none). Only then does Full know the export's expected
	// run-unique sentinel and enforce it; a caller-supplied command owns its
	// own output, so containment falls back to the non-empty-manifest floor.
	defaultWriter bool
}

// SuiteFixture parameterizes the synthetic handoff and probes with inert,
// digest-pinned fixture inputs. The daemon supplies its pinned project base
// image and a fixed benign writer; the reference-runtime test supplies the
// spike's alpine image and payload.
type SuiteFixture struct {
	// AgentImage is the digest-pinned benign writer image the synthetic
	// handoff and the probes run. Trust binds to bytes: a tag is refused.
	AgentImage string
	// AgentCommand is the benign writer payload. Empty defaults to a shell
	// payload that verifies the token equals the seeded credential marker (so
	// it aborts under `set -eu` unless the real credential mount was realized)
	// and only then writes this run's writer sentinel plus deterministic
	// workspace files, giving the exporter content and letting Full prove this
	// run's writer produced the export. A caller-supplied command owns its own
	// output: it must still read the credential and write workspace content,
	// but Full applies only the non-empty-manifest floor to it, not the
	// run-unique sentinel check the default writer earns.
	AgentCommand []string
	// CredentialTarget is where the fake credential volume mounts, read-only,
	// in the writer and the audit container. Defaults to "/credentials".
	CredentialTarget string
	// CredentialMarker is the inert fake credential seeded into the probe's
	// credential volume. The suite proves it is contained: absent from the
	// export, present in the detached credential volume. Must be non-empty and
	// distinctive.
	CredentialMarker string
	// WorkspaceSizeMB and CredentialSizeMB size the synthetic volumes.
	// Default to 64 and 8.
	WorkspaceSizeMB  int64
	CredentialSizeMB int64
	// RunID names the synthetic run's objects; it must match the handoff run
	// ID pattern and be unique among live runs (the caller makes it unique per
	// invocation, e.g. from a timestamp). Probe objects derive their names
	// from it.
	RunID string
}

const (
	// credentialTokenFile is the seeded marker's path within CredentialTarget.
	credentialTokenFile = "token"
	// writerResultPath is the workspace-relative file the default writer writes
	// its run-unique sentinel to; Full matches this manifest entry's digest to
	// prove this run's writer produced the export.
	writerResultPath = "result.txt"
	// workspaceStatePayload is the fixed content the default writer puts in a
	// nested workspace file (durable-directory-tree coverage). Named so the
	// marker-collision guard can reject a marker that is a substring of it.
	workspaceStatePayload = "durable-workspace"
	// workspaceStateFile is the workspace-relative path of that nested file.
	// Full asserts the default writer's export carries only writerResultPath
	// and this, so a smuggled filename or symlink target cannot pass the
	// content-only marker scan.
	workspaceStateFile = "nested/state.txt"
	// probeStopTimeout bounds the wait for a probe's own container to stop.
	probeStopTimeout = 3 * time.Minute
)

// suiteBudget is Full's overall wall-clock ceiling: the synthetic handoff's
// own budget plus room for the seed and the two probes (each a create, a
// start, and a bounded wait). A wedge backstop, not an SLA; it exists so a
// runtime that hangs inside a side-effecting call fails the suite closed
// instead of blocking a long-lived daemon context forever.
func (s *Suite) suiteBudget() time.Duration {
	return s.b.cfg.HandoffTimeout + 4*probeStopTimeout
}

// withDefaults fills unset fixture fields against the backend config.
func (fx SuiteFixture) withDefaults(cfg Config) SuiteFixture {
	if fx.CredentialTarget == "" {
		fx.CredentialTarget = "/credentials"
	}
	if fx.WorkspaceSizeMB == 0 {
		fx.WorkspaceSizeMB = 64
	}
	if fx.CredentialSizeMB == 0 {
		fx.CredentialSizeMB = 8
	}
	if len(fx.AgentCommand) == 0 {
		token := shellQuote(path.Join(fx.CredentialTarget, credentialTokenFile))
		ws := shellQuote(cfg.WorkspaceTarget)
		fx.AgentCommand = []string{
			"sh", "-c",
			// Verify the realized credential is the seeded marker before writing
			// anything: a runtime that mounted some other volume carrying a
			// `token` file, or did not realize the mount at all, makes the test
			// fail and (under set -eu) aborts the writer, so no output is produced
			// and containment fails closed rather than passing over a writer that
			// never saw the credential. The marker is an inert token
			// (markerPattern), safe unquoted as in the seed's own use.
			"set -eu; test \"$(cat " + token + ")\" = " + fx.CredentialMarker + "; " +
				// Emit this run's writer sentinel only after the marker check, so
				// its presence in the export proves this run's writer produced the
				// output (not stale or prepopulated content).
				"printf '%s\\n' " + writerSentinel(fx.RunID) + " > " + ws + "/" + writerResultPath + "; " +
				"mkdir -p " + ws + "/nested; " +
				"printf '%s\\n' " + workspaceStatePayload + " > " + ws + "/" + workspaceStateFile + "; sync",
		}
	}
	return fx
}

// shellQuote single-quotes s for safe inclusion in a `sh -c` command, so a
// path with spaces or shell metacharacters (which cleanAbs/cliSafe still
// allow) is treated as a literal, not parsed as shell syntax. Adjacent
// literals concatenate, so shellQuote(dir)+"/file" stays one path.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// markerPattern keeps the credential marker an inert token: the seed and
// audit containers embed it in a shell command, so a marker carrying shell
// metacharacters could break or inject that command. An underscore/alnum
// token (like the spike's FREESIDE_FAKE_..._DO_NOT_EXPORT) cannot.
var markerPattern = regexp.MustCompile(`^[A-Za-z0-9_]{1,128}$`)

// writerSentinel is the run-unique token the default writer emits into the
// workspace only after it verifies the seeded credential. Full requires it in
// the export to prove this run's writer produced the output (not stale or
// prepopulated content a runtime might return), keeping the containment result
// non-vacuous. RunID is validated (runIDPattern: lowercase alnum and hyphen),
// so the token stays inert in the writer's shell command and the export scan.
func writerSentinel(runID string) string {
	return "freeside-ward-writer-" + runID
}

// manifestHasContent reports whether the manifest carries a present
// (non-omitted) regular-file entry at path whose digest matches sha256(content).
// verifyExport re-hashes each present regular blob against its manifest digest,
// so a digest match on such an entry proves the blob's content equals content
// without re-reading it — and, unlike scanning the export tree, cannot be
// satisfied by a path/filename that merely echoes the content string in
// manifest.json. The Kind/BlobOmitted guard is load-bearing: verifyManifest
// skips re-hashing omitted regular entries (export_verify.go), so a runtime
// could otherwise return a blob_omitted entry carrying the (publicly derivable)
// expected digest and pass this check with no blob — and thus no proof the
// writer ran — behind it.
func manifestHasContent(entries []export.Entry, path, content string) bool {
	sum := sha256.Sum256([]byte(content))
	want := export.Digest("sha256:" + hex.EncodeToString(sum[:]))
	for _, e := range entries {
		if e.Path == path && e.Kind == export.EntryRegular && !e.BlobOmitted && e.Digest != nil && *e.Digest == want {
			return true
		}
	}
	return false
}

// auditSentinel is the run-unique token the detached-volume audit writes into
// its rootfs only after confirming the seeded marker is readable from the
// credential volume. probeCredentialContainment scans the audit rootfs export
// for it rather than the bare marker, so a short marker coincidentally present
// in the base image cannot pass the containment audit vacuously. RunID is
// validated (runIDPattern), so the token stays inert in the shell command and
// the export scan.
func auditSentinel(runID string) string {
	return "freeside-ward-audit-" + runID
}

// validate reports the first fixture violation.
func (fx SuiteFixture) validate() error {
	switch {
	case !digestPinnedImagePattern.MatchString(fx.AgentImage):
		return fmt.Errorf("%w: SuiteFixture.AgentImage must be digest-pinned", ErrInvalidConfig)
	case !markerPattern.MatchString(fx.CredentialMarker):
		return fmt.Errorf("%w: SuiteFixture.CredentialMarker must be a non-empty inert token matching %s", ErrInvalidConfig, markerPattern)
	case !cleanAbs(fx.CredentialTarget):
		return fmt.Errorf("%w: SuiteFixture.CredentialTarget %q is not a clean absolute non-root path", ErrInvalidConfig, fx.CredentialTarget)
	case !cliSafe(fx.CredentialTarget):
		return fmt.Errorf("%w: SuiteFixture.CredentialTarget %q carries a CLI mount-option delimiter", ErrInvalidConfig, fx.CredentialTarget)
	case fx.WorkspaceSizeMB <= 0:
		return fmt.Errorf("%w: SuiteFixture.WorkspaceSizeMB %d is not positive", ErrInvalidConfig, fx.WorkspaceSizeMB)
	case fx.CredentialSizeMB <= 0:
		return fmt.Errorf("%w: SuiteFixture.CredentialSizeMB %d is not positive", ErrInvalidConfig, fx.CredentialSizeMB)
	case !runIDPattern.MatchString(fx.RunID):
		return fmt.Errorf("%w: SuiteFixture.RunID %q does not match %s", ErrInvalidConfig, fx.RunID, runIDPattern)
	}
	return nil
}

// NewSuite builds a conformance suite over an initialized backend. The
// fixture must carry a digest-pinned agent image, a credential marker, and a
// valid run ID; other fields default.
func NewSuite(b *Backend, fx SuiteFixture) (*Suite, error) {
	if b == nil || !b.initialized {
		return nil, fmt.Errorf("%w: Suite requires an initialized Backend", ErrInvalidConfig)
	}
	// Capture before withDefaults fills it: whether the writer command is ours
	// decides how strong a containment non-vacuousness proof Full can apply
	// (only the default writer emits the run-unique sentinel Full checks for).
	defaultWriter := len(fx.AgentCommand) == 0
	fx = fx.withDefaults(b.cfg)
	if err := fx.validate(); err != nil {
		return nil, err
	}
	// The credential mounts alongside the workspace in the writer; a target
	// equal to or nested under the workspace (or the reverse) collides with
	// the workspace mount, and the handoff's own agent-spec validation would
	// reject it downstream, mis-reporting a malformed fixture as a runtime
	// conformance failure. Reject it at construction instead.
	if err := disjointPaths(b.cfg.WorkspaceTarget, fx.CredentialTarget); err != nil {
		return nil, fmt.Errorf("%w: SuiteFixture.CredentialTarget %q must be disjoint from the workspace %q", ErrInvalidConfig, fx.CredentialTarget, b.cfg.WorkspaceTarget)
	}
	// The audit writes its sentinel to a fixed rootfs path; a credential target
	// that equals or nests with it would mount the credential volume over that
	// path, shadowing the sentinel write and failing a conformant backend.
	if err := disjointPaths(auditMarkerPath(fx.RunID), fx.CredentialTarget); err != nil {
		return nil, fmt.Errorf("%w: SuiteFixture.CredentialTarget %q must be disjoint from the audit marker path %q", ErrInvalidConfig, fx.CredentialTarget, auditMarkerPath(fx.RunID))
	}
	// The default writer injects the run's writer sentinel and the fixed state
	// payload into the scanned workspace, and its two output paths appear in the
	// export manifest metadata (which the default path does not scan for the
	// marker, precisely to avoid this collision). A marker that is a substring of
	// any of these four would make the suite's own output indistinguishable from
	// a leak — spuriously failing a conformant backend, or letting the marker sit
	// in the released metadata unflagged. Reject such a fixture up front. (A
	// caller-supplied command owns its own output, so this cannot arise there.)
	if defaultWriter {
		for _, reserved := range []string{writerSentinel(fx.RunID), workspaceStatePayload, writerResultPath, workspaceStateFile} {
			if strings.Contains(reserved, fx.CredentialMarker) {
				return nil, fmt.Errorf("%w: SuiteFixture.CredentialMarker %q collides with the generated suite string %q", ErrInvalidConfig, fx.CredentialMarker, reserved)
			}
		}
	}
	// Freeze the caller-owned command so a later mutation cannot change the
	// synthetic writer after validation (matching New and Handoff).
	fx.AgentCommand = slices.Clone(fx.AgentCommand)
	return &Suite{b: b, fx: fx, defaultWriter: defaultWriter}, nil
}

// conformanceObjectName builds a probe object's runtime name from the run ID
// and a role, disjoint from the handoff's own object names (namesFor). A free
// function so NewSuite can derive generated paths before a Suite exists.
func conformanceObjectName(runID, role string) string {
	return "freeside-ward-conformance-" + runID + "-" + role
}

// conformanceName builds a probe object's runtime name for this suite's run.
func (s *Suite) conformanceName(role string) string {
	return conformanceObjectName(s.fx.RunID, role)
}

// auditMarkerPath is the audit container's rootfs path where it writes the run
// sentinel (probeCredentialContainment). It must stay disjoint from the
// credential mount target, or the mount would shadow the sentinel write.
func auditMarkerPath(runID string) string {
	return "/" + conformanceObjectName(runID, "audit-marker")
}

// cleanupContext detaches a reap from the caller's cancellation and gives it
// its own teardown deadline, so the suite reaps its own objects even when the
// caller cancels mid-run (the handoff teardown detaches the same way). Reused
// by every deferred reap; the caller must invoke the returned cancel.
func (s *Suite) cleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), s.b.cfg.TeardownTimeout)
}

// reapVolume best-effort deletes a suite-created volume under a detached
// context. Registered as a defer before the CreateVolume, so an ambiguous
// create (the volume made, then the call errors) is still reaped; deleting a
// name that was never created returns a tolerated not-found. Suite object
// names are unique per run, so name-addressed deletion only ever hits this
// run's own objects (unlike the handoff gate, which may face foreign
// same-name objects and needs the ownership machinery).
func (s *Suite) reapVolume(ctx context.Context, name string) {
	cctx, cancel := s.cleanupContext(ctx)
	defer cancel()
	_ = s.b.rt.DeleteVolume(cctx, name)
}

// suiteContainerNames and suiteVolumeNames are the exact names of every
// runtime object the suite creates for this run. verifyReaped matches these
// exactly rather than by prefix: run IDs may contain hyphens, so a prefix
// (RunID + "-") for run "conf-1" would also match a stale or concurrent run
// "conf-1-a" object and wrongly report a leak.
func (s *Suite) suiteContainerNames() []string {
	return []string{
		s.conformanceName("seed"),
		s.conformanceName("audit"),
		s.conformanceName("prejob"),
		s.conformanceName("liveness"),
		s.conformanceName("excl-writer"),
		s.conformanceName("excl-second"),
	}
}

func (s *Suite) suiteVolumeNames() []string {
	return []string{
		s.conformanceName("cred"),
		s.conformanceName("liveness-ws"),
		s.conformanceName("excl-ws"),
	}
}

// verifyReaped is the suite's fail-closed cleanup gate: after the best-effort
// reaps have run, it proves no suite-created object survived by listing the
// runtime (the handoff gate proves absence the same way, rather than trusting
// a delete call). A survivor, or an inability to list, fails the suite closed:
// the reaps are best-effort so an ambiguous create is still attempted, but the
// conformance result is not clean while an object the suite made remains. The
// object names are gate-generated, never credential material.
func (s *Suite) verifyReaped(ctx context.Context) error {
	cctx, cancel := s.cleanupContext(ctx)
	defer cancel()
	ctrNames := make(map[string]bool, 5)
	for _, n := range s.suiteContainerNames() {
		ctrNames[n] = true
	}
	volNames := make(map[string]bool, 2)
	for _, n := range s.suiteVolumeNames() {
		volNames[n] = true
	}
	ctrs, err := s.b.rt.ListContainers(cctx)
	if err != nil {
		return failf(CheckTeardown, "verify suite containers reaped: %v", err)
	}
	for _, c := range ctrs {
		if ctrNames[c.ID] {
			return failf(CheckTeardown, "suite container %q survived cleanup", c.ID)
		}
	}
	vols, err := s.b.rt.ListVolumes(cctx)
	if err != nil {
		return failf(CheckTeardown, "verify suite volumes reaped: %v", err)
	}
	for _, v := range vols {
		if volNames[v.Name] {
			return failf(CheckTeardown, "suite volume %q survived cleanup", v.Name)
		}
	}
	return nil
}

// Full runs the whole workspace-handoff contract as one conformance pass on
// the current runtime: a synthetic handoff exercising checks 1-5 and 7, the
// credential-marker containment probe, and the read-write-attach exclusion
// probe. It is fail-closed and self-cleaning: it reaps every object it
// creates on every path. A non-nil error means the backend is not proven
// conformant and the caller must not run unattended.
func (s *Suite) Full(ctx context.Context) (err error) {
	// Bound the whole pass so a runtime that wedges inside a side-effecting
	// call (e.g. after launching a probe VM but before StartContainer returns)
	// cannot hang the suite under a long-lived caller context; the handoff
	// guards itself the same way with HandoffTimeout. The deferred reaps and
	// the absence proof detach from this budget (cleanupContext), so they
	// still run after it fires. Registered first, so cancel runs last.
	ctx, cancel := context.WithTimeout(ctx, s.suiteBudget())
	defer cancel()
	// Fail closed on a surviving object: registered before the reaps so it
	// runs after them (LIFO), turning an otherwise-clean run whose cleanup
	// left something behind into a failure. Joined with any primary error
	// (as the handoff teardown does) so a leak is still surfaced even when a
	// probe or check already failed, never suppressed by it.
	defer func() {
		if verr := s.verifyReaped(ctx); verr != nil {
			err = errors.Join(err, verr)
		}
	}()

	credVolume := s.conformanceName("cred")

	// The credential volume is the suite's own object (unlike a real handoff,
	// where the caller owns it): create, seed, and always reap it here. The
	// reap is registered before the create so an ambiguous create (volume
	// made, call errored) is still reaped.
	defer s.reapVolume(ctx, credVolume)
	if err := s.b.rt.CreateVolume(ctx, credVolume, s.fx.CredentialSizeMB, runLabels(s.fx.RunID)); err != nil {
		return fmt.Errorf("conformance: create credential volume: %w", err)
	}

	// Prove the runtime does not eager-start at create before the synthetic
	// handoff trusts its pre-start isolation inspect. The gate inspects the
	// agent's config before starting it but does not assert it is stopped, so a
	// runtime whose CreateContainer executes the writer would run it over the
	// mounted credential before checks 1-2 ever look. This throwaway probe
	// (long-lived payload, StateStopped-after-create) mounts the writer's own
	// topology — a fresh workspace volume read-write and the credential volume
	// read-only — so a runtime that eager-starts only mounted containers is
	// caught too, not just an unmounted one. It runs before the seed, so the
	// credential is still empty (the payload never reads it). Reaps registered
	// before the creates (ambiguous-create safe); the deferred verifyReaped
	// proves absence.
	livenessName := s.conformanceName("liveness")
	livenessVol := s.conformanceName("liveness-ws")
	defer s.reapVolume(ctx, livenessVol)
	defer s.reapRunning(ctx, livenessName)
	if err := s.b.rt.CreateVolume(ctx, livenessVol, s.fx.WorkspaceSizeMB, runLabels(s.fx.RunID)); err != nil {
		return fmt.Errorf("conformance: create liveness workspace volume: %w", err)
	}
	livenessMounts := []Mount{
		{Type: MountVolume, Source: livenessVol, Target: s.b.cfg.WorkspaceTarget},
		{Type: MountVolume, Source: credVolume, Target: s.fx.CredentialTarget, ReadOnly: true},
	}
	if err := s.proveNoEagerStart(ctx, livenessName, livenessMounts, CheckControlPlaneIsolation); err != nil {
		return err
	}

	if err := s.seedCredential(ctx, credVolume); err != nil {
		return err
	}

	// The synthetic handoff exercises checks 1-5 and 7 end to end with the
	// benign writer holding the seeded credential read-only. Its own gate
	// tears the writer, workspace, and exporter down; the credential volume
	// (caller-owned to the gate) survives for the containment probe.
	res, err := s.b.Handoff(ctx, HandoffSpec{
		RunID:           s.fx.RunID,
		WorkspaceSizeMB: s.fx.WorkspaceSizeMB,
		Agent: AgentSpec{
			Image:            s.fx.AgentImage,
			Command:          s.fx.AgentCommand,
			CredentialMounts: []CredentialMount{{Volume: credVolume, Target: s.fx.CredentialTarget}},
		},
	})
	if err != nil {
		// A *ConformanceFailure already names its check; any other error is a
		// fail-closed operational failure of the same gate.
		return err
	}
	defer func() { _ = os.RemoveAll(res.ExportDir) }()

	// Containment must not be vacuous: the export has to carry this run's
	// writer output, or "marker absent" proves nothing. An empty export means
	// either the credential mount was not realized (the writer aborted) or
	// nothing was exported.
	if len(res.Manifest.Entries) == 0 {
		return failf(CheckCredentialContainment, "handoff exported an empty workspace; containment cannot be proven")
	}
	// For the default writer, prove this run's writer actually produced the
	// export, not just that some content is present: the writer writes exactly
	// the run-unique sentinel line to result.txt only after verifying the seeded
	// credential. Match the manifest's own digest for that path — which
	// verifyExport already confirmed the blob content hashes to — rather than
	// scanning the export tree, so a stale file merely NAMED like the sentinel
	// (which would appear as a path in manifest.json) cannot satisfy the proof,
	// and the check binds to the exact expected content at the exact path. A
	// caller supplied its own command, so only the non-empty floor above applies.
	if s.defaultWriter && !manifestHasContent(res.Manifest.Entries, writerResultPath, writerSentinel(s.fx.RunID)+"\n") {
		return failf(CheckCredentialContainment, "export does not carry this run's writer sentinel at %s; containment cannot be proven", writerResultPath)
	}
	// Prove the export carries only this run's writer output: blobsContainMarker
	// scans blob CONTENT only, so a runtime could otherwise exfiltrate the marker
	// as a filename (a manifest path) or a symlink target — neither becomes a
	// scanned blob, and the §5.4 scanner is blind to the inert marker. The
	// default writer produces exactly result.txt and the nested state file as
	// regular files; reject any other entry or non-regular kind.
	if s.defaultWriter {
		for _, e := range res.Manifest.Entries {
			if e.Kind != export.EntryRegular || (e.Path != writerResultPath && e.Path != workspaceStateFile) {
				return failf(CheckCredentialContainment, "export carries an unexpected manifest entry %q (kind %q); the default writer produces only %q and %q, so a filename or symlink target could smuggle the credential past the content scan", e.Path, e.Kind, writerResultPath, workspaceStateFile)
			}
		}
	}

	// A blob_omitted regular entry has no bytes under ExportDir/blobs, so the
	// scan below cannot see whether it carries the marker: an export that omits
	// the very file holding the leaked credential would scan clean. The marker
	// scan proves absence only over content actually present, so fail closed if
	// any workspace file's blob was omitted (verifyManifest does not re-hash
	// omitted entries either — the same gap the sentinel check guards against).
	for _, e := range res.Manifest.Entries {
		if e.Kind == export.EntryRegular && e.BlobOmitted {
			return failf(CheckCredentialContainment, "export omits workspace blob %q; the credential marker's absence cannot be proven over omitted content", e.Path)
		}
	}
	// Containment, export half: the credential marker must be absent from the
	// released workspace *content* — the extracted blobs, not the gate's
	// manifest.json. The manifest carries content-derived hex digests and fixed
	// vocabulary that a short marker could coincidentally match, which would
	// spuriously fail a conformant backend; only the agent-authored blobs can
	// carry a real credential leak. The configured §5.4 scanner already ran
	// inside the handoff; this is the probe's own assertion against this marker.
	found, err := blobsContainMarker(res.ExportDir, s.fx.CredentialMarker)
	if err != nil {
		return fmt.Errorf("conformance: scan export blobs for credential marker: %w", err)
	}
	if found {
		return failf(CheckCredentialContainment, "credential marker present in the released export")
	}
	// A caller-supplied command opts out of the exact-shape check above, so the
	// content-only blob scan is its only marker guard — but a custom payload or
	// exporter could leak the marker as a filename (a manifest path) or a symlink
	// target with clean blob content, invisible to that scan. Scan the manifest's
	// structured path and symlink-target metadata for the marker too (never the
	// digests, whose hex could coincidentally match a short marker). The default
	// writer needs no such scan: its exact-shape check already forbids any entry
	// but result.txt and the nested state file.
	if !s.defaultWriter && manifestLeaksMarker(res.Manifest.Entries, s.fx.CredentialMarker) {
		return failf(CheckCredentialContainment, "credential marker present in the released export metadata (a file path or symlink target)")
	}
	// The default writer also writes a nested workspace file, so the deterministic
	// directory tree must survive the export: a lossy exporter that drops nested
	// content while preserving result.txt would otherwise pass every check above.
	// Require the nested fixture's exact content too (verifyExport confirmed the
	// digest), completing "the export is exactly this run's writer output".
	if s.defaultWriter && !manifestHasContent(res.Manifest.Entries, workspaceStateFile, workspaceStatePayload+"\n") {
		return failf(CheckCredentialContainment, "export does not carry this run's nested workspace fixture at %s; the durable directory tree was not proven to survive", workspaceStateFile)
	}

	// Containment, detached-volume half: the marker is still readable from the
	// credential volume, proving absence from the export was mount omission,
	// not deletion.
	if err := s.probeCredentialContainment(ctx, credVolume); err != nil {
		return err
	}

	// The read-write-attach exclusion the gate's check-3 termination depends
	// on: a second VM cannot attach a volume a live writer holds read-write.
	return s.probeWriterVolumeExclusion(ctx)
}

// PreJob is the lightweight pre-job probe (plan §5.7): a fast, fail-closed
// precondition check before each unattended job. It verifies only cheap
// preconditions — the capability declaration is intact, the images are
// digest-pinned, the runtime is reachable, and a create→inspect→delete
// liveness round-trips — and boots no VM, copies no workspace, exports
// nothing.
//
// It deliberately does NOT re-verify the realized isolation the full suite
// proves: credential separation actually holding in a started writer, the
// read-only remount, export containment, or the negative probes. A green
// PreJob means the backend is plausibly still operable; only Full proves it
// conformant. Run Full at startup, after configuration changes, and on the
// doctor schedule; run PreJob before each job.
func (s *Suite) PreJob(ctx context.Context) (err error) {
	// Bound the probe so a wedged runtime call fails closed rather than hanging
	// a long-lived doctor/startup context. PreJob boots no VM, so the teardown
	// timeout is a generous ceiling for its create/inspect/delete round-trip.
	ctx, cancel := context.WithTimeout(ctx, s.b.cfg.TeardownTimeout)
	defer cancel()
	// Capability declaration intact (in-memory, free): the floor the gate
	// admits against must still be declared.
	caps := s.b.Capabilities()
	for _, c := range requiredCapabilities {
		if !caps.Has(c) {
			return failf(CheckPreJobProbe, "capability declaration missing a required capability")
		}
	}
	// Images digest-pinned (in-memory, free): trust binds to bytes.
	if !digestPinnedImagePattern.MatchString(s.b.cfg.ExporterImage) {
		return failf(CheckPreJobProbe, "exporter image is not digest-pinned")
	}
	if !digestPinnedImagePattern.MatchString(s.fx.AgentImage) {
		return failf(CheckPreJobProbe, "fixture agent image is not digest-pinned")
	}
	// Runtime reachable (cheap, no VM): a listing round-trips.
	if _, err := s.b.rt.ListVolumes(ctx); err != nil {
		return failf(CheckPreJobProbe, "runtime unreachable: %v", err)
	}
	// Liveness (create→inspect→delete, no start): the runtime can realize,
	// observe, and reap a container from the fixture image, and create realizes
	// metadata only (no eager start). From here the liveness container may be
	// created, so mirror Full: prove absence on every exit (not just after a
	// clean delete) and join any leak, and reap before the create so an
	// ambiguous create is reaped too.
	name := s.conformanceName("prejob")
	defer func() {
		if verr := s.verifyReaped(ctx); verr != nil {
			err = errors.Join(err, verr)
		}
	}()
	defer s.reapRunning(ctx, name)
	return s.proveNoEagerStart(ctx, name, nil, CheckPreJobProbe)
}

// proveNoEagerStart creates a throwaway container from the fixture image with a
// long-lived inert payload and the given mounts, and requires it StateStopped
// after create (create realizes metadata only; a runtime that eager-started it
// would keep the payload running past the inspect — a short-lived "true" could
// exit first and self-mask as stopped), then deletes it. Shared by PreJob (its
// core liveness check, unmounted — its scope is cheap preconditions) and Full
// (a preamble: the synthetic handoff's pre-start isolation inspect assumes
// create does not execute the writer, so an eager-start runtime would run the
// writer before those checks; Full mounts the writer's own workspace+credential
// topology so a mount-conditional eager-start is caught too). The caller
// registers the reap/absence-proof deferrals for name.
func (s *Suite) proveNoEagerStart(ctx context.Context, name string, mounts []Mount, check Check) error {
	spec := ContainerSpec{
		Name:    name,
		Image:   s.fx.AgentImage,
		Command: []string{"sh", "-c", "sleep 300"},
		Mounts:  mounts,
		Labels:  runLabels(s.fx.RunID),
	}
	if err := s.b.rt.CreateContainer(ctx, cloneContainerSpec(spec)); err != nil {
		return failf(check, "runtime cannot create a container: %v", err)
	}
	rep, err := s.b.rt.Inspect(ctx, name)
	if err != nil {
		return failf(check, "runtime cannot inspect a container: %v", err)
	}
	if rep.ID != name {
		return failf(check, "liveness inspection identified the wrong container")
	}
	// The stopped-state proof is only meaningful if the container realized the
	// probe's spec: a runtime that dropped the mounts or changed the long-lived
	// command could report stopped without ever exercising the mounted, running
	// create path this probe stands in for. Confirm the realized image, command,
	// and mounts match before trusting the state.
	if !sameImage(spec.Image, rep.ImageReference) || !slices.Equal(rep.Command, spec.Command) || !sameMounts(rep.Mounts, spec.Mounts) {
		return failf(check, "liveness container did not realize the probe spec (image, command, or mounts); the no-eager-start proof did not exercise the intended create path")
	}
	if rep.State != StateStopped {
		return failf(check, "container is not stopped after create (state %q); the runtime executed it before inspection", rep.State)
	}
	if err := s.b.rt.DeleteContainer(ctx, name); err != nil {
		return failf(check, "runtime cannot delete a container: %v", err)
	}
	return nil
}

// seedCredential writes the fake marker into the credential volume through a
// throwaway seed container, then reaps it. The marker lands at
// CredentialTarget/token, where the writer and audit containers read it.
func (s *Suite) seedCredential(ctx context.Context, credVolume string) error {
	name := s.conformanceName("seed")
	token := shellQuote(path.Join(s.fx.CredentialTarget, credentialTokenFile))
	spec := ContainerSpec{
		Name:    name,
		Image:   s.fx.AgentImage,
		Command: []string{"sh", "-c", "printf '%s\\n' " + s.fx.CredentialMarker + " > " + token + "; sync"},
		Mounts:  []Mount{{Type: MountVolume, Source: credVolume, Target: s.fx.CredentialTarget}},
		Labels:  runLabels(s.fx.RunID),
	}
	// Reap (stop then delete) is registered before the create so an ambiguous
	// create is reaped, and so a seed that starts but never stops is stopped
	// before deletion (a delete-only reap would leave the running VM holding
	// the credential volume).
	defer s.reapRunning(ctx, name)
	if err := s.b.rt.CreateContainer(ctx, cloneContainerSpec(spec)); err != nil {
		return fmt.Errorf("conformance: create credential seed: %w", err)
	}
	if err := s.b.rt.StartContainer(ctx, name); err != nil {
		return fmt.Errorf("conformance: start credential seed: %w", err)
	}
	if err := s.waitStopped(ctx, name, probeStopTimeout); err != nil {
		return fmt.Errorf("conformance: credential seed did not stop: %w", err)
	}
	return nil
}

// probeCredentialContainment is the detached-volume half of the second
// negative probe: mount the surviving credential volume read-only in a fresh
// audit container, confirm its token is the seeded marker, and emit this run's
// audit sentinel into the container's own root filesystem; the export must
// carry that sentinel. Its absence means the credential did not survive
// detachment (the containment claim would be deletion, not omission).
func (s *Suite) probeCredentialContainment(ctx context.Context, credVolume string) error {
	name := s.conformanceName("audit")
	token := shellQuote(path.Join(s.fx.CredentialTarget, credentialTokenFile))
	markerFile := shellQuote(auditMarkerPath(s.fx.RunID))
	spec := ContainerSpec{
		Name:  name,
		Image: s.fx.AgentImage,
		// Fail closed unless the detached volume is mounted and its token is the
		// seeded marker: set -eu plus an explicit equality test, so a deleted or
		// unmounted volume (or a wrong token) aborts before writing. Only then
		// emit this run's audit sentinel — a run-unique token, unlike the bare
		// marker, so scanning the whole rootfs export for it cannot match a
		// coincidental base-image occurrence of a short marker.
		Command: []string{
			"sh", "-c",
			"set -eu; test \"$(cat " + token + ")\" = " + s.fx.CredentialMarker + "; " +
				"printf '%s\\n' " + auditSentinel(s.fx.RunID) + " > " + markerFile + "; sync",
		},
		Mounts: []Mount{{Type: MountVolume, Source: credVolume, Target: s.fx.CredentialTarget, ReadOnly: true}},
		Labels: runLabels(s.fx.RunID),
	}
	// Reap before create (ambiguous-create safe) and stop-then-delete (an
	// audit that never stops would otherwise survive a delete-only reap).
	defer s.reapRunning(ctx, name)
	if err := s.b.rt.CreateContainer(ctx, cloneContainerSpec(spec)); err != nil {
		return failf(CheckCredentialContainment, "create credential-audit container: %v", err)
	}
	// Confirm the runtime realized this run's credential volume read-only at the
	// credential target before trusting the token: a runtime that mounted a
	// different volume/source that coincidentally holds the same marker (a stale
	// same-marker fixture volume) would otherwise let the token test pass while
	// proving nothing about credVolume's own survival. Mirror the exclusion
	// probe's mount-realization check (sameMounts compares source and readonly).
	// The create above got a clone, so `spec.Mounts` here is the immutable
	// expected allowlist a mutating runtime cannot have rewritten.
	arep, err := s.b.rt.Inspect(ctx, name)
	if err != nil {
		return failf(CheckCredentialContainment, "inspect credential-audit container: %v", err)
	}
	if arep.ID != name || !sameMounts(arep.Mounts, spec.Mounts) {
		return failf(CheckCredentialContainment, "credential-audit container did not realize this run's credential volume mount")
	}
	if err := s.b.rt.StartContainer(ctx, name); err != nil {
		return failf(CheckCredentialContainment, "start credential-audit container: %v", err)
	}
	if err := s.waitStopped(ctx, name, probeStopTimeout); err != nil {
		return failf(CheckCredentialContainment, "credential-audit container did not stop: %v", err)
	}
	// The mounted volume's contents are not in the container's own rootfs
	// export (that is exactly why the gate's exporter cannot leak them); the
	// audit writes the run-unique sentinel into the rootfs, gated on the marker
	// being readable, so this export can observe it. Scanning for the sentinel
	// rather than the bare marker keeps a short marker coincidentally present in
	// the base image from passing the audit. The stream is hard-capped at
	// MaxArchiveBytes host-side (as the handoff's materializeRootFS caps its
	// archive), so a runtime that cannot enforce maxBytes itself cannot make
	// Full drain an oversized audit export: the scan sits behind the cap writer
	// and an over-cap stream fails closed.
	scan := &markerScanWriter{marker: []byte(auditSentinel(s.fx.RunID))}
	capped := &archiveCapWriter{dest: scan, remaining: s.b.cfg.MaxArchiveBytes}
	exportErr := s.b.rt.ExportRootFS(ctx, name, capped, s.b.cfg.MaxArchiveBytes)
	// Check overflow first, as materializeRootFS does: a Runtime that swallows
	// the cap error and returns nil after writing past the cap must still fail
	// closed rather than pass containment on an oversized export.
	if capped.overflow {
		return failf(CheckCredentialContainment, "audit rootfs export exceeds the byte cap")
	}
	if exportErr != nil {
		return failf(CheckCredentialContainment, "export credential-audit rootfs: %v", exportErr)
	}
	if !scan.found {
		return failf(CheckCredentialContainment, "credential marker not readable from the detached credential volume")
	}
	return nil
}

// probeWriterVolumeExclusion is the first negative probe: while a live writer
// holds a fresh workspace volume read-write, a second container attaching the
// same volume must be refused by the runtime (Virtualization.framework
// VZErrorDomain Code=2 at bootstrap). A successful second attach means the
// exclusion check 3's writer-termination requirement depends on does not
// hold, so the gate could export a workspace another VM still holds.
func (s *Suite) probeWriterVolumeExclusion(ctx context.Context) error {
	volume := s.conformanceName("excl-ws")
	writer := s.conformanceName("excl-writer")
	second := s.conformanceName("excl-second")

	// Reaps registered before any create so an ambiguous create is reaped;
	// LIFO runs them in reverse dependency order (writer, then second, then
	// the volume they held). All best-effort under a detached context.
	defer s.reapVolume(ctx, volume)
	defer s.reapRunning(ctx, second)
	defer s.reapRunning(ctx, writer)

	if err := s.b.rt.CreateVolume(ctx, volume, s.fx.WorkspaceSizeMB, runLabels(s.fx.RunID)); err != nil {
		return failf(CheckWriterVolumeExclusion, "create exclusion workspace volume: %v", err)
	}

	// A long-lived writer holds the volume read-write and stays live while the
	// second attach is attempted.
	writerSpec := ContainerSpec{
		Name:    writer,
		Image:   s.fx.AgentImage,
		Command: []string{"sh", "-c", "sleep 300"},
		Mounts:  []Mount{{Type: MountVolume, Source: volume, Target: s.b.cfg.WorkspaceTarget}},
		Labels:  runLabels(s.fx.RunID),
	}
	if err := s.b.rt.CreateContainer(ctx, cloneContainerSpec(writerSpec)); err != nil {
		return failf(CheckWriterVolumeExclusion, "create exclusion writer: %v", err)
	}
	if err := s.b.rt.StartContainer(ctx, writer); err != nil {
		return failf(CheckWriterVolumeExclusion, "start exclusion writer: %v", err)
	}
	// The exclusion is only meaningful while the writer is actually live and
	// holding the volume; a writer that never booted would let the second
	// attach succeed for the wrong reason. Confirm it is observed running.
	wrep, err := s.b.rt.Inspect(ctx, writer)
	if err != nil {
		return failf(CheckWriterVolumeExclusion, "inspect exclusion writer: %v", err)
	}
	if wrep.ID != writer || wrep.State != StateRunning {
		return failf(CheckWriterVolumeExclusion, "exclusion writer not observed running before the second attach")
	}
	// The exclusion only means something if the writer actually realized the
	// read-write workspace mount: a runtime that dropped it, changed it, or
	// made it read-only would let the second attach fail (or succeed) for a
	// reason unrelated to a held read-write volume. Verify the realized mount
	// matches the requested rw workspace mount before trusting the refusal.
	if !sameMounts(wrep.Mounts, writerSpec.Mounts) {
		return failf(CheckWriterVolumeExclusion, "exclusion writer did not realize the read-write workspace mount")
	}

	// The writer now holds the volume rw. A second attach must be refused. The
	// spike observed the refusal at VM bootstrap (start), so create should
	// succeed and start must fail; a create failure is inconclusive
	// (unexpected, since the writer created fine with the same spec shape) and
	// fails closed rather than counting as the exclusion.
	secondSpec := ContainerSpec{
		Name:    second,
		Image:   s.fx.AgentImage,
		Command: []string{"sh", "-c", "sleep 300"},
		Mounts:  []Mount{{Type: MountVolume, Source: volume, Target: s.b.cfg.WorkspaceTarget}},
		Labels:  runLabels(s.fx.RunID),
	}
	if cerr := s.b.rt.CreateContainer(ctx, cloneContainerSpec(secondSpec)); cerr != nil {
		return failf(CheckWriterVolumeExclusion, "create exclusion second container: %v", cerr)
	}
	// Confirm the second realized the same read-write workspace mount before
	// start, so a start refusal is about the held volume and not some other
	// mount the runtime dropped or changed.
	srep, err := s.b.rt.Inspect(ctx, second)
	if err != nil {
		return failf(CheckWriterVolumeExclusion, "inspect exclusion second container: %v", err)
	}
	if srep.ID != second || !sameMounts(srep.Mounts, secondSpec.Mounts) {
		return failf(CheckWriterVolumeExclusion, "exclusion second container did not realize the workspace mount")
	}
	serr := s.b.rt.StartContainer(ctx, second)
	if serr == nil {
		// The second VM attached the volume the writer holds read-write: the
		// exclusion does not hold.
		return failf(CheckWriterVolumeExclusion, "a second VM attached the workspace volume while the writer held it read-write")
	}
	// The start failed. Only a storage-attachment refusal proves the exclusion;
	// an unrelated start failure must not pass as one, or the probe would
	// green-light a runtime whose exclusion does not actually hold.
	if !isAttachmentExclusion(serr) {
		return failf(CheckWriterVolumeExclusion, "second attach failed but not with the storage-device attachment exclusion")
	}
	// A storage-attachment refusal must mean the second VM did not attach.
	// Confirm it by observation, and fail closed if that observation is
	// inconclusive: an ignored inspect error could hide a second that is
	// actually running behind a transient runtime failure.
	srep2, ierr := s.b.rt.Inspect(ctx, second)
	if ierr != nil {
		return failf(CheckWriterVolumeExclusion, "could not confirm the second container's state after the attachment error: %v", ierr)
	}
	if srep2.ID != second {
		return failf(CheckWriterVolumeExclusion, "second-container inspection after the attachment error identified the wrong container")
	}
	if srep2.State == StateRunning {
		return failf(CheckWriterVolumeExclusion, "second container is running despite the attachment error; the volume was not excluded")
	}
	// runtime.go treats exactly StateStopped as proof the VM is gone (a
	// created-but-never-started container reports stopped too); any other
	// state — starting, the zero/unknown value, or a future one — leaves the
	// exclusion unproven, so fail closed rather than accept "not running".
	if srep2.State != StateStopped {
		return failf(CheckWriterVolumeExclusion, "second container is not stopped after the attachment error (state %q); the volume exclusion is unproven", srep2.State)
	}
	// The exclusion claim is that a *live* writer holding the rw mount excludes
	// the second attach. A runtime that instead resolved the conflict by
	// stopping or replacing the holder (then failing the second start for that
	// reason) would satisfy every check above without demonstrating exclusion.
	// Re-inspect the writer and require it still observed running with the same
	// rw mount before passing.
	wrep2, werr := s.b.rt.Inspect(ctx, writer)
	if werr != nil {
		return failf(CheckWriterVolumeExclusion, "could not re-confirm the exclusion writer after the attachment error: %v", werr)
	}
	if wrep2.ID != writer || wrep2.State != StateRunning || !sameMounts(wrep2.Mounts, writerSpec.Mounts) {
		return failf(CheckWriterVolumeExclusion, "exclusion writer no longer holds the read-write mount after the refusal; the runtime may have resolved the conflict by evicting the holder")
	}
	return nil
}

// vzDomainCodePattern captures the numeric code of the first VZErrorDomain
// occurrence in the lowercased error. isAttachmentExclusion requires that
// first/top-level code to be exactly 2 (the storage-device attachment
// refusal): a later VZErrorDomain Code=2 nested in an underlying error, a
// nested NSError Code=2, or a longer code like Code=20 does not qualify,
// because FindStringSubmatch returns the leftmost (top-level) match and the
// whole code is compared, not a prefix.
var vzDomainCodePattern = regexp.MustCompile(`vzerrordomain\s+code=(\d+)`)

// isAttachmentExclusion reports whether a StartContainer error is the
// Virtualization.framework refusal to attach a storage device a live VM
// already holds read-write (the spike observed VZErrorDomain Code=2, "The
// storage device attachment is invalid"). It requires the storage-device
// signal specifically: an unrelated VZError, even one about some other
// attachment, must not pass a probe Full uses to prove the read-write
// storage-volume exclusion actually holds. When a VZErrorDomain code is
// present it is authoritative and must be exactly 2, so the storage-device
// wording cannot rescue a differently-coded VZError (e.g. Code=20 "storage
// device attachment failed"); the wording is only a fallback for a reworded
// message that carries no VZ code at all.
func isAttachmentExclusion(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "attachment") {
		return false
	}
	// A VZErrorDomain code, when present, is authoritative: the first
	// (top-level) domain's code must be exactly 2, regardless of wording.
	if m := vzDomainCodePattern.FindStringSubmatch(msg); m != nil {
		return m[1] == "2"
	}
	// No VZ code at all: fall back to the storage-device wording, so a reworded
	// message that dropped the code still matches.
	return strings.Contains(msg, "storage device")
}

// reapRunning stops and deletes a probe container best-effort, tolerating its
// absence (an unstarted or never-created second container). It reaps under a
// context detached from the caller's cancellation, so a mid-run cancel still
// reaps the exclusion probe's live writer.
func (s *Suite) reapRunning(ctx context.Context, name string) {
	cctx, cancel := s.cleanupContext(ctx)
	defer cancel()
	_ = s.b.rt.StopContainer(cctx, name)
	_ = s.b.rt.DeleteContainer(cctx, name)
}

// waitStopped polls until the named container is observed stopped or the
// timeout elapses. It is the suite's own wait for its throwaway probe
// containers; the gate's waitStopped is bound to handoff ownership state.
func (s *Suite) waitStopped(ctx context.Context, name string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		rep, err := s.b.rt.Inspect(ctx, name)
		if err != nil {
			return err
		}
		if rep.ID != name {
			return fmt.Errorf("inspection identified the wrong container")
		}
		if rep.State == StateStopped {
			return nil
		}
		if err := s.b.cfg.Sleep(ctx, s.b.cfg.PollInterval); err != nil {
			return err
		}
	}
}

// markerScanWriter reports whether a marker substring appears in the bytes
// streamed through it, keeping only a marker-sized tail across chunk
// boundaries so the scan is bounded regardless of the stream size.
type markerScanWriter struct {
	marker []byte
	tail   []byte
	found  bool
}

func (w *markerScanWriter) Write(p []byte) (int, error) {
	if w.found || len(w.marker) == 0 {
		return len(p), nil
	}
	buf := append(w.tail, p...)
	if bytes.Contains(buf, w.marker) {
		w.found = true
		w.tail = nil
		return len(p), nil
	}
	// Retain the last len(marker)-1 bytes so a match spanning this chunk and
	// the next is not missed. Copy into a fresh slice; buf may alias tail.
	keep := len(w.marker) - 1
	if keep > len(buf) {
		keep = len(buf)
	}
	w.tail = append([]byte(nil), buf[len(buf)-keep:]...)
	return len(p), nil
}

// manifestLeaksMarker reports whether the credential marker appears in a
// manifest entry's metadata — a file path or a symlink target — rather than
// blob content. blobsContainMarker deliberately scans only blob content, so a
// runtime that exfiltrated the marker as a filename or symlink target (neither
// of which becomes a scanned blob) would otherwise escape it. Only the
// structured path/target fields are scanned, never the content-derived digests
// whose hex could coincidentally match a short marker; a non-UTF8 path is
// decoded from path_hex so a marker smuggled in raw name bytes is caught too.
func manifestLeaksMarker(entries []export.Entry, marker string) bool {
	for _, e := range entries {
		if strings.Contains(e.Path, marker) {
			return true
		}
		if e.Target != nil && strings.Contains(*e.Target, marker) {
			return true
		}
		if e.PathHex != "" {
			if raw, err := hex.DecodeString(e.PathHex); err == nil && strings.Contains(string(raw), marker) {
				return true
			}
		}
	}
	return false
}

// blobsContainMarker scans only the extracted blob content under exportDir
// (the agent-authored workspace files at blobs/sha256/<hex>), never
// manifest.json or other gate metadata. A missing blobs directory means the
// export carried no file content, so the marker is absent by construction.
func blobsContainMarker(exportDir, marker string) (bool, error) {
	blobs := filepath.Join(exportDir, "blobs")
	if _, err := os.Stat(blobs); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return dirContainsMarker(blobs, marker)
}

// dirContainsMarker reports whether any file under dir contains the marker.
func dirContainsMarker(dir, marker string) (bool, error) {
	want := []byte(marker)
	found := false
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		f, err := os.Open(p) //nolint:gosec // walking the suite-owned verified output
		if err != nil {
			return err
		}
		defer f.Close() //nolint:errcheck // read-only scan
		scan := &markerScanWriter{marker: want}
		if _, err := io.Copy(scan, f); err != nil {
			return err
		}
		if scan.found {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found, err
}
