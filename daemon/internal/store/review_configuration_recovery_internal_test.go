package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
	"github.com/freeside-ai/freeside/daemon/migrations"
)

func TestReviewConfigurationRecoveryMigrationAppliesFromHead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openRaw(t)
	migrateThrough(t, ctx, db, "0034_")
	if got := rawVersion(t, db); got != 33 {
		t.Fatalf("prior schema version = %d, want 33", got)
	}
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate to head: %v", err)
	}
	if got := rawVersion(t, db); got != 51 {
		t.Fatalf("schema version = %d, want 51", got)
	}
	var count int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM review_configuration_recovery_transitions`).Scan(&count); err != nil {
		t.Fatalf("count configuration recovery transitions: %v", err)
	}
	if count != 0 {
		t.Fatalf("new configuration recovery log contains %d rows, want 0", count)
	}
}

func configRecoveryProfile(t *testing.T, configDigest domain.Digest, widen bool) domain.AutomationTrustProfile {
	t.Helper()
	in := domain.AutomationTrustProfileInput{
		Repo:                       "acme/widgets",
		RepositoryID:               7,
		PRExecution:                domain.PRExecutionAuditedSameRepo,
		CandidateAutomationChanges: domain.AutomationChangesBlocked,
		PRGitHubTokenPermissions:   domain.TokenPermissionsReadOnly,
		CommitPlan:                 domain.CommitPlanSingleCommit,
		MessageRuleset:             domain.MessageRulesetGitHub1,
		WorkflowAuditDigest:        "sha256:workflow-audit",
		Review: domain.ReviewSettings{
			Mode: domain.ReviewFreesideInvoked, ConfigDigest: configDigest,
		},
	}
	if widen {
		in.PRGitHubTokenPermissions = domain.TokenPermissionsReadWrite
	}
	profile, err := domain.NewAutomationTrustProfile(in)
	if err != nil {
		t.Fatal(err)
	}
	return profile
}

func seedReviewConfigurationRecovery(t *testing.T) (*Store, domain.ReviewConfigurationRecoveryTransition) {
	return seedReviewConfigurationRecoveryWithOptions(t, Options{})
}

func seedReviewConfigurationRecoveryWithOptions(
	t *testing.T, opts Options,
) (*Store, domain.ReviewConfigurationRecoveryTransition) {
	t.Helper()
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "store.db"), opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	at := time.Date(2026, 8, 8, 1, 2, 3, 0, time.UTC)
	run := domain.Run{
		ID: "run-config-recovery", ProjectID: "project-1",
		SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
	}
	superseded := configRecoveryProfile(t, "sha256:config-old", false)
	superseding := configRecoveryProfile(t, "sha256:config-new", false)
	failure := domain.ReviewFailure{
		RunID: run.ID, InvocationID: "review-config-recovery-2", Round: 2,
		BaseSHA: "base", HeadSHA: "head", Class: domain.ReviewFailureConfiguration,
		Reason: "trust profile no longer approves the reviewer configuration", ObservedAt: at,
	}
	failureBody, err := encode(failure)
	if err != nil {
		t.Fatalf("encode failure: %v", err)
	}
	digest := domain.Digest(reviewBodyDigest(failureBody))
	binding := domain.ReviewConfigurationRecoveryBinding{
		RunID: failure.RunID, InvocationID: failure.InvocationID, Round: failure.Round,
		BaseSHA: failure.BaseSHA, HeadSHA: failure.HeadSHA, FailureDigest: digest,
		Repo: superseded.Repo, RepositoryID: superseded.RepositoryID,
		SupersededProfileDigest: superseded.ProfileDigest,
	}
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "review-config-recovery-item", ProjectID: run.ProjectID,
		Subject: domain.Subject{
			Type: domain.SubjectRun, ID: domain.SubjectID(run.ID), RunID: &run.ID,
		},
		Type: domain.AttentionReviewConfiguration, Priority: domain.PriorityHigh,
		Reason:            "review parked on an unapproved configuration",
		RequestedDecision: []domain.Action{domain.ActionAdoptReviewConfiguration, domain.ActionDiscuss, domain.ActionStop},
		PRHeadSHA:         failure.HeadSHA, ReviewConfigurationRecovery: &binding,
		ItemVersion: 1, InterruptionClass: domain.InterruptionPlannedGate,
		Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatalf("NewAttentionItem: %v", err)
	}
	commandID := "command-adopt-review-configuration"
	command, err := domain.NewCommand(domain.CommandInput{
		CommandID: commandID, DeviceID: "device-1", ItemID: item.ID,
		ItemVersion: item.ItemVersion, PRHeadSHA: item.PRHeadSHA,
		ArtifactDigests: item.ArtifactDigests, Action: domain.ActionAdoptReviewConfiguration,
	})
	if err != nil {
		t.Fatalf("NewCommand: %v", err)
	}
	transition := domain.ReviewConfigurationRecoveryTransition{
		RunID: binding.RunID, InvocationID: binding.InvocationID, Round: binding.Round,
		BaseSHA: binding.BaseSHA, HeadSHA: binding.HeadSHA, FailureDigest: binding.FailureDigest,
		Repo: binding.Repo, RepositoryID: binding.RepositoryID,
		SupersededProfileDigest:  superseded.ProfileDigest,
		SupersedingProfileDigest: superseding.ProfileDigest,
		CommandID:                &commandID,
		Reason:                   "operator adopted the superseding review configuration",
		OccurredAt:               at.Add(time.Minute),
	}
	if err := st.Write(ctx, func(tx *WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		if err := tx.PutReviewFailure(ctx, failure); err != nil {
			return err
		}
		if err := tx.PutAttentionItem(ctx, item); err != nil {
			return err
		}
		return tx.PutCommand(ctx, command)
	}); err != nil {
		t.Fatalf("seed configuration recovery: %v", err)
	}
	if err := st.WriteInternal(ctx, func(tx *InternalTx) error {
		if err := tx.RecordTrustProfile(ctx, superseded, at.Add(-time.Hour)); err != nil {
			return err
		}
		if err := tx.RecordTrustProfile(ctx, superseding, at); err != nil {
			return err
		}
		return tx.RecordReviewConfigurationRecoveryTransition(ctx, transition)
	}); err != nil {
		t.Fatalf("seed profiles and transition: %v", err)
	}
	return st, transition
}

func TestReviewConfigurationRecoveryTransitionRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, want := seedReviewConfigurationRecovery(t)
	var got domain.ReviewConfigurationRecoveryTransition
	var found bool
	if err := st.Read(ctx, func(tx *ReadTx) error {
		var err error
		got, found, err = tx.LatestReviewConfigurationRecoveryTransition(ctx, want.RunID)
		return err
	}); err != nil {
		t.Fatalf("LatestReviewConfigurationRecoveryTransition: %v", err)
	}
	if !found {
		t.Fatal("configuration recovery transition not found")
	}
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("round trip = %s, want %s", gotJSON, wantJSON)
	}
	pretty, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden.Assert(t, "review_configuration_recovery_transition", append(pretty, '\n'))

	var absent bool
	if err := st.Read(ctx, func(tx *ReadTx) error {
		_, found, err := tx.LatestReviewConfigurationRecoveryTransition(ctx, "run-other")
		absent = !found
		return err
	}); err != nil || !absent {
		t.Fatalf("unrelated run lookup = absent %v, err %v", absent, err)
	}
}

func TestReviewConfigurationRecoveryWriteRejectsEveryMismatchedBinding(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, valid := seedReviewConfigurationRecovery(t)
	for name, mutate := range map[string]func(*domain.ReviewConfigurationRecoveryTransition){
		"run":        func(tr *domain.ReviewConfigurationRecoveryTransition) { tr.RunID = "run-other" },
		"invocation": func(tr *domain.ReviewConfigurationRecoveryTransition) { tr.InvocationID = "review-other" },
		"round":      func(tr *domain.ReviewConfigurationRecoveryTransition) { tr.Round++ },
		"base":       func(tr *domain.ReviewConfigurationRecoveryTransition) { tr.BaseSHA = "other-base" },
		"head":       func(tr *domain.ReviewConfigurationRecoveryTransition) { tr.HeadSHA = "other-head" },
		"digest":     func(tr *domain.ReviewConfigurationRecoveryTransition) { tr.FailureDigest = "sha256:other" },
		"repo":       func(tr *domain.ReviewConfigurationRecoveryTransition) { tr.Repo = "acme/other" },
		"superseded profile": func(tr *domain.ReviewConfigurationRecoveryTransition) {
			tr.SupersededProfileDigest = "sha256:profile-other"
		},
		"superseding profile": func(tr *domain.ReviewConfigurationRecoveryTransition) {
			tr.SupersedingProfileDigest = "sha256:profile-other"
		},
	} {
		t.Run(name, func(t *testing.T) {
			transition := valid
			mutate(&transition)
			err := st.WriteInternal(ctx, func(tx *InternalTx) error {
				return tx.RecordReviewConfigurationRecoveryTransition(ctx, transition)
			})
			if err == nil {
				t.Fatal("mismatched write accepted")
			}
		})
	}
}

func TestReviewConfigurationRecoveryReadFailsClosedOnTamper(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cases := map[string]struct {
		tamper func(*Store, domain.ReviewConfigurationRecoveryTransition) error
		want   error
	}{
		"unbacked": {
			tamper: func(st *Store, _ domain.ReviewConfigurationRecoveryTransition) error {
				_, err := st.db.ExecContext(ctx,
					`UPDATE review_configuration_recovery_transitions SET command_id = NULL`)
				return err
			},
			want: domain.ErrTransitionUnbacked,
		},
		"binding": {
			tamper: func(st *Store, _ domain.ReviewConfigurationRecoveryTransition) error {
				_, err := st.db.ExecContext(ctx,
					`UPDATE review_configuration_recovery_transitions SET head_sha = 'forged-head'`)
				return err
			},
			want: domain.ErrReviewConfigRecoveryBindingMismatch,
		},
		"failure body bytes": {
			tamper: func(st *Store, _ domain.ReviewConfigurationRecoveryTransition) error {
				var invocationID, body string
				if err := st.db.QueryRowContext(ctx,
					`SELECT invocation_id, body FROM review_failures`).Scan(&invocationID, &body); err != nil {
					return err
				}
				body += "\n"
				_, err := st.db.ExecContext(ctx,
					`UPDATE review_failures SET body = ?, body_digest = ? WHERE invocation_id = ?`,
					body, reviewBodyDigest(body), invocationID)
				return err
			},
			want: domain.ErrReviewConfigRecoveryBindingMismatch,
		},
		"command action": {
			// The transition repointed at a real accepted command whose action
			// is not the authorizing adoption: the snapshot reads cleanly, so
			// the rejection is the action re-gate, not row consistency.
			tamper: func(st *Store, tr domain.ReviewConfigurationRecoveryTransition) error {
				stop, err := domain.NewCommand(domain.CommandInput{
					CommandID: "command-stop", DeviceID: "device-1",
					ItemID: "review-config-recovery-item", ItemVersion: 1,
					PRHeadSHA: tr.HeadSHA, Action: domain.ActionStop,
				})
				if err != nil {
					return err
				}
				if err := st.Write(ctx, func(tx *WriteTx) error {
					return tx.PutCommand(ctx, stop)
				}); err != nil {
					return err
				}
				_, err = st.db.ExecContext(ctx,
					`UPDATE review_configuration_recovery_transitions SET command_id = 'command-stop'`)
				return err
			},
			want: domain.ErrTransitionCommandMismatch,
		},
		"command row forged": {
			tamper: func(st *Store, _ domain.ReviewConfigurationRecoveryTransition) error {
				_, err := st.db.ExecContext(ctx, `UPDATE commands SET action = 'stop'`)
				return err
			},
			want: errRowInconsistent,
		},
		"command body malformed": {
			// A malformed (not merely mismatched) referent body: reconstruction
			// fails structural validation, which must classify as ineffective
			// like every other determinate rejection.
			tamper: func(st *Store, _ domain.ReviewConfigurationRecoveryTransition) error {
				_, err := st.db.ExecContext(ctx,
					`UPDATE commands SET body = json_set(body, '$.command.device_id', '')`)
				return err
			},
			want: domain.ErrEmptyID,
		},
		"superseding profile body": {
			// A self-consistent forgery: the superseded revision's body carrying
			// the superseding row's digest field decodes cleanly, so the
			// rejection is the recomputed content address.
			tamper: func(st *Store, tr domain.ReviewConfigurationRecoveryTransition) error {
				_, err := st.db.ExecContext(ctx,
					`UPDATE trust_profiles
					 SET body = (SELECT json_set(body, '$.profile_digest', ?)
					             FROM trust_profiles WHERE profile_digest = ?)
					 WHERE profile_digest = ?`,
					tr.SupersedingProfileDigest, tr.SupersededProfileDigest,
					tr.SupersedingProfileDigest)
				return err
			},
			want: domain.ErrProfileDigestMismatch,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			st, transition := seedReviewConfigurationRecovery(t)
			if err := tc.tamper(st, transition); err != nil {
				t.Fatalf("tamper: %v", err)
			}
			err := st.Read(ctx, func(tx *ReadTx) error {
				_, _, err := tx.LatestReviewConfigurationRecoveryTransition(ctx, transition.RunID)
				return err
			})
			if !errors.Is(err, tc.want) {
				t.Fatalf("tampered read = %v, want %v", err, tc.want)
			}
			// Every determinate rejection additionally carries the single
			// classification sentinel consumers park on instead of
			// error-looping.
			if !errors.Is(err, domain.ErrReviewConfigRecoveryIneffective) {
				t.Fatalf("tampered read = %v, want %v", err, domain.ErrReviewConfigRecoveryIneffective)
			}
		})
	}
}

func TestAdmissionGateRejectsAnIneffectiveReviewConfigurationRecovery(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	capabilities, ok := domain.ProvableCapabilities(domain.BackendFreshVMReadOnlyVolumeHandoff)
	if !ok {
		t.Fatal("fresh-vm backend has no provable capabilities")
	}
	healthy := domain.BackupHealth{
		Encryption: domain.BackupHealthHealthy, CheckpointCurrency: domain.BackupHealthHealthy,
		ArtifactClosure: domain.BackupHealthHealthy, RestoreTestAge: domain.BackupHealthHealthy,
	}
	st, transition := seedReviewConfigurationRecoveryWithOptions(t, Options{
		AdmissionFloors: map[domain.OperatingMode]domain.CapabilitySnapshot{
			domain.ModeUnattended: domain.NewCapabilitySnapshot(domain.CapPostExitExport),
		},
		ApprovedCredentialModes: []domain.CredentialMode{domain.CredentialSubscriptionContained},
		BackupHealthSource: BackupHealthSourceFunc(func(
			context.Context, BackupHealthContext,
		) (domain.BackupHealth, error) {
			return healthy, nil
		}),
	})
	identity := domain.AuthIdentity{
		ID: "auth-config-recovery", Provider: "claude", AuthStoreMutationLease: true,
		AuthStoreVolume: "provider-cred", MaxParallelExecutions: 1,
		RefreshStrategy: domain.RefreshOnDemand,
	}
	if err := st.Write(ctx, func(tx *WriteTx) error {
		return tx.RecordAuthIdentity(ctx, identity, transition.OccurredAt)
	}); err != nil {
		t.Fatalf("seed auth identity: %v", err)
	}
	identityID := identity.ID
	pinned := transition.SupersededProfileDigest
	admission, err := domain.NewExecutionAdmission(domain.ExecutionAdmissionInput{
		InvocationID: "inv-implement-config-recovery", RunID: transition.RunID,
		StageID: "stage-implement", AttemptID: "attempt-implement",
		Backend: string(domain.BackendFreshVMReadOnlyVolumeHandoff), Capabilities: capabilities,
		BackendConfigurationDigest: domain.Digest("sha256:" + strings.Repeat("1", 64)),
		OperatingMode:              domain.ModeUnattended,
		CredentialMode:             domain.CredentialSubscriptionContained,
		EgressProfile:              domain.EgressProviderOnly,
		ImageRef: domain.ImageRef(
			"ghcr.io/freeside-ai/agent@sha256:" + strings.Repeat("ab", 32),
		),
		SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy", InputDigest: "sha256:input",
		Base: domain.BaseRevision{
			Repo: transition.Repo, RepositoryID: transition.RepositoryID,
			BaseRef: "refs/heads/main", BaseSHA: "deadbeef",
		},
		Workspace: "workspace-1", AuthIdentityID: &identityID,
		TrustProfileDigest: &pinned, AdmittedAt: transition.OccurredAt,
	})
	if err != nil {
		t.Fatalf("NewExecutionAdmission: %v", err)
	}
	if err := st.Read(ctx, func(tx *ReadTx) error {
		return tx.gateReconstructedAdmission(ctx, admission)
	}); err != nil {
		t.Fatalf("effective recovery did not authorize admission: %v", err)
	}
	err = st.Write(ctx, func(tx *WriteTx) error {
		return tx.RecordExecutionAdmission(ctx, admission)
	})
	if !errors.Is(err, domain.ErrTrustProfileSuperseded) {
		t.Fatalf("fresh admission under adopted recovery = %v, want %v",
			err, domain.ErrTrustProfileSuperseded)
	}

	if _, err := st.db.ExecContext(ctx,
		`UPDATE review_configuration_recovery_transitions SET command_id = NULL`); err != nil {
		t.Fatalf("tamper transition authority: %v", err)
	}
	err = st.Read(ctx, func(tx *ReadTx) error {
		return tx.gateReconstructedAdmission(ctx, admission)
	})
	if !errors.Is(err, domain.ErrTrustProfileSuperseded) {
		t.Fatalf("admission under tampered recovery = %v, want %v",
			err, domain.ErrTrustProfileSuperseded)
	}
	if errors.Is(err, domain.ErrReviewConfigRecoveryIneffective) {
		t.Fatalf("admission leaked ineffective-transition detail instead of supersession: %v", err)
	}
}

func TestReviewConfigurationRecoveryRequiresConfigurationClass(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, transition := seedReviewConfigurationRecovery(t)
	var rewrittenBody string
	if err := st.db.QueryRowContext(ctx,
		`SELECT json_set(body, '$.class', 'contradiction') FROM review_failures WHERE invocation_id = ?`,
		transition.InvocationID).Scan(&rewrittenBody); err != nil {
		t.Fatalf("read rewritten failure: %v", err)
	}
	if _, err := st.db.ExecContext(ctx,
		`UPDATE review_failures SET failure_class = 'contradiction',
		 body = json_set(body, '$.class', 'contradiction'), body_digest = ?
		 WHERE invocation_id = ?`,
		reviewBodyDigest(rewrittenBody), transition.InvocationID); err != nil {
		t.Fatalf("rewrite failure: %v", err)
	}
	err := st.Read(ctx, func(tx *ReadTx) error {
		_, _, err := tx.LatestReviewConfigurationRecoveryTransition(ctx, transition.RunID)
		return err
	})
	if !errors.Is(err, domain.ErrReviewConfigRecoveryBindingMismatch) ||
		!errors.Is(err, domain.ErrReviewConfigRecoveryIneffective) {
		t.Fatalf("non-configuration recovery = %v, want %v and %v",
			err, domain.ErrReviewConfigRecoveryBindingMismatch,
			domain.ErrReviewConfigRecoveryIneffective)
	}
}

// TestReviewConfigurationRecoveryReadFailsClosedWhenProfileMovesOn pins the
// re-derived latest check: an accepted adoption stops authorizing the resume
// the moment a newer profile revision is activated, so a stale decision can
// never carry a run onto a revision the operator has since superseded.
func TestReviewConfigurationRecoveryReadFailsClosedWhenProfileMovesOn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, transition := seedReviewConfigurationRecovery(t)
	newer := configRecoveryProfile(t, "sha256:config-newer", false)
	if err := st.WriteInternal(ctx, func(tx *InternalTx) error {
		return tx.RecordTrustProfile(ctx, newer, transition.OccurredAt.Add(time.Hour))
	}); err != nil {
		t.Fatalf("record newer profile: %v", err)
	}
	err := st.Read(ctx, func(tx *ReadTx) error {
		_, _, err := tx.LatestReviewConfigurationRecoveryTransition(ctx, transition.RunID)
		return err
	})
	if !errors.Is(err, domain.ErrReviewConfigRecoveryBindingMismatch) ||
		!errors.Is(err, domain.ErrReviewConfigRecoveryIneffective) {
		t.Fatalf("moved-on profile read = %v, want %v and %v",
			err, domain.ErrReviewConfigRecoveryBindingMismatch,
			domain.ErrReviewConfigRecoveryIneffective)
	}
}

// TestReviewConfigurationRecoveryWriteRejectsTrustWideningSupersession pins
// the narrow-delta gate at the write boundary: a superseding revision that
// changes anything beyond the review configuration digest cannot ride a
// review recovery, however the operator command was carried.
func TestReviewConfigurationRecoveryWriteRejectsTrustWideningSupersession(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, transition := seedReviewConfigurationRecovery(t)
	widened := configRecoveryProfile(t, "sha256:config-widened", true)
	if err := st.WriteInternal(ctx, func(tx *InternalTx) error {
		return tx.RecordTrustProfile(ctx, widened, transition.OccurredAt.Add(time.Hour))
	}); err != nil {
		t.Fatalf("record widened profile: %v", err)
	}
	forged := transition
	forged.SupersedingProfileDigest = widened.ProfileDigest
	// A fresh run/invocation so the unique row constraint is not what rejects.
	err := st.WriteInternal(ctx, func(tx *InternalTx) error {
		return tx.RecordReviewConfigurationRecoveryTransition(ctx, forged)
	})
	if !errors.Is(err, domain.ErrReviewConfigSupersessionInvalid) {
		t.Fatalf("trust-widening write = %v, want %v",
			err, domain.ErrReviewConfigSupersessionInvalid)
	}
}
