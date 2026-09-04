package domain

import (
	"fmt"
	"time"
)

// Run observation is the operator-facing projection of a run's progress
// (plan §8, issue #394): typed milestones the workflow has durably crossed,
// the current hold (if any) as a closed reason code, and the daemon's last
// observation of each invocation. It is observational, never authority: the
// rows are written beside the workflow facts but no recovery, publication,
// or teardown decision reads them (the ward journal's anti-progress-bit
// position, internal/ward/journal.go). Nothing here can carry live writer
// output, free-text reasons, or a percentage complete: those are
// unrepresentable by construction, following publish.MintRecord.

// DefaultObservationFreshnessWindow is the staleness bound a reader passes
// to DeriveInvocationLiveness when it has no better cadence knowledge. The
// engine refreshes an unchanged observation every 10 seconds while it is
// alive (three refresh chances per window), so an observation older than
// this window means the daemon or the runtime is not currently observing,
// not that the run is idle.
const DefaultObservationFreshnessWindow = 30 * time.Second

// RunMilestoneKind names one typed milestone a run's workflow has durably
// crossed. The zero value "" is invalid by design.
type RunMilestoneKind string

const (
	// MilestoneRunSubmitted: the run, its stage, its reserved invocation,
	// and its dispatch intent committed in one transaction.
	MilestoneRunSubmitted RunMilestoneKind = "run_submitted"
	// MilestoneInvocationAdmitted: the attempt and its execution admission
	// committed; the invocation is authorized to start.
	MilestoneInvocationAdmitted RunMilestoneKind = "invocation_admitted"
	// MilestoneInvocationStarted: the driver accepted Start and the dispatch
	// intent was marked dispatched.
	MilestoneInvocationStarted RunMilestoneKind = "invocation_started"
	// MilestoneExecutionExportRecorded: the write-once completed-execution
	// authority (ExecutionExport) committed.
	MilestoneExecutionExportRecorded RunMilestoneKind = "execution_export_recorded"
	// MilestoneExecutionOutcomeRecorded: the write-once non-export terminal
	// authority (ExecutionOutcome) committed; Outcome carries its class.
	MilestoneExecutionOutcomeRecorded RunMilestoneKind = "execution_outcome_recorded"
	// MilestoneTerminalRecorded: the engine accepted and durably recorded
	// the stage's terminal class; Terminal carries it. This is the
	// lane-independent "terminal collected" fact (the export and outcome
	// milestones above additionally mark the trusted authorities when a
	// driver records them).
	MilestoneTerminalRecorded RunMilestoneKind = "terminal_recorded"
	// MilestonePublicationReady: publication converged and the run's
	// ready-for-final-review outcome committed.
	MilestonePublicationReady RunMilestoneKind = "publication_ready"
	// MilestonePublicationBlocked: publication reached a definitive block;
	// Reason carries the closed cause code.
	MilestonePublicationBlocked RunMilestoneKind = "publication_blocked"
	// MilestoneWorkUnitCompleted: the run's work unit satisfied its declared
	// completion criterion (the PR merged, or the bound issue closed by that
	// merge). The milestone is invocation-only, carrying the same publication
	// invocation as publication_ready; the PR, merge commit, and bound issue
	// stay on the re-gated work_unit_completions row, the single authority.
	MilestoneWorkUnitCompleted RunMilestoneKind = "work_unit_completed"
)

// AllRunMilestoneKinds is the single registration point for milestone kinds.
var AllRunMilestoneKinds = []RunMilestoneKind{
	MilestoneRunSubmitted,
	MilestoneInvocationAdmitted,
	MilestoneInvocationStarted,
	MilestoneExecutionExportRecorded,
	MilestoneExecutionOutcomeRecorded,
	MilestoneTerminalRecorded,
	MilestonePublicationReady,
	MilestonePublicationBlocked,
	MilestoneWorkUnitCompleted,
}

func (k RunMilestoneKind) valid() bool {
	switch k {
	case MilestoneRunSubmitted, MilestoneInvocationAdmitted,
		MilestoneInvocationStarted, MilestoneExecutionExportRecorded,
		MilestoneExecutionOutcomeRecorded, MilestoneTerminalRecorded,
		MilestonePublicationReady, MilestonePublicationBlocked,
		MilestoneWorkUnitCompleted:
		return true
	default:
		return false
	}
}

// ObservedInvocationStatus is the observation-side mirror of the driver
// lifecycle vocabulary (exec.Status). The domain cannot import exec, so the
// engine maps between the two with an exhaustive switch; the members are
// deliberately identical. The zero value "" is invalid by design.
type ObservedInvocationStatus string

const (
	ObservedStatusPending   ObservedInvocationStatus = "pending"
	ObservedStatusRunning   ObservedInvocationStatus = "running"
	ObservedStatusCompleted ObservedInvocationStatus = "completed"
	ObservedStatusFailed    ObservedInvocationStatus = "failed"
	ObservedStatusCanceled  ObservedInvocationStatus = "canceled"
	ObservedStatusBlocked   ObservedInvocationStatus = "blocked"
	ObservedStatusGone      ObservedInvocationStatus = "gone"
)

