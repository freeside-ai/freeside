package ward

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/export"
)

// RecoveryOutcome is how a recovery ended. The zero value "" is invalid by
// design.
type RecoveryOutcome string

const (
	// RecoveryExported: the handoff was adopted to completion and a freshly
	// verified export released.
	RecoveryExported RecoveryOutcome = "exported"
	// RecoveryFailed: a durable nonce-authenticated nonzero writer status was
	// recovered and teardown was proven.
	RecoveryFailed RecoveryOutcome = "failed"
	// RecoveryCanceled: durable daemon cancellation intent outranked marker
	// classification and teardown was proven.
	RecoveryCanceled RecoveryOutcome = "canceled"
	// RecoveryLoss: every runtime object was proven absent, the loss is
	// durably committed, and the caller may rerun the stage from its
	// durable admission.
	RecoveryLoss RecoveryOutcome = "loss"
)

// AllRecoveryOutcomes lists every valid RecoveryOutcome.
var AllRecoveryOutcomes = []RecoveryOutcome{
	RecoveryExported, RecoveryCanceled, RecoveryFailed, RecoveryLoss,
}

func (o RecoveryOutcome) valid() bool {
	switch o {
	case RecoveryExported, RecoveryCanceled, RecoveryFailed, RecoveryLoss:
		return true
	default:
		return false
	}
}

// RecoveryResult is one recovered handoff's outcome. Export fields are
// meaningful only for RecoveryExported; a loss result's rerun-safe signal is
// the nil-error return itself (absence proven, loss durably committed).
type RecoveryResult struct {
	Outcome   RecoveryOutcome
	Admission exec.Admission
	// LossCause names what forced a loss that arose from a failed adoption
	// attempt; empty for a straight pre-writer-complete loss and for
	// exported outcomes.
	LossCause string
	// FailureStatus is populated only for RecoveryFailed.
	FailureStatus int
	// ExportDir holds the freshly verified manifest and blobs; the caller
	// owns the directory and removes it when done.
	ExportDir         string
	Manifest          export.Manifest
	Evidence          export.EvidenceManifest
	EvidencePresent   bool
	CommitPlanPresent bool
	// Workspace carries the journal-attested base identity: the observed
	// base was proven before the writer ran and persisted at that moment;
	// after a restart it cannot be re-attested (the writer may have moved
	// HEAD), so this is the persisted amendment reported as such.
	Workspace WorkspaceObservation
	// AuthStore is the recovered §5.4 record for a leased run: identity,
	// holder, and fence from the journal, the pre-writer digest from its
	// persisted amendment, and the post-writer digest freshly observed
	// during adoption. Window timestamps are zero — they died with the
	// process, and the store's lease row remains the authority on them.
	AuthStore AuthStoreObservation
}

