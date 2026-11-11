package observe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/observe/observedb"
)

// ErrUntilTimeout reports that a wait's deadline passed before its condition
// held. It is a could-not-observe result (exit 1), not a usage mistake: the
// caller asked a well-formed question the run did not answer in time.
var ErrUntilTimeout = errors.New("condition not met before timeout")

// untilKind names one checkpoint a wait can stop at (-until). Each reads only
// the supervision snapshot follow -snapshot already prints, so a wait proves
// nothing the snapshot could not.
type untilKind string

const (
	// untilCard holds when the run has an open item of the named type that
	// requests a decision: the test deriveSupervisionState uses for
	// attention_required.
	untilCard untilKind = "card"
	// untilStageStarted holds when an invocation of the named stage has an
	// invocation_started milestone.
	untilStageStarted untilKind = "stage-started"
	// untilStageFinished holds when an invocation of the named stage has a
	// terminal_recorded milestone.
	untilStageFinished untilKind = "stage-finished"
	// untilParkEnded holds when the run has an invocation_admitted milestone.
	// A parked attempt admits nothing until its quota window resets, and the
	// hold row alone is no proof the park ended, because another hold can
	// replace it.
	untilParkEnded untilKind = "park-ended"
)

var allUntilKinds = []untilKind{untilCard, untilStageStarted, untilStageFinished, untilParkEnded}

func (k untilKind) valid() bool {
	switch k {
	case untilCard, untilStageStarted, untilStageFinished, untilParkEnded:
		return true
	default:
		return false
	}
}

// startableStages and finishableStages are the stages a stage-started or
// stage-finished wait can name: the stages whose invocations record that
// milestone on the run. A review or verification invocation records no
// execution admission or invocation milestone, and a specification run's
// terminal records no terminal_recorded milestone (its spec_approval card is
// that checkpoint), so a wait on any of them could only time out and is
// refused instead.
var (
	startableStages  = []domain.StageName{domain.StageNameSpecification, domain.StageNameImplementation}
	finishableStages = []domain.StageName{domain.StageNameImplementation}
)

// Until is one parsed -until condition.
type Until struct {
	kind      untilKind
	attention domain.AttentionType
	stage     domain.StageName
}

// ParseUntil parses an -until value: card=<attention type>,
// stage-started=<stage>, stage-finished=<stage>, or park-ended. Every refusal
// wraps ErrUsage.
func ParseUntil(text string) (Until, error) {
	name, value, hasValue := strings.Cut(text, "=")
	kind := untilKind(name)
	if !kind.valid() {
		return Until{}, fmt.Errorf("%w: -until %q: condition must be one of %v", ErrUsage, text, allUntilKinds)
	}
	switch kind {
	case untilCard:
		attention := domain.AttentionType(value)
		switch {
		case !slices.Contains(domain.AllAttentionTypes, attention):
			return Until{}, fmt.Errorf("%w: -until %q: unknown attention type %q", ErrUsage, text, value)
		case attention == domain.AttentionReadyForFinalReview:
			// The snapshot leaves ready items out (they accompany a final
			// outcome, not a hold), so this wait could only time out.
			return Until{}, fmt.Errorf("%w: -until %q: the snapshot never lists %s items; wait for the outcome instead",
				ErrUsage, text, attention)
		}
		return Until{kind: kind, attention: attention}, nil
	case untilStageStarted, untilStageFinished:
		stage, stages := domain.StageName(value), startableStages
		if kind == untilStageFinished {
			stages = finishableStages
		}
		if !slices.Contains(stages, stage) {
			return Until{}, fmt.Errorf("%w: -until %q: stage must be one of %v; no other stage records this milestone on the run",
				ErrUsage, text, stages)
		}
		return Until{kind: kind, stage: stage}, nil
	case untilParkEnded:
		if hasValue {
			return Until{}, fmt.Errorf("%w: -until %q: park-ended takes no value", ErrUsage, text)
		}
		return Until{kind: kind}, nil
	}
	return Until{}, fmt.Errorf("%w: -until %q", ErrUsage, text)
}

func (u Until) String() string {
	switch u.kind {
	case untilCard:
		return string(u.kind) + "=" + string(u.attention)
	case untilStageStarted, untilStageFinished:
		return string(u.kind) + "=" + string(u.stage)
	case untilParkEnded:
		return string(u.kind)
	}
	return ""
}

