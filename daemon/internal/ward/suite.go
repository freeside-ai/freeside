package ward

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	b            *Backend
	fx           SuiteFixture
	agentCommand []string
}

// SuiteFixture parameterizes the synthetic handoff and probes with inert,
// digest-pinned fixture inputs. The daemon supplies its pinned project base
// image; the suite owns the fixed marker-gated benign writer payload. The
// reference-runtime test supplies the spike's Alpine image.
type SuiteFixture struct {
	// AgentImage is the digest-pinned benign writer image the synthetic
	// handoff and the probes run. Trust binds to bytes: a tag is refused.
	AgentImage string
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
	// writerResultPath is the workspace-relative file the suite writer writes
	// its run-unique sentinel to; Full matches this manifest entry's digest to
	// prove this run's writer produced the export.
	writerResultPath = "result.txt"
	// workspaceStatePayload is the fixed content the suite writer puts in a
	// nested workspace file (durable-directory-tree coverage). Named so the
	// marker-collision guard can reject a marker that is a substring of it.
	workspaceStatePayload = "durable-workspace"
	// workspaceStateFile is the workspace-relative path of that nested file.
	// Full asserts the suite writer's export carries only writerResultPath
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

// withDefaults fills unset fixture fields.
func (fx SuiteFixture) withDefaults() SuiteFixture {
	if fx.CredentialTarget == "" {
		fx.CredentialTarget = "/credentials"
	}
	if fx.WorkspaceSizeMB == 0 {
		fx.WorkspaceSizeMB = 64
	}
	if fx.CredentialSizeMB == 0 {
		fx.CredentialSizeMB = 8
	}
	return fx
}

// agentCommand is the suite-owned benign writer. It always gates its output on
// reading the seeded marker and emits run-bound exact content. Letting callers
// replace this command would make Full's non-vacuousness proof optional: an
// arbitrary command has no output protocol the suite can authenticate.
func (fx SuiteFixture) agentCommand(cfg Config) []string {
	token := shellQuote(path.Join(fx.CredentialTarget, credentialTokenFile))
	ws := shellQuote(cfg.WorkspaceTarget)
	return []string{
		"sh", "-c",
		// Verify the realized credential is the seeded marker before writing
		// anything: a runtime that mounted some other volume carrying a `token`
		// file, or did not realize the mount at all, aborts under set -eu.
		"set -eu; test \"$(cat " + token + ")\" = " + fx.CredentialMarker + "; " +
			// Emit this run's writer sentinel only after the marker check, so its
			// presence proves this run's writer produced the output.
			"printf '%s\\n' " + writerSentinel(fx.RunID) + " > " + ws + "/" + writerResultPath + "; " +
			"mkdir -p " + ws + "/nested; " +
			"printf '%s\\n' " + workspaceStatePayload + " > " + ws + "/" + workspaceStateFile + "; sync",
	}
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

// writerSentinel is the run-unique token the suite writer emits into the
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

// expectedWriterManifest is the exact metadata shape Full accepts from the
// suite-owned writer.
func expectedWriterManifest(runID string) export.Manifest {
	entry := func(name, content string) export.Entry {
		mode := "0644"
		size := int64(len(content))
		sum := sha256.Sum256([]byte(content))
		digest := export.Digest("sha256:" + hex.EncodeToString(sum[:]))
		return export.Entry{
			Path:   name,
			Kind:   export.EntryRegular,
			Mode:   &mode,
			Size:   &size,
			Digest: &digest,
		}
	}
	return export.Manifest{
		Version: export.ManifestVersion,
		Entries: []export.Entry{
			entry(workspaceStateFile, workspaceStatePayload+"\n"),
			entry(writerResultPath, writerSentinel(runID)+"\n"),
		},
	}
}

// expectedWriterExportMetadata serializes every relative path and metadata
// byte the conformant released layout necessarily carries. NewSuite rejects a
// marker that collides with this oracle: Full scans agent blob content and all
// released paths, but intentionally does not treat gate-generated manifest
// bytes as credential material, so a known fixed collision is an invalid
// fixture rather than evidence for or against containment.
func expectedWriterExportMetadata(runID string) ([]byte, error) {
	manifest := expectedWriterManifest(runID)
	raw, err := json.Marshal(manifest)
	if err != nil {
		return nil, err
	}
	var metadata bytes.Buffer
	metadata.Write(raw)
	for _, name := range []string{"manifest.json", "blobs", "blobs/sha256"} {
		metadata.WriteByte('\n')
		metadata.WriteString(name)
	}
	for _, entry := range manifest.Entries {
		if entry.Digest == nil {
			continue
		}
		metadata.WriteByte('\n')
		metadata.WriteString("blobs/sha256/")
		metadata.WriteString(strings.TrimPrefix(string(*entry.Digest), "sha256:"))
	}
	return metadata.Bytes(), nil
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
	fx = fx.withDefaults()
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
	// The suite-owned writer injects the run's writer sentinel and fixed state
	// payload into the scanned workspace, and its two output paths appear in the
	// export manifest metadata (which the default path does not scan for the
	// marker, precisely to avoid this collision). A marker that is a substring of
	// any of these four would make the suite's own output indistinguishable from
	// a leak. Reject such a fixture up front.
	for _, reserved := range []string{writerSentinel(fx.RunID), workspaceStatePayload, writerResultPath, workspaceStateFile} {
		if strings.Contains(reserved, fx.CredentialMarker) {
			return nil, fmt.Errorf("%w: SuiteFixture.CredentialMarker %q collides with the generated suite string %q", ErrInvalidConfig, fx.CredentialMarker, reserved)
		}
	}
	metadata, err := expectedWriterExportMetadata(fx.RunID)
	if err != nil {
		return nil, fmt.Errorf("%w: encode expected writer export metadata: %w", ErrInvalidConfig, err)
	}
	if bytes.Contains(metadata, []byte(fx.CredentialMarker)) {
		return nil, fmt.Errorf("%w: SuiteFixture.CredentialMarker %q collides with generated export metadata", ErrInvalidConfig, fx.CredentialMarker)
	}
	return &Suite{b: b, fx: fx, agentCommand: fx.agentCommand(b.cfg)}, nil
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

// suiteRun carries one invocation's unpredictable ownership evidence. Suite
// names are deterministic for auditability, so a create collision, ambiguous
// create, concurrent invocation, or same-name replacement must never turn a
// deferred reap into authority over someone else's object. This mirrors the
// handoff gate's label-plus-creation-fingerprint rule at the smaller probe
// surface.
type suiteRun struct {
	s              *Suite
	ownershipLabel Label
	containers     map[string]objectClaim
	volumes        map[string]objectClaim
}

func (s *Suite) newRun() (*suiteRun, error) {
	owner, err := newOwnershipLabel()
	if err != nil {
		return nil, fmt.Errorf("conformance: %w", err)
	}
	return &suiteRun{
		s:              s,
		ownershipLabel: owner,
		containers:     make(map[string]objectClaim),
		volumes:        make(map[string]objectClaim),
	}, nil
}

func (r *suiteRun) labels() []Label {
	return append(runLabels(r.s.fx.RunID), r.ownershipLabel)
}

func (r *suiteRun) createVolume(ctx context.Context, name string, sizeMB int64) error {
	claim := objectClaim{attempted: true}
	r.volumes[name] = claim
	if err := r.s.b.rt.CreateVolume(ctx, name, sizeMB, slices.Clone(r.labels())); err != nil {
		return err
	}
	claim.owned = true
	r.volumes[name] = claim
	view, err := r.s.b.rt.InspectVolume(ctx, name)
	if err != nil {
		return fmt.Errorf("observe volume identity: %w", err)
	}
	if view.Name != name {
		return errors.New("volume observation returned the wrong identity")
	}
	claim.fingerprint, err = ownedFingerprint(view.CreationDate, view.Labels, view.LabelsObserved, r.ownershipLabel)
	if err != nil {
		return fmt.Errorf("volume %q: %w", name, err)
	}
	r.volumes[name] = claim
	return nil
}

func (r *suiteRun) createContainer(ctx context.Context, spec ContainerSpec) (ContainerSpec, error) {
	spec.Labels = r.labels()
	claim := objectClaim{attempted: true}
	r.containers[spec.Name] = claim
	if err := r.s.b.rt.CreateContainer(ctx, cloneContainerSpec(spec)); err != nil {
		return spec, err
	}
	claim.owned = true
	r.containers[spec.Name] = claim
	return spec, nil
}

// observeContainer binds a successful create to the immediately observed
// object. The ownership token is mandatory; CreationDate is an additional
// replacement veto when the runtime exposes it.
func (r *suiteRun) observeContainer(name string, rep InspectReport) error {
	if rep.ID != name {
		return errors.New("inspection identified the wrong container")
	}
	claim, ok := r.containers[name]
	if !ok || !claim.owned {
		return errors.New("container observation has no successful create claim")
	}
	fingerprint, err := ownedFingerprint(rep.CreationDate, rep.Labels, rep.LabelsObserved, r.ownershipLabel)
	if err != nil {
		return err
	}
	claim.fingerprint = fingerprint
	r.containers[name] = claim
	return nil
}

func (r *suiteRun) verifyContainerObservation(name string, rep InspectReport) error {
	if rep.ID != name {
		return errors.New("inspection identified the wrong container")
	}
	switch classifyEvidence(r.containers[name], r.ownershipLabel, rep.CreationDate, rep.Labels, rep.LabelsObserved) {
	case evidenceOurs:
		return nil
	case evidenceForeign:
		return errors.New("inspection identified a same-name replacement")
	case evidenceUnprovable:
		return errors.New("inspection could not prove this invocation owns the container")
	}
	return errors.New("invalid ownership evidence")
}

// reapContainer best-effort reaps only a freshly proven object owned by this
// invocation. It is safe to register before create: an already-existing
// foreign object lacks the unpredictable token and is left untouched.
func (r *suiteRun) reapContainer(ctx context.Context, name string) {
	claim, ok := r.containers[name]
	if !ok || !claim.attempted {
		return
	}
	cctx, cancel := r.s.cleanupContext(ctx)
	defer cancel()
	rep, err := r.s.b.rt.Inspect(cctx, name)
	if err == nil && rep.ID == name {
		if classifyEvidence(claim, r.ownershipLabel, rep.CreationDate, rep.Labels, rep.LabelsObserved) == evidenceOurs {
			_ = r.s.b.reapContainer(cctx, ContainerSummary{ID: name, State: rep.State})
		}
		return
	}
	ctrs, lerr := r.s.b.rt.ListContainers(cctx)
	if lerr != nil {
		return
	}
	candidate, found, ferr := uniqueContainer(ctrs, name)
	if ferr != nil || !found {
		return
	}
	ev, eerr := r.s.b.containerEvidence(cctx, candidate, claim, r.ownershipLabel)
	if eerr == nil && ev == evidenceOurs {
		_ = r.s.b.reapContainer(cctx, candidate)
	}
}

func (r *suiteRun) reapVolume(ctx context.Context, name string) {
	claim, ok := r.volumes[name]
	if !ok || !claim.attempted {
		return
	}
	cctx, cancel := r.s.cleanupContext(ctx)
	defer cancel()
	view, err := r.s.b.rt.InspectVolume(cctx, name)
	if err == nil && view.Name == name {
		if classifyEvidence(claim, r.ownershipLabel, view.CreationDate, view.Labels, view.LabelsObserved) == evidenceOurs {
			_ = r.s.b.rt.DeleteVolume(cctx, name)
		}
		return
	}
	vols, lerr := r.s.b.rt.ListVolumes(cctx)
	if lerr != nil {
		return
	}
	candidate, found, ferr := uniqueVolume(vols, name)
	if ferr != nil || !found {
		return
	}
	ev, eerr := r.s.b.volumeEvidence(cctx, candidate, claim, r.ownershipLabel)
	if eerr == nil && ev == evidenceOurs {
		_ = r.s.b.rt.DeleteVolume(cctx, name)
	}
}

// verifyReaped is the fail-closed cleanup gate. It pairs direct inspection of
// each claimed name with complete listings, so a runtime that omits a live
// object from one listing cannot make a lying delete pass. A fresh foreign
// replacement counts as absence of this invocation's object and is never
// removed.
func (r *suiteRun) verifyReaped(ctx context.Context) error {
	cctx, cancel := r.s.cleanupContext(ctx)
	defer cancel()
	ctrs, err := r.s.b.rt.ListContainers(cctx)
	if err != nil {
		return failf(CheckTeardown, "verify suite containers reaped: %v", err)
	}
	for name, claim := range r.containers {
		if !claim.attempted {
			continue
		}
		if rep, ierr := r.s.b.rt.Inspect(cctx, name); ierr == nil {
			if rep.ID != name {
				return failf(CheckTeardown, "verify suite container %q: inspect returned the wrong identity", name)
			}
			switch classifyEvidence(claim, r.ownershipLabel, rep.CreationDate, rep.Labels, rep.LabelsObserved) {
			case evidenceOurs:
				return failf(CheckTeardown, "suite container %q survived cleanup", name)
			case evidenceForeign:
				continue
			case evidenceUnprovable:
				return failf(CheckTeardown, "suite container %q ownership is unprovable after cleanup", name)
			}
		}
		candidate, found, ferr := uniqueContainer(ctrs, name)
		if ferr != nil {
			return failf(CheckTeardown, "verify suite container %q: %v", name, ferr)
		}
		if found {
			ev, eerr := r.s.b.containerEvidence(cctx, candidate, claim, r.ownershipLabel)
			if eerr != nil || ev != evidenceForeign {
				return failf(CheckTeardown, "suite container %q survived cleanup or has unprovable ownership", name)
			}
		}
	}
	vols, err := r.s.b.rt.ListVolumes(cctx)
	if err != nil {
		return failf(CheckTeardown, "verify suite volumes reaped: %v", err)
	}
	for name, claim := range r.volumes {
		if !claim.attempted {
			continue
		}
		if view, ierr := r.s.b.rt.InspectVolume(cctx, name); ierr == nil {
			if view.Name != name {
				return failf(CheckTeardown, "verify suite volume %q: inspect returned the wrong identity", name)
			}
			switch classifyEvidence(claim, r.ownershipLabel, view.CreationDate, view.Labels, view.LabelsObserved) {
			case evidenceOurs:
				return failf(CheckTeardown, "suite volume %q survived cleanup", name)
			case evidenceForeign:
				continue
			case evidenceUnprovable:
				return failf(CheckTeardown, "suite volume %q ownership is unprovable after cleanup", name)
			}
		}
		candidate, found, ferr := uniqueVolume(vols, name)
		if ferr != nil {
			return failf(CheckTeardown, "verify suite volume %q: %v", name, ferr)
		}
		if found {
			ev, eerr := r.s.b.volumeEvidence(cctx, candidate, claim, r.ownershipLabel)
			if eerr != nil || ev != evidenceForeign {
				return failf(CheckTeardown, "suite volume %q survived cleanup or has unprovable ownership", name)
			}
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
	run, err := s.newRun()
	if err != nil {
		return err
	}
	// Fail closed on a surviving object: registered before the reaps so it
	// runs after them (LIFO), turning an otherwise-clean run whose cleanup
	// left something behind into a failure. Joined with any primary error
	// (as the handoff teardown does) so a leak is still surfaced even when a
	// probe or check already failed, never suppressed by it.
	defer func() {
		if verr := run.verifyReaped(ctx); verr != nil {
			err = errors.Join(err, verr)
		}
	}()

	credVolume := s.conformanceName("cred")

	// The credential volume is the suite's own object (unlike a real handoff,
	// where the caller owns it): create, seed, and always reap it here. The
	// reap is registered before the create so an ambiguous create (volume
	// made, call errored) is still reaped.
	defer run.reapVolume(ctx, credVolume)
	if err := run.createVolume(ctx, credVolume, s.fx.CredentialSizeMB); err != nil {
		return fmt.Errorf("conformance: create credential volume: %w", err)
	}

	if err := s.seedCredential(ctx, run, credVolume); err != nil {
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
			Command:          s.agentCommand,
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
	// Prove this run's suite-owned writer actually produced the
	// export, not just that some content is present: the writer writes exactly
	// the run-unique sentinel line to result.txt only after verifying the seeded
	// credential. Match the manifest's own digest for that path — which
	// verifyExport already confirmed the blob content hashes to — rather than
	// scanning the export tree, so a stale file merely NAMED like the sentinel
	// (which would appear as a path in manifest.json) cannot satisfy the proof,
	// and the check binds to the exact expected content at the exact path.
	if !manifestHasContent(res.Manifest.Entries, writerResultPath, writerSentinel(s.fx.RunID)+"\n") {
		return failf(CheckCredentialContainment, "export does not carry this run's writer sentinel at %s; containment cannot be proven", writerResultPath)
	}
	// Prove the export carries only this run's writer output: blobsContainMarker
	// scans blob CONTENT only, so a runtime could otherwise exfiltrate the marker
	// as a filename (a manifest path) or a symlink target — neither becomes a
	// scanned blob, and the §5.4 scanner is blind to the inert marker. The
	// suite writer produces exactly result.txt and the nested state file as
	// regular files; reject any other entry or non-regular kind.
	for _, e := range res.Manifest.Entries {
		if e.Kind != export.EntryRegular || (e.Path != writerResultPath && e.Path != workspaceStateFile) {
			return failf(CheckCredentialContainment, "export carries an unexpected manifest entry %q (kind %q); the suite writer produces only %q and %q, so a filename or symlink target could smuggle the credential past the content scan", e.Path, e.Kind, writerResultPath, workspaceStateFile)
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
	found, err = dirMetadataContainsMarker(res.ExportDir, s.fx.CredentialMarker)
	if err != nil {
		return failf(CheckCredentialContainment, "scan released export paths for credential marker: %v", err)
	}
	if found {
		return failf(CheckCredentialContainment, "credential marker present in released export path metadata")
	}
	// The suite writer also writes a nested workspace file, so the deterministic
	// directory tree must survive the export: a lossy exporter that drops nested
	// content while preserving result.txt would otherwise pass every check above.
	// Require the nested fixture's exact content too (verifyExport confirmed the
	// digest), completing "the export is exactly this run's writer output".
	if !manifestHasContent(res.Manifest.Entries, workspaceStateFile, workspaceStatePayload+"\n") {
		return failf(CheckCredentialContainment, "export does not carry this run's nested workspace fixture at %s; the durable directory tree was not proven to survive", workspaceStateFile)
	}

	// Containment, detached-volume half: the marker is still readable from the
	// credential volume, proving absence from the export was mount omission,
	// not deletion.
	if err := s.probeCredentialContainment(ctx, run, credVolume); err != nil {
		return err
	}

	// The read-write-attach exclusion the gate's check-3 termination depends
	// on: a second VM cannot attach a volume a live writer holds read-write.
	return s.probeWriterVolumeExclusion(ctx, run)
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
	run, err := s.newRun()
	if err != nil {
		return err
	}
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
		if verr := run.verifyReaped(ctx); verr != nil {
			err = errors.Join(err, verr)
		}
	}()
	defer run.reapContainer(ctx, name)
	return s.proveNoEagerStart(ctx, run, name, CheckPreJobProbe)
}

// proveNoEagerStart creates a throwaway container from the fixture image with a
// nonterminating inert payload and requires it StateStopped
// after create (create realizes metadata only; a runtime that eager-started it
// would keep the payload running past the inspect — a short-lived "true" could
// exit first and self-mask as stopped), then deletes it. This is PreJob's core
// liveness check. Full needs no surrogate: the handoff gate now asserts the
// real writer and exporter are StateStopped in their own pre-start allowlist
// inspections. The caller registers the reap/absence-proof deferrals for name.
func (s *Suite) proveNoEagerStart(ctx context.Context, run *suiteRun, name string, check Check) error {
	spec := ContainerSpec{
		Name:  name,
		Image: s.fx.AgentImage,
		// A finite sleep shorter than the enclosing timeout can self-mask a
		// synchronous eager start: CreateContainer returns after it exits and the
		// inspect sees stopped. This payload never terminates by itself, so an
		// eager create can return only by respecting context cancellation.
		Command: []string{"sh", "-c", "while :; do sleep 3600; done"},
	}
	var err error
	spec, err = run.createContainer(ctx, spec)
	if err != nil {
		return failf(check, "runtime cannot create a container: %v", err)
	}
	rep, err := s.b.rt.Inspect(ctx, name)
	if err != nil {
		return failf(check, "runtime cannot inspect a container: %v", err)
	}
	if err := run.observeContainer(name, rep); err != nil {
		return failf(check, "liveness inspection could not bind the created container: %v", err)
	}
	// The stopped-state proof is only meaningful if the container realized the
	// probe's spec: a runtime that dropped the mounts or changed the long-lived
	// command could report stopped without ever exercising the mounted, running
	// create path this probe stands in for. Confirm the realized image, command,
	// and mounts match before trusting the state.
	if !probeSpecMatches(rep, spec) {
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

// probeSpecMatches verifies every realized process/configuration surface the
// probe depends on. Mount-only comparison is insufficient for the credential
// audit: a runtime could preserve the credential mount while replacing the
// marker-gated command with an unconditional sentinel write.
func probeSpecMatches(rep InspectReport, spec ContainerSpec) bool {
	expectedEnv := append([]string{fixedContainerPathEnv}, spec.Env...)
	return rep.ID == spec.Name &&
		rep.AllowlistFieldsObserved &&
		sameImage(spec.Image, rep.ImageReference) &&
		rep.WorkingDirectory == "/" &&
		slices.Equal(rep.Command, spec.Command) &&
		slices.Equal(rep.Env, expectedEnv) &&
		sameMounts(rep.Mounts, spec.Mounts) &&
		!rep.SSH && len(rep.PublishedSockets) == 0 && len(rep.PublishedPorts) == 0
}

// seedCredential writes the fake marker into the credential volume through a
// throwaway seed container, then reaps it. The marker lands at
// CredentialTarget/token, where the writer and audit containers read it.
func (s *Suite) seedCredential(ctx context.Context, run *suiteRun, credVolume string) error {
	name := s.conformanceName("seed")
	token := shellQuote(path.Join(s.fx.CredentialTarget, credentialTokenFile))
	spec := ContainerSpec{
		Name:    name,
		Image:   s.fx.AgentImage,
		Command: []string{"sh", "-c", "printf '%s\\n' " + s.fx.CredentialMarker + " > " + token + "; sync"},
		Mounts:  []Mount{{Type: MountVolume, Source: credVolume, Target: s.fx.CredentialTarget}},
	}
	// Reap (stop then delete) is registered before the create so an ambiguous
	// create is reaped, and so a seed that starts but never stops is stopped
	// before deletion (a delete-only reap would leave the running VM holding
	// the credential volume).
	defer run.reapContainer(ctx, name)
	var err error
	spec, err = run.createContainer(ctx, spec)
	if err != nil {
		return fmt.Errorf("conformance: create credential seed: %w", err)
	}
	rep, err := s.b.rt.Inspect(ctx, name)
	if err != nil {
		return fmt.Errorf("conformance: inspect credential seed: %w", err)
	}
	if err := run.observeContainer(name, rep); err != nil {
		return fmt.Errorf("conformance: bind credential seed identity: %w", err)
	}
	if !probeSpecMatches(rep, spec) || rep.State != StateStopped {
		return failf(CheckCredentialContainment, "credential seed did not realize the stopped suite-owned probe spec")
	}
	if err := s.b.rt.StartContainer(ctx, name); err != nil {
		return fmt.Errorf("conformance: start credential seed: %w", err)
	}
	if err := s.b.waitStopped(ctx, name, run.containers[name], run.ownershipLabel, probeStopTimeout); err != nil {
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
func (s *Suite) probeCredentialContainment(ctx context.Context, run *suiteRun, credVolume string) error {
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
	}
	// Reap before create (ambiguous-create safe) and stop-then-delete (an
	// audit that never stops would otherwise survive a delete-only reap).
	defer run.reapContainer(ctx, name)
	var err error
	spec, err = run.createContainer(ctx, spec)
	if err != nil {
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
	if err := run.observeContainer(name, arep); err != nil {
		return failf(CheckCredentialContainment, "credential-audit container identity is unproven: %v", err)
	}
	if !probeSpecMatches(arep, spec) || arep.State != StateStopped {
		return failf(CheckCredentialContainment, "credential-audit container did not realize the stopped suite-owned probe spec")
	}
	if err := s.b.rt.StartContainer(ctx, name); err != nil {
		return failf(CheckCredentialContainment, "start credential-audit container: %v", err)
	}
	if err := s.b.waitStopped(ctx, name, run.containers[name], run.ownershipLabel, probeStopTimeout); err != nil {
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
	// Full drain an oversized audit export. The result must be a valid tar with
	// exactly the expected sentinel file and content: a raw byte search would
	// accept a malformed archive, a tar header, or an unrelated file.
	archive, err := os.CreateTemp("", "freeside-ward-audit-"+s.fx.RunID+"-*.tar")
	if err != nil {
		return failf(CheckCredentialContainment, "create credential-audit archive: %v", err)
	}
	archivePath := archive.Name()
	defer func() {
		_ = archive.Close()
		_ = os.Remove(archivePath)
	}()
	capped := &archiveCapWriter{dest: archive, remaining: s.b.cfg.MaxArchiveBytes}
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
	if err := archive.Close(); err != nil {
		return failf(CheckCredentialContainment, "close credential-audit rootfs archive: %v", err)
	}
	archive, err = os.Open(archivePath) //nolint:gosec // suite-generated temp path
	if err != nil {
		return failf(CheckCredentialContainment, "open credential-audit rootfs archive: %v", err)
	}
	found, err := auditArchiveHasSentinel(archive, auditMarkerPath(s.fx.RunID), auditSentinel(s.fx.RunID)+"\n")
	if err != nil {
		return failf(CheckCredentialContainment, "validate credential-audit rootfs archive: %v", err)
	}
	if !found {
		return failf(CheckCredentialContainment, "credential marker not readable from the detached credential volume")
	}
	return nil
}

// auditArchiveHasSentinel validates the audit rootfs tar and binds the proof to
// one regular file at the suite-generated path with exact run-bound content.
// It parses through EOF even after finding the file, so trailing corruption or
// a duplicate contradictory entry fails closed.
func auditArchiveHasSentinel(r io.Reader, absolutePath, content string) (bool, error) {
	wantPath := strings.TrimPrefix(absolutePath, "/")
	wantContent := []byte(content)
	found := false
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return found, nil
		}
		if err != nil {
			return false, fmt.Errorf("read tar: %w", err)
		}
		if len(hdr.Name) > maxArchivePathBytes {
			return false, errors.New("archive entry path exceeds the length cap")
		}
		name := path.Clean(strings.TrimPrefix(hdr.Name, "./"))
		if strings.HasPrefix(name, "/") || name == ".." || strings.HasPrefix(name, "../") {
			return false, errors.New("archive entry escapes the archive root")
		}
		if name != wantPath {
			continue
		}
		if found {
			return false, errors.New("archive carries more than one audit sentinel entry")
		}
		found = true
		if hdr.Typeflag != tar.TypeReg {
			return false, errors.New("audit sentinel entry is not a regular file")
		}
		if hdr.Size != int64(len(wantContent)) {
			return false, errors.New("audit sentinel entry has unexpected size")
		}
		got, err := io.ReadAll(io.LimitReader(tr, int64(len(wantContent))+1))
		if err != nil {
			return false, fmt.Errorf("read audit sentinel entry: %w", err)
		}
		if !bytes.Equal(got, wantContent) {
			return false, errors.New("audit sentinel entry has unexpected content")
		}
	}
}

// probeWriterVolumeExclusion is the first negative probe: while a live writer
// holds a fresh workspace volume read-write, an exporter-shaped second
// container attaching the same volume read-only must be refused by the runtime
// (Virtualization.framework
// VZErrorDomain Code=2 at bootstrap). A successful second attach means the
// exclusion check 3's writer-termination requirement depends on does not
// hold, so the gate could export a workspace another VM still holds.
func (s *Suite) probeWriterVolumeExclusion(ctx context.Context, run *suiteRun) error {
	volume := s.conformanceName("excl-ws")
	writer := s.conformanceName("excl-writer")
	second := s.conformanceName("excl-second")

	// Reaps registered before any create so an ambiguous create is reaped;
	// LIFO runs them in reverse dependency order (writer, then second, then
	// the volume they held). All best-effort under a detached context.
	defer run.reapVolume(ctx, volume)
	defer run.reapContainer(ctx, second)
	defer run.reapContainer(ctx, writer)

	if err := run.createVolume(ctx, volume, s.fx.WorkspaceSizeMB); err != nil {
		return failf(CheckWriterVolumeExclusion, "create exclusion workspace volume: %v", err)
	}

	// A long-lived writer holds the volume read-write and stays live while the
	// second attach is attempted.
	writerSpec := ContainerSpec{
		Name:    writer,
		Image:   s.fx.AgentImage,
		Command: []string{"sh", "-c", "while :; do sleep 3600; done"},
		Mounts:  []Mount{{Type: MountVolume, Source: volume, Target: s.b.cfg.WorkspaceTarget}},
	}
	var err error
	writerSpec, err = run.createContainer(ctx, writerSpec)
	if err != nil {
		return failf(CheckWriterVolumeExclusion, "create exclusion writer: %v", err)
	}
	// Bind and verify the exact stopped object before starting by name. Without
	// this pre-start trust gate, a same-name replacement between create and
	// start could make the suite execute a foreign container. The nonterminating
	// payload also prevents a synchronous eager create from finishing and
	// self-masking as stopped.
	wrep, err := s.b.rt.Inspect(ctx, writer)
	if err != nil {
		return failf(CheckWriterVolumeExclusion, "inspect stopped exclusion writer: %v", err)
	}
	if err := run.observeContainer(writer, wrep); err != nil {
		return failf(CheckWriterVolumeExclusion, "exclusion writer identity is unproven before start: %v", err)
	}
	if !probeSpecMatches(wrep, writerSpec) || wrep.State != StateStopped {
		return failf(CheckWriterVolumeExclusion, "exclusion writer did not realize the stopped suite-owned writer spec")
	}
	if err := s.b.rt.StartContainer(ctx, writer); err != nil {
		return failf(CheckWriterVolumeExclusion, "start exclusion writer: %v", err)
	}
	// The exclusion is only meaningful while the writer is actually live and
	// holding the volume; a writer that never booted would let the second
	// attach succeed for the wrong reason. Confirm it is observed running.
	wrep, err = s.b.rt.Inspect(ctx, writer)
	if err != nil {
		return failf(CheckWriterVolumeExclusion, "inspect exclusion writer: %v", err)
	}
	if err := run.verifyContainerObservation(writer, wrep); err != nil {
		return failf(CheckWriterVolumeExclusion, "exclusion writer ownership changed after start: %v", err)
	}
	if !probeSpecMatches(wrep, writerSpec) || wrep.State != StateRunning {
		return failf(CheckWriterVolumeExclusion, "exclusion writer not observed running before the second attach")
	}

	// The writer now holds the volume rw. A second attach must be refused. The
	// spike observed the refusal at VM bootstrap (start), so create should
	// succeed and start must fail; a create failure is inconclusive
	// (unexpected, since the writer created fine with the same spec shape) and
	// fails closed rather than counting as the exclusion.
	secondSpec := ContainerSpec{
		Name:    second,
		Image:   s.b.cfg.ExporterImage,
		Command: slices.Clone(s.b.cfg.ExporterCommand),
		Mounts:  []Mount{{Type: MountVolume, Source: volume, Target: s.b.cfg.WorkspaceTarget, ReadOnly: true}},
	}
	secondSpec, err = run.createContainer(ctx, secondSpec)
	if err != nil {
		return failf(CheckWriterVolumeExclusion, "create exclusion second container: %v", err)
	}
	// Confirm the second realized the intended read-only exporter mount before
	// start, so a start refusal is about the writer-held volume and not some other
	// mount the runtime dropped or changed.
	srep, err := s.b.rt.Inspect(ctx, second)
	if err != nil {
		return failf(CheckWriterVolumeExclusion, "inspect exclusion second container: %v", err)
	}
	if err := run.observeContainer(second, srep); err != nil {
		return failf(CheckWriterVolumeExclusion, "exclusion second-container identity is unproven: %v", err)
	}
	if !probeSpecMatches(srep, secondSpec) || srep.State != StateStopped {
		return failf(CheckWriterVolumeExclusion, "exclusion second container did not realize the stopped exporter-shaped probe spec")
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
	if err := run.verifyContainerObservation(second, srep2); err != nil {
		return failf(CheckWriterVolumeExclusion, "second-container ownership changed after the attachment error: %v", err)
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
	if !probeSpecMatches(wrep2, writerSpec) || wrep2.State != StateRunning {
		return failf(CheckWriterVolumeExclusion, "exclusion writer no longer holds the read-write mount after the refusal; the runtime may have resolved the conflict by evicting the holder")
	}
	if err := run.verifyContainerObservation(writer, wrep2); err != nil {
		return failf(CheckWriterVolumeExclusion, "exclusion-writer ownership changed after the attachment error: %v", err)
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
// present it is authoritative and must be exactly 2. A code-less prose error
// is inconclusive: other storage-device failures can use the same words, so
// only the reference runtime's observed discriminator proves exclusion.
func isAttachmentExclusion(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "attachment") {
		return false
	}
	if !strings.Contains(msg, "storage device") {
		return false
	}
	// The first/top-level VZErrorDomain code must be exactly 2.
	if m := vzDomainCodePattern.FindStringSubmatch(msg); m != nil {
		return m[1] == "2"
	}
	return false
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

// dirMetadataContainsMarker scans every released relative path, including the
// fixed manifest/blob layout outside the agent-authored manifest. The host
// temp directory itself is excluded. WalkDir does not follow symlinks, so an
// entry cannot redirect this metadata-only traversal.
func dirMetadataContainsMarker(dir, marker string) (bool, error) {
	want := []byte(marker)
	found := false
	err := filepath.WalkDir(dir, func(p string, _ os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if p == dir {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		if bytes.Contains([]byte(filepath.ToSlash(rel)), want) {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found, err
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
		scan := &markerScanWriter{marker: want}
		_, copyErr := io.Copy(scan, f)
		closeErr := f.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		if scan.found {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found, err
}
