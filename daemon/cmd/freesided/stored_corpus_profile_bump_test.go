package main

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// TestStoredCorpusResumesAfterProfileEncodingBump starts the scenario daemon
// on a copy of every publication-pending fixture whose admitted trust profile
// row no longer reads, as a profile encoding bump leaves every row recorded
// before it (#3171). The run ends in its own trust block with no durable
// stop, and the owner's re-approval and rerun carry it to a published pull
// request. The row is the repository's active profile too, as after a real
// bump.
func TestStoredCorpusResumesAfterProfileEncodingBump(t *testing.T) {
	t.Parallel()
	ran := 0
	for _, dir := range storedCorpusFixtureDirs(t) {
		fixture := readStoredCorpusFixture(t, dir)
		if fixture.Case != "publication-pending" {
			continue
		}
		ran++
		rel, err := filepath.Rel(storedCorpusDir, dir)
		if err != nil {
			t.Fatal(err)
		}
		t.Run(filepath.ToSlash(rel), func(t *testing.T) {
			t.Parallel()
			played := storedCorpusCases[fixture.Case].scenario()
			cfg := scenarioConfig(t, &played)
			copyStoredCorpusFixture(t, dir, cfg.DBPath)
			assertStoredCorpusCopy(t, cfg.DBPath, fixture)
			admitted := staleStoredCorpusAdmittedProfile(t, cfg.DBPath, fixture.RunID)

			h := startDaemon(t, cfg)
			sweepStoredHistory(t, h)
			resumeAfterProfileEncodingBump(t, h, fixture, changedTrustProfile(t, admitted))
			assertNoDurableStop(t, h)
			if err := h.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}
		})
	}
	if ran == 0 {
		t.Fatalf("stored-state corpus %s holds no publication-pending fixture", storedCorpusDir)
	}
}