// AllObservedInvocationStatuses is the single registration point for
// observed invocation statuses.
var AllObservedInvocationStatuses = []ObservedInvocationStatus{
	ObservedStatusPending,
	ObservedStatusRunning,
	ObservedStatusCompleted,
	ObservedStatusFailed,
	ObservedStatusCanceled,
	ObservedStatusBlocked,
	ObservedStatusGone,
}

func (s ObservedInvocationStatus) valid() bool {
	switch s {
	case ObservedStatusPending, ObservedStatusRunning, ObservedStatusCompleted,
		ObservedStatusFailed, ObservedStatusCanceled, ObservedStatusBlocked, ObservedStatusGone:
		return true
	default:
		return false
	}
}

// Concluded reports whether the observed status is a committed terminal
// outcome (completed, failed, canceled, or blocked). Gone is not concluded:
// the session is lost but the terminal class is still the workflow's to
// record. Dispatch switch without default so the exhaustive linter forces a
// new member to be classified.
func (s ObservedInvocationStatus) Concluded() bool {
	switch s {
	case ObservedStatusCompleted, ObservedStatusFailed, ObservedStatusCanceled, ObservedStatusBlocked:
		return true
	case ObservedStatusPending, ObservedStatusRunning, ObservedStatusGone:
		return false
	}
	return false // invalid zero value
}

// RunHoldReason is the closed vocabulary of operator-safe hold and block
// causes. It is deliberately a code, never free text: the mapping from
// errors and workflow verdicts to a code lives in the engine, and anything
// that does not classify records no reason at all. Adding a member is a
// contract change. The zero value "" is invalid by design.
type RunHoldReason string

// Definitive production publication reasons are shared by the workflow that
// authors a terminal blocked item and the read boundary that authenticates
// its typed observation. Keeping the prose-to-code map here prevents either
// side from widening the other's closed contract independently.
const (
	PublicationBlockRecipeRevoked = "Current trust no longer approves the admitted project-image recipe."
	PublicationBlockVerification  = "Verification or current policy findings blocked production publication."
	PublicationBlockTrust         = "Current trust state definitively blocked publication."
	PublicationBlockBaseAdvanced  = "The target base advanced after admission; rerun and reverify against the current base."
)

const (
	// HoldOperationStopped: the durable operator stop is in force.
	HoldOperationStopped RunHoldReason = "operation_stopped"
	// HoldBlockingSystemHealth: an open blocking system_health item holds
	// unattended admission.
	HoldBlockingSystemHealth RunHoldReason = "blocking_system_health"
	// HoldInputUnavailable: a required stage input blob is not currently
	// materializable.
	HoldInputUnavailable RunHoldReason = "input_unavailable"
	// HoldBackendNotConformant: the runner backend lacks a current
	// conformance proof matching the admission configuration.
	HoldBackendNotConformant RunHoldReason = "backend_not_conformant"
	// HoldAdmissionPolicyRefused: current admission policy (floors,
	// credential modes, waivers, trust-profile currency, repository
	// identity, path boundary) refuses to start or accept the work.
	HoldAdmissionPolicyRefused RunHoldReason = "admission_policy_refused"
	// HoldBackupProtectionUnready: the encrypted-backup posture (health,
	// checkpoint currency, closure, restore test) does not currently admit
	// unattended work.
	HoldBackupProtectionUnready RunHoldReason = "backup_protection_unready"
	// HoldRepositoryUntrusted: the target repository has no current trust
	// profile activation.
	HoldRepositoryUntrusted RunHoldReason = "repository_untrusted"
	// HoldProviderAuthorityUnavailable: provider-side authority (the App
	// installation janitor's coverage) is not currently published.
	HoldProviderAuthorityUnavailable RunHoldReason = "provider_authority_unavailable"
	// HoldAttendedModeActive: the daemon is running attended, so a durable
	// unattended intent stays pending by design.
	HoldAttendedModeActive RunHoldReason = "attended_mode_active"
	// HoldPublicationEnvironment: a transient environmental failure (DNS,
	// runtime, forge, filesystem) held publication for a bounded retry.
	HoldPublicationEnvironment RunHoldReason = "publication_environment"
	// HoldExternalConflict: a conflicting or foreign external resource
	// (branch, pull request) holds the committed publication identity.
	HoldExternalConflict RunHoldReason = "external_conflict"
	// HoldRecipeRevoked: current trust no longer approves the admitted
	// project-image recipe.
	HoldRecipeRevoked RunHoldReason = "recipe_revoked"
	// HoldVerificationFindings: verification or current policy findings
	// block publication.
	HoldVerificationFindings RunHoldReason = "verification_findings"
	// HoldTrustBlocked: current trust state definitively blocked
	// publication.
	HoldTrustBlocked RunHoldReason = "trust_blocked"
	// HoldBaseAdvanced: the target base advanced after admission; the run
	// must be rerun against the current base.
	HoldBaseAdvanced RunHoldReason = "base_advanced"
	// HoldIdentityParallelism: the selected provider identity is already
	// running its experimentally established maximum number of executions.
	HoldIdentityParallelism RunHoldReason = "identity_parallelism"
)

