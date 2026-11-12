package engine

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/diffscope"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/gitrun"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

type productionReviewGateState uint8

const (
	productionReviewPending productionReviewGateState = iota
	productionReviewPassed
	productionReviewEscalated
	// productionReviewContinue is consumed inside reconcileReviewGate. It asks
	// the gate to schedule another candidate-bound pass and must never escape to
	// publication as a passed state.
	productionReviewContinue
)

// productionReviewRequest builds the request every review path derives from
// current state: the routed and shadow launches and the authority-only re-gate
// of a persisted request. The task's attachments ride in Inputs, so all three
// bind the same files into the authority digest.
func (w *productionPublicationWorkflow) productionReviewRequest(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
	checkpoint productionVerificationCheckpoint,
	id domain.InvocationID,
	round int,
	instructions exec.ReviewInstructionBinding,
	admission domain.ReviewAdmission,
) (exec.ReviewRequest, domain.Digest, error) {
	reviewWorkspace, err := w.reviewWorkspacePath(id)
	if err != nil {
		return exec.ReviewRequest{}, "", err
	}
	artifactDigests := make([]domain.Digest, len(checkpoint.Artifacts))
	for i := range checkpoint.Artifacts {
		artifactDigests[i] = checkpoint.Artifacts[i].Digest
	}
	verification, err := exec.NewReviewVerificationEvidence(
		exec.ReviewVerificationEvidence{
			Outcome:                checkpoint.Authorization.VerificationOutcome,
			RecipeDigest:           checkpoint.Authorization.VerificationRecipeDigest,
			EvidenceSnapshotDigest: checkpoint.Authorization.EvidenceSnapshotDigest,
			ArtifactDigests:        artifactDigests,
		},
	)
	if err != nil {
		return exec.ReviewRequest{}, "", err
	}
	inputs, err := w.reviewInputFiles(ctx, task.RunID)
	if err != nil {
		return exec.ReviewRequest{}, "", err
	}
	request := exec.ReviewRequest{
		RunID: task.RunID, Round: round,
		Repo: binding.admission.Base.Repo, RepositoryID: binding.admission.Base.RepositoryID,
		BaseRef: binding.admission.Base.BaseRef,
		BaseSHA: binding.admission.Base.BaseSHA, HeadSHA: task.HeadSHA,
		Workspace: reviewWorkspace, Verification: verification,
		Instructions: instructions, Admission: admission, Inputs: inputs, RequestedAt: w.now().UTC(),
	}
	authority, err := request.AuthorityDigest()
	return request, authority, err
}

// reviewInputFiles reads the reviewed run's task attachments, nil when it has
// none so a request without them keeps its earlier shape. Discussion
// attachments are a writer's conversation input and never reach a reviewer.
// As for writers, a run that names no task is a bug: skipping would drop the
// task's files silently.
func (w *productionPublicationWorkflow) reviewInputFiles(
	ctx context.Context, runID domain.RunID,
) ([]domain.StageInputAttachment, error) {
	var files []domain.StageInputAttachment
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		run, err := tx.GetRun(ctx, runID)
		if err != nil {
			return fmt.Errorf("read reviewed run: %w", err)
		}
		if run.TaskID == "" {
			return fmt.Errorf("run %q names no task: %w", runID, domain.ErrEmptyID)
		}
		files, err = taskInputFiles(ctx, tx, run.TaskID)
		return err
	}); err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, nil
	}
	return files, nil
}

// ProductionReviewInvocationID derives the bounded ward identity for one
// run/round without embedding an arbitrarily long durable run ID.
func ProductionReviewInvocationID(runID domain.RunID, round int) domain.InvocationID {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d", runID, round)))
	return domain.InvocationID(fmt.Sprintf("review-%x", sum[:12]))
}

func (w *productionPublicationWorkflow) reviewWorkspacePath(
	id domain.InvocationID,
) (string, error) {
	name := string(id)
	if name == "" || filepath.Base(name) != name || strings.ContainsAny(name, `/\\`) {
		return "", fmt.Errorf("review workspace invocation %q: %w", id, domain.ErrPathBoundaryMismatch)
	}
	return filepath.Join(w.workDir, "review-workspaces", name), nil
}

func findingAdjudicationBaseWorkspaceID(id domain.InvocationID) domain.InvocationID {
	return domain.InvocationID(string(id) + "-adjudication-base")
}

func (w *productionPublicationWorkflow) ensureReviewWorkspace(
	ctx context.Context, id domain.InvocationID, checkout PublicationCheckout, headSHA string,
) (string, error) {
	target, err := w.reviewWorkspacePath(id)
	if err != nil {
		return "", err
	}
	targetExists := false
	if info, err := os.Lstat(target); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("review workspace %q is not an owned directory: %w",
				target, domain.ErrPathBoundaryMismatch)
		}
		targetExists = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return "", err
	}
	// Retain, not move: under the pre-publication review anchor the sealed
	// publication checkout must survive for the subsequent PushHead when a
	// synchronous review source completes inline. The transport claims the
	// fresh staged destination and copies from that capability, so raw
	// materialization cannot clear a caller-nominated existing directory. Stage
	// the retained candidate in a sibling temp dir, then rename it, so it cannot leave a
	// partial workspace that a resumed unknown invocation could use on a later
	// pass. An existing path can be a pre-upgrade, unmaterialized copy from the
	// same pending invocation, so unknown means rebuild rather than accept it.
	// The staged materialization finishes before the old directory is removed;
	// os.Rename cannot replace a non-empty directory. If the process stops in
	// the resulting absent window, the same guard rebuilds on the next pass.
	staging, err := os.MkdirTemp(filepath.Dir(target), "staging-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(staging) //nolint:errcheck // best-effort staging cleanup
	staged := filepath.Join(staging, "workspace")
	if err := w.transport.RetainWorktree(ctx, checkout, staged, headSHA); err != nil {
		return "", fmt.Errorf("materialize review workspace: %w", err)
	}
	if targetExists {
		if err := os.RemoveAll(target); err != nil {
			return "", fmt.Errorf("replace review workspace: %w", err)
		}
	}
	if err := os.Rename(staged, target); err != nil {
		return "", fmt.Errorf("retain review workspace: %w", err)
	}
	return target, nil
}

func (w *productionPublicationWorkflow) removeReviewWorkspace(id domain.InvocationID) error {
	target, err := w.reviewWorkspacePath(id)
	if err != nil {
		return err
	}
	info, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refuse to remove unowned review workspace %q: %w",
			target, domain.ErrPathBoundaryMismatch)
	}
	if err := os.RemoveAll(target); err != nil {
		return fmt.Errorf("remove review workspace %q: %w", target, err)
	}
	return nil
}

// reviewedDiffScope renders the trusted `git diff -U0` for the bound
// base-to-head pair from a daemon-owned candidate checkout and parses it into
// the diff-overlap scope the routed review-finding gate consults. The engine
// derives this diff itself and never accepts it from the reviewer, so a routed
// finding can be admitted only against the exact reviewed change.
//
// It mirrors remediationCandidatePatch's gitrun idiom but deliberately forces
// -U0 and omits --binary/--full-index: diffscope.Parse consumes text hunks with
// zero context (it rejects a context line outright), and a binary patch carries
// no new-side line ranges for Overlaps to resolve. --no-renames is likewise
// deliberate: diffscope keys strictly on the new-side path, so a candidate that
// deletes a file and adds a similar one would, under git's default rename
// detection, re-key the deletion under the new path and drop the deleted path
// from the diff entirely; a whole-file (0,0) finding on that candidate-deleted
// file (the §7/#855 representation) would then fail closed in the wrong
// direction. Disabling rename detection renders every touched path as its own
// delete/add pair, so both endpoints stay resolvable and only genuinely
// untouched paths are absent (the fail-closed direction is preserved).
//
// The -U0 unified diff alone still misses a header-only change: git emits only
// `diff --git` and metadata (no ---/+++ headers, no hunk) for a file-mode
// change, a binary-content change, or an empty-file add or delete, so those
// touched paths never enter diffscope's index. A candidate-deleted *empty* file
// is the reachable trap: its whole-file (0,0) finding (again the §7/#855
// representation) would fail closed in the wrong direction. A companion
// `git diff --name-status -z` pass over the same pair recovers every touched
// path, so whole-file findings on header-only changes resolve while genuinely
// untouched paths stay absent; a concrete line finding on such a path still
// fails to overlap (it carries no new-side range). A failure in either pass is
// an engine fault (a diff we produce deterministically), not a reviewer
// contradiction, so the caller takes the retry/pending path rather than burning
// the review.
func reviewedDiffScope(
	ctx context.Context, workDir, candidateRoot, baseSHA, headSHA string,
) (diffscope.Diff, error) {
	scratch, err := os.MkdirTemp(workDir, ".review-diffscope-")
	if err != nil {
		return diffscope.Diff{}, err
	}
	defer os.RemoveAll(scratch) //nolint:errcheck // daemon-owned scratch
	runner, err := gitrun.New(gitrun.Options{Scratch: scratch})
	if err != nil {
		return diffscope.Diff{}, err
	}
	if _, err := runner.PinCheckout(ctx, candidateRoot); err != nil {
		return diffscope.Diff{}, fmt.Errorf("bind reviewed-diff candidate checkout: %w", err)
	}
	diff, err := runner.Run(
		ctx, nil, "diff", "-U0", "--no-renames", "--no-color", "--no-ext-diff", "--no-textconv",
		baseSHA, headSHA, "--",
	)
	if err != nil {
		return diffscope.Diff{}, fmt.Errorf("render reviewed diff: %w", err)
	}
	scope, err := diffscope.Parse(string(diff))
	if err != nil {
		return diffscope.Diff{}, fmt.Errorf("parse reviewed diff: %w", err)
	}
	// Seed the touched paths a header-only change omits from the -U0 diff (a
	// mode, binary, or empty-file add/delete carries no ---/+++ header), so a
	// whole-file finding on a candidate-deleted empty file resolves instead of
	// failing closed as a false contradiction. --no-renames matches the diff
	// above, keeping every touched path keyed 1:1 by its own name.
	nameStatus, err := runner.Run(
		ctx, nil, "diff", "--name-status", "-z", "--no-renames", "--no-color",
		"--no-ext-diff", "--no-textconv", baseSHA, headSHA, "--",
	)
	if err != nil {
		return diffscope.Diff{}, fmt.Errorf("render reviewed name-status diff: %w", err)
	}
	if err := scope.MergeNameStatusZ(string(nameStatus)); err != nil {
		return diffscope.Diff{}, fmt.Errorf("merge reviewed name-status diff: %w", err)
	}
	return scope, nil
}

// reconcileReviewGate runs one pass of the production review gate. After the
// review-mode check it runs the gate's phases in a fixed order. Each phase
// either ends the pass with the state and error it returns, or hands its
// result to the next.
func (w *productionPublicationWorkflow) reconcileReviewGate(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
	checkpoint productionVerificationCheckpoint,
	workspace PublicationCheckout,
	reviewInstructions exec.ReviewInstructionBinding,
) (productionReviewGateState, error) {
	if binding.profile.Review.Mode != domain.ReviewFreesideInvoked {
		return productionReviewPending, fmt.Errorf(
			"production profile review mode %q cannot satisfy readiness: %w",
			binding.profile.Review.Mode, domain.ErrInvalidReviewMode,
		)
	}
	sel, err := w.selectReviewState(ctx, task)
	if err != nil {
		return productionReviewPending, err
	}
	recoveredContradiction, state, done, err := w.recoverReviewContradiction(ctx, task, binding, sel)
	if done {
		return state, err
	}
	state, done, err = w.gateReviewConfiguration(ctx, task, binding, sel)
	if done {
		return state, err
	}
	state, done, err = w.reconcileCurrentReviewRecord(
		ctx, task, binding, checkpoint, workspace, reviewInstructions, sel)
	if done {
		return state, err
	}
	round, state, done, err := w.chooseReviewRound(ctx, task, binding, sel, recoveredContradiction)
	if done {
		return state, err
	}
	inv, state, done, err := w.requestReviewRound(ctx, task, binding, checkpoint, reviewInstructions, round)
	if done {
		return state, err
	}
	state, done, err = w.launchReviewRound(ctx, task, binding, workspace, inv)
	if done {
		return state, err
	}
	result, state, done, err := w.pollReviewRound(ctx, task, binding, inv)
	if done {
		return state, err
	}
	record, remediationOutcome, err := w.recordReviewRound(ctx, task, binding, checkpoint, inv, result)
	if err != nil {
		return productionReviewPending, err
	}
	return w.settleReviewRound(
		ctx, task, binding, checkpoint, workspace, reviewInstructions, inv, record, remediationOutcome)
}

