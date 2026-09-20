package main

import (
	"bytes"
	"context"
	"crypto/sha256"
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
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/specify"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// freesided submit (plan §5.12, §10): registers the task source and
// resolved policy as digest-addressed artifacts, creates its pre-approval
// specification run, and reserves the future implementation identity.
// Registration only: execution preconditions stay with the dispatch gates,
// so a submission is durable intent even while the daemon is stopped or held.

// maxSubmissionFileBytes bounds one submitted input file. Specifications and
// policies are prose and configuration; a larger file is far more likely a
// mistaken path than a real task.
const maxSubmissionFileBytes = 4 << 20

const submitResultHelp = `
Result JSON fields by lane:
  source submission: source_digest, source_artifact_id, publication_digest
  specification: specification_run_id, specification_invocation_id, specification_stage_id,
    specification_policy_digest, specification_policy_artifact_id
  reserved implementation: implementation_run_id, implementation_invocation_id,
    implementation_stage_id, campaign_id, attempt_number
  shared: project_id, composition_digest

The legacy fields run_id, invocation_id, stage_id, and work_unit_id are
compatibility aliases bound to the reserved implementation run. The former
spec_digest and spec_artifact_id fields are source_digest and
source_artifact_id; policy_digest and policy_artifact_id are
specification_policy_digest and specification_policy_artifact_id. No deprecated
digest or artifact aliases are emitted. A legacy production-only replay leaves
the specification fields empty because its source is already the implementation
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

// validateWriterStopBudget rejects a writer-stop timeout that the daemon or the
// supervision deadline cannot honor, at the run-creation boundary so a doomed
// value never produces a durable run. flag.Duration has already parsed both
// authoritatively (malformed and out-of-range inputs fail in Parse), so this
// only vets their meaning:
//
//   - A negative writer budget is not "unset": ward.Config.validate rejects it,
//     so the daemon would fail at startup after submission. Reject it here.
//   - A zero writer budget means "use ward's default", so it is compared at that
//     effective value (ward.DefaultWriterStopTimeout), not skipped.
//   - The effective writer budget must stay below supervision, which bounds the
//     whole workflow (specification approval through publication) with one
//     deadline; a budget at or above it is killed before it elapses. A
//     non-positive supervision means the harness asserts no bound.
func validateWriterStopBudget(writerStop, supervision time.Duration) error {
	if writerStop < 0 {
		return fmt.Errorf("writer-stop-timeout %s must not be negative", writerStop)
	}
	effective := writerStop
	if effective == 0 {
		effective = ward.DefaultWriterStopTimeout
	}
	if supervision > 0 && effective >= supervision {
		return fmt.Errorf(
			"writer-stop-timeout %s must be below supervision-timeout %s: supervision bounds the whole workflow and would stop the run before the writer budget elapses; raise -supervision-timeout or lower the writer budget",
			effective, supervision)
	}
	return nil
}

// runSubmitMain parses the submit verb's flags and runs the command,
// printing one JSON result line on success. Exit contract: 0 converged,
// 1 refused, 2 flag misuse.
func runSubmitMain(args []string) {
	flags := flag.NewFlagSet("freesided submit", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	configureSubmitUsage(flags)
	dbPath := flags.String("db", "", "SQLite database path (required)")
	taskPath := flags.String("task", "", "task source file (required)")
	policyPath := flags.String("policy", "", "resolved per-run policy-key JSON array (required)")
	publicationPath := flags.String("publication", "", "reviewer-facing pull-request metadata JSON file (required)")
	compositionPath := flags.String("composition-manifest", "", "passing production-composition manifest bound to the submitted inputs")
	requireComposition := flags.Bool("require-composition", false, "require trusted production-composition evidence (unattended submission)")
	workUnitPath := flags.String("work-unit", "", "work-unit declaration JSON file (optional; §5.18 capture)")
	projectID := flags.String("project", "", "project id the run belongs to (required)")
	runID := flags.String("run-id", "", "lookup-only legacy implementation run id; never creates work")
	submissionID := flags.String("submission-id", "", "prepared identity for new work (otherwise generated and saved before submission)")
	retrySubmissionID := flags.String("retry-submission-id", "", "manually retry a saved submission using its original inputs")
	// Validated here, at the run-creation boundary, so a malformed, out-of-range,
	// or unsatisfiable writer budget fails before a durable run exists rather
	// than stranding one when the daemon later parses the same flag. The daemon
	// run consumes -writer-stop-timeout; submit only vets it against the harness
	// supervision deadline.
	writerStopTimeout := flags.Duration("writer-stop-timeout", 0,
		"implementation writer-container budget, vetted against -supervision-timeout before the run is created; 0 uses the ward default")
	supervisionTimeout := flags.Duration("supervision-timeout", 0,
		"harness workflow supervision deadline the writer budget must stay below; 0 skips the check")
	if err := flags.Parse(args); err != nil {
		os.Exit(2)
	}
	if err := validateWriterStopBudget(*writerStopTimeout, *supervisionTimeout); err != nil {
		fmt.Fprintln(os.Stderr, "freesided:", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	result, err := runSubmitCommand(ctx, submitCommandConfig{
		SubmissionID: *submissionID, RetrySubmissionID: *retrySubmissionID,
		DBPath: *dbPath, TaskPath: *taskPath, PolicyPath: *policyPath,
		PublicationPath: *publicationPath, WorkUnitPath: *workUnitPath,
		CompositionPath:    *compositionPath,
		RequireComposition: *requireComposition,
		ProjectID:          domain.ProjectID(*projectID), RunID: domain.RunID(*runID),
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
	SubmissionID       string
	RetrySubmissionID  string
	SavedInputs        map[string][]byte
	DBPath             string
	TaskPath           string
	PolicyPath         string
	PublicationPath    string
	WorkUnitPath       string
	CompositionPath    string
	RequireComposition bool
	ProjectID          domain.ProjectID
	RunID              domain.RunID
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
	SubmissionID                  string              `json:"submission_id,omitempty"`
	RunID                         domain.RunID        `json:"run_id"`
	SpecificationRunID            domain.RunID        `json:"specification_run_id"`
	ProjectID                     domain.ProjectID    `json:"project_id"`
	InvocationID                  domain.InvocationID `json:"invocation_id"`
	StageID                       domain.StageID      `json:"stage_id"`
	ImplementationRunID           domain.RunID        `json:"implementation_run_id"`
	ImplementationInvocationID    domain.InvocationID `json:"implementation_invocation_id"`
	ImplementationStageID         domain.StageID      `json:"implementation_stage_id"`
	SpecificationInvocationID     domain.InvocationID `json:"specification_invocation_id"`
	SpecificationStageID          domain.StageID      `json:"specification_stage_id"`
	SourceDigest                  domain.Digest       `json:"source_digest"`
	SpecificationPolicyDigest     domain.Digest       `json:"specification_policy_digest"`
	SourceArtifactID              domain.ArtifactID   `json:"source_artifact_id"`
	SpecificationPolicyArtifactID domain.ArtifactID   `json:"specification_policy_artifact_id"`
	PublicationDigest             domain.Digest       `json:"publication_digest"`
	CompositionDigest             domain.Digest       `json:"composition_digest,omitempty"`
	WorkUnitID                    domain.WorkUnitID   `json:"work_unit_id,omitempty"`
	CampaignID                    domain.CampaignID   `json:"campaign_id,omitempty"`
	AttemptNumber                 int                 `json:"attempt_number,omitempty"`
	AttemptReason                 string              `json:"attempt_reason,omitempty"`
	ParentRunID                   domain.RunID        `json:"parent_run_id,omitempty"`
	ApprovedSpecDigest            domain.Digest       `json:"approved_spec_digest,omitempty"`
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
	if cfg.CompositionPath != "" && cfg.RunID != "" {
		return submitResult{}, errors.New("submit: --run-id cannot override production composition identity")
	}
	var err error
	cfg, err = prepareSubmission(cfg)
	if err != nil {
		return submitResult{}, err
	}
	switch {
	case cfg.DBPath == "":
		return submitResult{}, errors.New("submit: -db is required")
	case cfg.TaskPath == "":
		return submitResult{}, errors.New("submit: --task is required")
	case cfg.PolicyPath == "":
		return submitResult{}, errors.New("submit: --policy is required")
	case cfg.PublicationPath == "":
		return submitResult{}, errors.New("submit: --publication is required")
	case cfg.RequireComposition && cfg.CompositionPath == "":
		return submitResult{}, errors.New("submit: --composition-manifest is required")
	case cfg.ProjectID == "":
		return submitResult{}, errors.New("submit: --project is required")
	}

	spec, err := cfg.input("task", cfg.TaskPath)
	if err != nil {
		return submitResult{}, fmt.Errorf("submit: read specification: %w", err)
	}
	policyFile, err := cfg.input("policy", cfg.PolicyPath)
	if err != nil {
		return submitResult{}, fmt.Errorf("submit: read policy: %w", err)
	}
	publicationFile, err := cfg.input("publication", cfg.PublicationPath)
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
	if cfg.RunID == "" {
		publicationDigest = publicationFile.digest
	}

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
		workUnitFile, err := cfg.input("work-unit", cfg.WorkUnitPath)
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
			DeclaredPaths:       engine.DeclaredPathScope(keys),
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
		// Both the deliberate submission identity and every immutable run
		// binding participate. Only redelivery of that saved request converges.
		implementationRunID = engine.ManualSubmissionRunID(
			"cli:"+cfg.SubmissionID, cfg.ProjectID, spec.digest, policyDigest, publicationFile.digest, workUnitDigest)
	}
	var composition submissionFile
	if cfg.CompositionPath != "" {
		composition, err = cfg.input("composition", cfg.CompositionPath)
		if err != nil {
			return submitResult{}, fmt.Errorf("submit: read composition manifest: %w", err)
		}
		if err := validateSubmissionComposition(composition.body, implementationRunID, compositionIdentity{
			SubmissionID: cfg.SubmissionID,
			SourceDigest: spec.digest, PolicyDigest: policyDigest,
			PublicationDigest: publicationDigest, WorkUnitDigest: workUnitDigest,
		}); err != nil {
			return submitResult{}, fmt.Errorf("submit: composition manifest: %w", err)
		}
	}
	specificationRunID, err := engine.SpecificationRunIDForImplementation(implementationRunID)
	if err != nil {
		return submitResult{}, fmt.Errorf("submit: %w", err)
	}
	campaignID, err := engine.ProductionCampaignIDForImplementation(implementationRunID)
	if err != nil {
		return submitResult{}, fmt.Errorf("submit: %w", err)
	}
	resolvedPolicy, err := domain.NewResolvedPolicy(specificationRunID, keys)
	if err != nil {
		return submitResult{}, fmt.Errorf("submit: validate resolved policy: %w", err)
	}
	// The declared-path boundary is what the runner enforces, and it is
	// refused at start when it is absent or not an explicit allowlist. Refuse
	// it here instead: submission is the operator's door and can still say
	// no, while a run durable without one is a task the daemon holds
	// with no configuration change that could ever release it.
	if err := engine.SubmittedPathBoundary(resolvedPolicy); err != nil {
		return submitResult{}, fmt.Errorf("submit: %w", err)
	}
	policyBody, err := json.Marshal(resolvedPolicy.Keys)
	if err != nil {
		return submitResult{}, fmt.Errorf("submit: encode resolved policy keys: %w", err)
	}
	policy := submissionFile{digest: resolvedPolicy.Digest, body: policyBody}
	if cfg.RunID == "" {
		if _, err := specify.ParsePolicy(resolvedPolicy); err != nil {
			return submitResult{}, fmt.Errorf("submit: validate specification policy: %w", err)
		}
		if workUnit != nil {
			if _, err := domain.NewWorkUnitDeclaration(*workUnit, implementationRunID, cfg.ProjectID, time.Unix(1, 0)); err != nil {
				return submitResult{}, fmt.Errorf("submit: validate work-unit declaration: %w", err)
			}
		}
	}
	retained, err := retainSubmission(cfg)
	if err != nil {
		return submitResult{}, err
	}
	if !reflect.DeepEqual(retained.SavedInputs, cfg.SavedInputs) {
		// Equivalent redelivery can carry different JSON bytes. Derive all
		// digests from the first journal, including if another process won
		// publication while this request was being validated.
		return runSubmitCommand(ctx, submitCommandConfig{DBPath: cfg.DBPath, RetrySubmissionID: cfg.SubmissionID})
	}

	st, _, err := openStoreWithTopicKey(ctx, cfg.DBPath, store.Options{})
	if err != nil {
		return submitResult{}, fmt.Errorf("submit: open store: %w", err)
	}
	defer func() { _ = st.Close() }()
	// A database written before the rename holds this task's intake
	// state under the legacy specification identity; converge on it instead
	// of minting a second specification run for the same implementation.
	if resolved, err := engine.ResolveSpecificationRunID(ctx, st, implementationRunID); err != nil {
		return submitResult{}, fmt.Errorf("submit: %w", err)
	} else if resolved != specificationRunID {
		specificationRunID = resolved
		if resolvedPolicy, err = domain.NewResolvedPolicy(specificationRunID, keys); err != nil {
			return submitResult{}, fmt.Errorf("submit: validate resolved policy: %w", err)
		}
	}
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

	specArtifact, err := engine.SubmissionArtifact(
		domain.ArtifactKindSpecification, spec.digest, domain.EvidenceMediaTextMarkdown, int64(len(spec.body)))
	if err != nil {
		return submitResult{}, fmt.Errorf("submit: %w", err)
	}
	policyArtifact, err := engine.SubmissionArtifact(
		domain.ArtifactKindPolicy, policy.digest, domain.EvidenceMediaApplicationJSON, int64(len(policy.body)))
	if err != nil {
		return submitResult{}, fmt.Errorf("submit: %w", err)
	}
	specificationStatePresent, err := engine.HasSpecificationIntakeState(
		ctx, st, specificationRunID, implementationRunID,
	)
	if err != nil {
		return submitResult{}, fmt.Errorf("submit: inspect specification intake: %w", err)
	}
	if cfg.RunID != "" && !specificationStatePresent {
		legacy, found, err := legacyProductionReplay(ctx, st, implementationRunID, cfg.ProjectID,
			specArtifact, policyArtifact, keys, publication, workUnit, publicationDigest)
		if err != nil {
			return submitResult{}, fmt.Errorf("submit: inspect legacy production replay: %w", err)
		}
		if found {
			return legacy, nil
		}
		return submitResult{}, fmt.Errorf("submit: legacy run was not recorded: %w", store.ErrNotFound)
	}

	var manual *domain.ManualSubmission
	if cfg.RunID == "" {
		fingerprint, err := json.Marshal(struct {
			Project                                            domain.ProjectID
			Source, Policy, Publication, WorkUnit, Composition domain.Digest
		}{cfg.ProjectID, spec.digest, policyDigest, publicationFile.digest, workUnitDigest, composition.digest})
		if err != nil {
			return submitResult{}, err
		}
		manual = &domain.ManualSubmission{
			Identity: "cli:" + cfg.SubmissionID, ProjectID: cfg.ProjectID, SourceArtifactID: specArtifact.ID,
			SourceDigest: spec.digest, RequestDigest: domain.Digest(contentaddr.Sum(fingerprint)), ImplementationRunID: implementationRunID,
		}
	}
	var submitted engine.SpecificationRun
	lookupComplete := errors.New("submission lookup complete")
	err = st.Write(ctx, func(tx *store.WriteTx) error {
		replay := cfg.RunID != ""
		if manual != nil {
			if original, err := tx.GetManualSubmission(ctx, manual.Identity); err == nil {
				if original != *manual {
					return store.ErrImmutableConflict
				}
				replay = true
			} else if !errors.Is(err, store.ErrNotFound) {
				return err
			}
		}
		if cfg.RunID == "" {
			if err := engine.RegisterSubmissionArtifact(ctx, tx, specArtifact); err != nil {
				return err
			}
			if err := engine.RegisterSubmissionArtifact(ctx, tx, policyArtifact); err != nil {
				return err
			}
		}
		var submitErr error
		submitted, submitErr = engine.SubmitSpecificationRunTx(ctx, tx, engine.SpecificationRunSpec{
			ManualSubmission:   manual,
			SpecificationRunID: specificationRunID, ImplementationRunID: implementationRunID,
			ProjectID: cfg.ProjectID, SourceArtifactID: specArtifact.ID, SourceBytes: spec.body,
			PolicyArtifactID: policyArtifact.ID, ResolvedPolicy: resolvedPolicy, Publication: publication,
			PublicationDigest: publicationDigest,
			WorkUnit:          workUnit, CampaignID: campaignID, AttemptNumber: 1,
		})
		if submitErr == nil && replay {
			return lookupComplete // A replay never commits a write.
		}
		return submitErr
	})
	if err != nil && !errors.Is(err, lookupComplete) {
		return submitResult{}, fmt.Errorf("submit: %w", err)
	}
	result := submitResult{
		SubmissionID: cfg.SubmissionID,
		RunID:        submitted.ImplementationRunID, SpecificationRunID: submitted.Run.ID,
		ProjectID: submitted.Run.ProjectID,
		// Keep the original fields as implementation aliases for compatibility
		// while exposing both lanes without ambiguity.
		InvocationID:                  submitted.ImplementationInvocationID,
		StageID:                       submitted.ImplementationStageID,
		ImplementationRunID:           submitted.ImplementationRunID,
		ImplementationInvocationID:    submitted.ImplementationInvocationID,
		ImplementationStageID:         submitted.ImplementationStageID,
		SpecificationInvocationID:     submitted.SpecificationInvocationID,
		SpecificationStageID:          submitted.SpecificationStageID,
		SourceDigest:                  spec.digest,
		SpecificationPolicyDigest:     submitted.Run.PolicyDigest,
		SourceArtifactID:              specArtifact.ID,
		SpecificationPolicyArtifactID: policyArtifact.ID,
		PublicationDigest:             publicationDigest,
		CompositionDigest:             composition.digest,
		CampaignID:                    submitted.Run.CampaignID,
		AttemptNumber:                 submitted.Run.AttemptNumber,
	}
	if workUnit != nil {
		result.WorkUnitID = domain.WorkUnitIDForRun(submitted.ImplementationRunID)
	}
	return result, nil
}

// validateSubmissionComposition binds a passing preflight manifest to the
// exact inputs being submitted: a manifest for different inputs, a different
// derived run identity, or a non-passing status refuses the submission.
func validateSubmissionComposition(
	body []byte, runID domain.RunID, identity compositionIdentity,
) error {
	if err := ward.RejectDuplicateJSONKeys(body); err != nil {
		return err
	}
	var manifest compositionManifest
	if err := strictjson.Decode(
		body, &manifest, strictjson.RejectInvalidUTF8, strictjson.Limit(maxSubmissionFileBytes),
	); err != nil {
		return err
	}
	wantInvocation := domain.InvocationID("inv-implement-" + string(runID))
	if manifest.Version != compositionManifestVersion || manifest.Status != compositionPassed ||
		manifest.Identity.SubmissionID != identity.SubmissionID ||
		manifest.Identity.SourceDigest != identity.SourceDigest ||
		manifest.Identity.PolicyDigest != identity.PolicyDigest ||
		manifest.Identity.PublicationDigest != identity.PublicationDigest ||
		manifest.Identity.WorkUnitDigest != identity.WorkUnitDigest ||
		manifest.Identity.ImplementationRunID != runID ||
		manifest.Identity.ImplementationInvocationID != wantInvocation {
		return fmt.Errorf("passing manifest does not bind the submitted inputs, run %q, and invocation %q: %w",
			runID, wantInvocation, domain.ErrParentKeyMismatch)
	}
	return nil
}

// legacyProductionReplay preserves exact retries from the production-only
// submit protocol that preceded specification. A matching legacy run remains
// authoritative instead of being retrofitted with a specification reservation:
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
