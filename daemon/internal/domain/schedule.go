package domain

import (
	"fmt"
	"time"
)

// The durable scheduler's vocabulary (plan §5.16, issue #442): every durable
// deferred check — PR watches, deadlines, subject-bound polls — is a
// Schedule of a closed kind, fired as occurrences whose identity is
// (schedule_id, generation, nominal_fire_at). The domain owns the closed
// unions, the aggregate's validation, and the trusted event constructor; the
// scheduler component owns fire-time validation and transactional
// consumption, and the store persists the aggregate (synced) beside its
// internal timer and occurrence bookkeeping. Firing never extends or
// preserves authority: an event carries identity and expectations only,
// never credentials, tokens, or an authority grant.

// ScheduleKind names one member of the closed union of durable schedule
// kinds. 1B implements only the kinds with 1B consumers; adding a member is
// a kind:contract change. The zero value "" is invalid by design.
type ScheduleKind string

const (
	// SchedulePRChecksDeadline: one-shot deadline armed at publication; fires
	// when the ready_for_final_review item is still open at the deadline.
	SchedulePRChecksDeadline ScheduleKind = "pr_checks_deadline"
	// ScheduleReviewWaitThreshold: one-shot deadline armed at publication;
	// fires when no concluding decision arrived within the review-wait
	// threshold.
	ScheduleReviewWaitThreshold ScheduleKind = "review_wait_threshold"
	// ScheduleBaseAdvanceWatch: recurring staleness watch over a
	// ready_for_final_review item's target base; its consumer is the
	// base-freshness fact on the item, maintained until merge or close.
	ScheduleBaseAdvanceWatch ScheduleKind = "base_advance_watch"
	// ScheduleInstallationPoll: recurring poll bound to a pending
	// installation-or-expansion intent (§5.5, §10), expiring with the
	// envelope. Its handler observes readiness only; promotion stays an
	// operator-driven onboarding act.
	ScheduleInstallationPoll ScheduleKind = "installation_poll"
	// ScheduleDoctor: the permanent trusted-config doctor job (§10). Not
	// proposable, no expiry.
	ScheduleDoctor ScheduleKind = "doctor"
	// ScheduleJanitor: the permanent trusted-config installation-janitor job
	// (§5.5, §10). Not proposable, no expiry.
	ScheduleJanitor ScheduleKind = "janitor"
)

// AllScheduleKinds is the single registration point for schedule kinds.
var AllScheduleKinds = []ScheduleKind{
	SchedulePRChecksDeadline,
	ScheduleReviewWaitThreshold,
	ScheduleBaseAdvanceWatch,
	ScheduleInstallationPoll,
	ScheduleDoctor,
	ScheduleJanitor,
}

func (k ScheduleKind) valid() bool {
	switch k {
	case SchedulePRChecksDeadline, ScheduleReviewWaitThreshold,
		ScheduleBaseAdvanceWatch, ScheduleInstallationPoll,
		ScheduleDoctor, ScheduleJanitor:
		return true
	default:
		return false
	}
}

// TrustedConfigJob reports whether the kind is a permanent trusted-config
// job (doctor, janitor): armed from daemon configuration, never proposable,
// no expiry, running in every operating mode (§5.16). Dispatch without
// default so the exhaustive linter forces a new kind to classify itself.
func (k ScheduleKind) TrustedConfigJob() bool {
	switch k {
	case ScheduleDoctor, ScheduleJanitor:
		return true
	case SchedulePRChecksDeadline, ScheduleReviewWaitThreshold,
		ScheduleBaseAdvanceWatch, ScheduleInstallationPoll:
		return false
	}
	return false // invalid zero value
}

// OneShot reports whether the kind is a one-shot deadline (fires at most
// once per generation and then terminates fired-and-handled or explicitly
// resolved) as opposed to a recurring watch, poll, or job.
func (k ScheduleKind) OneShot() bool {
	switch k {
	case SchedulePRChecksDeadline, ScheduleReviewWaitThreshold:
		return true
	case ScheduleBaseAdvanceWatch, ScheduleInstallationPoll,
		ScheduleDoctor, ScheduleJanitor:
		return false
	}
	return false // invalid zero value
}

// EligibleIn reports the kind's operating-mode eligibility (§5.16, §5.7):
// permanent trusted-config jobs run in every operating mode, so the doctor
// and janitor keep their §10 obligations in attended_dev; workload kinds
// require the operating mode their consumer demands. Every 1B workload
// consumer admits both modes today — the watches are read-only observation
// over durable subjects that exist in either mode, and onboarding is
// operator-attended by nature — so the dispatch, not the values, is the
// contract; a consumer that narrows a kind's modes changes this switch and
// the golden eligibility matrix together. An invalid mode is eligible for
// nothing.
func (k ScheduleKind) EligibleIn(mode OperatingMode) bool {
	if !mode.valid() {
		return false
	}
	switch k {
	case ScheduleDoctor, ScheduleJanitor:
		return true
	case SchedulePRChecksDeadline, ScheduleReviewWaitThreshold,
		ScheduleBaseAdvanceWatch, ScheduleInstallationPoll:
		return true
	}
	return false // invalid zero value
}