// staleStoredCorpusAdmittedProfile rewrites the body of the one trust
// profile the fixture holds, which every admission of the run names, so the
// row no longer reconstructs under its digest. It returns the profile as it
// read before the change.
func staleStoredCorpusAdmittedProfile(t *testing.T, dbPath string, run domain.RunID) domain.AutomationTrustProfile {
	t.Helper()
	ctx := t.Context()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close() //nolint:errcheck // the update's own error is checked
	var (
		digest domain.Digest
		body   string
	)
	if err := db.QueryRowContext(ctx,
		`SELECT profile_digest, body FROM trust_profiles`).Scan(&digest, &body); err != nil {
		t.Fatalf("read the fixture's one trust profile: %v", err)
	}
	var profile domain.AutomationTrustProfile
	if err := json.Unmarshal([]byte(body), &profile); err != nil || profile.Validate() != nil ||
		profile.ProfileDigest != digest {
		t.Fatalf("fixture trust profile %s does not read: %v", digest, err)
	}
	rows, err := db.QueryContext(ctx, `SELECT body FROM execution_admissions WHERE run_id = ?`, run)
	if err != nil {
		t.Fatal(err)
	}
	admissions := 0
	for rows.Next() {
		var admission struct {
			TrustProfileDigest *domain.Digest `json:"trust_profile_digest"`
		}
		var raw string
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(raw), &admission); err != nil {
			t.Fatal(err)
		}
		if admission.TrustProfileDigest == nil || *admission.TrustProfileDigest != digest {
			t.Fatalf("run %s admission names profile %v, want %s", run, admission.TrustProfileDigest, digest)
		}
		admissions++
	}
	if err := rows.Err(); err != nil || admissions == 0 {
		t.Fatalf("run %s has %d admissions: %v", run, admissions, err)
	}
	staled := strings.Replace(body, `"allow_self_hosted_ci":false`, `"allow_self_hosted_ci":true`, 1)
	if staled == body {
		t.Fatal("profile body change did not apply")
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE trust_profiles SET body = ? WHERE profile_digest = ?`, staled, digest); err != nil {
		t.Fatal(err)
	}
	return profile
}

// changedTrustProfile is the profile the owner re-approves after the bump:
// the admitted one with one permission changed. It must differ in content,
// because this build would give the admitted content its old digest, which
// the unreadable row still holds; a real bump gives it a new digest.
func changedTrustProfile(t *testing.T, admitted domain.AutomationTrustProfile) domain.AutomationTrustProfile {
	t.Helper()
	changed, err := domain.NewAutomationTrustProfile(domain.AutomationTrustProfileInput{
		Repo: admitted.Repo, RepositoryID: admitted.RepositoryID,
		PRExecution:                admitted.PRExecution,
		CandidateAutomationChanges: admitted.CandidateAutomationChanges,
		PRGitHubTokenPermissions:   admitted.PRGitHubTokenPermissions,
		AllowOIDC:                  !admitted.AllowOIDC,
		AllowEnvironmentSecrets:    admitted.AllowEnvironmentSecrets,
		AllowSecretBearingPRJobs:   admitted.AllowSecretBearingPRJobs,
		AllowSelfHostedCI:          admitted.AllowSelfHostedCI,
		AllowPullRequestTarget:     admitted.AllowPullRequestTarget,
		AllowReusableWorkflows:     admitted.AllowReusableWorkflows,
		AllowPackagePublishing:     admitted.AllowPackagePublishing,
		AllowArtifactConsumers:     admitted.AllowArtifactConsumers,
		AllowIssueCommentWorkflows: admitted.AllowIssueCommentWorkflows,
		CommitPlan:                 admitted.CommitPlan,
		MessageRuleset:             admitted.MessageRuleset,
		WorkflowAuditDigest:        admitted.WorkflowAuditDigest,
		Review:                     admitted.Review,
		ProtectedPaths:             admitted.ProtectedPaths,
		IssueCommentWorkflows:      admitted.IssueCommentWorkflows,
		ExternalReviewers:          admitted.ExternalReviewers,
	})
	if err != nil {
		t.Fatal(err)
	}
	return changed
}

// resumeAfterProfileEncodingBump carries a publication-pending run whose
// admitted profile no longer reads to a published pull request: it waits for
// the run's trust block, records and activates the profile the owner
// re-approves, and decides rerun_trust_evaluation on the block. The plain
// publication-pending resume calls this once a real encoding bump lands.
func resumeAfterProfileEncodingBump(
	t *testing.T, h *daemon, fixture storedCorpusFixture, reapproved domain.AutomationTrustProfile,
) {
	t.Helper()
	blocked := waitForScenarioItem(t, h.Attention, domain.ProductionBlockedItemID(fixture.RunID))
	if blocked.Item.Reason != domain.PublicationBlockTrust ||
		!slices.Equal(blocked.Item.RequestedDecision, []domain.Action{
			domain.ActionRerunTrustEvaluation, domain.ActionInspectTrustFailure, domain.ActionStop,
		}) {
		t.Fatalf("run %s block = %#v, want the rerunnable trust block", fixture.RunID, blocked.Item)
	}
	if err := h.Store.WriteInternal(t.Context(), func(tx *store.InternalTx) error {
		return tx.RecordTrustProfile(t.Context(), reapproved, time.Now().UTC())
	}); err != nil {
		t.Fatalf("re-approve trust profile: %v", err)
	}
	if _, err := h.Attention.Submit(t.Context(), signet.ClientCommand{
		CommandID: "rerun-after-profile-bump", DeviceID: "device-scenario",
		ExpectedEntityVersion: blocked.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: blocked.Item.ID, ItemVersion: blocked.Item.ItemVersion,
			PRHeadSHA: blocked.Item.PRHeadSHA, ArtifactDigests: blocked.Item.ArtifactDigests,
			Action: domain.ActionRerunTrustEvaluation,
		},
	}); err != nil {
		t.Fatalf("rerun trust evaluation: %v", err)
	}
	assertScenarioRepublished(t, h.Attention, fixture.TaskID, fixture.RunID)
}

// assertScenarioRepublished waits for the run's pull request after a rerun,
// then checks that the run's latest terminal completed and its latest
// verification verdict is clean. assertScenarioPublished can't wait here: the
// block already recorded a completed terminal with no pull request.
func assertScenarioRepublished(t *testing.T, attention *signet.Service, task domain.TaskID, run domain.RunID) {
	t.Helper()
	deadline := time.Now().Add(scenarioWaitBound)
	for {
		timeline, err := attention.GetTaskTimeline(t.Context(), task)
		if err != nil {
			t.Fatalf("task timeline: %v", err)
		}
		var terminal *domain.ObservedInvocationStatus
		var pr *int
		var verdict domain.ReadinessVerdictClass
		for _, event := range timeline.Events {
			if event.RunID == nil || *event.RunID != run {
				continue
			}
			switch {
			case event.Milestone != nil && event.Milestone.Kind == domain.MilestoneTerminalRecorded:
				terminal = event.Milestone.Terminal
			case event.Kind == domain.TaskEventPROpened:
				pr = event.PRNumber
			case event.Kind == domain.TaskEventVerificationRecorded && event.Verification != nil:
				verdict = event.Verification.Class
			}
		}
		if pr != nil && terminal != nil && verdict != "" {
			if *terminal != domain.ObservedStatusCompleted || verdict != domain.ReadinessReadyClean {
				t.Fatalf("run %s: terminal %q, verdict %q; want completed, ready_clean", run, *terminal, verdict)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s opened no pull request within %s; events: %+v", run, scenarioWaitBound, timeline.Events)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
