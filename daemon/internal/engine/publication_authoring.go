package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/gitrun"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

// productionAuthoringCheckpointKind marks the engine-private inbox entry that
// records the authoring answer for one reviewed candidate. Like
// productionVerificationCheckpoint it is an inbox row, not a domain type: recipe
// v2 needs no migration for it (issue #1419, settled item 3).
const productionAuthoringCheckpointKind = "production_authoring_checkpoint"

const productionAuthoringCheckpointVersion = "1"

// maxAuthorOutcomeInputBytes bounds the verification and review outcome text the
// engine feeds the author. Neither is digest-bound, so a bounded prefix is safe;
// the builder separately bounds the diff and issue body.
const maxAuthorOutcomeInputBytes = 16 << 10

// productionAuthoringCheckpoint is the once-per-candidate authoring answer. A
// reviewed candidate is one head SHA on one base SHA (§7); the key binds both,
// so a base advance or a new head gets its own author run and never renders an
// artifact written for a different candidate (issue #1419, settled item 2).
// Either ArtifactDigest names the stored artifact, or Fallback records that this
// candidate fell back to v1 so a retry never calls the author again.
type productionAuthoringCheckpoint struct {
	Version        string        `json:"version"`
	RunID          domain.RunID  `json:"run_id"`
	HeadSHA        string        `json:"head_sha"`
	BaseSHA        string        `json:"base_sha"`
	ArtifactDigest domain.Digest `json:"artifact_digest,omitempty"`
	Fallback       bool          `json:"fallback"`
	Reason         string        `json:"reason,omitempty"`
}

// productionAuthoringCheckpointKey keys the authoring answer by run, head, and
// base. Including the base SHA (not only the head, as the verification
// checkpoint key does) makes a base advance select a fresh key, so the next
// clean review gets its own author run with fresh inputs.
func productionAuthoringCheckpointKey(runID domain.RunID, headSHA, baseSHA string) string {
	return "production-authoring/" + string(runID) + "/" + headSHA + "/" + baseSHA
}

// reconcilePublicationAuthoring runs the publication-author role once per
// reviewed candidate, after the final clean review and before the candidate body
// is composed. It is idempotent: an existing checkpoint (success or fallback)
// stops it, so retry, restart, and drift repair never call the author again. It
// never blocks publication: an unavailable author, a build fault, or a screen
// failure records a durable v1 fallback rather than failing the pass. Only a
// store fault the daemon should retry propagates as an error.
func (w *productionPublicationWorkflow) reconcilePublicationAuthoring(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
	checkpoint productionVerificationCheckpoint,
	reviewInstructions exec.ReviewInstructionBinding,
	checkoutDir string,
	report []byte,
) error {
	if task.Publication.Recipe != clientPublicationRecipeV2 {
		return nil
	}
	key := productionAuthoringCheckpointKey(task.RunID, task.HeadSHA, binding.admission.Base.BaseSHA)
	base := productionAuthoringCheckpoint{
		Version: productionAuthoringCheckpointVersion, RunID: task.RunID,
		HeadSHA: task.HeadSHA, BaseSHA: binding.admission.Base.BaseSHA,
	}
	if _, found, err := w.loadAuthoringCheckpoint(ctx, key, base); err != nil {
		return err
	} else if found {
		return nil
	}
	if w.inference == nil || !w.inference.SupportsSite(inference.PublicationAuthorExplainSiteID) {
		return w.recordAuthoringFallback(ctx, key, base, "publication author site unavailable")
	}
	input, err := w.buildPublicationAuthorInput(ctx, task, binding, checkpoint, reviewInstructions, checkoutDir, report)
	if err != nil {
		return w.recordAuthoringFallback(ctx, key, base, "resolve author input: "+err.Error())
	}
	authored, err := w.inference.AuthorPublication(ctx, input)
	if err != nil {
		// An error paired with a non-fallback result is a hard inference fault;
		// treat it as a fallback so the author role never blocks publication.
		return w.recordAuthoringFallback(ctx, key, base, "author call failed: "+err.Error())
	}
	if authored.Fallback {
		return w.recordAuthoringFallback(ctx, key, base, "author returned a fallback")
	}
	artifact, err := domain.NewPublicationAuthoring(domain.PublicationAuthoringInput{
		RunID: task.RunID, Title: authored.Title, Body: authored.Body,
		ReviewerNotes: authored.ReviewerNotes, EvidenceRefs: authored.EvidenceRefs,
		OutcomeSummary: authored.OutcomeSummary,
		Producer: domain.PublicationProducer{
			Site:        inference.PublicationAuthorExplainSiteID,
			Producer:    authored.Producer,
			InputDigest: domain.Digest(authored.InputDigest),
		},
		SensitivityClass: authored.TargetClass,
		CreatedAt:        w.attentionCreatedAt(),
	})
	if err != nil {
		return w.recordAuthoringFallback(ctx, key, base, "build authored artifact: "+err.Error())
	}
	// Screen the free-text fields before storing or rendering: v2 is at least as
	// strict as v1, and a refused artifact falls back rather than reaching a PR.
	if err := screenAuthoredText(artifact); err != nil {
		return w.recordAuthoringFallback(ctx, key, base, "authored artifact screen failed: "+err.Error())
	}
	// The title is model-controlled and neither the explain-site schema, the
	// artifact constructor, nor the screen enforces the single-line title contract
	// literal and v1 publication titles hold. Enforce it before storing so a
	// multiline or padded title never becomes malformed forge metadata.
	if !validAuthoredPublicationTitle(artifact.Title) {
		return w.recordAuthoringFallback(ctx, key, base, "authored title is not one non-empty trimmed line")
	}
	success := base
	success.ArtifactDigest = artifact.Digest
	return w.storeAuthoredArtifact(ctx, key, success, artifact)
}

