package publish_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/strayrequest"
)

// stallTimeout bounds how long a test waits for a call that must not stall.
// It is a failure bound, not a pacing sleep: a passing run returns at once.
const stallTimeout = 10 * time.Second

// stallingForge serves two App registrations, one repository each, and
// stalls the first registration's GitHub calls while a gate is set. Every
// resolution lists every registration's installations (the complete candidate
// set the ambiguity rule needs), so the discovery gate stalls one listing
// request, the one made for the first repository, and lets later listings
// through: a hung request must not hold up another caller's own request.
type stallingForge struct {
	firstJWT, secondJWT string
	stallDiscovery      atomic.Bool
	stallMint           atomic.Bool
	release             chan struct{}
	mints               sync.Map // authorization header -> *atomic.Int64
	stalled             chan string
}

func (f *stallingForge) mintCount(jwt string) int64 {
	count, ok := f.mints.Load("Bearer " + jwt)
	if !ok {
		return 0
	}
	return count.(*atomic.Int64).Load()
}

func (f *stallingForge) stall(r *http.Request, step string) {
	select {
	case f.stalled <- step:
	default:
	}
	select {
	case <-f.release:
	case <-r.Context().Done():
	}
}

const (
	stallSecondOwner   = "freeasinbird"
	stallSecondOwnerID = testOwnerID + 1
	stallSecondAppID   = fixtureAppID + 1
	stallSecondRepoID  = fixtureRepositoryID + 1
	stallSecondRepo    = stallSecondOwner + "/evidence-repo"
)

// newTwoRegistrationSource stands up the forge and a CachedTokenSource over
// both registrations, with testTrustRepo on the first and stallSecondRepo on
// the second.
func newTwoRegistrationSource(t *testing.T, recorder *captureRecorder) (*publish.CachedTokenSource, *stallingForge) {
	t.Helper()
	ks := newRegisteredKeystore(t)
	if err := ks.SaveApp(publish.AppCredentials{
		Owner: stallSecondOwner, OwnerID: stallSecondOwnerID, Visibility: publish.AppVisibilityPrivate,
		AppID: stallSecondAppID, Name: "second-app", Key: fixtureKey(t),
	}); err != nil {
		t.Fatal(err)
	}
	firstJWT, err := publish.AppJWT(fixtureKey(t), fixtureAppID, fixtureTime)
	if err != nil {
		t.Fatal(err)
	}
	secondJWT, err := publish.AppJWT(fixtureKey(t), stallSecondAppID, fixtureTime)
	if err != nil {
		t.Fatal(err)
	}
	forge := &stallingForge{
		firstJWT: firstJWT.Reveal(), secondJWT: secondJWT.Reveal(),
		release: make(chan struct{}), stalled: make(chan string, 4),
	}
	fixture, err := os.ReadFile(filepath.Join("testdata", "token-response.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(strayrequest.Ignore(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		first := auth == "Bearer "+forge.firstJWT
		if r.Method == http.MethodGet && r.URL.Path == "/app/installations" {
			if first && forge.stallDiscovery.CompareAndSwap(true, false) {
				forge.stall(r, "discovery")
			}
			if first {
				_, _ = fmt.Fprintf(w, `[{"id":777,"app_id":%d,"target_id":%d,"repository_selection":"selected","account":{"login":"freeside-ai","id":%d}}]`,
					fixtureAppID, testOwnerID, testOwnerID)
			} else {
				_, _ = fmt.Fprintf(w, `[{"id":778,"app_id":%d,"target_id":%d,"repository_selection":"selected","account":{"login":"%s","id":%d}}]`,
					stallSecondAppID, stallSecondOwnerID, stallSecondOwner, stallSecondOwnerID)
			}
			return
		}
		if first && forge.stallMint.Load() {
			forge.stall(r, "mint")
		}
		count, _ := forge.mints.LoadOrStore(auth, &atomic.Int64{})
		count.(*atomic.Int64).Add(1)
		body := fixture
		if !first {
			body = bytes.Replace(fixture, []byte(`"id": 990011`), []byte(`"id": 990012`), 1)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(body)
	})))
	t.Cleanup(srv.Close)
	t.Cleanup(func() {
		select {
		case <-forge.release:
		default:
			close(forge.release)
		}
	})
	trust := repositoryTrustSource{
		testTrustRepo:   trustProfileForRepo(t, testTrustRepo),
		stallSecondRepo: trustProfileForRepoID(t, stallSecondRepo, stallSecondRepoID),
	}
	m := newCoveredMinter(ks, srv.Client(), srv.URL, recorder, trust, fixedNow)
	return publish.NewCachedTokenSource(m, fixedNow), forge
}