// subjectType is the subject class the kind binds to. Dispatch without
// default so a new kind must declare its subject contract.
func (k ScheduleKind) subjectType() ScheduleSubjectType {
	switch k {
	case SchedulePRChecksDeadline, ScheduleReviewWaitThreshold,
		ScheduleBaseAdvanceWatch:
		return ScheduleSubjectAttentionItem
	case ScheduleInstallationPoll:
		return ScheduleSubjectInstallationIntent
	case ScheduleDoctor, ScheduleJanitor:
		return ScheduleSubjectTrustedConfig
	}
	return "" // invalid zero value
}

// ScheduleStatus is a schedule's lifecycle state. The zero value "" is
// invalid by design.
type ScheduleStatus string

const (
	// ScheduleArmed: live; the scheduler fires it when due.
	ScheduleArmed ScheduleStatus = "armed"
	// ScheduleFired: a one-shot deadline terminated fired-and-handled.
	ScheduleFired ScheduleStatus = "fired"
	// ScheduleResolved: explicitly resolved before or instead of firing —
	// the awaited condition was satisfied, or the subject concluded.
	ScheduleResolved ScheduleStatus = "resolved"
	// ScheduleExpired: the schedule's validity window ended.
	ScheduleExpired ScheduleStatus = "expired"
)

// AllScheduleStatuses is the single registration point for schedule
// statuses.
var AllScheduleStatuses = []ScheduleStatus{
	ScheduleArmed, ScheduleFired, ScheduleResolved, ScheduleExpired,
}

func (s ScheduleStatus) valid() bool {
	switch s {
	case ScheduleArmed, ScheduleFired, ScheduleResolved, ScheduleExpired:
		return true
	default:
		return false
	}
}

// Terminal reports whether the status ends the schedule's current
// generation; only a re-arm (a new generation) leaves it.
func (s ScheduleStatus) Terminal() bool {
	switch s {
	case ScheduleFired, ScheduleResolved, ScheduleExpired:
		return true
	case ScheduleArmed:
		return false
	}
	return false // invalid zero value
}

// ScheduleResolutionReason is the closed vocabulary of terminal causes. It
// is deliberately a code, never free text. The zero value "" is invalid by
// design.
type ScheduleResolutionReason string

const (
	// ResolutionDeadlineElapsed: a one-shot deadline fired with its awaited
	// transition still outstanding, and the handler recorded that outcome.
	ResolutionDeadlineElapsed ScheduleResolutionReason = "deadline_elapsed"
	// ResolutionConditionSatisfied: the awaited condition arrived (an
	// installation grant observed ready) before expiry.
	ResolutionConditionSatisfied ScheduleResolutionReason = "condition_satisfied"
	// ResolutionSubjectConcluded: the subject the schedule watched is no
	// longer live (the item was decided, dismissed, or superseded); the
	// recorded proof that the condition no longer applies.
	ResolutionSubjectConcluded ScheduleResolutionReason = "subject_concluded"
	// ResolutionIntentExpired: the bounded intent's validity window ended.
	ResolutionIntentExpired ScheduleResolutionReason = "intent_expired"
)

// AllScheduleResolutionReasons is the single registration point for
// resolution reasons.
var AllScheduleResolutionReasons = []ScheduleResolutionReason{
	ResolutionDeadlineElapsed,
	ResolutionConditionSatisfied,
	ResolutionSubjectConcluded,
	ResolutionIntentExpired,
}

func (r ScheduleResolutionReason) valid() bool {
	switch r {
	case ResolutionDeadlineElapsed, ResolutionConditionSatisfied,
		ResolutionSubjectConcluded, ResolutionIntentExpired:
		return true
	default:
		return false
	}
}

// allowedFor reports whether the reason can conclude a schedule with the
// given terminal status: fired carries exactly deadline_elapsed, resolved
// carries a satisfaction or subject-conclusion proof, expired carries
// exactly intent_expired.
func (r ScheduleResolutionReason) allowedFor(status ScheduleStatus) bool {
	switch status {
	case ScheduleFired:
		return r == ResolutionDeadlineElapsed
	case ScheduleResolved:
		return r == ResolutionConditionSatisfied || r == ResolutionSubjectConcluded
	case ScheduleExpired:
		return r == ResolutionIntentExpired
	case ScheduleArmed:
		return false
	}
	return false // invalid zero value
}

// ScheduleResolution records how and when a schedule's generation
// terminated; present exactly on terminal schedules.
type ScheduleResolution struct {
	Reason ScheduleResolutionReason `json:"reason"`
	// RecordedAt is the UTC instant the terminating transaction observed.
	RecordedAt time.Time `json:"recorded_at"`
}

// ScheduleSubjectType is the class of durable subject a schedule is bound
// to. The zero value "" is invalid by design.
type ScheduleSubjectType string

const (
	ScheduleSubjectAttentionItem      ScheduleSubjectType = "attention_item"
	ScheduleSubjectInstallationIntent ScheduleSubjectType = "installation_intent"
	// ScheduleSubjectTrustedConfig: the daemon's own trusted configuration;
	// permanent jobs have no narrower subject whose existence could lapse.
	ScheduleSubjectTrustedConfig ScheduleSubjectType = "trusted_config"
)

// AllScheduleSubjectTypes is the single registration point for schedule
// subject types.
var AllScheduleSubjectTypes = []ScheduleSubjectType{
	ScheduleSubjectAttentionItem,
	ScheduleSubjectInstallationIntent,
	ScheduleSubjectTrustedConfig,
}

func (t ScheduleSubjectType) valid() bool {
	switch t {
	case ScheduleSubjectAttentionItem, ScheduleSubjectInstallationIntent,
		ScheduleSubjectTrustedConfig:
		return true
	default:
		return false
	}
}