// AllRunHoldReasons is the single registration point for hold reasons.
var AllRunHoldReasons = []RunHoldReason{
	HoldOperationStopped,
	HoldBlockingSystemHealth,
	HoldInputUnavailable,
	HoldBackendNotConformant,
	HoldAdmissionPolicyRefused,
	HoldBackupProtectionUnready,
	HoldRepositoryUntrusted,
	HoldProviderAuthorityUnavailable,
	HoldAttendedModeActive,
	HoldPublicationEnvironment,
	HoldExternalConflict,
	HoldRecipeRevoked,
	HoldVerificationFindings,
	HoldTrustBlocked,
	HoldBaseAdvanced,
	HoldIdentityParallelism,
}

func (r RunHoldReason) valid() bool {
	switch r {
	case HoldOperationStopped, HoldBlockingSystemHealth, HoldInputUnavailable,
		HoldBackendNotConformant, HoldAdmissionPolicyRefused,
		HoldBackupProtectionUnready, HoldRepositoryUntrusted,
		HoldProviderAuthorityUnavailable, HoldAttendedModeActive,
		HoldPublicationEnvironment, HoldExternalConflict, HoldRecipeRevoked,
		HoldVerificationFindings, HoldTrustBlocked, HoldBaseAdvanced,
		HoldIdentityParallelism:
		return true
	default:
		return false
	}
}

// DefinitivePublicationBlockReason maps the exact durable terminal-item
// reason onto its operator-safe code. Transient publication holds deliberately
// have no member here, even when they use the same AttentionType.
func DefinitivePublicationBlockReason(reason string) (RunHoldReason, bool) {
	switch reason {
	case PublicationBlockRecipeRevoked:
		return HoldRecipeRevoked, true
	case PublicationBlockVerification:
		return HoldVerificationFindings, true
	case PublicationBlockTrust:
		return HoldTrustBlocked, true
	case PublicationBlockBaseAdvanced:
		return HoldBaseAdvanced, true
	}
	return "", false
}

// RunMilestone is one appended, first-observation-wins timeline entry: the
// instant a run's workflow durably crossed a typed milestone. Detail fields
// are kind-scoped (Validate enforces presence and absence), so a milestone
// can never carry vocabulary its kind does not declare.
type RunMilestone struct {
	RunID RunID            `json:"run_id"`
	Kind  RunMilestoneKind `json:"kind"`
	// InvocationID scopes invocation-level milestones; nil renders explicit
	// null for a milestone that is not about one invocation
	// (pointer-for-optional).
	InvocationID *InvocationID `json:"invocation_id"`
	// Terminal is the accepted terminal class; present exactly for
	// terminal_recorded.
	Terminal *ObservedInvocationStatus `json:"terminal"`
	// Outcome is the non-export terminal authority's class; present exactly
	// for execution_outcome_recorded.
	Outcome *ExecutionOutcomeStatus `json:"outcome"`
	// Reason is the closed block cause; present exactly for
	// publication_blocked.
	Reason *RunHoldReason `json:"reason"`
	// RecordedAt is the UTC instant the milestone's transaction observed.
	RecordedAt time.Time `json:"recorded_at"`
}

