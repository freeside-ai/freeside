package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

// The source-issue-closure proposal step (#1419 Part D, plan revision 68). After
// the author run and before the candidate body is composed, the engine admits a
// source_issue_closure proposal for a v2 record with a closable source, then
// binds the merge it observes now: under the default policy it records the
// policy approval at publication and opens no card (a daemon_fallback gets the
// non-holding notice instead); under the human gate it opens the effect_proposal
// card and records no approval. The publisher, not this step, decides how the
// pull request references the issue; this step hands it the instance id and the
// bound approval or open card.

const productionClosureCheckpointKind = "production_closure_checkpoint"

const productionClosureCheckpointVersion = "1"

// policySourceIssueClosureGate is the resolved-policy switch that turns on the
// human gate for source-issue closures (mirrored in
// publish.parseSourceIssueClosureHumanGate; the domain-change ban keeps the
// constant out of a shared package, and each consumer reads the run's stored
// policy itself). Absent or false is the default policy-approval mode.
const policySourceIssueClosureGate = "gates.source_issue_closure"

// parseSourceIssueClosureHumanGate reads the closure human-gate switch from a
// run's resolved policy. A malformed value fails closed to the human gate, so a
// configuration typo never turns into an unreviewed automatic close.
func parseSourceIssueClosureHumanGate(policy domain.ResolvedPolicy) bool {
	for _, k := range policy.Keys {
		if k.Key == policySourceIssueClosureGate {
			value, err := strconv.ParseBool(k.Value)
			if err != nil {
				return true
			}
			return value
		}
	}
	return false
}

// productionClosureCheckpoint is the once-per-run closure answer. The proposal is
// head- and base-independent, so the key binds only the run and its publication
// invocation; a base advance keeps the same proposal and re-binds the merge. A
// stored row is untrusted at reconstruction: HasProposal reports whether a
// proposal was admitted, InstanceID names it, and Origin selects the default
// policy binding (a propose_site records the approval, a daemon_fallback opens
// the notice). HasProposal == (InstanceID != "") is the invariant.
type productionClosureCheckpoint struct {
	Version       string                    `json:"version"`
	RunID         domain.RunID              `json:"run_id"`
	PublicationID domain.InvocationID       `json:"publication_id"`
	HasProposal   bool                      `json:"has_proposal"`
	InstanceID    domain.ProposalInstanceID `json:"instance_id,omitempty"`
	Origin        domain.ClosureFlagOrigin  `json:"origin,omitempty"`
	Reason        string                    `json:"reason,omitempty"`
}

func productionClosureCheckpointKey(runID domain.RunID, publicationID domain.InvocationID) string {
	return "production-closure/" + string(runID) + "/" + string(publicationID)
}

func closureProposalBatchID(runID domain.RunID) domain.ProposalBatchID {
	sum := sha256.Sum256([]byte("freeside.source-issue-closure.batch/v1\x00" + string(runID)))
	return domain.ProposalBatchID("batch-source-issue-closure-" + hex.EncodeToString(sum[:]))
}

// reconcilePublicationClosure admits the source-issue-closure proposal once per
// reviewed candidate and binds the merge observed now on every pass. It runs on
// the forward path, where the author input is available, after the author run
// and before the candidate body is composed. It never blocks publication: an
// unavailable propose site, an inference fault, or a source that is no longer
// closable records a durable no-close answer rather than failing the pass. Only
// a store fault the daemon should retry propagates.
func (w *productionPublicationWorkflow) reconcilePublicationClosure(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
	checkpoint productionVerificationCheckpoint,
	reviewInstructions exec.ReviewInstructionBinding,
	checkoutDir string,
	report []byte,
) error {
	if !authoredPublicationRecipe(task.Publication.Recipe) {
		return nil
	}
	closure, err := w.reconcileClosureDecision(ctx, task, binding, checkpoint, reviewInstructions, checkoutDir, report)
	if err != nil {
		return err
	}
	if !closure.HasProposal {
		return nil
	}
	// Bind the open card or the policy approval to the merge observed now, on
	// every pass: a no-op for an unchanged merge, a supersession or re-record
	// when the head or base moved (issue #1419 settled item 1).
	merge, err := w.publicationClosureMerge(task, binding, checkpoint)
	if err != nil {
		return err
	}
	return w.bindClosureMerge(ctx, task.RunID, binding, closure, merge)
}