// Recover reconciles one journalled handoff after a daemon restart: its
// runtime objects are either adopted to a freshly verified export or torn
// down with a durably committed loss, using the persisted ownership token as
// the only ownership evidence. Recover reads the run's record from the
// journal itself — a caller-supplied copy could carry a stale WriterComplete
// or Outcome, a decoded trust bit steering adoption — while the caller
// re-supplies only the HandoffSpec from its durable admission (the
// StartSpecFromAdmission principle: rebuild from the durable record, never
// from current policy); the record's spec digest binds the two, so a
// diverged record cannot resume under a different spec. Concurrent execution
// against the same run — a live Handoff or a second Recover — is the
// caller's to serialize; the journal's closed-record refusal makes a
// completed double recovery refuse, not race.
//
// The decision collapses on the record's WriterComplete mark. Without it,
// nothing is adoptable: the daemon-side CONNECT proxy died with the process,
// so a still-running writer has no enforced egress, and even a stopped one
// lacks the proxy-healthy-throughout evidence — adopting would deliver work
// below the gate's proof floor, the same trap as the refuted same-VM
// fallback class. Everything pre-writer-complete is torn down and committed
// as loss. With the mark, the still-read-only workspace is adopted only if
// it is provably this run's, and every release check (4 and 7) is earned
// freshly by a new exporter: an exporter a dead process started has an
// unpersisted pre-execution inspection and is reaped, never trusted.
//
// No path releases an unverified export; no loss is committed until the
// ownership-label audit proves absence on fresh evidence; a recovery error
// commits nothing, leaves the record open, and is retryable.
func (b *Backend) Recover(ctx context.Context, runID string, hs HandoffSpec) (result *RecoveryResult, err error) {
	// The recovery budget bounds everything, including the durable reads: a
	// journal or lease-store adapter that honors its context but stalls
	// would otherwise block restart recovery indefinitely.
	ctx, cancel := context.WithTimeout(ctx, b.cfg.HandoffTimeout)
	defer cancel()
	// Refusals first: nothing below may run a destructive call until the
	// record and spec are re-gated.
	if b.cfg.Journal == nil {
		return nil, failf(CheckRecovery, "no journal is configured; recovery has nothing durable to commit to")
	}
	rec, err := b.cfg.Journal.Get(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("journal get %q: %w", runID, err)
	}
	if rec.RunID != runID {
		return nil, fmt.Errorf("%w: journal returned record for run %q, asked for %q", ErrInvalidJournalRecord, rec.RunID, runID)
	}
	if err := rec.Validate(); err != nil {
		return nil, err
	}
	hs.Agent.Command = slices.Clone(hs.Agent.Command)
	hs.Agent.Env = slices.Clone(hs.Agent.Env)
	hs.Agent.CredentialMounts = slices.Clone(hs.Agent.CredentialMounts)
	hs.Agent.VendorInstructions.Body = slices.Clone(hs.Agent.VendorInstructions.Body)
	hs.Agent.InstructionPolicy.Boundaries = slices.Clone(
		hs.Agent.InstructionPolicy.Boundaries,
	)
	if err := hs.validate(); err != nil {
		return nil, err
	}
	if hs.RunID != rec.RunID {
		return nil, fmt.Errorf("%w: re-supplied spec names run %q, record names %q", ErrInvalidJournalRecord, hs.RunID, rec.RunID)
	}
	digest, err := specDigest(hs)
	if err != nil {
		return nil, err
	}
	if digest != rec.SpecDigest {
		legacyDigest, lerr := legacySpecDigest(hs)
		if lerr != nil {
			return nil, lerr
		}
		if legacyDigest != rec.SpecDigest {
			return nil, fmt.Errorf("%w: re-supplied spec does not match the record's spec digest", ErrInvalidJournalRecord)
		}
		// The legacy digest did not bind vendor bytes. Only the one safe
		// compatibility value is admissible: an explicit empty overlay that
		// masks any image-baked instruction. InstructionPolicy has only one
		// valid semantic value, already enforced by hs.validate above.
		if hs.Agent.VendorInstructions.Present {
			return nil, fmt.Errorf(
				"%w: legacy spec digest cannot authorize present vendor instructions",
				ErrInvalidJournalRecord,
			)
		}
	}
	if (hs.AuthStoreLease == nil) != (rec.Lease == nil) {
		return nil, fmt.Errorf("%w: record and spec disagree on the auth-store lease", ErrInvalidJournalRecord)
	}
	// The persisted lease reference must name the digest-bound claim's
	// identity and holder: a damaged or swapped row carrying some other
	// identity's live holder and fence would otherwise ride into runState,
	// and the release after teardown would end an unrelated identity's
	// window beside its real writer. The fence has no spec-side counterpart
	// (it is store-assigned at acquisition), and the store re-verifies
	// holder and fence together on release.
	if rec.Lease != nil &&
		(rec.Lease.AuthIdentityID != hs.AuthStoreLease.AuthIdentityID || rec.Lease.Holder != hs.AuthStoreLease.Holder) {
		return nil, fmt.Errorf("%w: record's lease does not name the spec's claimed identity and holder", ErrInvalidJournalRecord)
	}
	if rec.Lease != nil && b.cfg.AuthStoreLeaser == nil {
		return nil, failf(CheckRecovery, "record holds an auth-store lease but no leaser is configured")
	}
	if rec.Lease != nil && rec.WriterComplete && rec.CredentialPreDigest == "" {
		// A leased writer cannot have completed without the pre-writer
		// digest: it is journalled before the writer may start.
		return nil, fmt.Errorf("%w: writer-complete leased record carries no credential pre-digest", ErrInvalidJournalRecord)
	}
	if rec.WriterComplete && hs.Seed.Mode == SeedBaseCheckout && rec.ObservedBaseSHA == "" {
		// Symmetric with the pre-digest rule: a seeded writer cannot have
		// completed without the base observation (it is journalled before
		// the writer may start), and the observation cannot be recreated
		// after the writer ran. Adopting such a record would release a
		// workspace whose exact-base evidence is missing.
		return nil, fmt.Errorf("%w: writer-complete seeded record carries no observed base", ErrInvalidJournalRecord)
	}
	postPreparation := rec.CredentialPreDigest != "" ||
		rec.WriterComplete || rec.WriterFailureStatus != nil
	if hs.Agent.LaunchState == LaunchStateClaudeClean &&
		rec.State != nil && rec.Instructions == nil {
		return nil, fmt.Errorf(
			"%w: Claude state binding precedes no prepared instruction binding",
			ErrInvalidJournalRecord,
		)
	}
	if hs.Agent.LaunchState == LaunchStateClaudeClean &&
		postPreparation && rec.State == nil {
		return nil, fmt.Errorf(
			"%w: post-preparation Claude record carries no prepared state binding",
			ErrInvalidJournalRecord,
		)
	}
	if hs.Agent.LaunchState == LaunchStateClaudeClean &&
		postPreparation && rec.Instructions == nil {
		return nil, fmt.Errorf(
			"%w: post-preparation Claude record carries no prepared instruction binding",
			ErrInvalidJournalRecord,
		)
	}
	if hs.Agent.LaunchState == LaunchStateNone &&
		(rec.State != nil || rec.Instructions != nil) {
		return nil, fmt.Errorf(
			"%w: state-free spec carries prepared Claude launch bindings",
			ErrInvalidJournalRecord,
		)
	}
	// A present observation must be the digest-bound declared base, exactly:
	// the live path refuses a mismatch before ever journalling it, so any
	// other value — including a syntactically valid SHA on a blank-seed
	// record, which never observes a base at all — is a damaged row that
	// would otherwise be reported as this run's attested base.
	if rec.ObservedBaseSHA != "" &&
		(hs.Seed.Mode != SeedBaseCheckout || rec.ObservedBaseSHA != hs.Seed.Base.BaseSHA) {
		return nil, fmt.Errorf("%w: record's observed base does not match the spec's declared base", ErrInvalidJournalRecord)
	}
	if rec.Lease != nil {
		boundVolume, verr := b.cfg.AuthStoreLeaser.AuthStoreVolume(ctx, rec.Lease.AuthIdentityID)
		if verr != nil {
			return nil, fmt.Errorf("re-gate auth-store volume for identity %q: %w",
				rec.Lease.AuthIdentityID, verr)
		}
		if mounted := hs.leasedCredentialVolume(); boundVolume != mounted {
			return nil, fmt.Errorf(
				"%w: identity %q is bound to auth-store volume %q, not the record's credential mount %q",
				ErrInvalidJournalRecord, rec.Lease.AuthIdentityID, boundVolume, mounted)
		}
	}
	// Base capabilities only: suite-earned flags were cleared by the restart
	// and must not be assumed (§5.7 reconstructs gating state from persisted
	// evidence, and recovery asserts nothing a fresh check does not prove).
	adm, err := exec.CheckCapabilities(b, requiredCapabilities)
	if err != nil {
		return nil, err
	}
	if rec.Outcome != nil {
		switch *rec.Outcome {
		case HandoffLoss:
			return &RecoveryResult{Outcome: RecoveryLoss, Admission: adm}, nil
		case HandoffFailed:
			if rec.WriterFailureStatus == nil {
				return nil, fmt.Errorf("%w: failed record lacks writer status",
					ErrInvalidJournalRecord)
			}
			return &RecoveryResult{
				Outcome: RecoveryFailed, Admission: adm,
				FailureStatus: *rec.WriterFailureStatus,
			}, nil
		case HandoffCanceled:
			if !rec.CancellationRequested {
				return nil, fmt.Errorf("%w: canceled record lacks cancellation intent",
					ErrInvalidJournalRecord)
			}
			return &RecoveryResult{Outcome: RecoveryCanceled, Admission: adm}, nil
		case HandoffCompleted:
			if !rec.WriterComplete || rec.ExportDir == "" {
				return nil, fmt.Errorf(
					"%w: completed record lacks writer completion or export location",
					ErrInvalidJournalRecord,
				)
			}
			if err := validateMaterializedExportPath(
				b.cfg.ExportRoot, rec.RunID, rec.ExportDir,
			); err != nil {
				return nil, err
			}
			out, err := b.verifyMaterializedExport(ctx, rec.ExportDir)
			if err != nil {
				return nil, fmt.Errorf("re-verify completed handoff: %w", err)
			}
			return &RecoveryResult{
				Outcome:           RecoveryExported,
				Admission:         adm,
				ExportDir:         out.Dir,
				Manifest:          out.Manifest,
				Evidence:          out.Evidence,
				EvidencePresent:   out.EvidencePresent,
				CommitPlanPresent: out.CommitPlanPresent,
				Workspace: WorkspaceObservation{
					Volume:          namesFor(rec.RunID).Workspace,
					Seeded:          rec.ObservedBaseSHA != "",
					ObservedBaseSHA: rec.ObservedBaseSHA,
				},
			}, nil
		}
	}

	names := namesFor(rec.RunID)
	// The synthetic state: fingerprintless attempted claims, so every
	// evidence decision degrades to the persisted token exactly as teardown's
	// evidence rules define for an ambiguous claim. The held lease re-enters
	// through the same runState, but its release waits for the full-token
	// audit rather than teardown's name-keyed writer check (see the defer).
	st := &runState{ownershipLabel: Label{Key: ownershipLabelKey, Value: rec.OwnershipToken}}
	for _, claim := range []*objectClaim{
		&st.workspace, &st.instructions,
		&st.seeder, &st.observer, &st.instructionSeeder, &st.instructionObserver,
		&st.credObsPre, &st.credObsPost, &st.agent, &st.exporter, &st.network,
	} {
		claim.attempted = true
	}
	if hs.Agent.LaunchState == LaunchStateClaudeClean {
		for _, claim := range []*objectClaim{
			&st.configRoot, &st.continuity, &st.sessionScratch,
			&st.configRootSeeder, &st.configRootObserver,
			&st.continuityObserver, &st.scratchObserver,
		} {
			claim.attempted = true
		}
		if rec.State != nil {
			st.preparedState = rec.State
			st.configRoot.fingerprint = rec.State.ConfigRootFingerprint
			st.continuity.fingerprint = rec.State.ContinuityFingerprint
			st.sessionScratch.fingerprint = rec.State.SessionScratchFingerprint
		}
		st.preparedInstructions = rec.Instructions
	}
	if rec.Lease != nil {
		// Re-gate the recorded window against the live store row before any
		// release can ride it: identity and holder alone cannot distinguish
		// this run's window from a later run's (sequential same-holder
		// acquisition is legal), and a damaged row can carry a later
		// window's fence — or an ordering claim like OpenedAt — pointing at
		// that run's live window beside its writer. The binding is
		// therefore exact equality between the recorded window and the live
		// row (holder, fence, AcquiredAt, ExpiresAt), with the live row as
		// trusted current state; no decoded ordering claim is load-bearing.
		// Any mismatch, or a lapsed window, means there is nothing of this
		// run's to release, and the window is left alone.
		current, gerr := b.cfg.AuthStoreLeaser.Get(ctx, rec.Lease.AuthIdentityID)
		if gerr != nil {
			return nil, fmt.Errorf("re-gate recorded lease for identity %q: %w", rec.Lease.AuthIdentityID, gerr)
		}
		// The returned row crosses the same trust boundary as every other
		// leaser read. A malformed row, or one naming another identity, is an
		// incoherent store: no decision downstream of it (release, adoption,
		// observation) can be trusted, so recovery fails closed and stays
		// retryable rather than proceeding as though the window were simply
		// not this run's.
		if verr := current.Validate(); verr != nil {
			return nil, fmt.Errorf("re-gate recorded lease for identity %q: store row is malformed: %w", rec.Lease.AuthIdentityID, verr)
		}
		if current.AuthIdentityID != rec.Lease.AuthIdentityID {
			return nil, fmt.Errorf("re-gate recorded lease for identity %q: store row names identity %q",
				rec.Lease.AuthIdentityID, current.AuthIdentityID)
		}
		if current.Holder == rec.Lease.Holder && current.Fence == rec.Lease.Fence &&
			current.AcquiredAt.Equal(rec.Lease.AcquiredAt) && current.ExpiresAt.Equal(rec.Lease.ExpiresAt) &&
			current.HeldAt(b.cfg.Now()) {
			st.lease = current
			st.leaseHeld = true
		}
	}

	defer func() {
		// A recovery that decided nothing destroys nothing: on an error
		// return (or a panic) the world stays as observed — every object is
		// labeled and the record stays open, so a retry re-observes it —
		// and only this attempt's host scratch is removed. Tearing down
		// here would trade a still-adoptable workspace for a transient
		// failure that said nothing about it.
		if result == nil {
			for _, dir := range []string{
				st.archiveDir,
				st.baseArchiveDir,
				st.instructionArchiveDir,
				st.stateArchiveDir,
				st.credArchiveDir,
				st.writerArchiveDir,
				st.exportDir,
			} {
				if dir != "" {
					_ = os.RemoveAll(dir)
				}
			}
			return
		}
		// In recovery the lease releases on the audit's full-token absence
		// bar, not teardown's name-keyed one: a token-carrying orphan under
		// an unexpected name would pass teardown's writer check, and a
		// release before the audit could hand the identity to a new holder
		// beside that credential-bearing survivor. Teardown therefore runs
		// release-less here, and the release happens only after the audit
		// proves nothing of the run's survives anywhere.
		recoveredLease := st.leaseHeld
		st.leaseHeld = false
		terr := b.teardown(ctx, names, st)
		if terr != nil {
			result = nil
			if err == nil {
				err = terr
			} else {
				err = errors.Join(err, terr)
			}
		} else if aerr := b.auditRunAbsent(ctx, st.ownershipLabel); aerr != nil {
			// The audit is recovery's own absence bar: beyond teardown's
			// name-keyed proofs, no object anywhere may still carry this
			// run's token. An audit failure voids the outcome — a loss
			// cannot be committed and an export cannot be released over a
			// surviving orphan.
			result = nil
			if err == nil {
				err = aerr
			} else {
				err = errors.Join(err, aerr)
			}
		} else if recoveredLease {
			st.leaseHeld = true
			if problems := b.releaseAuthStoreLease(ctx, st); len(problems) > 0 {
				result = nil
				rerr := failf(CheckRecovery, "%s", strings.Join(problems, "; "))
				if err == nil {
					err = rerr
				} else {
					err = errors.Join(err, rerr)
				}
			}
		}
		// Commit the outcome only from proven state, on a detached bounded
		// context (a cancelled recovery still records a proven end). An
		// uncommitted close leaves the record open and the recovery
		// reports failure: loss is the durable rerun-safe signal and
		// release follows the durable append, so neither exists until the
		// journal row does.
		if err == nil && result != nil {
			outcome := HandoffLoss
			switch result.Outcome {
			case RecoveryExported:
				outcome = HandoffCompleted
			case RecoveryCanceled:
				outcome = HandoffCanceled
			case RecoveryFailed:
				outcome = HandoffFailed
			case RecoveryLoss:
			}
			jctx, jcancel := context.WithTimeout(context.WithoutCancel(ctx), b.cfg.TeardownTimeout)
			if cerr := b.cfg.Journal.Close(jctx, rec.RunID, outcome); cerr != nil {
				result = nil
				err = fmt.Errorf("journal close %s: %w", outcome, cerr)
			}
			jcancel()
		}
		if st.archiveDir != "" {
			_ = os.RemoveAll(st.archiveDir)
		}
		if st.baseArchiveDir != "" {
			_ = os.RemoveAll(st.baseArchiveDir)
		}
		if st.instructionArchiveDir != "" {
			_ = os.RemoveAll(st.instructionArchiveDir)
		}
		if st.stateArchiveDir != "" {
			_ = os.RemoveAll(st.stateArchiveDir)
		}
		if st.credArchiveDir != "" {
			_ = os.RemoveAll(st.credArchiveDir)
		}
		if st.writerArchiveDir != "" {
			_ = os.RemoveAll(st.writerArchiveDir)
		}
		if st.exportDir != "" && (!st.succeeded || err != nil) {
			_ = os.RemoveAll(st.exportDir)
		}
	}()

	if rec.CancellationRequested {
		return &RecoveryResult{Outcome: RecoveryCanceled, Admission: adm}, nil
	}
	if rec.WriterFailureStatus != nil {
		return &RecoveryResult{
			Outcome: RecoveryFailed, Admission: adm,
			FailureStatus: *rec.WriterFailureStatus,
		}, nil
	}
	if !rec.WriterComplete {
		if hs.Agent.OutcomeMarkerPath != "" {
			// A dead daemon cannot recover the proxy-health half of success,
			// but it can still classify failure. Quiesce the exact recorded
			// writer, then authenticate the durable workspace marker.
			if err := b.reapRecoveredContainer(
				ctx, names.Agent, &st.agent, st.ownershipLabel,
			); err != nil {
				return nil, err
			}
			ours, werr := b.workspaceOurs(ctx, names.Workspace, st)
			if werr != nil {
				return nil, werr
			}
			if ours {
				if err := b.reapRecoveredContainer(
					ctx, names.WriterObserver, &st.writerObserver, st.ownershipLabel,
				); err != nil {
					return nil, err
				}
				status, oerr := b.observeWriterOutcome(ctx, hs, names, st)
				if oerr == nil && status != 0 {
					if jerr := b.cfg.Journal.MarkWriterFailed(
						ctx, rec.RunID, status,
					); jerr != nil {
						return nil, fmt.Errorf("journal recovered writer failure: %w", jerr)
					}
					return &RecoveryResult{
						Outcome: RecoveryFailed, Admission: adm,
						FailureStatus: status,
					}, nil
				}
				if oerr != nil {
					var cf *ConformanceFailure
					if !errors.As(oerr, &cf) {
						return nil, oerr
					}
				}
			}
		}
		return &RecoveryResult{Outcome: RecoveryLoss, Admission: adm}, nil
	}

	// The record's writer-complete claim is re-gated against the world as
	// far as the world can answer: completion implies the writer was proven
	// absent, so an ours-classified writer container still standing
	// contradicts the record and refuses adoption outright. (The proxy-
	// health half of the proof is a durable record by design, like the §5.7
	// conformance rows that gate unattended admission: process-local
	// evidence that no re-observation can reconstruct.)
	if err := b.verifyRecordedWriterAbsent(ctx, names.Agent, st.agent, st.ownershipLabel); err != nil {
		return nil, err
	}
	// Adoption requires the workspace to be provably this run's; absent or
	// foreign means there is nothing of this run's to export.
	ours, err := b.workspaceOurs(ctx, names.Workspace, st)
	if err != nil {
		return nil, err
	}
	if !ours {
		return &RecoveryResult{
			Outcome: RecoveryLoss, Admission: adm,
			LossCause: "writer completed but the workspace is no longer provably this run's",
		}, nil
	}
	// A dead process may have left an exporter or post-writer observer under
	// this run's deterministic names; their pre-execution inspections died
	// with it, so they are reaped on fresh ownership evidence and rerun,
	// never trusted.
	if err := b.reapRecoveredContainer(ctx, names.Exporter, &st.exporter, st.ownershipLabel); err != nil {
		return nil, err
	}
	if err := b.reapRecoveredContainer(ctx, names.CredObsPost, &st.credObsPost, st.ownershipLabel); err != nil {
		return nil, err
	}
	if err := b.reapRecoveredContainer(
		ctx, names.WriterObserver, &st.writerObserver, st.ownershipLabel,
	); err != nil {
		return nil, err
	}
	// A damaged or interrupted world can also leave the pre-writer roles
	// standing; they are this run's deterministic-name leftovers, reaped on
	// the same fresh ownership evidence before anything is re-earned.
	if err := b.reapRecoveredContainer(ctx, names.Seeder, &st.seeder, st.ownershipLabel); err != nil {
		return nil, err
	}
	if err := b.reapRecoveredContainer(ctx, names.Observer, &st.observer, st.ownershipLabel); err != nil {
		return nil, err
	}
	if err := b.reapRecoveredContainer(
		ctx, names.InstructionSeeder, &st.instructionSeeder, st.ownershipLabel,
	); err != nil {
		return nil, err
	}
	if err := b.reapRecoveredContainer(
		ctx, names.InstructionObserver, &st.instructionObserver, st.ownershipLabel,
	); err != nil {
		return nil, err
	}
	for _, recovered := range []struct {
		name  string
		claim *objectClaim
	}{
		{names.ConfigRootSeeder, &st.configRootSeeder},
		{names.ConfigRootObserver, &st.configRootObserver},
		{names.ContinuityObserver, &st.continuityObserver},
		{names.ScratchObserver, &st.scratchObserver},
	} {
		if err := b.reapRecoveredContainer(
			ctx, recovered.name, recovered.claim, st.ownershipLabel,
		); err != nil {
			return nil, err
		}
	}
	if err := b.reapRecoveredContainer(ctx, names.CredObsPre, &st.credObsPre, st.ownershipLabel); err != nil {
		return nil, err
	}
	// After the deterministic names are settled, no container anywhere may
	// still carry this run's token: a credential-bearing survivor under an
	// unexpected name could mutate the leased store beside the observer —
	// outside any window a replacement lease serializes, and past the fresh
	// window's release — and the final audit's veto cannot undo either.
	// The gate runs before the observation or a replacement window is
	// taken; an unexpected-name token carrier is the anomaly the audit
	// fails closed on, so adoption refuses retryably rather than reaping
	// what the run cannot name.
	if err := b.verifyNoTokenStrayContainers(ctx, st.ownershipLabel); err != nil {
		return nil, err
	}

	// An adoption failure becomes a committed loss only when it carries the
	// explicit content-evidence mark: a refusal that derives
	// deterministically from the verified content itself, which a retry
	// would refuse identically. The conformance class alone is not enough —
	// shared paths wrap operational I/O (an extraction mkdir, an archive
	// read, a scanner hiccup) in it — so everything unmarked, including any
	// failure site added later, returns as a retryable error with the
	// record open and the workspace intact: the destructive direction is
	// opt-in, never the default.
	adoptionFailure := func(stage string, aerr error) (*RecoveryResult, error) {
		if ctx.Err() == nil && errors.Is(aerr, errContentEvidence) {
			return &RecoveryResult{
				Outcome: RecoveryLoss, Admission: adm,
				LossCause: fmt.Sprintf("adoption failed at the %s: %v", stage, aerr),
			}, nil
		}
		return nil, fmt.Errorf("adoption failed at the %s: %w", stage, aerr)
	}
	var credPostDigest string
	postAttested := false
	if rec.Lease != nil {
		// The post-write digest is attributable only when taken inside the
		// run's own still-held window, and only when that window outlives
		// the whole observation: with the window lapsed or taken over,
		// later holders may have mutated the store since the writer, and no
		// fresh serialization can recreate the state the writer left; a
		// window expiring mid-read would let a new holder write beside the
		// observer. The attestation is a process-window-local proof, like
		// the egress proxy's health: when its window is gone it is reported
		// as lost (PostAttested false), never recreated — recreating it
		// would attribute an intervening holder's mutation to this run.
		// The coverage bar: the window must outlast a full handoff budget
		// from this instant — a conservative stand-in for recovery's own
		// deadline (which is at most HandoffTimeout from an earlier
		// instant) that stays in the injected clock's timeline.
		covered := st.leaseHeld && st.lease.ExpiresAt.After(b.cfg.Now().Add(b.cfg.HandoffTimeout))
		if covered {
			credPostDigest, err = b.observeCredentialStore(ctx, hs, names.CredObsPost, st, &st.credObsPost)
			if err != nil {
				// Never a committed loss. adoptionFailure's premise — a
				// refusal deriving deterministically from the verified
				// content — does not hold here: the observer digests the
				// credential volume, not the workspace, and its failures
				// (including runtime errors the observer path wraps in the
				// conformance class, like a create collision) say nothing
				// about the content this adoption would export. The error
				// returns retryable, and the workspace survives.
				return nil, fmt.Errorf("adoption failed at the credential observation: %w", err)
			}
			postAttested = true
		}
	}
	// The exporter's two stages carry different evidence weight. The
	// container-lifecycle stage (create, inspect, start, wait, rootfs
	// export) says nothing about the workspace content this adoption would
	// deliver — the observer rule, applied to its sibling stage — so its
	// failures return retryable whatever error class the shared path wraps
	// them in. Only verifyExport's refusals are content evidence a
	// committed loss may stand on.
	tarPath, merr := b.materializeExport(ctx, hs, names, st)
	if merr != nil {
		return nil, fmt.Errorf("adoption failed materializing the export: %w", merr)
	}
	out, err := b.verifyExport(ctx, tarPath, st.exportDir)
	if err != nil {
		return adoptionFailure("export verification", err)
	}
	// As in Handoff: the export's location is durable before the completed
	// close, so a crash between the two leaves a locatable delivery.
	if merr := b.cfg.Journal.MarkExportMaterialized(ctx, rec.RunID, out.Dir); merr != nil {
		return nil, fmt.Errorf("journal export location: %w", merr)
	}
	st.succeeded = true
	authStore := AuthStoreObservation{}
	if rec.Lease != nil {
		authStore = AuthStoreObservation{
			Leased:         true,
			AuthIdentityID: rec.Lease.AuthIdentityID,
			Holder:         rec.Lease.Holder,
			Fence:          rec.Lease.Fence,
			PreDigest:      rec.CredentialPreDigest,
			PostDigest:     credPostDigest,
			PostAttested:   postAttested,
			Mutated:        postAttested && rec.CredentialPreDigest != credPostDigest,
		}
	}
	return &RecoveryResult{
		Outcome:           RecoveryExported,
		Admission:         adm,
		ExportDir:         out.Dir,
		Manifest:          out.Manifest,
		Evidence:          out.Evidence,
		EvidencePresent:   out.EvidencePresent,
		CommitPlanPresent: out.CommitPlanPresent,
		Workspace: WorkspaceObservation{
			Volume:          names.Workspace,
			Seeded:          rec.ObservedBaseSHA != "",
			ObservedBaseSHA: rec.ObservedBaseSHA,
		},
		AuthStore: authStore,
	}, nil
}