// buildPublicationAuthorInput resolves the author input from real repository
// state. #1418 put the input checks in the builder; this only resolves the
// values. The instruction snapshot and PR template come from the trusted base,
// never the candidate head, so a candidate edit cannot change them.
func (w *productionPublicationWorkflow) buildPublicationAuthorInput(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
	checkpoint productionVerificationCheckpoint,
	reviewInstructions exec.ReviewInstructionBinding,
	checkoutDir string,
	report []byte,
) (inference.PublicationAuthorInput, error) {
	class, err := w.publisher.RepositoryClass(ctx, publish.Candidate{Repo: binding.admission.Base.Repo})
	if err != nil {
		return inference.PublicationAuthorInput{}, fmt.Errorf("read target visibility: %w", err)
	}
	visibility := repositoryVisibilityForClass(class)

	instructions, err := w.readArtifactText(reviewInstructions.ResultDigest)
	if err != nil {
		return inference.PublicationAuthorInput{}, fmt.Errorf("read instruction snapshot: %w", err)
	}
	diff, err := authoredCandidateDiff(ctx, w.workDir, checkoutDir, binding.admission.Base.BaseSHA, task.HeadSHA)
	if err != nil {
		return inference.PublicationAuthorInput{}, fmt.Errorf("render candidate diff: %w", err)
	}

	reviewOutcome, err := w.authoredReviewOutcome(ctx, task.RunID)
	if err != nil {
		return inference.PublicationAuthorInput{}, fmt.Errorf("summarize review outcome: %w", err)
	}

	baseSHA := binding.admission.Base.BaseSHA
	return inference.PublicationAuthorInput{
		Project:     string(task.ProjectID),
		RootLineage: string(task.RunID),

		TargetRepository: binding.admission.Base.Repo,
		TargetVisibility: visibility,

		SourceIssueRef: task.Publication.SourceIssue,
		// The source is the same repository as the target for a client
		// submission, so its visibility matches. Part D resolves cross-repository
		// sources; here the source issue prose stays empty.
		SourceVisibility: visibility,

		Diff:                diff,
		VerificationOutcome: boundValidText(string(report), maxAuthorOutcomeInputBytes),
		ReviewOutcome:       reviewOutcome,

		PRTemplate:          trustedControlFile(readPRTemplate(ctx, w.workDir, checkoutDir, baseSHA), baseSHA),
		InstructionSnapshot: trustedControlFile(instructions, baseSHA),

		Evidence:        checkpoint.Artifacts,
		ApprovedRecipes: w.approvedRecipes,
	}, nil
}

