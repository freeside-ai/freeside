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

	"github.com/freeside-ai/freeside/daemon/internal/domain"
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
// but unattended admission must also carry both suite-earned proofs: the
// networkless exporter boundary and the enforced provider-egress writer
// boundary (§5.7, issue #327). Mirrors the plan-mandated minimum
// domain.requiredCapabilities appends for ModeUnattended.
var unattendedCapabilities = []exec.Capability{
	exec.CapDetachableWorkspace,
	exec.CapPostExitExport,
	exec.CapReadOnlyRemount,
	exec.CapNetworklessExport,
	exec.CapEnforcedProviderEgress,
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
	// CommitPlanPresent records that check 7 admitted and scanned the reserved
	// opaque plan member. Its bytes remain in ExportDir for the hostile importer.
	CommitPlanPresent bool
	// Workspace is what the gate observed about the workspace this run used.
	Workspace WorkspaceObservation
	// Egress is the writer network the runtime reported, not the requested
	// topology echoed back. Profile is the gate's resulting classification;
	// Network and HostOnly come from the attested runtime report.
	Egress EgressObservation
	// AuthStore records the §5.4 mutation window and the observed
	// credential-store digests for a leased run; zero when the spec carried
	// no lease claim.
	AuthStore AuthStoreObservation
}

// AuthStoreObservation is the audit record of one leased run's §5.4 window:
// the lease the gate held, and the credential volume's content digests
// observed before the writer started and after it was proven absent. It
// attests that the store changed (or did not), never what changed: the
// digests are hashes read by a VM that cannot write the store, and the §5.4
// export scan remains the content control.
type AuthStoreObservation struct {
	// Leased distinguishes a real zero-observation (no lease claim) from a
	// leased run; every other field is meaningful only when it is true.
	Leased         bool
	AuthIdentityID domain.AuthIdentityID
	Holder         domain.InvocationID
	Fence          int64
	AcquiredAt     time.Time
	ExpiresAt      time.Time
	// PreDigest and PostDigest are the volume's six-pass content digests
	// (content hashes, executable bits, directories, symlinks, the
	// remaining node kinds, and every entry's mode, hard-link count, and
	// ownership) before and after the writer.
	PreDigest  string
	PostDigest string
	// PostAttested records that PostDigest was taken inside this run's own
	// still-held window, so Mutated attributes the change to this run's
	// writer. Recovery after the window lapsed cannot recreate that
	// observation — later holders may have written since, and no fresh
	// serialization can restore the state the writer left — so it reports
	// the post-write attestation as lost (false, PostDigest empty) rather
	// than attributing an intervening holder's mutation to this run.
	PostAttested bool
	// Mutated reports PreDigest != PostDigest: the writer changed its auth
	// store inside the window. Meaningful only while PostAttested is true.
	Mutated bool
}

type EgressObservation struct {
	Profile        domain.EgressProfile
	Network        string
	HostOnly       bool
	ProxyAuthority string
}

