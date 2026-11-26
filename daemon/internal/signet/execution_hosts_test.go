package signet_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// TestHTTPExecutionHosts: a paired device lists the daemon's host and an
// extra host in a condition, without reach or count override, and an
// unpaired request is refused.
func TestHTTPExecutionHosts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	handler := signet.NewHTTPHandler(f.service, testAuthorizer)

	account := "runner"
	override := 2
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		return tx.AddExecutionHost(ctx, domain.ExecutionHost{
			ID: "host-1", Name: "studio",
			RunAccount:            domain.HostRunAccount{Name: &account},
			Reach:                 &domain.HostReach{Address: "studio.local", HostKey: "ssh-ed25519 AAAAhostkey"},
			CordonedCountOverride: &override,
			Conditions: []domain.HostObservedCondition{
				{Condition: domain.ExecutionHostNoDesktopSession, Since: time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)},
			},
		})
	}); err != nil {
		t.Fatal(err)
	}
	response := authenticatedRequest(t, handler, http.MethodGet, "/execution-hosts", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("GET /execution-hosts status = %d body=%s", response.Code, response.Body.Bytes())
	}
	golden.Assert(t, "execution-hosts", response.Body.Bytes())

	unpaired := bearerRequest(t, handler, http.MethodGet, "/execution-hosts", "", nil)
	if unpaired.Code != http.StatusUnauthorized || unpaired.Header().Get("WWW-Authenticate") != "Bearer" {
		t.Fatalf("unpaired status = %d WWW-Authenticate %q, want 401 Bearer",
			unpaired.Code, unpaired.Header().Get("WWW-Authenticate"))
	}
}