// reconcileClosureDecision loads the once-per-run closure answer, or, on the
// first pass, decides the closable source, asks the propose site, and admits the
// proposal, recording the answer so a retry never re-asks.
func (w *productionPublicationWorkflow) reconcileClosureDecision(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
	checkpoint productionVerificationCheckpoint,
	reviewInstructions exec.ReviewInstructionBinding,
	checkoutDir string,
	report []byte,
) (productionClosureCheckpoint, error) {
	key := productionClosureCheckpointKey(task.RunID, task.PublicationID)
	want := productionClosureCheckpoint{
		Version: productionClosureCheckpointVersion, RunID: task.RunID, PublicationID: task.PublicationID,
	}
	if stored, found, err := w.loadClosureCheckpoint(ctx, key, want); err != nil {
		return productionClosureCheckpoint{}, err
	} else if found {
		return stored, nil
	}
	source, proposedTarget, ok, err := w.decideClosableSource(ctx, task, binding)
	if err != nil {
		return productionClosureCheckpoint{}, err
	}
	if !ok {
		// A cross-repository or absent source: no proposal, no hold. Record the
		// answer so replay is stable and the propose site is never re-asked.
		return want, w.putClosureCheckpoint(ctx, key, want)
	}
	origin, resolves, reason := w.proposeClosure(ctx, task, binding, checkpoint, reviewInstructions, checkoutDir, report)
	decided, admitted, err := w.admitClosureProposal(ctx, key, want, task, source, proposedTarget, origin, resolves, reason)
	if err != nil {
		return productionClosureCheckpoint{}, err
	}
	if !admitted {
		// The store's re-gate found no closable source (the engine and the store
		// disagreed): fail safe to no proposal rather than block publication.
		return want, w.putClosureCheckpoint(ctx, key, want)
	}
	return decided, nil
}

// decideClosableSource mirrors the store's closableSource determination so the
// engine knows whether to propose and with what target. The store re-derives the
// same fact from live rows and re-gates the admitted proposal, so a mismatch
// fails the admission closed rather than closing a wrong issue. A daemon-bound
// same-repository issue subject is verified (the daemon uses its bound issue); a
// same-repository client source URL is recommended (its issue number is the
// client's choice); a cross-repository or absent source yields no proposal.
func (w *productionPublicationWorkflow) decideClosableSource(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
) (domain.ClosableSource, domain.IssueSubjectRef, bool, error) {
	repo := binding.admission.Base.Repo
	repositoryID := binding.admission.Base.RepositoryID
	var taskSource *domain.SpecificationSource
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		fetched, err := tx.GetTask(ctx, binding.run.TaskID)
		if err != nil {
			return err
		}
		taskSource = fetched.Source
		return nil
	}); err != nil {
		return domain.ClosableSource{}, domain.IssueSubjectRef{}, false, err
	}
	if taskSource != nil && taskSource.Kind == domain.SpecificationSourceIssueSubject && taskSource.IssueSubject != nil {
		subject := taskSource.IssueSubject
		if subject.Repo != repo || subject.RepositoryID != repositoryID {
			// A cross-repository issue subject never earns a verified close.
			return domain.ClosableSource{}, domain.IssueSubjectRef{}, false, nil
		}
		return domain.ClosableSource{
			Present: true, Provenance: domain.ClosureProvenanceVerified,
			Repo: repo, RepositoryID: repositoryID, IssueNumber: subject.IssueNumber,
		}, domain.IssueSubjectRef{}, true, nil
	}
	ownerRepo, number, ok := parseSourceIssueURL(task.Publication.SourceIssue)
	if !ok || ownerRepo != repo {
		// A cross-repository client URL, or none, yields no proposal.
		return domain.ClosableSource{}, domain.IssueSubjectRef{}, false, nil
	}
	return domain.ClosableSource{
			Present: true, Provenance: domain.ClosureProvenanceRecommended,
			Repo: repo, RepositoryID: repositoryID,
		},
		domain.IssueSubjectRef{Repo: repo, RepositoryID: repositoryID, IssueNumber: number}, true, nil
}

