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

// unattendedCapabilities is the policy floor PreJob protects. The handoff
// itself remains usable in attended_dev before the expensive full suite runs,
// but unattended admission must also carry the networkless-export proof.
var unattendedCapabilities = []exec.Capability{
	exec.CapDetachableWorkspace,
	exec.CapPostExitExport,
	exec.CapReadOnlyRemount,
	exec.CapNetworklessExport,
}

// HandoffResult is a completed, gate-passed handoff.
type HandoffResult struct {
	// Admission is the spawn-time capability snapshot the run was admitted
	// under (§5.3); audit binds to it.
	Admission exec.Admission
	// ExportDir holds the verified manifest and blobs (both §5.6 channels when
	// the workspace declared evidence). The caller owns the directory and
	// removes it when done.
	ExportDir string
	// Manifest is the decoded, digest-verified §5.6 repo-change manifest.
	Manifest export.Manifest
	// Evidence is the decoded, digest-verified evidence manifest; valid only
	// when EvidencePresent is true.
	Evidence        export.EvidenceManifest
	EvidencePresent bool
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
	// succeeded is set only immediately before the successful return. The
	// output-dir cleanup keys off it rather than the named err, so a panic
	// unwind (where err is still nil, e.g. a typed-nil scanner) does not leave
	// the unscanned output on the host.
	succeeded bool
}

