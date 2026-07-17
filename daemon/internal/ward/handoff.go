package ward

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/export"
)

// requiredCapabilities is the gate's own admission floor: the lifecycle
// below is meaningless on a runtime that cannot detach a workspace, remount
// it read-only, and export a stopped rootfs.
var requiredCapabilities = []exec.Capability{
	exec.CapDetachableWorkspace,
	exec.CapPostExitExport,
	exec.CapReadOnlyRemount,
}

// HandoffResult is a completed, gate-passed handoff.
type HandoffResult struct {
	// Admission is the spawn-time capability snapshot the run was admitted
	// under (§5.3); audit binds to it.
	Admission exec.Admission
	// ExportDir holds the verified manifest and blobs. The caller owns the
	// directory and removes it when done.
	ExportDir string
	// Manifest is the decoded, digest-verified §5.6 manifest.
	Manifest export.Manifest
}

// objectClaim is one runtime object's ownership state. attempted records
// that its create was called (an ambiguous error after that point may have
// made the object); owned records the create's success. fingerprint is the
// creation instant observed together with this invocation's ownership label
// right after a successful create: it binds the claim to the one object the
// run made, so cleanup can tell it from a later same-name replacement. It
// stays "" until that observation succeeds, or when the runtime reports no
// creation instant, and cleanup then degrades to fresh label evidence.
type objectClaim struct {
	attempted   bool
	owned       bool
	fingerprint string
}

// runState tracks host-temp directories and each runtime object's ownership
// state so deferred cleanup can remove only this invocation's objects. A
// successful create proves ownership of the exact object observed after it,
// never of whatever later holds the name; an ambiguous create must be
// resolved from a fresh runtime listing and this invocation's unpredictable
// label. On an ambiguous workspace create error, teardown may reap only a
// workspace carrying this invocation's unpredictable ownershipLabel; an
// ordinary already-exists collision does not carry it and is left untouched.
type runState struct {
	ownershipLabel Label
	workspace      objectClaim
	agent          objectClaim
	exporter       objectClaim
	// archiveDir holds the exported rootfs archive; always removed once
	// verification is done or the run fails (the archive is never returned).
	archiveDir string
	// exportDir holds the extracted, verified output. It is returned to the
	// caller only when the run ultimately succeeds; on any failure, including
	// a teardown failure after a good export, it is removed here (the caller
	// gets a nil result and cannot own it).
	exportDir string
}