// Validate reports whether the milestone is structurally sound, including
// the kind-scoped detail fields. Kind dispatch omits default so the
// exhaustive linter forces a new kind to declare its detail contract; the
// trailing return rejects the invalid zero value.
func (m RunMilestone) Validate() error {
	if m.RunID == "" {
		return fmt.Errorf("run milestone run_id: %w", ErrEmptyID)
	}
	if !m.Kind.valid() {
		return fmt.Errorf("run milestone kind %q: %w", m.Kind, ErrInvalidRunMilestoneKind)
	}
	if m.InvocationID != nil && *m.InvocationID == "" {
		return fmt.Errorf("run milestone %s invocation_id: %w", m.Kind, ErrEmptyID)
	}
	if m.RecordedAt.IsZero() {
		return fmt.Errorf("run milestone %s recorded_at: %w", m.Kind, ErrMissingTimestamp)
	}
	if m.RecordedAt.Location() != time.UTC {
		return fmt.Errorf("run milestone %s recorded_at: %w", m.Kind, ErrTimestampNotUTC)
	}
	if m.Terminal != nil && !m.Terminal.valid() {
		return fmt.Errorf("run milestone %s terminal %q: %w",
			m.Kind, *m.Terminal, ErrInvalidObservedStatus)
	}
	if m.Outcome != nil && !m.Outcome.valid() {
		return fmt.Errorf("run milestone %s outcome %q: %w",
			m.Kind, *m.Outcome, ErrInvalidExecOutcome)
	}
	if m.Reason != nil && !m.Reason.valid() {
		return fmt.Errorf("run milestone %s reason %q: %w",
			m.Kind, *m.Reason, ErrInvalidRunHoldReason)
	}
	requireDetail := func(name string, present, want bool) error {
		if present == want {
			return nil
		}
		if want {
			return fmt.Errorf("run milestone %s requires %s: %w", m.Kind, name, ErrMilestoneDetailMismatch)
		}
		return fmt.Errorf("run milestone %s carries %s: %w", m.Kind, name, ErrMilestoneDetailMismatch)
	}
	check := func(wantInvocation, wantTerminal, wantOutcome, wantReason bool) error {
		if err := requireDetail("invocation_id", m.InvocationID != nil, wantInvocation); err != nil {
			return err
		}
		if err := requireDetail("terminal", m.Terminal != nil, wantTerminal); err != nil {
			return err
		}
		if err := requireDetail("outcome", m.Outcome != nil, wantOutcome); err != nil {
			return err
		}
		return requireDetail("reason", m.Reason != nil, wantReason)
	}
	switch m.Kind {
	case MilestoneRunSubmitted, MilestoneInvocationAdmitted,
		MilestoneInvocationStarted, MilestoneExecutionExportRecorded:
		return check(true, false, false, false)
	case MilestoneExecutionOutcomeRecorded:
		return check(true, false, true, false)
	case MilestoneTerminalRecorded:
		if err := check(true, true, false, false); err != nil {
			return err
		}
		// The accepted terminal class is a committed outcome or a proven
		// lost session, never a live state.
		if !m.Terminal.Concluded() && *m.Terminal != ObservedStatusGone {
			return fmt.Errorf("run milestone %s terminal %q: %w",
				m.Kind, *m.Terminal, ErrInvalidObservedStatus)
		}
		return nil
	case MilestonePublicationReady, MilestoneWorkUnitCompleted:
		return check(true, false, false, false)
	case MilestonePublicationBlocked:
		return check(true, false, false, true)
	}
	return fmt.Errorf("run milestone kind %q: %w", m.Kind, ErrInvalidRunMilestoneKind)
}

// InvocationObservation is the daemon's last observation of one invocation:
// what the driver reported (status), whether the driver currently observed
// the underlying execution (live), and the daemon-clock instant. Last write
// wins; history is not kept, because the milestone timeline carries the
// durable facts and this row exists only to answer "when did the daemon
// last look, and what did it see".
type InvocationObservation struct {
	InvocationID InvocationID             `json:"invocation_id"`
	RunID        RunID                    `json:"run_id"`
	Status       ObservedInvocationStatus `json:"status"`
	// Live reports whether the driver observed the execution itself (a
	// running container or process), as opposed to deriving status from
	// durable records alone. A restarted daemon reports live=false until
	// the runtime is re-observed.
	Live bool `json:"live"`
	// ObservedAt is the UTC daemon-clock instant of this observation.
	ObservedAt time.Time `json:"observed_at"`
}

// Validate reports whether the observation is structurally sound.
func (o InvocationObservation) Validate() error {
	if o.InvocationID == "" {
		return fmt.Errorf("invocation observation invocation_id: %w", ErrEmptyID)
	}
	if o.RunID == "" {
		return fmt.Errorf("invocation observation %s run_id: %w", o.InvocationID, ErrEmptyID)
	}
	if !o.Status.valid() {
		return fmt.Errorf("invocation observation %s status %q: %w",
			o.InvocationID, o.Status, ErrInvalidObservedStatus)
	}
	if o.ObservedAt.IsZero() {
		return fmt.Errorf("invocation observation %s observed_at: %w",
			o.InvocationID, ErrMissingTimestamp)
	}
	if o.ObservedAt.Location() != time.UTC {
		return fmt.Errorf("invocation observation %s observed_at: %w",
			o.InvocationID, ErrTimestampNotUTC)
	}
	// Liveness beside a concluded or lost status is unrepresentable: a
	// session that ended or vanished has no execution left to observe.
	if o.Live && (o.Status.Concluded() || o.Status == ObservedStatusGone) {
		return fmt.Errorf("invocation observation %s live with status %q: %w",
			o.InvocationID, o.Status, ErrObservationInconsistent)
	}
	return nil
}

