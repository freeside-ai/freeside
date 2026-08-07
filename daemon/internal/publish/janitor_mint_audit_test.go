package publish_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

// grantReadExpiry is the expiry the grant-read mint body declares, one hour
// after fixedNow, so it passes the janitor's expiry check.
var grantReadExpiry = time.Date(2026, 7, 16, 13, 0, 0, 0, time.UTC)

func newMintAuditJanitor(
	t *testing.T,
	srv *httptest.Server,
	mintRecorder publish.InstallationMintRecorder,
) *publish.InstallationJanitor {
	t.Helper()
	janitor, err := publish.NewInstallationJanitor(
		publicJanitorKeystore(t),
		srv.Client(),
		srv.URL,
		trustedPublicBinding(fixtureRepositoryID),
		&removalRecorder{},
		mintRecorder,
		fixedNow,
		1,
	)
	if err != nil {
		t.Fatalf("NewInstallationJanitor: %v", err)
	}
	return janitor
}

// TestInstallationJanitorAuditsEveryMint: a bounded pass over one trusted
// installation whose remote grant matches exactly mints one grant-read token
// and records exactly one installation-mint audit row, carrying the safe
// coordinates and the validated metadata-only scope as both requested and
// granted (issue #545 acceptance 1).
func TestInstallationJanitorAuditsEveryMint(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if handleExactGrant(w, r, fixtureRepositoryID) {
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/app/installations":
			_, _ = io.WriteString(w,
				`[{"id":701,"app_id":501,"target_id":101,"repository_selection":"selected","account":{"login":"operator","id":101}}]`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	mintRec := &captureMintRecorder{}
	janitor := newMintAuditJanitor(t, srv, mintRec)
	if _, err := janitor.RunCycle(context.Background()); err != nil {
		t.Fatalf("clean pass: %v", err)
	}

	records := mintRec.snapshot()
	if len(records) != 1 {
		t.Fatalf("recorded %d installation mints, want exactly 1", len(records))
	}
	got := records[0]
	metadataRead := publish.Permissions{Metadata: "read"}
	if got.Outcome != publish.InstallationMintValidated {
		t.Errorf("outcome = %q, want %q", got.Outcome, publish.InstallationMintValidated)
	}
	if got.RegistrationID != 501 || got.InstallationID != 701 {
		t.Errorf("mint coordinates = reg %d inst %d, want reg 501 inst 701",
			got.RegistrationID, got.InstallationID)
	}
	if got.Requested != metadataRead || got.Granted != metadataRead {
		t.Errorf("mint scopes = requested %+v granted %+v, want metadata:read for both",
			got.Requested, got.Granted)
	}
	if !got.MintedAt.Equal(fixtureTime) {
		t.Errorf("minted_at = %v, want %v", got.MintedAt, fixtureTime)
	}
	if got.ExpiresAt == nil || !got.ExpiresAt.Equal(grantReadExpiry) {
		t.Errorf("expires_at = %v, want %v", got.ExpiresAt, grantReadExpiry)
	}
}

// TestInstallationJanitorAuditsMintBeforeFailedRevoke: when revocation fails,
// the daemon has proven it holds a live credential it cannot take back. The
// audit row is written at mint time, before the token is used, so it survives
// the later revoke failure and identifies the credential (issue #545
// acceptance 2). This is the exact gap the determination named
// (janitor.go's unrevoked-token path).
func TestInstallationJanitorAuditsMintBeforeFailedRevoke(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/app/installations":
			_, _ = io.WriteString(w,
				`[{"id":701,"app_id":501,"target_id":101,"repository_selection":"selected","account":{"login":"operator","id":101}}]`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/access_tokens"):
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, grantReadMintBody)
		case r.Method == http.MethodGet && r.URL.Path == "/installation/repositories":
			_, _ = io.WriteString(w, `{"total_count":1,"repositories":[{"id":990011}]}`)
		case r.Method == http.MethodDelete && r.URL.Path == "/installation/token":
			// The daemon minted a token it now cannot revoke.
			w.WriteHeader(http.StatusInternalServerError)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	mintRec := &captureMintRecorder{}
	janitor := newMintAuditJanitor(t, srv, mintRec)
	_, err := janitor.RunCycle(context.Background())
	if err == nil {
		t.Fatal("RunCycle accepted a revoke failure")
	}
	if strings.Contains(err.Error(), fixtureTokenValue) {
		t.Errorf("error leaked the enumeration token: %v", err)
	}

	records := mintRec.snapshot()
	if len(records) != 1 {
		t.Fatalf("recorded %d installation mints after failed revoke, want exactly 1", len(records))
	}
	if records[0].Outcome != publish.InstallationMintValidated {
		t.Errorf("failed-revoke audit outcome = %q, want %q", records[0].Outcome, publish.InstallationMintValidated)
	}
	if records[0].RegistrationID != 501 || records[0].InstallationID != 701 {
		t.Errorf("failed-revoke audit = reg %d inst %d, want reg 501 inst 701",
			records[0].RegistrationID, records[0].InstallationID)
	}
}

// TestInstallationJanitorAuditsRejectedMintBeforeFailedRevoke closes the gap a
// validated-only audit leaves: GitHub returns a 201 with a live token but an
// over-broad grant, so the mint is rejected and never used — yet if the revoke
// then also fails, the daemon still holds a live, unrevocable credential. The
// audit is written for the minted token before the returned grant is judged,
// so the ledger identifies that credential with outcome grant_rejected and no
// vouched-for expiry (issue #545 acceptance 2, generalized past the clean
// path).
func TestInstallationJanitorAuditsRejectedMintBeforeFailedRevoke(t *testing.T) {
	t.Parallel()
	listed := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/app/installations":
			_, _ = io.WriteString(w,
				`[{"id":701,"app_id":501,"target_id":101,"repository_selection":"selected","account":{"login":"operator","id":101}}]`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/access_tokens"):
			// A live token with an over-broad grant: rejected, never used.
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"token":"`+fixtureTokenValue+
				`","expires_at":"2026-07-16T13:00:00Z",`+
				`"permissions":{"metadata":"read","contents":"read"},"repository_selection":"selected"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/installation/repositories":
			listed++
			_, _ = io.WriteString(w, `{"total_count":1,"repositories":[{"id":990011}]}`)
		case r.Method == http.MethodDelete && r.URL.Path == "/installation/token":
			// The daemon minted a token it now cannot revoke.
			w.WriteHeader(http.StatusInternalServerError)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	mintRec := &captureMintRecorder{}
	janitor := newMintAuditJanitor(t, srv, mintRec)
	_, err := janitor.RunCycle(context.Background())
	if err == nil {
		t.Fatal("RunCycle accepted a rejected grant with a failed revoke")
	}
	if strings.Contains(err.Error(), fixtureTokenValue) {
		t.Errorf("error leaked the enumeration token: %v", err)
	}
	// A rejected grant is never used: no repository listing.
	if listed != 0 {
		t.Errorf("listed repositories %d times, want 0: a rejected grant must not be used", listed)
	}
	records := mintRec.snapshot()
	if len(records) != 1 {
		t.Fatalf("recorded %d installation mints for a rejected+unrevoked token, want exactly 1", len(records))
	}
	got := records[0]
	if got.Outcome != publish.InstallationMintGrantRejected {
		t.Errorf("outcome = %q, want %q", got.Outcome, publish.InstallationMintGrantRejected)
	}
	if got.RegistrationID != 501 || got.InstallationID != 701 {
		t.Errorf("rejected-mint audit = reg %d inst %d, want reg 501 inst 701",
			got.RegistrationID, got.InstallationID)
	}
	// The daemon does not vouch for a rejected grant: no granted scopes, no
	// validated expiry.
	if got.Granted != (publish.Permissions{}) {
		t.Errorf("granted = %+v, want empty for a rejected grant", got.Granted)
	}
	if got.ExpiresAt != nil {
		t.Errorf("expires_at = %v, want nil for a rejected grant", got.ExpiresAt)
	}
}

// TestInstallationJanitorMintAuditFailureBlocksTokenUse: an unauditable token
// must not circulate, mirroring the worker mint. When the audit write fails,
// the mint fails before the token is used (no repository listing), the token is
// still handed back for revocation, and the pass stops with an unsafe-marked
// error that does not leak the token (issue #545 acceptance 1: recorded before
// use).
func TestInstallationJanitorMintAuditFailureBlocksTokenUse(t *testing.T) {
	t.Parallel()
	listed := 0
	revokes := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/app/installations":
			_, _ = io.WriteString(w,
				`[{"id":701,"app_id":501,"target_id":101,"repository_selection":"selected","account":{"login":"operator","id":101}}]`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/access_tokens"):
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, grantReadMintBody)
		case r.Method == http.MethodGet && r.URL.Path == "/installation/repositories":
			listed++
			_, _ = io.WriteString(w, `{"total_count":1,"repositories":[{"id":990011}]}`)
		case r.Method == http.MethodDelete && r.URL.Path == "/installation/token":
			revokes++
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	auditErr := errors.New("audit ledger unavailable")
	janitor := newMintAuditJanitor(t, srv, &captureMintRecorder{err: auditErr})
	_, err := janitor.RunCycle(context.Background())
	if err == nil {
		t.Fatal("RunCycle accepted an unauditable mint")
	}
	if strings.Contains(err.Error(), fixtureTokenValue) {
		t.Errorf("error leaked the enumeration token: %v", err)
	}
	// The token was never used: the repository listing is the first use, and it
	// must not run when the mint could not be audited.
	if listed != 0 {
		t.Errorf("listed repositories %d times, want 0: an unauditable token must not be used", listed)
	}
	// It is still handed back for revocation, not left live.
	if revokes != 1 {
		t.Errorf("revokes = %d, want 1: the unauditable token must still be revoked", revokes)
	}
}