// Handoff runs one full workspace handoff: admit against the capability
// floor, run the agent with the workspace read-write and credentials in
// their own read-only mounts (checks 1-2), prove the writer VM terminated by
// observed state (check 3), run the exporter with the workspace read-only
// behind a pre-execution mount-allowlist inspection (check 4), and verify
// the in-exporter proof, exported digests, and §5.4 scan (checks 5, 7)
// before releasing anything. Teardown runs on every exit path; a teardown
// failure fails the gate even when everything else passed.
//
// Any error means no trusted export: a *ConformanceFailure names the failed
// contract check, and any other error is an operational failure of the same
// fail-closed gate.
func (b *Backend) Handoff(ctx context.Context, hs HandoffSpec) (result *HandoffResult, err error) {
	// The request is caller-owned. Freeze its slices before they feed either
	// expected allowlists or a Runtime call.
	hs.Agent.Command = slices.Clone(hs.Agent.Command)
	hs.Agent.Env = slices.Clone(hs.Agent.Env)
	hs.Agent.CredentialMounts = slices.Clone(hs.Agent.CredentialMounts)
	if err := hs.validate(); err != nil {
		return nil, err
	}
	adm, err := exec.CheckCapabilities(b, requiredCapabilities)
	if err != nil {
		return nil, err
	}
	names := namesFor(hs.RunID)
	ownershipLabel, err := newOwnershipLabel()
	if err != nil {
		return nil, err
	}
	// Bound the whole handoff so a runtime that wedges inside a side-effecting
	// call (e.g. after launching the credential VM but before StartContainer
	// returns) cannot block the gate, and the VM, indefinitely: the
	// per-operation waits only begin once their own call returns. Every runtime
	// call below derives from this ctx; teardown re-detaches (WithoutCancel) so
	// it still reaps what the budget interrupts. Registered before the teardown
	// defer so cancel runs after teardown on unwind.
	ctx, cancel := context.WithTimeout(ctx, b.cfg.HandoffTimeout)
	defer cancel()
	st := &runState{ownershipLabel: ownershipLabel}
	defer func() {
		terr := b.teardown(ctx, names, st)
		if terr != nil {
			result = nil
			if err == nil {
				err = terr
			} else {
				err = errors.Join(err, terr)
			}
		}
		// The archive is transient once verified; the output dir is removed on
		// any failure (including a teardown failure that nils an otherwise
		// good result) and kept only when the run fully succeeds, since then
		// the caller owns it. Both are best-effort host-temp cleanup.
		if st.archiveDir != "" {
			_ = os.RemoveAll(st.archiveDir)
		}
		if err != nil && st.exportDir != "" {
			_ = os.RemoveAll(st.exportDir)
		}
	}()

	// Checks 1-2: the generated writer spec is re-verified, not trusted.
	agentSpec := buildAgentSpec(b.cfg, hs, names, ownershipLabel)
	if err := validateAgentSpec(b.cfg, agentSpec, names.Workspace); err != nil {
		return nil, err
	}

	// A successful workspace create establishes ownership of this workspace.
	// If the call fails after creating the volume, teardown can still identify
	// that one object by its per-invocation ownership label; an ordinary
	// already-exists failure cannot authorize reaping another run.
	st.workspace.attempted = true
	volumeLabels := append(runLabels(hs.RunID), ownershipLabel)
	if err := b.rt.CreateVolume(ctx, names.Workspace, hs.WorkspaceSizeMB, slices.Clone(volumeLabels)); err != nil {
		return nil, fmt.Errorf("create workspace volume: %w", err)
	}
	st.workspace.owned = true
	// Bind the claim to the one volume just made: a failed observation fails
	// the run and leaves the claim fingerprintless, degrading cleanup to
	// fresh label evidence rather than name-wide authority.
	wsView, err := b.rt.InspectVolume(ctx, names.Workspace)
	if err != nil {
		return nil, fmt.Errorf("observe workspace volume identity: %w", err)
	}
	st.workspace.fingerprint, err = ownedFingerprint(wsView.CreationDate, wsView.Labels, wsView.LabelsObserved, ownershipLabel)
	if err != nil {
		return nil, fmt.Errorf("workspace volume %q: %w", names.Workspace, err)
	}

	st.agent.attempted = true
	if err := b.rt.CreateContainer(ctx, cloneContainerSpec(agentSpec)); err != nil {
		return nil, fmt.Errorf("create agent container: %w", err)
	}
	st.agent.owned = true
	agentRep, err := b.rt.Inspect(ctx, names.Agent)
	if err != nil {
		return nil, failf(CheckControlPlaneIsolation, "inspect agent before execution: %v", err)
	}
	st.agent.fingerprint, err = ownedFingerprint(agentRep.CreationDate, agentRep.Labels, agentRep.LabelsObserved, ownershipLabel)
	if err != nil {
		return nil, failf(CheckControlPlaneIsolation, "agent container %q: %v", names.Agent, err)
	}
	if err := verifyAgentAllowlist(agentRep, agentSpec); err != nil {
		return nil, err
	}
	if err := b.rt.StartContainer(ctx, names.Agent); err != nil {
		return nil, fmt.Errorf("start agent container: %w", err)
	}

	// Check 3: writer termination is observed state, never scheduling
	// intent (a second VM cannot attach a volume a live VM holds rw; only
	// observed "stopped" proves the attachment is gone).
	if err := b.waitStopped(ctx, names.Agent, st.agent.fingerprint, b.cfg.WriterStopTimeout); err != nil {
		return nil, failf(CheckWriterTermination, "agent: %v", err)
	}
	if err := b.rt.DeleteContainer(ctx, names.Agent); err != nil {
		return nil, failf(CheckWriterTermination, "delete stopped agent: %v", err)
	}
	// Our own delete succeeded, so this invocation no longer owns the agent by
	// create: a later object answering to the deterministic name may be a
	// foreign recycle, not ours. Downgrade to label-gated cleanup now, before
	// proving absence, so that if verifyContainerAbsent fails on a transient
	// list error deferred teardown re-proves this invocation's ownership label
	// rather than reaping a same-name stranger by identity alone.
	st.agent.owned = false
	if err := b.verifyContainerAbsent(ctx, names.Agent, CheckWriterTermination); err != nil {
		return nil, err
	}
	st.agent = objectClaim{}

	// Check 4: create the exporter but inspect it against the generated
	// allowlist before it ever executes.
	exporterSpec := buildExporterSpec(b.cfg, hs, names, ownershipLabel)
	st.exporter.attempted = true
	if err := b.rt.CreateContainer(ctx, cloneContainerSpec(exporterSpec)); err != nil {
		return nil, fmt.Errorf("create exporter container: %w", err)
	}
	st.exporter.owned = true
	rep, err := b.rt.Inspect(ctx, names.Exporter)
	if err != nil {
		return nil, failf(CheckExporterAllowlist, "inspect exporter before execution: %v", err)
	}
	st.exporter.fingerprint, err = ownedFingerprint(rep.CreationDate, rep.Labels, rep.LabelsObserved, ownershipLabel)
	if err != nil {
		return nil, failf(CheckExporterAllowlist, "exporter container %q: %v", names.Exporter, err)
	}
	if err := verifyExporterAllowlist(b.cfg, rep, names.Exporter, names.Workspace); err != nil {
		return nil, err
	}
	if err := b.rt.StartContainer(ctx, names.Exporter); err != nil {
		return nil, fmt.Errorf("start exporter container: %w", err)
	}
	if err := b.waitStopped(ctx, names.Exporter, st.exporter.fingerprint, b.cfg.ExporterTimeout); err != nil {
		return nil, failf(CheckExportVerification, "exporter: %v", err)
	}

	// Checks 5 and 7: collect the stopped exporter's rootfs and verify the
	// proof, manifest, digests, and scan before releasing anything. The
	// archive and the extracted output are separate host-temp entities so the
	// success path can hand the caller exactly the output directory with no
	// leftover parent (teardown removes the archive; the output dir is the
	// caller's once released).
	st.archiveDir, err = os.MkdirTemp("", "freeside-handoff-"+hs.RunID+"-tar-")
	if err != nil {
		return nil, fmt.Errorf("create export archive dir: %w", err)
	}
	st.exportDir, err = os.MkdirTemp("", "freeside-handoff-"+hs.RunID+"-out-")
	if err != nil {
		return nil, fmt.Errorf("create export output dir: %w", err)
	}
	tarPath := filepath.Join(st.archiveDir, "export.tar")
	if err := b.materializeRootFS(ctx, names.Exporter, tarPath); err != nil {
		return nil, err
	}
	out, err := b.verifyExport(ctx, tarPath, st.exportDir)
	if err != nil {
		return nil, err
	}
	return &HandoffResult{Admission: adm, ExportDir: out.Dir, Manifest: out.Manifest}, nil
}