// ScheduleSubject binds a schedule to its durable subject and carries the
// expected subject version the consuming handler rechecks (§5.16). Fields
// are type-scoped (Validate enforces presence and absence), so a subject
// can never carry vocabulary its type does not declare.
type ScheduleSubject struct {
	Type ScheduleSubjectType `json:"type"`
	// ItemID and ItemVersion bind an attention_item subject: the item and
	// the version expected at the last validation. Nil renders explicit null
	// for other subject types (pointer-for-optional).
	ItemID      *ItemID `json:"item_id"`
	ItemVersion *int    `json:"item_version"`
	// RegistrationID, ActiveEpoch, and DurableIntentRevision bind an
	// installation_intent subject to the exact pending envelope (§5.9
	// activation state): a superseded epoch or revision is a stale binding.
	RegistrationID        *int64 `json:"registration_id"`
	ActiveEpoch           *int64 `json:"active_epoch"`
	DurableIntentRevision *int64 `json:"durable_intent_revision"`
}

// Validate reports whether the subject is structurally sound, including the
// type-scoped field contract. Type dispatch omits default so a new subject
// type must declare its fields; the trailing return rejects the invalid
// zero value.
func (s ScheduleSubject) Validate() error {
	requireDetail := func(name string, present, want bool) error {
		if present == want {
			return nil
		}
		if want {
			return fmt.Errorf("schedule subject %s requires %s: %w", s.Type, name, ErrScheduleDetailMismatch)
		}
		return fmt.Errorf("schedule subject %s carries %s: %w", s.Type, name, ErrScheduleDetailMismatch)
	}
	check := func(wantItem, wantIntent bool) error {
		if err := requireDetail("item_id", s.ItemID != nil, wantItem); err != nil {
			return err
		}
		if err := requireDetail("item_version", s.ItemVersion != nil, wantItem); err != nil {
			return err
		}
		if err := requireDetail("registration_id", s.RegistrationID != nil, wantIntent); err != nil {
			return err
		}
		if err := requireDetail("active_epoch", s.ActiveEpoch != nil, wantIntent); err != nil {
			return err
		}
		return requireDetail("durable_intent_revision", s.DurableIntentRevision != nil, wantIntent)
	}
	switch s.Type {
	case ScheduleSubjectAttentionItem:
		if err := check(true, false); err != nil {
			return err
		}
		if *s.ItemID == "" {
			return fmt.Errorf("schedule subject item_id: %w", ErrEmptyID)
		}
		if *s.ItemVersion < 1 {
			return fmt.Errorf("schedule subject item_version %d: %w", *s.ItemVersion, ErrNonPositive)
		}
		return nil
	case ScheduleSubjectInstallationIntent:
		if err := check(false, true); err != nil {
			return err
		}
		for name, v := range map[string]int64{
			"registration_id":         *s.RegistrationID,
			"active_epoch":            *s.ActiveEpoch,
			"durable_intent_revision": *s.DurableIntentRevision,
		} {
			if v <= 0 {
				return fmt.Errorf("schedule subject %s %d: %w", name, v, ErrNonPositive)
			}
		}
		return nil
	case ScheduleSubjectTrustedConfig:
		return check(false, false)
	}
	return fmt.Errorf("schedule subject type %q: %w", s.Type, ErrInvalidScheduleSubjectType)
}

// ScheduleBaseWatch is the kind-scoped detail of a base_advance_watch: the
// repository, target base ref, and the base tip the publication was
// admitted against, all daemon-recorded from the trusted authorization,
// never caller text.
type ScheduleBaseWatch struct {
	Repo            string `json:"repo"`
	BaseRef         string `json:"base_ref"`
	AdmittedBaseSHA string `json:"admitted_base_sha"`
}

// Validate reports whether the detail is structurally sound.
func (b ScheduleBaseWatch) Validate() error {
	for name, v := range map[string]string{
		"repo": b.Repo, "base_ref": b.BaseRef, "admitted_base_sha": b.AdmittedBaseSHA,
	} {
		if v == "" {
			return fmt.Errorf("schedule base watch %s: %w", name, ErrEmptyField)
		}
	}
	return nil
}

// Schedule is one durable deferred check (§5.16): the synced aggregate
// carrying the binding, cadence or deadline, expiry, and terminal
// resolution. The rolling next-nominal-fire instant for recurring kinds and
// the occurrence ledger are the store's internal bookkeeping, deliberately
// not part of this shape: the client-visible fact is what is scheduled and
// how it ended, not the tick clock.
type Schedule struct {
	ID        ScheduleID      `json:"id"`
	ProjectID ProjectID       `json:"project_id"`
	Kind      ScheduleKind    `json:"kind"`
	Subject   ScheduleSubject `json:"subject"`
	// RunID and PolicyDigest are the independent fire-time authority binding
	// for attention-item workload kinds. They are fixed for the schedule's
	// lifetime and absent for installation intent and trusted-config jobs.
	RunID        *RunID  `json:"run_id"`
	PolicyDigest *Digest `json:"policy_digest"`
	// Generation increments on every re-arm; occurrence identity and event
	// staleness checks bind to it.
	Generation int64 `json:"generation"`
	// CreatedAt is the UTC instant the current generation was armed.
	CreatedAt time.Time `json:"created_at"`
	// FireAt is the one-shot deadline's nominal fire instant, immutable
	// within a generation; present exactly for one-shot kinds
	// (pointer-for-optional).
	FireAt *time.Time `json:"fire_at"`
	// IntervalSeconds is the recurring cadence; present exactly for
	// recurring kinds, positive.
	IntervalSeconds *int64 `json:"interval_seconds"`
	// ExpiresAt bounds the schedule's validity: required for
	// installation_poll (the envelope's expiry), optional for recurring
	// workload watches, and forbidden for one-shot deadlines and permanent
	// trusted-config jobs.
	ExpiresAt *time.Time `json:"expires_at"`
	// BaseWatch is present exactly for base_advance_watch.
	BaseWatch *ScheduleBaseWatch `json:"base_watch"`
	Status    ScheduleStatus     `json:"status"`
	// Resolution is present exactly when Status is terminal.
	Resolution *ScheduleResolution `json:"resolution"`
}

