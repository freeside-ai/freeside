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

// freesided submit (plan §5.12, §10): registers the operator-approved
// specification and resolved policy as digest-addressed artifacts and creates
// the production run the engine dispatches unattended. Registration only:
// the unattended preconditions (operating state, waiver, conformance,
// health) stay with the dispatch gates, so a submission is durable intent
// even while the daemon is stopped or held.

// maxSubmissionFileBytes bounds one submitted input file. Specifications and
// policies are prose and configuration; a larger file is far more likely a
// mistaken path than a real work item.
const maxSubmissionFileBytes = 4 << 20

// runSubmitMain parses the submit verb's flags and runs the command,
// printing one JSON result line on success. Exit contract: 0 converged,
// 1 refused, 2 flag misuse.
func runSubmitMain(args []string) {
	flags := flag.NewFlagSet("freesided submit", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	dbPath := flags.String("db", "", "SQLite database path (required)")
	specPath := flags.String("spec", "", "operator-approved specification file (required)")
	policyPath := flags.String("policy", "", "resolved per-run policy-key JSON array (required)")
	publicationPath := flags.String("publication", "", "reviewer-facing pull-request metadata JSON file (required)")
	workUnitPath := flags.String("work-unit", "", "work-unit declaration JSON file (optional; §5.18 capture)")
	projectID := flags.String("project", "", "project id the run belongs to (required)")
	runID := flags.String("run-id", "", "run id (defaults from project, specification, resolved policy, publication metadata, and any work-unit declaration so an exact re-submission converges)")
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
	RunID             domain.RunID        `json:"run_id"`
	ProjectID         domain.ProjectID    `json:"project_id"`
	InvocationID      domain.InvocationID `json:"invocation_id"`
	StageID           domain.StageID      `json:"stage_id"`
	SpecDigest        domain.Digest       `json:"spec_digest"`
	PolicyDigest      domain.Digest       `json:"policy_digest"`
	SpecArtifactID    domain.ArtifactID   `json:"spec_artifact_id"`
	PolicyArtifactID  domain.ArtifactID   `json:"policy_artifact_id"`
	PublicationDigest domain.Digest       `json:"publication_digest"`
	WorkUnitID        domain.WorkUnitID   `json:"work_unit_id,omitempty"`
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

	runID := cfg.RunID
	if runID == "" {
		// The default covers every immutable run binding so only an exact
		// resubmission converges; shared specification bytes in another
		// project, under another policy, with different reviewer-facing
		// metadata, or under a different work-unit declaration remain
		// distinct work items. An undeclared submission keeps the
		// pre-capture derivation byte-for-byte.
		runID = defaultSubmissionRunID(
			cfg.ProjectID, spec.digest, policyDigest, publicationFile.digest, workUnitDigest)
	}
	resolvedPolicy, err := domain.NewResolvedPolicy(runID, keys)
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

	_, statErr := os.Stat(cfg.DBPath)
	storePreexisting := statErr == nil
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return submitResult{}, fmt.Errorf("submit: inspect store: %w", statErr)
	}
	if _, err := loadOrCreateTopicKey(cfg.DBPath, storePreexisting); err != nil {
		return submitResult{}, fmt.Errorf("submit: initialize topic key: %w", err)
	}
	st, err := store.Open(ctx, cfg.DBPath, store.Options{})
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

	specArtifact, err := submissionArtifact("specification", spec.digest)
	if err != nil {
		return submitResult{}, fmt.Errorf("submit: %w", err)
	}
	policyArtifact, err := submissionArtifact("policy", policy.digest)
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

	submitted, err := engine.SubmitProductionRun(ctx, st, engine.ProductionRunSpec{
		RunID: runID, ProjectID: cfg.ProjectID,
		SpecArtifactID: specArtifact.ID, PolicyArtifactID: policyArtifact.ID,
		ResolvedPolicy: resolvedPolicy, Publication: publication,
		WorkUnit: workUnit,
	})
	if err != nil {
		return submitResult{}, fmt.Errorf("submit: %w", err)
	}
	result := submitResult{
		RunID: submitted.Run.ID, ProjectID: submitted.Run.ProjectID,
		InvocationID: submitted.InvocationID, StageID: submitted.StageID,
		SpecDigest: submitted.Run.SpecDigest, PolicyDigest: submitted.Run.PolicyDigest,
		SpecArtifactID: specArtifact.ID, PolicyArtifactID: policyArtifact.ID,
		PublicationDigest: publicationFile.digest,
	}
	if workUnit != nil {
		result.WorkUnitID = domain.WorkUnitIDForRun(submitted.Run.ID)
	}
	return result, nil
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
func submissionArtifact(role string, digest domain.Digest) (domain.Artifact, error) {
	hexDigits := string(digest[len("sha256:"):])
	return domain.NewArtifact(domain.ArtifactInput{
		ID:     domain.ArtifactID("artifact-" + role + "-" + hexDigits),
		Type:   role,
		Digest: digest,
		Provenance: domain.Provenance{
			ProducerClass:        domain.ProducerDaemon,
			ProducerInvocationID: domain.InvocationID("submit-" + role + "-" + hexDigits),
			HeadBinding:          domain.HeadIndependent,
			SensitivityClass:     domain.SensitivityNormal,
		},
	}, nil)
}
