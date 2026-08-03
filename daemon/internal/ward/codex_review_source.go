package ward

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
)

var ErrCodexReviewOutcomeNotFound = errors.New("codex review outcome not found")

const codexProductionReviewPromptVersion = "codex-production-review-prompt-v1"

type CodexReviewSourceConfig struct {
	Backend             *Backend
	Review              CodexReviewConfig
	Journal             CodexReviewJournal
	WorkspaceSizeMB     int64
	AuthMode            CodexAuthMode
	AuthIdentityID      domain.AuthIdentityID
	AuthSnapshot        string
	Instructions        VendorInstructions
	InstructionFile     string
	ConfigurationDigest domain.Digest
	CostOwner           string
	Now                 func() time.Time
}

// CodexReviewSourceOutcome is durably collected before topology cleanup. The
// journal's separate ready bit proves cleanup finished before Poll can expose
// either a pass or a failure to the workflow.
type CodexReviewSourceOutcome struct {
	InvocationID       domain.InvocationID       `json:"invocation_id"`
	Result             *exec.ReviewResult        `json:"result"`
	CollectionEvidence domain.Digest             `json:"collection_evidence,omitempty"`
	FailureClass       domain.ReviewFailureClass `json:"failure_class,omitempty"`
	Failure            string                    `json:"failure,omitempty"`
	AbortRequired      bool                      `json:"abort_required,omitempty"`
}

// Validate rejects an outcome whose result and failure representations overlap
// or whose terminal payload is malformed.
func (o CodexReviewSourceOutcome) Validate() error {
	if o.InvocationID == "" {
		return domain.ErrEmptyID
	}
	if o.Result != nil {
		if o.Result.InvocationID != o.InvocationID {
			return domain.ErrParentKeyMismatch
		}
		if o.FailureClass != "" || o.Failure != "" || o.AbortRequired {
			return domain.ErrParentKeyMismatch
		}
		if err := o.Result.Validate(); err != nil || !contentaddr.Valid(string(o.CollectionEvidence)) {
			return errors.Join(err, domain.ErrInvalidReviewCompletionEvidence)
		}
		evidence, err := CodexReviewResultEvidence(*o.Result, o.CollectionEvidence)
		if err != nil || evidence != o.Result.CompletionEvidence {
			return errors.Join(err, domain.ErrInvalidReviewCompletionEvidence)
		}
		return nil
	}
	if o.FailureClass == "" || o.Failure == "" {
		return domain.ErrParentKeyMismatch
	}
	validClass := false
	for _, class := range domain.AllReviewFailureClasses {
		validClass = validClass || class == o.FailureClass
	}
	if !validClass {
		return domain.ErrInvalidReviewFailureClass
	}
	return nil
}

// CodexReviewSource adapts ward's started-container handoff into the durable
// ReviewSource contract.
type CodexReviewSource struct {
	cfg      CodexReviewSourceConfig
	mu       sync.Mutex
	launches map[domain.InvocationID]*CodexReviewLaunch
}

