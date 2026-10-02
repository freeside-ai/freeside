package wardstore_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// failingAppendStore fails the generation append once, as a crash between
// the enrollment write and the append would leave things.
type failingAppendStore struct {
	ward.AuthAdoptionStore
	failures int
}

func (s *failingAppendStore) AppendGeneration(
	ctx context.Context, generation domain.EnrollmentGeneration, now time.Time,
) (domain.EnrollmentGeneration, error) {
	if s.failures > 0 {
		s.failures--
		return domain.EnrollmentGeneration{}, errors.New("append unavailable")
	}
	return s.AuthAdoptionStore.AppendGeneration(ctx, generation, now)
}

// TestClaudeAdoptionRetriesAfterAFailedAppendAndReleasesItsLease covers the
// adoption's own lease handling against the real store: a failed append
// leaves the enrollment recorded with no generation and no live lease, the
// rerun adopts over it, and a third run reuses the enrollment. The identity
// starts disabled, as the old harness seed step left it: adoption's
// first-enrollment write enables it.
func TestClaudeAdoptionRetriesAfterAFailedAppendAndReleasesItsLease(t *testing.T) {
	ctx := context.Background()
	st, adapters := openEnrollmentStore(t)
	identity := domain.AuthIdentity{
		ID: "claude-main", Provider: "claude", AuthStoreMutationLease: true,
		MaxParallelExecutions: 1,
		Interim:               domain.InterimClientFacts{AuthStoreVolume: "claude-main-auth", RefreshStrategy: domain.RefreshOnDemand},
	}
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordAuthIdentity(ctx, identity, enrollmentTestAt)
	}); err != nil {
		t.Fatal(err)
	}
	digest := domain.Digest(contentaddr.Sum([]byte("volume tree")))
	observed := 0
	// The identity's revisions are ordered by instant, so the clock advances.
	clock := enrollmentTestAt
	now := func() time.Time {
		clock = clock.Add(time.Second)
		return clock
	}
	adoption := &failingAppendStore{AuthAdoptionStore: adapters.Adoption, failures: 1}
	adopt := func() (ward.AuthAdoptionResult, error) {
		current, err := adapters.Leaser.GetIdentity(ctx, identity.ID)
		if err != nil {
			t.Fatal(err)
		}
		return ward.AdoptClaudeAuth(ctx, ward.ClaudeAuthAdoptionConfig{
			AuthAdoptionConfig: ward.AuthAdoptionConfig{
				Identity: current, EnrollmentID: "claude-main/claude_code",
				Route: "anthropic-subscription", CostOwner: "operator",
				Store: adoption, AuthStoreLeaser: adapters.Leaser,
				Now: now,
			},
			AccountBinding: "acct-fixture-0002",
			ObserveVolume: func(_ context.Context, volume string) (domain.Digest, error) {
				observed++
				if volume != "claude-main-auth" {
					t.Fatalf("observed volume %q", volume)
				}
				return digest, nil
			},
		})
	}
	leaseHeld := func() bool {
		lease, err := adapters.Leaser.Get(ctx, identity.ID)
		if errors.Is(err, store.ErrNotFound) {
			return false // release removes the row
		}
		if err != nil {
			t.Fatal(err)
		}
		return lease.HeldAt(clock)
	}

	if _, err := adopt(); err == nil || !strings.Contains(err.Error(), "append unavailable") {
		t.Fatalf("adoption with a failing append = %v", err)
	}
	if leaseHeld() {
		t.Fatal("a failed adoption left its lease live")
	}
	if _, _, found, err := adapters.Adoption.Enrolled(ctx, identity.ID, domain.HarnessClientClaudeCode); err != nil || found {
		t.Fatalf("enrolled after a failed append = %v, %v", found, err)
	}
	// The enable committed with the enrollment, so the rerun completes an
	// identity that is already enabled.
	if stored, err := adapters.Leaser.GetIdentity(ctx, identity.ID); err != nil || !stored.Enabled {
		t.Fatalf("identity after a failed append = %+v, %v", stored, err)
	}

	result, err := adopt()
	if err != nil || result.Reused || result.Generation.Ordinal != 1 ||
		result.Generation.StoreManifestDigest != digest || result.Generation.AuthStoreVolume != "claude-main-auth" {
		t.Fatalf("rerun = %+v, %v", result, err)
	}
	if leaseHeld() {
		t.Fatal("adoption left its lease live")
	}

	again, err := adopt()
	if err != nil || !again.Reused || again.Generation != result.Generation || observed != 2 {
		t.Fatalf("third run = %+v, %v (observed %d)", again, err, observed)
	}
	stored, err := adapters.Leaser.GetIdentity(ctx, identity.ID)
	if err != nil || !stored.SameFixedBindings(identity) || !stored.Enabled ||
		stored.AccountBinding != "acct-fixture-0002" || stored.CostOwner != "operator" {
		t.Fatalf("stored identity = %+v, %v", stored, err)
	}
}
