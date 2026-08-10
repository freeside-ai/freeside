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

// The mint clock is fixtureTime (2026-07-16T12:00:00Z), so GitHub's
// one-hour lifetime lands at 13:00:00Z and the accepted upper bound,
// that lifetime plus the package's clock skew, at 13:09:00Z. The
// package-internal table derives the same edges from the constants
// themselves; these literals are the wire-level statement of them.
type expiryCase struct {
	name string
	// member is the response's expires_at member verbatim, or "" to omit
	// the field entirely.
	member string
	// value is the untrusted text the member carries; no error or audit
	// record may reproduce it.
	value string
	// want is the instant an accepted case must yield, RFC3339 UTC.
	want string
}

// rejectedExpiries enumerates the untrusted expires_at values every mint
// path must refuse: missing, malformed, lapsed, and past the bound. The
// far-future case is the reported regression (#413).
var rejectedExpiries = []expiryCase{
	{name: "omitted"},
	{name: "null", member: `"expires_at":null`},
	{name: "empty", member: `"expires_at":""`},
	{name: "malformed", member: `"expires_at":"not-a-timestamp"`, value: "not-a-timestamp"},
	{name: "date only", member: `"expires_at":"2026-07-16"`, value: "2026-07-16"},
	{name: "unix seconds", member: `"expires_at":"1784206800"`, value: "1784206800"},
	{
		name:   "credential-shaped",
		member: `"expires_at":"` + fixtureTokenValue + `"`,
		value:  fixtureTokenValue,
	},
	{name: "already expired", member: `"expires_at":"2026-07-16T11:00:00Z"`, value: "2026-07-16T11:00:00Z"},
	{name: "expiring exactly now", member: `"expires_at":"2026-07-16T12:00:00Z"`, value: "2026-07-16T12:00:00Z"},
	{
		name:   "one nanosecond past the bound",
		member: `"expires_at":"2026-07-16T13:09:00.000000001Z"`,
		value:  "2026-07-16T13:09:00.000000001Z",
	},
	{name: "a day out", member: `"expires_at":"2026-07-17T12:00:00Z"`, value: "2026-07-17T12:00:00Z"},
	{name: "a century out", member: `"expires_at":"2126-07-16T13:00:00Z"`, value: "2126-07-16T13:00:00Z"},
}

// acceptedExpiries enumerates the values that stay inside the bound,
// including both of its edges.
var acceptedExpiries = []expiryCase{
	{
		name:   "GitHub's one-hour lifetime",
		member: `"expires_at":"2026-07-16T13:00:00Z"`,
		want:   "2026-07-16T13:00:00Z",
	},
	{
		name:   "that lifetime in another zone",
		member: `"expires_at":"2026-07-16T15:00:00+02:00"`,
		want:   "2026-07-16T13:00:00Z",
	},
	{
		name:   "exactly at the skew bound",
		member: `"expires_at":"2026-07-16T13:09:00Z"`,
		want:   "2026-07-16T13:09:00Z",
	},
	{
		name:   "a shorter lifetime",
		member: `"expires_at":"2026-07-16T12:30:00Z"`,
		want:   "2026-07-16T12:30:00Z",
	},
}

// jsonMember renders an optional response member for concatenation.
func jsonMember(member string) string {
	if member == "" {
		return ""
	}
	return "," + member
}

func workerMintBody(member string) string {
	return `{"token":"` + fixtureTokenValue + `"` + jsonMember(member) + `,` +
		`"permissions":{"actions":"read","administration":"read","contents":"write",` +
		`"environments":"read","pull_requests":"write","metadata":"read"},` +
		`"repository_selection":"selected","repositories":[{"id":990011,"name":"evidence-repo"}]}`
}

func onboardingMintBody(member string) string {
	return `{"token":"` + fixtureTokenValue + `"` + jsonMember(member) + `,` +
		`"permissions":{"actions":"read","administration":"read","contents":"read",` +
		`"environments":"read","metadata":"read"},` +
		`"repository_selection":"selected","repositories":[{"id":990011,"name":"evidence-repo"}]}`
}

func grantReadBody(member string) string {
	return `{"token":"` + fixtureTokenValue + `"` + jsonMember(member) + `,` +
		`"permissions":{"metadata":"read"},"repository_selection":"selected"}`
}