var errArchiveByteCap = errors.New("archive byte cap exceeded")

type archiveCapWriter struct {
	dest      io.Writer
	remaining int64
	overflow  bool
}

func (w *archiveCapWriter) Write(p []byte) (int, error) {
	limit := len(p)
	if int64(limit) > w.remaining {
		limit = int(w.remaining)
		w.overflow = true
	}
	n, err := w.dest.Write(p[:limit])
	w.remaining -= int64(n)
	if err != nil {
		return n, err
	}
	if n != limit {
		return n, io.ErrShortWrite
	}
	if w.overflow {
		return n, errArchiveByteCap
	}
	return n, nil
}

// materializeRootFS keeps the full runtime-returned archive behind a hard
// host-side byte cap. Runtime receives only the Writer, never the scratch
// path, so an oversized or hostile stream cannot fill the archive directory
// before verification gets a chance to reject it.
func (b *Backend) materializeRootFS(ctx context.Context, id, tarPath string) error {
	f, err := os.OpenFile(tarPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // gate-owned path under a fresh temp directory
	if err != nil {
		return failf(CheckExportVerification, "create bounded rootfs archive: %v", err)
	}
	w := &archiveCapWriter{dest: f, remaining: b.cfg.MaxArchiveBytes}
	exportErr := b.rt.ExportRootFS(ctx, id, w, b.cfg.MaxArchiveBytes)
	closeErr := f.Close()
	if w.overflow {
		return failf(CheckExportVerification, "exported rootfs archive exceeds the byte cap")
	}
	if exportErr != nil {
		return failf(CheckExportVerification, "export stopped exporter rootfs: %v", exportErr)
	}
	if closeErr != nil {
		return failf(CheckExportVerification, "close bounded rootfs archive: %v", closeErr)
	}
	return nil
}

func newOwnershipLabel() (Label, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return Label{}, fmt.Errorf("generate runtime ownership token: %w", err)
	}
	return Label{Key: ownershipLabelKey, Value: hex.EncodeToString(token[:])}, nil
}