// ScheduleInput carries the caller-supplied fields of a new Schedule. It
// deliberately omits Generation, Status, and Resolution: a schedule is
// always created armed at generation one, so no input path can mint a
// pre-terminated or generation-skipped row; re-arming and concluding go
// through ReArmed and Concluded on an existing value.
type ScheduleInput struct {
	ID              ScheduleID
	ProjectID       ProjectID
	Kind            ScheduleKind
	Subject         ScheduleSubject
	RunID           *RunID
	PolicyDigest    *Digest
	CreatedAt       time.Time
	FireAt          *time.Time
	IntervalSeconds *int64
	ExpiresAt       *time.Time
	BaseWatch       *ScheduleBaseWatch
}

// NewSchedule builds a validated, armed, generation-one Schedule,
// normalizing every instant to UTC and detaching the returned value from
// caller-owned pointers.
func NewSchedule(in ScheduleInput) (Schedule, error) {
	s := Schedule{
		ID:              in.ID,
		ProjectID:       in.ProjectID,
		Kind:            in.Kind,
		Subject:         cloneSubject(in.Subject),
		RunID:           clonePtr(in.RunID),
		PolicyDigest:    clonePtr(in.PolicyDigest),
		Generation:      1,
		CreatedAt:       in.CreatedAt.UTC(),
		FireAt:          cloneUTC(in.FireAt),
		IntervalSeconds: clonePtr(in.IntervalSeconds),
		ExpiresAt:       cloneUTC(in.ExpiresAt),
		BaseWatch:       clonePtr(in.BaseWatch),
		Status:          ScheduleArmed,
	}
	if err := s.Validate(); err != nil {
		return Schedule{}, err
	}
	return s, nil
}

// Concluded returns a terminal copy of the schedule: the recorded end of
// its current generation. One-shot deadlines always terminate through here
// (fired-and-handled or explicitly resolved), and a recurring schedule's
// watch ends through here with its recorded proof.
func (s Schedule) Concluded(
	status ScheduleStatus, reason ScheduleResolutionReason, recordedAt time.Time,
) (Schedule, error) {
	if s.Status.Terminal() {
		return Schedule{}, fmt.Errorf("schedule %s is already terminal: %w", s.ID, ErrImmutableTransition)
	}
	out := s
	out.Status = status
	out.Resolution = &ScheduleResolution{Reason: reason, RecordedAt: recordedAt.UTC()}
	if err := out.Validate(); err != nil {
		return Schedule{}, err
	}
	return out, nil
}

// ReArmed returns the next generation of the schedule: armed, resolution
// cleared, with a corrected subject binding and (for one-shot kinds) a new
// nominal fire instant. This is the §5.16 stale-event path: the handler
// recomputes and re-arms under a new generation instead of silently
// discarding.
func (s Schedule) ReArmed(
	subject ScheduleSubject, fireAt *time.Time, armedAt time.Time,
) (Schedule, error) {
	out := s
	out.Generation = s.Generation + 1
	out.Subject = cloneSubject(subject)
	out.FireAt = cloneUTC(fireAt)
	out.CreatedAt = armedAt.UTC()
	out.Status = ScheduleArmed
	out.Resolution = nil
	if err := out.Validate(); err != nil {
		return Schedule{}, err
	}
	return out, nil
}

