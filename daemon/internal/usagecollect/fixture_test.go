package usagecollect

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

const (
	sharedPool = domain.UsagePoolID("pool-shared")
	otherPool  = domain.UsagePoolID("pool-other")
	// enrollFirst is sharedPool's default collector: recorded first.
	enrollFirst  = domain.ClientEnrollmentID("enroll-first")
	enrollSecond = domain.ClientEnrollmentID("enroll-second")
	enrollOther  = domain.ClientEnrollmentID("enroll-other")
	// enrollClaude is on its own pool under a harness client the fixture
	// registers no collector for.
	enrollClaude = domain.ClientEnrollmentID("enroll-claude")
	claudePool   = domain.UsagePoolID("pool-claude")
	manifest     = domain.Digest("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
)

// fakeCollector answers through answer and records every request. While gate
// is non-nil a call blocks until it closes.
type fakeCollector struct {
	mu     sync.Mutex
	calls  []ward.UsageCollectionRequest
	answer func(ward.UsageCollectionRequest) (ward.UsageCollection, error)
	gate   chan struct{}
}

func (f *fakeCollector) CollectUsage(
	ctx context.Context, request ward.UsageCollectionRequest,
) (ward.UsageCollection, error) {
	f.mu.Lock()
	f.calls = append(f.calls, request)
	gate, answer := f.gate, f.answer
	f.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return ward.UsageCollection{}, ctx.Err()
		}
	}
	return answer(request)
}

func (f *fakeCollector) set(answer func(ward.UsageCollectionRequest) (ward.UsageCollection, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answer = answer
}

func (f *fakeCollector) block() chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gate = make(chan struct{})
	return f.gate
}

func (f *fakeCollector) requests() []ward.UsageCollectionRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ward.UsageCollectionRequest(nil), f.calls...)
}

// reports answers supported with one allowed reading per window, observed now
// and stamped as asked.
func reports(windows ...string) func(ward.UsageCollectionRequest) (ward.UsageCollection, error) {
	return func(request ward.UsageCollectionRequest) (ward.UsageCollection, error) {
		result := ward.UsageCollection{Availability: domain.AvailabilitySupported}
		for _, window := range windows {
			result.Windows = append(result.Windows, reading(request, window))
		}
		return result, nil
	}
}

func reading(request ward.UsageCollectionRequest, window string) domain.WindowReading {
	return domain.WindowReading{
		Window: window, EnrollmentID: request.Enrollment.ID, Generation: request.Generation,
		Status: domain.WindowStatusAllowed, UsedPercent: new(10), ObservedAt: time.Now().UTC(),
	}
}

type fixture struct {
	t         *testing.T
	st        *store.Store
	collector *fakeCollector
	coord     *Coordinator
	// stop ends the running coordinator, as a daemon shutdown does.
	stop func()
}

// newFixture runs a coordinator over a store holding sharedPool's two
// enrollments, otherPool's one, and claudePool's one, each at generation one.
// Call it inside a synctest bubble: the coordinator's timers and clock are the
// bubble's.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "store.db"), store.Options{})
	f := &fixture{t: t, st: st, collector: &fakeCollector{answer: reports("five_hour")}}
	f.enroll("auth-first", enrollFirst, sharedPool, domain.HarnessClientCodexCLI)
	f.tick()
	f.enroll("auth-second", enrollSecond, sharedPool, domain.HarnessClientCodexCLI)
	f.enroll("auth-other", enrollOther, otherPool, domain.HarnessClientCodexCLI)
	f.enroll("auth-claude", enrollClaude, claudePool, domain.HarnessClientClaudeCode)
	f.tick()
	f.start()
	return f
}