// RunHoldObservation is the run's current hold, if any: the closed reason
// code and the observation span of the current cause. One row per run, last
// cause wins; a reason change resets FirstObservedAt, and any forward
// milestone clears the row.
type RunHoldObservation struct {
	RunID RunID `json:"run_id"`
	// InvocationID scopes the hold when it concerns one invocation; nil
	// renders explicit null (pointer-for-optional).
	InvocationID *InvocationID `json:"invocation_id"`
	Reason       RunHoldReason `json:"reason"`
	// FirstObservedAt is the UTC instant the current cause was first
	// observed; LastObservedAt the most recent.
	FirstObservedAt time.Time `json:"first_observed_at"`
	LastObservedAt  time.Time `json:"last_observed_at"`
}

// Validate reports whether the hold observation is structurally sound.
func (h RunHoldObservation) Validate() error {
	if h.RunID == "" {
		return fmt.Errorf("run hold observation run_id: %w", ErrEmptyID)
	}
	if h.InvocationID != nil && *h.InvocationID == "" {
		return fmt.Errorf("run hold observation %s invocation_id: %w", h.RunID, ErrEmptyID)
	}
	if !h.Reason.valid() {
		return fmt.Errorf("run hold observation %s reason %q: %w",
			h.RunID, h.Reason, ErrInvalidRunHoldReason)
	}
	for name, ts := range map[string]time.Time{
		"first_observed_at": h.FirstObservedAt,
		"last_observed_at":  h.LastObservedAt,
	} {
		if ts.IsZero() {
			return fmt.Errorf("run hold observation %s %s: %w", h.RunID, name, ErrMissingTimestamp)
		}
		if ts.Location() != time.UTC {
			return fmt.Errorf("run hold observation %s %s: %w", h.RunID, name, ErrTimestampNotUTC)
		}
	}
	if h.LastObservedAt.Before(h.FirstObservedAt) {
		return fmt.Errorf("run hold observation %s span reads backwards: %w",
			h.RunID, ErrTimestampOutOfOrder)
	}
	return nil
}

// InvocationLiveness is the derived answer to "is this invocation being
// observed right now": never stored, always computed from the last
// observation against a freshness window, so a stopped daemon yields an
// observation gap structurally instead of a stale "live" verdict.
type InvocationLiveness string

const (
	// LivenessLive: a fresh observation reports the execution itself as
	// currently observed.
	LivenessLive InvocationLiveness = "observed_live"
	// LivenessIdle: a fresh observation found no live execution and no
	// concluded outcome (queued, between observations, or a lost session
	// awaiting its recorded outcome).
	LivenessIdle InvocationLiveness = "observed_idle"
	// LivenessGap: the last observation is older than the freshness window;
	// the daemon or the runtime is not currently observing.
	LivenessGap InvocationLiveness = "observation_gap"
	// LivenessTerminal: the last observation reports a concluded outcome;
	// there is nothing left to observe.
	LivenessTerminal InvocationLiveness = "terminal"
	// LivenessUnobserved: the daemon has never observed this invocation.
	LivenessUnobserved InvocationLiveness = "never_observed"
)

// AllInvocationLivenesses is the single registration point for liveness
// classes.
var AllInvocationLivenesses = []InvocationLiveness{
	LivenessLive,
	LivenessIdle,
	LivenessGap,
	LivenessTerminal,
	LivenessUnobserved,
}

func (l InvocationLiveness) valid() bool {
	switch l {
	case LivenessLive, LivenessIdle, LivenessGap, LivenessTerminal,
		LivenessUnobserved:
		return true
	default:
		return false
	}
}

// DeriveInvocationLiveness classifies one invocation's observation state at
// asOf. A nil observation is never_observed; a concluded status is terminal
// regardless of age (a committed outcome does not decay); otherwise
// freshness against the window decides between an observation gap and the
// live/idle pair. An observation dated after asOf is a gap too: the clocks
// disagree (a step-back, a VM restore), so "currently observed" cannot be
// claimed, and letting the negative age pass the window would keep an
// arbitrarily old live verdict standing until wall time caught up.
func DeriveInvocationLiveness(
	obs *InvocationObservation, asOf time.Time, window time.Duration,
) InvocationLiveness {
	switch {
	case obs == nil:
		return LivenessUnobserved
	case obs.Status.Concluded():
		return LivenessTerminal
	case obs.ObservedAt.After(asOf):
		return LivenessGap
	case asOf.Sub(obs.ObservedAt) > window:
		return LivenessGap
	case obs.Live:
		return LivenessLive
	default:
		return LivenessIdle
	}
}

// RunObservation is the read-surface aggregate an operator client consumes:
// the milestone timeline, the current hold, and the last invocation
// observations for one run. Elapsed time and last observation derive from
// it; no completion fraction exists or can be added without a contract
// change.
type RunObservation struct {
	RunID      RunID          `json:"run_id"`
	Milestones []RunMilestone `json:"milestones"`
	// Hold is the current hold; nil renders explicit null when the run is
	// not held (pointer-for-optional).
	Hold        *RunHoldObservation     `json:"hold"`
	Invocations []InvocationObservation `json:"invocations"`
}