// validateMaterializedExportPath proves a journal-decoded path has the exact
// shape ward itself creates before it is allowed to steer filesystem reads.
// Content is then re-verified independently by verifyMaterializedExport.
func validateMaterializedExportPath(exportRoot, runID, dir string) error {
	clean := filepath.Clean(dir)
	prefix := "freeside-handoff-" + runID + "-out-"
	if clean != dir || filepath.Dir(clean) != filepath.Clean(exportRoot) ||
		!strings.HasPrefix(filepath.Base(clean), prefix) ||
		len(filepath.Base(clean)) == len(prefix) {
		return fmt.Errorf("%w: completed record export location is not a gate-owned path",
			ErrInvalidJournalRecord)
	}
	info, err := os.Lstat(clean)
	if err != nil {
		return fmt.Errorf("%w: completed record export location is unavailable",
			ErrInvalidJournalRecord)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: completed record export location is not a directory",
			ErrInvalidJournalRecord)
	}
	return nil
}

// workspaceOurs reports whether the run's workspace volume still exists and
// is provably this run's, on fresh listing evidence. Absent and foreign are
// honest "no"; unprovable is an error, because recovery may neither adopt
// nor promise absence over it.
func (b *Backend) workspaceOurs(ctx context.Context, name string, st *runState) (bool, error) {
	vols, err := b.rt.ListVolumes(ctx)
	if err != nil {
		return false, failf(CheckRecovery, "list volumes: %v", err)
	}
	v, found, ferr := uniqueVolume(vols, name)
	if ferr != nil {
		return false, failf(CheckRecovery, "%v", ferr)
	}
	if !found {
		return false, nil
	}
	ev, eerr := b.volumeEvidence(ctx, v, st.workspace, st.ownershipLabel)
	if eerr != nil {
		return false, failf(CheckRecovery, "%v", eerr)
	}
	switch ev {
	case evidenceOurs:
		return true, nil
	case evidenceForeign:
		return false, nil
	case evidenceUnprovable:
		return false, failf(CheckRecovery, "workspace %q ownership unprovable; neither adoption nor loss can proceed", name)
	}
	return false, failf(CheckRecovery, "workspace %q evidence unclassified", name)
}