// reviewGateSelection is what the select phase hands to the phases that
// decide whether a new round is needed: the latest review record and failure
// at or above the round floor, and that floor.
type reviewGateSelection struct {
	record       *domain.ReviewRecord
	failure      *domain.ReviewFailure
	minimumRound int
}

// reviewRoundInvocation is the round the request phase admitted and
// requested, handed to the phases that launch, poll, record, and settle it.
// The review workspace is req.Workspace.
type reviewRoundInvocation struct {
	id        domain.InvocationID
	round     int
	admission domain.ReviewAdmission
	req       exec.ReviewRequest
	authority domain.Digest
	verifier  exec.ReviewRequestAuthorityVerifier
}

// The phase methods below that may either end the pass or hand on share one
// return shape: the values a phase hands on, then the gate state, a bool that
// is true when the pass ends there, and the error. A phase that does not end
// the pass returns a nil error.

// selectReviewState reads the latest review record and failure, drops either
// when it sits below a successor's or a reevaluation's round floor, and
// removes the workspaces of settled rounds.
func (w *productionPublicationWorkflow) selectReviewState(
	ctx context.Context, task productionPublicationTask,
) (reviewGateSelection, error) {
	latestRecord, latestFailure, err := w.latestReviewState(ctx, task.RunID)
	if err != nil {
		return reviewGateSelection{}, err
	}
	minimumRound := 1
	if task.Successor != nil {
		minimumRound = task.Successor.ReviewRound
		if latestRecord != nil && latestRecord.Round < minimumRound {
			latestRecord = nil
		}
		if latestFailure != nil && latestFailure.Round < minimumRound {
			latestFailure = nil
		}
	}
	if task.reevaluation != nil {
		minimumRound = task.reevaluation.ReviewRound
		if latestRecord != nil && latestRecord.Round < minimumRound {
			latestRecord = nil
		}
		if latestFailure != nil && latestFailure.Round < minimumRound {
			latestFailure = nil
		}
	}
	if latestRecord != nil && latestRecord.Outcome != domain.ReviewFindings {
		if err := w.removeReviewWorkspace(latestRecord.InvocationID); err != nil {
			return reviewGateSelection{}, err
		}
	}
	if latestFailure != nil {
		if err := w.removeReviewWorkspace(latestFailure.InvocationID); err != nil {
			return reviewGateSelection{}, err
		}
	}
	return reviewGateSelection{
		record: latestRecord, failure: latestFailure, minimumRound: minimumRound,
	}, nil
}

// recoverReviewContradiction handles a contradiction failure that outranks
// the latest record. It refuses one bound to another candidate, parks the run
// on the contradiction until reviewContradictionRecovered says the operator
// recovered it, and then aborts the recovered invocation. The first result
// reports that recovery for the hard-limit item ID.
func (w *productionPublicationWorkflow) recoverReviewContradiction(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
	sel reviewGateSelection,
) (bool, productionReviewGateState, bool, error) {
	latestRecord, latestFailure := sel.record, sel.failure
	if latestFailure != nil && latestFailure.Class == domain.ReviewFailureContradiction &&
		(latestRecord == nil || latestFailure.Round > latestRecord.Round) {
		if latestFailure.BaseSHA != binding.admission.Base.BaseSHA ||
			latestFailure.HeadSHA != task.HeadSHA {
			return false, productionReviewPending, true, fmt.Errorf(
				"latest review failure is bound to a different candidate: %w",
				domain.ErrParentKeyMismatch,
			)
		}
		recovered, err := w.reviewContradictionRecovered(ctx, *latestFailure)
		if err != nil {
			return false, productionReviewPending, true, err
		}
		if !recovered {
			if err := w.putReviewContradictionAttention(ctx, task, *latestFailure); err != nil {
				return false, productionReviewPending, true, err
			}
			return false, productionReviewPending, true, nil
		}
		if err := w.abortRecoveredReview(ctx, latestFailure.InvocationID); err != nil {
			return false, productionReviewPending, true, err
		}
		return true, productionReviewPending, false, nil
	}
	return false, productionReviewPending, false, nil
}

// gateReviewConfiguration holds the profile's reviewer configuration digest
// against the effective one. On a mismatch it passes only when
// reviewConfigurationApproved finds an adopted supersession; otherwise it
// parks the run on its configuration failure or records a new one.
func (w *productionPublicationWorkflow) gateReviewConfiguration(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
	sel reviewGateSelection,
) (productionReviewGateState, bool, error) {
	latestRecord, latestFailure, minimumRound := sel.record, sel.failure, sel.minimumRound
	if binding.profile.Review.ConfigDigest != w.reviewConfigurationDigest {
		// The gate consults the adoption whenever the pinned profile disagrees
		// with the effective configuration, not only while the parked failure
		// outranks the latest review row: after the adopted round persists its
		// record (or a later transient failure), a crash-recovered pass must
		// still fall through here instead of re-recording a configuration
		// failure and re-parking an already recovered run.
		approved, err := w.reviewConfigurationApproved(ctx, binding)
		if err != nil {
			return productionReviewPending, true, err
		}
		if !approved {
			if latestFailure != nil && latestFailure.Class == domain.ReviewFailureConfiguration &&
				(latestRecord == nil || latestFailure.Round > latestRecord.Round) {
				state, err := w.parkReviewConfiguration(ctx, task, binding, *latestFailure)
				return state, true, err
			}
			round := minimumRound
			if latestRecord != nil {
				round = latestRecord.Round + 1
			}
			if latestFailure != nil && latestFailure.Round >= round {
				round = latestFailure.Round + 1
			}
			state, err := w.recordReviewSourceFailure(ctx, task,
				ProductionReviewInvocationID(task.RunID, round), round,
				binding.admission.Base.BaseSHA, task.HeadSHA,
				&exec.ReviewSourceFailure{
					Class: domain.ReviewFailureConfiguration,
					Err: reviewConfigurationUnapprovedError(
						binding.profile.Review.ConfigDigest,
						w.reviewConfigurationDigest,
					),
				})
			return state, true, err
		}
		// approved: the operator-authorized supersession approves the
		// effective configuration, so the gate falls through; when the latest
		// row is still the parked failure, the round machinery below re-gates
		// the exact-failure adoption binding before advancing past it.
	}
	return productionReviewPending, false, nil
}

// reconcileCurrentReviewRecord acts on the latest record when it is current.
// It first holds an admitted record's admission to the one the daemon stored.
// A record bound to another candidate passes on only when a remediation
// supersedes it. A current record (same candidate, admitted, current
// instructions) runs the remediation reconcile, the shadow review, and
// external or reentry escalation, then adjudicates its findings or passes
// through passCurrentReviewRecord. The pass continues to a new round only
// when the record is not current or adjudication asks for another round.
func (w *productionPublicationWorkflow) reconcileCurrentReviewRecord(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
	checkpoint productionVerificationCheckpoint,
	workspace PublicationCheckout,
	reviewInstructions exec.ReviewInstructionBinding,
	sel reviewGateSelection,
) (productionReviewGateState, bool, error) {
	latestRecord, latestFailure := sel.record, sel.failure
	// A legacy record carries no admission snapshot: it was requested before
	// review admission was recorded. Its persisted request is a legacy
	// request the source no longer verifies, so the record cannot be re-gated
	// and is not current: the next round supersedes it, as it does a record
	// bound to superseded instructions. Its findings may then never be
	// dispositioned, so the published disposition lineage leaves out a
	// legacy round the admitted round reviewed again
	// (publish.currentDispositionLineage). The record itself stays as
	// stored, and nothing resolves an agent for it from the current lineup.
	//
	// A record's snapshot is a decoded field, so it is held against the
	// snapshot the daemon stored when it admitted the invocation. A record
	// that names another agent, or one the daemon never admitted, is a
	// contradiction, and nothing below acts on it.
	var latestAdmission domain.ReviewAdmission
	latestAdmitted := latestRecord != nil && latestRecord.Admission != nil
	if latestAdmitted {
		stored, found, err := w.storedReviewAdmission(
			ctx, domain.RoleReviewer, task.RunID, latestRecord.InvocationID)
		if err != nil {
			return productionReviewPending, true, err
		}
		if !found || !latestRecord.Admission.Equal(stored) {
			return productionReviewPending, true, fmt.Errorf(
				"review record %s carries an admission the daemon did not store for it: %w",
				latestRecord.InvocationID, domain.ErrReviewAdmissionMismatch)
		}
		latestAdmission = stored
	}
	advanceReview := false
	if latestRecord != nil && latestRecord.ConfigurationDigest == w.reviewConfigurationDigest &&
		(latestFailure == nil || latestRecord.Round > latestFailure.Round) {
		if latestRecord.BaseSHA != binding.admission.Base.BaseSHA || latestRecord.HeadSHA != task.HeadSHA {
			superseded, err := w.remediationSupersedesReview(ctx, task, *latestRecord)
			if err != nil {
				return productionReviewPending, true, err
			}
			if !superseded {
				return productionReviewPending, true, fmt.Errorf(
					"latest review record is bound to a different candidate: %w", domain.ErrParentKeyMismatch,
				)
			}
		} else if latestAdmitted && latestRecord.InstructionDigest == reviewInstructions.ResultDigest {
			var remediationOutcome remediationReviewOutcome
			if _, remediated := remediationRoundForInvocation(
				task.RunID, task.ProducingInvocationID,
			); remediated {
				findings, err := w.reviewRecordFindings(ctx, *latestRecord)
				if err != nil {
					return productionReviewPending, true, err
				}
				remediationOutcome, err = w.reconcileRemediationReview(
					ctx, task, *latestRecord, findings, checkpoint.Imported.Claims)
				if err != nil {
					return productionReviewPending, true, err
				}
				if remediationOutcome.attention != "" {
					if err := w.putReviewAttentionWithActionsAndID(
						ctx, task, *latestRecord, remediationOutcome.attention,
						domain.AttentionReviewDispute,
						productionReviewItemID(task.RunID, latestRecord.Round),
						[]domain.Action{domain.ActionDiscuss, domain.ActionStop},
						remediationOutcome.claims,
					); err != nil {
						return productionReviewPending, true, err
					}
					return productionReviewEscalated, true, nil
				}
			}
			shadowComplete, err := w.reconcileShadowReview(
				ctx, task, binding, checkpoint, workspace, reviewInstructions, *latestRecord,
			)
			if err != nil {
				return productionReviewPending, true, err
			}
			if !shadowComplete {
				return productionReviewPending, true, nil
			}
			adjudicatesExternal, err := w.adjudicatesExternalReview(ctx, task, binding, *latestRecord)
			if err != nil {
				return productionReviewPending, true, err
			}
			if task.answersExternalReview() && !adjudicatesExternal {
				state, err := w.escalateExternalReviewFindings(ctx, task, *latestRecord)
				return state, true, err
			}
			if latestRecord.Outcome == domain.ReviewFindings && task.reentersInPlace() &&
				!adjudicatesExternal {
				state, err := w.escalateReentryFindings(ctx, task, *latestRecord)
				return state, true, err
			}
			// An external review cycle's first round adjudicates its external
			// findings whatever Freeside's own review found, so a clean record
			// reaches adjudication there too.
			if latestRecord.Outcome == domain.ReviewFindings || adjudicatesExternal {
				candidateWorkspace, err := w.ensureReviewWorkspace(
					ctx, latestRecord.InvocationID, workspace, task.HeadSHA)
				if err != nil {
					return productionReviewPending, true, err
				}
				baseWorkspaceID := findingAdjudicationBaseWorkspaceID(latestRecord.InvocationID)
				baseWorkspace, err := w.ensureReviewWorkspace(
					ctx, baseWorkspaceID, workspace, binding.admission.Base.BaseSHA)
				if err != nil {
					return productionReviewPending, true, err
				}
				var state productionReviewGateState
				if remediationOutcome.dissent != nil {
					state, err = w.reenterFindingAdjudication(
						ctx, task, binding, *latestRecord, baseWorkspace,
						candidateWorkspace, *remediationOutcome.dissent)
				} else {
					state, err = w.reconcileFindingAdjudication(
						ctx, task, binding, *latestRecord, baseWorkspace, candidateWorkspace)
				}
				if err != nil {
					return productionReviewPending, true, err
				}
				if err := w.removeReviewWorkspace(latestRecord.InvocationID); err != nil {
					return productionReviewPending, true, err
				}
				if err := w.removeReviewWorkspace(baseWorkspaceID); err != nil {
					return productionReviewPending, true, err
				}
				if state != productionReviewContinue {
					return state, true, nil
				}
				advanceReview = true
			}
			if !advanceReview {
				state, err := w.passCurrentReviewRecord(
					ctx, task, binding, checkpoint, reviewInstructions, latestRecord, latestAdmission)
				return state, true, err
			}
		}
	}
	return productionReviewPending, false, nil
}