// Validate reports whether the aggregate is structurally sound and
// internally consistent: every entry validates and names the aggregate's
// run.
func (o RunObservation) Validate() error {
	if o.RunID == "" {
		return fmt.Errorf("run observation run_id: %w", ErrEmptyID)
	}
	for i, m := range o.Milestones {
		if err := m.Validate(); err != nil {
			return fmt.Errorf("run observation milestones[%d]: %w", i, err)
		}
		if m.RunID != o.RunID {
			return fmt.Errorf("run observation milestones[%d] names run %q: %w",
				i, m.RunID, ErrParentKeyMismatch)
		}
	}
	if o.Hold != nil {
		if err := o.Hold.Validate(); err != nil {
			return fmt.Errorf("run observation hold: %w", err)
		}
		if o.Hold.RunID != o.RunID {
			return fmt.Errorf("run observation hold names run %q: %w",
				o.Hold.RunID, ErrParentKeyMismatch)
		}
	}
	for i, obs := range o.Invocations {
		if err := obs.Validate(); err != nil {
			return fmt.Errorf("run observation invocations[%d]: %w", i, err)
		}
		if obs.RunID != o.RunID {
			return fmt.Errorf("run observation invocations[%d] names run %q: %w",
				i, obs.RunID, ErrParentKeyMismatch)
		}
	}
	return nil
}

// SubmittedAt returns the run_submitted instant, if observed.
func (o RunObservation) SubmittedAt() (time.Time, bool) {
	for _, m := range o.Milestones {
		if m.Kind == MilestoneRunSubmitted {
			return m.RecordedAt, true
		}
	}
	return time.Time{}, false
}

// ConcludedAt returns the instant the run's final outcome was recorded, if
// one was: the latest of the publication, completion, and terminal
// milestones.
func (o RunObservation) ConcludedAt() (time.Time, bool) {
	var (
		latest time.Time
		found  bool
	)
	for _, m := range o.Milestones {
		switch m.Kind {
		case MilestoneTerminalRecorded, MilestonePublicationReady,
			MilestonePublicationBlocked, MilestoneWorkUnitCompleted:
			if m.RecordedAt.After(latest) {
				latest = m.RecordedAt
				found = true
			}
		case MilestoneRunSubmitted, MilestoneInvocationAdmitted,
			MilestoneInvocationStarted, MilestoneExecutionExportRecorded,
			MilestoneExecutionOutcomeRecorded:
		}
	}
	return latest, found
}

// Elapsed returns the run's elapsed clock at asOf: submission to conclusion
// when concluded, submission to asOf otherwise. False before submission is
// observed, and false when the endpoints read backwards (a clock stepped
// back between the instants, the same correction DeriveInvocationLiveness
// treats as a gap): a negative span is not an elapsed clock, and reporting
// one would present clock skew as observation.
func (o RunObservation) Elapsed(asOf time.Time) (time.Duration, bool) {
	submitted, ok := o.SubmittedAt()
	if !ok {
		return 0, false
	}
	end := asOf
	if concluded, ok := o.ConcludedAt(); ok {
		end = concluded
	}
	if end.Before(submitted) {
		return 0, false
	}
	return end.Sub(submitted), true
}

// LastObservedAt returns the most recent instant anything about the run was
// observed: milestone, hold, or invocation observation.
func (o RunObservation) LastObservedAt() (time.Time, bool) {
	var (
		latest time.Time
		found  bool
	)
	consider := func(ts time.Time) {
		if ts.After(latest) {
			latest = ts
			found = true
		}
	}
	for _, m := range o.Milestones {
		consider(m.RecordedAt)
	}
	if o.Hold != nil {
		consider(o.Hold.LastObservedAt)
	}
	for _, obs := range o.Invocations {
		consider(obs.ObservedAt)
	}
	return latest, found
}

// RunOutcome is the operator-facing classification of a run's final result.
// It is derived fresh from the milestone timeline and is never workflow
// authority persisted for the engine to read back.
type RunOutcome string

const (
	// RunOutcomeUnobserved marks a run with no observation history at all:
	// a run created before migration 0024, which deliberately backfills no
	// milestones, and with no nonterminal invocation observation either. It
	// is non-final with pending-shaped detail, distinct from pending so a
	// legacy terminal run is not rendered as still in flight; a legacy run
	// still executing across the upgrade has a nonterminal invocation
	// observation and reports pending instead.
	RunOutcomeUnobserved RunOutcome = "unobserved"
	RunOutcomePending    RunOutcome = "pending"
	RunOutcomePublished  RunOutcome = "published"
	RunOutcomeBlocked    RunOutcome = "blocked"
	RunOutcomeFailed     RunOutcome = "failed"
	RunOutcomeLost       RunOutcome = "lost"
	// RunOutcomeCompleted marks a published run whose work unit then
	// satisfied its completion criterion: the PR merged. It is a distinct
	// terminal outcome rather than "published plus a milestone" so every
	// consumer keeps switching on one field instead of re-deriving it.
	RunOutcomeCompleted RunOutcome = "completed"
)