// Validate reports whether the schedule is structurally sound, including
// the kind-scoped detail contract. Kind dispatch omits default so a new
// kind must declare its shape; the trailing return rejects the invalid zero
// kind.
func (s Schedule) Validate() error {
	if s.ID == "" {
		return fmt.Errorf("schedule id: %w", ErrEmptyID)
	}
	if s.ProjectID == "" {
		return fmt.Errorf("schedule %s project_id: %w", s.ID, ErrEmptyID)
	}
	if !s.Kind.valid() {
		return fmt.Errorf("schedule %s kind %q: %w", s.ID, s.Kind, ErrInvalidScheduleKind)
	}
	if err := s.Subject.Validate(); err != nil {
		return fmt.Errorf("schedule %s: %w", s.ID, err)
	}
	if s.Subject.Type != s.Kind.subjectType() {
		return fmt.Errorf("schedule %s kind %s binds subject %s, want %s: %w",
			s.ID, s.Kind, s.Subject.Type, s.Kind.subjectType(), ErrScheduleDetailMismatch)
	}
	requiresPolicy := false
	switch s.Kind {
	case SchedulePRChecksDeadline, ScheduleReviewWaitThreshold, ScheduleBaseAdvanceWatch:
		requiresPolicy = true
	case ScheduleInstallationPoll, ScheduleDoctor, ScheduleJanitor:
		requiresPolicy = false
	}
	if err := requireOptionalBinding("run_id", s.RunID != nil, requiresPolicy); err != nil {
		return fmt.Errorf("schedule %s kind %s: %w", s.ID, s.Kind, err)
	}
	if err := requireOptionalBinding("policy_digest", s.PolicyDigest != nil, requiresPolicy); err != nil {
		return fmt.Errorf("schedule %s kind %s: %w", s.ID, s.Kind, err)
	}
	if s.RunID != nil && *s.RunID == "" {
		return fmt.Errorf("schedule %s run_id: %w", s.ID, ErrEmptyID)
	}
	if s.PolicyDigest != nil && *s.PolicyDigest == "" {
		return fmt.Errorf("schedule %s policy_digest: %w", s.ID, ErrEmptyField)
	}
	if s.Generation < 1 {
		return fmt.Errorf("schedule %s generation %d: %w", s.ID, s.Generation, ErrNonPositive)
	}
	if !s.Status.valid() {
		return fmt.Errorf("schedule %s status %q: %w", s.ID, s.Status, ErrInvalidScheduleStatus)
	}
	for name, ts := range map[string]*time.Time{
		"created_at": &s.CreatedAt, "fire_at": s.FireAt, "expires_at": s.ExpiresAt,
	} {
		if ts == nil {
			continue
		}
		if ts.IsZero() {
			return fmt.Errorf("schedule %s %s: %w", s.ID, name, ErrMissingTimestamp)
		}
		if ts.Location() != time.UTC {
			return fmt.Errorf("schedule %s %s: %w", s.ID, name, ErrTimestampNotUTC)
		}
	}
	requireDetail := func(name string, present, want bool) error {
		if present == want {
			return nil
		}
		if want {
			return fmt.Errorf("schedule %s kind %s requires %s: %w", s.ID, s.Kind, name, ErrScheduleDetailMismatch)
		}
		return fmt.Errorf("schedule %s kind %s carries %s: %w", s.ID, s.Kind, name, ErrScheduleDetailMismatch)
	}
	if err := requireDetail("fire_at", s.FireAt != nil, s.Kind.OneShot()); err != nil {
		return err
	}
	if err := requireDetail("interval_seconds", s.IntervalSeconds != nil, !s.Kind.OneShot()); err != nil {
		return err
	}
	if s.IntervalSeconds != nil && *s.IntervalSeconds <= 0 {
		return fmt.Errorf("schedule %s interval_seconds %d: %w", s.ID, *s.IntervalSeconds, ErrNonPositive)
	}
	if err := requireDetail("base_watch", s.BaseWatch != nil, s.Kind == ScheduleBaseAdvanceWatch); err != nil {
		return err
	}
	if s.BaseWatch != nil {
		if err := s.BaseWatch.Validate(); err != nil {
			return fmt.Errorf("schedule %s: %w", s.ID, err)
		}
	}
	switch s.Kind {
	case ScheduleInstallationPoll:
		if s.ExpiresAt == nil {
			return fmt.Errorf("schedule %s kind %s requires expires_at: %w", s.ID, s.Kind, ErrScheduleDetailMismatch)
		}
	case SchedulePRChecksDeadline, ScheduleReviewWaitThreshold:
		if s.ExpiresAt != nil {
			return fmt.Errorf("schedule %s one-shot kind %s carries expires_at: %w",
				s.ID, s.Kind, ErrScheduleDetailMismatch)
		}
	case ScheduleDoctor, ScheduleJanitor:
		if s.ExpiresAt != nil {
			return fmt.Errorf("schedule %s permanent trusted-config job carries expires_at: %w",
				s.ID, ErrScheduleDetailMismatch)
		}
	case ScheduleBaseAdvanceWatch:
		// A recurring workload watch may carry a validity bound; absent one,
		// it lives until its subject concludes.
	}
	if s.Status.Terminal() != (s.Resolution != nil) {
		return fmt.Errorf("schedule %s status %s resolution presence: %w",
			s.ID, s.Status, ErrScheduleDetailMismatch)
	}
	if s.Resolution != nil {
		if !s.Resolution.Reason.valid() {
			return fmt.Errorf("schedule %s resolution reason %q: %w",
				s.ID, s.Resolution.Reason, ErrInvalidScheduleResolution)
		}
		if !s.Resolution.Reason.allowedFor(s.Status) {
			return fmt.Errorf("schedule %s status %s cannot carry reason %s: %w",
				s.ID, s.Status, s.Resolution.Reason, ErrInvalidScheduleResolution)
		}
		if s.Resolution.RecordedAt.IsZero() {
			return fmt.Errorf("schedule %s resolution recorded_at: %w", s.ID, ErrMissingTimestamp)
		}
		if s.Resolution.RecordedAt.Location() != time.UTC {
			return fmt.Errorf("schedule %s resolution recorded_at: %w", s.ID, ErrTimestampNotUTC)
		}
	}
	if s.Status == ScheduleFired && !s.Kind.OneShot() {
		return fmt.Errorf("schedule %s recurring kind %s cannot terminate fired: %w",
			s.ID, s.Kind, ErrInvalidScheduleStatus)
	}
	if s.Status == ScheduleExpired && s.Kind.OneShot() {
		return fmt.Errorf("schedule %s one-shot kind %s cannot terminate expired: %w",
			s.ID, s.Kind, ErrInvalidScheduleStatus)
	}
	if s.Status.Terminal() && s.Kind.TrustedConfigJob() {
		return fmt.Errorf("schedule %s permanent trusted-config job cannot terminate: %w",
			s.ID, ErrInvalidScheduleStatus)
	}
	return nil
}