// start runs a fresh coordinator until stop or the test's end.
func (f *fixture) start() {
	f.t.Helper()
	f.coord = New(f.st, map[domain.HarnessClientKind]ward.UsageCollector{
		domain.HarnessClientCodexCLI: f.collector,
	}, Config{RefreshAge: 24 * time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	f.coord.Start(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.coord.Wait()
	}()
	f.stop = sync.OnceFunc(func() {
		cancel()
		<-done
	})
	f.t.Cleanup(f.stop)
	synctest.Wait()
}

// tick advances the bubble's clock, so the next write is stamped after the
// last one.
func (f *fixture) tick() { time.Sleep(time.Minute) }

func (f *fixture) enroll(
	identity domain.AuthIdentityID, enrollment domain.ClientEnrollmentID, pool domain.UsagePoolID,
	harness domain.HarnessClientKind,
) {
	f.t.Helper()
	ctx := context.Background()
	at := time.Now().UTC()
	account := "acct-" + string(identity)
	record := domain.ClientEnrollment{
		ID: enrollment, AuthIdentityID: identity, HarnessClient: harness,
		Route: "openai_chatgpt_codex", AuthMethod: domain.AuthMethodOAuth,
		CredentialMode:  domain.CredentialSubscriptionContained,
		RefreshStrategy: domain.RefreshOnDemand, SupportsReadOnlyAuthSnapshot: true,
		AccountBinding: account,
	}
	provider := domain.AuthProviderOpenAI
	if harness == domain.HarnessClientClaudeCode {
		provider = domain.AuthProviderClaude
		record.Route, record.AuthMethod, record.RefreshStrategy = "anthropic-subscription", domain.AuthMethodSetupToken, domain.RefreshExternal
	}
	if err := f.st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.RecordAuthIdentity(ctx, domain.AuthIdentity{
			ID: identity, Provider: provider, AccountBinding: account, UsagePool: string(pool),
			AuthStoreMutationLease: true, Enabled: true,
		}, at); err != nil {
			return err
		}
		return tx.RecordClientEnrollment(ctx, record, at)
	}); err != nil {
		f.t.Fatalf("enroll %s: %v", enrollment, err)
	}
	f.appendGeneration(enrollment)
}

// appendGeneration moves enrollment to its next generation, as a re-enrollment
// does.
func (f *fixture) appendGeneration(enrollment domain.ClientEnrollmentID) {
	f.t.Helper()
	ctx := context.Background()
	at := time.Now().UTC()
	if err := f.st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		record, err := tx.GetClientEnrollment(ctx, enrollment)
		if err != nil {
			return err
		}
		binding := &domain.LeaseGenerationBinding{
			EnrollmentID: enrollment, AuthStoreVolume: "store-" + string(enrollment), StoreManifestDigest: manifest,
		}
		if current, err := tx.CurrentEnrollmentGeneration(ctx, enrollment); err == nil {
			binding.Generation = current.Ordinal
		}
		lease, err := tx.AcquireAuthStoreMutationLeaseBound(ctx, record.AuthIdentityID, "fixture", binding, at, at.Add(time.Minute))
		if err != nil {
			return err
		}
		entry := domain.EnrollmentGeneration{
			EnrollmentID: enrollment, AuthStoreVolume: binding.AuthStoreVolume, StoreManifestDigest: manifest,
			LeaseFence: lease.Fence, AccountBinding: record.AccountBinding, RecordedAt: at,
		}
		if record.AuthMethod == domain.AuthMethodOAuth {
			entry.TokenExpiry = new(at.Add(24 * time.Hour))
		}
		if _, err := tx.AppendEnrollmentGeneration(ctx, entry, at); err != nil {
			return err
		}
		return tx.ReleaseAuthStoreMutationLease(ctx, record.AuthIdentityID, "fixture", lease.Fence, at)
	}); err != nil {
		f.t.Fatalf("append generation of %s: %v", enrollment, err)
	}
}

func (f *fixture) record(pool domain.UsagePoolID) domain.PoolUsageObservation {
	f.t.Helper()
	var record domain.PoolUsageObservation
	if err := f.st.Read(context.Background(), func(tx *store.ReadTx) error {
		var err error
		record, err = tx.GetPoolUsageObservation(context.Background(), pool)
		return err
	}); err != nil {
		f.t.Fatalf("read record of %s: %v", pool, err)
	}
	return record
}

func (f *fixture) hasRecord(pool domain.UsagePoolID) bool {
	f.t.Helper()
	var held []domain.PoolUsageObservation
	if err := f.st.Read(context.Background(), func(tx *store.ReadTx) error {
		var err error
		held, err = tx.ListPoolUsageObservations(context.Background())
		return err
	}); err != nil {
		f.t.Fatal(err)
	}
	for _, record := range held {
		if record.Pool == pool {
			return true
		}
	}
	return false
}

func (f *fixture) refresh(pool domain.UsagePoolID) Report {
	f.t.Helper()
	report, err := f.coord.Refresh(context.Background(), pool)
	if err != nil {
		f.t.Fatalf("refresh %s: %v", pool, err)
	}
	return report
}

func windowNames(record domain.PoolUsageObservation) []string {
	names := []string{}
	for _, reading := range record.Windows {
		names = append(names, reading.Window)
	}
	return names
}
