package publish_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

// newMintCountingSource stands up a fixture-backed mint endpoint and a
// CachedTokenSource over it, returning the source and a pointer to the
// mint count. The fixture token expires at 13:00; now starts at
// fixtureTime (12:00) and is advanced through the clock pointer.
func newMintCountingSource(t *testing.T, clock *time.Time) (*publish.CachedTokenSource, *int) {
	t.Helper()
	mints := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mints++
		w.WriteHeader(http.StatusCreated)
		fixture, err := os.ReadFile(filepath.Join("testdata", "token-response.json"))
		if err != nil {
			t.Errorf("read fixture: %v", err)
		}
		_, _ = w.Write(fixture)
	}))
	t.Cleanup(srv.Close)

	m := publish.NewMinter(newRegisteredKeystore(t), srv.Client(), srv.URL, &captureRecorder{}, func() time.Time { return *clock })
	return publish.NewCachedTokenSource(m, 777, func() time.Time { return *clock }), &mints
}

// TestCachedTokenSourceReusesUntilExpiry: repeated Token calls inside
// the token's lifetime mint exactly once (one audit row per mint, not
// per request).
func TestCachedTokenSourceReusesUntilExpiry(t *testing.T) {
	clock := fixtureTime
	src, mints := newMintCountingSource(t, &clock)

	first, err := src.Token(context.Background(), "evidence-repo")
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	clock = fixtureTime.Add(30 * time.Minute)
	second, err := src.Token(context.Background(), "evidence-repo")
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if *mints != 1 {
		t.Errorf("minted %d times, want 1", *mints)
	}
	if first.Token.Reveal() != second.Token.Reveal() {
		t.Error("cached call returned a different token")
	}
}

// TestCachedTokenSourceRemintsNearExpiry: once inside the expiry skew
// the cached token is no longer handed out — a token about to lapse
// mid-publication would fail the path halfway.
func TestCachedTokenSourceRemintsNearExpiry(t *testing.T) {
	clock := fixtureTime
	src, mints := newMintCountingSource(t, &clock)

	if _, err := src.Token(context.Background(), "evidence-repo"); err != nil {
		t.Fatalf("Token: %v", err)
	}
	// 12:59, one minute from the fixture's 13:00 expiry: inside the
	// two-minute skew.
	clock = fixtureTime.Add(59 * time.Minute)
	if _, err := src.Token(context.Background(), "evidence-repo"); err != nil {
		t.Fatalf("Token near expiry: %v", err)
	}
	if *mints != 2 {
		t.Errorf("minted %d times, want 2 (re-mint inside the skew)", *mints)
	}
}

// TestCachedTokenSourceValidation covers the fail-fast argument check.
func TestCachedTokenSourceValidation(t *testing.T) {
	clock := fixtureTime
	src, _ := newMintCountingSource(t, &clock)
	if _, err := src.Token(context.Background(), ""); err == nil {
		t.Error("empty repo accepted, want error")
	}
}
