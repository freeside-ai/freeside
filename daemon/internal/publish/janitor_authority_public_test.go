package publish_test

import (
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

func TestInstallationAuthorityAllowsRepositoryRevalidatesCredentialBinding(t *testing.T) {
	const (
		appID        = int64(12345)
		ownerID      = int64(67890)
		repositoryID = int64(1278475858)
	)
	app := publish.AppCredentials{
		Owner: "freeside-ai", OwnerID: ownerID,
		Visibility: publish.AppVisibilityPublic, AppID: appID,
	}
	authority := publish.InstallationAuthority{
		ActiveEpoch: 1, DurableIntentRevision: 1,
		TrustedOwners: []publish.TrustedOwner{{Login: "freeside-ai", ID: ownerID}},
		TrustedInstallations: []publish.TrustedInstallation{{
			RegistrationID: appID, InstallationID: 777,
			Account: "freeside-ai", AccountID: ownerID,
			RepositoryIDs: []int64{repositoryID},
		}},
	}

	allowed, err := publish.InstallationAuthorityAllowsRepository(
		app, authority, repositoryID, time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC),
	)
	if err != nil || !allowed {
		t.Fatalf("allowed = %t, error = %v, want true, nil", allowed, err)
	}
	allowed, err = publish.InstallationAuthorityAllowsRepository(
		app, authority, repositoryID+1, time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC),
	)
	if err != nil || allowed {
		t.Fatalf("unrelated allowed = %t, error = %v, want false, nil", allowed, err)
	}

	authority.TrustedInstallations[0].RegistrationID++
	if _, err := publish.InstallationAuthorityAllowsRepository(
		app, authority, repositoryID, time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC),
	); err == nil {
		t.Fatal("mismatched registration unexpectedly passed credential-bound validation")
	}
}