// waitStopped polls until the container is observed stopped. The wait is
// budgeted in whole poll intervals (timeout / PollInterval attempts), so
// tests with an injected Sleep are fully deterministic. Its own deadline also
// bounds each runtime call: a wedged Inspect must not defeat the named writer
// or exporter timeout when the caller context has no deadline.
// ownedFingerprint extracts a creation fingerprint from the observation made
// right after this invocation successfully created an object. The observation
// must itself carry the invocation's unpredictable ownership label: the
// create succeeded with that label, so a same-name object that cannot show it
// is contradictory evidence, possibly already a replacement, and is never
// fingerprinted as ours. An empty fingerprint from a labeled observation is
// valid (the runtime reports no creation instant) and degrades cleanup to
// fresh label evidence.
func ownedFingerprint(creationDate string, labels []Label, labelsObserved bool, ownershipLabel Label) (string, error) {
	if !labelsObserved {
		return "", errors.New("post-create observation omitted labels")
	}
	if !slices.Contains(labels, ownershipLabel) {
		return "", errors.New("post-create observation does not carry this invocation's ownership label")
	}
	return creationDate, nil
}

// waitStopped polls until the container with the claimed creation fingerprint
// is observed stopped. Every poll re-verifies the fingerprint: check 3's
// stopped observation is proof about the one VM the gate started, so a
// same-name object with a different creation instant (a replacement) can
// never satisfy it, and the delete that follows a satisfied wait always
// targets a just-verified observation.
func (b *Backend) waitStopped(ctx context.Context, id, fingerprint string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	attempts := int(timeout / b.cfg.PollInterval)
	if attempts < 1 {
		attempts = 1
	}
	last := ContainerState("never inspected")
	for i := 0; i < attempts; i++ {
		rep, err := b.rt.Inspect(ctx, id)
		if err != nil {
			return fmt.Errorf("inspect: %w", err)
		}
		if rep.ID != id {
			return fmt.Errorf("inspect returned a report for the wrong container")
		}
		if rep.CreationDate != fingerprint {
			return fmt.Errorf("inspect returned a same-name container with a different creation identity")
		}
		if rep.State == StateStopped {
			return nil
		}
		last = rep.State
		if i+1 < attempts {
			if err := b.cfg.Sleep(ctx, b.cfg.PollInterval); err != nil {
				return fmt.Errorf("wait interrupted: %w", err)
			}
		}
	}
	return fmt.Errorf("state %q after %s, never observed %q", last, timeout, StateStopped)
}