type tokenResult struct {
	token publish.InstallationToken
	err   error
}

func tokenAsync(ctx context.Context, src *publish.CachedTokenSource, repo string) <-chan tokenResult {
	done := make(chan tokenResult, 1)
	go func() {
		token, err := src.Token(ctx, repo)
		done <- tokenResult{token: token, err: err}
	}()
	return done
}

func awaitToken(t *testing.T, name string, done <-chan tokenResult) publish.InstallationToken {
	t.Helper()
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("%s: %v", name, got.err)
		}
		return got.token
	case <-time.After(stallTimeout):
		t.Fatalf("%s: no token within %s", name, stallTimeout)
	}
	return publish.InstallationToken{}
}

// TestTokenSourceServesOtherRepositoryWhileOneStalls: while the first
// repository's installation listing or mint hangs on GitHub, the second
// repository still gets its token, and the stalled call finishes once GitHub
// answers.
func TestTokenSourceServesOtherRepositoryWhileOneStalls(t *testing.T) {
	t.Parallel()
	for _, step := range []string{"discovery", "mint"} {
		t.Run(step, func(t *testing.T) {
			t.Parallel()
			src, forge := newTwoRegistrationSource(t, &captureRecorder{})
			if step == "discovery" {
				forge.stallDiscovery.Store(true)
			} else {
				forge.stallMint.Store(true)
			}
			stalled := tokenAsync(t.Context(), src, testTrustRepo)
			select {
			case got := <-forge.stalled:
				if got != step {
					t.Fatalf("stalled at %s, want %s", got, step)
				}
			case <-time.After(stallTimeout):
				t.Fatal("first repository never reached GitHub")
			}
			second := awaitToken(t, "second repository", tokenAsync(t.Context(), src, stallSecondRepo))
			if second.RepositoryID != stallSecondRepoID {
				t.Fatalf("second repository token = %+v", second)
			}
			select {
			case got := <-stalled:
				t.Fatalf("stalled call returned before GitHub answered: %+v", got)
			default:
			}
			close(forge.release)
			if first := awaitToken(t, "first repository", stalled); first.RepositoryID != fixtureRepositoryID {
				t.Fatalf("first repository token = %+v", first)
			}
		})
	}
}

// TestTokenSourceMintsOnceForConcurrentCallers: callers that ask for one key
// while its mint runs wait on that mint, so GitHub sees one mint and the audit
// one row.
func TestTokenSourceMintsOnceForConcurrentCallers(t *testing.T) {
	t.Parallel()
	recorder := &captureRecorder{}
	src, forge := newTwoRegistrationSource(t, recorder)
	forge.stallMint.Store(true)
	const callers = 8
	results := make([]<-chan tokenResult, callers)
	results[0] = tokenAsync(t.Context(), src, testTrustRepo)
	select {
	case <-forge.stalled:
	case <-time.After(stallTimeout):
		t.Fatal("first caller never reached the mint")
	}
	for index := 1; index < callers; index++ {
		results[index] = tokenAsync(t.Context(), src, testTrustRepo)
	}
	close(forge.release)
	for index, done := range results {
		awaitToken(t, fmt.Sprintf("caller %d", index), done)
	}
	if got := forge.mintCount(forge.firstJWT); got != 1 {
		t.Fatalf("concurrent callers minted %d times, want 1", got)
	}
	if len(recorder.records) != 1 {
		t.Fatalf("audit holds %d records, want 1", len(recorder.records))
	}
}

