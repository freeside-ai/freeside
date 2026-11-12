package signet_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// TestHTTPRestartReadiness: a paired device reads the verdict, which is not
// ready with no drain and ready once a drain is in force with nothing held,
// and an unpaired request is refused.
func TestHTTPRestartReadiness(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	handler := signet.NewHTTPHandler(f.service, testAuthorizer)

	read := func() (*http.Response, []byte) {
		t.Helper()
		response := authenticatedRequest(t, handler, http.MethodGet, "/restart-readiness", nil)
		return response.Result(), response.Body.Bytes()
	}
	response, body := read()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET /restart-readiness status = %d body=%s", response.StatusCode, body)
	}
	var undrained signet.RestartReadinessResponse
	if err := json.Unmarshal(body, &undrained); err != nil {
		t.Fatal(err)
	}
	if undrained.Ready || undrained.Drain != nil || undrained.Blockers == nil || len(undrained.Blockers) != 0 {
		t.Fatalf("readiness with no drain = %+v, want not ready, no drain, an empty blocker list", undrained)
	}

	if err := f.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordUpdateDrainTransition(ctx, domain.UpdateDrainTransition{
			State:      domain.UpdateDrainDraining,
			Build:      "sha256:1111111111111111111111111111111111111111111111111111111111111111",
			Forced:     true,
			OccurredAt: f.now.Add(-time.Minute),
		})
	}); err != nil {
		t.Fatal(err)
	}
	response, body = read()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET /restart-readiness status = %d body=%s", response.StatusCode, body)
	}
	golden.Assert(t, "restart-readiness", body)

	unpaired := bearerRequest(t, handler, http.MethodGet, "/restart-readiness", "", nil)
	if unpaired.Code != http.StatusUnauthorized || unpaired.Header().Get("WWW-Authenticate") != "Bearer" {
		t.Fatalf("unpaired status = %d WWW-Authenticate %q, want 401 Bearer",
			unpaired.Code, unpaired.Header().Get("WWW-Authenticate"))
	}
}