// verifyContainerAbsent proves id is gone from the runtime's full container
// list; check 3 requires absence, not a successful delete call.
func (b *Backend) verifyContainerAbsent(ctx context.Context, id string, c Check) error {
	ctrs, err := b.rt.ListContainers(ctx)
	if err != nil {
		return failf(c, "list containers to verify %q absent: %v", id, err)
	}
	if slices.ContainsFunc(ctrs, func(cs ContainerSummary) bool { return cs.ID == id }) {
		return failf(c, "container %q still listed after delete", id)
	}
	return nil
}

// teardown reaps every runtime object the run owns and proves it is gone. A
// successful create owns the exact deterministic name. After an ambiguous
// create (the object was made but the call returned an error), the exact name
// is reaped only when a fresh runtime listing also carries this invocation's
// unpredictable ownership label. The deterministic run label is inspection
// metadata, not ownership evidence: caller-owned objects may carry it.
// Teardown runs detached from the caller's cancellation so an aborted run is
// still reaped, under its own deadline so a wedged runtime call cannot hang
// Handoff.
func (b *Backend) teardown(ctx context.Context, names handoffNames, st *runState) error {
	// Before the first create attempt this invocation owns no runtime object.
	if !st.workspace.attempted {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), b.cfg.TeardownTimeout)
	defer cancel()
	var problems []string

	type containerClaim struct {
		id    string
		claim objectClaim
	}
	containerClaims := []containerClaim{
		{id: names.Agent, claim: st.agent},
		{id: names.Exporter, claim: st.exporter},
	}
	// Each container is reaped only when its own create succeeded or a fresh
	// inspect carries the unpredictable ownership label after an ambiguous
	// create error, whether that inspect comes from the teardown listing's
	// labels or, when the full list is unavailable, from a direct inspect.
	if st.agent.attempted || st.exporter.attempted {
		if ctrs, err := b.rt.ListContainers(ctx); err != nil {
			problems = append(problems, fmt.Sprintf("list containers: %v", err))
			// A full-list failure can be caused by an unrelated malformed row.
			// It must not suppress cleanup of an exact name this invocation
			// owns: a successful create is reaped by identity alone, and an
			// ambiguous create is reaped by exact name once a direct inspect
			// proves this invocation's ownership label. Otherwise one unrelated
			// broken row could leave the credential-mounted writer restartable.
			for _, c := range containerClaims {
				if !c.claim.attempted {
					continue
				}
				if c.claim.owned {
					if rerr := b.reapKnownOwnedContainer(ctx, c.id); rerr != nil {
						problems = append(problems, fmt.Sprintf("remove known-owned %q after list failure: %v", c.id, rerr))
					}
					continue
				}
				if rerr := b.reapAmbiguousOwnedContainer(ctx, c.id, st.ownershipLabel); rerr != nil {
					problems = append(problems, fmt.Sprintf("remove ambiguous-owned %q after list failure: %v", c.id, rerr))
				}
			}
		} else {
			for _, c := range containerClaims {
				if !c.claim.attempted {
					continue
				}
				candidate, found, ferr := uniqueContainer(ctrs, c.id)
				if ferr != nil {
					problems = append(problems, ferr.Error())
					continue
				}
				if !found {
					continue
				}
				if !c.claim.owned {
					owned, oerr := b.containerHasOwnership(ctx, candidate, st.ownershipLabel)
					if oerr != nil {
						problems = append(problems, oerr.Error())
						continue
					}
					if !owned {
						continue
					}
				}
				if rerr := b.reapContainer(ctx, candidate); rerr != nil {
					problems = append(problems, fmt.Sprintf("remove %q: %v", c.id, rerr))
				}
			}
		}
	}
	ownsWorkspace := func(v VolumeSummary) bool {
		if v.Name != names.Workspace {
			return false
		}
		return st.workspace.owned || (v.LabelsObserved && slices.Contains(v.Labels, st.ownershipLabel))
	}
	// After a successful create, the exact workspace name is owned. After an
	// ambiguous failed create, require the unpredictable ownership label too.
	if vols, err := b.rt.ListVolumes(ctx); err != nil {
		problems = append(problems, fmt.Sprintf("list volumes: %v", err))
		// As with containers, an unrelated malformed row must not suppress
		// cleanup of the exact workspace name established by a successful
		// create. An ambiguous create still requires list/label evidence.
		if st.workspace.owned {
			if derr := b.rt.DeleteVolume(ctx, names.Workspace); derr != nil {
				problems = append(problems, fmt.Sprintf("delete known-owned volume %q after list failure: %v", names.Workspace, derr))
			}
		}
	} else {
		v, found, ferr := uniqueVolume(vols, names.Workspace)
		if ferr != nil {
			problems = append(problems, ferr.Error())
		} else if found {
			if v.Name == names.Workspace && !st.workspace.owned && !v.LabelsObserved {
				problems = append(problems, fmt.Sprintf("volume %q list entry omitted labels after ambiguous create", v.Name))
			} else if ownsWorkspace(v) {
				if derr := b.rt.DeleteVolume(ctx, v.Name); derr != nil {
					problems = append(problems, fmt.Sprintf("delete volume %q: %v", v.Name, derr))
				}
			}
		}
	}

	// Prove absence: nothing the run owns may survive the reap (a delete that
	// reported success but left the object is caught here).
	if st.agent.attempted || st.exporter.attempted {
		if ctrs, err := b.rt.ListContainers(ctx); err != nil {
			problems = append(problems, fmt.Sprintf("re-list containers: %v", err))
		} else {
			for _, c := range containerClaims {
				if !c.claim.attempted {
					continue
				}
				candidate, found, ferr := uniqueContainer(ctrs, c.id)
				if ferr != nil {
					problems = append(problems, "re-list "+ferr.Error())
					continue
				}
				if !found {
					continue
				}
				owned := c.claim.owned
				if !owned {
					var oerr error
					owned, oerr = b.containerHasOwnership(ctx, candidate, st.ownershipLabel)
					if oerr != nil {
						problems = append(problems, "re-list "+oerr.Error())
						continue
					}
				}
				if owned {
					problems = append(problems, fmt.Sprintf("container %q survived teardown", c.id))
				}
			}
		}
	}
	if vols, err := b.rt.ListVolumes(ctx); err != nil {
		problems = append(problems, fmt.Sprintf("re-list volumes: %v", err))
	} else {
		v, found, ferr := uniqueVolume(vols, names.Workspace)
		if ferr != nil {
			problems = append(problems, "re-list "+ferr.Error())
		} else if found {
			if v.Name == names.Workspace && !st.workspace.owned && !v.LabelsObserved {
				problems = append(problems, fmt.Sprintf("volume %q re-list entry omitted labels after ambiguous create", v.Name))
			} else if ownsWorkspace(v) {
				problems = append(problems, fmt.Sprintf("volume %q survived teardown", v.Name))
			}
		}
	}

	if len(problems) > 0 {
		return failf(CheckTeardown, "%s", strings.Join(problems, "; "))
	}
	return nil
}