func NewCodexReviewSource(cfg CodexReviewSourceConfig) (*CodexReviewSource, error) {
	if cfg.Backend == nil || cfg.Journal == nil || cfg.Review.Journal != cfg.Journal ||
		cfg.Review.VolumeLifecycleLeaser == nil || cfg.WorkspaceSizeMB <= 0 ||
		cfg.AuthIdentityID == "" || cfg.AuthSnapshot == "" || cfg.InstructionFile == "" ||
		!contentaddr.Valid(string(cfg.ConfigurationDigest)) || cfg.CostOwner == "" {
		return nil, errors.New("codex review source: incomplete configuration")
	}
	digest, err := CodexReviewConfigurationDigest(
		cfg.Review, cfg.WorkspaceSizeMB, cfg.AuthMode, cfg.AuthIdentityID,
		cfg.Instructions.Digest, cfg.CostOwner,
	)
	if err != nil || digest != cfg.ConfigurationDigest {
		return nil, fmt.Errorf("codex review source: configuration digest mismatch: %w",
			errors.Join(err, ErrInvalidCodexReviewSpec))
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &CodexReviewSource{cfg: cfg, launches: make(map[domain.InvocationID]*CodexReviewLaunch)}, nil
}

func (s *CodexReviewSource) RequestReview(
	ctx context.Context, id domain.InvocationID, req exec.ReviewRequest,
) error {
	if err := req.Validate(); err != nil || !runIDPattern.MatchString(string(id)) {
		return &exec.ReviewSourceFailure{
			Class: domain.ReviewFailureConfiguration,
			Err:   errors.Join(err, ErrInvalidCodexReviewSpec),
		}
	}
	if _, err := s.cfg.Journal.GetCodexReviewRequest(ctx, string(id)); err == nil {
		return fmt.Errorf("codex review request %s: %w", id, exec.ErrDuplicateStart)
	} else if !errors.Is(err, exec.ErrUnknownInvocation) {
		if errors.Is(err, ErrCodexReviewRequestRejected) {
			return s.rejectPersistedRequest(ctx, id, err)
		}
		return &exec.ReviewSourceFailure{Class: domain.ReviewFailureTransient, Err: err}
	}
	if err := s.cfg.Journal.PutCodexReviewRequest(ctx, string(id), req); err != nil {
		return &exec.ReviewSourceFailure{Class: classifyCodexObservationFailure(err), Err: err}
	}
	if err := s.startRequestedReview(ctx, id, req); err != nil {
		return err
	}
	return nil
}

func (s *CodexReviewSource) startRequestedReview(
	ctx context.Context, id domain.InvocationID, req exec.ReviewRequest,
) error {
	candidate := domain.BaseRevision{
		Repo: req.Repo, RepositoryID: req.RepositoryID, BaseRef: req.BaseRef, BaseSHA: req.HeadSHA,
	}
	workspace, err := s.cfg.Journal.GetCodexReviewWorkspaceBinding(ctx, string(id))
	if errors.Is(err, ErrCodexReviewWorkspaceNotFound) ||
		(err == nil && workspace.CreationFingerprint == "") {
		workspace, err = s.cfg.Backend.PrepareCodexReviewWorkspace(
			ctx, s.cfg.Journal, string(id), req.Workspace, candidate, s.cfg.WorkspaceSizeMB,
		)
	}
	if err != nil {
		return &exec.ReviewSourceFailure{Class: classifyCodexLaunchFailure(err), Err: err}
	}
	launch, err := s.cfg.Backend.CodexReview(ctx, s.cfg.Review, CodexReviewLaunchSpec{
		RunID: string(id), Image: s.cfg.Review.ApprovedImage,
		WorkspaceSourceRunID: string(id), WorkspaceVolume: workspace.Volume,
		ExpectedHead: req.HeadSHA, Prompt: codexProductionReviewPrompt(req),
		Boundary: CodexReviewFreshStart, AuthMode: s.cfg.AuthMode,
		AuthIdentityID: s.cfg.AuthIdentityID, AuthSnapshot: s.cfg.AuthSnapshot,
		Instructions: s.cfg.Instructions, InstructionFile: s.cfg.InstructionFile,
	})
	if err != nil {
		if classifyCodexLaunchFailure(err) == domain.ReviewFailureTransient {
			return &exec.ReviewSourceFailure{Class: domain.ReviewFailureTransient, Err: err}
		}
		cleanupErr := s.cfg.Backend.CleanupCodexReviewWorkspace(
			context.WithoutCancel(ctx), s.cfg.Journal, string(id))
		if cleanupErr != nil {
			return codexReviewLaunchCleanupFailure(err, cleanupErr)
		}
		return &exec.ReviewSourceFailure{
			Class: classifyCodexLaunchFailure(err),
			Err:   err,
		}
	}
	s.mu.Lock()
	s.launches[id] = launch
	s.mu.Unlock()
	return nil
}

// codexReviewLaunchCleanupFailure prevents a terminal launch refusal from
// advancing past a workspace whose cleanup has not yet converged.
func codexReviewLaunchCleanupFailure(launchErr, cleanupErr error) error {
	class := classifyCodexObservationFailure(cleanupErr)
	if class == domain.ReviewFailureTransient {
		return &exec.ReviewSourceFailure{Class: class, Err: errors.Join(launchErr, cleanupErr)}
	}
	return &exec.ReviewSourceFailure{
		Class: domain.ReviewFailureContradiction, Err: errors.Join(launchErr, cleanupErr),
	}
}

func codexProductionReviewPrompt(req exec.ReviewRequest) string {
	evidence, _ := json.Marshal(req.Verification)
	return fmt.Sprintf("Review the exact candidate at head %s against base %s. The preceding verification evidence is %s. Focus on correctness, security, data loss, and regressions. Return every actionable finding through the required JSON schema. Return an empty findings array only when the candidate is clean.",
		req.HeadSHA, req.BaseSHA, evidence)
}

func (s *CodexReviewSource) Inspect(
	ctx context.Context, id domain.InvocationID,
) (exec.Status, error) {
	outcome, ready, err := s.cfg.Journal.GetCodexReviewOutcome(ctx, string(id))
	if err == nil {
		if err := outcome.Validate(); err != nil {
			return "", &exec.ReviewSourceFailure{Class: domain.ReviewFailureContradiction, Err: err}
		}
		if outcome.InvocationID != id {
			return "", &exec.ReviewSourceFailure{
				Class: domain.ReviewFailureContradiction, Err: domain.ErrParentKeyMismatch,
			}
		}
		if !ready {
			if err := s.finishCleanup(ctx, id, outcome.AbortRequired); err != nil {
				return codexReviewCleanupStatus(err)
			}
		}
		if outcome.Result != nil {
			return exec.StatusCompleted, nil
		}
		return exec.StatusFailed, nil
	}
	if !errors.Is(err, ErrCodexReviewOutcomeNotFound) {
		if errors.Is(err, ErrCodexReviewOutcomeRejected) {
			return s.rejectedOutcomeStatus(ctx, id, err)
		}
		return "", &exec.ReviewSourceFailure{Class: domain.ReviewFailureTransient, Err: err}
	}
	request, err := s.cfg.Journal.GetCodexReviewRequest(ctx, string(id))
	if err != nil {
		if errors.Is(err, ErrCodexReviewRequestRejected) {
			rejectErr := s.rejectPersistedRequest(ctx, id, err)
			if exec.ClassifyReviewSourceFailure(rejectErr) == domain.ReviewFailureTransient {
				return exec.StatusPending, nil
			}
			return "", rejectErr
		}
		if !errors.Is(err, exec.ErrUnknownInvocation) {
			return "", &exec.ReviewSourceFailure{Class: domain.ReviewFailureTransient, Err: err}
		}
		return "", err
	}
	if err := request.Validate(); err != nil {
		rejectErr := s.rejectPersistedRequest(ctx, id, err)
		if rejectErr != nil {
			if exec.ClassifyReviewSourceFailure(rejectErr) == domain.ReviewFailureTransient {
				return exec.StatusPending, nil
			}
			return "", rejectErr
		}
	}
	intent, intentErr := s.cfg.Journal.GetCodexReviewIntent(ctx, string(id))
	if intentErr == nil && intent.validateIdentity(string(id)) != nil {
		return "", &exec.ReviewSourceFailure{
			Class: domain.ReviewFailureContradiction,
			Err:   errors.New("persisted Codex review launch intent is invalid"),
		}
	}
	if errors.Is(intentErr, ErrCodexReviewIntentNotFound) ||
		(intentErr == nil && intent.State != CodexReviewIntentStarted) {
		if err := s.startRequestedReview(ctx, id, request); err != nil {
			return "", err
		}
		return exec.StatusRunning, nil
	}
	if intentErr != nil {
		return "", &exec.ReviewSourceFailure{
			Class: classifyCodexObservationFailure(intentErr), Err: intentErr,
		}
	}
	state, err := s.cfg.Backend.InspectCodexReview(ctx, s.cfg.Review, string(id))
	if err != nil {
		return "", &exec.ReviewSourceFailure{Class: classifyCodexObservationFailure(err), Err: err}
	}
	if state == StateRunning {
		s.mu.Lock()
		launch := s.launches[id]
		s.mu.Unlock()
		if launch == nil {
			outcome = CodexReviewSourceOutcome{
				InvocationID:  id,
				FailureClass:  domain.ReviewFailureTransient,
				Failure:       "daemon restarted while Codex review was running; the invocation proxy was lost",
				AbortRequired: true,
			}
			if err := s.cfg.Journal.PutCodexReviewOutcome(ctx, string(id), outcome); err != nil {
				return "", codexReviewOutcomeWriteFailure(err)
			}
			if err := s.finishCleanup(ctx, id, true); err != nil {
				return codexReviewCleanupStatus(err)
			}
			return exec.StatusFailed, nil
		}
		return exec.StatusRunning, nil
	}
	collection, err := s.cfg.Backend.CollectCodexReview(ctx, s.cfg.Review, string(id))
	if err != nil {
		if !errors.Is(err, ErrCodexReviewOutputInvalid) {
			return "", &exec.ReviewSourceFailure{Class: classifyCodexObservationFailure(err), Err: err}
		}
		outcome = CodexReviewSourceOutcome{
			InvocationID: id, FailureClass: domain.ReviewFailureContradiction,
			Failure: fmt.Sprintf("Codex review returned invalid raw output: %v", err),
		}
	} else {
		outcome = s.normalizeCollection(id, request, collection)
		if err := outcome.Validate(); err != nil {
			// A collected result that fails validation is a content contradiction
			// inside a healthy topology: the reviewer answered, the answer is
			// unusable. Persist it as a durable failure and finish authenticated
			// cleanup; an unpersisted early return would terminalize the
			// invocation while leaving the stopped credential-bearing topology
			// behind forever. Topology contradictions (conformance failures from
			// inspect/collect/cleanup) stay loud without cleanup above, because
			// there the topology itself can no longer be trusted to tear down.
			outcome = CodexReviewSourceOutcome{
				InvocationID:       id,
				FailureClass:       domain.ReviewFailureContradiction,
				Failure:            fmt.Sprintf("Codex review returned an invalid collected result: %v", err),
				CollectionEvidence: outcome.CollectionEvidence,
			}
		}
	}
	if err := s.cfg.Journal.PutCodexReviewOutcome(ctx, string(id), outcome); err != nil {
		return "", codexReviewOutcomeWriteFailure(err)
	}
	if err := s.finishCleanup(ctx, id, false); err != nil {
		return codexReviewCleanupStatus(err)
	}
	if outcome.Result != nil {
		return exec.StatusCompleted, nil
	}
	return exec.StatusFailed, nil
}

func codexReviewOutcomeWriteFailure(err error) error {
	return &exec.ReviewSourceFailure{
		Class: classifyCodexObservationFailure(err),
		Err:   fmt.Errorf("persist Codex review outcome: %w", err),
	}
}

func codexReviewCleanupStatus(err error) (exec.Status, error) {
	class := classifyCodexObservationFailure(err)
	if class == domain.ReviewFailureTransient {
		return exec.StatusPending, nil
	}
	return "", &exec.ReviewSourceFailure{Class: class, Err: err}
}

func (s *CodexReviewSource) finishCleanup(
	ctx context.Context, id domain.InvocationID, abort bool,
) error {
	s.mu.Lock()
	launch := s.launches[id]
	delete(s.launches, id)
	s.mu.Unlock()
	if launch != nil {
		_ = launch.Close()
	}
	var err error
	if abort {
		err = s.cfg.Backend.AbortCodexReview(ctx, s.cfg.Review, string(id))
	} else {
		err = s.cfg.Backend.CleanupCodexReview(ctx, s.cfg.Review, string(id))
	}
	if err != nil {
		return err
	}
	return s.cfg.Journal.MarkCodexReviewOutcomeReady(ctx, string(id))
}

func (s *CodexReviewSource) normalizeCollection(
	id domain.InvocationID, req exec.ReviewRequest, collection CodexReviewCollection,
) CodexReviewSourceOutcome {
	evidenceBytes := fmt.Appendf(nil, "codex-review-completion-v1:%d:", len(collection.Events))
	evidenceBytes = append(evidenceBytes, collection.Events...)
	evidenceBytes = fmt.Appendf(evidenceBytes, ":%d:", len(collection.Result))
	evidenceBytes = append(evidenceBytes, collection.Result...)
	evidenceBytes = fmt.Appendf(evidenceBytes, ":%d", collection.ExitStatus)
	collectionEvidence := domain.Digest(fmt.Sprintf("sha256:%x", sha256.Sum256(evidenceBytes)))
	if collection.ExitStatus != 0 {
		class := classifyCodexTerminalFailure(collection.Events)
		return CodexReviewSourceOutcome{
			InvocationID: id,
			FailureClass: class,
			Failure:      fmt.Sprintf("Codex review exited with status %d", collection.ExitStatus),
		}
	}
	var raw struct {
		Findings *[]struct {
			Severity    string `json:"severity"`
			Location    string `json:"location"`
			Explanation string `json:"explanation"`
		} `json:"findings"`
	}
	if err := RejectDuplicateJSONKeys(collection.Result); err != nil {
		return CodexReviewSourceOutcome{
			InvocationID: id,
			FailureClass: domain.ReviewFailureContradiction,
			Failure:      "Codex review returned malformed structured output",
		}
	}
	decoder := json.NewDecoder(strings.NewReader(string(collection.Result)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&raw); err != nil {
		return CodexReviewSourceOutcome{
			InvocationID: id,
			FailureClass: domain.ReviewFailureContradiction,
			Failure:      "Codex review returned malformed structured output",
		}
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return CodexReviewSourceOutcome{
			InvocationID: id,
			FailureClass: domain.ReviewFailureContradiction,
			Failure:      "Codex review returned trailing structured output",
		}
	}
	if raw.Findings == nil {
		return CodexReviewSourceOutcome{
			InvocationID: id,
			FailureClass: domain.ReviewFailureContradiction,
			Failure:      "Codex review omitted the required findings array",
		}
	}
	completedAt := s.cfg.Now().UTC()
	findings := make([]domain.Finding, len(*raw.Findings))
	for i, item := range *raw.Findings {
		identity := fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%s\x00%s",
			id, req.BaseSHA, req.HeadSHA, item.Severity, item.Location, item.Explanation)
		sum := sha256.Sum256([]byte(identity))
		findings[i] = domain.Finding{
			ID: domain.FindingID(fmt.Sprintf("review-%x", sum[:12])), RunID: req.RunID,
			Source: "codex_local", Severity: item.Severity, Location: item.Location,
			Message: item.Explanation, RawText: item.Explanation, CreatedAt: completedAt,
		}
	}
	result := exec.ReviewResult{
		InvocationID: id, BaseSHA: req.BaseSHA, HeadSHA: req.HeadSHA,
		Provider: "openai", ModelConfiguration: s.cfg.Review.Model + "/" + s.cfg.Review.ReasoningEffort,
		ConfigurationDigest: s.cfg.ConfigurationDigest,
		CostOwner:           s.cfg.CostOwner, CompletedAt: completedAt,
		Findings: findings,
	}
	result.CompletionEvidence, _ = CodexReviewResultEvidence(result, collectionEvidence)
	return CodexReviewSourceOutcome{
		InvocationID: id, Result: &result, CollectionEvidence: collectionEvidence,
	}
}

// CodexReviewResultEvidence binds the normalized result to the authenticated,
// bounded raw collection account before either is persisted.
func CodexReviewResultEvidence(
	result exec.ReviewResult, collectionEvidence domain.Digest,
) (domain.Digest, error) {
	result.CompletionEvidence = ""
	body, err := json.Marshal(struct {
		Version            string            `json:"version"`
		CollectionEvidence domain.Digest     `json:"collection_evidence"`
		Result             exec.ReviewResult `json:"result"`
	}{"codex-review-result-v1", collectionEvidence, result})
	if err != nil {
		return "", err
	}
	return domain.Digest(fmt.Sprintf("sha256:%x", sha256.Sum256(body))), nil
}

// CodexReviewConfigurationDigest binds the trust profile to every
// deployment-owned input that can change a production review's behavior.
// Credential material and host paths are deliberately excluded.
func CodexReviewConfigurationDigest(
	cfg CodexReviewConfig,
	workspaceSizeMB int64,
	authMode CodexAuthMode,
	authIdentityID domain.AuthIdentityID,
	instructionDigest domain.Digest,
	costOwner string,
) (domain.Digest, error) {
	if cfg.WorkspaceTarget == "" || !digestPinnedImagePattern.MatchString(cfg.ApprovedImage) ||
		!digestPinnedImagePattern.MatchString(cfg.ObserverImage) || cfg.Model == "" ||
		cfg.ReasoningEffort == "" || len(cfg.ProviderEndpoints) == 0 ||
		cfg.AccessTokenLifetimeFloor <= 0 || workspaceSizeMB <= 0 || !authMode.valid() || authIdentityID == "" ||
		!contentaddr.Valid(string(instructionDigest)) || costOwner == "" {
		return "", ErrInvalidCodexReviewSpec
	}
	endpoints := slices.Clone(cfg.ProviderEndpoints)
	slices.Sort(endpoints)
	canonical := struct {
		Version                  string                `json:"version"`
		Topology                 string                `json:"topology"`
		ApprovedImage            string                `json:"approved_image"`
		ObserverImage            string                `json:"observer_image"`
		WorkspaceTarget          string                `json:"workspace_target"`
		WorkspaceSizeMB          int64                 `json:"workspace_size_mb"`
		ProviderEndpoints        []string              `json:"provider_endpoints"`
		Model                    string                `json:"model"`
		ReasoningEffort          string                `json:"reasoning_effort"`
		AccessTokenLifetimeFloor int64                 `json:"access_token_lifetime_floor_ns"`
		AuthMode                 CodexAuthMode         `json:"auth_mode"`
		AuthIdentityID           domain.AuthIdentityID `json:"auth_identity_id"`
		InstructionDigest        domain.Digest         `json:"instruction_digest"`
		CostOwner                string                `json:"cost_owner"`
		CommandTemplateDigest    string                `json:"command_template_digest"`
		PromptProtocol           string                `json:"prompt_protocol"`
	}{
		Version: "codex-review-configuration-v1", Topology: codexReviewTopologyVersion,
		ApprovedImage: cfg.ApprovedImage, ObserverImage: cfg.ObserverImage,
		WorkspaceTarget: cfg.WorkspaceTarget, WorkspaceSizeMB: workspaceSizeMB,
		ProviderEndpoints: endpoints, Model: cfg.Model, ReasoningEffort: cfg.ReasoningEffort,
		AccessTokenLifetimeFloor: int64(cfg.AccessTokenLifetimeFloor), AuthMode: authMode,
		AuthIdentityID: authIdentityID, InstructionDigest: instructionDigest, CostOwner: costOwner,
		CommandTemplateDigest: digestStrings(codexReviewCommand(
			cfg.WorkspaceTarget, cfg.Model, cfg.ReasoningEffort, "<runtime-review-prompt>",
		)),
		PromptProtocol: codexProductionReviewPromptVersion,
	}
	body, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	return domain.Digest(fmt.Sprintf("sha256:%x", sha256.Sum256(body))), nil
}

func (s *CodexReviewSource) Poll(
	ctx context.Context, id domain.InvocationID,
) (exec.ReviewResult, error) {
	outcome, ready, err := s.cfg.Journal.GetCodexReviewOutcome(ctx, string(id))
	if errors.Is(err, ErrCodexReviewOutcomeNotFound) {
		if _, requestErr := s.cfg.Journal.GetCodexReviewRequest(ctx, string(id)); requestErr != nil {
			if errors.Is(requestErr, exec.ErrUnknownInvocation) {
				return exec.ReviewResult{}, requestErr
			}
			if errors.Is(requestErr, ErrCodexReviewRequestRejected) {
				return exec.ReviewResult{}, s.rejectPersistedRequest(ctx, id, requestErr)
			}
			return exec.ReviewResult{}, codexReviewResultNotReady(requestErr)
		}
		return exec.ReviewResult{}, exec.ErrResultNotReady
	}
	if err != nil {
		if errors.Is(err, ErrCodexReviewOutcomeRejected) {
			return exec.ReviewResult{}, s.rejectPersistedOutcome(ctx, id, err)
		}
		return exec.ReviewResult{}, codexReviewResultNotReady(err)
	}
	if err := outcome.Validate(); err != nil {
		return exec.ReviewResult{}, &exec.ReviewSourceFailure{
			Class: domain.ReviewFailureContradiction, Err: err,
		}
	}
	if outcome.InvocationID != id {
		return exec.ReviewResult{}, &exec.ReviewSourceFailure{
			Class: domain.ReviewFailureContradiction, Err: domain.ErrParentKeyMismatch,
		}
	}
	if !ready {
		return exec.ReviewResult{}, exec.ErrResultNotReady
	}
	if outcome.Result == nil {
		return exec.ReviewResult{}, errors.Join(exec.ErrNoResult,
			&exec.ReviewSourceFailure{
				Class: outcome.FailureClass, Err: errors.New(outcome.Failure),
			})
	}
	return *outcome.Result, nil
}

func codexReviewResultNotReady(err error) error {
	return errors.Join(exec.ErrResultNotReady, &exec.ReviewSourceFailure{
		Class: domain.ReviewFailureTransient,
		Err:   err,
	})
}

func (s *CodexReviewSource) Verify(
	ctx context.Context, id domain.InvocationID, expectedBase, expectedHead string,
) error {
	request, err := s.cfg.Journal.GetCodexReviewRequest(ctx, string(id))
	if err != nil {
		if errors.Is(err, exec.ErrUnknownInvocation) {
			return err
		}
		if errors.Is(err, ErrCodexReviewRequestRejected) {
			return s.rejectPersistedRequest(ctx, id, err)
		}
		return &exec.ReviewSourceFailure{Class: domain.ReviewFailureTransient, Err: err}
	}
	if err := request.Validate(); err != nil {
		return s.rejectPersistedRequest(ctx, id, err)
	}
	result, err := s.Poll(ctx, id)
	if err != nil {
		return err
	}
	if result.InvocationID != id || request.BaseSHA != result.BaseSHA || request.HeadSHA != result.HeadSHA {
		return domain.ErrParentKeyMismatch
	}
	if result.BaseSHA != expectedBase || result.HeadSHA != expectedHead {
		return exec.ErrStaleHead
	}
	return nil
}

func (s *CodexReviewSource) VerifyRequestAuthority(
	ctx context.Context, id domain.InvocationID, expected domain.Digest,
) error {
	request, err := s.cfg.Journal.GetCodexReviewRequest(ctx, string(id))
	if err != nil {
		if errors.Is(err, exec.ErrUnknownInvocation) {
			return err
		}
		if errors.Is(err, ErrCodexReviewRequestRejected) {
			return s.rejectPersistedRequest(ctx, id, err)
		}
		return &exec.ReviewSourceFailure{Class: domain.ReviewFailureTransient, Err: err}
	}
	authority, err := request.AuthorityDigest()
	if err != nil || authority != expected {
		if reconcileErr := s.reconcileRejectedRequest(ctx, id); reconcileErr != nil {
			return reconcileErr
		}
		return &exec.ReviewSourceFailure{
			Class: domain.ReviewFailureContradiction,
			Err:   errors.Join(err, domain.ErrParentKeyMismatch),
		}
	}
	return nil
}

func (s *CodexReviewSource) rejectPersistedRequest(
	ctx context.Context, id domain.InvocationID, cause error,
) error {
	if reconcileErr := s.reconcileRejectedRequest(ctx, id); reconcileErr != nil {
		return reconcileErr
	}
	return &exec.ReviewSourceFailure{
		Class: domain.ReviewFailureContradiction,
		Err:   errors.Join(ErrCodexReviewRequestRejected, cause),
	}
}

func (s *CodexReviewSource) rejectPersistedOutcome(
	ctx context.Context, id domain.InvocationID, cause error,
) error {
	if cleanupErr := s.finishCleanup(ctx, id, true); cleanupErr != nil {
		return &exec.ReviewSourceFailure{
			Class: classifyCodexObservationFailure(cleanupErr), Err: cleanupErr,
		}
	}
	return &exec.ReviewSourceFailure{
		Class: domain.ReviewFailureContradiction,
		Err:   errors.Join(ErrCodexReviewOutcomeRejected, cause),
	}
}

func (s *CodexReviewSource) rejectedOutcomeStatus(
	ctx context.Context, id domain.InvocationID, cause error,
) (exec.Status, error) {
	err := s.rejectPersistedOutcome(ctx, id, cause)
	if exec.ClassifyReviewSourceFailure(err) == domain.ReviewFailureTransient {
		return exec.StatusPending, nil
	}
	return "", err
}

// reconcileRejectedRequest converges teardown for an invocation whose
// persisted request has just been rejected (rewritten past its recomputed
// authority, or invalid), before the rejection terminalizes the invocation.
// Without it the rejection strands whatever the original request already
// started: reconciliation halts at the immutable contradiction before
// Inspect, so no later pass would ever abort the credential-bearing
// topology. Once the durable binding exists (every state from prepared
// onward), the invocation is aborted through the durable outcome/ready
// protocol, so a crash mid-teardown resumes on the next verification
// instead of leaking; that teardown authenticates purely against the
// launch intent, binding, and ownership labels, never any row the
// rejected request could have influenced (the workspace reap and the
// outcome row are re-checked here for the same reason). A never-started
// or already-closed invocation reaps at most its prepared workspace
// volume. Only the pre-binding preparing state stays loud without
// teardown: no durable binding exists yet to authenticate one, so it
// lands on the recorded topology-contradiction boundary. A nil return
// means nothing this source started remains live, and the caller reports
// the rejection itself.
func (s *CodexReviewSource) reconcileRejectedRequest(
	ctx context.Context, id domain.InvocationID,
) error {
	outcome, ready, err := s.cfg.Journal.GetCodexReviewOutcome(ctx, string(id))
	if err == nil {
		if validateErr := outcome.Validate(); validateErr != nil || outcome.InvocationID != id {
			return &exec.ReviewSourceFailure{
				Class: domain.ReviewFailureContradiction,
				Err:   errors.Join(validateErr, domain.ErrParentKeyMismatch),
			}
		}
		if ready {
			return nil
		}
		// abort=true regardless of the decoded AbortRequired bit: in this
		// threat model the row is rewritable, a flipped bit against a still
		// running container would refuse teardown forever, and abort also
		// reaps an already stopped topology.
		if cleanupErr := s.finishCleanup(ctx, id, true); cleanupErr != nil {
			return &exec.ReviewSourceFailure{
				Class: classifyCodexObservationFailure(cleanupErr), Err: cleanupErr,
			}
		}
		return nil
	}
	if !errors.Is(err, ErrCodexReviewOutcomeNotFound) {
		if errors.Is(err, ErrCodexReviewOutcomeRejected) {
			return s.rejectPersistedOutcome(ctx, id, err)
		}
		return &exec.ReviewSourceFailure{Class: domain.ReviewFailureTransient, Err: err}
	}
	intent, err := s.cfg.Journal.GetCodexReviewIntent(ctx, string(id))
	if err == nil && intent.validateIdentity(string(id)) != nil {
		return &exec.ReviewSourceFailure{
			Class: domain.ReviewFailureContradiction,
			Err:   errors.New("persisted Codex review launch intent is invalid"),
		}
	}
	if errors.Is(err, ErrCodexReviewIntentNotFound) ||
		(err == nil && intent.State == CodexReviewIntentClosed) {
		if cleanupErr := s.cfg.Backend.CleanupCodexReviewWorkspace(
			ctx, s.cfg.Journal, string(id),
		); cleanupErr != nil && !errors.Is(cleanupErr, ErrCodexReviewWorkspaceNotFound) {
			return &exec.ReviewSourceFailure{
				Class: classifyCodexObservationFailure(cleanupErr), Err: cleanupErr,
			}
		}
		return nil
	}
	if err != nil {
		return &exec.ReviewSourceFailure{Class: classifyCodexObservationFailure(err), Err: err}
	}
	if intent.State == CodexReviewIntentPreparing {
		return &exec.ReviewSourceFailure{
			Class: domain.ReviewFailureContradiction,
			Err: fmt.Errorf(
				"codex review invocation %s was rejected in pre-binding state %q; no durable binding exists yet to authenticate a teardown",
				id, intent.State,
			),
		}
	}
	rejected := CodexReviewSourceOutcome{
		InvocationID:  id,
		FailureClass:  domain.ReviewFailureContradiction,
		Failure:       "persisted Codex review request was rejected after launch; the launched invocation is aborted",
		AbortRequired: true,
	}
	if err := s.cfg.Journal.PutCodexReviewOutcome(ctx, string(id), rejected); err != nil {
		return codexReviewOutcomeWriteFailure(err)
	}
	if cleanupErr := s.finishCleanup(ctx, id, true); cleanupErr != nil {
		return &exec.ReviewSourceFailure{
			Class: classifyCodexObservationFailure(cleanupErr), Err: cleanupErr,
		}
	}
	return nil
}

func classifyCodexLaunchFailure(err error) domain.ReviewFailureClass {
	if errors.Is(err, ErrCodexReviewOperational) {
		return domain.ReviewFailureTransient
	}
	if errors.Is(err, ErrInvalidCodexReviewSpec) || errors.Is(err, ErrConformance) {
		return domain.ReviewFailureConfiguration
	}
	return domain.ReviewFailureTransient
}

func classifyCodexObservationFailure(err error) domain.ReviewFailureClass {
	if errors.Is(err, ErrCodexReviewOperational) {
		return domain.ReviewFailureTransient
	}
	if errors.Is(err, ErrConformance) {
		return domain.ReviewFailureContradiction
	}
	return domain.ReviewFailureTransient
}

func classifyCodexTerminalFailure(events []byte) domain.ReviewFailureClass {
	text := strings.ToLower(string(events))
	switch {
	case strings.Contains(text, "quota"), strings.Contains(text, "rate limit"),
		strings.Contains(text, "too many requests"):
		return domain.ReviewFailureQuota
	case strings.Contains(text, "authentication"), strings.Contains(text, "unauthorized"),
		strings.Contains(text, "invalid api key"), strings.Contains(text, "configuration"):
		return domain.ReviewFailureConfiguration
	default:
		return domain.ReviewFailureTransient
	}
}
