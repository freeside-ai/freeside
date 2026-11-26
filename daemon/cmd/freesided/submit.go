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
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/admissionauth"
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
// For new work, the daemon overlays the Workflow settings pane's keys from
// the store's layers onto the -policy file's keys before storing the run's
// policy; the run identity stays derived from the file's keys.
// Registration only: execution preconditions stay with the dispatch gates,
// so a submission is durable intent even while the daemon is stopped or held.

// maxSubmissionFileBytes bounds one submitted input file. Specifications and
// policies are prose and configuration; a larger file is far more likely a
// mistaken path than a real task.
const maxSubmissionFileBytes = 4 << 20

const submitResultHelp = `
Result JSON fields by lane:
  source submission: source_digest, source_artifact_id, publication_digest,
    and one of source_issue or source_reference_omitted
  specification: specification_run_id, specification_invocation_id, specification_stage_id,
    specification_policy_digest, specification_policy_artifact_id
  reserved implementation: implementation_run_id, implementation_invocation_id,
    implementation_stage_id, campaign_id, attempt_number
  shared: task_id, project_id, composition_digest

task_id is the input freesided inspect task -task takes.

The legacy fields run_id, invocation_id, stage_id, and work_unit_id are
compatibility aliases bound to the reserved implementation run. The former
spec_digest and spec_artifact_id fields are source_digest and
source_artifact_id; policy_digest and policy_artifact_id are
specification_policy_digest and specification_policy_artifact_id. No deprecated
digest or artifact aliases are emitted.

specification_policy_digest and specification_policy_artifact_id name the
stored policy (the -policy file with the Workflow settings pane's keys
overlaid from the store's layers), which can differ from the file's digest
used to derive the run identity.

source_issue is the issue the pull request will reference: the publication
file's source_issue, which must equal a task source that is exactly one GitHub
issue URL. source_reference_omitted gives the reason when the pull request
will carry no publisher-written issue reference.

replaced_task, present only with --replace, is the abandon result for the
replaced task: released, episode, workflow_closed, retired_items, and
cleanup_complete, as freesided abandon reports them.

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
	policyPath := flags.String("policy", "", "base per-run policy-key JSON array (required); keys the Workflow settings pane owns resolve from the store's layers for new work")
	publicationPath := flags.String("publication", "", "reviewer-facing pull-request metadata JSON file (required); "+
		"a task that is exactly one GitHub issue URL needs source_issue set to that URL")
	compositionPath := flags.String("composition-manifest", "", "passing production-composition manifest bound to the submitted inputs "+
		"(with --require-composition, defaults to composition-manifest.json beside -db, where deputy writes it)")
	requireComposition := flags.Bool("require-composition", false, "require trusted production-composition evidence (unattended submission)")
	workUnitPath := flags.String("work-unit", "", "work-unit declaration JSON file (required for new work; §5.18 capture); "+
		`the smallest is {"completion_criterion":"bound_pr_merged"}`)
	projectID := flags.String("project", "", "project id the run belongs to (required)")
	submissionID := flags.String("submission-id", "", "prepared identity for new work (otherwise generated and saved before submission)")
	retrySubmissionID := flags.String("retry-submission-id", "", "manually retry a saved submission using its original inputs")
	replaceTaskID := flags.String("replace", "", "abandon this task and submit the new one in the same write; the task must be "+
		"in -project, hold the submitted source, be unfinished, and have only execution failures open")
	var taskLines taskLineFlag
	flags.Var(&taskLines, "task-line", "choose what runs this task's role, as role=enrollment/model/effort "+
		"(repeatable; roles: "+taskLineRoleList()+"); a line that cannot be honored refuses the attempt and never "+
		"falls back to the lineup; a reviewer line must select what the review source runs, or the task's review is refused")
	var attachPaths attachFlag
	flags.Var(&attachPaths, "attach", "attach this file to the task under its base name (repeatable; files only, "+
		"an image at most 10 MiB, any other file at most 32 MiB, 100 MiB and 1,000 files per task); "+
		"the bytes go to the blob store beside -db, so run submit as the daemon's user with the daemon's database path")
	// Validated here, at the run-creation boundary, so a malformed, out-of-range,
	// or unsatisfiable writer budget fails before a durable run exists rather
	// than stranding one when the daemon later parses the same flag. The daemon
	// run consumes -writer-stop-timeout; submit only vets it against the harness
	// supervision deadline.
	writerStopTimeout := flags.Duration("writer-stop-timeout", 0,
		"implementation writer-container budget, vetted against -supervision-timeout before the run is created; 0 uses the ward default")
	supervisionTimeout := flags.Duration("supervision-timeout", 0,
		"harness workflow supervision deadline the writer budget must stay below; 0 skips the check")
	deputyStartWait := flags.Duration("deputy-start-wait", 15*time.Minute,
		"with --require-composition, how long to wait for a deputy that is still starting on -db to finish its "+
			"self-check and serve; 0 doesn't wait")
	if err := flags.Parse(args); err != nil {
		os.Exit(2)
	}
	if err := validateWriterStopBudget(*writerStopTimeout, *supervisionTimeout); err != nil {
		fmt.Fprintln(os.Stderr, "freesided:", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// The wait stays out of submitCommandConfig, whose JSON is the recovery
	// journal, so it changes no journal's bytes.
	if *requireComposition && *dbPath != "" {
		if err := waitForStartingDeputy(ctx, *dbPath, *deputyStartWait, os.Stderr); err != nil {
			fmt.Fprintln(os.Stderr, "freesided:", err)
			os.Exit(1)
		}
	}
	result, err := runSubmitCommand(ctx, submitCommandConfig{
		SubmissionID: *submissionID, RetrySubmissionID: *retrySubmissionID,
		DBPath: *dbPath, TaskPath: *taskPath, PolicyPath: *policyPath,
		PublicationPath: *publicationPath, WorkUnitPath: *workUnitPath,
		CompositionPath:    *compositionPath,
		RequireComposition: *requireComposition,
		ProjectID:          domain.ProjectID(*projectID),
		TaskLines:          taskLines, AttachPaths: attachPaths,
		ReplaceTaskID: domain.TaskID(*replaceTaskID),
	})
	if err != nil {
		// A replacement whose cleanup failed still created the new task, so
		// the result is printed beside the repair command.
		if result.TaskID != "" {
			_ = json.NewEncoder(os.Stdout).Encode(result)
		}
		fmt.Fprintln(os.Stderr, "freesided:", err)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		fmt.Fprintln(os.Stderr, "freesided:", err)
		os.Exit(1)
	}
}

// deputyStartPoll is how often submit looks again for a starting deputy's
// files.
const deputyStartPoll = 500 * time.Millisecond

// waitForStartingDeputy waits, up to bound, while a deputy on db is still
// starting: its rig acquisition exists, but its control address doesn't yet.
// A deputy start writes the acquisition first, then runs its self-check,
// which writes the composition manifest only when the agent tree matches,
// and publishes the control address once it serves, whatever the check
// found. So the wait ends when the address exists, and at once when no
// acquisition says a deputy is starting; submit then proceeds, or fails on a
// missing manifest, exactly as it would without the wait. It only delays
// the reads: submit validates the manifest as before. A bound of zero
// doesn't wait.
func waitForStartingDeputy(ctx context.Context, db string, bound time.Duration, stderr io.Writer) error {
	// The control address sits beside the canonical path, where the daemon
	// publishes it; a path that doesn't resolve is submit's to report.
	canonical, err := canonicalDatabasePath(db)
	if err != nil {
		canonical = db
	}
	acquisition := newDeputyPaths(filepath.Dir(canonical)).Acquisition
	ready := func() bool { return fileExists(canonical + ".control.json") }
	if bound <= 0 || ready() || !fileExists(acquisition) {
		return nil
	}
	_, _ = fmt.Fprintf(stderr, "freesided: waiting up to %s for the deputy on %s to finish starting\n", bound, db)
	timeout := time.NewTimer(bound)
	defer timeout.Stop()
	poll := time.NewTicker(deputyStartPoll)
	defer poll.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout.C:
			return fmt.Errorf("the deputy on %s is still starting after %s, or it stopped without removing %s; "+
				"rerun once it logs \"ready for submissions\"", db, bound, acquisition)
		case <-poll.C:
			// A deputy that stopped while starting removes its acquisition;
			// submit then reports what is missing.
			if ready() || !fileExists(acquisition) {
				return nil
			}
		}
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
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
	// TaskLines are the operator's per-role agent choices (plan §5.4), in
	// canonical role order once runSubmitCommand has checked them. The
	// recovery journal is this struct's JSON, so a manual retry replays the
	// saved choice; the tag omits the field when there are none, which keeps
	// a line-less journal the bytes it was before the field existed.
	TaskLines []domain.TaskLineChoice `json:",omitempty"`
	// AttachPaths are the --attach files. prepareSubmission reads and checks
	// them; the journal keeps only the references in Attachments.
	AttachPaths []string `json:"-"`
	// Attachments reference the --attach files by digest, in the given
	// order, so a manual retry resubmits the same list from the journal. The
	// tag omits the field when there are none, which keeps an
	// attachment-less journal the bytes it was before the field existed.
	Attachments []signet.TaskAttachmentInput `json:",omitempty"`
	// ReplaceTaskID is the --replace task, which the submission abandons in
	// the write that creates the new task. The journal keeps it so a manual
	// retry replays the choice; the tag omits it when unset, which keeps a
	// journal saved without the flag the bytes it was before the field
	// existed.
	ReplaceTaskID domain.TaskID `json:",omitempty"`
	// attachmentFiles hold the --attach bytes of a new submission until they
	// are stored, before the journal is written.
	attachmentFiles []attachmentFile
}

// taskLineFlag collects repeated --task-line role=enrollment/model/effort
// values. It checks only that form; runSubmitCommand validates the set. An
// enrollment id carries a slash of its own (<identity>/<client>), so the
// effort and the model are the last two segments and the enrollment is the
// rest; neither an effort nor a model id of a supported harness carries one.
type taskLineFlag []domain.TaskLineChoice

func (f *taskLineFlag) String() string { return "" }

func (f *taskLineFlag) Set(value string) error {
	role, selection, found := strings.Cut(value, "=")
	rest, effort, cutEffort := cutLast(selection, "/")
	enrollment, model, cutModel := cutLast(rest, "/")
	if !found || !cutEffort || !cutModel || role == "" || enrollment == "" || model == "" || effort == "" {
		return errors.New("want role=enrollment/model/effort")
	}
	*f = append(*f, domain.TaskLineChoice{Role: domain.RoleName(role), Selection: &domain.TaskLineSelection{
		EnrollmentID: domain.ClientEnrollmentID(enrollment), ModelID: model, Effort: effort,
	}})
	return nil
}

// cutLast is strings.Cut at the last sep.
func cutLast(s, sep string) (before, after string, found bool) {
	i := strings.LastIndex(s, sep)
	if i < 0 {
		return s, "", false
	}
	return s[:i], s[i+len(sep):], true
}

func taskLineRoleList() string {
	roles := make([]string, len(domain.TaskLineRoles))
	for i, role := range domain.TaskLineRoles {
		roles[i] = string(role)
	}
	return strings.Join(roles, ", ")
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
	TaskID                        domain.TaskID       `json:"task_id"`
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
	// A submit result sets exactly one of the two; a reattempt result sets
	// neither. SourceIssue is the issue the publisher's source-reference
	// section will name; SourceReferenceOmitted says why the stored
	// publication names none.
	SourceIssue            string `json:"source_issue,omitempty"`
	SourceReferenceOmitted string `json:"source_reference_omitted,omitempty"`
	// ReplacedTask is the abandon outcome of the --replace task, absent
	// without the flag.
	ReplacedTask *abandonResult `json:"replaced_task,omitempty"`
}

// The reasons a submission's stored publication names no source issue.
const (
	sourceOmittedNotAnIssue = "the task source is not exactly one canonical GitHub issue URL"
	sourceOmittedBeforeRule = "accepted before a source reference was required"
)

// sourceReference is what a submission result reports about the issue its
// pull request will reference: the issue, or the reason there is none.
type sourceReference struct{ issue, omitted string }

// sourceReferenceError refuses a publication file that disagrees with its
// task source. Its text repeats no submitted bytes, so preflight can print it
// where other input errors stay generic.
type sourceReferenceError string

func (e sourceReferenceError) Error() string { return string(e) }

// errWorkUnitRequired refuses a new submission that declares no work unit
// (#1931). Completion is recorded against the declaration, so a task accepted
// without one stays unfinished after its pull request merges and holds its
// work-in-progress slot. The operator writes the declaration and nothing is
// defaulted: its digest joins the run identity, so a default would change the
// identity a saved submission recomputes on retry. The text repeats no
// submitted bytes, so preflight can print it.
var errWorkUnitRequired = errors.New(
	`a new submission needs a work-unit declaration: pass --work-unit with a JSON file, ` +
		`the smallest being {"completion_criterion":"bound_pr_merged"}; ` +
		`a task submitted without one never finishes when its pull request merges`)

// errDuplicateSource marks new work refused because its source is an
// unfinished task's.
var errDuplicateSource = errors.New("duplicate source")

// submittedSourceReference checks a publication file against the task source
// (#1928). A task that is exactly one issue URL must name that issue in
// source_issue, and a declared source_issue must be the task's whole source,
// so a pull request can neither lose nor misstate the issue it answers. The
// operator declares the source and nothing is derived for them: the
// publication digest joins the run identity, so a field the operator did not
// write would change the identity a saved submission recomputes on retry.
func submittedSourceReference(task []byte, publication engine.ProductionPublication) (sourceReference, error) {
	issue := engine.TaskSourceIssue(task)
	switch {
	case publication.SourceIssue != "" && publication.SourceIssue != issue:
		return sourceReference{}, sourceReferenceError(
			"publication source_issue is not the task source: the task file must hold that issue URL and nothing else")
	case issue != "" && publication.SourceIssue == "":
		return sourceReference{}, sourceReferenceError(
			`the task source is one GitHub issue URL, so the publication file must name it: set "source_issue" to that URL under "recipe": "freeside.client-publication/v3" and remove "title" and "body"`)
	case issue == "":
		return sourceReference{omitted: sourceOmittedNotAnIssue}, nil
	}
	return sourceReference{issue: issue}, nil
}

// acceptedSourceReference reports a submission that submittedSourceReference
// would refuse as new and that was accepted before the rule existed. Its
// stored publication is unchanged, so the result says what that record does.
func acceptedSourceReference(publication engine.ProductionPublication) sourceReference {
	if publication.SourceIssue != "" {
		return sourceReference{issue: publication.SourceIssue}
	}
	return sourceReference{omitted: sourceOmittedBeforeRule}
}

type submissionFile struct {
	digest domain.Digest
	body   []byte
}

// submitApplyRequest is the validated, journaled input crossing the private
// control socket. File paths stay in the client; the daemon receives bytes.
// PolicyBody, PolicyDigest, Keys, and ResolvedPolicy carry the -policy file's
// policy, from which the run identity is derived; applyLaunchSubmission
// overlays the store's workflow policy layers on Keys to build the stored
// policy.
type submitApplyRequest struct {
	SubmissionID          string
	ProjectID             domain.ProjectID
	SpecBody              []byte
	SpecDigest            domain.Digest
	PolicyBody            []byte
	PolicyDigest          domain.Digest
	Publication           engine.ProductionPublication
	PublicationDigest     domain.Digest
	PublicationBodyDigest domain.Digest
	WorkUnitDigest        domain.Digest
	Keys                  []domain.PolicyKey
	WorkUnit              *domain.WorkUnitDeclarationInput
	ResolvedPolicy        domain.ResolvedPolicy
	ImplementationRunID   domain.RunID
	SpecificationRunID    domain.RunID
	CampaignID            domain.CampaignID
	CompositionDigest     domain.Digest
	// TaskLines are the operator's per-role agent choices, in canonical role
	// order. Omitted when there are none, so a line-less request is the
	// bytes a daemon that predates the field accepts; that daemon's strict
	// decode refuses a request that carries lines and never drops them.
	TaskLines []domain.TaskLineChoice `json:",omitempty"`
	// Attachments reference blobs already stored beside the database, in
	// submitted order; omitted when there are none, like TaskLines.
	Attachments []signet.TaskAttachmentInput `json:",omitempty"`
	// ReplaceTaskID is the task the submission abandons in its write;
	// omitted when unset, like TaskLines, so a daemon that predates the
	// field refuses a replacement rather than ignore it.
	ReplaceTaskID domain.TaskID `json:",omitempty"`
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
	if cfg.RequireComposition && cfg.CompositionPath == "" && cfg.DBPath != "" {
		// A deputy writes its run's composition beside its database, so
		// unattended submission to it needs no path.
		cfg.CompositionPath = filepath.Join(filepath.Dir(cfg.DBPath), deputyCompositionManifestName)
	}
	// The lines are checked and put in canonical role order before anything
	// is saved or compared, so the journal, the fingerprint, and a repeated
	// request all see one form of the same set.
	lines, err := domain.CanonicalTaskLineChoices(cfg.TaskLines)
	if err != nil {
		return submitResult{}, fmt.Errorf("submit: --task-line: %w", err)
	}
	cfg.TaskLines = lines
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
	if _, err := submittedSourceReference(spec.body, publication); err != nil {
		// Refused here, before retainSubmission, so a new submission saves
		// nothing. A journal that already exists may belong to a submission
		// the database accepted before the rule; applySubmission tells that
		// replay from a journal that never reached the database.
		if _, retainedErr := matchingRetainedSubmission(cfg); retainedErr != nil {
			return submitResult{}, fmt.Errorf("submit: %w", err)
		}
	}
	publicationBody, err := json.Marshal(publication)
	if err != nil {
		return submitResult{}, fmt.Errorf("submit: encode publication metadata: %w", err)
	}
	publicationFile = submissionBytes(publicationBody)
	publicationDigest := publicationFile.digest

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
	} else {
		// Refused before retainSubmission, with the source rule's exception:
		// a journal that already exists may belong to a submission the
		// database accepted without a declaration, and applySubmission
		// tells that replay from a journal that never reached the database.
		if _, retainedErr := matchingRetainedSubmission(cfg); retainedErr != nil {
			return submitResult{}, fmt.Errorf("submit: %w", errWorkUnitRequired)
		}
	}

	// Both the deliberate submission identity and every immutable run binding
	// participate. Only redelivery of that saved request converges.
	implementationRunID := engine.ManualSubmissionRunID(
		"cli:"+cfg.SubmissionID, cfg.ProjectID, spec.digest, policyDigest, publicationFile.digest, workUnitDigest)
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
	var composition submissionFile
	launchComposition := false
	if cfg.CompositionPath != "" {
		composition, err = cfg.input("composition", cfg.CompositionPath)
		if err != nil {
			return submitResult{}, fmt.Errorf("submit: read composition manifest: %w", err)
		}
		if launchComposition, err = validateSubmissionComposition(composition.body, implementationRunID, compositionIdentity{
			SubmissionID: cfg.SubmissionID,
			SourceDigest: spec.digest, PolicyDigest: policyDigest,
			PublicationDigest: publicationDigest, WorkUnitDigest: workUnitDigest,
			CommitAuthor: publication.CommitAuthor,
		}, cfg.ProjectID, resolvedPolicy); err != nil {
			return submitResult{}, fmt.Errorf("submit: composition manifest: %w", err)
		}
	}
	if err := engine.SubmittedEgressPolicy(resolvedPolicy); err != nil {
		// A retained request may be an already accepted manual submission.
		// applySubmission distinguishes that replay from a journal that has not
		// reached the database yet, which must still be refused before writing.
		if _, retainedErr := matchingRetainedSubmission(cfg); retainedErr != nil {
			return submitResult{}, fmt.Errorf("submit: %w", err)
		}
	}
	// Refuse a bad launch size at submission, not when the writer starts.
	// Submit knows no instance's memory budget; the instance refuses a size
	// that can never fit it when the launch is admitted.
	if _, err := ward.ResolveLaunchSizes(resolvedPolicy.Keys, ward.LaunchBound{}); err != nil {
		return submitResult{}, fmt.Errorf("submit: %w", err)
	}
	policyBody, err := json.Marshal(resolvedPolicy.Keys)
	if err != nil {
		return submitResult{}, fmt.Errorf("submit: encode resolved policy keys: %w", err)
	}
	policy := submissionFile{digest: resolvedPolicy.Digest, body: policyBody}
	if _, err := specify.ParsePolicy(resolvedPolicy); err != nil {
		return submitResult{}, fmt.Errorf("submit: validate specification policy: %w", err)
	}
	if workUnit != nil {
		if _, err := domain.NewWorkUnitDeclaration(*workUnit, implementationRunID, cfg.ProjectID, time.Unix(1, 0)); err != nil {
			return submitResult{}, fmt.Errorf("submit: validate work-unit declaration: %w", err)
		}
	}
	// Bytes land before the journal that names them, so a manual retry from
	// the journal finds every attachment it references.
	if err := storeAttachmentFiles(cfg.DBPath, cfg.attachmentFiles); err != nil {
		return submitResult{}, fmt.Errorf("submit: store attachment bytes: %w", err)
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
	// Warn after the journal-convergence retry so each pane-owned key prints
	// once per submit invocation, while keeping keys intact on the wire so the
	// run identity stays derived from the file.
	for _, line := range workflowPolicyFileWarnings(keys) {
		_, _ = fmt.Fprintln(os.Stderr, line)
	}

	req := submitApplyRequest{
		SubmissionID: cfg.SubmissionID, ProjectID: cfg.ProjectID,
		SpecBody: spec.body, SpecDigest: spec.digest, PolicyBody: policy.body,
		PolicyDigest: policy.digest, Publication: publication, PublicationDigest: publicationDigest,
		PublicationBodyDigest: publicationFile.digest, WorkUnitDigest: workUnitDigest,
		Keys: keys, WorkUnit: workUnit, ResolvedPolicy: resolvedPolicy,
		ImplementationRunID: implementationRunID, SpecificationRunID: specificationRunID,
		CampaignID: campaignID, CompositionDigest: composition.digest,
		TaskLines: cfg.TaskLines, Attachments: cfg.Attachments, ReplaceTaskID: cfg.ReplaceTaskID,
	}
	handle, client, err := openCommandStore(ctx, cfg.DBPath, store.Options{}, storeMigrating)
	if err != nil {
		return submitResult{}, fmt.Errorf("submit: open store: %w", err)
	}
	if client != nil {
		defer client.Close()
		var result submitResult
		if err := client.call(ctx, "/submissions", req, &result); err != nil {
			return result, err
		}
		return result, replacementError(result)
	}
	defer func() { _ = handle.Close() }()
	if launchComposition {
		// A launch manifest attests the deputy that composed it while that
		// deputy serves; with no daemon on the database it is a record of a
		// run that has ended, and binds nothing.
		return submitResult{}, fmt.Errorf("submit: composition manifest %s is a deputy launch's, and no deputy serves %s: %w",
			cfg.CompositionPath, cfg.DBPath, domain.ErrParentKeyMismatch)
	}
	blobs, err := signet.NewBlobStore(cfg.DBPath + ".blobs")
	if err != nil {
		return submitResult{}, fmt.Errorf("submit: open blob store: %w", err)
	}
	result, err := applySubmission(ctx, handle.store, blobs, req)
	if err != nil {
		return result, err
	}
	return result, replacementError(result)
}

// workflowPolicyFileWarnings returns one stderr warning line for each key in
// keys that the Workflow settings pane owns. The submit path keeps the file's
// keys on the wire so the run identity stays with the file, while the daemon
// resolves those six keys from the store's layers for new work.
func workflowPolicyFileWarnings(keys []domain.PolicyKey) []string {
	var warnings []string
	for _, key := range keys {
		if !domain.IsWorkflowPolicyKey(key.Key) {
			continue
		}
		warnings = append(warnings, fmt.Sprintf(
			"freesided: warning: -policy sets %s=%q, but the Workflow settings pane decides that key for new work, so the file's value is ignored; set it in the Workflow pane instead",
			key.Key, key.Value,
		))
	}
	return warnings
}

// replacementError reports a replacement whose cleanup failed after the new
// task committed. applyLaunchSubmission returns that outcome in the result,
// as /tasks/abandon does, so the control route still carries the new task.
func replacementError(result submitResult) error {
	if result.ReplacedTask != nil && result.ReplacedTask.Error != "" {
		return errors.New(result.ReplacedTask.Error)
	}
	return nil
}

// applySubmission runs the same store transaction for offline and daemon-routed
// submissions. The caller owns the store and blob store. An offline
// submission applies no readiness gate: no daemon serves the database, so
// none has composed settings to read readiness under, and the next
// daemon's admission holds the task while its project is not ready.
func applySubmission(ctx context.Context, st *store.Store, blobs *signet.BlobStore, req submitApplyRequest) (submitResult, error) {
	return applyLaunchSubmission(ctx, st, blobs, req, nil, nil)
}

// applyLaunchSubmission is applySubmission for a daemon that may be a
// deputy. A non-nil launch is the deputy's own composition, and new work it
// accepts must bind to it (deputyLaunch.admit); a nil launch admits as
// before. gate is the daemon's project readiness gate, which refuses new
// work, never a replay, while the project is not ready (#3205); nil checks
// nothing.
func applyLaunchSubmission(
	ctx context.Context, st *store.Store, blobs *signet.BlobStore, req submitApplyRequest, launch *deputyLaunch,
	gate engine.ProjectReadinessGate,
) (submitResult, error) {
	if err := validateSubmitApply(req); err != nil {
		return submitResult{}, fmt.Errorf("submit: invalid prepared request: %w", err)
	}
	cfg := submitCommandConfig{SubmissionID: req.SubmissionID, ProjectID: req.ProjectID}
	spec := submissionFile{digest: req.SpecDigest, body: req.SpecBody}
	publication := req.Publication
	publicationDigest := req.PublicationDigest
	workUnit := req.WorkUnit
	resolvedPolicy := req.ResolvedPolicy
	publicationFile := submissionFile{digest: req.PublicationBodyDigest}
	workUnitDigest := req.WorkUnitDigest
	policyDigest := resolvedPolicy.Digest
	implementationRunID := req.ImplementationRunID
	specificationRunID := req.SpecificationRunID
	campaignID := req.CampaignID
	composition := submissionFile{digest: req.CompositionDigest}
	// Checked again on this side of the control socket, so a CLI that
	// predates the rule cannot submit past it.
	reference, sourceErr := submittedSourceReference(spec.body, publication)
	if sourceErr != nil {
		// Only a replay of an accepted submission returns a result from here
		// on; a new submission is refused below.
		reference = acceptedSourceReference(publication)
	}
	specArtifact, err := engine.SubmissionArtifact(
		domain.ArtifactKindSpecification, spec.digest, domain.EvidenceMediaTextMarkdown, int64(len(spec.body)))
	if err != nil {
		return submitResult{}, fmt.Errorf("submit: %w", err)
	}
	// The task lines, attachments, and replaced task join the fingerprint so
	// a submission identity names one choice of agents, one list of files,
	// and one task it replaces. Each is omitted when empty, which keeps a
	// submission without them at the digest it had before the field existed,
	// so one recorded then still replays.
	fingerprint, err := json.Marshal(struct {
		Project                                            domain.ProjectID
		Source, Policy, Publication, WorkUnit, Composition domain.Digest
		TaskLines                                          []domain.TaskLineChoice      `json:",omitempty"`
		Attachments                                        []signet.TaskAttachmentInput `json:",omitempty"`
		ReplaceTaskID                                      domain.TaskID                `json:",omitempty"`
	}{
		cfg.ProjectID, spec.digest, policyDigest, publicationFile.digest, workUnitDigest, composition.digest,
		req.TaskLines, req.Attachments, req.ReplaceTaskID,
	})
	if err != nil {
		return submitResult{}, err
	}
	manual := &domain.ManualSubmission{
		Identity: "cli:" + cfg.SubmissionID, ProjectID: cfg.ProjectID, SourceArtifactID: specArtifact.ID,
		SourceDigest: spec.digest, RequestDigest: domain.Digest(contentaddr.Sum(fingerprint)), ImplementationRunID: implementationRunID,
		TaskLines: req.TaskLines,
	}
	egressErr := engine.SubmittedEgressPolicy(resolvedPolicy)
	acceptedManual := false
	if egressErr != nil || sourceErr != nil || workUnit == nil {
		if err := st.Read(ctx, func(tx *store.ReadTx) error {
			original, err := tx.GetManualSubmission(ctx, manual.Identity)
			if errors.Is(err, store.ErrNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			if !original.SameRequest(*manual) {
				return store.ErrImmutableConflict
			}
			acceptedManual = true
			return nil
		}); err != nil {
			return submitResult{}, fmt.Errorf("submit: inspect saved submission: %w", err)
		}
	}
	if egressErr != nil && !acceptedManual {
		return submitResult{}, fmt.Errorf("submit: %w", egressErr)
	}
	if sourceErr != nil && !acceptedManual {
		return submitResult{}, fmt.Errorf("submit: %w", sourceErr)
	}
	// The declaration rule is checked on this side of the control socket too,
	// and likewise leaves an accepted submission's replay alone.
	if workUnit == nil && !acceptedManual {
		return submitResult{}, fmt.Errorf("submit: %w", errWorkUnitRequired)
	}
	// The attachments are resolved against the blob store before the write
	// transaction: every reference must be stored and the sniffed list must
	// meet the limits, or the submission writes nothing.
	attachments, err := signet.ResolveTaskAttachments(blobs, req.Attachments)
	if err != nil {
		return submitResult{}, fmt.Errorf("submit: %w", err)
	}
	// Bytes land before metadata: an artifact row must never name a digest
	// the blob store cannot serve, since admission materializes stage inputs
	// by digest.
	if _, err := blobs.Put(spec.digest, bytes.NewReader(spec.body)); err != nil {
		return submitResult{}, fmt.Errorf("submit: store specification bytes: %w", err)
	}
	var (
		submitted      engine.SpecificationRun
		policyArtifact domain.Artifact
		replaced       *abandonResult
	)
	lookupComplete := errors.New("submission lookup complete")
	err = st.Write(ctx, func(tx *store.WriteTx) error {
		replaced = nil
		policyArtifact = domain.Artifact{}
		replay := false
		if original, err := tx.GetManualSubmission(ctx, manual.Identity); err == nil {
			if !original.SameRequest(*manual) {
				return store.ErrImmutableConflict
			}
			replay = true
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if !replay {
			if err := launch.admit(req.ProjectID, publication.CommitAuthor, composition.digest, resolvedPolicy); err != nil {
				return fmt.Errorf("submit: %w", err)
			}
			if req.ReplaceTaskID != "" {
				var err error
				if replaced, err = abandonReplacedTask(
					ctx, tx, req.ReplaceTaskID, cfg.ProjectID, specArtifact.ID, time.Now().UTC(),
				); err != nil {
					return err
				}
			}
			if err := refuseUnfinishedDuplicate(ctx, &tx.ReadTx, cfg.ProjectID, specArtifact.ID); err != nil {
				return err
			}
		}
		if err := engine.RegisterSubmissionArtifact(ctx, tx, specArtifact); err != nil {
			return err
		}
		// Resolve the Workflow settings pane's keys inside this transaction so
		// new work sees the current store layers and a replay keeps the stored
		// run's immutable policy.
		policyKeys, err := engine.ResolveRunWorkflowPolicyKeys(
			ctx, &tx.ReadTx, cfg.ProjectID, req.Keys, specificationRunID,
		)
		if err != nil {
			return fmt.Errorf("resolve workflow policy: %w", err)
		}
		storedPolicy, err := domain.NewResolvedPolicy(specificationRunID, policyKeys)
		if err != nil {
			return fmt.Errorf("resolve stored policy: %w", err)
		}
		policyBytes, err := json.Marshal(storedPolicy.Keys)
		if err != nil {
			return fmt.Errorf("encode stored policy: %w", err)
		}
		if _, err := blobs.Put(storedPolicy.Digest, bytes.NewReader(policyBytes)); err != nil {
			return fmt.Errorf("store policy bytes: %w", err)
		}
		policyArtifact, err = engine.SubmissionArtifact(
			domain.ArtifactKindPolicy, storedPolicy.Digest, domain.EvidenceMediaApplicationJSON, int64(len(policyBytes)),
		)
		if err != nil {
			return err
		}
		if err := engine.RegisterSubmissionArtifact(ctx, tx, policyArtifact); err != nil {
			return err
		}
		var submitErr error
		submitted, submitErr = engine.SubmitSpecificationRunTx(ctx, tx, engine.SpecificationRunSpec{
			ManualSubmission:   manual,
			SpecificationRunID: specificationRunID, ImplementationRunID: implementationRunID,
			ProjectID: cfg.ProjectID, SourceArtifactID: specArtifact.ID, SourceBytes: spec.body,
			PolicyArtifactID: policyArtifact.ID, ResolvedPolicy: storedPolicy, Publication: publication,
			PublicationDigest: publicationDigest,
			WorkUnit:          workUnit, CampaignID: campaignID, AttemptNumber: 1,
		}, gate)
		if submitErr == nil && replay {
			return lookupComplete // A replay never commits a write.
		}
		if submitErr != nil {
			return submitErr
		}
		// Only the submission that creates the task records its lines and
		// attachments, in the same transaction: a replay returned above, so a
		// retry appends no second version.
		if err := recordSubmittedTaskLines(ctx, tx, submitted.Run.TaskID, req.TaskLines, "cli:"+cfg.SubmissionID); err != nil {
			return err
		}
		if err := tx.PutTaskAttachments(ctx, submitted.Run.TaskID, attachments); err != nil {
			return fmt.Errorf("record task attachments: %w", err)
		}
		return nil
	})
	if err != nil && !errors.Is(err, lookupComplete) {
		return submitResult{}, fmt.Errorf("submit: %w", err)
	}
	result := submitResult{
		SubmissionID: cfg.SubmissionID, TaskID: submitted.Run.TaskID,
		RunID: submitted.ImplementationRunID, SpecificationRunID: submitted.Run.ID,
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
		SourceIssue:                   reference.issue,
		SourceReferenceOmitted:        reference.omitted,
	}
	if workUnit != nil {
		result.WorkUnitID = domain.WorkUnitIDForRun(submitted.ImplementationRunID)
	}
	if req.ReplaceTaskID != "" {
		if replaced == nil {
			// A replay: the abandon committed with the new task, and only
			// the episode is read back so its cleanup can finish.
			if replaced, err = replayedReplacement(ctx, st, req.ReplaceTaskID); err != nil {
				return submitResult{}, fmt.Errorf("submit: replaced task %s: %w", req.ReplaceTaskID, err)
			}
		}
		if replaced.Episode == 0 {
			replaced.CleanupComplete = true
		} else {
			// The error is in replaced.Error, which the caller reports.
			_ = finishAbandonedEpisode(ctx, st, replaced)
		}
		result.ReplacedTask = replaced
	}
	return result, nil
}

// errReplaceRefused marks a --replace submission refused because of the task
// it names. Nothing is written when it is returned.
var errReplaceRefused = errors.New("replace refused")

// abandonReplacedTask abandons the task a --replace submission names, in the
// submission's own write, so the new task never runs beside it and the old
// one is never abandoned for a submission that fails. It refuses unless the
// task is in the submitted project, holds the submitted source, is
// unfinished, and has only execution failures open, at least one: that is a
// task that failed, and any other open item may still need the owner. The
// open items are the ones abandoning would retire, so an effect or task
// proposal the episode owns refuses too. A finished task, abandoned or
// released, is refused: a retry of a submission that already abandoned it
// never reaches here, since replayedReplacement answers it from the journal,
// and a retry of one that never committed replaced nothing to report.
func abandonReplacedTask(
	ctx context.Context, tx *store.WriteTx, taskID domain.TaskID, project domain.ProjectID, source domain.ArtifactID,
	now time.Time,
) (*abandonResult, error) {
	snapshot, err := tx.GetTaskSnapshot(ctx, taskID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("%w: task %s does not exist", errReplaceRefused, taskID)
	}
	if err != nil {
		return nil, err
	}
	task := snapshot.Value
	switch {
	case task.ProjectID != project:
		return nil, fmt.Errorf("%w: task %s is in project %s, not %s", errReplaceRefused, taskID, task.ProjectID, project)
	case task.Source == nil || task.Source.WorkItemArtifactID != source:
		return nil, fmt.Errorf("%w: task %s does not hold this submission's source", errReplaceRefused, taskID)
	}
	if lifecycle := task.DisplayLifecycle(nil); lifecycle != nil {
		// A finished task no longer holds its source, so a submission
		// without --replace, under a new id, is accepted.
		return nil, fmt.Errorf("%w: task %s is %s; submit without --replace under a new submission id",
			errReplaceRefused, taskID, *lifecycle)
	}
	result := &abandonResult{TaskID: taskID, RetiredItems: []domain.ItemID{}}
	start := task.CurrentStart()
	items, err := replacedTaskOpenItems(ctx, &tx.ReadTx, task)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("%w: task %s has no execution failure open; abandon it with `freesided abandon` first if it should stop",
			errReplaceRefused, taskID)
	}
	open := make([]string, 0, len(items))
	onlyFailures := true
	for _, item := range items {
		open = append(open, fmt.Sprintf("%s (%s)", item.ID, item.Type))
		onlyFailures = onlyFailures && item.Type == domain.AttentionExecutionFailure
	}
	if !onlyFailures {
		return nil, fmt.Errorf("%w: task %s has items open other than execution failures: %s",
			errReplaceRefused, taskID, strings.Join(open, ", "))
	}
	// Submission records a task's start, so an unfinished task has one; a
	// task without one would hold no slot and record no abandon, and
	// refuseUnfinishedDuplicate then refuses the submission.
	if start != nil {
		result.Episode = start.Ordinal
	}
	if result.Released, err = tx.AbandonTask(ctx, taskID, now); err != nil {
		return nil, err
	}
	return result, nil
}

// replacedTaskOpenItems lists the open items abandoning an unfinished task
// would retire, the set --replace accepts only execution failures in. A task
// with no start has no episode, so no closure, and nothing to retire.
func replacedTaskOpenItems(ctx context.Context, tx *store.ReadTx, task domain.Task) ([]domain.AttentionItem, error) {
	start := task.CurrentStart()
	if start == nil {
		return nil, nil
	}
	return engine.OpenTaskWorkflowItems(ctx, tx, task.ID, start.Ordinal)
}

// replayedReplacement reads back the replaced task of an accepted
// submission. Its abandoned episode is the one the cleanup finishes; a task
// no longer abandoned has nothing left to clean up.
func replayedReplacement(ctx context.Context, st *store.Store, taskID domain.TaskID) (*abandonResult, error) {
	result := &abandonResult{TaskID: taskID, RetiredItems: []domain.ItemID{}}
	err := st.Read(ctx, func(tx *store.ReadTx) error {
		task, err := tx.GetTask(ctx, taskID)
		if err != nil {
			return err
		}
		lifecycle, start := task.DisplayLifecycle(nil), task.CurrentStart()
		if lifecycle != nil && *lifecycle == domain.TaskAbandoned && start != nil {
			result.Episode = start.Ordinal
		}
		return nil
	})
	return result, err
}

// recordSubmittedTaskLines appends the operator's lines to the task a CLI
// submission just created. Each line is set when its task is created. The
// flag writes selections; a choice naming an agent comes only from a
// recovery journal saved before #2597, and its retry writes the v1 line its
// record backs.
func recordSubmittedTaskLines(
	ctx context.Context, tx *store.WriteTx, taskID domain.TaskID, lines []domain.TaskLineChoice, setBy string,
) error {
	if len(lines) == 0 {
		return nil
	}
	task, err := tx.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	for _, line := range lines {
		if _, err := tx.AppendTaskLine(ctx, domain.TaskLineInput{
			TaskID: taskID, Role: line.Role, Agent: line.Agent, Selection: line.Selection,
			Source: domain.TaskLineSourceCLISubmit, SetBy: setBy,
		}, task.CreatedAt); err != nil {
			return fmt.Errorf("record task line: %w", err)
		}
	}
	return nil
}

// validateSubmitApply checks the prepared request that crossed the control
// socket. PolicyDigest, Keys, ResolvedPolicy, and ImplementationRunID all
// check against the -policy file's keys; the daemon overlays the Workflow
// settings pane's keys afterward when storing the run's policy.
func validateSubmitApply(req submitApplyRequest) error {
	// The request crossed the control socket, so its lines are checked again
	// here and must already be in the canonical form the fingerprint digests.
	lines, err := domain.CanonicalTaskLineChoices(req.TaskLines)
	if err != nil {
		return err
	}
	if !slices.EqualFunc(lines, req.TaskLines, domain.TaskLineChoice.Same) {
		return errors.New("task lines are not in canonical role order")
	}
	// The attachment references' shape is checked here too; their bytes are
	// checked when applyLaunchSubmission resolves them.
	if err := signet.CheckTaskAttachmentInputs(req.Attachments); err != nil {
		return err
	}
	if len(req.SpecBody) == 0 || len(req.SpecBody) > maxSubmissionFileBytes ||
		len(req.PolicyBody) == 0 || len(req.PolicyBody) > maxSubmissionFileBytes ||
		submissionBytes(req.SpecBody).digest != req.SpecDigest ||
		submissionBytes(req.PolicyBody).digest != req.PolicyDigest {
		return errors.New("input bytes do not match their digests or size limits")
	}
	if err := req.Publication.Validate(); err != nil {
		return err
	}
	publicationBody, err := json.Marshal(req.Publication)
	if err != nil {
		return err
	}
	if submissionBytes(publicationBody).digest != req.PublicationBodyDigest {
		return errors.New("publication digest does not match its body")
	}
	policy, err := domain.NewResolvedPolicy(req.SpecificationRunID, req.Keys)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(policy, req.ResolvedPolicy) || req.PolicyDigest != policy.Digest {
		return errors.New("resolved policy does not match its keys and digest")
	}
	if err := engine.SubmittedPathBoundary(policy); err != nil {
		return err
	}
	wantSpec, err := engine.SpecificationRunIDForImplementation(req.ImplementationRunID)
	if err != nil || wantSpec != req.SpecificationRunID {
		return errors.New("specification run identity mismatch")
	}
	wantCampaign, err := engine.ProductionCampaignIDForImplementation(req.ImplementationRunID)
	if err != nil || wantCampaign != req.CampaignID {
		return errors.New("campaign identity mismatch")
	}
	if req.WorkUnit == nil {
		if req.WorkUnitDigest != "" {
			return errors.New("work-unit digest without declaration")
		}
	} else {
		declared := submittedWorkUnit{
			CompletionCriterion: req.WorkUnit.CompletionCriterion, BoundIssue: req.WorkUnit.BoundIssue,
			DependsOnIssues: req.WorkUnit.DependsOnIssues, ContractSerialized: req.WorkUnit.ContractSerialized,
		}
		body, err := json.Marshal(declared)
		if err != nil {
			return err
		}
		if submissionBytes(body).digest != req.WorkUnitDigest ||
			!slices.Equal(req.WorkUnit.DeclaredPaths, engine.DeclaredPathScope(req.Keys)) {
			return errors.New("work-unit declaration does not match its digest or policy paths")
		}
	}
	if req.SubmissionID == "" || req.PublicationDigest != req.PublicationBodyDigest ||
		engine.ManualSubmissionRunID("cli:"+req.SubmissionID, req.ProjectID, req.SpecDigest, policy.Digest, req.PublicationBodyDigest, req.WorkUnitDigest) != req.ImplementationRunID {
		return errors.New("manual submission identity mismatch")
	}
	return nil
}

// validateSubmissionComposition binds a passing preflight manifest to the
// exact inputs being submitted: a manifest for different inputs, a different
// derived run identity, or a non-passing status refuses the submission. It
// reports whether the manifest is a deputy's launch manifest, which binds
// only through the running deputy.
func validateSubmissionComposition(
	body []byte, runID domain.RunID, identity compositionIdentity,
	project domain.ProjectID, policy domain.ResolvedPolicy,
) (launch bool, err error) {
	if err := ward.RejectDuplicateJSONKeys(body); err != nil {
		return false, err
	}
	var manifest compositionManifest
	if err := strictjson.Decode(
		body, &manifest, strictjson.RejectInvalidUTF8, strictjson.Limit(maxSubmissionFileBytes),
	); err != nil {
		return false, err
	}
	launch = manifest.Identity == (compositionIdentity{})
	if manifest.Version != compositionManifestVersion || manifest.Status != compositionPassed {
		return false, fmt.Errorf("manifest is not a passing %s composition: %w", compositionManifestVersion, domain.ErrParentKeyMismatch)
	}
	if launch {
		return true, validateLaunchComposition(manifest, identity, project, policy)
	}
	wantInvocation := domain.InvocationID("inv-implement-" + string(runID))
	if manifest.Identity.SubmissionID != identity.SubmissionID ||
		manifest.Identity.SourceDigest != identity.SourceDigest ||
		manifest.Identity.PolicyDigest != identity.PolicyDigest ||
		manifest.Identity.PublicationDigest != identity.PublicationDigest ||
		manifest.Identity.WorkUnitDigest != identity.WorkUnitDigest ||
		manifest.Identity.ImplementationRunID != runID ||
		manifest.Identity.ImplementationInvocationID != wantInvocation {
		return false, fmt.Errorf("passing manifest does not bind the submitted inputs, run %q, and invocation %q: %w",
			runID, wantInvocation, domain.ErrParentKeyMismatch)
	}
	return false, nil
}

// refuseUnfinishedDuplicate refuses new work whose source is already an
// unfinished task's in the same project, so one work item cannot run twice
// by accident. A replay of the same submission never reaches it; deliberate
// new work on the same source goes through that task's own reattempt, after
// it is abandoned, or with --replace when only execution failures are open.
// The same source in another project is new work there; a deputy's launch
// manifest admits only its own project, so in a deputy's store this covers
// every task. The scan reads every task: a real-work store holds few.
func refuseUnfinishedDuplicate(
	ctx context.Context, tx *store.ReadTx, project domain.ProjectID, source domain.ArtifactID,
) error {
	tasks, err := tx.ListTasks(ctx)
	if err != nil {
		return err
	}
	for _, row := range tasks {
		task := row.Value
		if task.ProjectID != project || task.Source == nil || task.Source.WorkItemArtifactID != source ||
			task.DisplayLifecycle(nil) != nil {
			continue
		}
		items, err := replacedTaskOpenItems(ctx, tx, task)
		if err != nil {
			return err
		}
		replaceable := len(items) != 0 && !slices.ContainsFunc(items, func(item domain.AttentionItem) bool {
			return item.Type != domain.AttentionExecutionFailure
		})
		return duplicateSourceError{task: task.ID, replaceable: replaceable}
	}
	return nil
}

// duplicateSourceError is refuseUnfinishedDuplicate's refusal. It names the
// task holding the source and whether only execution failures are open on
// it, the case --replace accepts, so prelaunch can say which fix applies.
type duplicateSourceError struct {
	task        domain.TaskID
	replaceable bool
}

func (e duplicateSourceError) Error() string {
	if e.replaceable {
		return fmt.Sprintf("%s: task %s already holds this source and has only execution failures open; "+
			"replace it with `freesided submit --replace %s`, reattempt it with `freesided reattempt`, "+
			"or redeliver its own submission with --retry-submission-id", errDuplicateSource, e.task, e.task)
	}
	return fmt.Sprintf("%s: task %s already holds this source and is not finished; "+
		"reattempt it with `freesided reattempt`, abandon it with `freesided abandon`, "+
		"or redeliver its own submission with --retry-submission-id", errDuplicateSource, e.task)
}

func (e duplicateSourceError) Unwrap() error { return errDuplicateSource }

// validateLaunchComposition binds a submission to the manifest a deputy
// composed for its whole run, which names no submission. What the
// per-submission check proves through the identity is checked here against
// the run: the project is the one the run serves, the commit author is the
// App bot the run resolved, and the policy's paths stay inside the run's
// allowlist. The settings digest marks the manifest as a launch's; a
// manifest without one binds nothing.
func validateLaunchComposition(
	manifest compositionManifest, identity compositionIdentity,
	project domain.ProjectID, policy domain.ResolvedPolicy,
) error {
	if manifest.SettingsDigest == "" || manifest.Project == "" || manifest.PublicationAuthor == nil {
		return fmt.Errorf("passing manifest binds no submission and no launch: %w", domain.ErrParentKeyMismatch)
	}
	if project != manifest.Project {
		return fmt.Errorf("project %s is not the run's project %s: %w", project, manifest.Project, domain.ErrParentKeyMismatch)
	}
	if identity.CommitAuthor != *manifest.PublicationAuthor {
		return fmt.Errorf("publication commit_author %s[bot] (%d) is not the run's App bot %s[bot] (%d): %w",
			identity.CommitAuthor.AppSlug, identity.CommitAuthor.BotUserID,
			manifest.PublicationAuthor.AppSlug, manifest.PublicationAuthor.BotUserID, domain.ErrParentKeyMismatch)
	}
	if _, err := admissionauth.ResolvedPathAllowlist(policy, policy.Digest, manifest.AllowedPaths); err != nil {
		return fmt.Errorf("paths policy against the run's allowed paths: %w", err)
	}
	return nil
}

// deputyLaunch is a deputy's own record of the composition it serves under:
// the digest of its passing launch manifest and the manifest itself, or no
// digest when its start-up check failed.
type deputyLaunch struct {
	ManifestDigest domain.Digest
	Manifest       compositionManifest
}

// admit binds new work to the deputy's launch on the daemon's side of the
// control socket. Submit checks the same binding before it sends, but the
// socket accepts any prepared request, including one from submit run
// without --require-composition, so the deputy does not rely on that. New
// work must name this launch's manifest and pass its project, commit
// author, and path checks; a deputy whose check failed accepts none. A nil
// launch is a daemon that is not a deputy.
func (l *deputyLaunch) admit(
	project domain.ProjectID, author engine.ProductionCommitAuthor, composition domain.Digest, policy domain.ResolvedPolicy,
) error {
	switch {
	case l == nil:
		return nil
	case l.ManifestDigest == "":
		return fmt.Errorf("this deputy's start-up check failed, so it accepts no new work: %w", domain.ErrParentKeyMismatch)
	case composition != l.ManifestDigest:
		return fmt.Errorf("new work must bind this deputy's launch manifest %s (submit --require-composition): %w",
			l.ManifestDigest, domain.ErrParentKeyMismatch)
	}
	if err := validateLaunchComposition(l.Manifest, compositionIdentity{CommitAuthor: author}, project, policy); err != nil {
		return fmt.Errorf("composition manifest: %w", err)
	}
	return nil
}

// gateManualInitiator binds paired-client submit_task to the launch, as
// admit binds /submissions. A paired client names no manifest, so it binds
// this launch implicitly: a project's configured commit author and policy
// must pass admit, and a deputy whose start-up check failed admits none.
// The lookup runs only for new tasks (the signet boundary replays recorded
// ones first). A refused project answers as unconfigured, a definite
// no-effect 404; the log says why. A nil launch is a daemon that is not a
// deputy.
func (l *deputyLaunch) gateManualInitiator(
	lookup func(domain.ProjectID) (engine.ManualInitiator, bool), logger *slog.Logger,
) func(domain.ProjectID) (engine.ManualInitiator, bool) {
	if l == nil {
		return lookup
	}
	return func(project domain.ProjectID) (engine.ManualInitiator, bool) {
		initiator, ok := lookup(project)
		if !ok {
			return engine.ManualInitiator{}, false
		}
		policy, err := domain.NewResolvedPolicy("deputy-launch-admission", initiator.PolicyKeys)
		if err == nil {
			err = l.admit(project, initiator.CommitAuthor, l.ManifestDigest, policy)
		}
		if err != nil {
			if logger != nil {
				logger.Warn("paired-client submission refused by the deputy launch", "project", project, "error", err)
			}
			return engine.ManualInitiator{}, false
		}
		return initiator, true
	}
}
