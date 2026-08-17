package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"reflect"
	"slices"
	"syscall"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// freesided submit (plan §5.12, §10): registers the source work item and
// resolved policy as digest-addressed artifacts, creates its pre-approval
// elaboration run, and reserves the future implementation identity.
// Registration only: execution preconditions stay with the dispatch gates,
// so a submission is durable intent even while the daemon is stopped or held.

// maxSubmissionFileBytes bounds one submitted input file. Specifications and
// policies are prose and configuration; a larger file is far more likely a
// mistaken path than a real work item.
const maxSubmissionFileBytes = 4 << 20

const submitResultHelp = `
Result JSON fields by lane:
  source submission: source_digest, source_artifact_id, publication_digest
  elaboration: elaboration_run_id, elaboration_invocation_id, elaboration_stage_id,
    elaboration_policy_digest, elaboration_policy_artifact_id
  reserved implementation: implementation_run_id, implementation_invocation_id,
    implementation_stage_id, campaign_id, attempt_number
  shared: project_id

The legacy fields run_id, invocation_id, stage_id, and work_unit_id are
compatibility aliases bound to the reserved implementation run. The former
spec_digest and spec_artifact_id fields are source_digest and
source_artifact_id; policy_digest and policy_artifact_id are
elaboration_policy_digest and elaboration_policy_artifact_id. No deprecated
digest or artifact aliases are emitted. A legacy production-only replay leaves
the elaboration fields empty because its source is already the implementation
specification.

The approved implementation specification digest is available before start on
the specification-approval AttentionItem claim, and after the run exists from
the spec_digest field returned by GET /runs/{implementation_run_id}.
`

func configureSubmitUsage(flags *flag.FlagSet) {
	flags.Usage = func() {
		_, _ = fmt.Fprintf(flags.Output(), "Usage of %s:\n", flags.Name())
		flags.PrintDefaults()
		_, _ = fmt.Fprint(flags.Output(), submitResultHelp)
	}
}