// passCurrentReviewRecord passes a current record that needs no adjudication.
// It rebuilds the record's request from current state, re-gates its persisted
// authority through the review source's VerifyRequestAuthority, and clears the
// pending retry before reporting the pass.
func (w *productionPublicationWorkflow) passCurrentReviewRecord(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
	checkpoint productionVerificationCheckpoint,
	reviewInstructions exec.ReviewInstructionBinding,
	latestRecord *domain.ReviewRecord,
	latestAdmission domain.ReviewAdmission,
) (productionReviewGateState, error) {
	req, requestAuthority, err := w.productionReviewRequest(
		ctx, task, binding, checkpoint, latestRecord.InvocationID,
		latestRecord.Round, reviewInstructions, latestAdmission,
	)
	if err != nil {
		return productionReviewPending, err
	}
	requestAuthority, err = w.persistedReviewAuthority(
		ctx, latestRecord.InvocationID, req, requestAuthority)
	if err != nil {
		return productionReviewPending, err
	}
	authorityVerifier, ok := w.reviewSource.(exec.ReviewRequestAuthorityVerifier)
	if !ok {
		return productionReviewPending, errors.New(
			"review source cannot re-gate persisted request authority")
	}
	if err := authorityVerifier.VerifyRequestAuthority(
		ctx, latestRecord.InvocationID, requestAuthority,
	); err != nil {
		return productionReviewPending, fmt.Errorf(
			"re-gate persisted review instruction authority: %w", err)
	}
	// This clean-pass re-entry is downstream of the record write that
	// already cleared the durable row; clear its durable twin too so a
	// crash-recovered pass never leaves an orphan pending retry.
	if err := w.store.Write(ctx, func(tx *store.WriteTx) error {
		return tx.DeleteReviewRetry(ctx, task.RunID)
	}); err != nil {
		return productionReviewPending, err
	}
	delete(w.reviewRetryAfter, task.RunID)
	return productionReviewPassed, nil
}

// chooseReviewRound picks the next round. A failure that outranks the latest
// record must be bound to this candidate; a quota failure escalates, a
// configuration failure waits for reviewConfigurationRecovered, and a
// transient one sets the retry deadline. It then escalates past
// reviewHardRoundLimit and waits out the retry deadline, rebuilding a lost
// one through reconstructReviewRetryDeadline. An adopted configuration
// failure sets its own copy of recoveredContradiction, which only the
// hard-limit item ID here reads.
func (w *productionPublicationWorkflow) chooseReviewRound(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
	sel reviewGateSelection,
	recoveredContradiction bool,
) (int, productionReviewGateState, bool, error) {
	latestRecord, latestFailure, minimumRound := sel.record, sel.failure, sel.minimumRound
	round := minimumRound
	if latestRecord != nil {
		round = latestRecord.Round + 1
	}
	if latestFailure != nil && latestFailure.Round >= round {
		if latestFailure.BaseSHA != binding.admission.Base.BaseSHA ||
			latestFailure.HeadSHA != task.HeadSHA {
			return 0, productionReviewPending, true, fmt.Errorf(
				"latest review failure is bound to a different candidate: %w",
				domain.ErrParentKeyMismatch,
			)
		}
		round = latestFailure.Round + 1
		if latestFailure.Class == domain.ReviewFailureQuota {
			record := domain.ReviewRecord{
				InvocationID: latestFailure.InvocationID, RunID: latestFailure.RunID,
				Round: latestFailure.Round, BaseSHA: latestFailure.BaseSHA, HeadSHA: latestFailure.HeadSHA,
			}
			if err := w.putReviewAttention(ctx, task, record,
				fmt.Sprintf("Review stopped because of a %s failure.", latestFailure.Class),
				domain.AttentionReviewDispute,
			); err != nil {
				return 0, productionReviewPending, true, err
			}
			return 0, productionReviewEscalated, true, nil
		}
		if latestFailure.Class == domain.ReviewFailureConfiguration {
			adopted, err := w.reviewConfigurationRecovered(ctx, binding, *latestFailure)
			if err != nil {
				return 0, productionReviewPending, true, err
			}
			if !adopted {
				state, err := w.parkReviewConfiguration(ctx, task, binding, *latestFailure)
				return 0, state, true, err
			}
			// The adopted failure row's round may sit exactly at the hard
			// limit; the exhaustion item below must then live under the
			// recovered-identity namespace, because this round's ordinary
			// identity already carries the concluded review_configuration item.
			recoveredContradiction = true
		}
		if latestFailure.Class == domain.ReviewFailureTransient {
			retryAt := latestFailure.ObservedAt.Add(reviewRetryDelay(latestFailure.Round))
			if w.now().Before(retryAt) {
				w.reviewRetryAfter[task.RunID] = retryAt
			}
		}
	}
	hardLimit, err := w.reviewHardRoundLimit(ctx, task.RunID)
	if err != nil {
		return 0, productionReviewPending, true, err
	}
	if round > hardLimit {
		record := domain.ReviewRecord{
			RunID: task.RunID, Round: hardLimit,
			BaseSHA: binding.admission.Base.BaseSHA, HeadSHA: task.HeadSHA,
		}
		itemID := productionReviewHardLimitItemID(task.RunID, hardLimit, recoveredContradiction)
		if task.answersExternalReview() {
			itemID = externalReviewExhaustionItemID(task.RunID, task.Successor.ReviewRound)
		} else if task.reentersInPlace() {
			legacyOpen, err := w.readinessLegacyExhaustionOpen(ctx, task, itemID)
			if err != nil {
				return 0, productionReviewPending, true, err
			}
			if !legacyOpen {
				itemID = readinessReentryExhaustionItemID(*task.Successor)
			}
		}
		if err := w.putReviewAttentionWithID(ctx, task, record,
			fmt.Sprintf("Review exhausted the resolved hard limit of %d rounds.", hardLimit),
			domain.AttentionReviewDiminishing, itemID,
		); err != nil {
			return 0, productionReviewPending, true, err
		}
		return 0, productionReviewEscalated, true, nil
	}
	if retryAt, ok := w.reviewRetryAfter[task.RunID]; ok {
		if w.now().Before(retryAt) {
			return 0, productionReviewPending, true, nil
		}
	} else {
		// No process-local deadline: a restart may have dropped a pending
		// same-invocation retry. Reconstruct it from the durable row, which
		// fails closed on a corrupt or unreadable row rather than retrying.
		pending, err := w.reconstructReviewRetryDeadline(ctx, task, binding, round)
		if err != nil {
			return 0, productionReviewPending, true, err
		}
		if pending {
			return 0, productionReviewPending, true, nil
		}
	}
	return round, productionReviewPending, false, nil
}

// requestReviewRound admits the round's reviewer through ensureReviewAdmission
// (a refusal fails the invocation, a capacity or memory refusal waits),
// releases a launch-memory quarantine, and records the request. Before any
// launch it requires an authority verifier, checks the request against
// VerifyReviewRequestSupersession, and re-gates its persisted authority
// through VerifyRequestAuthority.
func (w *productionPublicationWorkflow) requestReviewRound(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
	checkpoint productionVerificationCheckpoint,
	reviewInstructions exec.ReviewInstructionBinding,
	round int,
) (reviewRoundInvocation, productionReviewGateState, bool, error) {
	id := ProductionReviewInvocationID(task.RunID, round)
	// Admission comes before the request: the request and its authority
	// digest carry the snapshot. A refusal fails the invocation as the
	// source's own admission check does, and nothing is requested.
	admission, err := w.ensureReviewAdmission(ctx, domain.RoleReviewer, task.RunID, id)
	if err != nil {
		var refusal *exec.ReviewSourceFailure
		if errors.As(err, &refusal) {
			state, err := w.retryOrRecordReviewFailure(ctx, task, id, round,
				binding.admission.Base.BaseSHA, task.HeadSHA, err)
			return reviewRoundInvocation{}, state, true, err
		}
		// A full pool, memory that does not fit yet, a routed reviewer's
		// marked window, or an update drain is a wait, not a source failure:
		// it spends no retry budget, and a later pass asks again once
		// capacity frees, the window resets, or the drain lifts.
		if reviewCapacityRefusal(err) {
			return reviewRoundInvocation{}, productionReviewPending, true,
				w.holdReviewForCapacity(ctx, task.RunID, id, err)
		}
		// A launch the budget refuses for good, as under a runtime with no
		// recorded overhead or a budget lowered below it, waits under the
		// notice for a budget or start that fits it.
		if launchMemoryRefusal(err) {
			return reviewRoundInvocation{}, productionReviewPending, true, noticeLaunchMemoryRefusal(
				ctx, w.store, w.attention, &w.holdPace, task.RunID, id, task.ProjectID, err)
		}
		return reviewRoundInvocation{}, productionReviewPending, true, err
	}
	// Not gated on the startup bound: a budget recorded since may have raised
	// the notice.
	if err := releaseProductionQuarantine(ctx, w.store, w.attention, launchMemoryRefusedPrefix, task.RunID); err != nil {
		return reviewRoundInvocation{}, productionReviewPending, true, err
	}
	req, requestAuthority, err := w.productionReviewRequest(
		ctx, task, binding, checkpoint, id, round, reviewInstructions, admission,
	)
	if err != nil {
		return reviewRoundInvocation{}, productionReviewPending, true, err
	}
	if err := w.recordReviewRequest(ctx, id, req); err != nil {
		return reviewRoundInvocation{}, productionReviewPending, true, err
	}
	authorityVerifier, ok := w.reviewSource.(exec.ReviewRequestAuthorityVerifier)
	if !ok {
		state, err := w.recordReviewSourceFailure(ctx, task, id, round,
			binding.admission.Base.BaseSHA, task.HeadSHA,
			&exec.ReviewSourceFailure{
				Class: domain.ReviewFailureConfiguration,
				Err:   errors.New("review source cannot verify request authority"),
			},
		)
		return reviewRoundInvocation{}, state, true, err
	}
	if supersessionVerifier, ok := w.reviewSource.(exec.ReviewRequestSupersessionVerifier); ok {
		if err := supersessionVerifier.VerifyReviewRequestSupersession(ctx, id, req); err != nil &&
			!errors.Is(err, exec.ErrUnknownInvocation) {
			if errors.Is(err, exec.ErrSupersededReviewRequest) || errors.Is(err, exec.ErrLegacyReviewRequest) {
				state, err := w.recordReviewSourceFailure(ctx, task, id, round,
					binding.admission.Base.BaseSHA, task.HeadSHA,
					&exec.ReviewSourceFailure{Class: domain.ReviewFailureTransient, Err: err})
				return reviewRoundInvocation{}, state, true, err
			}
			state, err := w.retryOrRecordReviewFailure(ctx, task, id, round,
				binding.admission.Base.BaseSHA, task.HeadSHA, err)
			return reviewRoundInvocation{}, state, true, err
		}
	}
	// Re-gate the persisted request against the engine's current authority
	// before Inspect may act on it: Inspect's restart-recovery window
	// relaunches from the decoded journal row, so a rewritten-but-valid row
	// must fail closed here, before it can prepare a workspace or start a
	// credential-bearing review, not only after Poll delivers a result.
	if err := authorityVerifier.VerifyRequestAuthority(ctx, id, requestAuthority); err != nil &&
		!errors.Is(err, exec.ErrUnknownInvocation) {
		if errors.Is(err, exec.ErrLegacyReviewRequest) {
			state, err := w.recordReviewSourceFailure(ctx, task, id, round,
				binding.admission.Base.BaseSHA, task.HeadSHA,
				&exec.ReviewSourceFailure{
					Class: domain.ReviewFailureTransient,
					Err:   err,
				},
			)
			return reviewRoundInvocation{}, state, true, err
		}
		state, err := w.retryOrRecordReviewFailure(ctx, task, id, round,
			binding.admission.Base.BaseSHA, task.HeadSHA, err)
		return reviewRoundInvocation{}, state, true, err
	}
	return reviewRoundInvocation{
		id: id, round: round, admission: admission, req: req,
		authority: requestAuthority, verifier: authorityVerifier,
	}, productionReviewPending, false, nil
}