// AllRunOutcomes is the single registration point for run outcomes.
var AllRunOutcomes = []RunOutcome{
	RunOutcomeUnobserved,
	RunOutcomePending,
	RunOutcomePublished,
	RunOutcomeBlocked,
	RunOutcomeFailed,
	RunOutcomeLost,
	RunOutcomeCompleted,
}

func (o RunOutcome) valid() bool {
	switch o {
	case RunOutcomeUnobserved, RunOutcomePending, RunOutcomePublished,
		RunOutcomeBlocked, RunOutcomeFailed, RunOutcomeLost, RunOutcomeCompleted:
		return true
	default:
		return false
	}
}

// RunConclusion is the run's daemon-derived outcome as the timeline currently
// reads. A current hold is independent and can coexist with Pending; Reason is
// present only for a definitive publication block.
type RunConclusion struct {
	Outcome  RunOutcome
	Reason   *RunHoldReason
	Terminal *ObservedInvocationStatus
	Final    bool
}

// Validate reports whether the conclusion carries exactly the detail its
// outcome declares.
func (c RunConclusion) Validate() error {
	if !c.Outcome.valid() {
		return fmt.Errorf("run conclusion outcome %q: %w", c.Outcome, ErrInvalidRunOutcome)
	}
	check := func(wantReason, wantTerminal, wantFinal bool) error {
		switch {
		case (c.Reason != nil) != wantReason:
			return fmt.Errorf("run conclusion %s reason: %w", c.Outcome, ErrRunOutcomeDetailMismatch)
		case (c.Terminal != nil) != wantTerminal:
			return fmt.Errorf("run conclusion %s terminal: %w", c.Outcome, ErrRunOutcomeDetailMismatch)
		case c.Final != wantFinal:
			return fmt.Errorf("run conclusion %s final: %w", c.Outcome, ErrRunOutcomeDetailMismatch)
		}
		if c.Reason != nil && !c.Reason.valid() {
			return fmt.Errorf("run conclusion %s reason %q: %w",
				c.Outcome, *c.Reason, ErrInvalidRunHoldReason)
		}
		if c.Terminal != nil && !c.Terminal.valid() {
			return fmt.Errorf("run conclusion %s terminal %q: %w",
				c.Outcome, *c.Terminal, ErrInvalidObservedStatus)
		}
		return nil
	}
	switch c.Outcome {
	case RunOutcomeUnobserved, RunOutcomePending:
		return check(false, false, false)
	case RunOutcomePublished, RunOutcomeCompleted:
		return check(false, false, true)
	case RunOutcomeBlocked:
		return check(true, false, true)
	case RunOutcomeFailed, RunOutcomeLost:
		return check(false, true, true)
	}
	return fmt.Errorf("run conclusion outcome %q: %w", c.Outcome, ErrInvalidRunOutcome)
}

// ConcludeRun classifies a run from its milestone timeline. A definitive
// publication block outranks ready because it is the actionable result; a
// work-unit completion outranks both because the merge is the last word.
func ConcludeRun(observation RunObservation) RunConclusion {
	// An empty history is a pre-0024 legacy run: 0024 backfills no
	// milestones, so classify it as unobserved rather than pending. The
	// observation history is milestones and invocation observations
	// together: a run still in flight across the upgrade gains liveness
	// observations, not milestones (observeInvocation records observations,
	// never milestones), so a nonterminal invocation observation is already
	// in-flight evidence and classifies as pending. Only a run with neither
	// a milestone nor a nonterminal invocation observation is unobserved.
	if len(observation.Milestones) == 0 {
		if hasNonterminalInvocation(observation.Invocations) {
			return RunConclusion{Outcome: RunOutcomePending}
		}
		return RunConclusion{Outcome: RunOutcomeUnobserved}
	}
	var (
		terminal           *ObservedInvocationStatus
		terminalInvocation InvocationID
		blocked            *RunHoldReason
		published          bool
		completed          bool
	)
	for _, milestone := range observation.Milestones {
		switch milestone.Kind {
		case MilestonePublicationReady:
			published = true
		case MilestoneWorkUnitCompleted:
			completed = true
		case MilestonePublicationBlocked:
			blocked = milestone.Reason
		case MilestoneTerminalRecorded:
			terminal = milestone.Terminal
			terminalInvocation = *milestone.InvocationID
		case MilestoneInvocationAdmitted, MilestoneInvocationStarted:
			if terminal != nil && *milestone.InvocationID != terminalInvocation {
				terminal = nil
			}
		case MilestoneRunSubmitted, MilestoneExecutionExportRecorded,
			MilestoneExecutionOutcomeRecorded:
		}
	}
	switch {
	case completed:
		return RunConclusion{Outcome: RunOutcomeCompleted, Final: true}
	case blocked != nil:
		return RunConclusion{Outcome: RunOutcomeBlocked, Reason: blocked, Final: true}
	case published:
		return RunConclusion{Outcome: RunOutcomePublished, Final: true}
	case terminal == nil:
		return RunConclusion{Outcome: RunOutcomePending}
	}
	outcome, final := concludeTerminalOutcome(*terminal)
	if !final {
		return RunConclusion{Outcome: outcome}
	}
	return RunConclusion{Outcome: outcome, Terminal: terminal, Final: true}
}