// proposeClosure asks the propose site whether the pull request should close the
// source issue. An unavailable site, an input-build fault, a call error, or a
// returned fallback is a daemon_fallback proposal that never resolves and never
// holds; only a clean answer carries a propose_site origin and its resolve flag.
func (w *productionPublicationWorkflow) proposeClosure(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
	checkpoint productionVerificationCheckpoint,
	reviewInstructions exec.ReviewInstructionBinding,
	checkoutDir string,
	report []byte,
) (domain.ClosureFlagOrigin, bool, string) {
	if w.inference == nil || !w.inference.SupportsSite(inference.PublicationAuthorProposeSiteID) {
		return domain.ClosureFlagOriginDaemonFallback, false, ""
	}
	input, err := w.buildPublicationAuthorInput(ctx, task, binding, checkpoint, reviewInstructions, checkoutDir, report)
	if err != nil {
		return domain.ClosureFlagOriginDaemonFallback, false, ""
	}
	proposed, err := w.inference.ProposeSourceIssueClosure(ctx, input)
	if err != nil || proposed.Fallback {
		return domain.ClosureFlagOriginDaemonFallback, false, proposed.InputRefusalReason
	}
	return domain.ClosureFlagOriginProposeSite, proposed.Resolves, ""
}

// admitClosureProposal builds and allocates the proposal under a once-per-run
// run-emission admission key, and records the decided checkpoint in the same
// transaction, so a crash can never leave an allocated instance with no
// checkpoint (which a later non-deterministic propose-site answer could turn
// into an immutable-conflict). admitted is false when the store's re-gate finds
// no closable source (a fail-safe no-proposal outcome, distinct from a retryable
// store fault, which propagates).
func (w *productionPublicationWorkflow) admitClosureProposal(
	ctx context.Context,
	key string,
	want productionClosureCheckpoint,
	task productionPublicationTask,
	source domain.ClosableSource,
	proposedTarget domain.IssueSubjectRef,
	origin domain.ClosureFlagOrigin,
	resolves bool,
	reason string,
) (productionClosureCheckpoint, bool, error) {
	handle := domain.OpaqueSubjectHandle(domain.WorkUnitIDForRun(task.RunID))
	admission := domain.ProposalAdmissionKey{
		Source: domain.ProposalSourceRunEmission, InvocationID: task.PublicationID, EmissionOrdinal: 1,
	}
	batchID := closureProposalBatchID(task.RunID)
	decided := want
	err := w.store.Write(ctx, func(tx *store.WriteTx) error {
		_, policy, err := tx.ResolveProposalSubject(ctx, handle)
		if err != nil {
			return err
		}
		proposal, err := domain.NewEffectProposal(domain.EffectSourceIssueClosure, domain.SourceIssueClosureInput{
			SubjectHandle: handle, Source: source, ProposedTarget: proposedTarget,
			Origin: origin, Resolves: resolves,
		}, policy)
		if err != nil {
			return err
		}
		instance, _, err := tx.AllocateProposalInstance(ctx, admission, batchID, proposal, w.attentionCreatedAt())
		if err != nil {
			return err
		}
		decided.HasProposal = true
		decided.InstanceID = instance.ID
		decided.Origin = origin
		decided.Reason = reason
		return recordClosureCheckpoint(ctx, tx, key, decided)
	})
	if err != nil {
		if isClosureGateRejection(err) {
			return productionClosureCheckpoint{}, false, nil
		}
		return productionClosureCheckpoint{}, false, err
	}
	return decided, true, nil
}