// authoredReviewOutcome summarizes the run's latest clean review for the author.
func (w *productionPublicationWorkflow) authoredReviewOutcome(
	ctx context.Context, runID domain.RunID,
) (string, error) {
	record, _, err := w.latestReviewState(ctx, runID)
	if err != nil {
		return "", err
	}
	if record == nil {
		return "The routed review returned a clean pass.", nil
	}
	return fmt.Sprintf(
		"Round %d %s review by %s/%s completed at %s.",
		record.Round, record.Outcome, record.Provider, record.ModelConfiguration,
		record.CompletedAt.UTC().Format(time.RFC3339),
	), nil
}

// publicationTitleBody renders the PR title and body for a task's publication
// record. A v1 or literal record renders as today. A v2 record renders the
// stored authored artifact when its checkpoint names a digest, the artifact and
// its evidence gate still hold, and the repository is no more open than the
// stored class allows; every other case (no checkpoint, a recorded fallback, or
// a store or gate failure) renders v1 and never blocks. Every publication path
// routes through here, so for a given candidate, while its stored artifact and
// the repository's visibility are unchanged, retry, restart, and drift repair
// replay identical bytes.
func (w *productionPublicationWorkflow) publicationTitleBody(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
	checkpoint productionVerificationCheckpoint,
) (string, string, error) {
	renderV1 := func() (string, string, error) {
		return publicationMetadata(task.Publication, task.ProducingInvocationID, checkpoint.Imported.Claims)
	}
	if task.Publication.Recipe != clientPublicationRecipeV2 {
		return renderV1()
	}
	key := productionAuthoringCheckpointKey(task.RunID, task.HeadSHA, binding.admission.Base.BaseSHA)
	want := productionAuthoringCheckpoint{
		Version: productionAuthoringCheckpointVersion, RunID: task.RunID,
		HeadSHA: task.HeadSHA, BaseSHA: binding.admission.Base.BaseSHA,
	}
	authoring, found, err := w.loadAuthoringCheckpoint(ctx, key, want)
	if err != nil || !found || authoring.Fallback || authoring.ArtifactDigest == "" {
		return renderV1()
	}
	if title, body, ok := w.renderStoredAuthoring(ctx, task, binding, authoring.ArtifactDigest); ok {
		return title, body, nil
	}
	return renderV1()
}

