package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/scheduler"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// Durable-scheduler composition for the production (claude) daemon: the
// §5.16 permanent trusted-config jobs. The doctor and janitor keep their
// §10 obligations — including in attended_dev — with only their cadence
// migrated off plain tickers; each keeps its synchronous startup pass
// (main.go's initial doctor run, the janitor's coverage priming in
// composeClaudeDriver) as a direct call, and a handler failure stops the
// scheduler loop, which the daemon treats as fatal.

const (
	schedulerSystemProjectID = domain.ProjectID("project-system")
	doctorScheduleID         = domain.ScheduleID("schedule-doctor")
	janitorScheduleID        = domain.ScheduleID("schedule-janitor")
)

// baseTipObserver reads the current tip of a watch's target base ref. The
// production composition observes through the publish reconciler's
// conditional ref read; the fake lane's world holds no advancing base, so
// its observer reports the admitted base unchanged.
type baseTipObserver func(ctx context.Context, watch domain.ScheduleBaseWatch) (string, error)

// pullObserver and issueObserver read one bound resource's state. The
// production composition observes through the publish reconciler's
// conditional reads; the fake lane's world holds PRs that never merge and
// issues that never close.
type (
	pullObserver  func(ctx context.Context, repo string, number int) (publish.PullObservation, error)
	issueObserver func(ctx context.Context, repo string, number int) (publish.IssueObservation, error)
)

// nativeReviewObserver reads one PR's native (forge-hosted) review activity —
// its submitted reviews, inline review comments, and description reactions —
// as best-effort extra evidence (plan §5.16, §7). The production composition
// observes through the publish reconciler's conditional reads; the fake lane
// wires no native reviewer.
type nativeReviewObserver func(ctx context.Context, repo string, number int) (publish.PullReviewObservation, error)

// mergeCapture is the former §5.18 base-watch capture hook. Production and
// fake composition leave it unwired after #463 moved resource observation to
// active_resource.go; it remains here only as a focused regression harness for
// the scheduler/capture transaction seam while that older coverage is ported.
type mergeCapture struct {
	pull  pullObserver
	issue issueObserver
}