// verifyRecordedWriterAbsent refuses adoption when the world contradicts the
// record's writer-complete claim: an ours-classified container under the
// writer's deterministic name means the absence proof the record asserts
// does not hold now, and an unprovable one cannot confirm it. Foreign and
// absent are consistent with the claim. The check observes only; the refusal
// commits nothing and is retryable.
func (b *Backend) verifyRecordedWriterAbsent(ctx context.Context, id string, claim objectClaim, ownershipLabel Label) error {
	ctrs, err := b.rt.ListContainers(ctx)
	if err != nil {
		return failf(CheckRecovery, "list containers: %v", err)
	}
	candidate, found, ferr := uniqueContainer(ctrs, id)
	if ferr != nil {
		return failf(CheckRecovery, "%v", ferr)
	}
	if !found {
		return nil
	}
	ev, eerr := b.containerEvidence(ctx, candidate, claim, ownershipLabel)
	if eerr != nil {
		return failf(CheckRecovery, "%v", eerr)
	}
	switch ev {
	case evidenceOurs:
		return failf(CheckRecovery, "record claims writer completion but this run's writer container still exists")
	case evidenceForeign:
		return nil
	case evidenceUnprovable:
		return failf(CheckRecovery, "writer container %q unprovable against the record's completion claim", id)
	}
	return failf(CheckRecovery, "writer container %q evidence unclassified", id)
}

