package main

import (
	"context"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/daemoncompose"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

const (
	externalTestMaintainerID    = 4101
	externalTestMaintainerLogin = "maintainer"
)

func externalTestMaintainer() domain.ExternalReviewer {
	return domain.ExternalReviewer{
		Forge: domain.ExternalReviewForgeGitHub, AccountID: externalTestMaintainerID,
		Login: externalTestMaintainerLogin, Authority: domain.ExternalReviewDriveRound,
	}
}

// activateExternalTestProfile records and activates a trust profile for the
// bound repository whose allowlist is reviewers. The digest differs by
// allowlist, so a test can activate one profile after another.
func activateExternalTestProfile(
	t *testing.T, st *store.Store, at time.Time, reviewers ...domain.ExternalReviewer,
) domain.AutomationTrustProfile {
	t.Helper()
	profile, err := domain.NewAutomationTrustProfile(domain.AutomationTrustProfileInput{
		Repo: "owner/repo", RepositoryID: 424242,
		PRExecution:                domain.PRExecutionAuditedSameRepo,
		CandidateAutomationChanges: domain.AutomationChangesBlocked,
		PRGitHubTokenPermissions:   domain.TokenPermissionsReadOnly,
		CommitPlan:                 domain.CommitPlanSingleCommit,
		MessageRuleset:             domain.MessageRulesetGitHub1,
		WorkflowAuditDigest:        "sha256:workflow-audit",
		Review:                     domain.ReviewSettings{Mode: domain.ReviewFreesideInvoked, ConfigDigest: "sha256:review-config"},
		ExternalReviewers:          reviewers,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		if err := tx.RecordInactiveTrustProfile(ctx, profile, at); err != nil {
			return err
		}
		return tx.ActivateTrustProfile(ctx, "owner/repo", profile.ProfileDigest, at)
	}); err != nil {
		t.Fatal(err)
	}
	return profile
}

// externalReentryActivity is two inline comments by the maintainer on the
// published head, the second a minute after the first, and one by a bot no
// test lists.
func externalReentryActivity() publish.PullReviewObservation {
	comment := func(id, author int64, login, body string, at time.Time) publish.PullReviewComment {
		return publish.PullReviewComment{
			ID: id, AuthorID: author, AuthorLogin: login,
			Path: "daemon/main.go", Line: 42, OriginalLine: 42, Body: body,
			CommitID: reentryTestHead, OriginalCommitID: reentryTestHead, CreatedAt: at,
		}
	}
	return publish.PullReviewObservation{Comments: []publish.PullReviewComment{
		comment(800401, externalTestMaintainerID, externalTestMaintainerLogin,
			"second: also this", activeResourceTestTime.Add(time.Minute)),
		comment(800400, externalTestMaintainerID, externalTestMaintainerLogin,
			"first: this leaks the handle", activeResourceTestTime),
		comment(800402, 4102, "review-bot[bot]", "nit: rename", activeResourceTestTime.Add(-time.Minute)),
	}}
}

// externalReentryReconciler reconciles the open pull request reentryReadyItem
// binds, at its bound head, with review observing its activity.
func externalReentryReconciler(st *store.Store, review daemoncompose.NativeReviewObserver) daemoncompose.ActiveResourceReconciler {
	return daemoncompose.ActiveResourceReconciler{
		Store: st,
		Pull: func(context.Context, string, int) (publish.PullObservation, error) {
			observed := exactPull("open", false)
			observed.HeadSHA = reentryTestHead
			return observed, nil
		},
		Review:    review,
		Reviewers: testReviewers(),
		Now:       func() time.Time { return activeResourceTestTime },
	}
}

func reconcileClean(t *testing.T, reconciler *daemoncompose.ActiveResourceReconciler, step string) {
	t.Helper()
	result, err := reconciler.Reconcile(context.Background())
	if err != nil || len(result.Failures) != 0 || len(result.Skipped) != 0 {
		t.Fatalf("%s: Reconcile = %+v, %v", step, result, err)
	}
}