// launchReviewRound inspects the requested invocation, claiming provider
// dispatch first. An unknown invocation is launched: its retained workspace
// must match the request's, and the launch runs between the
// DurableTransitionReviewRequest hooks with a dispatch claim before the
// request and before the second Inspect. It records the observed status and
// ends the pass while the review is pending or running.
func (w *productionPublicationWorkflow) launchReviewRound(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
	workspace PublicationCheckout,
	inv reviewRoundInvocation,
) (productionReviewGateState, bool, error) {
	id, round, req := inv.id, inv.round, inv.req
	reviewWorkspace := req.Workspace
	if err := w.claimProviderDispatch(ctx, task.RunID); err != nil {
		return productionReviewPending, true, err
	}
	status, err := w.reviewSource.Inspect(ctx, id)
	if errors.Is(err, exec.ErrUnknownInvocation) {
		// Unknown is also the workspace-staleness signal: no live review
		// container can reference this invocation's retained path, and the
		// launch below will seed its replacement. Keep that answer in this
		// control flow instead of querying the review source again.
		retainedWorkspace, workspaceErr := w.ensureReviewWorkspace(ctx, id, workspace, req.HeadSHA)
		if workspaceErr != nil {
			if errors.Is(workspaceErr, publish.ErrMaterializationRefused) {
				state, err := w.recordReviewSourceFailure(ctx, task, id, round,
					binding.admission.Base.BaseSHA, task.HeadSHA,
					&exec.ReviewSourceFailure{
						Class: domain.ReviewFailureConfiguration,
						Err:   workspaceErr,
					},
				)
				return state, true, err
			}
			return productionReviewPending, true, workspaceErr
		}
		if retainedWorkspace != reviewWorkspace {
			return productionReviewPending, true, fmt.Errorf(
				"retained review workspace changed: %w", domain.ErrPathBoundaryMismatch,
			)
		}
		if err := runDurableTransitionHook(w.transitionHook,
			DurableTransitionReviewRequest, DurableTransitionBefore); err != nil {
			return productionReviewPending, true, err
		}
		if err := w.claimProviderDispatch(ctx, task.RunID); err != nil {
			return productionReviewPending, true, err
		}
		if err := w.reviewSource.RequestReview(ctx, id, req); err != nil {
			state, err := w.retryOrRecordReviewFailure(ctx, task, id, round, req.BaseSHA, req.HeadSHA, err)
			return state, true, err
		}
		if err := runDurableTransitionHook(w.transitionHook,
			DurableTransitionReviewRequest, DurableTransitionAfter); err != nil {
			return productionReviewPending, true, err
		}
		if err := w.claimProviderDispatch(ctx, task.RunID); err != nil {
			return productionReviewPending, true, err
		}
		status, err = w.reviewSource.Inspect(ctx, id)
		if err != nil {
			state, err := w.retryOrRecordReviewFailure(ctx, task, id, round, req.BaseSHA, req.HeadSHA, err)
			return state, true, err
		}
	}
	if err != nil {
		state, err := w.retryOrRecordReviewFailure(
			ctx, task, id, round, binding.admission.Base.BaseSHA, task.HeadSHA, err,
		)
		return state, true, err
	}
	if err := w.observeReviewInvocation(ctx, task.RunID, id, status); err != nil {
		return productionReviewPending, true, err
	}
	if status == exec.StatusPending || status == exec.StatusRunning {
		return productionReviewPending, true, nil
	}
	return productionReviewPending, false, nil
}

// pollReviewRound polls the finished invocation and validates its result
// before anything is recorded: result.Validate, the invocation, base, head,
// and configuration binding, the instruction digest, equality with the stored
// admission, a post-result VerifyRequestAuthority, the source's Verify, each
// finding's run, and each finding's overlap with the engine's own reviewed
// diff. A failed check records a source failure or schedules a retry.
func (w *productionPublicationWorkflow) pollReviewRound(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
	inv reviewRoundInvocation,
) (exec.ReviewResult, productionReviewGateState, bool, error) {
	id, round, req, admission := inv.id, inv.round, inv.req, inv.admission
	requestAuthority, authorityVerifier := inv.authority, inv.verifier
	reviewWorkspace := req.Workspace
	result, err := w.reviewSource.Poll(ctx, id)
	if errors.Is(err, exec.ErrNoResult) {
		err = normalizeTerminalReviewFailure(err)
	} else if err != nil {
		scheduled, schedErr := w.scheduleReviewInvocationRetry(
			ctx, id, task.RunID, round, binding.admission.Base.BaseSHA, task.HeadSHA, err)
		if schedErr != nil {
			return exec.ReviewResult{}, productionReviewPending, true, schedErr
		}
		if scheduled {
			return exec.ReviewResult{}, productionReviewPending, true, nil
		}
	}
	if errors.Is(err, exec.ErrResultNotReady) {
		return exec.ReviewResult{}, productionReviewPending, true, nil
	}
	if err != nil {
		state, err := w.recordReviewSourceFailure(
			ctx, task, id, round, binding.admission.Base.BaseSHA, task.HeadSHA, err,
		)
		return exec.ReviewResult{}, state, true, err
	}
	if err := result.Validate(); err != nil {
		state, err := w.recordReviewSourceFailure(ctx, task, id, round,
			binding.admission.Base.BaseSHA, task.HeadSHA,
			reviewSourceFailureWithUsage(domain.ReviewFailureContradiction, err, result.Usage),
		)
		return exec.ReviewResult{}, state, true, err
	}
	if result.InvocationID != id || result.BaseSHA != binding.admission.Base.BaseSHA ||
		result.HeadSHA != task.HeadSHA || result.ConfigurationDigest != w.reviewConfigurationDigest {
		state, err := w.recordReviewSourceFailure(ctx, task, id, round,
			binding.admission.Base.BaseSHA, task.HeadSHA,
			reviewSourceFailureWithUsage(
				domain.ReviewFailureContradiction, domain.ErrParentKeyMismatch, result.Usage),
		)
		return exec.ReviewResult{}, state, true, err
	}
	if result.InstructionDigest != req.Instructions.ResultDigest {
		state, err := w.recordReviewSourceFailure(ctx, task, id, round,
			binding.admission.Base.BaseSHA, task.HeadSHA,
			reviewSourceFailureWithUsage(
				domain.ReviewFailureContradiction, domain.ErrParentKeyMismatch, result.Usage),
		)
		return exec.ReviewResult{}, state, true, err
	}
	// The result says which agent reviewed by returning the snapshot it was
	// asked under. One that differs from the stored snapshot names an agent
	// the daemon did not admit for this invocation, so the pass is a
	// contradiction and no record is written.
	if !result.Admission.Equal(admission) {
		state, err := w.recordReviewSourceFailure(ctx, task, id, round,
			binding.admission.Base.BaseSHA, task.HeadSHA,
			reviewSourceFailureWithUsage(domain.ReviewFailureContradiction,
				fmt.Errorf("review result %s: %w", id, domain.ErrReviewAdmissionMismatch), result.Usage),
		)
		return exec.ReviewResult{}, state, true, err
	}
	if err := authorityVerifier.VerifyRequestAuthority(ctx, id, requestAuthority); err != nil {
		state, err := w.retryOrRecordReviewFailure(ctx, task, id, round,
			binding.admission.Base.BaseSHA, task.HeadSHA,
			preserveReviewSourceFailureUsage(err, result.Usage),
		)
		return exec.ReviewResult{}, state, true, err
	}
	if err := w.reviewSource.Verify(ctx, id, binding.admission.Base.BaseSHA, task.HeadSHA); err != nil {
		scheduled, schedErr := w.scheduleReviewInvocationRetry(
			ctx, id, task.RunID, round, binding.admission.Base.BaseSHA, task.HeadSHA,
			preserveReviewSourceFailureUsage(err, result.Usage))
		if schedErr != nil {
			return exec.ReviewResult{}, productionReviewPending, true, schedErr
		}
		if scheduled || errors.Is(err, exec.ErrResultNotReady) {
			return exec.ReviewResult{}, productionReviewPending, true, nil
		}
		state, err := w.recordReviewSourceFailure(ctx, task, id, round,
			binding.admission.Base.BaseSHA, task.HeadSHA,
			preserveReviewSourceFailureUsage(err, result.Usage),
		)
		return exec.ReviewResult{}, state, true, err
	}
	for _, finding := range result.Findings {
		if finding.RunID != task.RunID {
			state, err := w.recordReviewSourceFailure(ctx, task, id, round,
				binding.admission.Base.BaseSHA, task.HeadSHA,
				reviewSourceFailureWithUsage(
					domain.ReviewFailureContradiction, domain.ErrParentKeyMismatch, result.Usage),
			)
			return exec.ReviewResult{}, state, true, err
		}
	}
	// Fail closed unless every routed finding resolves into the exact reviewed
	// base-to-head diff. The engine derives that diff itself (never trusting the
	// reviewer's), so a syntactically valid finding on an untouched path or an
	// unchanged line is a reviewer contradiction rejected before any review
	// record, finding, disposition, or remediation intent is persisted. The
	// routed ward schema always carries a location (a line range, or a whole-file
	// marker for a candidate-deleted file), so a nil or non-overlapping location
	// is a contract violation that Overlaps fails closed on. A failure to derive
	// or parse our own trusted diff is an engine fault, not a reviewer
	// contradiction, so it takes the retry/pending path instead.
	if len(result.Findings) > 0 {
		scope, err := reviewedDiffScope(
			ctx, w.workDir, reviewWorkspace, binding.admission.Base.BaseSHA, req.HeadSHA)
		if err != nil {
			return exec.ReviewResult{}, productionReviewPending, true, err
		}
		for _, finding := range result.Findings {
			if scope.Overlaps(finding.Location) {
				continue
			}
			locText := "<none>"
			if finding.Location != nil {
				locText = finding.Location.String()
			}
			state, err := w.recordReviewSourceFailure(ctx, task, id, round,
				binding.admission.Base.BaseSHA, task.HeadSHA,
				reviewSourceFailureWithUsage(
					domain.ReviewFailureContradiction, fmt.Errorf(
						"review finding %s location %s does not overlap the reviewed diff: %w",
						finding.ID, locText, domain.ErrParentKeyMismatch), result.Usage),
			)
			return exec.ReviewResult{}, state, true, err
		}
	}
	return result, productionReviewPending, false, nil
}

// recordReviewRound builds the round's record from the stored admission,
// never the result's, reconciles a remediation's dispositions, and writes the
// usage, record, diff metrics, dispositions, and the retry delete in one
// write between the DurableTransitionReviewResult hooks.
func (w *productionPublicationWorkflow) recordReviewRound(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
	checkpoint productionVerificationCheckpoint,
	inv reviewRoundInvocation,
	result exec.ReviewResult,
) (domain.ReviewRecord, remediationReviewOutcome, error) {
	id, round, admission := inv.id, inv.round, inv.admission
	reviewWorkspace := inv.req.Workspace
	outcome := domain.ReviewClean
	findingIDs := make([]domain.FindingID, len(result.Findings))
	if len(result.Findings) > 0 {
		outcome = domain.ReviewFindings
		for i := range result.Findings {
			findingIDs[i] = result.Findings[i].ID
		}
	}
	record, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: id, RunID: task.RunID, Round: round,
		// The stored snapshot, which the result was just held to: the record
		// never takes its attribution from a returned object.
		Admission:           &admission,
		ConfigurationDigest: result.ConfigurationDigest,
		InstructionDigest:   result.InstructionDigest,
		BaseSHA:             result.BaseSHA, HeadSHA: result.HeadSHA,
		CompletedAt: result.CompletedAt, CompletionEvidence: result.CompletionEvidence,
		Outcome: outcome, FindingIDs: findingIDs,
	})
	if err != nil {
		return domain.ReviewRecord{}, remediationReviewOutcome{}, err
	}
	remediationOutcome, err := w.reconcileRemediationReview(
		ctx, task, record, result.Findings, checkpoint.Imported.Claims)
	if err != nil {
		return domain.ReviewRecord{}, remediationReviewOutcome{}, err
	}
	diffMetrics, err := w.reviewRoundDiffMetrics(ctx, task, binding, reviewWorkspace, record)
	if err != nil {
		return domain.ReviewRecord{}, remediationReviewOutcome{}, err
	}
	// The completed record supersedes any pending same-invocation retry for
	// this run: clear the durable row atomically with the record it writes.
	if err := runDurableTransitionHook(w.transitionHook,
		DurableTransitionReviewResult, DurableTransitionBefore); err != nil {
		return domain.ReviewRecord{}, remediationReviewOutcome{}, err
	}
	if err := w.store.Write(ctx, func(tx *store.WriteTx) error {
		if _, err := tx.AppendUsageObservations(ctx, id, result.Usage); err != nil {
			return err
		}
		// Metrics are written only with the record they describe, never
		// backfilled: an item may already have snapshotted a replayed round's
		// history without them.
		_, err := tx.GetReviewRecord(ctx, id)
		created := errors.Is(err, store.ErrNotFound)
		if err != nil && !created {
			return err
		}
		if err := tx.PutReviewRecord(ctx, record, result.Findings); err != nil {
			return err
		}
		if created && diffMetrics != nil {
			if err := tx.PutReviewRoundDiffMetrics(
				ctx, task.RunID, record.Round, *diffMetrics); err != nil {
				return err
			}
		}
		for _, disposition := range remediationOutcome.dispositions {
			if err := tx.PutFindingDisposition(ctx, disposition); err != nil {
				return err
			}
		}
		for _, disposition := range remediationOutcome.externalDispositions {
			if err := tx.PutExternalFindingDisposition(ctx, disposition); err != nil {
				return err
			}
		}
		return tx.DeleteReviewRetry(ctx, task.RunID)
	}); err != nil {
		return domain.ReviewRecord{}, remediationReviewOutcome{}, err
	}
	if err := runDurableTransitionHook(w.transitionHook,
		DurableTransitionReviewResult, DurableTransitionAfter); err != nil {
		return domain.ReviewRecord{}, remediationReviewOutcome{}, err
	}
	return record, remediationOutcome, nil
}