// WorkspaceObservation is the workspace's identity as the gate observed it,
// not as the caller described it.
type WorkspaceObservation struct {
	// Volume is the workspace volume's name: the ward lane's opaque workspace
	// reference (plan §5.7), the same value WorkspaceRef derives from a run ID.
	Volume string
	// Seeded is true only when the gate placed a declared base into the
	// workspace and then proved the result. A caller cannot set it, and a
	// seeding attempt that was not attested does not reach here at all.
	//
	// What it asserts, exactly: the workspace holds the tree the daemon staged
	// and proved is faithful to the commit BaseSHA names. The read-only
	// observer compares raw file bytes and executable modes directly with
	// HEAD, refuses extras and replacement objects, and does not trust the
	// copied index or attribute conversions. The independent host/observer
	// digest comparison still proves that the tree observed is the tree the
	// daemon staged.
	Seeded bool
	// ObservedBaseSHA is the base the workspace was observed to hold, read
	// through a read-only mount by a container that did not write it. It is
	// empty exactly when Seeded is false.
	//
	// It is stated in domain.ExecutionExport.ObservedBaseSHA's vocabulary (a
	// full lowercase commit) so a caller can carry it into the durable record
	// unchanged. It is never the declared value echoed back: a run whose
	// workspace held a different base fails the gate instead of reporting one.
	ObservedBaseSHA string
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
	seeder         objectClaim
	observer       objectClaim
	credObsPre     objectClaim
	credObsPost    objectClaim
	agent          objectClaim
	exporter       objectClaim
	network        objectClaim
	proxy          *connectProxy
	// archiveDir holds the exported rootfs archive; always removed once
	// verification is done or the run fails (the archive is never returned).
	archiveDir string
	// seedTreeDigest is the digest the host computed over the verified seed
	// source. The observer recomputes it over the workspace volume, so the
	// attestation covers the tree that actually landed rather than only the
	// HEAD pointer that came with it.
	seedTreeDigest string
	// seedSnapshotDir holds the gate's private copy of the seed source. It is
	// what gets staged into the seeder, so nothing outside the gate can mutate
	// the tree between verification and the copy.
	seedSnapshotDir string
	// baseArchiveDir holds the observer's rootfs archive while its proof is
	// read. It is cleared as soon as that read finishes, so it is non-empty
	// only inside that window; the deferred cleanup removes whatever it names
	// if the run unwinds mid-read.
	baseArchiveDir string
	// credArchiveDir is the credential observer's counterpart to
	// baseArchiveDir, its own field so the two proof reads can never orphan
	// or double-remove each other's scratch.
	credArchiveDir string
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
	// lease is the acquired §5.4 mutation window when the spec carries an
	// AuthStoreLease claim; leaseHeld records that this run acquired it and
	// still owes the release teardown performs once the writer is provably
	// gone. leaseSlot and leaseIdentity track the backend's in-process
	// per-identity slot, freed when the run ends regardless of how the
	// window itself ended.
	lease         domain.AuthStoreMutationLease
	leaseHeld     bool
	leaseSlot     bool
	leaseIdentity domain.AuthIdentityID
	// credPreDigest is the leased credential volume's content digest observed
	// before the writer started; the post-writer observation compares against
	// it to attest whether the store mutated.
	credPreDigest string
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
		if st.baseArchiveDir != "" {
			_ = os.RemoveAll(st.baseArchiveDir)
		}
		if st.credArchiveDir != "" {
			_ = os.RemoveAll(st.credArchiveDir)
		}
		if st.seedSnapshotDir != "" {
			_ = os.RemoveAll(st.seedSnapshotDir)
		}
		if st.exportDir != "" && (!st.succeeded || err != nil) {
			_ = os.RemoveAll(st.exportDir)
		}
	}()

	// §5.4: the writable credential mount rides inside an acquired, verified
	// mutation window. Acquired before any runtime object exists — a busy
	// identity refuses the run before it costs a volume — and after the
	// teardown defer, so every later refusal still releases it.
	if hs.AuthStoreLease != nil {
		// The credential observers mount the leased volume read-only at its
		// declared target and write their proof to the configured proof
		// path; overlap in either direction fails every leased handoff
		// mid-run. A target covering the proof path shadows the proof
		// write; a target nested beneath it forces the proof path to be a
		// directory (the mount's ancestor), so the observer's file redirect
		// can never land. Config validation cannot see the per-spec target,
		// so both directions are refused here, before the lease is acquired
		// or anything is created.
		if t := hs.writableCredentialTarget(); b.cfg.CredProofPath == t ||
			strings.HasPrefix(b.cfg.CredProofPath, t+"/") ||
			strings.HasPrefix(t, b.cfg.CredProofPath+"/") {
			return nil, fmt.Errorf("%w: writable credential target %q overlaps the configured credential proof path",
				ErrInvalidHandoffSpec, t)
		}
		if err := b.acquireAuthStoreLease(ctx, *hs.AuthStoreLease, st); err != nil {
			return nil, err
		}
	}

	// Reject every caller-controlled writer-shape violation before acquiring
	// any runtime object. The real proxy URL is not known until the host-only
	// network exists; a syntactically valid placeholder exercises the same
	// mount, environment-key, and explicit-network checks.
	preflightAgentSpec := buildAgentSpec(b.cfg, hs, names, ownershipLabel, "http://127.0.0.1:1")
	if err := validateAgentSpec(b.cfg, preflightAgentSpec, names.Workspace, hs.writableCredentialTarget()); err != nil {
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

	// Seed the workspace at the declared base and prove the seeder gone before
	// the writer can attach, then attest what the volume actually holds from a
	// read-only mount in a container that did not write it. Both are no-ops for
	// a blank seed. The attestation runs before the writer because the base is
	// a pre-writer fact: the agent may legitimately move HEAD.
	if err := b.seedWorkspace(ctx, hs, names, st); err != nil {
		return nil, err
	}
	observedBaseSHA, err := b.observeSeededBase(ctx, hs, names, st)
	if err != nil {
		return nil, err
	}

	// The leased credential store's pre-writer digest is a pre-writer fact
	// like the base: once the agent runs it may legitimately mutate the
	// store, so the "before" side must be attested now or never.
	if hs.AuthStoreLease != nil {
		st.credPreDigest, err = b.observeCredentialStore(ctx, hs, names.CredObsPre, st, &st.credObsPre)
		if err != nil {
			return nil, err
		}
	}

	networkReport, proxyURL, err := b.prepareProviderEgress(ctx, hs, names, st)
	if err != nil {
		return nil, err
	}
	// Checks 1-2 plus provider_only: the generated writer spec is re-verified,
	// not trusted, after the runtime supplies the host-only gateway the proxy
	// address is derived from.
	agentSpec := buildAgentSpec(b.cfg, hs, names, ownershipLabel, proxyURL)
	if err := validateAgentSpec(b.cfg, agentSpec, names.Workspace, hs.writableCredentialTarget()); err != nil {
		return nil, err
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
	currentNetwork, err := b.rt.InspectNetwork(ctx, names.Network)
	if err != nil {
		return nil, failf(CheckAgentEgress, "re-inspect provider network before writer execution: %v", err)
	}
	if currentNetwork.Name != networkReport.Name ||
		currentNetwork.Mode != NetworkHostOnly ||
		currentNetwork.IPv4Gateway != networkReport.IPv4Gateway ||
		currentNetwork.IPv4Subnet != networkReport.IPv4Subnet {
		return nil, failf(CheckAgentEgress, "provider network changed before writer execution")
	}
	switch classifyEvidence(
		st.network,
		st.ownershipLabel,
		currentNetwork.CreationDate,
		currentNetwork.Labels,
		currentNetwork.LabelsObserved,
	) {
	case evidenceOurs:
	case evidenceForeign:
		return nil, failf(CheckAgentEgress, "provider network was replaced before writer execution")
	case evidenceUnprovable:
		return nil, failf(CheckAgentEgress, "provider network identity became unprovable before writer execution")
	}
	st.agent.fingerprint, err = ownedFingerprint(agentRep.CreationDate, agentRep.Labels, agentRep.LabelsObserved, ownershipLabel)
	if err != nil {
		return nil, failf(CheckControlPlaneIsolation, "agent container %q: %v", names.Agent, err)
	}
	// Re-verify the lease at the last instant before mutation ability exists:
	// a takeover (bumped fence) or lapse between acquisition and here means
	// another holder may already be mutating the store, and starting the
	// writer would break §5.4's serialization the moment it refreshes.
	if hs.AuthStoreLease != nil {
		if err := b.verifyAuthStoreLeaseLive(ctx, st); err != nil {
			return nil, err
		}
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
	if err := st.proxy.Close(); err != nil {
		return nil, failf(CheckAgentEgress, "provider proxy failed while the writer ran: %v", err)
	}
	st.proxy = nil

	// The post-writer credential digest is taken only after the writer is
	// proven absent, so what it attests is the store as the writer left it,
	// read by a VM that could not have written it.
	var credPostDigest string
	if hs.AuthStoreLease != nil {
		credPostDigest, err = b.observeCredentialStore(ctx, hs, names.CredObsPost, st, &st.credObsPost)
		if err != nil {
			return nil, err
		}
	}

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
	if err := b.materializeRootFS(ctx, names.Exporter, tarPath, CheckExportVerification); err != nil {
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
		Admission:         adm,
		ExportDir:         out.Dir,
		Manifest:          out.Manifest,
		Evidence:          out.Evidence,
		EvidencePresent:   out.EvidencePresent,
		CommitPlanPresent: out.CommitPlanPresent,
		Workspace: WorkspaceObservation{
			Volume: names.Workspace,
			// Seeded is derived from the observation having been made, not from
			// the request: observeSeededBase returns a value only after the
			// proof validated and matched, and fails the run otherwise.
			Seeded:          observedBaseSHA != "",
			ObservedBaseSHA: observedBaseSHA,
		},
		Egress: EgressObservation{
			Profile:        domain.EgressProviderOnly,
			Network:        networkReport.Name,
			HostOnly:       networkReport.Mode == NetworkHostOnly,
			ProxyAuthority: mustProxyAddress(proxyURL),
		},
		AuthStore: authStoreObservation(hs, st, credPostDigest),
	}, nil
}

// authStoreObservation assembles the §5.4 audit record from the acquired
// lease and the two observed digests; a run with no lease claim reports the
// zero value.
func authStoreObservation(hs HandoffSpec, st *runState, postDigest string) AuthStoreObservation {
	if hs.AuthStoreLease == nil {
		return AuthStoreObservation{}
	}
	return AuthStoreObservation{
		Leased:         true,
		AuthIdentityID: st.lease.AuthIdentityID,
		Holder:         st.lease.Holder,
		Fence:          st.lease.Fence,
		AcquiredAt:     st.lease.AcquiredAt,
		ExpiresAt:      st.lease.ExpiresAt,
		PreDigest:      st.credPreDigest,
		PostDigest:     postDigest,
		PostAttested:   true,
		Mutated:        st.credPreDigest != postDigest,
	}
}

// observeCredentialStore runs one credential-store observer under the given
// name and returns the digest its proof attests. The observation mirrors
// observeSeededBase's discipline: taken by a different VM than any writer,
// through a read-only mount, reaching the host as bytes in the observer's own
// exported root filesystem, bound to this run by the unpredictable nonce, and
// the observer is proven absent before the flow continues.
func (b *Backend) observeCredentialStore(ctx context.Context, hs HandoffSpec, name string, st *runState, claim *objectClaim) (string, error) {
	spec := buildCredentialObserverSpec(b.cfg, hs, name, st.ownershipLabel)
	claim.attempted = true
	if err := b.rt.CreateContainer(ctx, cloneContainerSpec(spec)); err != nil {
		return "", failf(CheckAuthStoreMutationLease, "create credential observer container: %v", err)
	}
	claim.owned = true
	rep, err := b.rt.Inspect(ctx, name)
	if err != nil {
		return "", failf(CheckAuthStoreMutationLease, "inspect credential observer before execution: %v", err)
	}
	if err := verifyCredentialObserverAllowlist(rep, spec); err != nil {
		return "", err
	}
	claim.fingerprint, err = ownedFingerprint(rep.CreationDate, rep.Labels, rep.LabelsObserved, st.ownershipLabel)
	if err != nil {
		return "", failf(CheckAuthStoreMutationLease, "credential observer container %q: %v", name, err)
	}
	if err := b.rt.StartContainer(ctx, name); err != nil {
		return "", failf(CheckAuthStoreMutationLease, "start credential observer container: %v", err)
	}
	if err := b.waitStopped(ctx, name, *claim, st.ownershipLabel, b.cfg.SeedTimeout); err != nil {
		return "", failf(CheckAuthStoreMutationLease, "credential observer: %v", err)
	}

	digest, err := b.readCredProof(ctx, hs.RunID, name, st)
	if err != nil {
		return "", err
	}

	if err := b.rt.DeleteContainer(ctx, name); err != nil {
		return "", failf(CheckAuthStoreMutationLease, "delete stopped credential observer: %v", err)
	}
	if err := b.verifyContainerAbsent(ctx, name, *claim, st.ownershipLabel, CheckAuthStoreMutationLease); err != nil {
		return "", err
	}
	*claim = objectClaim{}
	return digest, nil
}

// readCredProof collects the credential observer's proof out of its stopped
// root filesystem and validates it, under the same byte cap and
// evidence-not-output handling as the base proof.
func (b *Backend) readCredProof(ctx context.Context, runID, id string, st *runState) (string, error) {
	dir, err := os.MkdirTemp("", "freeside-handoff-"+runID+"-cred-")
	if err != nil {
		return "", failf(CheckAuthStoreMutationLease, "create credential proof directory: %v", err)
	}
	st.credArchiveDir = dir
	defer func() {
		_ = os.RemoveAll(dir) // best-effort; the deferred teardown removes it again
		st.credArchiveDir = ""
	}()
	tarPath := filepath.Join(dir, "observer.tar")
	if err := b.materializeRootFS(ctx, id, tarPath, CheckAuthStoreMutationLease); err != nil {
		return "", err
	}
	f, err := os.Open(tarPath) //nolint:gosec // gate-owned path under a fresh temp directory
	if err != nil {
		return "", failf(CheckAuthStoreMutationLease, "open credential proof archive: %v", err)
	}
	defer f.Close() //nolint:errcheck // read-only handle on a temp file removed above
	data, found, err := extractArchiveRegularFile(f, b.cfg.CredProofPath, maxBaseProofBytes)
	if err != nil {
		return "", failf(CheckAuthStoreMutationLease, "read credential proof from observer rootfs: %v", err)
	}
	if !found {
		return "", failf(CheckAuthStoreMutationLease, "credential observer produced no proof")
	}
	return verifyCredProof(data, st.ownershipLabel.Value)
}

func mustProxyAddress(proxyURL string) string {
	address, _ := proxyAddress(proxyURL)
	return address
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
// materializeRootFS streams one stopped container's root filesystem to a
// host-side archive under the byte cap. The check names the assertion the
// caller is proving, so the same bounded collection serves the export path and
// the base observation without either borrowing the other's failure vocabulary.
func (b *Backend) materializeRootFS(ctx context.Context, id, tarPath string, c Check) error {
	f, err := os.OpenFile(tarPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // gate-owned path under a fresh temp directory
	if err != nil {
		return failf(c, "create bounded rootfs archive: %v", err)
	}
	w := &archiveCapWriter{dest: f, remaining: b.cfg.MaxArchiveBytes}
	exportErr := b.rt.ExportRootFS(ctx, id, w, b.cfg.MaxArchiveBytes)
	closeErr := f.Close()
	if w.overflow {
		return failf(c, "exported rootfs archive exceeds the byte cap")
	}
	if exportErr != nil {
		return failf(c, "export stopped container rootfs: %v", exportErr)
	}
	if closeErr != nil {
		return failf(c, "close bounded rootfs archive: %v", closeErr)
	}
	return nil
}

// leaseWindowMargin pads the acquired mutation window past the handoff and
// teardown budgets, so the release stamp of a run that spends its entire
// budget still lands inside the window it ends (the store refuses a release
// outside the window).
const leaseWindowMargin = time.Minute

// acquireAuthStoreLease takes the claimed identity's §5.4 mutation lease and
// verifies the granted window covers this run's whole budget. The gate sizes
// and verifies the window itself: only it knows the handoff deadline and
// when the writer is provably absent, so a caller-estimated window could
// lapse mid-run and hand the serialization point to a second holder while
// this run's writer can still mutate the store. Store refusals (a held
// lease, an undeclared identity) are wrapped operational errors, so their
// typed causes stay reachable with errors.Is; the gate's own verification
// refusals are CheckAuthStoreMutationLease conformance failures.
func (b *Backend) acquireAuthStoreLease(ctx context.Context, claim AuthStoreLeaseClaim, st *runState) error {
	if b.cfg.AuthStoreLeaser == nil {
		return failf(CheckAuthStoreMutationLease,
			"spec claims the auth-store mutation lease for identity %q but no leaser is configured", claim.AuthIdentityID)
	}
	// The store serializes distinct holders; two concurrent handoffs reusing
	// one holder ID would instead converge on one window and both write the
	// same store. The gate's stated posture is that a caller cannot break
	// §5.4 serialization by what it asserts, so the backend holds one
	// in-process slot per identity (freed when the run ends; the store's
	// window remains the cross-restart authority).
	b.leaseMu.Lock()
	if b.activeLeases[claim.AuthIdentityID] {
		b.leaseMu.Unlock()
		return failf(CheckAuthStoreMutationLease,
			"identity %q already has a live leased handoff in this process", claim.AuthIdentityID)
	}
	b.activeLeases[claim.AuthIdentityID] = true
	b.leaseMu.Unlock()
	st.leaseSlot = true
	st.leaseIdentity = claim.AuthIdentityID
	now := b.cfg.Now()
	wantExpiry := now.Add(b.cfg.HandoffTimeout + b.cfg.TeardownTimeout + leaseWindowMargin)
	lease, err := b.cfg.AuthStoreLeaser.Acquire(ctx, claim.AuthIdentityID, claim.Holder, now, wantExpiry)
	if err != nil {
		return fmt.Errorf("acquire auth store mutation lease for identity %q: %w", claim.AuthIdentityID, err)
	}
	// The returned lease is store output, not gate state: re-verify it names
	// this claim and actually covers the budget before anything trusts it. A
	// same-holder re-acquire converges on an existing window without
	// extending it, so a reused holder ID with a shorter window lands here.
	if err := lease.Validate(); err != nil {
		return failf(CheckAuthStoreMutationLease, "acquired lease for identity %q is malformed: %v", claim.AuthIdentityID, err)
	}
	if lease.AuthIdentityID != claim.AuthIdentityID || lease.Holder != claim.Holder {
		return failf(CheckAuthStoreMutationLease,
			"acquired lease does not name the claimed identity %q and holder", claim.AuthIdentityID)
	}
	if !lease.HeldAt(now) {
		return failf(CheckAuthStoreMutationLease, "acquired lease for identity %q is not live at acquisition", claim.AuthIdentityID)
	}
	// A window acquired at any earlier instant is a same-holder convergence
	// on a still-live lease (a crashed run's window, or a concurrent run
	// under a reused holder ID): this run did not open it and must not ride
	// it. Recovery or expiry ends the old window first.
	if !lease.AcquiredAt.Equal(now) {
		return failf(CheckAuthStoreMutationLease,
			"identity %q already holds a mutation window from an earlier acquisition", claim.AuthIdentityID)
	}
	if lease.ExpiresAt.Before(wantExpiry) {
		// Every earlier rejection refuses a window this run cannot prove it
		// opened; this one comes after the freshness check, so the short
		// window is provably ours and is released rather than abandoned
		// held until its expiry blocks other holders.
		st.lease, st.leaseHeld = lease, true
		reason := fmt.Sprintf("acquired lease for identity %q ends at %s, before the run's budget needs it",
			claim.AuthIdentityID, lease.ExpiresAt)
		if problems := b.releaseAuthStoreLease(ctx, st); len(problems) > 0 {
			reason += "; " + strings.Join(problems, "; ")
		}
		return failf(CheckAuthStoreMutationLease, "%s", reason)
	}
	st.lease = lease
	st.leaseHeld = true
	return nil
}

// verifyAuthStoreLeaseLive re-reads the lease immediately before the writer
// starts: the row must be the acquired window exactly, and still live. The
// window was sized to cover the whole budget, so a failure here is a
// takeover or an incoherent store row, not an expected lapse; either way the
// writer must not start. The re-read row crosses the same trust boundary as
// the acquisition's return, so it is held to the same gate: validated shape
// and the exact acquired window, never just the holder and fence — a row
// naming another identity, or a same-holder-and-fence row with a moved
// window, is store output contradicting gate state, and the writer must not
// start over it.
func (b *Backend) verifyAuthStoreLeaseLive(ctx context.Context, st *runState) error {
	current, err := b.cfg.AuthStoreLeaser.Get(ctx, st.lease.AuthIdentityID)
	if err != nil {
		return fmt.Errorf("re-verify auth store mutation lease for identity %q: %w", st.lease.AuthIdentityID, err)
	}
	if verr := current.Validate(); verr != nil {
		return failf(CheckAuthStoreMutationLease,
			"re-read lease for identity %q is malformed: %v", st.lease.AuthIdentityID, verr)
	}
	if current.AuthIdentityID != st.lease.AuthIdentityID {
		return failf(CheckAuthStoreMutationLease,
			"re-read lease names identity %q, not the acquired %q", current.AuthIdentityID, st.lease.AuthIdentityID)
	}
	if current.Holder != st.lease.Holder || current.Fence != st.lease.Fence ||
		!current.AcquiredAt.Equal(st.lease.AcquiredAt) || !current.ExpiresAt.Equal(st.lease.ExpiresAt) {
		return failf(CheckAuthStoreMutationLease,
			"lease for identity %q was taken over before the writer started", st.lease.AuthIdentityID)
	}
	if !current.HeldAt(b.cfg.Now()) {
		return failf(CheckAuthStoreMutationLease,
			"lease for identity %q is no longer live before the writer started", st.lease.AuthIdentityID)
	}
	return nil
}

// releaseAuthStoreLease ends the run's §5.4 mutation window; teardown calls
// it once the writer is provably gone (or was never created). A failed
// release is a teardown problem, never silent: until the row is released or
// expires, the identity stays serialized against every other holder. A crash
// that skips teardown leaves the persisted store row for recovery to release,
// or expiry to lapse.
func (b *Backend) releaseAuthStoreLease(ctx context.Context, st *runState) []string {
	if !st.leaseHeld {
		return nil
	}
	// The release must reach the store even when the run's context is
	// already cancelled or past its deadline: a window outliving the run
	// blocks the identity until expiry, so the one call that ends it runs
	// detached, bounded by its own teardown-sized budget (teardown detaches
	// the same way, so its calls re-bound harmlessly).
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), b.cfg.TeardownTimeout)
	defer cancel()
	if err := b.cfg.AuthStoreLeaser.Release(ctx, st.lease.AuthIdentityID, st.lease.Holder, st.lease.Fence, b.cfg.Now()); err != nil {
		return []string{fmt.Sprintf("release auth store mutation lease for identity %q: %v", st.lease.AuthIdentityID, err)}
	}
	st.leaseHeld = false
	return nil
}

// freeLeaseSlot returns the run's in-process per-identity slot; a no-op for
// runs that never took one.
func (b *Backend) freeLeaseSlot(st *runState) {
	if !st.leaseSlot {
		return
	}
	b.leaseMu.Lock()
	delete(b.activeLeases, st.leaseIdentity)
	b.leaseMu.Unlock()
	st.leaseSlot = false
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
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), b.cfg.TeardownTimeout)
	defer cancel()
	// The in-process per-identity slot frees when this run ends, however the
	// window itself ended: cross-run serialization is the store's (a live
	// window refuses other holders; a same-holder convergence is refused at
	// acquisition), so holding the slot past the run would only wedge the
	// identity in-process.
	defer b.freeLeaseSlot(st)
	// Before the first create attempt this invocation owns no runtime
	// object, but it may already hold the §5.4 lease: acquisition precedes
	// the first create so the window covers everything, and an early refusal
	// must not leave the identity serialized until expiry.
	if !st.workspace.attempted {
		if problems := b.releaseAuthStoreLease(ctx, st); len(problems) > 0 {
			return failf(CheckTeardown, "%s", strings.Join(problems, "; "))
		}
		return nil
	}
	var problems []string
	if st.proxy != nil {
		if err := st.proxy.Close(); err != nil {
			problems = append(problems, fmt.Sprintf("close provider proxy: %v", err))
		}
		st.proxy = nil
	}

	type containerClaim struct {
		id    string
		claim objectClaim
	}
	containerClaims := []containerClaim{
		{id: names.Seeder, claim: st.seeder},
		{id: names.Observer, claim: st.observer},
		{id: names.CredObsPre, claim: st.credObsPre},
		{id: names.CredObsPost, claim: st.credObsPost},
		{id: names.Agent, claim: st.agent},
		{id: names.Exporter, claim: st.exporter},
	}
	// Derived from the claim list rather than naming roles: a failure during
	// seeding leaves the agent and exporter unattempted, so a role-by-role
	// condition would skip both the reap sweep below and the survival re-proof
	// after it, and a seeder holding the workspace read-write would survive
	// teardown unnoticed.
	anyContainerAttempted := slices.ContainsFunc(containerClaims, func(c containerClaim) bool {
		return c.claim.attempted
	})
	// Every candidate, owned or ambiguous, is reaped only on fresh evidence
	// that it is still the object this invocation created (its captured
	// creation fingerprint corroborated by the unpredictable ownership label,
	// else the label alone). A foreign verdict — a collision or a same-name
	// replacement — leaves the object untouched; an unprovable one withholds
	// the delete and fails teardown.
	if anyContainerAttempted {
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
	if st.network.attempted {
		if err := b.teardownNetwork(ctx, names.Network, st.network, st.ownershipLabel); err != nil {
			problems = append(problems, err.Error())
		}
	}

	// Prove absence: nothing the run owns may survive the reap (a delete that
	// reported success but left the object is caught here). A surviving
	// same-name row classified foreign is a replacement that appeared after
	// this run's object was reaped: it counts as absent and is never
	// re-reaped; only an unprovable row still fails the proof.
	//
	// writerGone tracks whether the credential-bearing writer is provably
	// absent on this pass's fresh evidence: an agent claim cleared mid-flow
	// was already proven absent there, and an attempted one must be proven
	// here. The §5.4 lease release below keys off it.
	writerGone := !st.agent.attempted
	if anyContainerAttempted {
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
					if c.id == names.Agent {
						writerGone = true
					}
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
					if c.id == names.Agent {
						writerGone = true
					}
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

	// The lease releases last, after the reap and the survival re-proof: the
	// §5.4 window must outlive every state in which this run's writer could
	// still mutate the store. A writer whose absence this pass could not
	// prove keeps the window held — releasing it would invite a second
	// holder to mutate beside a possibly-live writer — and the held window
	// is recorded as its own teardown problem; expiry remains the backstop.
	if writerGone {
		problems = append(problems, b.releaseAuthStoreLease(ctx, st)...)
	} else if st.leaseHeld {
		problems = append(problems, fmt.Sprintf(
			"auth store mutation lease for identity %q kept held: writer absence unproven", st.lease.AuthIdentityID))
	}

	if len(problems) > 0 {
		return failf(CheckTeardown, "%s", strings.Join(problems, "; "))
	}
	return nil
}

func (b *Backend) teardownNetwork(ctx context.Context, name string, claim objectClaim, ownershipLabel Label) error {
	networks, err := b.rt.ListNetworks(ctx)
	if err != nil {
		report, inspectErr := b.rt.InspectNetwork(ctx, name)
		if inspectErr != nil {
			return errors.Join(
				fmt.Errorf("list networks: %w", err),
				fmt.Errorf("inspect network %q: %w", name, inspectErr),
			)
		}
		if report.Name != name {
			return fmt.Errorf("inspect network %q returned the wrong identity", name)
		}
		switch classifyEvidence(claim, ownershipLabel, report.CreationDate, report.Labels, report.LabelsObserved) {
		case evidenceOurs:
			if deleteErr := b.rt.DeleteNetwork(ctx, name); deleteErr != nil {
				return fmt.Errorf("delete network %q after list failure: %w", name, deleteErr)
			}
			remaining, relistErr := b.rt.ListNetworks(ctx)
			if relistErr != nil {
				return fmt.Errorf("re-list networks after direct delete: %w", relistErr)
			}
			return verifyNetworkAbsent(remaining, name, claim, ownershipLabel)
		case evidenceForeign:
			return nil
		case evidenceUnprovable:
			return fmt.Errorf("network %q ownership unprovable; not deleting", name)
		}
	}
	candidate, found, err := uniqueNetwork(networks, name)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	switch classifyEvidence(claim, ownershipLabel, candidate.CreationDate, candidate.Labels, candidate.LabelsObserved) {
	case evidenceOurs:
		if err := b.rt.DeleteNetwork(ctx, name); err != nil {
			return fmt.Errorf("delete network %q: %w", name, err)
		}
	case evidenceForeign:
		return nil
	case evidenceUnprovable:
		return fmt.Errorf("network %q ownership unprovable; not deleting", name)
	}
	remaining, err := b.rt.ListNetworks(ctx)
	if err != nil {
		return fmt.Errorf("re-list networks: %w", err)
	}
	return verifyNetworkAbsent(remaining, name, claim, ownershipLabel)
}

func verifyNetworkAbsent(networks []NetworkSummary, name string, claim objectClaim, ownershipLabel Label) error {
	candidate, found, err := uniqueNetwork(networks, name)
	if err != nil {
		return err
	}
	if found && classifyEvidence(claim, ownershipLabel, candidate.CreationDate, candidate.Labels, candidate.LabelsObserved) != evidenceForeign {
		return fmt.Errorf("network %q survived teardown or has unprovable ownership", name)
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

func uniqueNetwork(networks []NetworkSummary, name string) (NetworkSummary, bool, error) {
	var found NetworkSummary
	seen := false
	for _, network := range networks {
		if network.Name != name {
			continue
		}
		if seen {
			return NetworkSummary{}, false, fmt.Errorf("network %q appeared more than once in runtime listing", name)
		}
		found, seen = network, true
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
