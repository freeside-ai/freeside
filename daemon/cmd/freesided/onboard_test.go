package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

type installationAuthorityFixtures map[int64]publish.InstallationAuthority

func (f installationAuthorityFixtures) InstallationAuthority(
	_ context.Context, registrationID int64,
) (publish.InstallationAuthority, error) {
	authority, ok := f[registrationID]
	if !ok {
		return publish.InstallationAuthority{}, errors.New("missing registration fixture")
	}
	return authority, nil
}

func TestTrustedInstallationChecksEveryLocalRegistration(t *testing.T) {
	const repositoryID int64 = 44
	trusted := func(registrationID, installationID int64) publish.InstallationAuthority {
		return publish.InstallationAuthority{
			TrustedInstallations: []publish.TrustedInstallation{{
				RegistrationID: registrationID, InstallationID: installationID,
				Account: "example", AccountID: 33,
				RepositoryIDs: []int64{repositoryID},
			}},
		}
	}
	for _, tc := range []struct {
		name        string
		authorities installationAuthorityFixtures
		selected    int64
		want        *publish.TrustedInstallation
		wantErr     error
	}{
		{
			name: "selected registration owns repository",
			authorities: installationAuthorityFixtures{
				11: trusted(11, 21),
				12: {TrustedInstallations: []publish.TrustedInstallation{}},
			},
			selected: 11,
			want: &publish.TrustedInstallation{
				RegistrationID: 11, InstallationID: 21,
				Account: "example", AccountID: 33,
				RepositoryIDs: []int64{repositoryID},
			},
		},
		{
			name: "different registration already owns repository",
			authorities: installationAuthorityFixtures{
				11: {TrustedInstallations: []publish.TrustedInstallation{}},
				12: trusted(12, 22),
			},
			selected: 11, wantErr: publish.ErrAmbiguousInstallation,
		},
		{
			name: "multiple registrations already own repository",
			authorities: installationAuthorityFixtures{
				11: trusted(11, 21),
				12: trusted(12, 22),
			},
			selected: 11, wantErr: publish.ErrAmbiguousInstallation,
		},
		{
			name: "repository is not yet trusted",
			authorities: installationAuthorityFixtures{
				11: {TrustedInstallations: []publish.TrustedInstallation{}},
				12: {TrustedInstallations: []publish.TrustedInstallation{}},
			},
			selected: 11,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := trustedInstallation(
				t.Context(), tc.authorities, []int64{11, 12},
				tc.selected, repositoryID,
			)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("trustedInstallation error = %v, want %v", err, tc.wantErr)
			}
			if tc.want == nil {
				if got != nil {
					t.Fatalf("trustedInstallation = %+v, want nil", got)
				}
				return
			}
			if got == nil || got.RegistrationID != tc.want.RegistrationID ||
				got.InstallationID != tc.want.InstallationID {
				t.Fatalf("trustedInstallation = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestAttendedImportUsesCurrentActiveTrustProfile(t *testing.T) {
	ctx := t.Context()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	profile, err := domain.NewAutomationTrustProfile(domain.AutomationTrustProfileInput{
		Repo: "example/repo", RepositoryID: 44,
		PRExecution:                domain.PRExecutionAuditedSameRepo,
		CandidateAutomationChanges: domain.AutomationChangesBlocked,
		PRGitHubTokenPermissions:   domain.TokenPermissionsReadOnly,
		CommitPlan:                 domain.CommitPlanSingleCommit,
		MessageRuleset:             domain.MessageRulesetGitHub1,
		WorkflowAuditDigest:        "sha256:workflow-audit",
		Review: domain.ReviewSettings{
			Mode: domain.ReviewFreesideInvoked, ConfigDigest: "sha256:review",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 31, 6, 15, 0, 0, time.UTC)
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		if err := tx.RecordInactiveTrustProfile(ctx, profile, now); err != nil {
			return err
		}
		return tx.ActivateTrustProfile(ctx, profile.Repo, profile.ProfileDigest, now)
	}); err != nil {
		t.Fatal(err)
	}
	authority := storeAdmissionAuthority{store: st}
	admission := domain.ExecutionAdmission{
		ID: "sha256:attended-admission", OperatingMode: domain.ModeAttendedDev,
		Base: domain.BaseRevision{Repo: profile.Repo, RepositoryID: profile.RepositoryID},
	}
	got, err := authority.importTrustProfile(ctx, admission)
	if err != nil {
		t.Fatalf("attended import profile: %v", err)
	}
	if got.ProfileDigest != profile.ProfileDigest {
		t.Fatalf("attended import profile = %s, want %s", got.ProfileDigest, profile.ProfileDigest)
	}
	admission.OperatingMode = domain.ModeUnattended
	digest := profile.ProfileDigest
	admission.TrustProfileDigest = &digest
	got, err = authority.importTrustProfile(ctx, admission)
	if err != nil {
		t.Fatalf("bound unattended import profile: %v", err)
	}
	if got.ProfileDigest != profile.ProfileDigest {
		t.Fatalf("unattended import profile = %s, want %s", got.ProfileDigest, profile.ProfileDigest)
	}
	admission.TrustProfileDigest = nil
	if _, err := authority.importTrustProfile(ctx, admission); !errors.Is(err, domain.ErrEmptyField) {
		t.Fatalf("unbound unattended import profile = %v, want ErrEmptyField", err)
	}
}