// settleReviewRound concludes the recorded round: the shadow review first,
// then a remediation's attention item, external or reentry escalation, and
// finding adjudication, removing the round's workspaces. Unlike
// reconcileCurrentReviewRecord, it turns productionReviewContinue into
// productionReviewPending, so the next pass starts the next round.
func (w *productionPublicationWorkflow) settleReviewRound(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
	checkpoint productionVerificationCheckpoint,
	workspace PublicationCheckout,
	reviewInstructions exec.ReviewInstructionBinding,
	inv reviewRoundInvocation,
	record domain.ReviewRecord,
	remediationOutcome remediationReviewOutcome,
) (productionReviewGateState, error) {
	id := inv.id
	reviewWorkspace := inv.req.Workspace
	shadowComplete, err := w.reconcileShadowReview(
		ctx, task, binding, checkpoint, workspace, reviewInstructions, record,
	)
	if err != nil {
		return productionReviewPending, err
	}
	if !shadowComplete {
		return productionReviewPending, nil
	}
	if remediationOutcome.attention != "" {
		if err := w.putReviewAttentionWithActionsAndID(
			ctx, task, record, remediationOutcome.attention, domain.AttentionReviewDispute,
			productionReviewItemID(task.RunID, record.Round),
			[]domain.Action{domain.ActionDiscuss, domain.ActionStop},
			remediationOutcome.claims,
		); err != nil {
			return productionReviewPending, err
		}
		return productionReviewEscalated, nil
	}
	adjudicatesExternal, err := w.adjudicatesExternalReview(ctx, task, binding, record)
	if err != nil {
		return productionReviewPending, err
	}
	if task.answersExternalReview() && !adjudicatesExternal {
		return w.escalateExternalReviewFindings(ctx, task, record)
	}
	if record.Outcome == domain.ReviewFindings && task.reentersInPlace() && !adjudicatesExternal {
		return w.escalateReentryFindings(ctx, task, record)
	}
	// An external review cycle's first round adjudicates its external findings
	// whatever Freeside's own review found, so a clean record reaches
	// adjudication there too.
	if record.Outcome == domain.ReviewFindings || adjudicatesExternal {
		baseWorkspaceID := findingAdjudicationBaseWorkspaceID(id)
		baseWorkspace, err := w.ensureReviewWorkspace(
			ctx, baseWorkspaceID, workspace, binding.admission.Base.BaseSHA)
		if err != nil {
			return productionReviewPending, err
		}
		var state productionReviewGateState
		if remediationOutcome.dissent != nil {
			state, err = w.reenterFindingAdjudication(
				ctx, task, binding, record, baseWorkspace, reviewWorkspace,
				*remediationOutcome.dissent)
		} else {
			state, err = w.reconcileFindingAdjudication(
				ctx, task, binding, record, baseWorkspace, reviewWorkspace)
		}
		if err != nil {
			return productionReviewPending, err
		}
		if err := w.removeReviewWorkspace(id); err != nil {
			return productionReviewPending, err
		}
		if err := w.removeReviewWorkspace(baseWorkspaceID); err != nil {
			return productionReviewPending, err
		}
		if state == productionReviewContinue {
			return productionReviewPending, nil
		}
		return state, nil
	}
	if err := w.removeReviewWorkspace(id); err != nil {
		return productionReviewPending, err
	}
	return productionReviewPassed, nil
}

// An upgrade may resume after the old writer created an exhaustion item but
// before it finished the task. Only an open item with matching coordinates
// proves a person is still being asked; a resolved item may be an older cycle's.
func (w *productionPublicationWorkflow) readinessLegacyExhaustionOpen(
	ctx context.Context, task productionPublicationTask, itemID domain.ItemID,
) (bool, error) {
	open := false
	err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		item, err := tx.GetAttentionItem(ctx, itemID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		open = item.Status == domain.StatusOpen && item.Type == domain.AttentionReviewDiminishing &&
			item.ProjectID == task.ProjectID && item.Subject.Type == domain.SubjectRun &&
			item.Subject.ID == domain.SubjectID(task.RunID) && item.Subject.RunID != nil &&
			*item.Subject.RunID == task.RunID && item.PRHeadSHA == task.HeadSHA
		return nil
	})
	return open, err
}

func (w *productionPublicationWorkflow) classifyReviewFindings(
	ctx context.Context, task productionPublicationTask, record domain.ReviewRecord,
) (bool, error) {
	if w.inference == nil || record.Outcome != domain.ReviewFindings {
		return false, nil
	}
	requiresAttention := false
	for _, findingID := range record.FindingIDs {
		var (
			finding        domain.Finding
			classification domain.Classification
			classified     bool
		)
		if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			finding, err = tx.GetFinding(ctx, findingID)
			if err != nil {
				return err
			}
			classification, err = tx.GetClassification(ctx, findingID, record.Round)
			if err == nil {
				classified = true
				return nil
			}
			if errors.Is(err, store.ErrNotFound) {
				return nil
			}
			return err
		}); err != nil {
			return true, err
		}
		if classified {
			persistedAttention, err := w.inference.EvaluateClassification(finding, classification)
			if err != nil {
				return true, err
			}
			requiresAttention = requiresAttention || persistedAttention
			continue
		}
		if err := w.claimProviderDispatch(ctx, task.RunID); err != nil {
			return true, err
		}
		decision, err := w.inference.ClassifyFinding(
			ctx, string(task.ProjectID), string(task.RunID), finding, record.Round,
		)
		if err != nil {
			return true, err
		}
		requiresAttention = requiresAttention || decision.RequiresAttention
		if err := w.store.Write(ctx, func(tx *store.WriteTx) error {
			return tx.PutClassification(ctx, decision.Classification)
		}); err != nil {
			return true, err
		}
	}
	return requiresAttention, nil
}

func normalizeTerminalReviewFailure(err error) error {
	var failure *exec.ReviewSourceFailure
	if errors.As(err, &failure) {
		return err
	}
	return &exec.ReviewSourceFailure{Class: domain.ReviewFailureTransient, Err: err}
}

// reviewRetryDelay is the exponential same-invocation/terminal-transient
// backoff: 1s doubling per round, capped at round 9 (2^8 s). The single
// definition keeps the same-invocation scheduler, the terminal-transient
// reconstruction, and the durable-row reconstruction from drifting apart.
func reviewRetryDelay(round int) time.Duration {
	return time.Second << min(round-1, 8)
}

// scheduleReviewInvocationRetry paces a same-invocation transient retry: it
// sets the process-local deadline and durably records it so a restart during
// the backoff reconstructs the remaining delay instead of retrying
// immediately (issue #498). The row does not terminalize or advance the
// invocation. On a persist failure it still holds the process-local bound,
// then returns the error so the pass surfaces it rather than proceeding.
func (w *productionPublicationWorkflow) scheduleReviewInvocationRetry(
	ctx context.Context, id domain.InvocationID, runID domain.RunID, round int,
	baseSHA, headSHA string, cause error,
) (bool, error) {
	if exec.ClassifyReviewSourceFailure(cause) != domain.ReviewFailureTransient {
		return false, nil
	}
	observedAt := w.now().UTC()
	w.reviewRetryAfter[runID] = observedAt.Add(reviewRetryDelay(round))
	retry := domain.ReviewRetry{
		RunID: runID, InvocationID: id, Round: round,
		BaseSHA: baseSHA, HeadSHA: headSHA, ObservedAt: observedAt, Reason: cause.Error(),
	}
	if err := w.store.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutReviewRetry(ctx, retry)
	}); err != nil {
		return true, fmt.Errorf("persist review retry %q: %w", id, err)
	}
	return true, nil
}

// retryOrRecordReviewFailure paces a transient observation failure as a
// same-invocation retry, or records a terminal failure otherwise. It
// centralizes the pending-or-record branch shared by the observation call
// sites; the two sites that also fold in ErrResultNotReady stay inline.
func (w *productionPublicationWorkflow) retryOrRecordReviewFailure(
	ctx context.Context, task productionPublicationTask, id domain.InvocationID,
	round int, baseSHA, headSHA string, cause error,
) (productionReviewGateState, error) {
	scheduled, err := w.scheduleReviewInvocationRetry(ctx, id, task.RunID, round, baseSHA, headSHA, cause)
	if err != nil {
		return productionReviewPending, err
	}
	if scheduled {
		return productionReviewPending, nil
	}
	return w.recordReviewSourceFailure(ctx, task, id, round, baseSHA, headSHA, cause)
}

// reconstructReviewRetryDeadline restores a pending same-invocation retry that
// a restart dropped from the process-local map. The decoded row is a delay
// claim, never authority: a row bound to a superseded round, invocation, or
// candidate is stale and dropped, and the deadline is re-derived from the
// row's round, so the row can only postpone a retry, never authorize skipping
// backoff, changing the invocation, or advancing the round. It returns
// (true, nil) when a live deadline still binds the pass to pending.
func (w *productionPublicationWorkflow) reconstructReviewRetryDeadline(
	ctx context.Context, task productionPublicationTask, binding productionBinding, round int,
) (bool, error) {
	var retry domain.ReviewRetry
	found := false
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		got, err := tx.GetReviewRetry(ctx, task.RunID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		retry = got
		found = true
		return nil
	}); err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}
	if retry.Round != round ||
		retry.InvocationID != ProductionReviewInvocationID(task.RunID, round) ||
		retry.BaseSHA != binding.admission.Base.BaseSHA || retry.HeadSHA != task.HeadSHA {
		if err := w.store.Write(ctx, func(tx *store.WriteTx) error {
			return tx.DeleteReviewRetry(ctx, task.RunID)
		}); err != nil {
			return false, err
		}
		return false, nil
	}
	retryAt := retry.ObservedAt.Add(reviewRetryDelay(retry.Round))
	if w.now().Before(retryAt) {
		w.reviewRetryAfter[task.RunID] = retryAt
		return true, nil
	}
	// Deadline already passed: proceed now; the next transient overwrites the
	// row and any terminal outcome clears it.
	return false, nil
}

func (w *productionPublicationWorkflow) latestReviewState(
	ctx context.Context, runID domain.RunID,
) (*domain.ReviewRecord, *domain.ReviewFailure, error) {
	var record *domain.ReviewRecord
	var failure *domain.ReviewFailure
	err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		gotRecord, err := tx.LatestReviewRecord(ctx, runID)
		if err == nil {
			record = &gotRecord
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		gotFailure, err := tx.LatestReviewFailure(ctx, runID)
		if err == nil {
			failure = &gotFailure
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		return nil
	})
	return record, failure, err
}

func (w *productionPublicationWorkflow) reviewHardRoundLimit(
	ctx context.Context, runID domain.RunID,
) (int, error) {
	var policy store.ReviewConvergencePolicy
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		policy, err = tx.ReviewConvergencePolicy(ctx, runID)
		return err
	}); err != nil {
		return 0, err
	}
	return policy.HardRoundLimit, nil
}