// reapRecoveredContainer removes a leftover container under one of this
// run's deterministic names before its role is rerun. Ours is reaped and
// proven absent; absent is fine; foreign or unprovable refuses — a foreign
// squatter blocks the fresh create, and an unprovable row authorizes
// nothing.
func (b *Backend) reapRecoveredContainer(ctx context.Context, id string, claim *objectClaim, ownershipLabel Label) error {
	ctrs, err := b.rt.ListContainers(ctx)
	if err != nil {
		return failf(CheckRecovery, "list containers: %v", err)
	}
	candidate, found, ferr := uniqueContainer(ctrs, id)
	if ferr != nil {
		return failf(CheckRecovery, "%v", ferr)
	}
	if !found {
		*claim = objectClaim{}
		return nil
	}
	ev, eerr := b.containerEvidence(ctx, candidate, *claim, ownershipLabel)
	if eerr != nil {
		return failf(CheckRecovery, "%v", eerr)
	}
	switch ev {
	case evidenceOurs:
		if rerr := b.reapContainer(ctx, candidate); rerr != nil {
			return failf(CheckRecovery, "remove leftover %q: %v", id, rerr)
		}
		if aerr := b.verifyContainerAbsent(ctx, id, *claim, ownershipLabel, CheckRecovery); aerr != nil {
			return aerr
		}
		*claim = objectClaim{}
		return nil
	case evidenceForeign:
		return failf(CheckRecovery, "name %q is held by a foreign object; adoption cannot rerun the role", id)
	case evidenceUnprovable:
		return failf(CheckRecovery, "leftover %q ownership unprovable; not deleting", id)
	}
	return failf(CheckRecovery, "leftover %q evidence unclassified", id)
}