// assertNoExpiryLeak asserts the rejection carries neither the token nor
// the attacker-chosen expiry text: both are response fields a proxy
// chooses, and the error is rendered beside audit output.
func assertNoExpiryLeak(t *testing.T, err error, tc expiryCase) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s expiry accepted, want rejection", tc.name)
	}
	if strings.Contains(err.Error(), fixtureTokenValue) {
		t.Errorf("error carries the token: %v", err)
	}
	if tc.value != "" && strings.Contains(err.Error(), tc.value) {
		t.Errorf("error carries the rejected expiry %q: %v", tc.value, err)
	}
}

// TestMintRejectsExpiryOutsideTheDeclaredBound is the #413 regression on
// the worker-bound path: an expiry that is missing, malformed, lapsed,
// or longer than GitHub's declared lifetime allows is refused before the
// token is audited or returned.
func TestMintRejectsExpiryOutsideTheDeclaredBound(t *testing.T) {
	t.Parallel()
	ks := newRegisteredKeystore(t)
	for _, tc := range rejectedExpiries {
		t.Run(tc.name, func(t *testing.T) {
			srv := newMintServer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusCreated)
				_, _ = io.WriteString(w, workerMintBody(tc.member))
			})
			defer srv.Close()

			rec := &captureRecorder{}
			m := newCoveredMinter(ks, srv.Client(), srv.URL, rec, conformantTrust(t), fixedNow)
			tok, err := m.MintInstallationToken(context.Background(), testTrustRepo)
			assertNoExpiryLeak(t, err, tc)
			if !errors.Is(err, publish.ErrTokenExpiry) {
				t.Fatalf("err = %v, want ErrTokenExpiry", err)
			}
			if tok.Token.Reveal() != "" || !tok.ExpiresAt.IsZero() {
				t.Errorf("rejected mint returned a token: %+v", tok.ExpiresAt)
			}
			if len(rec.records) != 0 {
				t.Errorf("rejected mint recorded %d audit rows, want 0", len(rec.records))
			}
		})
	}
}

// TestMintAcceptsExpiryInsideTheDeclaredBound pins both edges of the
// bound: the fix must not refuse a conformant GitHub response, including
// one that arrives at the skew limit or in a non-UTC zone.
func TestMintAcceptsExpiryInsideTheDeclaredBound(t *testing.T) {
	t.Parallel()
	ks := newRegisteredKeystore(t)
	for _, tc := range acceptedExpiries {
		t.Run(tc.name, func(t *testing.T) {
			want, err := time.Parse(time.RFC3339, tc.want)
			if err != nil {
				t.Fatal(err)
			}
			srv := newMintServer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusCreated)
				_, _ = io.WriteString(w, workerMintBody(tc.member))
			})
			defer srv.Close()

			rec := &captureRecorder{}
			m := newCoveredMinter(ks, srv.Client(), srv.URL, rec, conformantTrust(t), fixedNow)
			tok, err := m.MintInstallationToken(context.Background(), testTrustRepo)
			if err != nil {
				t.Fatalf("conformant expiry rejected: %v", err)
			}
			if !tok.ExpiresAt.Equal(want) || tok.ExpiresAt.Location() != time.UTC {
				t.Errorf("token expiry = %v, want %v as a UTC instant", tok.ExpiresAt, want)
			}
			if len(rec.records) != 1 || !rec.records[0].ExpiresAt.Equal(want) {
				t.Errorf("audit rows = %+v, want one carrying %v", rec.records, want)
			}
		})
	}
}

// TestCachedTokenSourceNeverCachesARejectedExpiry is the downstream
// negative assertion for the worker path: a refused response leaves no
// cache entry, so a second call re-mints instead of handing out the
// token the first call rejected.
func TestCachedTokenSourceNeverCachesARejectedExpiry(t *testing.T) {
	t.Parallel()
	ks := newRegisteredKeystore(t)
	for _, tc := range rejectedExpiries {
		t.Run(tc.name, func(t *testing.T) {
			mints := 0
			srv := newMintServer(t, func(w http.ResponseWriter, _ *http.Request) {
				mints++
				w.WriteHeader(http.StatusCreated)
				_, _ = io.WriteString(w, workerMintBody(tc.member))
			})
			defer srv.Close()

			rec := &captureRecorder{}
			m := newCoveredMinter(ks, srv.Client(), srv.URL, rec, conformantTrust(t), fixedNow)
			tokens := publish.NewCachedTokenSource(m, fixedNow)
			for range 2 {
				tok, err := tokens.Token(context.Background(), testTrustRepo)
				assertNoExpiryLeak(t, err, tc)
				if tok.Token.Reveal() != "" {
					t.Fatalf("rejected expiry circulated as a token")
				}
			}
			if mints != 2 {
				t.Errorf("mint requests = %d, want 2: a rejected token was cached", mints)
			}
			if len(rec.records) != 0 {
				t.Errorf("rejected mints recorded %d audit rows, want 0", len(rec.records))
			}
		})
	}
}