// observe derives the capture commit for the item's bound work unit: nil
// with no error when there is nothing to record (no capture observers
// wired, an undeclared run, a unit with no PR binding yet, or an
// already-complete one), an error when a bound resource could not be
// observed — the caller decides whether that blocks concluding the watch
// or waits for the next fire.
//
// watch and item are the pass's first-party anchors, re-read live every
// fire: the watch carries the admitted repository and base ref, the ready
// item the published head, both written by the engine from the admission
// and import records. A reconstructed binding that disagrees with them is
// refused before any observation, so the store's internal-consistency
// re-gates are backed here by state a row-writing corruption cannot
// silently satisfy without also rewriting the synced aggregates.
func (c mergeCapture) observe(
	ctx context.Context, st *store.Store,
	item domain.AttentionItem, watch domain.ScheduleBaseWatch, firedAt time.Time,
) (func(context.Context, *store.InternalTx) error, error) {
	if c.pull == nil || item.Subject.RunID == nil {
		return nil, nil
	}
	var (
		declaration domain.WorkUnitDeclaration
		binding     domain.WorkUnitPRBinding
		declared    bool
		bound       bool
		complete    bool
	)
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		d, err := tx.GetWorkUnitDeclarationByRun(ctx, *item.Subject.RunID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		declaration, declared = d, true
		b, err := tx.GetWorkUnitPRBinding(ctx, d.ID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := tx.GetWorkUnitCompletion(ctx, d.ID); err == nil {
			complete = true
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		binding, bound = b, true
		return nil
	}); err != nil {
		return nil, err
	}
	if !declared || complete {
		return nil, nil
	}
	if !bound {
		// Declared but not yet bound: the publication workflow records the
		// binding before it arms these watches, so this state is a
		// not-yet-converged pass or a foreign write order, never "nothing to
		// record". Fail the observation so a concluded item's final capture
		// retries instead of resolving the watch with the unit's merge
		// forever unobserved.
		return nil, fmt.Errorf("work unit %s is declared but has no pr binding yet", declaration.ID)
	}
	// The binding's coordinates must restate the first-party anchors before
	// anything is observed or evaluated through them.
	if binding.Repo != watch.Repo || binding.BaseRef != watch.BaseRef {
		return nil, fmt.Errorf("work unit %s pr binding names %s@%s, the armed watch %s@%s",
			declaration.ID, binding.Repo, binding.BaseRef, watch.Repo, watch.BaseRef)
	}
	if item.PRHeadSHA == "" || binding.HeadSHA != item.PRHeadSHA {
		return nil, fmt.Errorf("work unit %s pr binding head does not restate the ready item's published head",
			declaration.ID)
	}

	obs, err := c.pull(ctx, binding.Repo, binding.PRNumber)
	if err != nil {
		return nil, fmt.Errorf("observe pull %s#%d: %w", binding.Repo, binding.PRNumber, err)
	}
	// The fact carries the OBSERVED base-repository identity, never the
	// binding's: the request is addressed by name, and stamping the
	// binding's id onto whatever the name currently reaches would make the
	// evaluator's repository check compare a value against itself. A name
	// re-bound to a different repository records that repository's facts
	// under its own id, and the unit never completes on them.
	pullFact := domain.PullMergeFact{
		Repo: binding.Repo, RepositoryID: obs.BaseRepoID,
		PRNumber: binding.PRNumber, State: domain.PullRequestState(obs.State),
		Merged: obs.Merged, MergeCommitSHA: obs.MergeCommitSHA,
		BaseRef: obs.BaseRef, HeadSHA: obs.HeadSHA, ObservedAt: firedAt.UTC(),
	}
	// An observation the capture vocabulary cannot express (including a
	// response carrying no base-repository id) is a failed observation,
	// never a weaker fact.
	if err := pullFact.Validate(); err != nil {
		return nil, fmt.Errorf("observe pull %s#%d: %w", binding.Repo, binding.PRNumber, err)
	}
	var issueFact *domain.IssueStateFact
	if declaration.CompletionCriterion == domain.CompletionBoundIssueClosedByMergedPR &&
		declaration.BoundIssue != nil && pullFact.Merged &&
		pullFact.RepositoryID == binding.RepositoryID {
		// The issue read is addressed by the same repository name in the
		// same pass, so the pull observation's verified identity is the
		// issue fact's anchor; when the name does not reach the bound
		// repository, the issue is not read at all.
		if c.issue == nil {
			return nil, fmt.Errorf("observe issue %s#%d: no issue observer wired", binding.Repo, *declaration.BoundIssue)
		}
		iobs, err := c.issue(ctx, binding.Repo, *declaration.BoundIssue)
		if err != nil {
			return nil, fmt.Errorf("observe issue %s#%d: %w", binding.Repo, *declaration.BoundIssue, err)
		}
		// The name could be re-bound between the two requests (transfer
		// plus reuse), so the name→identity binding is re-verified after
		// the issue read with a second pull observation — conditional, so
		// an unchanged resource answers 304 — and the pass fails when the
		// identity moved rather than stamping the earlier id onto an issue
		// that may belong to a different repository.
		recheck, err := c.pull(ctx, binding.Repo, binding.PRNumber)
		if err != nil {
			return nil, fmt.Errorf("re-verify repository identity %s: %w", binding.Repo, err)
		}
		if recheck.BaseRepoID != pullFact.RepositoryID {
			return nil, fmt.Errorf("repository %s changed identity during the capture pass", binding.Repo)
		}
		fact := domain.IssueStateFact{
			Repo: binding.Repo, RepositoryID: pullFact.RepositoryID,
			IssueNumber: *declaration.BoundIssue, State: domain.IssueState(iobs.State),
			ClosedByCommitSHA: iobs.ClosedByCommitSHA, ObservedAt: firedAt.UTC(),
		}
		if err := fact.Validate(); err != nil {
			return nil, fmt.Errorf("observe issue %s#%d: %w", binding.Repo, *declaration.BoundIssue, err)
		}
		issueFact = &fact
	}
	completion, completed := domain.EvaluateWorkUnitCompletion(declaration, binding, pullFact, issueFact)
	// A commit is returned only when it would write: an attached Commit
	// forces the scheduler's client-visible Write path, and a routine fire
	// over an unchanged PR must not bump the §5.14 sync revision (the 0025
	// no-churn rule). The commit re-checks materiality inside the
	// transaction, so this read is an optimization, never the authority.
	material := completed
	if !material {
		if err := st.Read(ctx, func(tx *store.ReadTx) error {
			latest, err := tx.LatestPullMergeFact(ctx, pullFact.RepositoryID, pullFact.PRNumber)
			switch {
			case errors.Is(err, store.ErrNotFound):
				material = true
				return nil
			case err != nil:
				return err
			case pullFact.MaterialChangeFrom(latest):
				material = true
				return nil
			}
			if issueFact == nil {
				return nil
			}
			latestIssue, err := tx.LatestIssueStateFact(ctx, issueFact.RepositoryID, issueFact.IssueNumber)
			switch {
			case errors.Is(err, store.ErrNotFound):
				material = true
			case err != nil:
				return err
			case issueFact.MaterialChangeFrom(latestIssue):
				material = true
			}
			return nil
		}); err != nil {
			return nil, err
		}
	}
	if !material {
		return nil, nil
	}
	return func(ctx context.Context, tx *store.InternalTx) error {
		if _, err := tx.AppendPullMergeFact(ctx, pullFact); err != nil {
			return err
		}
		if issueFact != nil {
			if _, err := tx.AppendIssueStateFact(ctx, *issueFact); err != nil {
				return err
			}
		}
		if !completed {
			return nil
		}
		// A concurrent pass may have settled the unit between the
		// observation and this transaction; the write-once record stands.
		if _, err := tx.GetWorkUnitCompletion(ctx, completion.UnitID); err == nil {
			return nil
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		return tx.RecordWorkUnitCompletion(ctx, completion)
	}, nil
}

// withCapture composes the capture commit onto a consumption. The capture
// rows are daemon-internal, so they ride whichever transaction the
// consumption commits; a prior Commit's ErrStaleConsumption abandons the
// capture too, and the next fire re-derives it from current state.
func withCapture(
	c scheduler.Consumption, capture func(context.Context, *store.InternalTx) error,
) scheduler.Consumption {
	if capture == nil {
		return c
	}
	prior := c.Commit
	c.Commit = func(ctx context.Context, tx *store.WriteTx) error {
		if prior != nil {
			if err := prior(ctx, tx); err != nil {
				return err
			}
		}
		return capture(ctx, &tx.InternalTx)
	}
	return c
}

// deadlineRegistration wires a one-shot publication deadline
// (pr_checks_deadline, review_wait_threshold). The handler first rechecks
// the event's expected subject version (§5.16): a stale expectation — the
// base-advance watch's fact write, say, bumped the item — re-arms under a
// new generation with the corrected binding and the same nominal deadline,
// so the elapsed wall time is never postponed and the deadline fires again
// on the next pass against the current binding. A matching, still-open item
// terminates fired-and-handled with deadline_elapsed on the synced
// aggregate. Richer consumers (surfacing the elapsed deadline as attention,
// binding to observed check runs) arrive with the §7 review stage (#427).
func deadlineRegistration(st *store.Store) scheduler.Registration {
	return scheduler.Registration{Handle: func(
		ctx context.Context, ev domain.ScheduleEvent, sc domain.Schedule,
	) (scheduler.Consumption, error) {
		current, consumption, err := recheckItemSubject(ctx, st, ev, sc)
		if err != nil || consumption != nil {
			if consumption != nil {
				return *consumption, err
			}
			return scheduler.Consumption{}, err
		}
		if current.Status != domain.StatusOpen {
			// Concluded between fire-time validation and consumption: record
			// the proof rather than a deadline that no longer applies.
			resolved, err := sc.Concluded(
				domain.ScheduleResolved, domain.ResolutionSubjectConcluded, ev.FiredAt)
			if err != nil {
				return scheduler.Consumption{}, err
			}
			return scheduler.Consumption{
				Outcome: domain.OutcomeConditionNoLongerApplies, Schedule: &resolved,
			}, nil
		}
		fired, err := sc.Concluded(domain.ScheduleFired, domain.ResolutionDeadlineElapsed, ev.FiredAt)
		if err != nil {
			return scheduler.Consumption{}, err
		}
		return scheduler.Consumption{
			Outcome: domain.OutcomeHandled, Schedule: &fired,
			Commit: func(ctx context.Context, tx *store.WriteTx) error {
				// The decisive check re-runs inside the consuming transaction:
				// an operator conclusion that serialized after the handler's
				// read abandons this consumption, and the next pass records
				// the subject's conclusion instead of a stale
				// deadline_elapsed.
				item, err := tx.GetAttentionItem(ctx, *sc.Subject.ItemID)
				if errors.Is(err, store.ErrNotFound) {
					return scheduler.ErrStaleConsumption
				}
				if err != nil {
					return err
				}
				if item.Status != domain.StatusOpen || item.ItemVersion != *ev.Subject.ItemVersion {
					return scheduler.ErrStaleConsumption
				}
				return nil
			},
		}, nil
	}}
}

// recheckItemSubject is the consuming handler's §5.16 expectation recheck
// for attention-item subjects, shared by the deadline and base-watch kinds:
// a vanished item resolves with recorded proof, and a moved item version
// re-arms with the corrected binding (the deadline keeps its nominal fire
// instant, so the re-armed generation is due immediately). A nil
// consumption with a nil error means the expectation held and the caller
// proceeds with the current item.
func recheckItemSubject(
	ctx context.Context, st *store.Store, ev domain.ScheduleEvent, sc domain.Schedule,
) (domain.AttentionItem, *scheduler.Consumption, error) {
	var current domain.AttentionItem
	err := st.Read(ctx, func(tx *store.ReadTx) error {
		var readErr error
		current, readErr = tx.GetAttentionItem(ctx, *sc.Subject.ItemID)
		return readErr
	})
	if errors.Is(err, store.ErrNotFound) {
		resolved, err := sc.Concluded(
			domain.ScheduleResolved, domain.ResolutionSubjectConcluded, ev.FiredAt)
		if err != nil {
			return domain.AttentionItem{}, nil, err
		}
		return domain.AttentionItem{}, &scheduler.Consumption{
			Outcome: domain.OutcomeConditionNoLongerApplies, Schedule: &resolved,
		}, nil
	}
	if err != nil {
		return domain.AttentionItem{}, nil, err
	}
	if current.ItemVersion != *ev.Subject.ItemVersion {
		version := current.ItemVersion
		subject := sc.Subject
		subject.ItemVersion = &version
		reArmed, err := sc.ReArmed(subject, sc.FireAt, ev.FiredAt)
		if err != nil {
			return domain.AttentionItem{}, nil, err
		}
		return domain.AttentionItem{}, &scheduler.Consumption{
			Outcome: domain.OutcomeReArmed, Schedule: &reArmed,
		}, nil
	}
	return current, nil, nil
}

// baseAdvanceRegistration wires the base-advance staleness watch: observe
// the base tip, and maintain the item's base-freshness fact on material
// change only (first observation, or a changed tip), so a routine fire does
// not churn item versions while a base advance correctly invalidates
// commands prepared against the stale base claim. A transient observation
// failure is an observe_failed outcome; the recurring watch retries at its
// next nominal fire.
//
// The optional mergeCapture argument is unwired in every daemon composition;
// see its declaration for the narrow regression-test reason it remains.
func baseAdvanceRegistration(st *store.Store, observe baseTipObserver, capture mergeCapture) scheduler.Registration {
	return scheduler.Registration{
		// The built-in open-item check would resolve a concluded item's
		// watch before the handler fires, skipping the final capture pass
		// above. For this kind an existing item is live whatever its
		// status: the handler resolves a concluded subject itself, after
		// capturing; only a vanished item concludes without one.
		SubjectLive: func(ctx context.Context, sc domain.Schedule) (bool, error) {
			var item domain.AttentionItem
			err := st.Read(ctx, func(tx *store.ReadTx) error {
				var readErr error
				item, readErr = tx.GetAttentionItem(ctx, *sc.Subject.ItemID)
				return readErr
			})
			if errors.Is(err, store.ErrNotFound) {
				return false, nil
			}
			if err != nil {
				return false, err
			}
			if item.ProjectID != sc.ProjectID {
				return false, fmt.Errorf("schedule %s project %s binds item %s of project %s",
					sc.ID, sc.ProjectID, item.ID, item.ProjectID)
			}
			return true, nil
		},
		Handle: func(
			ctx context.Context, ev domain.ScheduleEvent, sc domain.Schedule,
		) (scheduler.Consumption, error) {
			// The subject recheck precedes the observation (§5.16): even a
			// failed observation must not consume against a vanished, concluded,
			// or version-moved subject.
			var current domain.AttentionItem
			if err := st.Read(ctx, func(tx *store.ReadTx) error {
				var readErr error
				current, readErr = tx.GetAttentionItem(ctx, *sc.Subject.ItemID)
				return readErr
			}); err != nil {
				if errors.Is(err, store.ErrNotFound) {
					resolved, err := sc.Concluded(
						domain.ScheduleResolved, domain.ResolutionSubjectConcluded, ev.FiredAt)
					if err != nil {
						return scheduler.Consumption{}, err
					}
					return scheduler.Consumption{
						Outcome: domain.OutcomeConditionNoLongerApplies, Schedule: &resolved,
					}, nil
				}
				return scheduler.Consumption{}, err
			}
			if current.Status != domain.StatusOpen {
				// Concluded between fire-time validation and this read: record
				// the proof now rather than re-arming a watch over a closed item.
				// This is also the merge-capture pass's usual site — the operator
				// merges, then concludes the item — so the final capture
				// observation happens before the schedule resolves; a failed
				// observation leaves the watch armed and retries, since resolving
				// uncaptured would silently drop the unit's merge state.
				captureCommit, captureErr := capture.observe(ctx, st, current, *sc.BaseWatch, ev.FiredAt)
				if captureErr != nil {
					return scheduler.Consumption{Outcome: domain.OutcomeObserveFailed}, nil
				}
				resolved, err := sc.Concluded(
					domain.ScheduleResolved, domain.ResolutionSubjectConcluded, ev.FiredAt)
				if err != nil {
					return scheduler.Consumption{}, err
				}
				return withCapture(scheduler.Consumption{
					Outcome: domain.OutcomeConditionNoLongerApplies, Schedule: &resolved,
				}, captureCommit), nil
			}
			staleExpectation := current.ItemVersion != *ev.Subject.ItemVersion
			// The merge-capture observation happens beside the base observation,
			// outside the consuming transaction. A capture failure while the item
			// is open does not block the base-freshness fact: the recurring watch
			// re-observes at its next fire, and the §5.18 projection renders the
			// gap as explicit staleness rather than this pass dropping what it
			// could observe.
			captureCommit, captureErr := capture.observe(ctx, st, current, *sc.BaseWatch, ev.FiredAt)
			if captureErr != nil {
				captureCommit = nil
			}
			observed, err := observe(ctx, *sc.BaseWatch)
			if err != nil {
				if staleExpectation {
					// The observation failed, but the event's expectation is
					// already stale: re-arm with the corrected binding rather
					// than staying bound to old state until a later successful
					// fire.
					version := current.ItemVersion
					subject := sc.Subject
					subject.ItemVersion = &version
					reArmed, err := sc.ReArmed(subject, nil, ev.FiredAt)
					if err != nil {
						return scheduler.Consumption{}, err
					}
					return withCapture(scheduler.Consumption{
						Outcome: domain.OutcomeReArmed, Schedule: &reArmed,
					}, captureCommit), nil
				}
				return withCapture(scheduler.Consumption{
					Outcome: domain.OutcomeObserveFailed,
				}, captureCommit), nil
			}
			fact := domain.BaseFreshness{
				BaseRef:         sc.BaseWatch.BaseRef,
				AdmittedBaseSHA: sc.BaseWatch.AdmittedBaseSHA,
				ObservedBaseSHA: observed,
				Advanced:        observed != sc.BaseWatch.AdmittedBaseSHA,
				ObservedAt:      ev.FiredAt,
			}
			if fact.Advanced {
				// The target base advanced past the admitted base: the clean
				// review pass no longer describes the candidate's base (plan §7,
				// issue #496). Supersede the item, recording both the base fact
				// and the readiness-invalidation reason, and conclude the
				// publication schedules; the watch does not re-arm.
				//
				// This precedes the unchanged-observation fast path below by
				// design: an item whose BaseFreshness already recorded this
				// advance under the pre-#496 record-only behavior is still open
				// with an unchanged observed tip, so keying supersession on a
				// *new* observation would let it fall through the fast path and
				// stay open indefinitely, presenting a pass bound to a base the
				// tip has already left. Superseding on the advance itself, not
				// on the observation being new, drains that upgrade population on
				// the next fire.
				invalidation := &domain.ReadinessInvalidation{
					Reason:     domain.ReadinessInvalidationBaseAdvanced,
					Bound:      sc.BaseWatch.AdmittedBaseSHA,
					Observed:   observed,
					ObservedAt: ev.FiredAt,
				}
				return withCapture(scheduler.Consumption{
					Outcome: domain.OutcomeHandled,
					Commit: func(ctx context.Context, tx *store.WriteTx) error {
						item, err := tx.GetAttentionItem(ctx, *sc.Subject.ItemID)
						if errors.Is(err, store.ErrNotFound) {
							return nil
						}
						if err != nil {
							return err
						}
						if item.Status != domain.StatusOpen || item.ItemVersion != current.ItemVersion {
							// A conclusion or any other item update serialized
							// after the handler's read: abandon, so the
							// supersession is never committed against a different
							// version, and the next pass recomputes from the
							// winning state.
							return scheduler.ErrStaleConsumption
						}
						item.BaseFreshness = &fact
						item.ReadinessInvalidation = invalidation
						item.Status = domain.StatusSuperseded
						item.ItemVersion++
						if err := tx.PutAttentionItem(ctx, item); err != nil {
							return err
						}
						return concludePublicationSchedules(ctx, tx, item.ID, ev.FiredAt)
					},
				}, captureCommit), nil
			}
			if current.BaseFreshness != nil && current.BaseFreshness.ObservedBaseSHA == observed {
				if staleExpectation {
					// No fact to write, but the event's expectation is stale
					// (§5.16): re-arm with the corrected binding instead of
					// silently consuming against it.
					version := current.ItemVersion
					subject := sc.Subject
					subject.ItemVersion = &version
					reArmed, err := sc.ReArmed(subject, nil, ev.FiredAt)
					if err != nil {
						return scheduler.Consumption{}, err
					}
					return withCapture(scheduler.Consumption{
						Outcome: domain.OutcomeReArmed, Schedule: &reArmed,
					}, captureCommit), nil
				}
				return withCapture(scheduler.Consumption{
					Outcome: domain.OutcomeHandled,
				}, captureCommit), nil
			}
			// A material change back to (or still at) the admitted base: the fact
			// write bumps the item version, so the same consumption re-arms the
			// watch with the binding it is about to create, keeping the
			// schedule's expectation current instead of leaving every later fire
			// one version behind.
			expected := current.ItemVersion + 1
			subject := sc.Subject
			subject.ItemVersion = &expected
			reArmed, err := sc.ReArmed(subject, nil, ev.FiredAt)
			if err != nil {
				return scheduler.Consumption{}, err
			}
			return withCapture(scheduler.Consumption{
				Outcome:  domain.OutcomeHandled,
				Schedule: &reArmed,
				Commit: func(ctx context.Context, tx *store.WriteTx) error {
					// The consuming-transaction recheck (§5.16): the item is
					// re-read here, so a version that moved since the event still
					// receives the fact against its current state, and a
					// concurrently concluded item receives nothing.
					item, err := tx.GetAttentionItem(ctx, *sc.Subject.ItemID)
					if errors.Is(err, store.ErrNotFound) {
						return nil
					}
					if err != nil {
						return err
					}
					if item.Status != domain.StatusOpen || item.ItemVersion != current.ItemVersion {
						// A conclusion or any other item update serialized after
						// the handler's read: abandon, so the fact and the
						// re-armed subject binding (derived from that read) are
						// never committed against a different version, and the
						// next pass recomputes from the winning state.
						return scheduler.ErrStaleConsumption
					}
					item.BaseFreshness = &fact
					item.ItemVersion++
					return tx.PutAttentionItem(ctx, item)
				},
			}, captureCommit), nil
		},
	}
}

// publicationWatchRegistrations is the shared registration set for the three
// §5.16 publication watch kinds, identical in both daemon compositions.
func publicationWatchRegistrations(
	st *store.Store, observe baseTipObserver, capture mergeCapture,
) map[domain.ScheduleKind]scheduler.Registration {
	return map[domain.ScheduleKind]scheduler.Registration{
		domain.SchedulePRChecksDeadline:    deadlineRegistration(st),
		domain.ScheduleReviewWaitThreshold: deadlineRegistration(st),
		domain.ScheduleBaseAdvanceWatch:    baseAdvanceRegistration(st, observe, capture),
	}
}

// staticBaseObserver is the fake lane's world: its base never advances.
func staticBaseObserver(_ context.Context, watch domain.ScheduleBaseWatch) (string, error) {
	return watch.AdmittedBaseSHA, nil
}

// newFakeScheduler runs the publication watch kinds for the walking-skeleton
// composition, keeping fake-lane parity with the production wiring.
func newFakeScheduler(st *store.Store) (*scheduler.Scheduler, error) {
	return scheduler.New(st, domain.ModeAttendedDev,
		func() time.Time { return time.Now().UTC() },
		publicationWatchRegistrations(st, staticBaseObserver, mergeCapture{}))
}

func newClaudeScheduler(
	st *store.Store,
	cfg config,
	wiring *claudeComposition,
	runDoctor func(context.Context) error,
) (*scheduler.Scheduler, error) {
	kinds := map[domain.ScheduleKind]scheduler.Registration{
		domain.ScheduleDoctor: {Handle: func(
			ctx context.Context, _ domain.ScheduleEvent, _ domain.Schedule,
		) (scheduler.Consumption, error) {
			if err := runScheduledDoctorPass(ctx, wiring.runConformance, runDoctor); err != nil {
				return scheduler.Consumption{}, fmt.Errorf("scheduled doctor pass: %w", err)
			}
			return scheduler.Consumption{Outcome: domain.OutcomeHandled}, nil
		}},
		domain.ScheduleJanitor: {Handle: func(
			ctx context.Context, _ domain.ScheduleEvent, _ domain.Schedule,
		) (scheduler.Consumption, error) {
			if err := wiring.janitor.RunScheduledPass(ctx); err != nil {
				return scheduler.Consumption{}, fmt.Errorf("installation janitor: %w", err)
			}
			return scheduler.Consumption{Outcome: domain.OutcomeHandled}, nil
		}},
	}
	// The installation-poll kind registers with the daemon too: a pending
	// intent recorded by the onboarding CLI keeps its durable observation
	// (and its expiry gets recorded) even when the operator never resumes.
	kinds[domain.ScheduleInstallationPoll] = installPollRegistration(wiring.authority, wiring.janitor)
	// PR/issue capture has its own plain-ticker lifecycle (active_resource.go).
	// The durable base watch observes only base freshness in production.
	for kind, reg := range publicationWatchRegistrations(st, wiring.observeBaseTip, mergeCapture{}) {
		kinds[kind] = reg
	}
	return scheduler.New(st, cfg.Claude.OperatingMode,
		func() time.Time { return time.Now().UTC() }, kinds)
}

// armTrustedConfigJobs converges the doctor and janitor schedules onto the
// current configuration. An unchanged schedule keeps its durable clock, so
// a restart preserves cadence (a missed fire coalesces with a recorded gap
// rather than resetting); a reconfigured interval re-arms under the next
// generation.
func armTrustedConfigJobs(ctx context.Context, sched *scheduler.Scheduler, cfg config) error {
	now := time.Now().UTC()
	for _, job := range []struct {
		id       domain.ScheduleID
		kind     domain.ScheduleKind
		interval time.Duration
	}{
		{doctorScheduleID, domain.ScheduleDoctor, cfg.DoctorInterval},
		{janitorScheduleID, domain.ScheduleJanitor, defaultJanitorInterval},
	} {
		seconds := int64(job.interval / time.Second)
		schedule, err := domain.NewSchedule(domain.ScheduleInput{
			ID: job.id, ProjectID: schedulerSystemProjectID, Kind: job.kind,
			Subject:   domain.ScheduleSubject{Type: domain.ScheduleSubjectTrustedConfig},
			CreatedAt: now, IntervalSeconds: &seconds,
		})
		if err != nil {
			return fmt.Errorf("arm %s: %w", job.id, err)
		}
		// The startup obligation already ran synchronously, so a fresh
		// schedule's first fire is one interval out; an existing schedule
		// keeps its own clock.
		if err := sched.Arm(ctx, schedule, now.Add(job.interval)); err != nil {
			return fmt.Errorf("arm %s: %w", job.id, err)
		}
	}
	return nil
}