// renderStoredAuthoring reads the stored artifact, re-runs its evidence gate,
// and renders it when the stored class still covers the repository's current
// visibility. It returns ok=false (render v1) on a store or gate failure, a
// run-ID mismatch, an affirmatively observed opening past the stored class, or
// a rendered body that would not fit the candidate budget. A transient
// visibility read failure keeps the stored class and renders the stored
// artifact, so it cannot flip an already published v2 PR to v1.
func (w *productionPublicationWorkflow) renderStoredAuthoring(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
	digest domain.Digest,
) (string, string, bool) {
	var artifact domain.PublicationAuthoring
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		artifact, err = tx.GetPublicationAuthoring(ctx, digest)
		return err
	}); err != nil {
		return "", "", false
	}
	if artifact.RunID != task.RunID {
		return "", "", false
	}
	// Re-screen and re-check the title on every render. A stored artifact is
	// reconstructed at this boundary, and the screening policy can tighten between
	// authoring and a later replay (retry, restart, or drift repair). The store
	// re-runs the evidence and class gates but not the text screen, so a newly
	// banned secret pattern, directive, or reserved section in stored prose, or a
	// title that no longer meets the single-line contract, must fall back to v1
	// rather than reach a public PR. The screen, not the class, is the leak guard.
	if screenAuthoredText(artifact) != nil || !validAuthoredPublicationTitle(artifact.Title) {
		return "", "", false
	}
	class, err := w.publisher.RepositoryClass(ctx, publish.Candidate{Repo: binding.admission.Base.Repo})
	if err != nil {
		// The visibility read failed, so the repository's current openness is
		// unknown. The sensitivity class, not the screen, is the load-bearing
		// control for content sensitivity: the screen is syntactic and cannot
		// recognize confidential context the model paraphrased, so it cannot make
		// sensitive-authored prose safe on a repository that may now be more open.
		if artifact.SensitivityClass != domain.SensitivityNormal {
			// Fail closed to v1: a first publish after a private->public transition
			// must never put private-authored prose on a now-public PR when the
			// current visibility cannot be confirmed.
			return "", "", false
		}
		// Public-grade (normal) content is safe under any visibility, so keep the
		// stored class and replay byte-for-byte: a transient read failure cannot
		// flip an already published public v2 PR to v1.
		class = artifact.SensitivityClass
	}
	if artifact.SensitivityClass.MoreRestrictiveThan(class) {
		// The repository is now more open than the stored class allows: the
		// content was authored for a more restrictive audience, so fall back to
		// the deterministic v1 rendering (issue #1419, settled item 9).
		return "", "", false
	}
	title, body := renderAuthoredPublication(artifact)
	if err := publish.ValidateCandidateBody(body); err != nil {
		return "", "", false
	}
	return title, body, true
}

// loadAuthoringCheckpoint reads the authoring answer for a candidate key and
// re-validates the decoded row against the candidate identity the key encodes.
// A stored row is untrusted at this reconstruction boundary: a restored or
// corrupted row under the current key could otherwise name an artifact from
// another head or base in the same run, or carry a forged fallback bit that
// permanently suppresses authoring. Every field mismatch fails closed with
// ErrParentKeyMismatch so the caller either retries (reconcile) or renders v1
// (publicationTitleBody) rather than trusting stale prose or a spoofed state.
func (w *productionPublicationWorkflow) loadAuthoringCheckpoint(
	ctx context.Context, key string, want productionAuthoringCheckpoint,
) (productionAuthoringCheckpoint, bool, error) {
	var checkpoint productionAuthoringCheckpoint
	var found bool
	err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		entry, err := tx.GetInbox(ctx, key)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if entry.Kind != productionAuthoringCheckpointKind {
			return fmt.Errorf("authoring checkpoint kind %q: %w", entry.Kind, domain.ErrParentKeyMismatch)
		}
		if err := strictjson.Decode(
			entry.Payload, &checkpoint, strictjson.TolerateInvalidUTF8, strictjson.NoLimit,
		); err != nil {
			return fmt.Errorf("decode authoring checkpoint: %w", errors.Join(err, domain.ErrParentKeyMismatch))
		}
		if err := validateAuthoringCheckpoint(checkpoint, want); err != nil {
			return err
		}
		found = true
		return nil
	})
	if err != nil {
		return productionAuthoringCheckpoint{}, false, err
	}
	return checkpoint, found, nil
}

// validateAuthoringCheckpoint fails closed unless the decoded checkpoint matches
// the expected candidate identity and holds the fallback/artifact invariant. A
// valid row is exactly one of: a fallback (Fallback set, no digest) or a success
// (digest set, Fallback clear); Fallback == (ArtifactDigest == "") captures both.
func validateAuthoringCheckpoint(got, want productionAuthoringCheckpoint) error {
	switch {
	case got.Version != productionAuthoringCheckpointVersion:
		return fmt.Errorf("authoring checkpoint version %q: %w", got.Version, domain.ErrParentKeyMismatch)
	case got.RunID != want.RunID:
		return fmt.Errorf("authoring checkpoint run %q: %w", got.RunID, domain.ErrParentKeyMismatch)
	case got.HeadSHA != want.HeadSHA:
		return fmt.Errorf("authoring checkpoint head %q: %w", got.HeadSHA, domain.ErrParentKeyMismatch)
	case got.BaseSHA != want.BaseSHA:
		return fmt.Errorf("authoring checkpoint base %q: %w", got.BaseSHA, domain.ErrParentKeyMismatch)
	case got.Fallback != (got.ArtifactDigest == ""):
		return fmt.Errorf("authoring checkpoint fallback/artifact state inconsistent: %w",
			domain.ErrParentKeyMismatch)
	}
	return nil
}