// overLongExpiry is an expiry the bound refuses, used to prove the
// expiry check does not preempt the scope comparison.
const overLongExpiry = `"expires_at":"2126-07-16T13:00:00Z"`

// TestMintKeepsGrantMismatchForAnOverLongGrant pins the check order on
// the worker path. A response that is over-broad *and* over-long must
// still surface ErrGrantMismatch: that sentinel is what callers key
// permanence on, so an expiry check placed ahead of the scope
// comparison would silently downgrade a forged grant to a retryable
// failure.
func TestMintKeepsGrantMismatchForAnOverLongGrant(t *testing.T) {
	t.Parallel()
	ks := newRegisteredKeystore(t)
	rec := &captureRecorder{}
	srv := newMintServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"token":"`+fixtureTokenValue+`",`+overLongExpiry+`,`+
			`"permissions":{"actions":"read","administration":"read","contents":"write",`+
			`"environments":"read","pull_requests":"write","metadata":"read","issues":"write"},`+
			`"repository_selection":"selected","repositories":[{"id":990011,"name":"evidence-repo"}]}`)
	})
	defer srv.Close()

	m := newCoveredMinter(ks, srv.Client(), srv.URL, rec, conformantTrust(t), fixedNow)
	_, err := m.MintInstallationToken(context.Background(), testTrustRepo)
	if !errors.Is(err, publish.ErrGrantMismatch) {
		t.Fatalf("err = %v, want ErrGrantMismatch", err)
	}
	if errors.Is(err, publish.ErrTokenExpiry) {
		t.Fatalf("err = %v, want grant mismatch without ErrTokenExpiry", err)
	}
	if len(rec.records) != 0 {
		t.Errorf("rejected mint recorded %d audit rows, want 0", len(rec.records))
	}
}

// TestInstallationJanitorKeepsGrantMismatchForAnOverLongGrant is the
// same ordering guarantee on the grant-read mint.
func TestInstallationJanitorKeepsGrantMismatchForAnOverLongGrant(t *testing.T) {
	t.Parallel()
	ks := publicJanitorKeystore(t)
	recorder := &removalRecorder{}
	revokes := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/app/installations":
			_, _ = io.WriteString(w,
				`[{"id":701,"app_id":501,"target_id":101,`+
					`"repository_selection":"selected","account":{"login":"operator","id":101}}]`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/access_tokens"):
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"token":"`+fixtureTokenValue+`",`+overLongExpiry+`,`+
				`"permissions":{"metadata":"read","contents":"read"},"repository_selection":"selected"}`)
		case r.Method == http.MethodDelete && r.URL.Path == "/installation/token":
			revokes++
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	janitor := newJanitor(t, ks, srv, trustedPublicBinding(fixtureRepositoryID), recorder, 1)
	_, err := janitor.RunCycle(context.Background())
	if !errors.Is(err, publish.ErrGrantMismatch) {
		t.Fatalf("err = %v, want ErrGrantMismatch", err)
	}
	if errors.Is(err, publish.ErrTokenExpiry) {
		t.Fatalf("err = %v, want grant mismatch without ErrTokenExpiry", err)
	}
	if revokes != 1 || len(recorder.snapshot()) != 0 {
		t.Errorf("revokes = %d, records = %d", revokes, len(recorder.snapshot()))
	}
}