// isClosureGateRejection reports whether an admission error is the store's
// re-gate refusing a source it no longer finds closable, rather than a retryable
// store fault. The former fails safe to no proposal; the latter propagates.
func isClosureGateRejection(err error) bool {
	return errors.Is(err, domain.ErrClosableSourceAbsent) ||
		errors.Is(err, domain.ErrClosableSourceInconsistent) ||
		errors.Is(err, domain.ErrClosureProvenanceMismatch) ||
		errors.Is(err, domain.ErrClosureTargetMismatch) ||
		errors.Is(err, domain.ErrEffectProposalInconsistent) ||
		errors.Is(err, domain.ErrInvalidEffectKind) ||
		errors.Is(err, store.ErrNotFound)
}

// bindClosureMerge binds the closure to the merge observed now. Under the human
// gate the effect_proposal card is (re)opened and holds the pull request; under
// the default policy a propose_site records the policy approval and opens no
// card, and a daemon_fallback opens the non-holding notice. Every call is
// idempotent for an unchanged merge and supersedes or re-records for a moved one.
func (w *productionPublicationWorkflow) bindClosureMerge(
	ctx context.Context,
	runID domain.RunID,
	binding productionBinding,
	closure productionClosureCheckpoint,
	merge domain.ProspectiveMerge,
) error {
	return closureBindNonBlocking(w.bindClosureMergeOnce(ctx, runID, binding, closure, merge))
}

// closureBindNonBlocking upholds the step's never-block contract symmetrically
// with admitClosureProposal: a re-gate that now refuses the closure (a source no
// longer closable) degrades to no binding rather than failing the pass, so the
// publisher writes Refs; only a retryable store fault propagates.
func closureBindNonBlocking(err error) error {
	if isClosureGateRejection(err) {
		return nil
	}
	return err
}

func (w *productionPublicationWorkflow) bindClosureMergeOnce(
	ctx context.Context,
	runID domain.RunID,
	binding productionBinding,
	closure productionClosureCheckpoint,
	merge domain.ProspectiveMerge,
) error {
	if parseSourceIssueClosureHumanGate(binding.resolvedPolicy) {
		_, err := w.attention.OpenEffectProposalItem(ctx, closure.InstanceID, merge)
		return err
	}
	switch closure.Origin {
	case domain.ClosureFlagOriginProposeSite:
		return w.store.Write(ctx, func(tx *store.WriteTx) error {
			// The instance id is decoded from an untrusted stored checkpoint. Re-bind
			// it to this run before recording an approval, so a restored or corrupted
			// row that names another run's instance fails closed rather than binding
			// this candidate's merge to a foreign proposal (which the publisher would
			// then read as an authorized Closes for the other run's issue).
			// GetProposalInstance also re-gates closability; an unclosable source
			// stays a never-block degrade via closureBindNonBlocking.
			instance, err := tx.GetProposalInstance(ctx, closure.InstanceID)
			if err != nil {
				return err
			}
			if err := verifyClosureInstanceRun(instance, runID); err != nil {
				return err
			}
			_, err = tx.RecordPolicyClosureApproval(ctx, closure.InstanceID, merge, w.attentionCreatedAt())
			return err
		})
	case domain.ClosureFlagOriginDaemonFallback:
		_, err := w.attention.OpenEffectProposalNotice(ctx, closure.InstanceID, merge)
		return err
	default:
		return fmt.Errorf("closure checkpoint origin %q: %w", closure.Origin, domain.ErrEffectProposalInconsistent)
	}
}