// recordAuthoringFallback durably records that a candidate fell back to v1, so a
// retry renders v1 without calling the author again (issue #1419, settled item
// 3). A store fault here is retryable, so it propagates.
func (w *productionPublicationWorkflow) recordAuthoringFallback(
	ctx context.Context, key string, checkpoint productionAuthoringCheckpoint, reason string,
) error {
	checkpoint.Fallback = true
	checkpoint.Reason = reason
	return w.putAuthoringCheckpoint(ctx, key, checkpoint)
}

// storeAuthoredArtifact writes the authored artifact and its success checkpoint
// in one transaction, so a crash between them cannot leave a checkpoint that
// names an unstored artifact.
func (w *productionPublicationWorkflow) storeAuthoredArtifact(
	ctx context.Context, key string, checkpoint productionAuthoringCheckpoint, artifact domain.PublicationAuthoring,
) error {
	payload, err := json.Marshal(checkpoint)
	if err != nil {
		return err
	}
	return w.store.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutPublicationAuthoring(ctx, artifact); err != nil {
			return err
		}
		return recordAuthoringInbox(ctx, tx, key, payload)
	})
}

func (w *productionPublicationWorkflow) putAuthoringCheckpoint(
	ctx context.Context, key string, checkpoint productionAuthoringCheckpoint,
) error {
	payload, err := json.Marshal(checkpoint)
	if err != nil {
		return err
	}
	return w.store.Write(ctx, func(tx *store.WriteTx) error {
		return recordAuthoringInbox(ctx, tx, key, payload)
	})
}

// recordAuthoringInbox records the checkpoint row and confirms it converged
// byte-for-byte, the same immutability guard persistCheckpoint applies.
func recordAuthoringInbox(ctx context.Context, tx *store.WriteTx, key string, payload []byte) error {
	entry, _, err := tx.RecordInbox(ctx, key, productionAuthoringCheckpointKind, payload)
	if err != nil {
		return err
	}
	if entry.Kind != productionAuthoringCheckpointKind || !bytes.Equal(entry.Payload, payload) {
		return fmt.Errorf("production authoring checkpoint disagrees with stored row: %w",
			domain.ErrImmutableTransition)
	}
	return nil
}