// containerHasOwnership uses a list row's labels when present and falls back
// to inspect when the runtime's list shape omits them. The inspect report must
// identify the exact candidate and expose labels before its invocation token
// can authorize cleanup after an ambiguous create.
func (b *Backend) containerHasOwnership(ctx context.Context, candidate ContainerSummary, ownershipLabel Label) (bool, error) {
	if candidate.LabelsObserved {
		return slices.Contains(candidate.Labels, ownershipLabel), nil
	}
	rep, err := b.rt.Inspect(ctx, candidate.ID)
	if err != nil {
		return false, fmt.Errorf("inspect container %q ownership after ambiguous create: %w", candidate.ID, err)
	}
	if rep.ID != candidate.ID {
		return false, fmt.Errorf("inspect container %q ownership returned the wrong identity", candidate.ID)
	}
	if !rep.LabelsObserved {
		return false, fmt.Errorf("container %q omitted labels from both list and inspect after ambiguous create", candidate.ID)
	}
	return slices.Contains(rep.Labels, ownershipLabel), nil
}

// reapKnownOwnedContainer reconstructs the state needed for cleanup when the
// full list is unavailable. It is only for a name whose create succeeded;
// ambiguous creates must go through fresh per-invocation ownership evidence.
func (b *Backend) reapKnownOwnedContainer(ctx context.Context, id string) error {
	rep, err := b.rt.Inspect(ctx, id)
	if err != nil {
		return fmt.Errorf("inspect: %w", err)
	}
	if rep.ID != id {
		return fmt.Errorf("inspect returned the wrong identity")
	}
	return b.reapContainer(ctx, ContainerSummary{ID: id, State: rep.State})
}

