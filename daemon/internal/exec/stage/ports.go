package stage

import (
	"context"
	"errors"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// Provider supplies the behavior that varies between stage providers. The
// durable state machine owns every other transition and persistence rule.
type Provider interface {
	HandoffSpec(context.Context, ProviderHandoffInput) (ward.HandoffSpec, error)
	RenderPrompt(ProviderPromptInputs) (string, error)
	PromptDelivery() PromptDelivery
	RunID(domain.InvocationID) string
	Workspace(domain.InvocationID) string
	PrepareFailedStatus() int
}

// PromptDelivery selects a durable launch protocol. Every member delivers
// the prompt through ward's protected prompt file, which the driver binds to
// the durable prompt; PromptFileV8 is the only member.
//
// A member names a whole launch command, not only how the prompt travels:
// recovery rebuilds a running launch's command from its stored member and
// ward refuses a rebuilt spec whose digest differs. So a provider changes its
// command by adding a member, never by changing what an existing one builds.
type PromptDelivery string

const (
	PromptFileV8 PromptDelivery = "file_v8"
)

var AllPromptDeliveries = []PromptDelivery{PromptFileV8}

func (d PromptDelivery) valid() bool {
	switch d {
	case PromptFileV8:
		return true
	default:
		return false
	}
}

// UsageExtractor is the optional provider hook for numbers-only telemetry in
// an already authenticated and imported evidence channel. A Claude provider
// also returns its writer's usage capture; any other provider returns nil.
type UsageExtractor interface {
	ExtractUsage(
		evidenceDir string,
		evidence export.EvidenceManifest,
		observedAt time.Time,
	) ([]exec.UsageMeasurement, *exec.ClaudeUsageCapture, error)
}

// QuotaRefusalClassifier is the optional provider hook that reads a failed
// writer's diagnostic transcript for a provider quota refusal. The transcript
// is untrusted agent output, and the refusal only feeds scheduling: it parks
// the task and marks a window exhausted, and grants nothing. Nil means the
// transcript is not a readable quota refusal, so the failure keeps no cause.
// A provider that doesn't implement it fails as before.
type QuotaRefusalClassifier interface {
	ClassifyQuotaRefusal(transcript []byte) *domain.QuotaRefusal
}

// FailureUsageExtractor is the optional provider hook that reads a failed
// writer's diagnostic transcript for the tokens it spent before it ended, so
// a killed writer doesn't look free. The transcript is untrusted agent
// output, and the measurements feed advisory readings only: they grant
// nothing and decide nothing. observedAt is the intent's recorded instant, so
// the live and restart paths commit the same observation. Nil means no
// readable usage. A provider that doesn't implement it records none.
type FailureUsageExtractor interface {
	ExtractFailureUsage(transcript []byte, observedAt time.Time) []exec.UsageMeasurement
}

// ProviderHandoffInput is the durable input needed to render one provider's
// ward handoff request.
type ProviderHandoffInput struct {
	InvocationID   domain.InvocationID
	RunID          string
	Spec           exec.StartSpec
	Seed           string
	Prompt         string
	PromptDelivery PromptDelivery
	Instructions   ward.VendorInstructions
	Preparation    []string
}

// CredentialMountPolicy is the immutable provider topology a returned
// credential mount must match. The volume is excluded because ward resolves
// and authenticates it independently against the admitted identity lease.
type CredentialMountPolicy struct {
	Target   string
	Manifest ward.CredentialManifestPolicy
	Writable bool
}

// ProviderPromptInputs are the admitted immutable bodies a provider renders
// into its invocation prompt.
type ProviderPromptInputs struct {
	Delivery       PromptDelivery
	Specification  []byte
	PromptPackage  []byte
	Policy         []byte
	PriorArtifacts [][]byte
	// Attachments are the admitted operator input files, in order, that the
	// writer reads under ward.InputFilesTarget. Only this metadata enters the
	// prompt; the bytes reach the writer through Ward's input volume.
	Attachments []domain.StageInputAttachment
	// ProtectedPaths are the patterns whose edit fails the run at import,
	// captured at start (AdmissionAuthority.ProtectedPatterns). Empty renders
	// nothing, so an intent recorded before the list existed re-renders its
	// original prompt.
	ProtectedPaths []string
}

// InputFilesFor is the Ward input manifest an admitted start delivers: each
// stage-input attachment's name, digest, and size, in admitted order. The
// provider builds its handoff from it and the driver holds the returned
// handoff to it, so neither can add, drop, or reorder a file.
func InputFilesFor(spec exec.StartSpec) []ward.InputFile {
	if spec.StageInputs == nil || len(spec.StageInputs.Attachments) == 0 {
		return nil
	}
	files := make([]ward.InputFile, len(spec.StageInputs.Attachments))
	for i, a := range spec.StageInputs.Attachments {
		files[i] = ward.InputFile{Name: a.Name, Digest: a.Digest, SizeBytes: a.SizeBytes}
	}
	return files
}

// Gate is the ward workspace-handoff gate (production: *ward.Backend). The
// driver never talks to a container runtime directly: every containment
// property §5.4 and §5.7 require is the gate's, and a driver-side shortcut
// would be a second, unaudited path to the same effects.
type Gate interface {
	Handoff(ctx context.Context, hs ward.HandoffSpec) (*ward.HandoffResult, error)
	HandoffStarted(ctx context.Context, runID string) (bool, error)
	RequestCancellation(ctx context.Context, runID string) error
	Recover(ctx context.Context, runID string, hs ward.HandoffSpec) (*ward.RecoveryResult, error)
	AuthenticateReleasedExport(ctx context.Context, runID, exportDir string) error
	// ReleaseFinishedHandoffs withdraws the rig names of each run whose
	// journal record is absent or closed. The caller guarantees that no
	// Handoff or Recover for a named run creates under its names without
	// binding them again: either it holds a terminal intent for the run, or
	// no intent exists and it holds the driver lock, so the next start runs
	// the pre-job hook that binds them. It is best effort and returns
	// nothing: a failed release leaves names bound, the safe direction, for
	// the next startup reconcile.
	ReleaseFinishedHandoffs(ctx context.Context, runIDs []string)
	// ReadLiveTranscript returns up to maxBytes of the running writer's
	// transcript from byte offset, or ward.ErrNoLiveWriter when the run's
	// handoff has no registered, running writer. The bytes are untrusted and
	// unscanned; only the live transcript tailer calls it, and it scans them
	// before any leave the daemon (live_transcript.go).
	ReadLiveTranscript(ctx context.Context, runID string, offset int64, maxBytes int) ([]byte, error)
}

// CordonGate is the cordon's port (production: the daemoncompose adapter
// over *cordon.Backend). The driver uses it in place of Gate for a start
// whose exec.StartSpec.RunnerClass is cordoned_host_process. Like Gate, it
// owns every containment property of the run; the driver only sequences it.
// The package declares the port rather than importing internal/cordon, so
// the cordon's cgo stays out of this package's build.
//
// Launch blocks until the run has ended and its end is proven, and canceling
// its context cancels the run for good. A daemon that shuts down must
// therefore make every waiting Launch return ErrCordonDetached before it
// cancels any launch's context; the run keeps going under its supervisor
// for the next daemon's Recover. Errors wrap the sentinels below where the
// driver must tell them apart.
type CordonGate interface {
	// PreJob is the pre-job probe a cordoned start runs in place of
	// Config.PreJob. Its refusal holds the start (exec.ErrPreJobRefused).
	PreJob(ctx context.Context, id domain.InvocationID) error
	Launch(ctx context.Context, launch CordonLaunch) (CordonEnd, error)
	// Export exports a cleanly ended run. A refusal that would repeat is a
	// *CordonExportRefusal; any other error is transient, since Recover
	// exports again.
	Export(ctx context.Context, req CordonExportRequest) (CordonExport, error)
	// Started reports whether the run's directory exists. A launch claims
	// it before it starts anything, so false proves nothing ran.
	Started(ctx context.Context, runID string) (bool, error)
	// RequestCancellation makes the run end canceled and never export. It
	// returns ErrCordonNoRun when the run has no directory.
	RequestCancellation(ctx context.Context, runID string) error
	// Recover waits for the run's end, ending it lost once its end is
	// proven when no supervisor will record one, and exports a clean exit.
	// Canceling ctx stops the wait and leaves the run running.
	Recover(ctx context.Context, req CordonExportRequest) (CordonRecovery, error)
	// AuthenticateReleasedExport accepts dir only as the run's own export
	// with the bytes Export recorded. A mismatch wraps ErrUnsupportedStart,
	// so the driver refuses it for good.
	AuthenticateReleasedExport(ctx context.Context, runID, dir string) error
	// ReleaseFinished removes each finished run's directory. The caller
	// holds a terminal intent for every run named. It is best effort, as
	// Gate.ReleaseFinishedHandoffs is: a failed release keeps a directory,
	// the safe direction, for the next reconcile.
	ReleaseFinished(ctx context.Context, runIDs []string)
}

var (
	// ErrCordonNoRun means the run has no directory: nothing was launched,
	// or the run was released.
	ErrCordonNoRun = errors.New("cordon run not found")
	// ErrCordonDetached is a Launch or Recover that returned because the
	// daemon is shutting down. The run keeps running for the next Recover.
	ErrCordonDetached = errors.New("cordon detached")
	// ErrCordonEndUnproven is a run whose end couldn't be proven: something
	// it started may still be alive, so only a later Recover may end it.
	ErrCordonEndUnproven = errors.New("cordon run end unproven")
)

// CordonExportRefusal is an export the cordon refused in a way that will
// repeat: the credential scan matched, the agent replaced its worktree or
// hard-linked a file, or the run didn't exit cleanly. Reason becomes an
// outcome's summary: a scan refusal's names the rule and the item, which
// can be a worktree path the agent chose, as a warded scan refusal's does;
// any other is a fixed phrase. Neither carries matched text. Err is the
// full refusal, for the log only.
type CordonExportRefusal struct {
	Reason string
	Err    error
}

func (e *CordonExportRefusal) Error() string { return "cordon export refused: " + e.Reason }

func (e *CordonExportRefusal) Unwrap() error { return e.Err }

// CordonLaunch is one cordoned writer launch. The adapter adds what the
// driver never holds: the setup token of the admission's own enrollment
// generation, and whether the launch must prove the class first.
type CordonLaunch struct {
	InvocationID domain.InvocationID
	RunID        string
	Base         domain.BaseRevision
	// Prompt goes to the agent on stdin.
	Prompt []byte
	// Args are the agent CLI's arguments, without --settings: the cordon
	// fixes the CLI's settings itself.
	Args []string
}

// CordonExportRequest is the export of one cordoned run: the transcript's
// evidence entry and the reserved sources the agent may have staged. The
// provider builds it from the intent, so the live launch and every Recover
// declare the same set.
type CordonExportRequest struct {
	RunID      string
	Transcript export.EvidenceSource
	Sources    []export.EvidenceSource
}

// CordonExport is a cordoned run's export: the facts a warded handoff
// releases. BaseSHA is the commit the run was seeded from, read from its
// end record.
type CordonExport struct {
	Dir               string
	Manifest          export.Manifest
	Evidence          export.EvidenceManifest
	EvidencePresent   bool
	CommitPlanPresent bool
	BaseSHA           string
}

// CordonEndedBy names what ended a cordoned run.
type CordonEndedBy string

const (
	CordonEndedByExited   CordonEndedBy = "exited"
	CordonEndedByCanceled CordonEndedBy = "canceled"
	CordonEndedByStalled  CordonEndedBy = "stalled"
	CordonEndedByLost     CordonEndedBy = "lost"
)

// AllCordonEndedBy lists every valid CordonEndedBy; it is the single
// registration point.
var AllCordonEndedBy = []CordonEndedBy{
	CordonEndedByExited, CordonEndedByCanceled, CordonEndedByStalled, CordonEndedByLost,
}

func (e CordonEndedBy) valid() bool {
	switch e {
	case CordonEndedByExited, CordonEndedByCanceled, CordonEndedByStalled, CordonEndedByLost:
		return true
	default:
		return false
	}
}

// CordonEnd is how a cordoned run ended, once nothing from it is alive.
type CordonEnd struct {
	EndedBy CordonEndedBy
	// ExitCode is the agent's exit status, -1 when a signal ended it.
	ExitCode int
	// Failure is the run's diagnostic transcript, set for a nonzero exit or
	// a stall: the agent's stdout with its stderr appended, as the warded
	// launcher keeps a failed CLI's. Unavailable when it was absent,
	// oversize, not UTF-8, or refused by the credential scan.
	Failure *ward.FailureEvidence
}

// CordonRecoveryOutcome is how Recover found a cordoned run.
type CordonRecoveryOutcome string

const (
	CordonRecoveryExported CordonRecoveryOutcome = "exported"
	CordonRecoveryFailed   CordonRecoveryOutcome = "failed"
	CordonRecoveryCanceled CordonRecoveryOutcome = "canceled"
	CordonRecoveryLost     CordonRecoveryOutcome = "lost"
)

// AllCordonRecoveryOutcomes lists every valid CordonRecoveryOutcome; it is
// the single registration point.
var AllCordonRecoveryOutcomes = []CordonRecoveryOutcome{
	CordonRecoveryExported, CordonRecoveryFailed, CordonRecoveryCanceled, CordonRecoveryLost,
}

func (o CordonRecoveryOutcome) valid() bool {
	switch o {
	case CordonRecoveryExported, CordonRecoveryFailed, CordonRecoveryCanceled, CordonRecoveryLost:
		return true
	default:
		return false
	}
}

// CordonRecovery is a recovered cordoned run.
type CordonRecovery struct {
	Outcome CordonRecoveryOutcome
	End     CordonEnd
	// Reason says why a lost run is lost. It is the cordon's own text.
	Reason string
	// Export is set for CordonRecoveryExported only.
	Export CordonExport
}

// CordonLaunchProvider is the optional provider hook that builds a
// cordoned launch, as Provider.HandoffSpec builds a warded one. A provider
// without it can't run a cordoned start. The driver checks what it returns
// against the intent.
type CordonLaunchProvider interface {
	CordonLaunch(ctx context.Context, in ProviderHandoffInput) (CordonLaunch, CordonExportRequest, error)
}

// Seeder materializes daemon-owned checkouts at exactly base (production:
// publish.Transport, whose results carry the canonical repository-name and
// numeric-ID binding ward refuses to seed without). dir must not exist yet.
//
// The two methods differ in one load-bearing way. FetchBaseWorktree carries
// the working-tree files and is what a workspace seed requires: ward's
// observer proves the raw worktree against HEAD, so a repository-only
// directory is dirty (every tracked path missing) and the run is refused
// before the writer starts. FetchBase omits them for the import lane, which
// applies an export over the checkout and must not inherit files nobody put
// there.
type Seeder interface {
	FetchBase(ctx context.Context, repo, baseRef, baseSHA, dir string) error
	FetchBaseWorktree(ctx context.Context, repo, baseRef, baseSHA, dir string) error
}

// ExportRecorder persists the write-once ExecutionExport and reads it back.
// store.RecordExecutionExport lives on the store's internal transaction, so
// the composition supplies this adapter rather than the driver holding a
// store handle.
type ExportRecorder interface {
	// RecordExecutionExport commits the export and its directory-free replay
	// as one durable decision. Implementations must converge an identical
	// retry and reject a differing replay for an existing invocation.
	RecordExecutionExport(
		ctx context.Context,
		export domain.ExecutionExport,
		replay ExecutionReplay,
	) error
	// LookupExecutionExport distinguishes confirmed absence from a read or
	// reconstruction error. Recovery may declare a run lost only on absence.
	LookupExecutionExport(
		ctx context.Context, id domain.InvocationID,
	) (domain.ExecutionExport, bool, error)
	LookupExecutionExportRecord(
		ctx context.Context, id domain.InvocationID,
	) (domain.ExecutionExport, bool, error)
}

// ImportStartRecorder persists and authenticates the write-once boundary
// after which every released-export retry must use current policy. The
// driver's private phase is replay data and cannot provide this authority.
type ImportStartRecorder interface {
	RecordCurrentImportStart(context.Context, domain.CurrentImportStart) error
	LookupCurrentImportStart(
		context.Context, domain.InvocationID,
	) (domain.CurrentImportStart, bool, error)
}

// OutcomeRecorder is the trusted durable authority for a non-export terminal
// outcome. A private intent file can replay one only when this port returns
// the same write-once record.
type OutcomeRecorder interface {
	RecordExecutionOutcome(context.Context, domain.ExecutionOutcome) error
	LookupExecutionOutcome(
		context.Context, domain.InvocationID,
	) (domain.ExecutionOutcome, bool, error)
	LookupExecutionOutcomeRecord(
		context.Context, domain.InvocationID,
	) (domain.ExecutionOutcome, bool, error)
	// RecordExportRejection persists the diagnostic per-finding detail of a
	// definitively rejected export, so it survives the released directory's
	// cleanup. It is write-once and converges on a byte-identical replay; it is
	// not an execution authority and coexists with the failed outcome the same
	// rejection records.
	RecordExportRejection(context.Context, domain.ExportRejection) error
	// LookupExportRejection reads that diagnostic detail back, distinguishing a
	// clean absence (no rejection recorded) from a read error.
	LookupExportRejection(
		context.Context, domain.InvocationID,
	) (domain.ExportRejection, bool, error)
}

// AdmissionAuthority authenticates a reconstructed record against its durable
// admission, requires current conformance before work can start or resume,
// and derives the exact import policy bound to that admission's trust profile.
// A private driver-state file is not an authority for any of those decisions.
type AdmissionAuthority interface {
	AuthenticateAdmission(context.Context, domain.InvocationID, exec.StartSpec) error
	AuthenticateStart(context.Context, domain.InvocationID, exec.StartSpec) error
	// AuthenticateImport requires current admission and import policy for a
	// handoff already authenticated as released by Gate. It grants no authority
	// to start or resume a provider and does not require its old launch proof.
	AuthenticateImport(context.Context, domain.InvocationID, exec.StartSpec) error
	// ImportOptions applies current import policy only after Gate authenticates
	// the released handoff. It does not authorize provider execution.
	ImportOptions(context.Context, domain.InvocationID, exec.StartSpec, importer.Options) (importer.Options, error)
	// ImportOptionsRecord reconstructs the exact policy for a released export
	// from immutable admission and resolved-policy records.
	// Mutable daemon configuration may block new work but cannot retarget or
	// strand terminal replay before the publisher applies its current gates.
	ImportOptionsRecord(context.Context, domain.InvocationID, exec.StartSpec, importer.Options) (importer.Options, error)
	// ProtectedPatterns lists the path patterns whose edit would fail this
	// invocation's run at import under publish_strict, derived from the same
	// trust profile ImportOptions reads. The list only informs the writer's
	// prompt; it is no authority, and import re-derives its own patterns.
	ProtectedPatterns(context.Context, domain.InvocationID, exec.StartSpec) ([]string, error)
}

// Artifacts persists the evidence bytes an export released, and records the
// agent claims that name them. The gate hands the export back in a directory
// the driver owns and deletes, so anything a terminal result names must be
// copied into durable storage first: a result naming artifacts no store can
// resolve is an audit trail with no evidence behind it.
type Artifacts interface {
	// PutBlob stores one evidence blob under its content address.
	PutBlob(ctx context.Context, digest domain.Digest, body []byte) error
	// RecordClaims durably persists the complete §5.15 agent claim set for one
	// invocation on its invocation-bound record: each claim's label, artifact
	// identity, digest, provenance (sensitivity class included), and optional
	// inline text survive intact, not the artifact rows alone. It is write-once
	// per invocation: recording the byte-identical set again converges without
	// duplication, while any differing set (a changed label, digest, membership,
	// inline text, or order) is a conflict, never a silent overwrite. An empty
	// set records nothing.
	RecordClaims(ctx context.Context, id domain.InvocationID, claims []domain.AgentClaim) error
	// LookupClaims reconstructs the immutable claim set for terminal recovery.
	// found is false only when no set was recorded for the invocation.
	LookupClaims(ctx context.Context, id domain.InvocationID) (claims []domain.AgentClaim, found bool, err error)
}