// ValidateScheduleTransition reports whether an update from old to new is a
// legal aggregate transition: identity and binding class never change; a
// same-generation update only concludes an armed generation (or replays the
// identical value); a re-arm advances the generation by exactly one and is
// armed. Everything else is stale or immutable-violating.
func ValidateScheduleTransition(old, new Schedule) error {
	if old.ID != new.ID || old.Kind != new.Kind || old.ProjectID != new.ProjectID ||
		old.Subject.Type != new.Subject.Type || !ptrEqual(old.RunID, new.RunID) ||
		!ptrEqual(old.PolicyDigest, new.PolicyDigest) {
		return fmt.Errorf("schedule %s identity would change: %w", old.ID, ErrImmutableTransition)
	}
	switch new.Generation {
	case old.Generation:
		if schedulesEqual(old, new) {
			return nil // idempotent replay
		}
		if old.Status.Terminal() {
			return fmt.Errorf("schedule %s generation %d is terminal: %w",
				old.ID, old.Generation, ErrImmutableTransition)
		}
		if !new.Status.Terminal() {
			return fmt.Errorf("schedule %s same-generation update must conclude it: %w",
				old.ID, ErrStaleTransition)
		}
		armed := new
		armed.Status = old.Status
		armed.Resolution = old.Resolution
		if !schedulesEqual(old, armed) {
			return fmt.Errorf("schedule %s conclusion would rewrite armed fields: %w",
				old.ID, ErrImmutableTransition)
		}
		return nil
	case old.Generation + 1:
		if new.Status != ScheduleArmed {
			return fmt.Errorf("schedule %s re-arm must be armed: %w", old.ID, ErrStaleTransition)
		}
		return nil
	default:
		return fmt.Errorf("schedule %s generation %d -> %d: %w",
			old.ID, old.Generation, new.Generation, ErrStaleTransition)
	}
}

func schedulesEqual(a, b Schedule) bool {
	return a.ID == b.ID && a.ProjectID == b.ProjectID && a.Kind == b.Kind &&
		subjectsEqual(a.Subject, b.Subject) && ptrEqual(a.RunID, b.RunID) &&
		ptrEqual(a.PolicyDigest, b.PolicyDigest) && a.Generation == b.Generation &&
		a.CreatedAt.Equal(b.CreatedAt) &&
		timePtrEqual(a.FireAt, b.FireAt) &&
		int64PtrEqual(a.IntervalSeconds, b.IntervalSeconds) &&
		timePtrEqual(a.ExpiresAt, b.ExpiresAt) &&
		baseWatchEqual(a.BaseWatch, b.BaseWatch) &&
		a.Status == b.Status && resolutionEqual(a.Resolution, b.Resolution)
}

func subjectsEqual(a, b ScheduleSubject) bool {
	return a.Type == b.Type &&
		ptrEqual(a.ItemID, b.ItemID) && ptrEqual(a.ItemVersion, b.ItemVersion) &&
		ptrEqual(a.RegistrationID, b.RegistrationID) &&
		ptrEqual(a.ActiveEpoch, b.ActiveEpoch) &&
		ptrEqual(a.DurableIntentRevision, b.DurableIntentRevision)
}

func ptrEqual[T comparable](a, b *T) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	return a == nil || *a == *b
}

func int64PtrEqual(a, b *int64) bool { return ptrEqual(a, b) }

func timePtrEqual(a, b *time.Time) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	return a == nil || a.Equal(*b)
}

func baseWatchEqual(a, b *ScheduleBaseWatch) bool { return ptrEqual(a, b) }

func resolutionEqual(a, b *ScheduleResolution) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	return a == nil || (a.Reason == b.Reason && a.RecordedAt.Equal(b.RecordedAt))
}

func cloneSubject(s ScheduleSubject) ScheduleSubject {
	s.ItemID = clonePtr(s.ItemID)
	s.ItemVersion = clonePtr(s.ItemVersion)
	s.RegistrationID = clonePtr(s.RegistrationID)
	s.ActiveEpoch = clonePtr(s.ActiveEpoch)
	s.DurableIntentRevision = clonePtr(s.DurableIntentRevision)
	return s
}

func requireOptionalBinding(name string, present, want bool) error {
	if present == want {
		return nil
	}
	if want {
		return fmt.Errorf("requires %s: %w", name, ErrScheduleDetailMismatch)
	}
	return fmt.Errorf("carries %s: %w", name, ErrScheduleDetailMismatch)
}

