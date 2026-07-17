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

// runState tracks host-temp directories and each runtime object's ownership
// state so deferred cleanup can remove only this invocation's objects. A
// successful create proves ownership; an ambiguous create must be resolved
// from a fresh runtime listing and this invocation's unpredictable label.
type runState struct {
	// workspaceAttempted records that CreateVolume was called. On an ambiguous
	// error, teardown may reap only a workspace carrying this invocation's
	// unpredictable ownershipLabel; an ordinary already-exists collision does
	// not carry it and is left untouched.
	workspaceAttempted bool
	ownershipLabel     Label
	workspaceOwned     bool
	agentAttempted     bool
	agentOwned         bool
	exporterAttempted  bool
	exporterOwned      bool
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
	st.workspaceAttempted = true
	volumeLabels := append(runLabels(hs.RunID), ownershipLabel)
	if err := b.rt.CreateVolume(ctx, names.Workspace, hs.WorkspaceSizeMB, volumeLabels); err != nil {
		return nil, fmt.Errorf("create workspace volume: %w", err)
	}
	st.workspaceOwned = true

	st.agentAttempted = true
	if err := b.rt.CreateContainer(ctx, agentSpec); err != nil {
		return nil, fmt.Errorf("create agent container: %w", err)
	}
	st.agentOwned = true
	if err := b.rt.StartContainer(ctx, names.Agent); err != nil {
		return nil, fmt.Errorf("start agent container: %w", err)
	}

	// Check 3: writer termination is observed state, never scheduling
	// intent (a second VM cannot attach a volume a live VM holds rw; only
	// observed "stopped" proves the attachment is gone).
	if err := b.waitStopped(ctx, names.Agent, b.cfg.WriterStopTimeout); err != nil {
		return nil, failf(CheckWriterTermination, "agent: %v", err)
	}
	if err := b.rt.DeleteContainer(ctx, names.Agent); err != nil {
		return nil, failf(CheckWriterTermination, "delete stopped agent: %v", err)
	}
	if err := b.verifyContainerAbsent(ctx, names.Agent, CheckWriterTermination); err != nil {
		return nil, err
	}
	st.agentAttempted = false
	st.agentOwned = false

	// Check 4: create the exporter but inspect it against the generated
	// allowlist before it ever executes.
	exporterSpec := buildExporterSpec(b.cfg, hs, names, ownershipLabel)
	st.exporterAttempted = true
	if err := b.rt.CreateContainer(ctx, exporterSpec); err != nil {
		return nil, fmt.Errorf("create exporter container: %w", err)
	}
	st.exporterOwned = true
	rep, err := b.rt.Inspect(ctx, names.Exporter)
	if err != nil {
		return nil, failf(CheckExporterAllowlist, "inspect exporter before execution: %v", err)
	}
	if err := verifyExporterAllowlist(b.cfg, rep, names.Exporter, names.Workspace); err != nil {
		return nil, err
	}
	if err := b.rt.StartContainer(ctx, names.Exporter); err != nil {
		return nil, fmt.Errorf("start exporter container: %w", err)
	}
	if err := b.waitStopped(ctx, names.Exporter, b.cfg.ExporterTimeout); err != nil {
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
// tests with an injected Sleep are fully deterministic.
func (b *Backend) waitStopped(ctx context.Context, id string, timeout time.Duration) error {
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
	if !st.workspaceAttempted {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), b.cfg.TeardownTimeout)
	defer cancel()
	var problems []string

	type containerClaim struct {
		id        string
		attempted bool
		owned     bool
	}
	containerClaims := []containerClaim{
		{id: names.Agent, attempted: st.agentAttempted, owned: st.agentOwned},
		{id: names.Exporter, attempted: st.exporterAttempted, owned: st.exporterOwned},
	}
	// Each container is reaped only when its own create succeeded or the fresh
	// teardown listing carries the unpredictable ownership label after an
	// ambiguous create error.
	if st.agentAttempted || st.exporterAttempted {
		if ctrs, err := b.rt.ListContainers(ctx); err != nil {
			problems = append(problems, fmt.Sprintf("list containers: %v", err))
			// A full-list failure can be caused by an unrelated malformed row.
			// It must not suppress cleanup of an exact name this invocation
			// already owns from a successful create. Ambiguous creates still
			// require list/label evidence and are deliberately not reaped here.
			for _, claim := range containerClaims {
				if !claim.attempted || !claim.owned {
					continue
				}
				if rerr := b.reapKnownOwnedContainer(ctx, claim.id); rerr != nil {
					problems = append(problems, fmt.Sprintf("remove known-owned %q after list failure: %v", claim.id, rerr))
				}
			}
		} else {
			for _, claim := range containerClaims {
				if !claim.attempted {
					continue
				}
				candidate, found, ferr := uniqueContainer(ctrs, claim.id)
				if ferr != nil {
					problems = append(problems, ferr.Error())
					continue
				}
				if !found {
					continue
				}
				if !claim.owned {
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
					problems = append(problems, fmt.Sprintf("remove %q: %v", claim.id, rerr))
				}
			}
		}
	}
	ownsWorkspace := func(v VolumeSummary) bool {
		if v.Name != names.Workspace {
			return false
		}
		return st.workspaceOwned || (v.LabelsObserved && slices.Contains(v.Labels, st.ownershipLabel))
	}
	// After a successful create, the exact workspace name is owned. After an
	// ambiguous failed create, require the unpredictable ownership label too.
	if vols, err := b.rt.ListVolumes(ctx); err != nil {
		problems = append(problems, fmt.Sprintf("list volumes: %v", err))
	} else {
		v, found, ferr := uniqueVolume(vols, names.Workspace)
		if ferr != nil {
			problems = append(problems, ferr.Error())
		} else if found {
			if v.Name == names.Workspace && !st.workspaceOwned && !v.LabelsObserved {
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
	if st.agentAttempted || st.exporterAttempted {
		if ctrs, err := b.rt.ListContainers(ctx); err != nil {
			problems = append(problems, fmt.Sprintf("re-list containers: %v", err))
		} else {
			for _, claim := range containerClaims {
				if !claim.attempted {
					continue
				}
				candidate, found, ferr := uniqueContainer(ctrs, claim.id)
				if ferr != nil {
					problems = append(problems, "re-list "+ferr.Error())
					continue
				}
				if !found {
					continue
				}
				owned := claim.owned
				if !owned {
					var oerr error
					owned, oerr = b.containerHasOwnership(ctx, candidate, st.ownershipLabel)
					if oerr != nil {
						problems = append(problems, "re-list "+oerr.Error())
						continue
					}
				}
				if owned {
					problems = append(problems, fmt.Sprintf("container %q survived teardown", claim.id))
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
			if v.Name == names.Workspace && !st.workspaceOwned && !v.LabelsObserved {
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