// Handoff runs one full workspace handoff: admit against the capability
// floor, run the agent with the workspace read-write and credentials in
// their own read-only mounts (checks 1-2), prove the writer VM terminated by
// observed state (check 3), run the exporter with the workspace read-only
// behind a pre-execution mount-allowlist inspection (check 4), and verify the
// exported digests of both §5.6 channels plus the §5.4 scan (check 7) before
// releasing anything. Check 5 (the in-exporter environment proof) is attested
// at conformance time by a dedicated probe (Suite.Full), not on every handoff:
// the exporter now runs only the trusted helper, which emits the channels but
// not the proof, and check 4's inspect-before-execute covers the mount topology
// per handoff. Teardown runs on every exit path; a teardown failure fails the
// gate even when everything else passed.
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
		// The archive is transient once verified; the output dir is kept only
		// when the caller actually receives it: the run reached its successful
		// return and teardown left the result intact. Any other unwind removes
		// the unscanned output, including a teardown failure that nils an
		// otherwise good result and a panic (where err is still nil). Both are
		// best-effort host-temp cleanup.
		if st.archiveDir != "" {
			_ = os.RemoveAll(st.archiveDir)
		}
		if st.exportDir != "" && (!st.succeeded || err != nil) {
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
	// The gate re-checks the report's identity rather than trusting the
	// Runtime implementation: a fingerprint bound to the wrong object would
	// make cleanup misclassify this run's own volume later.
	if wsView.Name != names.Workspace {
		return nil, fmt.Errorf("workspace volume observation returned the wrong identity")
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
	// Capture the fingerprint only after the allowlist verified the report's
	// identity (rep.ID against the generated name): a fingerprint bound to
	// the wrong object would make cleanup misclassify this run's own agent.
	if err := verifyAgentAllowlist(agentRep, agentSpec); err != nil {
		return nil, err
	}
	st.agent.fingerprint, err = ownedFingerprint(agentRep.CreationDate, agentRep.Labels, agentRep.LabelsObserved, ownershipLabel)
	if err != nil {
		return nil, failf(CheckControlPlaneIsolation, "agent container %q: %v", names.Agent, err)
	}
	if err := b.rt.StartContainer(ctx, names.Agent); err != nil {
		return nil, fmt.Errorf("start agent container: %w", err)
	}

	// Check 3: writer termination is observed state, never scheduling
	// intent (a second VM cannot attach a volume a live VM holds rw; only
	// observed "stopped" proves the attachment is gone).
	if err := b.waitStopped(ctx, names.Agent, st.agent, st.ownershipLabel, b.cfg.WriterStopTimeout); err != nil {
		return nil, failf(CheckWriterTermination, "agent: %v", err)
	}
	if err := b.rt.DeleteContainer(ctx, names.Agent); err != nil {
		return nil, failf(CheckWriterTermination, "delete stopped agent: %v", err)
	}
	// This invocation's own delete succeeded, so the object it created is
	// gone; whatever answers to the deterministic name from here on is
	// classified like any other candidate (the round-28 ownership downgrade
	// is subsumed: no path reaps by create-success identity anymore, and the
	// absence proof below treats a foreign same-name replacement as absent).
	if err := b.verifyContainerAbsent(ctx, names.Agent, st.agent, st.ownershipLabel, CheckWriterTermination); err != nil {
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
	// As with the agent: the allowlist's identity check runs before the
	// fingerprint is captured from the same report.
	if err := verifyExporterAllowlist(b.cfg, rep, names.Exporter, names.Workspace); err != nil {
		return nil, err
	}
	st.exporter.fingerprint, err = ownedFingerprint(rep.CreationDate, rep.Labels, rep.LabelsObserved, ownershipLabel)
	if err != nil {
		return nil, failf(CheckExporterAllowlist, "exporter container %q: %v", names.Exporter, err)
	}
	if err := b.rt.StartContainer(ctx, names.Exporter); err != nil {
		return nil, fmt.Errorf("start exporter container: %w", err)
	}
	if err := b.waitStopped(ctx, names.Exporter, st.exporter, st.ownershipLabel, b.cfg.ExporterTimeout); err != nil {
		return nil, failf(CheckExportVerification, "exporter: %v", err)
	}

	// Check 7: collect the stopped exporter's rootfs and verify both channels'
	// manifests, digests, and the §5.4 scan before releasing anything. The
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
	// Mark success only here, so the deferred cleanup keeps the output dir only
	// on a real delivery; a panic before this point still removes it.
	st.succeeded = true
	return &HandoffResult{
		Admission:       adm,
		ExportDir:       out.Dir,
		Manifest:        out.Manifest,
		Evidence:        out.Evidence,
		EvidencePresent: out.EvidencePresent,
	}, nil
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

// waitStopped polls until the claimed container is observed stopped. The
// wait is budgeted in whole poll intervals (ceil(timeout / PollInterval)
// attempts), so tests with an injected Sleep are fully deterministic while a
// timeout shorter than, or not a whole multiple of, the interval still spends
// its full budget instead of giving up a poll early; its own deadline also
// bounds each runtime call, so a wedged Inspect cannot defeat the named
// writer or exporter timeout. Every poll re-classifies the observation
// against the claim: check 3's stopped observation is proof about the one VM
// the gate started, so a same-name replacement can never satisfy it (even on
// a runtime that reports no creation instants, where the unpredictable token
// is the whole evidence), and the delete that follows a satisfied wait
// always targets a just-verified observation.
func (b *Backend) waitStopped(ctx context.Context, id string, claim objectClaim, ownershipLabel Label, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	attempts := int((timeout + b.cfg.PollInterval - 1) / b.cfg.PollInterval)
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
		switch classifyEvidence(claim, ownershipLabel, rep.CreationDate, rep.Labels, rep.LabelsObserved) {
		case evidenceOurs:
			// The one object this run created; its state is meaningful.
		case evidenceForeign:
			return fmt.Errorf("inspect returned a same-name container with a different creation identity")
		case evidenceUnprovable:
			return fmt.Errorf("inspect could not prove the container is the one this run created")
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

// verifyContainerAbsent proves the container this run created is gone from
// the runtime's full container list; check 3 requires absence, not a
// successful delete call. Absence is about the claimed object, not the name:
// a same-name row whose fresh evidence proves it foreign is a replacement
// that appeared in the delete-to-absence window and counts as absent, so the
// caller clears the claim and deferred teardown never reaps the replacement
// (failing the run here would instead leave the claim owned, and teardown
// would destroy an object this run did not create). A row whose evidence is
// unprovable still fails the check.
func (b *Backend) verifyContainerAbsent(ctx context.Context, id string, claim objectClaim, ownershipLabel Label, c Check) error {
	ctrs, err := b.rt.ListContainers(ctx)
	if err != nil {
		return failf(c, "list containers to verify %q absent: %v", id, err)
	}
	candidate, found, ferr := uniqueContainer(ctrs, id)
	if ferr != nil {
		return failf(c, "verify %q absent: %v", id, ferr)
	}
	if !found {
		return nil
	}
	ev, eerr := b.containerEvidence(ctx, candidate, claim, ownershipLabel)
	if eerr != nil {
		return failf(c, "verify %q absent: %v", id, eerr)
	}
	switch ev {
	case evidenceOurs:
		return failf(c, "container %q still listed after delete", id)
	case evidenceForeign:
		return nil
	case evidenceUnprovable:
		return failf(c, "container %q absence unprovable after delete", id)
	}
	return failf(c, "container %q absence evidence invalid", id)
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
	// Every candidate, owned or ambiguous, is reaped only on fresh evidence
	// that it is still the object this invocation created (its captured
	// creation fingerprint corroborated by the unpredictable ownership label,
	// else the label alone). A foreign verdict — a collision or a same-name
	// replacement — leaves the object untouched; an unprovable one withholds
	// the delete and fails teardown.
	if st.agent.attempted || st.exporter.attempted {
		if ctrs, err := b.rt.ListContainers(ctx); err != nil {
			problems = append(problems, fmt.Sprintf("list containers: %v", err))
			// A full-list failure can be caused by an unrelated malformed row.
			// It must not suppress cleanup of a name this invocation created:
			// owned and ambiguous claims alike fall back to a direct inspect,
			// and the reap happens only on that fresh evidence. Otherwise one
			// unrelated broken row could leave the credential-mounted writer
			// restartable.
			for _, c := range containerClaims {
				if !c.claim.attempted {
					continue
				}
				if rerr := b.reapUnlistedContainer(ctx, c.id, c.claim, st.ownershipLabel); rerr != nil {
					problems = append(problems, fmt.Sprintf("remove %q after list failure: %v", c.id, rerr))
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
				ev, eerr := b.containerEvidence(ctx, candidate, c.claim, st.ownershipLabel)
				if eerr != nil {
					problems = append(problems, eerr.Error())
					continue
				}
				switch ev {
				case evidenceOurs:
					if rerr := b.reapContainer(ctx, candidate); rerr != nil {
						problems = append(problems, fmt.Sprintf("remove %q: %v", c.id, rerr))
					}
				case evidenceForeign:
					// Not this run's object; leave it.
				case evidenceUnprovable:
					problems = append(problems, fmt.Sprintf("container %q ownership unprovable; not deleting", c.id))
				}
			}
		}
	}
	// The workspace follows the same evidence rule: a successful create alone
	// no longer authorizes a name-addressed delete, the volume observed at
	// teardown must still prove it is the one this run made.
	if vols, err := b.rt.ListVolumes(ctx); err != nil {
		problems = append(problems, fmt.Sprintf("list volumes: %v", err))
		// As with containers, an unrelated malformed row must not suppress
		// cleanup of the workspace name this invocation created: owned and
		// ambiguous claims alike fall back to the per-object inspect, which
		// supplies the evidence the list could not (an ambiguous claim has no
		// fingerprint, so only the fresh token can authorize the delete).
		if st.workspace.attempted {
			v, verr := b.rt.InspectVolume(ctx, names.Workspace)
			switch {
			case verr != nil:
				problems = append(problems, fmt.Sprintf("inspect volume %q after list failure: %v", names.Workspace, verr))
			case v.Name != names.Workspace:
				problems = append(problems, fmt.Sprintf("inspect volume %q after list failure returned the wrong identity", names.Workspace))
			default:
				switch classifyEvidence(st.workspace, st.ownershipLabel, v.CreationDate, v.Labels, v.LabelsObserved) {
				case evidenceOurs:
					if derr := b.rt.DeleteVolume(ctx, names.Workspace); derr != nil {
						problems = append(problems, fmt.Sprintf("delete volume %q after list failure: %v", names.Workspace, derr))
					}
				case evidenceForeign:
					// Not this run's volume; leave it.
				case evidenceUnprovable:
					problems = append(problems, fmt.Sprintf("volume %q ownership unprovable after list failure; not deleting", names.Workspace))
				}
			}
		}
	} else {
		v, found, ferr := uniqueVolume(vols, names.Workspace)
		if ferr != nil {
			problems = append(problems, ferr.Error())
		} else if found {
			ev, eerr := b.volumeEvidence(ctx, v, st.workspace, st.ownershipLabel)
			switch {
			case eerr != nil:
				problems = append(problems, eerr.Error())
			case ev == evidenceOurs:
				if derr := b.rt.DeleteVolume(ctx, v.Name); derr != nil {
					problems = append(problems, fmt.Sprintf("delete volume %q: %v", v.Name, derr))
				}
			case ev == evidenceForeign:
				// Not this run's volume; leave it.
			case ev == evidenceUnprovable:
				problems = append(problems, fmt.Sprintf("volume %q ownership unprovable; not deleting", v.Name))
			}
		}
	}

	// Prove absence: nothing the run owns may survive the reap (a delete that
	// reported success but left the object is caught here). A surviving
	// same-name row classified foreign is a replacement that appeared after
	// this run's object was reaped: it counts as absent and is never
	// re-reaped; only an unprovable row still fails the proof.
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
				ev, eerr := b.containerEvidence(ctx, candidate, c.claim, st.ownershipLabel)
				if eerr != nil {
					problems = append(problems, "re-list "+eerr.Error())
					continue
				}
				switch ev {
				case evidenceOurs:
					problems = append(problems, fmt.Sprintf("container %q survived teardown", c.id))
				case evidenceForeign:
					// A replacement, not a survivor.
				case evidenceUnprovable:
					problems = append(problems, fmt.Sprintf("container %q survival unprovable after teardown", c.id))
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
			ev, eerr := b.volumeEvidence(ctx, v, st.workspace, st.ownershipLabel)
			switch {
			case eerr != nil:
				problems = append(problems, "re-list "+eerr.Error())
			case ev == evidenceOurs:
				problems = append(problems, fmt.Sprintf("volume %q survived teardown", v.Name))
			case ev == evidenceForeign:
				// A replacement, not a survivor.
			case ev == evidenceUnprovable:
				problems = append(problems, fmt.Sprintf("volume %q survival unprovable after teardown", v.Name))
			}
		}
	}

	if len(problems) > 0 {
		return failf(CheckTeardown, "%s", strings.Join(problems, "; "))
	}
	return nil
}

// objectEvidence classifies a fresh observation of an attempted name against
// this invocation's claim. Every destructive decision routes through it: a
// successful create never confers standing name-wide authority, only the
// right to reap the object fresh evidence proves is the one this run made.
// The zero value "" is invalid by design.
type objectEvidence string

const (
	// evidenceOurs: the observation proves the object is the one this
	// invocation created; reaping it is authorized.
	evidenceOurs objectEvidence = "ours"
	// evidenceForeign: the observation proves the object is not this
	// invocation's (a caller-owned collision or a same-name replacement); it
	// is left untouched and counts as absent for this run's proofs.
	evidenceForeign objectEvidence = "foreign"
	// evidenceUnprovable: the observation cannot prove the object either ours
	// or foreign; destructive action is withheld and teardown fails.
	evidenceUnprovable objectEvidence = "unprovable"
)

// AllObjectEvidence lists every valid objectEvidence; it drives table-driven
// tests and is the single place a new classification is registered.
var AllObjectEvidence = []objectEvidence{evidenceOurs, evidenceForeign, evidenceUnprovable}

func (e objectEvidence) valid() bool {
	switch e {
	case evidenceOurs, evidenceForeign, evidenceUnprovable:
		return true
	default:
		return false
	}
}

// classifyEvidence weighs one fresh observation (creation instant and labels)
// against a claim (the fingerprint captured after create, the invocation's
// unpredictable ownership label). The fingerprint is a veto, never proof by
// itself: a differing instant is a replacement, foreign even when it copies
// this run's labels, while a matching instant still needs the token to
// corroborate it — creation instants are coarse (second granularity on the
// reference runtime), so a same-instant observation that lacks the token, or
// cannot show labels at all, is unprovable rather than ours. Without a
// usable instant comparison the label decides alone: the token is
// unpredictable, so observing it proves ours, observing its absence proves
// foreign, and an observation that cannot show labels proves nothing.
func classifyEvidence(claim objectClaim, ownershipLabel Label, observedDate string, labels []Label, labelsObserved bool) objectEvidence {
	if claim.fingerprint != "" && observedDate != "" && observedDate != claim.fingerprint {
		return evidenceForeign
	}
	if !labelsObserved {
		return evidenceUnprovable
	}
	if slices.Contains(labels, ownershipLabel) {
		return evidenceOurs
	}
	if claim.fingerprint != "" && observedDate == claim.fingerprint {
		// Same instant, not our labels: contradictory (or a coarse-instant
		// collision with a replacement); withhold rather than classify.
		return evidenceUnprovable
	}
	return evidenceForeign
}

// underObserved reports whether an observation was too incomplete to carry a
// verdict on its own: labels unobserved, or a claimed fingerprint with no
// reported instant to compare against. Only such observations earn the
// per-object inspect fallback; contradictory evidence from a complete
// observation never retries into a cleaner answer.
func underObserved(observedDate string, labelsObserved bool, claim objectClaim) bool {
	return !labelsObserved || (claim.fingerprint != "" && observedDate == "")
}

// containerEvidence resolves a listed candidate to evidence, classifying the
// row itself first and falling back to a direct inspect only when the row was
// too incomplete to carry a verdict. The fallback report must identify the
// exact candidate.
func (b *Backend) containerEvidence(ctx context.Context, candidate ContainerSummary, claim objectClaim, ownershipLabel Label) (objectEvidence, error) {
	ev := classifyEvidence(claim, ownershipLabel, candidate.CreationDate, candidate.Labels, candidate.LabelsObserved)
	if ev != evidenceUnprovable || !underObserved(candidate.CreationDate, candidate.LabelsObserved, claim) {
		// The row already carried a verdict (a mismatched instant proves
		// foreign whatever the labels say), or it was fully observed and
		// still contradictory.
		return ev, nil
	}
	rep, err := b.rt.Inspect(ctx, candidate.ID)
	if err != nil {
		return evidenceUnprovable, fmt.Errorf("inspect container %q ownership: %w", candidate.ID, err)
	}
	if rep.ID != candidate.ID {
		return evidenceUnprovable, fmt.Errorf("inspect container %q ownership returned the wrong identity", candidate.ID)
	}
	return classifyEvidence(claim, ownershipLabel, rep.CreationDate, rep.Labels, rep.LabelsObserved), nil
}

// volumeEvidence is the volume analogue of containerEvidence, using the
// per-object InspectVolume when the list row was too incomplete to carry a
// verdict.
func (b *Backend) volumeEvidence(ctx context.Context, candidate VolumeSummary, claim objectClaim, ownershipLabel Label) (objectEvidence, error) {
	ev := classifyEvidence(claim, ownershipLabel, candidate.CreationDate, candidate.Labels, candidate.LabelsObserved)
	if ev != evidenceUnprovable || !underObserved(candidate.CreationDate, candidate.LabelsObserved, claim) {
		return ev, nil
	}
	v, err := b.rt.InspectVolume(ctx, candidate.Name)
	if err != nil {
		return evidenceUnprovable, fmt.Errorf("inspect volume %q ownership: %w", candidate.Name, err)
	}
	if v.Name != candidate.Name {
		return evidenceUnprovable, fmt.Errorf("inspect volume %q ownership returned the wrong identity", candidate.Name)
	}
	return classifyEvidence(claim, ownershipLabel, v.CreationDate, v.Labels, v.LabelsObserved), nil
}

// reapUnlistedContainer reconstructs cleanup evidence when the full list is
// unavailable, for owned and ambiguous claims alike: the direct inspect must
// prove the object is the one this run created (fingerprint corroborated by
// the token, else the token alone; an ambiguous claim has no fingerprint and
// so always needs the token). A foreign replacement or collision is left
// untouched; a wrong identity or unprovable observation withholds the delete
// and fails closed. The reap uses the inspected state so an already-stopped
// container is not needlessly stopped.
func (b *Backend) reapUnlistedContainer(ctx context.Context, id string, claim objectClaim, ownershipLabel Label) error {
	rep, err := b.rt.Inspect(ctx, id)
	if err != nil {
		return fmt.Errorf("inspect: %w", err)
	}
	if rep.ID != id {
		return fmt.Errorf("inspect returned the wrong identity")
	}
	switch classifyEvidence(claim, ownershipLabel, rep.CreationDate, rep.Labels, rep.LabelsObserved) {
	case evidenceOurs:
		return b.reapContainer(ctx, ContainerSummary{ID: id, State: rep.State})
	case evidenceForeign:
		return nil
	case evidenceUnprovable:
		return errors.New("ownership unprovable from inspect; not deleting")
	}
	return errors.New("invalid ownership evidence")
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