func (w *productionPublicationWorkflow) recordReviewSourceFailure(
	ctx context.Context,
	task productionPublicationTask,
	id domain.InvocationID,
	round int,
	baseSHA, headSHA string,
	sourceErr error,
) (productionReviewGateState, error) {
	class := exec.ClassifyReviewSourceFailure(sourceErr)
	failure := domain.ReviewFailure{
		InvocationID: id, RunID: task.RunID, Round: round,
		BaseSHA: baseSHA, HeadSHA: headSHA, Class: class,
		Reason: sourceErr.Error(), ObservedAt: w.now().UTC(),
	}
	// The terminal outcome supersedes any pending same-invocation retry for
	// this run: clear the durable row atomically with the failure it records.
	if err := w.store.Write(ctx, func(tx *store.WriteTx) error {
		if _, err := tx.AppendUsageObservations(
			ctx, id, exec.ReviewSourceFailureUsage(sourceErr),
		); err != nil {
			return err
		}
		if err := tx.PutReviewFailure(ctx, failure); err != nil {
			return err
		}
		return tx.DeleteReviewRetry(ctx, task.RunID)
	}); err != nil {
		return productionReviewPending, err
	}
	if err := w.removeReviewWorkspace(id); err != nil {
		return productionReviewPending, err
	}
	switch class {
	case domain.ReviewFailureTransient:
		// A terminal transient advances the round; its deadline reconstructs
		// from the persisted ReviewFailure.ObservedAt, not the same-invocation
		// row (which the write above cleared).
		w.reviewRetryAfter[task.RunID] = w.now().Add(reviewRetryDelay(round))
		return productionReviewPending, nil
	case domain.ReviewFailureQuota:
		record := domain.ReviewRecord{
			InvocationID: failure.InvocationID, RunID: failure.RunID,
			Round: failure.Round, BaseSHA: failure.BaseSHA, HeadSHA: failure.HeadSHA,
		}
		if err := w.putReviewAttention(ctx, task, record,
			fmt.Sprintf("Review stopped because of a %s failure.", class),
			domain.AttentionReviewDispute,
		); err != nil {
			return productionReviewPending, err
		}
		return productionReviewEscalated, nil
	case domain.ReviewFailureConfiguration:
		// Parked, never terminalized (issue #611, revising issue #527
		// decision 3 for this class: the approved configuration cannot always
		// be restored, because the change may itself be a required safety
		// fix). The run stays tickable; the review gate raises the
		// recovery-bearing item with the trust binding in scope on the next
		// reconcile and holds the run until an operator decision.
		return productionReviewPending, nil
	case domain.ReviewFailureContradiction:
		if err := w.putReviewContradictionAttention(ctx, task, failure); err != nil {
			return productionReviewPending, err
		}
		return productionReviewPending, nil
	}
	return productionReviewPending, fmt.Errorf("review invocation %q has invalid failure class: %w",
		id, domain.ErrInvalidReviewFailureClass)
}

// reviewContradictionRecovered reports whether the latest command-backed
// transition for this run matches every coordinate and the exact persisted
// body digest of failure. An older recovery deliberately does not authorize a
// later contradiction.
func (w *productionPublicationWorkflow) reviewContradictionRecovered(
	ctx context.Context, failure domain.ReviewFailure,
) (bool, error) {
	var recovered bool
	err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		digest, err := tx.ReviewFailureBodyDigest(ctx, failure.InvocationID)
		if err != nil {
			return err
		}
		transition, found, err := tx.LatestReviewRecoveryTransition(ctx, failure.RunID)
		if err != nil {
			return err
		}
		recovered = found && transition.Binding().Matches(failure, digest)
		return nil
	})
	return recovered, err
}

// abortRecoveredReview reaps a recovered contradiction's invocation before the
// next round reaches the usage pool gate. The gate never inspects that
// invocation again, so only this abort, or a restart's recovery, writes the
// ward's proof that ends its slot. The source makes a repeat call a no-op once
// the proof exists. A transient failure keeps the slot and retries on a later
// pass; any other failure fails loud, as startup recovery does.
func (w *productionPublicationWorkflow) abortRecoveredReview(
	ctx context.Context, id domain.InvocationID,
) error {
	adapter, ok := w.reviewSource.(interface {
		AbortRecoveredReview(context.Context, domain.InvocationID) error
	})
	if !ok {
		return errors.New("review source has no recovered-review abort adapter")
	}
	err := adapter.AbortRecoveredReview(ctx, id)
	if err == nil || errors.Is(err, exec.ErrUnknownInvocation) {
		return nil // An unknown invocation was never journaled, so nothing runs.
	}
	err = fmt.Errorf("abort recovered review %q: %w", id, err)
	if exec.ClassifyReviewSourceFailure(err) == domain.ReviewFailureTransient {
		return productionPublicationRetryableError(err)
	}
	return err
}

// putReviewContradictionAttention exposes one persisted contradiction without
// terminalizing its publication task. The deterministic identity plus the
// preflight below keeps subsequent ticks from rewriting the same open item or
// consuming a new sync revision; any divergent row fails closed.
func (w *productionPublicationWorkflow) putReviewContradictionAttention(
	ctx context.Context, task productionPublicationTask, failure domain.ReviewFailure,
) error {
	var digest domain.Digest
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		digest, err = tx.ReviewFailureBodyDigest(ctx, failure.InvocationID)
		return err
	}); err != nil {
		return err
	}
	binding := domain.ReviewRecoveryBinding{
		RunID: failure.RunID, InvocationID: failure.InvocationID, Round: failure.Round,
		BaseSHA: failure.BaseSHA, HeadSHA: failure.HeadSHA, FailureDigest: digest,
	}
	runID := task.RunID
	createdAt := w.attentionCreatedAt()
	subject := domain.Subject{
		Type: domain.SubjectRun, ID: domain.SubjectID(task.RunID), RunID: &runID,
	}
	names, err := displayNames(ctx, w.store, task.ProjectID, subject)
	if err != nil {
		return err
	}
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: productionReviewItemID(task.RunID, failure.Round), ProjectID: task.ProjectID,
		Subject: subject,
		Type:    domain.AttentionReviewContradiction, Priority: domain.PriorityHigh,
		Reason:            fmt.Sprintf("Review contradicted its execution contract: %s", failure.Reason),
		RequestedDecision: []domain.Action{domain.ActionRecoverReview},
		PRHeadSHA:         failure.HeadSHA, ReviewRecoveryBinding: &binding,
		DisplayNames: names,
		ItemVersion:  1, InterruptionClass: domain.InterruptionExceptional,
		CreatedAt: &createdAt,
		Status:    domain.StatusOpen,
	}, w.operatorRecipes)
	if err != nil {
		return err
	}
	var existing *domain.AttentionItem
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		got, err := tx.GetAttentionItem(ctx, item.ID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		existing = &got
		return nil
	}); err != nil {
		return err
	}
	if existing != nil {
		item.CreatedAt = existing.CreatedAt
		if item.Subject.TaskID == nil {
			item.Subject.TaskID = existing.Subject.TaskID
		}
		// Delivery receipts legitimately advance the item's derived timing
		// while it remains parked, and before #1946 advanced its version too.
		// Ignore only those two mutable telemetry fields; every identity,
		// presentation, action, and recovery coordinate must still match the
		// persisted contradiction exactly.
		comparable := *existing
		comparable.ItemVersion = item.ItemVersion
		comparable.Timing = item.Timing
		comparable.Recommendation = item.Recommendation
		comparable.DecisionSurface = item.DecisionSurface
		if comparable.Reason == legacyCodexReviewReason(item.Reason) {
			comparable.Reason = item.Reason
		}
		if !reflect.DeepEqual(comparable, item) {
			return fmt.Errorf("review contradiction item %q diverges from failure %q: %w",
				item.ID, failure.InvocationID, domain.ErrReviewRecoveryBindingMismatch)
		}
		return nil
	}
	return w.attention.PutItem(ctx, item)
}

// reviewConfigurationAdoption returns the run's operator-authorized profile
// supersession when one is effective right now. Every trust fact is
// re-derived on this read: the transition itself re-gates its command, item
// binding, immutable failure row, latest-profile status, and
// review-configuration-only delta inside the store; this helper adds the run
// binding (the superseded revision must be the run's admission-pinned
// profile) and the effective-configuration equality (the adopted revision
// must approve the digest this daemon actually computes). A decoded or stale
// transition therefore never carries authority the operator did not grant to
// this exact run under this exact configuration.
func (w *productionPublicationWorkflow) reviewConfigurationAdoption(
	ctx context.Context, binding productionBinding,
) (domain.ReviewConfigurationRecoveryTransition, bool, error) {
	var (
		transition domain.ReviewConfigurationRecoveryTransition
		found      bool
	)
	err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		transition, found, err = tx.LatestReviewConfigurationRecoveryTransition(ctx, binding.run.ID)
		if err != nil {
			// An ineffective row (the adopted revision was itself superseded,
			// or the delta widened) and a tampered or unbacked row all grant
			// nothing. The store classifies every determinate integrity or
			// policy rejection under one sentinel; treat it as no adoption so
			// the run stays visibly parked behind its item instead of
			// error-looping the lane, while any other failure is environmental
			// and propagates for retry.
			if errors.Is(err, domain.ErrReviewConfigRecoveryIneffective) {
				found = false
				return nil
			}
			return err
		}
		if !found {
			return nil
		}
		if transition.SupersededProfileDigest != binding.profile.ProfileDigest ||
			transition.Repo != binding.admission.Base.Repo ||
			transition.RepositoryID != binding.admission.Base.RepositoryID {
			found = false
			return nil
		}
		superseding, err := tx.GetTrustProfile(ctx, transition.SupersedingProfileDigest)
		if err != nil {
			return err
		}
		if superseding.Review.Mode != domain.ReviewFreesideInvoked ||
			superseding.Review.ConfigDigest != w.reviewConfigurationDigest {
			found = false
		}
		return nil
	})
	if err != nil || !found {
		return domain.ReviewConfigurationRecoveryTransition{}, false, err
	}
	return transition, true, nil
}

func reviewConfigurationUnapprovedError(pinned, effective domain.Digest) error {
	return fmt.Errorf(
		"profile pins %s, daemon effective is %s: %w",
		pinned, effective, domain.ErrReviewConfigurationUnapproved,
	)
}

// reviewConfigurationApproved reports whether the run's trust context
// approves the daemon's effective reviewer configuration: directly through
// its admission-pinned profile, or through an effective operator-authorized
// review-configuration-only supersession.
func (w *productionPublicationWorkflow) reviewConfigurationApproved(
	ctx context.Context, binding productionBinding,
) (bool, error) {
	if binding.profile.Review.ConfigDigest == w.reviewConfigurationDigest {
		return true, nil
	}
	_, adopted, err := w.reviewConfigurationAdoption(ctx, binding)
	return adopted, err
}

// reviewConfigurationRecovered reports whether the run's effective adoption
// also authorizes advancing past exactly this parked failure row: the
// transition must match every coordinate and the exact persisted body
// digest, so an older recovery never authorizes a later failure.
func (w *productionPublicationWorkflow) reviewConfigurationRecovered(
	ctx context.Context, binding productionBinding, failure domain.ReviewFailure,
) (bool, error) {
	transition, adopted, err := w.reviewConfigurationAdoption(ctx, binding)
	if err != nil || !adopted {
		return false, err
	}
	var digest domain.Digest
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		digest, err = tx.ReviewFailureBodyDigest(ctx, failure.InvocationID)
		return err
	}); err != nil {
		return false, err
	}
	return transition.Binding().Matches(failure, digest), nil
}

// parkReviewConfiguration holds one run on a configuration-class review
// failure without terminalizing its publication task (issue #611, revising
// issue #527 decision 3 for this class). The run stays tickable: it resumes
// when an operator adopts a review-configuration-only profile supersession
// that approves the effective configuration, and it terminalizes only when
// the operator concludes the displayed item without an effective recovery
// (a stop, or an adoption whose target has since been superseded).
func (w *productionPublicationWorkflow) parkReviewConfiguration(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
	failure domain.ReviewFailure,
) (productionReviewGateState, error) {
	if binding.admission.TrustProfileDigest == nil {
		// Recovery rebinds an admission-pinned revision; a legacy admission
		// without the pin resolves its profile as the repository's latest on
		// every load, so the recovery binding would drift under the parked
		// item. Keep #527 decision 3's terminalizing dispute for that shape.
		record := domain.ReviewRecord{
			InvocationID: failure.InvocationID, RunID: failure.RunID,
			Round: failure.Round, BaseSHA: failure.BaseSHA, HeadSHA: failure.HeadSHA,
		}
		if err := w.putReviewAttention(ctx, task, record,
			"Review stopped because of a configuration failure.",
			domain.AttentionReviewDispute,
		); err != nil {
			return productionReviewPending, err
		}
		return productionReviewEscalated, nil
	}
	itemID := productionReviewItemID(task.RunID, failure.Round)
	var existing *domain.AttentionItem
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		got, err := tx.GetAttentionItem(ctx, itemID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		existing = &got
		return nil
	}); err != nil {
		return productionReviewPending, err
	}
	if existing != nil && existing.Type == domain.AttentionReviewDispute {
		// A pre-#611 daemon raised the terminalizing dispute at this identity
		// and crashed before completing the task. Honor the contract that
		// item presented rather than failing closed against its shape.
		return productionReviewEscalated, nil
	}
	if existing != nil && existing.Type == domain.AttentionReviewConfiguration &&
		existing.Status != domain.StatusOpen {
		// Presence, not effectiveness, separates the operator's two exits: a
		// conclusion that appended an adoption keeps the run parked (the
		// signet transaction may have landed between this tick's transition
		// and item reads, or the adoption may not be effective yet), while a
		// conclusion without one is the explicit decline that ends the run
		// exactly as #527 decision 3 always did for this class.
		var digest domain.Digest
		var adopted bool
		if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			digest, err = tx.ReviewFailureBodyDigest(ctx, failure.InvocationID)
			if err != nil {
				return err
			}
			adopted, err = tx.HasReviewConfigurationRecoveryTransition(
				ctx, failure.RunID, failure.InvocationID, digest)
			return err
		}); err != nil {
			return productionReviewPending, err
		}
		if adopted {
			return productionReviewPending, nil
		}
		return productionReviewEscalated, nil
	}
	if err := w.putReviewConfigurationAttention(ctx, task, binding, failure); err != nil {
		return productionReviewPending, err
	}
	return productionReviewPending, nil
}