// runSubmitMain parses the submit verb's flags and runs the command,
// printing one JSON result line on success. Exit contract: 0 converged,
// 1 refused, 2 flag misuse.
func runSubmitMain(args []string) {
	flags := flag.NewFlagSet("freesided submit", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	configureSubmitUsage(flags)
	dbPath := flags.String("db", "", "SQLite database path (required)")
	specPath := flags.String("spec", "", "source work-item specification file (required)")
	policyPath := flags.String("policy", "", "resolved per-run policy-key JSON array (required)")
	publicationPath := flags.String("publication", "", "reviewer-facing pull-request metadata JSON file (required)")
	workUnitPath := flags.String("work-unit", "", "work-unit declaration JSON file (optional; §5.18 capture)")
	projectID := flags.String("project", "", "project id the run belongs to (required)")
	runID := flags.String("run-id", "", "implementation run id (defaults from project, specification, resolved policy, publication metadata, and any work-unit declaration so an exact re-submission converges)")
	if err := flags.Parse(args); err != nil {
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	result, err := runSubmitCommand(ctx, submitCommandConfig{
		DBPath: *dbPath, SpecPath: *specPath, PolicyPath: *policyPath,
		PublicationPath: *publicationPath, WorkUnitPath: *workUnitPath,
		ProjectID: domain.ProjectID(*projectID), RunID: domain.RunID(*runID),
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "freesided:", err)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		fmt.Fprintln(os.Stderr, "freesided:", err)
		os.Exit(1)
	}
}

type submitCommandConfig struct {
	DBPath          string
	SpecPath        string
	PolicyPath      string
	PublicationPath string
	WorkUnitPath    string
	ProjectID       domain.ProjectID
	RunID           domain.RunID
}

// submittedWorkUnit is the --work-unit file's wire shape: exactly the §5.18
// declarations no other submitted input carries. The declared path scope is
// deliberately absent — it derives from the resolved policy's paths key,
// the same declaration the runner enforces, so the two cannot drift.
type submittedWorkUnit struct {
	CompletionCriterion domain.CompletionCriterionKind `json:"completion_criterion"`
	BoundIssue          *int                           `json:"bound_issue,omitempty"`
	DependsOnIssues     []int                          `json:"depends_on_issues,omitempty"`
	ContractSerialized  bool                           `json:"contract_serialized,omitempty"`
}

type submitResult struct {
	RunID                       domain.RunID        `json:"run_id"`
	ElaborationRunID            domain.RunID        `json:"elaboration_run_id"`
	ProjectID                   domain.ProjectID    `json:"project_id"`
	InvocationID                domain.InvocationID `json:"invocation_id"`
	StageID                     domain.StageID      `json:"stage_id"`
	ImplementationRunID         domain.RunID        `json:"implementation_run_id"`
	ImplementationInvocationID  domain.InvocationID `json:"implementation_invocation_id"`
	ImplementationStageID       domain.StageID      `json:"implementation_stage_id"`
	ElaborationInvocationID     domain.InvocationID `json:"elaboration_invocation_id"`
	ElaborationStageID          domain.StageID      `json:"elaboration_stage_id"`
	SourceDigest                domain.Digest       `json:"source_digest"`
	ElaborationPolicyDigest     domain.Digest       `json:"elaboration_policy_digest"`
	SourceArtifactID            domain.ArtifactID   `json:"source_artifact_id"`
	ElaborationPolicyArtifactID domain.ArtifactID   `json:"elaboration_policy_artifact_id"`
	PublicationDigest           domain.Digest       `json:"publication_digest"`
	WorkUnitID                  domain.WorkUnitID   `json:"work_unit_id,omitempty"`
	CampaignID                  domain.CampaignID   `json:"campaign_id,omitempty"`
	AttemptNumber               int                 `json:"attempt_number,omitempty"`
	AttemptReason               string              `json:"attempt_reason,omitempty"`
	ParentRunID                 domain.RunID        `json:"parent_run_id,omitempty"`
	ApprovedSpecDigest          domain.Digest       `json:"approved_spec_digest,omitempty"`
}

type submissionFile struct {
	digest domain.Digest
	body   []byte
}

// readSubmissionFile hashes one input file under the size cap. The digest is
// computed here, never trusted from the caller, so the registered artifact
// and the run's trusted configuration name exactly the bytes read.
func readSubmissionFile(path string) (submissionFile, error) {
	f, err := os.Open(path) //nolint:gosec // G304: operator-supplied submission path is this command's whole purpose; bytes are hashed, size-capped, and registered by digest
	if err != nil {
		return submissionFile{}, err
	}
	defer func() { _ = f.Close() }()
	body, err := io.ReadAll(io.LimitReader(f, maxSubmissionFileBytes+1))
	if err != nil {
		return submissionFile{}, err
	}
	if len(body) > maxSubmissionFileBytes {
		return submissionFile{}, fmt.Errorf("%s exceeds the %d-byte submission cap", path, maxSubmissionFileBytes)
	}
	if len(body) == 0 {
		return submissionFile{}, fmt.Errorf("%s is empty", path)
	}
	return submissionBytes(body), nil
}

func submissionBytes(body []byte) submissionFile {
	sum := sha256.Sum256(body)
	return submissionFile{
		digest: domain.Digest(contentaddr.Format(sum[:])),
		body:   body,
	}
}

func runSubmitCommand(ctx context.Context, cfg submitCommandConfig) (submitResult, error) {
	switch {
	case cfg.DBPath == "":
		return submitResult{}, errors.New("submit: -db is required")
	case cfg.SpecPath == "":
		return submitResult{}, errors.New("submit: --spec is required")
	case cfg.PolicyPath == "":
		return submitResult{}, errors.New("submit: --policy is required")
	case cfg.PublicationPath == "":
		return submitResult{}, errors.New("submit: --publication is required")
	case cfg.ProjectID == "":
		return submitResult{}, errors.New("submit: --project is required")
	}

	spec, err := readSubmissionFile(cfg.SpecPath)
	if err != nil {
		return submitResult{}, fmt.Errorf("submit: read specification: %w", err)
	}
	policyFile, err := readSubmissionFile(cfg.PolicyPath)
	if err != nil {
		return submitResult{}, fmt.Errorf("submit: read policy: %w", err)
	}
	publicationFile, err := readSubmissionFile(cfg.PublicationPath)
	if err != nil {
		return submitResult{}, fmt.Errorf("submit: read publication metadata: %w", err)
	}
	publicationDigest := publicationFile.digest
	if err := ward.RejectDuplicateJSONKeys(publicationFile.body); err != nil {
		return submitResult{}, fmt.Errorf("submit: decode publication metadata: %w", err)
	}
	var publication engine.ProductionPublication
	if err := strictjson.Decode(
		publicationFile.body, &publication, strictjson.TolerateInvalidUTF8, strictjson.Limit(maxSubmissionFileBytes),
	); err != nil {
		if errors.Is(err, strictjson.ErrTrailingData) {
			return submitResult{}, errors.New("submit: decode publication metadata: trailing JSON value")
		}
		return submitResult{}, fmt.Errorf("submit: decode publication metadata: %w", err)
	}
	if err := publication.Validate(); err != nil {
		return submitResult{}, fmt.Errorf("submit: decode publication metadata: %w", err)
	}
	publicationBody, err := json.Marshal(publication)
	if err != nil {
		return submitResult{}, fmt.Errorf("submit: encode publication metadata: %w", err)
	}
	publicationFile = submissionBytes(publicationBody)

	if err := ward.RejectDuplicateJSONKeys(policyFile.body); err != nil {
		return submitResult{}, fmt.Errorf("submit: decode resolved policy keys: %w", err)
	}
	var keys []domain.PolicyKey
	if err := strictjson.Decode(
		policyFile.body, &keys, strictjson.TolerateInvalidUTF8, strictjson.Limit(maxSubmissionFileBytes),
	); err != nil {
		if errors.Is(err, strictjson.ErrTrailingData) {
			return submitResult{}, errors.New("submit: decode resolved policy keys: trailing JSON value")
		}
		return submitResult{}, fmt.Errorf("submit: decode resolved policy keys: %w", err)
	}
	policyDigest, err := (domain.ResolvedPolicy{Keys: keys}).ComputeDigest()
	if err != nil {
		return submitResult{}, fmt.Errorf("submit: digest resolved policy keys: %w", err)
	}

	var (
		workUnit       *domain.WorkUnitDeclarationInput
		workUnitDigest domain.Digest
	)
	if cfg.WorkUnitPath != "" {
		workUnitFile, err := readSubmissionFile(cfg.WorkUnitPath)
		if err != nil {
			return submitResult{}, fmt.Errorf("submit: read work-unit declaration: %w", err)
		}
		if err := ward.RejectDuplicateJSONKeys(workUnitFile.body); err != nil {
			return submitResult{}, fmt.Errorf("submit: decode work-unit declaration: %w", err)
		}
		var declared submittedWorkUnit
		if err := strictjson.Decode(
			workUnitFile.body, &declared, strictjson.TolerateInvalidUTF8, strictjson.Limit(maxSubmissionFileBytes),
		); err != nil {
			if errors.Is(err, strictjson.ErrTrailingData) {
				return submitResult{}, errors.New("submit: decode work-unit declaration: trailing JSON value")
			}
			return submitResult{}, fmt.Errorf("submit: decode work-unit declaration: %w", err)
		}
		// Declared collections are canonicalized here, not refused: their
		// order carries no meaning, and the canonical form is what makes
		// replay convergence — and the run-id digest below — insensitive
		// to restatements of the same declaration.
		slices.Sort(declared.DependsOnIssues)
		declared.DependsOnIssues = slices.Compact(declared.DependsOnIssues)
		workUnit = &domain.WorkUnitDeclarationInput{
			CompletionCriterion: declared.CompletionCriterion,
			BoundIssue:          declared.BoundIssue,
			DependsOnIssues:     declared.DependsOnIssues,
			DeclaredPaths:       declaredPathScope(keys),
			ContractSerialized:  declared.ContractSerialized,
		}
		canonicalBody, err := json.Marshal(declared)
		if err != nil {
			return submitResult{}, fmt.Errorf("submit: encode work-unit declaration: %w", err)
		}
		workUnitDigest = submissionBytes(canonicalBody).digest
	}

	implementationRunID := cfg.RunID
	if implementationRunID == "" {
		// The default covers every immutable run binding so only an exact
		// resubmission converges; shared specification bytes in another
		// project, under another policy, with different reviewer-facing
		// metadata, or under a different work-unit declaration remain
		// distinct work items. An undeclared submission keeps the
		// pre-capture derivation byte-for-byte.
		implementationRunID = defaultSubmissionRunID(
			cfg.ProjectID, spec.digest, policyDigest, publicationFile.digest, workUnitDigest)
	}
	elaborationRunID, err := engine.ElaborationRunIDForImplementation(implementationRunID)
	if err != nil {
		return submitResult{}, fmt.Errorf("submit: %w", err)
	}
	campaignID, err := engine.ProductionCampaignIDForImplementation(implementationRunID)
	if err != nil {
		return submitResult{}, fmt.Errorf("submit: %w", err)
	}
	resolvedPolicy, err := domain.NewResolvedPolicy(elaborationRunID, keys)
	if err != nil {
		return submitResult{}, fmt.Errorf("submit: validate resolved policy: %w", err)
	}
	// The declared-path boundary is what the runner enforces, and it is
	// refused at start when it is absent or not an explicit allowlist. Refuse
	// it here instead: submission is the operator's door and can still say
	// no, while a run durable without one is a work item the daemon holds
	// with no configuration change that could ever release it.
	if err := submittedPathBoundary(resolvedPolicy); err != nil {
		return submitResult{}, fmt.Errorf("submit: %w", err)
	}
	policyBody, err := json.Marshal(resolvedPolicy.Keys)
	if err != nil {
		return submitResult{}, fmt.Errorf("submit: encode resolved policy keys: %w", err)
	}
	policy := submissionFile{digest: resolvedPolicy.Digest, body: policyBody}

	st, _, err := openStoreWithTopicKey(ctx, cfg.DBPath, store.Options{})
	if err != nil {
		return submitResult{}, fmt.Errorf("submit: open store: %w", err)
	}
	defer func() { _ = st.Close() }()
	blobs, err := signet.NewBlobStore(cfg.DBPath + ".blobs")
	if err != nil {
		return submitResult{}, fmt.Errorf("submit: open blob store: %w", err)
	}

	// Bytes land before metadata: an artifact row must never name a digest
	// the blob store cannot serve, since admission materializes stage inputs
	// by digest.
	if _, err := blobs.Put(spec.digest, bytes.NewReader(spec.body)); err != nil {
		return submitResult{}, fmt.Errorf("submit: store specification bytes: %w", err)
	}
	if _, err := blobs.Put(policy.digest, bytes.NewReader(policy.body)); err != nil {
		return submitResult{}, fmt.Errorf("submit: store policy bytes: %w", err)
	}

	specArtifact, err := submissionArtifact(domain.ArtifactKindSpecification, spec.digest)
	if err != nil {
		return submitResult{}, fmt.Errorf("submit: %w", err)
	}
	policyArtifact, err := submissionArtifact(domain.ArtifactKindPolicy, policy.digest)
	if err != nil {
		return submitResult{}, fmt.Errorf("submit: %w", err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutArtifact(ctx, specArtifact); err != nil {
			return err
		}
		return tx.PutArtifact(ctx, policyArtifact)
	}); err != nil {
		return submitResult{}, fmt.Errorf("submit: register artifacts: %w", err)
	}
	elaborationStatePresent, err := engine.HasElaborationIntakeState(
		ctx, st, elaborationRunID, implementationRunID,
	)
	if err != nil {
		return submitResult{}, fmt.Errorf("submit: inspect elaboration intake: %w", err)
	}
	if !elaborationStatePresent {
		legacy, found, err := legacyProductionReplay(ctx, st, implementationRunID, cfg.ProjectID,
			specArtifact, policyArtifact, keys, publication, workUnit, publicationDigest)
		if err != nil {
			return submitResult{}, fmt.Errorf("submit: inspect legacy production replay: %w", err)
		}
		if found {
			return legacy, nil
		}
	}

	submitted, err := engine.SubmitElaborationRun(ctx, st, engine.ElaborationRunSpec{
		ElaborationRunID: elaborationRunID, ImplementationRunID: implementationRunID,
		ProjectID: cfg.ProjectID, SourceArtifactID: specArtifact.ID,
		PolicyArtifactID: policyArtifact.ID, ResolvedPolicy: resolvedPolicy, Publication: publication,
		PublicationDigest: publicationDigest,
		WorkUnit:          workUnit, CampaignID: campaignID, AttemptNumber: 1,
	})
	if err != nil {
		return submitResult{}, fmt.Errorf("submit: %w", err)
	}
	result := submitResult{
		RunID: submitted.ImplementationRunID, ElaborationRunID: submitted.Run.ID,
		ProjectID: submitted.Run.ProjectID,
		// Keep the original fields as implementation aliases for compatibility
		// while exposing both lanes without ambiguity.
		InvocationID:                submitted.ImplementationInvocationID,
		StageID:                     submitted.ImplementationStageID,
		ImplementationRunID:         submitted.ImplementationRunID,
		ImplementationInvocationID:  submitted.ImplementationInvocationID,
		ImplementationStageID:       submitted.ImplementationStageID,
		ElaborationInvocationID:     submitted.ElaborationInvocationID,
		ElaborationStageID:          submitted.ElaborationStageID,
		SourceDigest:                spec.digest,
		ElaborationPolicyDigest:     submitted.Run.PolicyDigest,
		SourceArtifactID:            specArtifact.ID,
		ElaborationPolicyArtifactID: policyArtifact.ID,
		PublicationDigest:           publicationDigest,
		CampaignID:                  submitted.Run.CampaignID,
		AttemptNumber:               submitted.Run.AttemptNumber,
	}
	if workUnit != nil {
		result.WorkUnitID = domain.WorkUnitIDForRun(submitted.ImplementationRunID)
	}
	return result, nil
}

// legacyProductionReplay preserves exact retries from the production-only
// submit protocol that preceded elaboration. A matching legacy run remains
// authoritative instead of being retrofitted with an elaboration reservation:
// that reservation is an intake-time fact, and creating it after execution
// could retarget a live or terminal production workflow.
func legacyProductionReplay(
	ctx context.Context,
	st *store.Store,
	runID domain.RunID,
	projectID domain.ProjectID,
	specArtifact, policyArtifact domain.Artifact,
	keys []domain.PolicyKey,
	publication engine.ProductionPublication,
	workUnit *domain.WorkUnitDeclarationInput,
	publicationDigest domain.Digest,
) (submitResult, bool, error) {
	var run domain.Run
	var resolved domain.ResolvedPolicy
	var marker store.QueueEntry
	var invocation domain.AgentInvocation
	var declaration domain.WorkUnitDeclaration
	var declarationFound bool
	err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		run, err = tx.GetRun(ctx, runID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		resolved, err = tx.GetResolvedPolicy(ctx, runID)
		if err != nil {
			return err
		}
		marker, err = tx.GetOutbox(ctx, "inv-implement-"+string(runID))
		if err != nil {
			return err
		}
		invocation, err = tx.GetAgentInvocation(ctx, domain.InvocationID("inv-implement-"+string(runID)))
		if err != nil {
			return err
		}
		declaration, err = tx.GetWorkUnitDeclarationByRun(ctx, runID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		declarationFound = err == nil
		return err
	})
	if err != nil {
		return submitResult{}, false, err
	}
	if run.ID == "" {
		return submitResult{}, false, nil
	}
	implementationPolicy, err := domain.NewResolvedPolicy(runID, keys)
	if err != nil {
		return submitResult{}, false, err
	}
	publicationFromMarker, present, err := engine.ProductionInvocationPublication(marker)
	if err != nil {
		return submitResult{}, false, nil
	}
	implementationStage := false
	for _, stage := range run.Stages {
		if stage.ID == domain.StageID("implement-"+string(runID)) && stage.Name == "implement" {
			implementationStage = true
			break
		}
	}
	if run.ProjectID != projectID || run.SpecDigest != specArtifact.Digest ||
		run.PolicyDigest != policyArtifact.Digest || resolved.RunID != runID ||
		resolved.Digest != implementationPolicy.Digest ||
		!slices.Equal(resolved.Keys, implementationPolicy.Keys) || !present ||
		!reflect.DeepEqual(publicationFromMarker, publication) ||
		!implementationStage || invocation.ConversationID != nil ||
		!slices.Equal(invocation.InputIDs, []domain.ArtifactID{specArtifact.ID}) ||
		invocation.ThroughSequence != 0 {
		return submitResult{}, false, nil
	}
	if workUnit == nil {
		if declarationFound {
			return submitResult{}, false, nil
		}
	} else {
		want, err := domain.NewWorkUnitDeclaration(*workUnit, runID, projectID, declaration.DeclaredAt)
		if err != nil || !declarationFound || !reflect.DeepEqual(want, declaration) {
			return submitResult{}, false, err
		}
	}
	invocationID := domain.InvocationID("inv-implement-" + string(runID))
	stageID := domain.StageID("implement-" + string(runID))
	result := submitResult{
		RunID: runID, ProjectID: projectID,
		InvocationID: invocationID, StageID: stageID,
		ImplementationRunID: runID, ImplementationInvocationID: invocationID,
		ImplementationStageID: stageID,
		SourceDigest:          specArtifact.Digest,
		SourceArtifactID:      specArtifact.ID,
		PublicationDigest:     publicationDigest,
	}
	if declarationFound {
		result.WorkUnitID = domain.WorkUnitIDForRun(runID)
	}
	return result, true, nil
}

// declaredPathScope extracts the resolved policy's paths key as the unit's
// declared path scope, through the domain's single canonical definition —
// the same one the store's declaration re-gate re-derives with, so the
// recorded scope and the re-gate can never disagree. The submission gate
// (submittedPathBoundary) has already refused a policy without an explicit
// allowlist, so a declared unit always carries the scope the runner
// enforces.
func declaredPathScope(keys []domain.PolicyKey) []string {
	return domain.CanonicalDeclaredPaths(domain.ResolvedPolicy{Keys: keys})
}

func defaultSubmissionRunID(
	projectID domain.ProjectID, specDigest, policyDigest, publicationDigest, workUnitDigest domain.Digest,
) domain.RunID {
	bindings := string(projectID) + "\x00" + string(specDigest) + "\x00" +
		string(policyDigest) + "\x00" + string(publicationDigest)
	if workUnitDigest != "" {
		bindings += "\x00" + string(workUnitDigest)
	}
	sum := sha256.Sum256([]byte(bindings))
	return domain.RunID("run-" + hex.EncodeToString(sum[:]))
}

// submissionArtifact is the digest-addressed registration of one submitted
// input. Identity and provenance are both content-derived (never
// run-derived), so two runs submitting the same bytes converge on one
// write-once artifact row instead of conflicting; the daemon-produced
// provenance carries no recipe, so the artifact is never publish-eligible.
func submissionArtifact(role domain.ArtifactKind, digest domain.Digest) (domain.Artifact, error) {
	hexDigits := string(digest[len("sha256:"):])
	return domain.NewArtifact(domain.ArtifactInput{
		ID:     domain.ArtifactID("artifact-" + string(role) + "-" + hexDigits),
		Type:   role,
		Digest: digest,
		Provenance: domain.Provenance{
			ProducerClass:        domain.ProducerDaemon,
			ProducerInvocationID: domain.InvocationID("submit-" + string(role) + "-" + hexDigits),
			HeadBinding:          domain.HeadIndependent,
			SensitivityClass:     domain.SensitivityNormal,
		},
	}, nil)
}