// Holds reports whether the condition holds in one snapshot.
func (u Until) Holds(snapshot observedb.Snapshot) bool {
	switch u.kind {
	case untilCard:
		return slices.ContainsFunc(snapshot.AttentionItems, func(item observedb.AttentionItem) bool {
			return item.Type == u.attention && len(item.RequestedDecision) > 0
		})
	case untilStageStarted:
		return stageMilestone(snapshot, u.stage, domain.MilestoneInvocationStarted)
	case untilStageFinished:
		return stageMilestone(snapshot, u.stage, domain.MilestoneTerminalRecorded)
	case untilParkEnded:
		return slices.ContainsFunc(snapshot.Observation.Milestones, func(m domain.RunMilestone) bool {
			return m.Kind == domain.MilestoneInvocationAdmitted
		})
	}
	return false
}

// stageMilestone reports whether an invocation admitted to stage carries a
// milestone of kind. The run stores a stage under its engine spelling
// ("implement"), so each admission's stage is read through the canonical
// resolver; a stage it cannot resolve belongs to no waitable stage.
func stageMilestone(snapshot observedb.Snapshot, stage domain.StageName, kind domain.RunMilestoneKind) bool {
	invocations := map[domain.InvocationID]bool{}
	for _, admission := range snapshot.Admissions {
		if canonical, err := domain.CanonicalStageRole(admission.Stage); err == nil && canonical == stage {
			invocations[admission.InvocationID] = true
		}
	}
	return slices.ContainsFunc(snapshot.Observation.Milestones, func(m domain.RunMilestone) bool {
		return m.Kind == kind && m.InvocationID != nil && invocations[*m.InvocationID]
	})
}

// SnapshotObserver is the read surface Wait consumes.
type SnapshotObserver interface {
	ObserveSnapshot(ctx context.Context, runID domain.RunID) (observedb.Snapshot, error)
}

// WaitConfig selects the run and condition and bounds the wait.
type WaitConfig struct {
	RunID domain.RunID
	Until Until
	// Timeout bounds the wait, a read in flight included; it must be
	// positive, so a wait always ends.
	Timeout time.Duration
	// Interval is the read cadence; zero takes DefaultInterval.
	Interval time.Duration
	// Window is the liveness freshness bound for the status block; zero
	// takes the contract default.
	Window time.Duration
}

// Wait reads the run's supervision snapshot every interval, printing its
// timeline the way Follow does, until the condition holds (nil), the timeout
// passes (ErrUntilTimeout), or the context ends (its error). Its last line
// names the condition and how the wait ended. Only a met condition succeeds:
// a script waiting on a checkpoint must never read an interrupt as one.
func Wait(ctx context.Context, src SnapshotObserver, out io.Writer, cfg WaitConfig) error {
	switch {
	case cfg.RunID == "":
		return fmt.Errorf("until: run id: %w", domain.ErrEmptyID)
	case cfg.Timeout <= 0:
		return fmt.Errorf("until: timeout must be positive, got %s", cfg.Timeout)
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	window := cfg.Window
	if window <= 0 {
		window = domain.DefaultObservationFreshnessWindow
	}
	// The deadline rides the reads' context, so a read that hangs (a stuck
	// control socket) ends at the timeout too.
	waitCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()

	f := newFollower(out, window)
	started := false
	ended := func(how string, err error) error {
		if lineErr := f.line("until  " + cfg.Until.String() + "  " + how); lineErr != nil {
			return lineErr
		}
		return err
	}
	// stopped reports how the wait ended once waitCtx is done: the caller's
	// interrupt, or else the deadline.
	stopped := func() error {
		if ctx.Err() != nil {
			return ended("interrupted", fmt.Errorf("until %q: %w", cfg.RunID, ctx.Err()))
		}
		return ended("timed out", fmt.Errorf("until %q after %s: %w", cfg.Until, cfg.Timeout, ErrUntilTimeout))
	}
	for {
		snapshot, err := src.ObserveSnapshot(waitCtx, cfg.RunID)
		if err != nil {
			if waitCtx.Err() != nil {
				return stopped()
			}
			return fmt.Errorf("until %q: %w", cfg.RunID, err)
		}
		if !started {
			if err := f.line("run " + identifier(string(cfg.RunID))); err != nil {
				return err
			}
			started = true
		}
		if _, err := f.emit(snapshot.Observation, time.Now().UTC()); err != nil {
			return err
		}
		if cfg.Until.Holds(snapshot) {
			return ended("met", nil)
		}
		select {
		case <-waitCtx.Done():
			return stopped()
		case <-time.After(interval):
		}
	}
}