// verifyNoTokenStrayContainers is adoption's pre-observation absence bar
// over containers: after the deterministic-name roles are refused or reaped,
// a full listing may show no container, under any name, still carrying this
// run's ownership token. Such a survivor could be credential-bearing and
// mutating outside every lease window, so nothing downstream (observation,
// replacement window, export) is safe beside it. Unobservable labels fail
// closed, like the final audit; the refusal is retryable and reaps nothing —
// an object the run cannot name is the audit-trail anomaly, not cleanup.
func (b *Backend) verifyNoTokenStrayContainers(ctx context.Context, ownershipLabel Label) error {
	ctrs, err := b.rt.ListContainers(ctx)
	if err != nil {
		return failf(CheckRecovery, "list containers: %v", err)
	}
	for _, c := range ctrs {
		labels, observed := c.Labels, c.LabelsObserved
		if !observed {
			rep, ierr := b.rt.Inspect(ctx, c.ID)
			if ierr == nil && rep.ID == c.ID && rep.LabelsObserved {
				labels, observed = rep.Labels, true
			}
		}
		switch {
		case !observed:
			return failf(CheckRecovery, "container %q labels unobservable before adoption", c.ID)
		case slices.Contains(labels, ownershipLabel):
			return failf(CheckRecovery, "container %q still carries this run's ownership token before adoption", c.ID)
		}
	}
	return nil
}

