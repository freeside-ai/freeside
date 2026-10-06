package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publicationrecord"
)

// externalReviewFixture is a published ready item an admitted external
// reviewer commented on: the clean pass that earned it, the finding, the
// profile that admits the reviewer, and the authority current state proves.
type externalReviewFixture struct {
	reentryFixture
	finding   domain.Finding
	admitting domain.AutomationTrustProfile
	// Profiles recorded for the same repository name that do not admit the
	// reviewer, each superseded as the active profile by admitting.
	unlisted, renamed, otherAccount, otherRepository domain.AutomationTrustProfile
}

type externalReviewOptions struct {
	// leaveOpen skips superseding the predecessor.
	leaveOpen bool
	// priorRound is the round of the review the cycle follows; zero means 1.
	priorRound int
}

func externalFindingWithText(t *testing.T, runID domain.RunID, head, raw string) domain.Finding {
	t.Helper()
	finding, err := domain.NewExternalFinding(domain.ExternalFindingInput{
		RunID: runID, Forge: domain.ExternalReviewForgeGitHub,
		ReviewerAccountID: externalReviewerID, ReviewerLogin: externalReviewerLogin,
		ThreadID: "PRRT_kwDOexample", HeadSHA: head, Message: "unchecked error", RawText: raw, CreatedAt: reentryAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	return finding
}

// supersedeReadyItem withdraws an open ready item with no invalidation fact,
// as the transaction that seals an external_review authority does.
func supersedeReadyItem(t *testing.T, st *Store, id domain.ItemID) {
	t.Helper()
	ctx := context.Background()
	if err := st.Write(ctx, func(tx *WriteTx) error {
		item, err := tx.GetAttentionItem(ctx, id)
		if err != nil {
			return err
		}
		item.Status = domain.StatusSuperseded
		item.ItemVersion++
		return tx.PutAttentionItem(ctx, item)
	}); err != nil {
		t.Fatal(err)
	}
}

func externalReviewAuthority(
	runID domain.RunID, predecessor domain.ItemID, prior domain.ReviewRecord,
	finding domain.Finding, profile domain.AutomationTrustProfile,
) domain.PublicationSuccessor {
	return domain.PublicationSuccessor{
		Version: domain.PublicationExternalReviewVersion, Origin: domain.PublicationSuccessorExternalReview,
		RunID: runID, PredecessorItemID: predecessor,
		PriorReviewInvocationID: prior.InvocationID, ReviewRound: prior.Round + 1,
		Reentry:           &domain.PublicationSuccessorReentry{BaseSHA: prior.BaseSHA, HeadSHA: finding.External.HeadSHA},
		ExternalFindingID: finding.ID, AdmittingProfileDigest: profile.ProfileDigest,
	}
}

func seedExternalReview(t *testing.T, opts externalReviewOptions) externalReviewFixture {
	t.Helper()
	f := externalReviewFixture{reentryFixture: reentryFixture{
		readyBindingFixture: seedReadyItemBinding(t, "feat/meaningful-task", reentryBase1, reentryHead1),
	}}
	prior := putReentryReview(t, f.st, f.run.ID, max(opts.priorRound, 1), reentryBase1, reentryHead1)
	repo, id := f.binding.Repo, f.binding.RepositoryID
	at := func(minute int) time.Time { return reentryAt.Add(time.Duration(minute) * time.Minute) }
	f.unlisted = activateProfile(t, f.st, repo, id, at(1), reviewerEntry(900, "maintainer"))
	f.renamed = activateProfile(t, f.st, repo, id, at(2), reviewerEntry(externalReviewerID, "codex-renamed[bot]"))
	f.otherAccount = activateProfile(t, f.st, repo, id, at(3), reviewerEntry(externalReviewerID+1, externalReviewerLogin))
	f.otherRepository = activateProfile(t, f.st, repo, id+1, at(4), reviewerEntry(externalReviewerID, externalReviewerLogin))
	f.admitting = activateProfile(t, f.st, repo, id, at(5), reviewerEntry(externalReviewerID, externalReviewerLogin))
	f.finding = externalFindingWithText(t, f.run.ID, reentryHead1, "P1: the error return is dropped")
	putExternalFinding(t, f.st, f.finding)
	if !opts.leaveOpen {
		supersedeReadyItem(t, f.st, f.item.ID)
	}
	f.authority = externalReviewAuthority(f.run.ID, f.item.ID, prior, f.finding, f.admitting)
	return f
}

// gate runs the authority gate alone, without the seal-time checks
// RecordPublicationSuccessor adds.
func (f externalReviewFixture) gate(authority domain.PublicationSuccessor) error {
	ctx := context.Background()
	return f.st.Read(ctx, func(tx *ReadTx) error { return tx.validatePublicationSuccessor(ctx, authority) })
}

// TestExternalReviewAuthorityAccepted: an admitted reviewer's finding on the
// published head seals a commandless cycle on that same base and head, and
// the cycle becomes the run's current one without having re-earned anything.
func TestExternalReviewAuthorityAccepted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := seedExternalReview(t, externalReviewOptions{})
	if err := f.record(t, f.authority); err != nil {
		t.Fatal(err)
	}
	if err := f.record(t, f.authority); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if err := f.st.Read(ctx, func(tx *ReadTx) error {
		got, err := tx.PublicationSuccessorForReadyItem(ctx, f.run.ID, f.authority.ReadyItemID())
		if err != nil {
			return err
		}
		if got.ExternalFindingID != f.finding.ID || got.AdmittingProfileDigest != f.admitting.ProfileDigest ||
			*got.Reentry != *f.authority.Reentry {
			t.Fatalf("resolved %#v", got)
		}
		current, err := tx.CurrentProductionReadyItemID(ctx, f.run.ID)
		if err != nil {
			return err
		}
		if current != f.authority.ReadyItemID() {
			t.Fatalf("current ready item = %q, want the re-entered cycle's %q", current, f.authority.ReadyItemID())
		}
		published, err := tx.PublishedProductionReadyItemID(ctx, f.run.ID)
		if err != nil {
			return err
		}
		if published != f.item.ID {
			t.Fatalf("published ready item = %q, want %q", published, f.item.ID)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// The pull request did not move, so the target is the published head.
	var target publicationrecord.SuccessorTarget
	if err := f.st.Read(ctx, func(tx *ReadTx) (err error) {
		target, err = tx.PublicationSuccessorTarget(ctx, f.run.ID, f.authority.PublicationID())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	want := publicationrecord.SuccessorTarget{
		ItemID: f.item.ID, Identity: f.binding.PublicationIdentity, HeadSHA: reentryHead1,
		PRNumber: f.binding.PRNumber, Branch: "feat/meaningful-task",
	}
	if target != want {
		t.Fatalf("external review target = %#v, want %#v", target, want)
	}
}

// TestExternalReviewAuthorityRejectsEachMismatch offers a caller-supplied
// record that disagrees with current state on one fact at a time. Each is
// refused by the gate itself, so the same record is refused on every read.
func TestExternalReviewAuthorityRejectsEachMismatch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mismatch, notAdmitted := domain.ErrParentKeyMismatch, domain.ErrExternalReviewNotAdmitted
	for _, tc := range []struct {
		name   string
		want   error
		mutate func(externalReviewFixture, *domain.PublicationSuccessor)
	}{
		{"finding on another head", mismatch, func(f externalReviewFixture, s *domain.PublicationSuccessor) {
			other := externalFindingWithText(t, f.run.ID, reentryHead2, "P1: on a head that was never published")
			putExternalFinding(t, f.st, other)
			s.ExternalFindingID = other.ID
		}},
		{"finding of another run", mismatch, func(f externalReviewFixture, s *domain.PublicationSuccessor) {
			run := f.run
			run.ID, run.Stages = "run-other", nil
			if err := f.st.Write(ctx, func(tx *WriteTx) error { return tx.PutRun(ctx, run) }); err != nil {
				t.Fatal(err)
			}
			other := externalFindingWithText(t, run.ID, reentryHead1, "P1: on another run's pull request")
			putExternalFinding(t, f.st, other)
			s.ExternalFindingID = other.ID
		}},
		{"finding from a Freeside review", mismatch, func(f externalReviewFixture, s *domain.PublicationSuccessor) {
			native := domain.Finding{
				ID: "find-native", RunID: f.run.ID, Source: "codex_github", Message: "m", RawText: "r", CreatedAt: reentryAt,
			}
			if err := f.st.Write(ctx, func(tx *WriteTx) error { return tx.PutFinding(ctx, native) }); err != nil {
				t.Fatal(err)
			}
			s.ExternalFindingID = native.ID
		}},
		{"unknown finding", mismatch, func(_ externalReviewFixture, s *domain.PublicationSuccessor) {
			s.ExternalFindingID = "external-unknown"
		}},
		{"unknown profile", notAdmitted, func(_ externalReviewFixture, s *domain.PublicationSuccessor) {
			s.AdmittingProfileDigest = "sha256:unknown"
		}},
		{"profile that lists someone else", notAdmitted, func(f externalReviewFixture, s *domain.PublicationSuccessor) {
			s.AdmittingProfileDigest = f.unlisted.ProfileDigest
		}},
		{"profile listing the account under another login", notAdmitted, func(f externalReviewFixture, s *domain.PublicationSuccessor) {
			s.AdmittingProfileDigest = f.renamed.ProfileDigest
		}},
		{"profile listing the login under another account", notAdmitted, func(f externalReviewFixture, s *domain.PublicationSuccessor) {
			s.AdmittingProfileDigest = f.otherAccount.ProfileDigest
		}},
		{"profile for another repository ID", notAdmitted, func(f externalReviewFixture, s *domain.PublicationSuccessor) {
			s.AdmittingProfileDigest = f.otherRepository.ProfileDigest
		}},
		{"base the last review did not use", mismatch, func(_ externalReviewFixture, s *domain.PublicationSuccessor) {
			s.Reentry.BaseSHA = reentryBase2
		}},
		{"head that is not published", mismatch, func(_ externalReviewFixture, s *domain.PublicationSuccessor) {
			s.Reentry.HeadSHA = reentryHead2
		}},
		{"round not one above prior", mismatch, func(_ externalReviewFixture, s *domain.PublicationSuccessor) { s.ReviewRound = 3 }},
		{"unknown prior review", mismatch, func(_ externalReviewFixture, s *domain.PublicationSuccessor) {
			s.PriorReviewInvocationID = "review-unknown"
		}},
		{"other run", mismatch, func(_ externalReviewFixture, s *domain.PublicationSuccessor) { s.RunID = "run-elsewhere" }},
		{"unknown predecessor", mismatch, func(_ externalReviewFixture, s *domain.PublicationSuccessor) {
			s.PredecessorItemID = "production-ready-unknown"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := seedExternalReview(t, externalReviewOptions{})
			forged := f.authority
			reentry := *f.authority.Reentry
			forged.Reentry = &reentry
			tc.mutate(f, &forged)
			if err := forged.Validate(); err != nil {
				t.Fatalf("mutation is not a store-gate case: %v", err)
			}
			if err := f.gate(forged); !errors.Is(err, tc.want) {
				t.Fatalf("gate error = %v, want %v", err, tc.want)
			}
			if err := f.record(t, forged); err == nil {
				t.Fatal("forged authority sealed")
			}
			if err := f.record(t, f.authority); err != nil {
				t.Fatalf("true authority refused after the forgery: %v", err)
			}
		})
	}
}

// TestExternalReviewAuthorityRegatesCurrentState: the gate reads the
// predecessor, the finding and the named profile from the store, at record
// time and again on every read. The sealed record itself cannot be rewritten
// to test with: the outbox payload digest refuses that before the gate runs,
// and GetPublicationSuccessor runs the same gate function the mismatch table
// above calls directly.
func TestExternalReviewAuthorityRegatesCurrentState(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	t.Run("predecessor still open", func(t *testing.T) {
		f := seedExternalReview(t, externalReviewOptions{leaveOpen: true})
		if err := f.record(t, f.authority); !errors.Is(err, domain.ErrParentKeyMismatch) {
			t.Fatalf("error = %v, want ErrParentKeyMismatch", err)
		}
	})
	t.Run("predecessor invalidated", func(t *testing.T) {
		// An invalidated item re-enters through its own origin, which
		// re-checks the coordinate that moved; this origin assumes none did.
		f := seedExternalReview(t, externalReviewOptions{leaveOpen: true})
		invalidateReadyItem(t, f.st, f.item.ID,
			reentryFact(domain.ReadinessInvalidationBaseAdvanced, reentryBase1, reentryBase2))
		if err := f.record(t, f.authority); !errors.Is(err, domain.ErrParentKeyMismatch) {
			t.Fatalf("error = %v, want ErrParentKeyMismatch", err)
		}
	})
	t.Run("readiness re-entry of an uninvalidated predecessor", func(t *testing.T) {
		// The new origin does not widen the old one.
		f := seedExternalReview(t, externalReviewOptions{})
		readiness := f.authority
		readiness.Version, readiness.Origin = domain.PublicationReentryVersion, domain.PublicationSuccessorReadinessInvalidation
		readiness.ExternalFindingID, readiness.AdmittingProfileDigest = "", ""
		readiness.Reentry = &domain.PublicationSuccessorReentry{
			Reason: domain.ReadinessInvalidationBaseAdvanced, BaseSHA: reentryBase2, HeadSHA: reentryHead1,
		}
		if err := f.record(t, readiness); !errors.Is(err, domain.ErrParentKeyMismatch) {
			t.Fatalf("error = %v, want ErrParentKeyMismatch", err)
		}
	})
	for _, tc := range []struct {
		name string
		want error
		sql  func(externalReviewFixture) (string, []any)
	}{
		{"finding row removed", domain.ErrParentKeyMismatch, func(f externalReviewFixture) (string, []any) {
			return `DELETE FROM findings WHERE id = ?`, []any{f.finding.ID}
		}},
		{"invalidation written onto the predecessor", domain.ErrParentKeyMismatch, func(f externalReviewFixture) (string, []any) {
			return `UPDATE attention_items SET body = json_set(body, '$.readiness_invalidation', json(?)) WHERE id = ?`,
				[]any{`{"reason":"base_advanced","bound":"` + reentryBase1 + `","observed":"` + reentryBase2 +
					`","observed_at":"2026-08-02T12:00:00Z"}`, f.item.ID}
		}},
		{"binding removed", domain.ErrParentKeyMismatch, func(f externalReviewFixture) (string, []any) {
			return `DELETE FROM ready_item_pr_bindings WHERE item_id = ?`, []any{f.item.ID}
		}},
	} {
		t.Run("read after "+tc.name, func(t *testing.T) {
			f := seedExternalReview(t, externalReviewOptions{})
			if err := f.record(t, f.authority); err != nil {
				t.Fatal(err)
			}
			if err := f.read(t, f.authority); err != nil {
				t.Fatal(err)
			}
			query, args := tc.sql(f)
			result, err := f.st.db.ExecContext(ctx, query, args...)
			if err != nil {
				t.Fatal(err)
			}
			if changed, _ := result.RowsAffected(); changed != 1 {
				t.Fatalf("tamper changed %d rows, want 1", changed)
			}
			if err := f.read(t, f.authority); !errors.Is(err, tc.want) {
				t.Fatalf("read error = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestExternalReviewCycleSurvivesAllowlistEdit follows a cycle to readiness
// and through the owner removing the reviewer: the sealed cycle and the item
// it re-earned keep reading under the profile the authority named, a new
// cycle for the same reviewer is refused, and restoring the entry admits one.
func TestExternalReviewCycleSurvivesAllowlistEdit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := seedExternalReview(t, externalReviewOptions{})
	if err := f.record(t, f.authority); err != nil {
		t.Fatal(err)
	}
	// The cycle pushes nothing and re-earns readiness in place.
	item, binding := f.reenteredItemAndBinding(t, f.authority, f.binding)
	f.putItem(t, item)
	foreign := binding
	foreign.PRNumber++
	if err := f.recordBinding(foreign); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("in-place binding on another pull request: %v, want ErrParentKeyMismatch", err)
	}
	if err := f.recordBinding(binding); err != nil {
		t.Fatal(err)
	}

	// The owner removes the reviewer.
	removed := activateProfile(t, f.st, f.binding.Repo, f.binding.RepositoryID, reentryAt.Add(time.Hour))
	if err := f.read(t, f.authority); err != nil {
		t.Fatalf("sealed authority after the reviewer was removed: %v", err)
	}
	if err := f.st.Read(ctx, func(tx *ReadTx) error {
		got, err := tx.GetReadyItemPRBinding(ctx, item.ID)
		if err != nil {
			return err
		}
		if got.HeadSHA != reentryHead1 || got.PublicationIdentity != f.binding.PublicationIdentity {
			t.Fatalf("binding = %#v", got)
		}
		published, err := tx.PublishedProductionReadyItemID(ctx, f.run.ID)
		if err != nil {
			return err
		}
		if published != item.ID {
			t.Fatalf("published ready item = %q, want the re-entered %q", published, item.ID)
		}
		return nil
	}); err != nil {
		t.Fatalf("re-earned item after the reviewer was removed: %v", err)
	}
	if _, err := admittedRead(f.st, f.finding.ID); !errors.Is(err, domain.ErrExternalReviewNotAdmitted) {
		t.Fatalf("admitted read after the reviewer was removed: %v, want ErrExternalReviewNotAdmitted", err)
	}

	// The same reviewer comments again on the re-earned item.
	prior := putReentryReview(t, f.st, f.run.ID, 2, reentryBase1, reentryHead1)
	again := externalFindingWithText(t, f.run.ID, reentryHead1, "P1: still dropped")
	putExternalFinding(t, f.st, again)
	supersedeReadyItem(t, f.st, item.ID)
	next := externalReviewAuthority(f.run.ID, item.ID, prior, again, f.admitting)
	if err := f.record(t, next); !errors.Is(err, domain.ErrExternalReviewNotAdmitted) {
		t.Fatalf("new cycle under a profile that is no longer active: %v, want ErrExternalReviewNotAdmitted", err)
	}
	underActive := next
	underActive.AdmittingProfileDigest = removed.ProfileDigest
	if err := f.record(t, underActive); !errors.Is(err, domain.ErrExternalReviewNotAdmitted) {
		t.Fatalf("new cycle under the active profile, which lists nobody: %v, want ErrExternalReviewNotAdmitted", err)
	}
	// The first item is no longer the run's current ready item, so it cannot
	// start a second cycle whatever the allowlist says.
	if err := f.st.WriteInternal(ctx, func(tx *InternalTx) error {
		return tx.ActivateTrustProfile(ctx, f.binding.Repo, f.admitting.ProfileDigest, reentryAt.Add(2*time.Hour))
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.record(t, next); err != nil {
		t.Fatalf("new cycle after the owner restored the reviewer: %v", err)
	}
	if err := f.st.Read(ctx, func(tx *ReadTx) error {
		chain, err := tx.PublicationSuccessorChain(ctx, f.run.ID)
		if err != nil {
			return err
		}
		if len(chain) != 2 || chain[1].ReadyItemID() != next.ReadyItemID() {
			t.Fatalf("chain = %#v", chain)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestExternalReviewOneCyclePerFinding: a cycle that re-earns readiness in
// place leaves its triggering finding on the published head, where the gate
// alone would admit it again. Sealing refuses a finding any earlier cycle of
// the run already named.
func TestExternalReviewOneCyclePerFinding(t *testing.T) {
	t.Parallel()
	f := seedExternalReview(t, externalReviewOptions{})
	if err := f.record(t, f.authority); err != nil {
		t.Fatal(err)
	}
	item, binding := f.reenteredItemAndBinding(t, f.authority, f.binding)
	f.putItem(t, item)
	if err := f.recordBinding(binding); err != nil {
		t.Fatal(err)
	}
	prior := putReentryReview(t, f.st, f.run.ID, 2, reentryBase1, reentryHead1)
	supersedeReadyItem(t, f.st, item.ID)
	again := externalReviewAuthority(f.run.ID, item.ID, prior, f.finding, f.admitting)
	if err := f.gate(again); err != nil {
		t.Fatalf("the gate alone no longer admits the reused finding, so this test proves nothing: %v", err)
	}
	if err := f.record(t, again); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("second cycle from one finding: %v, want ErrParentKeyMismatch", err)
	}
}

// TestExternalReviewRefusedOnAForeignHead: the origin admits remediation,
// which would replace the published head, so it is refused while that head is
// one someone else pushed, including when a base advance was re-earned on top
// of it. A base advance over Freeside's own head stays admissible.
func TestExternalReviewRefusedOnAForeignHead(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		reason  domain.ReadinessInvalidationReason
		foreign bool
	}{
		{"head someone else pushed", domain.ReadinessInvalidationHeadChanged, true},
		{"own head after a base advance", domain.ReadinessInvalidationBaseAdvanced, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := seedReentry(t, tc.reason, reentryOptions{})
			if err := r.record(t, r.authority); err != nil {
				t.Fatal(err)
			}
			item, binding := r.reenteredItemAndBinding(t, r.authority, r.binding)
			r.putItem(t, item)
			if err := r.recordBinding(binding); err != nil {
				t.Fatal(err)
			}
			base, head := r.authority.Reentry.BaseSHA, r.authority.Reentry.HeadSHA
			admitting := activateProfile(t, r.st, r.binding.Repo, r.binding.RepositoryID, reentryAt,
				reviewerEntry(externalReviewerID, externalReviewerLogin))
			finding := externalFindingWithText(t, r.run.ID, head, "P1: the error return is dropped")
			putExternalFinding(t, r.st, finding)

			// The same question one level further down: the base advances
			// under the re-earned item and readiness is re-earned again.
			prior := putReentryReview(t, r.st, r.run.ID, 2, base, head)
			invalidateReadyItem(t, r.st, item.ID, reentryFact(domain.ReadinessInvalidationBaseAdvanced, base, reentryBase3))
			advanced := domain.PublicationSuccessor{
				Version: domain.PublicationReentryVersion, Origin: domain.PublicationSuccessorReadinessInvalidation,
				RunID: r.run.ID, PredecessorItemID: item.ID, PriorReviewInvocationID: prior.InvocationID, ReviewRound: 3,
				Reentry: &domain.PublicationSuccessorReentry{
					Reason: domain.ReadinessInvalidationBaseAdvanced, BaseSHA: reentryBase3, HeadSHA: head,
				},
			}
			if err := r.record(t, advanced); err != nil {
				t.Fatal(err)
			}
			again, againBinding := r.reenteredItemAndBinding(t, advanced, binding)
			r.putItem(t, again)
			if err := r.recordBinding(againBinding); err != nil {
				t.Fatal(err)
			}
			prior = putReentryReview(t, r.st, r.run.ID, 3, reentryBase3, head)
			// The trigger asks the same question from a read, while the item
			// is still open, so a refusal withdraws nothing.
			for id, want := range map[domain.ItemID]bool{r.item.ID: false, again.ID: tc.foreign} {
				var foreign bool
				if err := r.st.Read(context.Background(), func(tx *ReadTx) (err error) {
					foreign, err = tx.ReadyHeadIsForeign(context.Background(), id)
					return err
				}); err != nil || foreign != want {
					t.Fatalf("ReadyHeadIsForeign(%s) = %t, %v; want %t", id, foreign, err, want)
				}
			}
			supersedeReadyItem(t, r.st, again.ID)
			external := externalReviewAuthority(r.run.ID, again.ID, prior, finding, admitting)
			err := r.record(t, external)
			if tc.foreign && !errors.Is(err, domain.ErrParentKeyMismatch) {
				t.Fatalf("external review on a foreign head: %v, want ErrParentKeyMismatch", err)
			}
			if !tc.foreign && err != nil {
				t.Fatalf("external review on Freeside's own head: %v", err)
			}
		})
	}
}