func cloneUTC(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// ScheduleFireGap records that missed fires coalesced into the occurrence
// carrying it: how many nominal occurrences were skipped and the earliest
// skipped instant (§5.16 "missed fires coalesce to the latest nominal
// occurrence with a recorded gap").
type ScheduleFireGap struct {
	MissedOccurrences int64 `json:"missed_occurrences"`
	// EarliestMissedAt is the earliest skipped nominal fire instant, UTC.
	EarliestMissedAt time.Time `json:"earliest_missed_at"`
}

// Validate reports whether the gap is structurally sound.
func (g ScheduleFireGap) Validate() error {
	if g.MissedOccurrences < 1 {
		return fmt.Errorf("schedule fire gap missed_occurrences %d: %w",
			g.MissedOccurrences, ErrNonPositive)
	}
	if g.EarliestMissedAt.IsZero() {
		return fmt.Errorf("schedule fire gap earliest_missed_at: %w", ErrMissingTimestamp)
	}
	if g.EarliestMissedAt.Location() != time.UTC {
		return fmt.Errorf("schedule fire gap earliest_missed_at: %w", ErrTimestampNotUTC)
	}
	return nil
}

// ScheduleOccurrenceStatus is an occurrence's delivery state. The zero
// value "" is invalid by design.
type ScheduleOccurrenceStatus string

const (
	OccurrencePending  ScheduleOccurrenceStatus = "pending"
	OccurrenceConsumed ScheduleOccurrenceStatus = "consumed"
)

// AllScheduleOccurrenceStatuses is the single registration point for
// occurrence statuses.
var AllScheduleOccurrenceStatuses = []ScheduleOccurrenceStatus{
	OccurrencePending, OccurrenceConsumed,
}

func (s ScheduleOccurrenceStatus) valid() bool {
	switch s {
	case OccurrencePending, OccurrenceConsumed:
		return true
	default:
		return false
	}
}

// ScheduleOccurrenceOutcome is the closed vocabulary of consumption
// outcomes. The zero value "" is invalid by design.
type ScheduleOccurrenceOutcome string

const (
	// OutcomeHandled: the handler ran and its outcome committed with this
	// consumption.
	OutcomeHandled ScheduleOccurrenceOutcome = "handled"
	// OutcomeConditionNoLongerApplies: the handler recomputed and recorded
	// proof that the awaited condition no longer applies.
	OutcomeConditionNoLongerApplies ScheduleOccurrenceOutcome = "condition_no_longer_applies"
	// OutcomeReArmed: the event was stale; the handler re-armed the schedule
	// under a new generation with a corrected binding.
	OutcomeReArmed ScheduleOccurrenceOutcome = "re_armed"
	// OutcomeCoalesced: a later nominal occurrence superseded this pending
	// one; the successor carries the recorded gap.
	OutcomeCoalesced ScheduleOccurrenceOutcome = "coalesced"
	// OutcomeStaleGeneration: the schedule re-armed under a new generation
	// while this occurrence was pending; it can no longer be consumed.
	OutcomeStaleGeneration ScheduleOccurrenceOutcome = "stale_generation"
	// OutcomeObserveFailed: the handler's external observation failed
	// transiently; a recurring schedule retries at its next nominal fire.
	OutcomeObserveFailed ScheduleOccurrenceOutcome = "observe_failed"
)

// AllScheduleOccurrenceOutcomes is the single registration point for
// occurrence outcomes.
var AllScheduleOccurrenceOutcomes = []ScheduleOccurrenceOutcome{
	OutcomeHandled,
	OutcomeConditionNoLongerApplies,
	OutcomeReArmed,
	OutcomeCoalesced,
	OutcomeStaleGeneration,
	OutcomeObserveFailed,
}

func (o ScheduleOccurrenceOutcome) valid() bool {
	switch o {
	case OutcomeHandled, OutcomeConditionNoLongerApplies, OutcomeReArmed,
		OutcomeCoalesced, OutcomeStaleGeneration, OutcomeObserveFailed:
		return true
	default:
		return false
	}
}

// ScheduleOccurrence is one fired (or firing) instance of a schedule.
// Identity is (schedule_id, generation, nominal_fire_at); a pending
// occurrence is durably redeliverable until its consumption and outcome
// commit in one transaction (§5.16). Occurrences are daemon-internal
// bookkeeping (the 0014 rule), never synchronized client state.
type ScheduleOccurrence struct {
	ScheduleID    ScheduleID `json:"schedule_id"`
	Generation    int64      `json:"generation"`
	NominalFireAt time.Time  `json:"nominal_fire_at"`
	// Gap records coalesced missed fires; nil when none were missed.
	Gap    *ScheduleFireGap         `json:"gap"`
	Status ScheduleOccurrenceStatus `json:"status"`
	// CreatedAt is the UTC instant the occurrence committed as pending.
	CreatedAt time.Time `json:"created_at"`
	// ConsumedAt and Outcome are present exactly when consumed.
	ConsumedAt *time.Time                 `json:"consumed_at"`
	Outcome    *ScheduleOccurrenceOutcome `json:"outcome"`
}

// Validate reports whether the occurrence is structurally sound.
func (o ScheduleOccurrence) Validate() error {
	if o.ScheduleID == "" {
		return fmt.Errorf("schedule occurrence schedule_id: %w", ErrEmptyID)
	}
	if o.Generation < 1 {
		return fmt.Errorf("schedule occurrence %s generation %d: %w",
			o.ScheduleID, o.Generation, ErrNonPositive)
	}
	if !o.Status.valid() {
		return fmt.Errorf("schedule occurrence %s status %q: %w",
			o.ScheduleID, o.Status, ErrInvalidScheduleOccurrenceStatus)
	}
	for name, ts := range map[string]*time.Time{
		"nominal_fire_at": &o.NominalFireAt, "created_at": &o.CreatedAt, "consumed_at": o.ConsumedAt,
	} {
		if ts == nil {
			continue
		}
		if ts.IsZero() {
			return fmt.Errorf("schedule occurrence %s %s: %w", o.ScheduleID, name, ErrMissingTimestamp)
		}
		if ts.Location() != time.UTC {
			return fmt.Errorf("schedule occurrence %s %s: %w", o.ScheduleID, name, ErrTimestampNotUTC)
		}
	}
	if o.Gap != nil {
		if err := o.Gap.Validate(); err != nil {
			return fmt.Errorf("schedule occurrence %s: %w", o.ScheduleID, err)
		}
		if !o.Gap.EarliestMissedAt.Before(o.NominalFireAt) {
			return fmt.Errorf("schedule occurrence %s gap reads forwards: %w",
				o.ScheduleID, ErrTimestampOutOfOrder)
		}
	}
	consumed := o.Status == OccurrenceConsumed
	if (o.ConsumedAt != nil) != consumed || (o.Outcome != nil) != consumed {
		return fmt.Errorf("schedule occurrence %s status %s consumption fields: %w",
			o.ScheduleID, o.Status, ErrScheduleDetailMismatch)
	}
	if o.Outcome != nil && !o.Outcome.valid() {
		return fmt.Errorf("schedule occurrence %s outcome %q: %w",
			o.ScheduleID, *o.Outcome, ErrInvalidScheduleOccurrenceOutcome)
	}
	return nil
}

// ScheduleEvent is the trusted fired-occurrence event handed to a consuming
// handler. It is constructed only by NewScheduleEvent after fire-time
// validation (§5.16) and carries the expected schedule generation and
// subject binding for the handler's own recheck — identity and
// expectations only, never authority: firing never extends or preserves
// authority, so no credential, token, or grant is representable here.
type ScheduleEvent struct {
	ScheduleID    ScheduleID      `json:"schedule_id"`
	ProjectID     ProjectID       `json:"project_id"`
	Kind          ScheduleKind    `json:"kind"`
	Generation    int64           `json:"generation"`
	Subject       ScheduleSubject `json:"subject"`
	NominalFireAt time.Time       `json:"nominal_fire_at"`
	FiredAt       time.Time       `json:"fired_at"`
	// Gap carries the coalesced-miss record from the occurrence, nil when no
	// fires were missed.
	Gap *ScheduleFireGap `json:"gap"`
}

// NewScheduleEvent builds a validated event from an armed schedule and its
// pending occurrence. It is the single trusted constructor: the scheduler
// calls it after fire-time validation, and a handler receives no event that
// did not pass here.
func NewScheduleEvent(s Schedule, o ScheduleOccurrence, firedAt time.Time) (ScheduleEvent, error) {
	if err := s.Validate(); err != nil {
		return ScheduleEvent{}, err
	}
	if err := o.Validate(); err != nil {
		return ScheduleEvent{}, err
	}
	if s.Status != ScheduleArmed {
		return ScheduleEvent{}, fmt.Errorf("schedule event for %s status %s: %w",
			s.ID, s.Status, ErrInvalidScheduleStatus)
	}
	if o.ScheduleID != s.ID || o.Generation != s.Generation {
		return ScheduleEvent{}, fmt.Errorf("schedule event occurrence %s/%d against %s/%d: %w",
			o.ScheduleID, o.Generation, s.ID, s.Generation, ErrParentKeyMismatch)
	}
	if o.Status != OccurrencePending {
		return ScheduleEvent{}, fmt.Errorf("schedule event for consumed occurrence: %w",
			ErrInvalidScheduleOccurrenceStatus)
	}
	ev := ScheduleEvent{
		ScheduleID:    s.ID,
		ProjectID:     s.ProjectID,
		Kind:          s.Kind,
		Generation:    s.Generation,
		Subject:       cloneSubject(s.Subject),
		NominalFireAt: o.NominalFireAt,
		FiredAt:       firedAt.UTC(),
		Gap:           clonePtr(o.Gap),
	}
	if err := ev.Validate(); err != nil {
		return ScheduleEvent{}, err
	}
	return ev, nil
}

// Validate reports whether the event is structurally sound.
func (e ScheduleEvent) Validate() error {
	if e.ScheduleID == "" {
		return fmt.Errorf("schedule event schedule_id: %w", ErrEmptyID)
	}
	if e.ProjectID == "" {
		return fmt.Errorf("schedule event %s project_id: %w", e.ScheduleID, ErrEmptyID)
	}
	if !e.Kind.valid() {
		return fmt.Errorf("schedule event %s kind %q: %w", e.ScheduleID, e.Kind, ErrInvalidScheduleKind)
	}
	if e.Generation < 1 {
		return fmt.Errorf("schedule event %s generation %d: %w",
			e.ScheduleID, e.Generation, ErrNonPositive)
	}
	if err := e.Subject.Validate(); err != nil {
		return fmt.Errorf("schedule event %s: %w", e.ScheduleID, err)
	}
	if e.Subject.Type != e.Kind.subjectType() {
		return fmt.Errorf("schedule event %s kind %s binds subject %s, want %s: %w",
			e.ScheduleID, e.Kind, e.Subject.Type, e.Kind.subjectType(), ErrScheduleDetailMismatch)
	}
	for name, ts := range map[string]time.Time{
		"nominal_fire_at": e.NominalFireAt, "fired_at": e.FiredAt,
	} {
		if ts.IsZero() {
			return fmt.Errorf("schedule event %s %s: %w", e.ScheduleID, name, ErrMissingTimestamp)
		}
		if ts.Location() != time.UTC {
			return fmt.Errorf("schedule event %s %s: %w", e.ScheduleID, name, ErrTimestampNotUTC)
		}
	}
	if e.Gap != nil {
		if err := e.Gap.Validate(); err != nil {
			return fmt.Errorf("schedule event %s: %w", e.ScheduleID, err)
		}
	}
	return nil
}
