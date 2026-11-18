package integration_test

import (
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// staleAdmittedTrustProfile rewrites the body of the profile the run was
// admitted under so the row no longer reconstructs under this build, as a
// profile encoding bump leaves every row recorded before it (#3171). The
// digest key stays, so the run stays bound to it. The row is also the
// repository's active profile, as it would be after a real bump.
func staleAdmittedTrustProfile(t *testing.T, p *productionPublicationHarness) domain.Digest {
	t.Helper()
	digest := p.profile.ProfileDigest
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		admission, err := tx.GetExecutionAdmissionRecord(p.ctx, p.invocation)
		if err != nil {
			return err
		}
		if admission.TrustProfileDigest == nil || *admission.TrustProfileDigest != digest {
			t.Fatalf("admitted profile = %v, want %s", admission.TrustProfileDigest, digest)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", p.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close() //nolint:errcheck // the update's own error is checked
	var body string
	if err := raw.QueryRowContext(p.ctx,
		`SELECT body FROM trust_profiles WHERE profile_digest = ?`, digest).Scan(&body); err != nil {
		t.Fatal(err)
	}
	staled := strings.Replace(body, `"allow_self_hosted_ci":false`, `"allow_self_hosted_ci":true`, 1)
	if staled == body {
		t.Fatal("profile body change did not apply")
	}
	if _, err := raw.ExecContext(p.ctx,
		`UPDATE trust_profiles SET body = ? WHERE profile_digest = ?`, staled, digest); err != nil {
		t.Fatal(err)
	}
	return digest
}

// blockOnUnreadableAdmittedProfile runs the first publication pass of a run
// whose admitted profile no longer reads, and checks the per-run outcome: the
// definitive trust block, its milestone, a completed task, and no
// publication state at all.
func blockOnUnreadableAdmittedProfile(t *testing.T, p *productionPublicationHarness) signet.AttentionItemSnapshot {
	t.Helper()
	p.startAndRecordExport(t)
	stale := staleAdmittedTrustProfile(t, p)
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	result, err := p.reconcileLanes()
	if err != nil || result.PublicationTasksCompleted != 1 || result.BlockedItemsCreated != 1 ||
		result.ReadyItemsCreated != 0 {
		t.Fatalf("unreadable admitted profile pass = %#v, %v; want one completed block and no lane error", result, err)
	}
	blocked, err := p.attention.GetAttentionItem(p.ctx, domain.ProductionBlockedItemID(p.runID))
	if err != nil {
		t.Fatal(err)
	}
	if blocked.Item.Status != domain.StatusOpen || blocked.Item.Reason != domain.PublicationBlockTrust ||
		!slices.Equal(blocked.Item.RequestedDecision, []domain.Action{
			domain.ActionRerunTrustEvaluation, domain.ActionInspectTrustFailure, domain.ActionStop,
		}) ||
		blocked.Item.PRHeadSHA != p.replay.HeadSHA ||
		blocked.Item.PublishBlock == nil || blocked.Item.PublishBlock.TrustRule == nil ||
		*blocked.Item.PublishBlock.TrustRule != domain.TrustRuleTrustProfileDrift {
		t.Fatalf("unreadable admitted profile block = %#v", blocked.Item)
	}
	if conclusion := authenticatedProductionConclusion(t, p); !conclusion.Final ||
		conclusion.Outcome != domain.RunOutcomeBlocked ||
		conclusion.Reason == nil || *conclusion.Reason != domain.HoldTrustBlocked {
		t.Fatalf("unreadable admitted profile conclusion = %#v", conclusion)
	}
	assertNothingAuthorizedUnder(t, p, stale)
	if p.room.runs != 0 {
		t.Fatalf("an unreadable admitted profile verified the candidate %d times", p.room.runs)
	}
	return blocked
}

// assertNothingAuthorizedUnder checks that no candidate authorization names
// the stale digest, and that no publication intent, forge effect, or ready
// item exists. The invocation's reservation, which export recorded under the
// intent's key before the profile went stale, may stay: it authorizes
// nothing, and a rerun upgrades it.
func assertNothingAuthorizedUnder(t *testing.T, p *productionPublicationHarness, stale domain.Digest) {
	t.Helper()
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		authorizations, err := tx.ListCandidateAuthorizations(p.ctx, fakePublicationRepo, p.replay.HeadSHA)
		if err != nil {
			return err
		}
		for _, authorization := range authorizations {
			if authorization.TrustProfileDigest == stale {
				t.Errorf("candidate authorization %s names the stale profile", authorization.ID)
			}
		}
		key, err := publish.IntentKey(domain.ProductionPublicationInvocationID(p.runID), publish.IntentKindPublication)
		if err != nil {
			return err
		}
		if entry, err := tx.GetOutbox(p.ctx, key); err != nil || entry.Kind != publish.IntentKindReservation {
			t.Errorf("publication key holds %q, %v; want the export's reservation", entry.Kind, err)
		}
		if _, err := tx.GetAttentionItem(p.ctx, domain.ProductionReadyItemID(p.runID)); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("ready item under the stale profile: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if refs, prs := p.forge.Counts(); refs != 0 || prs != 0 {
		t.Fatalf("stale profile reached the forge: %d refs, %d PRs", refs, prs)
	}
}

// TestUnreadableAdmittedProfileBlocksOneRun: a profile encoding bump leaves a
// pending publication bound to a profile row this build can't read. That run
// ends in the definitive trust block instead of failing the lane, which would
// stop the daemon; after the owner approves a current profile, the block's
// rerun_trust_evaluation carries the run to a published pull request under it.
func TestUnreadableAdmittedProfileBlocksOneRun(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	stale := p.profile.ProfileDigest
	blocked := blockOnUnreadableAdmittedProfile(t, p)

	current := repairPublicationTrustProfile(t, p)
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	submitPublicationRerun(t, p, blocked, "rerun-unreadable-profile")
	var published engine.ReconcileResult
	for range 4 {
		var err error
		published, err = p.reconcileLanes()
		if err != nil {
			t.Fatal(err)
		}
		if published.ReadyItemsCreated == 1 {
			break
		}
	}
	if published.ReadyItemsCreated != 1 || published.PublicationTasksCompleted != 1 {
		t.Fatalf("rerun under the current profile = %#v", published)
	}
	if refs, prs := p.forge.Counts(); refs != 1 || prs != 1 {
		t.Fatalf("rerun forge effects = %d refs, %d PRs, want one each", refs, prs)
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		authorizations, err := tx.ListCandidateAuthorizations(p.ctx, fakePublicationRepo, p.replay.HeadSHA)
		if err != nil {
			return err
		}
		if len(authorizations) != 1 || authorizations[0].TrustProfileDigest != current.ProfileDigest ||
			authorizations[0].TrustProfileDigest == stale || !authorizations[0].AuthorizesPublication {
			t.Fatalf("rerun authorizations = %#v, want one under %s", authorizations, current.ProfileDigest)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestUnreadableAdmittedProfileRerunCanStillBlock: the rerun is gated like any
// other publication, so a current trust state that refuses the candidate ends
// it in a definitive block, not a pull request.
func TestUnreadableAdmittedProfileRerunCanStillBlock(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	stale := p.profile.ProfileDigest
	blocked := blockOnUnreadableAdmittedProfile(t, p)

	repairPublicationTrustProfile(t, p)
	p.audit.EffectiveTokenPerms = domain.TokenPermissionsReadWrite
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	const commandID = "rerun-refused"
	submitPublicationRerun(t, p, blocked, commandID)
	var refused engine.ReconcileResult
	for range 4 {
		var err error
		refused, err = p.reconcileLanes()
		if err != nil {
			t.Fatal(err)
		}
		if refused.BlockedItemsCreated == 1 {
			break
		}
	}
	if refused.BlockedItemsCreated != 1 || refused.ReadyItemsCreated != 0 {
		t.Fatalf("refused rerun = %#v", refused)
	}
	rerun, err := p.attention.GetAttentionItem(p.ctx, signet.ReevaluatedBlockedItemID(p.runID, commandID))
	if err != nil {
		t.Fatal(err)
	}
	if rerun.Item.Status != domain.StatusOpen || rerun.Item.Reason != domain.PublicationBlockTrust {
		t.Fatalf("refused rerun block = %#v", rerun.Item)
	}
	assertNothingAuthorizedUnder(t, p, stale)
}

// TestUnreadableAdmittedProfileHoldsACommittedIntent: when the publication
// intent is already committed, a forge effect may be in flight or a pull
// request may already exist, so the run holds instead of ending in a
// definitive block, and nothing stops the lane. The intent stays as it was.
func TestUnreadableAdmittedProfileHoldsACommittedIntent(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		interrupt func(p *productionPublicationHarness) productionCrashSeams
		finalized bool
	}{
		{name: "pending", interrupt: func(p *productionPublicationHarness) productionCrashSeams {
			p.forge.FailAfterNextPRCreate()
			return productionCrashSeams{}
		}},
		{name: "finalized", finalized: true, interrupt: func(*productionPublicationHarness) productionCrashSeams {
			return productionCrashSeams{afterPublication: func() error {
				return errors.New("stop after publication, before readiness")
			}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := newProductionPublicationHarness(t, "")
			p.workflow = p.newEngine(t, tc.interrupt(p), true)
			p.startAndRecordExport(t)
			_, _ = p.reconcileLanes() // the interruption is the setup; the intent state below proves it
			key, err := publish.IntentKey(domain.ProductionPublicationInvocationID(p.runID), publish.IntentKindPublication)
			if err != nil {
				t.Fatal(err)
			}
			intentFinalized := func() bool {
				t.Helper()
				var entry store.QueueEntry
				if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
					var err error
					entry, err = tx.GetOutbox(p.ctx, key)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if entry.Kind != publish.IntentKindPublication {
					t.Fatalf("interrupted publication left %q, want a publication intent", entry.Kind)
				}
				return entry.Dispatched()
			}
			if intentFinalized() != tc.finalized {
				t.Fatalf("interrupted publication intent finalized = %t, want %t", !tc.finalized, tc.finalized)
			}

			staleAdmittedTrustProfile(t, p)
			p.workflow = p.newEngine(t, productionCrashSeams{}, true)
			result, err := p.reconcileLanes()
			if err != nil || result.PublicationTasksCompleted != 0 || result.BlockedItemsCreated != 1 ||
				result.ReadyItemsCreated != 0 {
				t.Fatalf("committed intent under an unreadable profile = %#v, %v; want a hold and no lane error", result, err)
			}
			hold, err := p.attention.GetAttentionItem(p.ctx, domain.ProductionBlockedItemID(p.runID))
			if err != nil {
				t.Fatal(err)
			}
			if hold.Item.Status != domain.StatusOpen || !signet.PublicationHoldDecision(hold.Item) ||
				!strings.Contains(hold.Item.Reason, "admitted under no longer reads") {
				t.Fatalf("committed intent hold = %#v", hold.Item)
			}
			if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
				observed, held, err := tx.GetRunHold(p.ctx, p.runID)
				if err != nil {
					return err
				}
				if !held || observed.Reason != domain.HoldTrustBlocked {
					t.Fatalf("committed intent run hold = %#v, %t", observed, held)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if intentFinalized() != tc.finalized {
				t.Fatal("the hold changed the committed intent")
			}
			if conclusion := authenticatedProductionConclusion(t, p); conclusion.Final {
				t.Fatalf("held run concluded: %#v", conclusion)
			}
		})
	}
}

// TestUnreadableAdmittedProfileStopsAReenteredCycle: a cycle re-entered on a
// published pull request stops on that pull request.
func TestUnreadableAdmittedProfileStopsAReenteredCycle(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.publishReady(t)
	p.startHeadChangeReentry(t)
	runs := p.room.runs
	staleAdmittedTrustProfile(t, p)
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	result, err := p.reconcileLanes()
	if err != nil || result.BlockedItemsCreated != 1 || result.ReadyItemsCreated != 0 {
		t.Fatalf("re-entered cycle under an unreadable profile = %#v, %v; want a stop and no lane error", result, err)
	}
	stop := p.reentryStopItem(t)
	if !strings.Contains(stop.Reason, "admitted under no longer reads") {
		t.Fatalf("re-entry stop reason = %q", stop.Reason)
	}
	if p.room.runs != runs {
		t.Fatalf("a stopped cycle verified its head %d times", p.room.runs-runs)
	}
}

// TestUnreadableAdmittedProfileMakesFeedbackUndeliverable: return_to_agent
// on a published run rebuilds the candidate patch from its admitted binding.
// With that profile unreadable, the one command is undeliverable and the lane
// keeps running.
func TestUnreadableAdmittedProfileMakesFeedbackUndeliverable(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.startAndRecordExport(t)
	if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 1 {
		t.Fatalf("publish baseline: %#v, %v", result, err)
	}
	ready, err := p.attention.GetAttentionItem(p.ctx, domain.ProductionReadyItemID(p.runID))
	if err != nil {
		t.Fatal(err)
	}
	const commandID = "return-unreadable-profile"
	const deviceID domain.DeviceID = "feedback-device"
	if err := p.store.Write(p.ctx, func(tx *store.WriteTx) error {
		return tx.PutDevice(p.ctx, domain.Device{
			ID: deviceID, DisplayName: "Feedback device", Status: domain.DeviceActive, PairedAt: p.now,
		})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.attention.Submit(p.ctx, signet.ClientCommand{
		CommandID: commandID, DeviceID: deviceID, ExpectedEntityVersion: ready.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: ready.Item.ID, ItemVersion: ready.Item.ItemVersion,
			PRHeadSHA: ready.Item.PRHeadSHA, ArtifactDigests: ready.Item.ArtifactDigests,
			Action: domain.ActionReturnToAgent, Message: "Tighten the decision note.",
		},
	}); err != nil {
		t.Fatal(err)
	}

	staleAdmittedTrustProfile(t, p)
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	if _, err := p.workflow.ReconcileProductionPublications(p.ctx); err != nil {
		t.Fatalf("feedback under an unreadable profile stopped the lane: %v", err)
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		notice, err := tx.GetAttentionItem(p.ctx, domain.ItemID("operator-feedback-undeliverable-"+commandID))
		if err != nil {
			return err
		}
		if notice.Type != domain.AttentionSystemHealth || notice.Status != domain.StatusOpen {
			t.Fatalf("undeliverable notice = %#v", notice)
		}
		invocation := domain.InvocationID("inv-operator-feedback-" + commandID)
		if _, err := tx.GetExecutionAdmissionRecord(p.ctx, invocation); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("undeliverable feedback admitted: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
