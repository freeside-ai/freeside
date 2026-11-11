package daemoncompose

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

const usageManifest = domain.Digest("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")

// seedUsageEnrollment records an identity on pool and one enrollment of
// harness on it at generation one.
func seedUsageEnrollment(
	t *testing.T, st *store.Store, identity domain.AuthIdentityID, enrollment domain.ClientEnrollmentID,
	pool domain.UsagePoolID, harness domain.HarnessClientKind, at time.Time,
) {
	t.Helper()
	ctx := t.Context()
	account := "acct-" + string(identity)
	record := domain.ClientEnrollment{
		ID: enrollment, AuthIdentityID: identity, HarnessClient: harness,
		Route: "openai_chatgpt_codex", AuthMethod: domain.AuthMethodOAuth,
		CredentialMode:  domain.CredentialSubscriptionContained,
		RefreshStrategy: domain.RefreshOnDemand, SupportsReadOnlyAuthSnapshot: true, AccountBinding: account,
	}
	provider := domain.AuthProviderOpenAI
	if harness == domain.HarnessClientClaudeCode {
		provider = domain.AuthProviderClaude
		record.Route, record.AuthMethod, record.RefreshStrategy = "anthropic-subscription", domain.AuthMethodSetupToken, domain.RefreshExternal
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.RecordAuthIdentity(ctx, domain.AuthIdentity{
			ID: identity, Provider: provider, AccountBinding: account, UsagePool: string(pool),
			AuthStoreMutationLease: true, Enabled: true,
		}, at); err != nil {
			return err
		}
		if err := tx.RecordClientEnrollment(ctx, record, at); err != nil {
			return err
		}
		binding := &domain.LeaseGenerationBinding{
			EnrollmentID: enrollment, AuthStoreVolume: "store-" + string(enrollment), StoreManifestDigest: usageManifest,
		}
		lease, err := tx.AcquireAuthStoreMutationLeaseBound(ctx, identity, "seed", binding, at, at.Add(time.Minute))
		if err != nil {
			return err
		}
		entry := domain.EnrollmentGeneration{
			EnrollmentID: enrollment, AuthStoreVolume: binding.AuthStoreVolume, StoreManifestDigest: usageManifest,
			LeaseFence: lease.Fence, AccountBinding: account, RecordedAt: at,
		}
		if record.AuthMethod == domain.AuthMethodOAuth {
			entry.TokenExpiry = new(at.Add(24 * time.Hour))
		}
		if _, err := tx.AppendEnrollmentGeneration(ctx, entry, at); err != nil {
			return err
		}
		return tx.ReleaseAuthStoreMutationLease(ctx, identity, "seed", lease.Fence, at)
	}); err != nil {
		t.Fatalf("seed %s: %v", enrollment, err)
	}
}

// blockingUsageCollector answers supported with one window once released,
// signaling each call on called.
type blockingUsageCollector struct {
	mu       sync.Mutex
	requests []ward.UsageCollectionRequest
	called   chan struct{}
	release  chan struct{}
}

func (c *blockingUsageCollector) CollectUsage(
	ctx context.Context, request ward.UsageCollectionRequest,
) (ward.UsageCollection, error) {
	c.mu.Lock()
	c.requests = append(c.requests, request)
	c.mu.Unlock()
	c.called <- struct{}{}
	select {
	case <-c.release:
	case <-ctx.Done():
		return ward.UsageCollection{}, ctx.Err()
	}
	return ward.UsageCollection{
		Availability: domain.AvailabilitySupported,
		Windows: []domain.WindowReading{{
			Window: "five_hour", EnrollmentID: request.Enrollment.ID, Generation: request.Generation,
			Status: domain.WindowStatusAllowed, UsedPercent: new(42), ObservedAt: time.Now().UTC(),
		}},
	}, nil
}