// reapAmbiguousOwnedContainer reaps the exact name after an ambiguous create
// when the full list is unavailable. Ownership is unproven here (create neither
// clearly succeeded nor failed), so it acts only once a fresh inspect both
// identifies the exact container and exposes this invocation's unpredictable
// ownership label, the same evidence containerHasOwnership requires on the
// success-list path. A foreign same-name object lacks the label and is left
// untouched; a wrong identity or absent labels fails closed. The reap uses the
// inspected state so an already-stopped container is not needlessly stopped.
func (b *Backend) reapAmbiguousOwnedContainer(ctx context.Context, id string, ownershipLabel Label) error {
	rep, err := b.rt.Inspect(ctx, id)
	if err != nil {
		return fmt.Errorf("inspect: %w", err)
	}
	if rep.ID != id {
		return fmt.Errorf("inspect returned the wrong identity")
	}
	if !rep.LabelsObserved {
		return fmt.Errorf("inspect omitted labels after ambiguous create")
	}
	if !slices.Contains(rep.Labels, ownershipLabel) {
		return nil
	}
	return b.reapContainer(ctx, ContainerSummary{ID: id, State: rep.State})
}

// uniqueContainer returns the one exact-id entry from a full runtime list.
// Contradictory duplicate identities are unknown evidence, never an ordering
// rule for ownership or absence.
func uniqueContainer(ctrs []ContainerSummary, id string) (ContainerSummary, bool, error) {
	var found ContainerSummary
	seen := false
	for _, cs := range ctrs {
		if cs.ID != id {
			continue
		}
		if seen {
			return ContainerSummary{}, false, fmt.Errorf("container %q appeared more than once in runtime listing", id)
		}
		found, seen = cs, true
	}
	return found, seen, nil
}

// uniqueVolume applies the same exact-identity rule before a name-based
// delete can use one row's ownership evidence.
func uniqueVolume(vols []VolumeSummary, name string) (VolumeSummary, bool, error) {
	var found VolumeSummary
	seen := false
	for _, v := range vols {
		if v.Name != name {
			continue
		}
		if seen {
			return VolumeSummary{}, false, fmt.Errorf("volume %q appeared more than once in runtime listing", name)
		}
		found, seen = v, true
	}
	return found, seen, nil
}

// reapContainer stops a container unless it is affirmatively observed stopped,
// then attempts deletion even when stop reports an error. Unknown/drifted state
// is not proof of stopped, and a stop error may still mean the side effect took
// place; joining both results maximizes cleanup without hiding either failure.
func (b *Backend) reapContainer(ctx context.Context, cs ContainerSummary) error {
	var stopErr error
	if cs.State != StateStopped {
		if err := b.rt.StopContainer(ctx, cs.ID); err != nil {
			stopErr = fmt.Errorf("stop: %w", err)
		}
	}
	deleteErr := b.rt.DeleteContainer(ctx, cs.ID)
	return errors.Join(stopErr, deleteErr)
}