// hasNonterminalInvocation reports whether any invocation observation is
// still in flight (pending or running). Such evidence means the run has
// been observed and is active even when no milestone has been recorded, as
// for a pre-0024 run still executing across the upgrade. Dispatch switch
// without default so the exhaustive linter forces a new member to be
// classified; the trailing return covers the invalid zero value.
func hasNonterminalInvocation(observations []InvocationObservation) bool {
	for _, obs := range observations {
		switch obs.Status {
		case ObservedStatusPending, ObservedStatusRunning:
			return true
		case ObservedStatusCompleted, ObservedStatusFailed,
			ObservedStatusCanceled, ObservedStatusBlocked, ObservedStatusGone:
		}
	}
	return false
}

func concludeTerminalOutcome(terminal ObservedInvocationStatus) (RunOutcome, bool) {
	switch terminal {
	case ObservedStatusCompleted, ObservedStatusBlocked:
		// A blocked stage waits on a human answer; the run is not final.
		return RunOutcomePending, false
	case ObservedStatusFailed, ObservedStatusCanceled:
		return RunOutcomeFailed, true
	case ObservedStatusGone:
		return RunOutcomeLost, true
	case ObservedStatusPending, ObservedStatusRunning:
		return RunOutcomePending, false
	}
	return RunOutcomePending, false
}

// PublicationReadyStands reports whether the timeline's publication_ready
// authority is current: a ready milestone exists and none of the definitive
// publication_blocked milestones follows the last one. The sync boundary
// serves work_unit_completed only over such a history, and the recorders
// consult the same rule before mirroring a completion, because a milestone
// is append-only and one the boundary refuses would exclude the run for
// good. Dispatch switch without default so the exhaustive linter forces a
// new kind to be classified.
func PublicationReadyStands(observation RunObservation) bool {
	lastReady, lastBlock := -1, -1
	for index, milestone := range observation.Milestones {
		switch milestone.Kind {
		case MilestonePublicationReady:
			lastReady = index
		case MilestonePublicationBlocked:
			lastBlock = index
		case MilestoneRunSubmitted, MilestoneInvocationAdmitted,
			MilestoneInvocationStarted, MilestoneExecutionExportRecorded,
			MilestoneExecutionOutcomeRecorded, MilestoneTerminalRecorded,
			MilestoneWorkUnitCompleted:
		}
	}
	return lastReady >= 0 && lastReady > lastBlock
}

// RunLifecycle is the operator-facing split of the runs list: whether the
// daemon or the operator still has something to do with the run. It is a
// separate derivation from RunConclusion.Final, which is engine input meaning
// "the daemon will not change this outcome on its own"; a blocked run is
// final yet active, because the operator still holds a decision. The zero
// value "" is invalid by design.
type RunLifecycle string

const (
	RunLifecycleActive   RunLifecycle = "active"
	RunLifecycleFinished RunLifecycle = "finished"
)

// AllRunLifecycles is the single registration point for lifecycles.
var AllRunLifecycles = []RunLifecycle{RunLifecycleActive, RunLifecycleFinished}

func (l RunLifecycle) valid() bool {
	switch l {
	case RunLifecycleActive, RunLifecycleFinished:
		return true
	default:
		return false
	}
}

// LifecycleOf derives a run's lifecycle from its conclusion and whether a
// later attempt superseded it. A superseded run is finished whatever its
// outcome: the engine only retries a final run, and the successor now owns
// the work. Otherwise completed, failed, lost, and unobserved are finished
// (unobserved is a pre-0024 legacy run, chosen in #733 so it is never shown
// in flight; a legacy run still running gains a liveness observation and
// becomes pending). Pending, published with no completion yet, and blocked
// with no successor are active: something is still pending for the daemon or
// the operator. Dispatch switch without default so the exhaustive linter
// forces a new outcome to be classified; the trailing return covers the
// invalid zero value.
func LifecycleOf(conclusion RunConclusion, superseded bool) RunLifecycle {
	if superseded {
		return RunLifecycleFinished
	}
	switch conclusion.Outcome {
	case RunOutcomeCompleted, RunOutcomeFailed, RunOutcomeLost, RunOutcomeUnobserved:
		return RunLifecycleFinished
	case RunOutcomePending, RunOutcomePublished, RunOutcomeBlocked:
		return RunLifecycleActive
	}
	return RunLifecycleFinished
}