// verifyClosureInstanceRun re-binds a decoded checkpoint's instance id to the run
// that is publishing. The id crosses the reconstruction trust boundary, so a
// restored or corrupted row can name another run's instance; the store's closable
// gate does not check ownership. A subject handle that is not this run's work unit
// fails closed with ErrParentKeyMismatch (a spoofed instance, not a benign
// re-gate rejection, so it propagates instead of degrading to Refs).
func verifyClosureInstanceRun(instance domain.ProposalInstance, runID domain.RunID) error {
	closure := instance.Proposal.ClosureProposal
	if closure == nil || closure.SubjectHandle != domain.OpaqueSubjectHandle(domain.WorkUnitIDForRun(runID)) {
		return fmt.Errorf("closure instance %q is not bound to run %q: %w",
			instance.ID, runID, domain.ErrParentKeyMismatch)
	}
	return nil
}

// publicationClosureMerge builds the merge the closure approval binds: the
// publication identity derived from the same candidate material the publisher
// derives, the candidate head, and the base ref and SHA. The publisher rebuilds
// this exact merge from its own identity, head, and authorization base SHA, so
// ClosureApproval.AuthorizesClose judges the same merge on both sides.
func (w *productionPublicationWorkflow) publicationClosureMerge(
	task productionPublicationTask,
	binding productionBinding,
	checkpoint productionVerificationCheckpoint,
) (domain.ProspectiveMerge, error) {
	recipe := binding.image.RecipeDigest
	identity, err := fakePublicationIdentity(publish.Candidate{
		Repo: binding.admission.Base.Repo, BaseRef: binding.admission.Base.BaseRef,
		HeadSHA: task.HeadSHA, Artifacts: checkpoint.Artifacts, RecipeDigest: &recipe,
	})
	if err != nil {
		return domain.ProspectiveMerge{}, err
	}
	merge := domain.ProspectiveMerge{
		PublicationIdentity: identity.Digest(),
		CandidateHeadSHA:    task.HeadSHA,
		BaseRef:             binding.admission.Base.BaseRef,
		BaseSHA:             binding.admission.Base.BaseSHA,
	}
	if err := merge.Validate(); err != nil {
		return domain.ProspectiveMerge{}, err
	}
	return merge, nil
}

// applyClosureReference sets the candidate's source-issue reference fields from
// the stored closure answer, so every publication path (forward, recovery, drift
// repair) hands the publisher the same reference. It is read-only and never
// blocks: a missing or unreadable checkpoint leaves the fields empty, so the
// publisher writes no source-reference section.
func (w *productionPublicationWorkflow) applyClosureReference(
	ctx context.Context,
	task productionPublicationTask,
	candidate *publish.Candidate,
) error {
	if !authoredPublicationRecipe(task.Publication.Recipe) {
		return nil
	}
	key := productionClosureCheckpointKey(task.RunID, task.PublicationID)
	want := productionClosureCheckpoint{
		Version: productionClosureCheckpointVersion, RunID: task.RunID, PublicationID: task.PublicationID,
	}
	closure, found, err := w.loadClosureCheckpoint(ctx, key, want)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	if closure.HasProposal {
		candidate.ClosureInstanceID = closure.InstanceID
		return nil
	}
	// No proposal: a cross-repository or absent source. Name it in a descriptive
	// link when the client supplied a canonical URL.
	candidate.SourceIssueURL = canonicalSourceIssue(task.Publication.SourceIssue)
	return nil
}

// loadClosureCheckpoint reads the closure answer and re-validates the decoded row
// against the run identity the key encodes and the proposal invariant. A restored
// or corrupted row that disagrees on run, publication, origin, or the
// HasProposal/instance-id invariant fails closed here with ErrParentKeyMismatch.
// A spoofed instance id that keeps the correct run and publication is caught where
// the id can authorize a close: verifyClosureInstanceRun before the approval write
// and the publisher's resolveClosure re-bind it to this run's work unit.
func (w *productionPublicationWorkflow) loadClosureCheckpoint(
	ctx context.Context, key string, want productionClosureCheckpoint,
) (productionClosureCheckpoint, bool, error) {
	var checkpoint productionClosureCheckpoint
	var found bool
	err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		entry, err := tx.GetInbox(ctx, key)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if entry.Kind != productionClosureCheckpointKind {
			return fmt.Errorf("closure checkpoint kind %q: %w", entry.Kind, domain.ErrParentKeyMismatch)
		}
		if err := strictjson.Decode(
			entry.Payload, &checkpoint, strictjson.TolerateInvalidUTF8, strictjson.NoLimit,
		); err != nil {
			return fmt.Errorf("decode closure checkpoint: %w", errors.Join(err, domain.ErrParentKeyMismatch))
		}
		if err := validateClosureCheckpoint(checkpoint, want); err != nil {
			return err
		}
		found = true
		return nil
	})
	if err != nil {
		return productionClosureCheckpoint{}, false, err
	}
	return checkpoint, found, nil
}