// readArtifactText reads a stored artifact's bytes as a string. An empty digest
// yields empty content (no instruction snapshot resolved). The bytes are
// re-hashed and verified against the requested digest: the store is not
// guaranteed to return content that matches its key (a corrupted blob or an
// incorrectly restored row), so an unverified read at this reconstruction
// boundary could substitute trusted control-plane text. A mismatch is an error,
// which the author input builder turns into a v1 fallback.
func (w *productionPublicationWorkflow) readArtifactText(digest domain.Digest) (string, error) {
	if digest == "" {
		return "", nil
	}
	rc, err := w.artifacts.Open(digest)
	if err != nil {
		return "", err
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(rc)
	if err != nil {
		return "", err
	}
	if got := domain.Digest(contentaddr.Sum(data)); got != digest {
		return "", fmt.Errorf("artifact %q bytes hash to %q: %w", digest, got, domain.ErrParentKeyMismatch)
	}
	return string(data), nil
}

// repositoryVisibilityForClass maps the derived sensitivity class to the
// author-input visibility enum: normal is public, anything more restrictive is
// private.
func repositoryVisibilityForClass(class domain.SensitivityClass) inference.RepositoryVisibility {
	if class == domain.SensitivityNormal {
		return inference.RepositoryPublic
	}
	return inference.RepositoryPrivate
}

// validAuthoredPublicationTitle enforces the single-line title contract literal
// and v1 publication titles hold (production_workflow.go): a non-empty, trimmed
// line with no embedded carriage return or newline. The authored title is
// model-controlled, so v2 applies the same contract before storing and again on
// render, failing closed to v1 on violation.
func validAuthoredPublicationTitle(title string) bool {
	return title != "" && title == strings.TrimSpace(title) && !strings.ContainsAny(title, "\r\n")
}

// trustedControlFile wraps trusted-base content as a digest-consistent control
// file. Empty content is valid: it names a base commit and its own empty-content
// digest, which the builder's check accepts.
func trustedControlFile(content, baseSHA string) inference.ControlFile {
	return inference.ControlFile{
		Content:           content,
		Digest:            contentaddr.Sum([]byte(content)),
		TrustedBaseCommit: baseSHA,
	}
}

// readPRTemplate reads the repository's pull-request template from the trusted
// base commit's objects, best effort. The production FetchBase checkout has no
// working tree (transport.go: HEAD detached at the base, no working-tree
// content), so the template must be read from the base commit's tree rather
// than the filesystem; reading a worktree path would always return empty in
// production and only appear to work behind a materialized test transport. A
// missing template yields empty content, which is a valid control file. Only
// the conventional locations are consulted.
func readPRTemplate(ctx context.Context, workDir, checkoutDir, baseSHA string) string {
	if checkoutDir == "" || baseSHA == "" {
		return ""
	}
	scratch, err := os.MkdirTemp(workDir, ".author-template-")
	if err != nil {
		return ""
	}
	defer os.RemoveAll(scratch) //nolint:errcheck // daemon-owned scratch
	runner, err := gitrun.New(gitrun.Options{Scratch: scratch})
	if err != nil {
		return ""
	}
	if _, err := runner.PinCheckout(ctx, checkoutDir); err != nil {
		return ""
	}
	for _, rel := range []string{
		".github/PULL_REQUEST_TEMPLATE.md",
		".github/pull_request_template.md",
		"PULL_REQUEST_TEMPLATE.md",
		"pull_request_template.md",
		"docs/PULL_REQUEST_TEMPLATE.md",
		"docs/pull_request_template.md",
	} {
		// cat-file blob resolves the path within the base tree and errors if the
		// object is missing or not a blob, so a missing template or a same-named
		// directory is skipped rather than mistaken for content.
		data, err := runner.Run(ctx, nil, "cat-file", "blob", baseSHA+":"+rel)
		if err == nil && utf8.Valid(data) {
			return string(data)
		}
	}
	return ""
}

// authoredCandidateDiff renders the candidate's raw unified diff over the
// trusted base-to-head pair from the daemon-owned checkout. The builder bounds
// it, so the full diff is passed.
func authoredCandidateDiff(ctx context.Context, workDir, candidateRoot, baseSHA, headSHA string) (string, error) {
	scratch, err := os.MkdirTemp(workDir, ".author-diff-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(scratch) //nolint:errcheck // daemon-owned scratch
	runner, err := gitrun.New(gitrun.Options{Scratch: scratch})
	if err != nil {
		return "", err
	}
	if _, err := runner.PinCheckout(ctx, candidateRoot); err != nil {
		return "", fmt.Errorf("bind author-diff candidate checkout: %w", err)
	}
	diff, err := runner.Run(
		ctx, nil, "diff", "--no-color", "--no-ext-diff", "--no-textconv", baseSHA, headSHA, "--",
	)
	if err != nil {
		return "", fmt.Errorf("render author diff: %w", err)
	}
	return string(diff), nil
}

// boundValidText returns a valid-UTF-8 prefix of s no longer than max bytes,
// cut on a rune boundary. It is used for the non-digest-bound outcome inputs.
func boundValidText(s string, max int) string {
	s = strings.ToValidUTF8(s, "")
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