// TestTokenSourceWaiterStopsWhenItsContextEnds: a caller waiting on another
// caller's mint returns its context error instead of waiting for GitHub.
func TestTokenSourceWaiterStopsWhenItsContextEnds(t *testing.T) {
	t.Parallel()
	src, forge := newTwoRegistrationSource(t, &captureRecorder{})
	forge.stallMint.Store(true)
	minting := tokenAsync(t.Context(), src, testTrustRepo)
	select {
	case <-forge.stalled:
	case <-time.After(stallTimeout):
		t.Fatal("first caller never reached the mint")
	}
	ctx, cancel := context.WithCancel(t.Context())
	waiting := tokenAsync(ctx, src, testTrustRepo)
	cancel()
	select {
	case got := <-waiting:
		if got.err == nil {
			t.Fatal("canceled waiter returned a token")
		}
	case <-time.After(stallTimeout):
		t.Fatal("canceled waiter kept waiting on the mint")
	}
	close(forge.release)
	awaitToken(t, "minting caller", minting)
}

// repoTokenSource hands each repository its own token, and stalls the first
// repository's Token call while stall is set.
type repoTokenSource struct {
	stall   atomic.Bool
	release chan struct{}
	stalled chan struct{}
}

func (s *repoTokenSource) Token(ctx context.Context, repo string) (publish.InstallationToken, error) {
	token := appBotIdentityToken()
	if repo == testTrustRepo && s.stall.Load() {
		s.stalled <- struct{}{}
		select {
		case <-s.release:
		case <-ctx.Done():
			return publish.InstallationToken{}, ctx.Err()
		}
	}
	if repo != testTrustRepo {
		token.Token = publish.Secret("github_pat_app_identity_second")
		token.RepositoryID = fixtureRepositoryID + 1
		token.Repo = repo
	}
	return token, nil
}

// TestBotIdentityServesOtherRepositoryWhileOneStalls: while the first
// repository's token or bot lookup hangs, the second repository's identity
// still resolves.
func TestBotIdentityServesOtherRepositoryWhileOneStalls(t *testing.T) {
	t.Parallel()
	const secondRepo = "freeside-ai/second-repo"
	for _, step := range []string{"token", "lookup"} {
		t.Run(step, func(t *testing.T) {
			t.Parallel()
			release, lookupStalled := make(chan struct{}), make(chan struct{}, 1)
			var once sync.Once
			t.Cleanup(func() { once.Do(func() { close(release) }) })
			server := httptest.NewServer(strayrequest.Ignore(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if step == "lookup" && r.Header.Get("Authorization") == "Bearer github_pat_app_identity_test" {
					lookupStalled <- struct{}{}
					select {
					case <-release:
					case <-r.Context().Done():
					}
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"login":"freeside-publish[bot]","id":308829240,"type":"Bot"}`))
			})))
			t.Cleanup(server.Close)
			tokens := &repoTokenSource{release: release, stalled: make(chan struct{}, 1)}
			tokens.stall.Store(step == "token")
			resolver, err := publish.NewGitHubAppBotIdentityResolver(
				tokens, appBotIdentityKeystore(t), server.Client(), server.URL,
				func() time.Time { return appBotIdentityNow },
			)
			if err != nil {
				t.Fatal(err)
			}
			stalled := make(chan error, 1)
			go func() {
				_, err := resolver.Resolve(t.Context(), testTrustRepo)
				stalled <- err
			}()
			reached := map[string]chan struct{}{"token": tokens.stalled, "lookup": lookupStalled}[step]
			select {
			case <-reached:
			case <-time.After(stallTimeout):
				t.Fatal("first repository never reached its stalled step")
			}
			second := make(chan error, 1)
			go func() {
				identity, err := resolver.Resolve(t.Context(), secondRepo)
				if err == nil && identity.BotUserID != 308829240 {
					err = fmt.Errorf("second identity = %+v", identity)
				}
				second <- err
			}()
			select {
			case err := <-second:
				if err != nil {
					t.Fatalf("second repository: %v", err)
				}
			case <-time.After(stallTimeout):
				t.Fatal("second repository waited on the first repository's stall")
			}
			once.Do(func() { close(release) })
			select {
			case err := <-stalled:
				if err != nil {
					t.Fatalf("first repository: %v", err)
				}
			case <-time.After(stallTimeout):
				t.Fatal("first repository never finished")
			}
		})
	}
}