// putReviewConfigurationAttention exposes one parked configuration failure
// without terminalizing its publication task. The deterministic identity plus
// the preflight below keeps subsequent ticks from rewriting the same open
// item or consuming a new sync revision; any divergent row fails closed.
func (w *productionPublicationWorkflow) putReviewConfigurationAttention(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
	failure domain.ReviewFailure,
) error {
	var digest domain.Digest
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		digest, err = tx.ReviewFailureBodyDigest(ctx, failure.InvocationID)
		return err
	}); err != nil {
		return err
	}
	recovery := domain.ReviewConfigurationRecoveryBinding{
		RunID: failure.RunID, InvocationID: failure.InvocationID, Round: failure.Round,
		BaseSHA: failure.BaseSHA, HeadSHA: failure.HeadSHA, FailureDigest: digest,
		Repo: binding.admission.Base.Repo, RepositoryID: binding.admission.Base.RepositoryID,
		SupersededProfileDigest: binding.profile.ProfileDigest,
	}
	runID := task.RunID
	createdAt := w.attentionCreatedAt()
	subject := domain.Subject{
		Type: domain.SubjectRun, ID: domain.SubjectID(task.RunID), RunID: &runID,
	}
	names, err := displayNames(ctx, w.store, task.ProjectID, subject)
	if err != nil {
		return err
	}
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: productionReviewItemID(task.RunID, failure.Round), ProjectID: task.ProjectID,
		Subject: subject,
		Type:    domain.AttentionReviewConfiguration, Priority: domain.PriorityHigh,
		Reason: fmt.Sprintf(
			"Review parked on a reviewer configuration the trust profile no longer approves: %s",
			failure.Reason),
		RequestedDecision: []domain.Action{
			domain.ActionAdoptReviewConfiguration, domain.ActionDiscuss, domain.ActionStop,
		},
		PRHeadSHA: failure.HeadSHA, ReviewConfigurationRecovery: &recovery,
		DisplayNames: names,
		ItemVersion:  1, InterruptionClass: domain.InterruptionPlannedGate,
		CreatedAt: &createdAt,
		Status:    domain.StatusOpen,
	}, w.operatorRecipes)
	if err != nil {
		return err
	}
	var existing *domain.AttentionItem
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		got, err := tx.GetAttentionItem(ctx, item.ID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		existing = &got
		return nil
	}); err != nil {
		return err
	}
	if existing != nil {
		item.CreatedAt = existing.CreatedAt
		if item.Subject.TaskID == nil {
			item.Subject.TaskID = existing.Subject.TaskID
		}
		// Delivery receipts advance the item's derived timing, and a discuss
		// decision advances its version and attaches its conversation, all
		// while the item remains parked. Ignore only those mutable fields;
		// every identity, presentation, action, and recovery coordinate must
		// still match the persisted failure exactly.
		comparable := *existing
		comparable.ItemVersion = item.ItemVersion
		comparable.Timing = item.Timing
		comparable.ConversationID = item.ConversationID
		comparable.Recommendation = item.Recommendation
		comparable.DecisionSurface = item.DecisionSurface
		if comparable.Reason == legacyCodexReviewReason(item.Reason) {
			comparable.Reason = item.Reason
		}
		if !reflect.DeepEqual(comparable, item) {
			return fmt.Errorf("review configuration item %q diverges from failure %q: %w",
				item.ID, failure.InvocationID, domain.ErrReviewConfigRecoveryBindingMismatch)
		}
		return nil
	}
	return w.attention.PutItem(ctx, item)
}

func (w *productionPublicationWorkflow) putReviewAttention(
	ctx context.Context,
	task productionPublicationTask,
	record domain.ReviewRecord,
	reason string,
	itemType domain.AttentionType,
) error {
	reason, err := w.withExternalReviewFindings(ctx, task, reason)
	if err != nil {
		return err
	}
	actions := []domain.Action{domain.ActionDiscuss, domain.ActionStop}
	if record.Outcome == domain.ReviewFindings && !task.externalReviewFirstRound(record) {
		actions = append([]domain.Action{domain.ActionAdjudicateFindings}, actions...)
	}
	return w.putReviewAttentionWithActionsAndID(
		ctx, task, record, reason, itemType,
		productionReviewItemID(task.RunID, record.Round),
		actions, nil,
	)
}

func (w *productionPublicationWorkflow) putReviewAttentionWithID(
	ctx context.Context,
	task productionPublicationTask,
	record domain.ReviewRecord,
	reason string,
	itemType domain.AttentionType,
	itemID domain.ItemID,
) error {
	reason, err := w.withExternalReviewFindings(ctx, task, reason)
	if err != nil {
		return err
	}
	return w.putReviewAttentionWithActionsAndID(
		ctx, task, record, reason, itemType, itemID,
		[]domain.Action{domain.ActionDiscuss, domain.ActionStop},
		nil,
	)
}

func (w *productionPublicationWorkflow) putReviewAttentionWithActionsAndID(
	ctx context.Context,
	task productionPublicationTask,
	record domain.ReviewRecord,
	reason string,
	itemType domain.AttentionType,
	itemID domain.ItemID,
	disputeActions []domain.Action,
	agentClaims []domain.AgentClaim,
) error {
	return w.putReviewAttentionWithListedFindings(ctx, task, record, reason, itemType,
		itemID, disputeActions, agentClaims, nil)
}

func (w *productionPublicationWorkflow) putReviewAttentionWithListedFindings(
	ctx context.Context, task productionPublicationTask, record domain.ReviewRecord,
	reason string, itemType domain.AttentionType, itemID domain.ItemID,
	disputeActions []domain.Action, agentClaims []domain.AgentClaim,
	listedFindings []domain.FindingID,
) error {
	runID := task.RunID
	subject := domain.Subject{
		Type: domain.SubjectRun, ID: domain.SubjectID(task.RunID), RunID: &runID,
	}
	names, err := displayNames(ctx, w.store, task.ProjectID, subject)
	if err != nil {
		return err
	}
	actions := []domain.Action{domain.ActionFinishNow}
	if itemType == domain.AttentionReviewDispute {
		actions = disputeActions
	}
	var cost *domain.CostSoFar
	if itemType == domain.AttentionReviewDiminishing {
		cost, err = billableCostSoFar(ctx, w.store, task.RunID)
		if err != nil {
			return err
		}
	}
	var reviewBinding *domain.ReviewDisputeBinding
	if itemType == domain.AttentionReviewDispute && slices.Contains(disputeActions, domain.ActionAdjudicateFindings) {
		reviewBinding, err = w.routedReviewDisputeBinding(ctx, task, record, listedFindings)
		if err != nil {
			return err
		}
	}
	if strings.HasPrefix(string(itemID), domain.PublicationContinuationItemPrefix) {
		reviewBinding = &domain.ReviewDisputeBinding{RunID: record.RunID, Round: record.Round, FindingIDs: slices.Clone(record.FindingIDs), CompletionEvidence: record.CompletionEvidence}
	}
	// The first item written at this round's deterministic identity durably
	// binds its routing decision. Classification may recover differently on a
	// later reconciliation, but changing the item's type in place is forbidden
	// and would otherwise strand terminalization behind an immutable-transition
	// error. Reuse the bound decision after verifying its run coordinates.
	var existing *domain.AttentionItem
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		got, err := tx.GetAttentionItem(ctx, itemID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		existing = &got
		return nil
	}); err != nil {
		return err
	}
	if existing != nil {
		validType := existing.Type == domain.AttentionReviewDiminishing ||
			existing.Type == domain.AttentionReviewDispute
		validSubject := existing.Subject.Type == domain.SubjectRun &&
			existing.Subject.ID == domain.SubjectID(task.RunID) &&
			existing.Subject.RunID != nil && *existing.Subject.RunID == task.RunID
		if !validType || existing.ProjectID != task.ProjectID || !validSubject ||
			existing.PRHeadSHA != task.HeadSHA {
			return fmt.Errorf("review attention item %q disagrees with run %q: %w",
				itemID, task.RunID, domain.ErrParentKeyMismatch)
		}
		if existing.Type != domain.AttentionReviewDispute {
			return nil
		}
		if existing.Status == domain.StatusOpen && reviewBinding == nil &&
			slices.Contains(disputeActions, domain.ActionAdjudicateFindings) {
			reviewBinding, err = w.routedReviewDisputeBinding(ctx, task, record, listedFindings)
			if err != nil {
				return err
			}
		}
		if agentClaims == nil {
			if existing.Status == domain.StatusOpen &&
				(!slices.Equal(existing.RequestedDecision, disputeActions) ||
					(reviewBinding != nil && !domain.ReviewDisputeBindingMatches(existing.ReviewDispute, reviewBinding))) {
				repaired := *existing
				repaired.RequestedDecision = disputeActions
				if reviewBinding != nil {
					repaired.ReviewDispute = reviewBinding
				}
				repaired.ItemVersion++
				return w.attention.PutItem(ctx, repaired)
			}
			return nil
		}
		createdAt := w.attentionCreatedAt()
		if existing.CreatedAt != nil {
			createdAt = *existing.CreatedAt
		}
		desired, err := domain.NewAttentionItem(domain.AttentionItemInput{
			ID: itemID, ProjectID: task.ProjectID,
			Subject: subject,
			Type:    itemType, Priority: domain.PriorityNormal, Reason: reason,
			RequestedDecision: actions, AgentClaims: agentClaims,
			ReviewDispute: reviewBinding,
			PRHeadSHA:     task.HeadSHA, ItemVersion: existing.ItemVersion + 1,
			InterruptionClass: domain.InterruptionPlannedGate, Status: domain.StatusOpen,
			CreatedAt: &createdAt, DisplayNames: names,
		}, w.operatorRecipes)
		if err != nil {
			return err
		}
		claimsMatch := reflect.DeepEqual(existing.AgentClaims, desired.AgentClaims)
		digestsMatch := slices.Equal(existing.ArtifactDigests, desired.ArtifactDigests)
		presentationMatches := slices.Equal(existing.RequestedDecision, desired.RequestedDecision) &&
			existing.Reason == desired.Reason && claimsMatch && digestsMatch && domain.ReviewDisputeBindingMatches(existing.ReviewDispute, desired.ReviewDispute)
		if !presentationMatches && existing.Status != domain.StatusOpen {
			return fmt.Errorf("review attention item %q presentation disagrees with run %q: %w",
				itemID, task.RunID, domain.ErrParentKeyMismatch)
		}
		// Reconcile an open dispute's context-appropriate controls and bound
		// claims. The routing decision and recovery bindings stay fixed; the
		// rendered decision surface advances together under one new version.
		executableDisputeActions := disputeActions
		if existing.Status == domain.StatusOpen && !presentationMatches {
			repaired := *existing
			repaired.RequestedDecision = executableDisputeActions
			repaired.Reason = desired.Reason
			repaired.AgentClaims = desired.AgentClaims
			repaired.ReviewDispute = desired.ReviewDispute
			repaired.ArtifactDigests = desired.ArtifactDigests
			repaired.ItemVersion++
			return w.attention.PutItem(ctx, repaired)
		}
		return nil
	}
	createdAt := w.attentionCreatedAt()
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: itemID, ProjectID: task.ProjectID,
		Subject: subject,
		Type:    itemType, Priority: domain.PriorityNormal, Reason: reason,
		RequestedDecision: actions, AgentClaims: agentClaims, ReviewDispute: reviewBinding,
		PRHeadSHA: task.HeadSHA, ItemVersion: 1,
		InterruptionClass: domain.InterruptionPlannedGate, Status: domain.StatusOpen,
		CreatedAt: &createdAt, BillableCostSoFar: cost, DisplayNames: names,
	}, w.operatorRecipes)
	if err != nil {
		return err
	}
	return w.attention.PutItem(ctx, item)
}