// auditRunAbsent is recovery's final absence bar: a full sweep of every
// runtime listing for any object still carrying this run's ownership token,
// under whatever name (teardown's proofs are name-keyed and cannot see an
// object that ended up elsewhere). An under-observed row falls back to a
// per-object inspect; a row that still cannot show its labels fails the
// audit, because absence cannot be promised over it. Foreign objects —
// including ones carrying the run's inspection metadata label but not the
// token — are none of this run's business and pass.
func (b *Backend) auditRunAbsent(ctx context.Context, ownershipLabel Label) error {
	// The audit is teardown's sibling absence bar and detaches the same way:
	// teardown outlives the run's deadline by design, and an audit handed
	// the expired context would fail its listing calls after teardown
	// already deleted the run's objects — a spurious voiding of delivered
	// work, with the workspace gone and only a loss left for the retry.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), b.cfg.TeardownTimeout)
	defer cancel()
	var problems []string
	carries := func(labels []Label) bool { return slices.Contains(labels, ownershipLabel) }

	if ctrs, err := b.rt.ListContainers(ctx); err != nil {
		problems = append(problems, fmt.Sprintf("audit list containers: %v", err))
	} else {
		for _, c := range ctrs {
			labels, observed := c.Labels, c.LabelsObserved
			if !observed {
				rep, ierr := b.rt.Inspect(ctx, c.ID)
				if ierr == nil && rep.ID == c.ID && rep.LabelsObserved {
					labels, observed = rep.Labels, true
				}
			}
			switch {
			case !observed:
				problems = append(problems, fmt.Sprintf("container %q labels unobservable during audit", c.ID))
			case carries(labels):
				problems = append(problems, fmt.Sprintf("container %q still carries this run's ownership token", c.ID))
			}
		}
	}
	if vols, err := b.rt.ListVolumes(ctx); err != nil {
		problems = append(problems, fmt.Sprintf("audit list volumes: %v", err))
	} else {
		for _, v := range vols {
			labels, observed := v.Labels, v.LabelsObserved
			if !observed {
				sum, ierr := b.rt.InspectVolume(ctx, v.Name)
				if ierr == nil && sum.Name == v.Name && sum.LabelsObserved {
					labels, observed = sum.Labels, true
				}
			}
			switch {
			case !observed:
				problems = append(problems, fmt.Sprintf("volume %q labels unobservable during audit", v.Name))
			case carries(labels):
				problems = append(problems, fmt.Sprintf("volume %q still carries this run's ownership token", v.Name))
			}
		}
	}
	if nets, err := b.rt.ListNetworks(ctx); err != nil {
		problems = append(problems, fmt.Sprintf("audit list networks: %v", err))
	} else {
		for _, n := range nets {
			labels, observed := n.Labels, n.LabelsObserved
			if !observed {
				rep, ierr := b.rt.InspectNetwork(ctx, n.Name)
				if ierr == nil && rep.Name == n.Name && rep.LabelsObserved {
					labels, observed = rep.Labels, true
				}
			}
			switch {
			case !observed:
				problems = append(problems, fmt.Sprintf("network %q labels unobservable during audit", n.Name))
			case carries(labels):
				problems = append(problems, fmt.Sprintf("network %q still carries this run's ownership token", n.Name))
			}
		}
	}
	if len(problems) > 0 {
		return failf(CheckRecovery, "%s", strings.Join(problems, "; "))
	}
	return nil
}