// validateClosureCheckpoint fails closed unless the decoded row matches the run
// identity and holds the proposal invariant: HasProposal == (InstanceID != ""),
// and a proposal carries one of the two valid origins.
func validateClosureCheckpoint(got, want productionClosureCheckpoint) error {
	switch {
	case got.Version != productionClosureCheckpointVersion:
		return fmt.Errorf("closure checkpoint version %q: %w", got.Version, domain.ErrParentKeyMismatch)
	case got.RunID != want.RunID:
		return fmt.Errorf("closure checkpoint run %q: %w", got.RunID, domain.ErrParentKeyMismatch)
	case got.PublicationID != want.PublicationID:
		return fmt.Errorf("closure checkpoint publication %q: %w", got.PublicationID, domain.ErrParentKeyMismatch)
	case got.HasProposal != (got.InstanceID != ""):
		return fmt.Errorf("closure checkpoint proposal/instance state inconsistent: %w", domain.ErrParentKeyMismatch)
	case got.HasProposal && got.Origin != domain.ClosureFlagOriginProposeSite &&
		got.Origin != domain.ClosureFlagOriginDaemonFallback:
		return fmt.Errorf("closure checkpoint origin %q: %w", got.Origin, domain.ErrParentKeyMismatch)
	case !got.HasProposal && got.Origin != "":
		return fmt.Errorf("closure checkpoint carries an origin without a proposal: %w", domain.ErrParentKeyMismatch)
	}
	return nil
}

func (w *productionPublicationWorkflow) putClosureCheckpoint(
	ctx context.Context, key string, checkpoint productionClosureCheckpoint,
) error {
	return w.store.Write(ctx, func(tx *store.WriteTx) error {
		return recordClosureCheckpoint(ctx, tx, key, checkpoint)
	})
}

// recordClosureCheckpoint writes the closure answer and confirms it converged
// byte-for-byte, the same immutability guard the authoring checkpoint applies.
func recordClosureCheckpoint(ctx context.Context, tx *store.WriteTx, key string, checkpoint productionClosureCheckpoint) error {
	payload, err := json.Marshal(checkpoint)
	if err != nil {
		return err
	}
	entry, _, err := tx.RecordInbox(ctx, key, productionClosureCheckpointKind, payload)
	if err != nil {
		return err
	}
	if entry.Kind != productionClosureCheckpointKind || !bytes.Equal(entry.Payload, payload) {
		return fmt.Errorf("production closure checkpoint disagrees with stored row: %w",
			domain.ErrImmutableTransition)
	}
	return nil
}

// parseSourceIssueURL extracts the "owner/repo" and issue number from a canonical
// GitHub source-issue URL, reusing the same acceptance the v1 renderer applies.
func parseSourceIssueURL(source string) (ownerRepo string, number int, ok bool) {
	if canonicalSourceIssue(source) == "" {
		return "", 0, false
	}
	rest := strings.TrimPrefix(source, "https://github.com/")
	owner, after, found := strings.Cut(rest, "/")
	if !found {
		return "", 0, false
	}
	repo, tail, found := strings.Cut(after, "/issues/")
	if !found {
		return "", 0, false
	}
	n, err := strconv.Atoi(tail)
	if err != nil || n < 1 {
		return "", 0, false
	}
	return owner + "/" + repo, n, true
}