// completeReviewEscalationTask terminalizes a run whose §7 review escalated
// before publication: no branch push, no PR, no publication intent. The durable
// AttentionItem written by the gate (PRHeadSHA = task.HeadSHA) is the operator
// surface; the terminal outcome carries no PR number (issue #527, decision 3).
func (w *productionPublicationWorkflow) completeReviewEscalationTask(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
) (productionTaskOutcome, error) {
	accepted, err := w.recordCompletedTerminalAtBoundary(
		ctx, binding.run, task, signet.PublicationReevaluationReviewEscalated)
	if err != nil {
		return productionTaskOutcome{}, err
	}
	if err := w.finishTask(ctx, task); err != nil {
		return productionTaskOutcome{}, err
	}
	return productionTaskOutcome{
		completed: true, accepted: accepted, blocked: true,
	}, nil
}

// storedReviewAdmission reads the snapshot an invocation was admitted under.
// found is false for an invocation requested before review admission was
// recorded. It never admits: a caller that needs a snapshot for a new
// invocation uses ensureReviewAdmission.
func (w *productionPublicationWorkflow) storedReviewAdmission(
	ctx context.Context, role domain.RoleName, runID domain.RunID, id domain.InvocationID,
) (admission domain.ReviewAdmission, found bool, err error) {
	err = w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		admission, err = tx.GetReviewAdmission(ctx, id)
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return domain.ReviewAdmission{}, false, nil
	}
	if err != nil {
		return domain.ReviewAdmission{}, false, err
	}
	if admission.RunID != runID || admission.Role != role {
		return domain.ReviewAdmission{}, false, fmt.Errorf(
			"review admission %q is bound to run %q as %s: %w",
			id, admission.RunID, admission.Role, domain.ErrParentKeyMismatch)
	}
	return admission, true, nil
}

// ensureReviewAdmission returns the one snapshot the invocation is admitted
// under (plan §5.4 step 5). The first pass over an invocation resolves it and
// stores it; every later pass reads the stored row, so a restart, a retry, or
// a lineup change never replaces it. A refusal is a configuration failure of
// the invocation, returned as a *exec.ReviewSourceFailure, and stores nothing.
//
// The admission takes a slot on its identity's usage pool and, under a budget,
// reserves the review's memory, so both gates run in the transaction that
// writes the row. A full pool or a reservation that does not fit yet stores
// nothing and returns ErrUsagePoolExhausted or ErrMachineCapacity, for the
// caller to wait on, and so does a routed reviewer whose pool's window is
// marked, with ErrQuotaWindowExhausted, and any admission while an update
// drain is in force, with store.ErrUpdateDrainInForce; writing the admission
// ends the wait, and with it the hold the wait recorded and that hold's pace.
//
// The drain refuses before the row is written, never after: a written
// admission holds a review slot until its review launches and ends, so one
// the drain kept from launching would keep a restart from ever being ready
// (issue #3104). A stored admission returns above and launches, drain or not.
func (w *productionPublicationWorkflow) ensureReviewAdmission(
	ctx context.Context, role domain.RoleName, runID domain.RunID, id domain.InvocationID,
) (domain.ReviewAdmission, error) {
	stored, found, err := w.storedReviewAdmission(ctx, role, runID, id)
	if err != nil || found {
		return stored, err
	}
	if w.reviewAdmitter == nil {
		return domain.ReviewAdmission{}, errors.New("review admission is not configured")
	}
	admission, err := w.reviewAdmitter(ctx, role, runID, id, w.now().UTC())
	if errors.Is(err, ErrQuotaWindowExhausted) {
		return domain.ReviewAdmission{}, err
	}
	if err != nil {
		return domain.ReviewAdmission{}, &exec.ReviewSourceFailure{
			Class: domain.ReviewFailureConfiguration, Err: err,
		}
	}
	// The resolver is a configured boundary: hold what it returned to the
	// invocation it was asked about before the row makes it permanent.
	if admission, err = domain.NewReviewAdmission(admission); err != nil {
		return domain.ReviewAdmission{}, err
	}
	if admission.InvocationID != id || admission.RunID != runID || admission.Role != role {
		return domain.ReviewAdmission{}, fmt.Errorf(
			"review admission for %q resolved %q of run %q as %s: %w",
			id, admission.InvocationID, admission.RunID, admission.Role, domain.ErrParentKeyMismatch)
	}
	var waited bool
	if err := w.store.Write(ctx, func(tx *store.WriteTx) error {
		if err := requireTaskExecutionOpen(ctx, &tx.ReadTx, runID); err != nil {
			return err
		}
		if err := tx.RequireNoUpdateDrain(ctx); err != nil {
			return err
		}
		if err := tx.RequireReviewUsagePoolCapacity(ctx, admission); err != nil {
			return err
		}
		bound, err := launchBoundAt(ctx, &tx.ReadTx, w.launchBound)
		if err != nil {
			return err
		}
		if reservation, reserve, err := reviewReservation(bound, runID, id); err != nil {
			return err
		} else if reserve {
			if err := tx.RequireMachineCapacity(ctx, bound.BudgetMiB, reservation, w.now().UTC()); err != nil {
				return err
			}
		}
		if err := tx.PutReviewAdmission(ctx, admission); err != nil {
			return err
		}
		hold, present, err := tx.GetRunHold(ctx, runID)
		if err != nil {
			return err
		}
		waited = present && reviewCapacityWait(hold.Reason)
		for _, cause := range []domain.RunHoldReason{
			domain.HoldUsagePoolExhausted, domain.HoldMachineCapacity, domain.HoldQuotaWindowExhausted,
			domain.HoldUpdateDrain,
		} {
			if err := tx.ClearRunHoldCause(ctx, runID, cause); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return domain.ReviewAdmission{}, err
	}
	// The key also paces the attended clear, so only an ended wait resets it.
	if waited {
		w.holdPace.forget("hold:" + string(runID))
	}
	return admission, nil
}

// reviewCapacityWait reports whether a hold reason is a review admission's
// capacity wait: a full pool, memory that does not fit yet, a routed
// reviewer's marked quota window, or an update drain, which admits nothing.
func reviewCapacityWait(reason domain.RunHoldReason) bool {
	return reason == domain.HoldUsagePoolExhausted || reason == domain.HoldMachineCapacity ||
		reason == domain.HoldQuotaWindowExhausted || reason == domain.HoldUpdateDrain
}

// reviewCapacityRefusal reports whether a review admission refusal is a wait
// for capacity rather than a failure.
func reviewCapacityRefusal(err error) bool {
	return errors.Is(err, domain.ErrUsagePoolExhausted) || errors.Is(err, domain.ErrMachineCapacity) ||
		errors.Is(err, ErrQuotaWindowExhausted) || errors.Is(err, store.ErrUpdateDrainInForce)
}

// holdReviewForCapacity records the wait of a review refused for a full pool,
// for memory, for a routed reviewer's marked window, or for an update drain.
// cause is that refusal, which names the pool, the memory arithmetic, or the
// window; a memory refusal also places the review in the memory order.
func (w *productionPublicationWorkflow) holdReviewForCapacity(
	ctx context.Context, runID domain.RunID, id domain.InvocationID, cause error,
) error {
	reason, ok := dispatchHoldReason(cause)
	if !ok || !reviewCapacityWait(reason) {
		return fmt.Errorf("review %q capacity hold: %w", id, cause)
	}
	detail := holdDetailFor(cause)
	now := w.now().UTC()
	if err := recordLaunchOrder(ctx, w.store, w.attention, runID, id, cause, now); err != nil {
		return err
	}
	key := "hold:" + string(runID)
	if !w.holdPace.due(key, holdPaceState(reason, detail), now) {
		return nil
	}
	if err := w.store.Write(ctx, func(tx *store.WriteTx) error {
		return recordRunHold(ctx, tx, runID, id, reason, detail, now)
	}); err != nil {
		w.holdPace.forget(key)
		return err
	}
	return nil
}

// recordReviewRequest preserves the first attempt instant. RequestReview may
// fail or be replayed after a crash; neither changes the request's binding.
func (w *productionPublicationWorkflow) recordReviewRequest(ctx context.Context, id domain.InvocationID, req exec.ReviewRequest) error {
	var prior domain.ReviewRequestRecord
	err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		if err := requireTaskExecutionOpen(ctx, tx, req.RunID); err != nil {
			return err
		}
		var err error
		prior, err = tx.GetReviewRequest(ctx, id)
		return err
	})
	if err == nil {
		if prior.RunID != req.RunID || prior.Round != req.Round || prior.BaseSHA != req.BaseSHA || prior.HeadSHA != req.HeadSHA {
			return domain.ErrParentKeyMismatch
		}
		return nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	// Before migration 67, an in-flight Codex request has only its provider
	// journal. Codex persists that request before launch; absence therefore
	// identifies a new request, while a damaged row must fail closed.
	err = w.store.Read(ctx, func(tx *store.ReadTx) error {
		row, err := tx.GetCodexReviewRequest(ctx, string(id))
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		binding := domain.ReviewRequestRecord{InvocationID: id, RunID: req.RunID, Round: req.Round, BaseSHA: req.BaseSHA, HeadSHA: req.HeadSHA}
		original, err := ward.DecodeRetainedCodexReviewRequestFacts(binding, row.Body, row.BodyDigest)
		if err != nil {
			return err
		}
		req.RequestedAt = original.RequestedAt
		return nil
	})
	if err != nil {
		return err
	}
	return w.store.Write(ctx, func(tx *store.WriteTx) error {
		if err := requireTaskExecutionOpen(ctx, &tx.ReadTx, req.RunID); err != nil {
			return err
		}
		return tx.PutReviewRequest(ctx, domain.ReviewRequestRecord{
			InvocationID: id, RunID: req.RunID, Round: req.Round, BaseSHA: req.BaseSHA, HeadSHA: req.HeadSHA, RequestedAt: req.RequestedAt,
		})
	})
}

// ReviewSource reports status, not the stage driver's separate Live evidence.
// Preserve that distinction while using the same paced observation vocabulary.
func (w *productionPublicationWorkflow) observeReviewInvocation(ctx context.Context, runID domain.RunID, id domain.InvocationID, status exec.Status) error {
	observed, ok := observedStatus(status)
	if !ok {
		return nil
	}
	now := w.now().UTC()
	key := "review:" + string(id)
	if !w.holdPace.due(key, string(observed), now) {
		return nil
	}
	if err := w.store.Write(ctx, func(tx *store.WriteTx) error {
		return tx.RecordInvocationObservation(ctx, domain.InvocationObservation{
			InvocationID: id, RunID: runID, Status: observed, Live: false, ObservedAt: now,
		})
	}); err != nil {
		w.holdPace.forget(key)
		return err
	}
	return nil
}

// legacyCodexReviewReason is a review item's reason in the wording an
// earlier daemon wrote, "Codex review ..." where the harness-neutral
// wording reads "Review ..." (#397). A parked contradiction or configuration
// item is re-put on every tick while it stays open, so its comparison
// accepts the earlier wording rather than failing every reconcile after an
// upgrade.
func legacyCodexReviewReason(reason string) string {
	return "Codex review" + strings.TrimPrefix(reason, "Review")
}

// persistedReviewAuthority is the authority a finished review's persisted
// request is re-gated against. A request persisted before reviewers received
// the task's attachments carries none, and the source recognizes it as
// superseded by req; its review stands under the authority it was persisted
// with, req without inputs, as a findings record's adjudication already lets
// it. Rerunning it instead would leave a round behind a remediation's
// review, which reconcileRemediationReview refuses, or push a clean final
// round past the hard limit. An unknown invocation, or one the source does
// not supersede, keeps req's authority, and the re-gate decides.
func (w *productionPublicationWorkflow) persistedReviewAuthority(
	ctx context.Context, id domain.InvocationID, req exec.ReviewRequest, authority domain.Digest,
) (domain.Digest, error) {
	verifier, ok := w.reviewSource.(exec.ReviewRequestSupersessionVerifier)
	if !ok || len(req.Inputs) == 0 {
		return authority, nil
	}
	err := verifier.VerifyReviewRequestSupersession(ctx, id, req)
	switch {
	case err == nil, errors.Is(err, exec.ErrUnknownInvocation):
		return authority, nil
	case errors.Is(err, exec.ErrSupersededReviewRequest):
		persisted := req
		persisted.Inputs = nil
		return persisted.AuthorityDigest()
	default:
		return "", err
	}
}