// newOnboardingSource wires the read-only onboarding token source against
// server, with the janitor gate already reporting the pending
// installation reconciled.
func newOnboardingSource(
	t *testing.T,
	ks *publish.Keystore,
	server *httptest.Server,
	recorder publish.Recorder,
) *publish.OnboardingTokenSource {
	t.Helper()
	authority := &onboardingAuthoritySource{authority: publish.InstallationAuthority{
		ActiveEpoch: 1, DurableIntentRevision: 2,
		TrustedOwners: []publish.TrustedOwner{{Login: "freeside-ai", ID: testOwnerID}},
		Pending: &publish.PendingInstallationEnvelope{
			ActiveEpoch: 1, DurableIntentRevision: 2,
			RegistrationID:  fixtureAppID,
			ExpectedAccount: "freeside-ai", ExpectedAccountID: testOwnerID,
			InstallationID:         777,
			CurrentRepositoryIDs:   []int64{},
			ExpectedRepositoryIDs:  []int64{fixtureRepositoryID},
			RequiredRepositoryMode: "selected",
			ExpiresAt:              fixtureTime.Add(time.Hour),
		},
	}}
	minter := publish.NewMinterWithJanitor(
		ks, server.Client(), server.URL, recorder, nil, fixedNow, activeJanitorStatus{},
	)
	return publish.NewOnboardingTokenSource(
		minter,
		authority,
		&onboardingGate{installationID: 777, pendingReady: true},
		fixtureAppID,
		fixtureRepositoryID,
		fixedNow,
	)
}

// TestOnboardingTokenSourceRejectsExpiryOutsideTheDeclaredBound proves
// the read-only onboarding mint applies the same bound, caches nothing
// it refused, and audits nothing.
func TestOnboardingTokenSourceRejectsExpiryOutsideTheDeclaredBound(t *testing.T) {
	t.Parallel()
	ks := newTestKeystore(t)
	if err := ks.SaveApp(publicFixtureCredentials(t)); err != nil {
		t.Fatal(err)
	}
	for _, tc := range rejectedExpiries {
		t.Run(tc.name, func(t *testing.T) {
			mints := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				mints++
				w.WriteHeader(http.StatusCreated)
				_, _ = io.WriteString(w, onboardingMintBody(tc.member))
			}))
			defer srv.Close()

			rec := &captureRecorder{}
			tokens := newOnboardingSource(t, ks, srv, rec)
			for range 2 {
				tok, err := tokens.Token(context.Background(), testTrustRepo)
				assertNoExpiryLeak(t, err, tc)
				if tok.Token.Reveal() != "" {
					t.Fatalf("rejected expiry circulated as an onboarding token")
				}
			}
			if mints != 2 {
				t.Errorf("mint requests = %d, want 2: a rejected token was cached", mints)
			}
			if len(rec.records) != 0 {
				t.Errorf("rejected mints recorded %d audit rows, want 0", len(rec.records))
			}
		})
	}
}

// TestInstallationJanitorRejectsGrantReadExpiryOutsideTheDeclaredBound
// closes the janitor half of #413: the grant-read credential's expiry is
// decoded and bounded before enumeration, the refused token is still
// revoked, and the pass publishes no coverage and performs no removal.
func TestInstallationJanitorRejectsGrantReadExpiryOutsideTheDeclaredBound(t *testing.T) {
	t.Parallel()
	ks := publicJanitorKeystore(t)
	for _, tc := range rejectedExpiries {
		t.Run(tc.name, func(t *testing.T) {
			var lists, revokes, destructive int
			recorder := &removalRecorder{}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/app/installations":
					_, _ = io.WriteString(w,
						`[{"id":701,"app_id":501,"target_id":101,`+
							`"repository_selection":"selected","account":{"login":"operator","id":101}}]`)
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/access_tokens"):
					w.WriteHeader(http.StatusCreated)
					_, _ = io.WriteString(w, grantReadBody(tc.member))
				case r.Method == http.MethodGet && r.URL.Path == "/installation/repositories":
					lists++
					_, _ = io.WriteString(w, `{"total_count":1,"repositories":[{"id":990011}]}`)
				case r.Method == http.MethodDelete && r.URL.Path == "/installation/token":
					revokes++
					w.WriteHeader(http.StatusNoContent)
				default:
					destructive++
					w.WriteHeader(http.StatusNoContent)
				}
			}))
			defer srv.Close()

			janitor := newJanitor(t, ks, srv, trustedPublicBinding(fixtureRepositoryID), recorder, 1)
			_, err := janitor.RunCycle(context.Background())
			assertNoExpiryLeak(t, err, tc)
			if lists != 0 {
				t.Errorf("enumeration ran %d times on a rejected token, want 0", lists)
			}
			if revokes != 1 {
				t.Errorf("revokes = %d, want the rejected token revoked once", revokes)
			}
			if destructive != 0 || len(recorder.snapshot()) != 0 || janitor.ActiveFor(501) {
				t.Errorf("destructive = %d, records = %d, active = %t",
					destructive, len(recorder.snapshot()), janitor.ActiveFor(501))
			}
		})
	}
}
