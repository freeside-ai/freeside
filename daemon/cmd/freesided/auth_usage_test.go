package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
	"github.com/freeside-ai/freeside/daemon/internal/usagecollect"
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
	provider := "openai"
	if harness == domain.HarnessClientClaudeCode {
		provider = "anthropic"
		record.Route, record.AuthMethod, record.RefreshStrategy = "anthropic-subscription", domain.AuthMethodSetupToken, domain.RefreshExternal
	}
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
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

// seedUsagePools records pool-a's two enrollments, first and second in that
// order, pool-b's one, and pool-claude's one under a harness client with no
// collector.
func seedUsagePools(t *testing.T, st *store.Store) {
	t.Helper()
	at := time.Now().UTC().Add(-time.Hour)
	seedUsageEnrollment(t, st, "auth-first", "enroll-first", "pool-a", domain.HarnessClientCodexCLI, at)
	at = at.Add(time.Minute)
	seedUsageEnrollment(t, st, "auth-second", "enroll-second", "pool-a", domain.HarnessClientCodexCLI, at)
	seedUsageEnrollment(t, st, "auth-b", "enroll-b", "pool-b", domain.HarnessClientCodexCLI, at)
	seedUsageEnrollment(t, st, "auth-claude", "enroll-claude", "pool-claude", domain.HarnessClientClaudeCode, at)
}

func runAuthUsage(t *testing.T, ctx context.Context, args ...string) (string, error) {
	t.Helper()
	var stdout bytes.Buffer
	err := runAuthCommand(ctx, args, nil, &stdout, io.Discard)
	return stdout.String(), err
}

func readPoolCollector(t *testing.T, st *store.Store, pool domain.UsagePoolID) domain.ClientEnrollmentID {
	t.Helper()
	var collector domain.ClientEnrollmentID
	if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
		record, err := tx.GetPoolUsageObservation(t.Context(), pool)
		collector = record.Collector
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return collector
}

func TestAuthSetCollectorWorksWithoutTheDaemon(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	st := storetest.Open(t, dbPath, store.Options{})
	seedUsagePools(t, st)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	output, err := runAuthUsage(t, t.Context(),
		"set-collector", "-db", dbPath, "-pool", "pool-a", "-enrollment", "enroll-second")
	if err != nil {
		t.Fatal(err)
	}
	var result usagecollect.Reassignment
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("decode %q: %v", output, err)
	}
	if result != (usagecollect.Reassignment{
		Pool: "pool-a", PreviousCollector: "enroll-first", Collector: "enroll-second", Recorded: true,
	}) {
		t.Fatalf("result = %+v", result)
	}

	_, err = runAuthUsage(t, t.Context(),
		"set-collector", "-db", dbPath, "-pool", "pool-a", "-enrollment", "enroll-b")
	if !errors.Is(err, store.ErrEnrollmentNotOnPool) {
		t.Fatalf("an enrollment on another pool = %v, want ErrEnrollmentNotOnPool", err)
	}
	reopened := storetest.Open(t, dbPath, store.Options{})
	if got := readPoolCollector(t, reopened, "pool-a"); got != "enroll-second" {
		t.Fatalf("collector = %s, want the recorded reassignment", got)
	}
}

func TestAuthRefreshUsageNeedsTheDaemon(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	st := storetest.Open(t, dbPath, store.Options{})
	seedUsagePools(t, st)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	_, err := runAuthUsage(t, t.Context(), "refresh-usage", "-db", dbPath, "-pool", "pool-a")
	if err == nil || !strings.Contains(err.Error(), "needs the running daemon") {
		t.Fatalf("refresh without a daemon = %v", err)
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

func (c *blockingUsageCollector) asked() []domain.ClientEnrollmentID {
	c.mu.Lock()
	defer c.mu.Unlock()
	var asked []domain.ClientEnrollmentID
	for _, request := range c.requests {
		asked = append(asked, request.Enrollment.ID)
	}
	return asked
}

func TestAuthUsageCommandsUseTheRunningDaemon(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	collector := &blockingUsageCollector{called: make(chan struct{}, 8), release: make(chan struct{})}
	h, err := run(t.Context(), nil, config{
		Environment: environmentEphemeral, DBPath: dbPath, ListenAddr: "127.0.0.1:0",
		usageCollectors: map[domain.HarnessClientKind]ward.UsageCollector{domain.HarnessClientCodexCLI: collector},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	})
	seedUsagePools(t, h.store)
	ctx := context.WithValue(t.Context(), directStoreOpenerKey{}, directStoreOpener(
		func(context.Context, string, store.Options, storeOpenMode) (*store.Store, error) {
			return nil, errors.New("unexpected direct store open")
		}))

	// A refresh while a collection is in flight joins it: the collector
	// stays blocked, so a second collection would show as a second call.
	h.usage.RateLimitEvent("pool-a")
	<-collector.called
	joining, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	_, err = runAuthUsage(t, joining, "refresh-usage", "-db", dbPath, "-pool", "pool-a")
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("refresh during a blocked collection = %v, want it to wait out its deadline", err)
	}
	if asked := collector.asked(); len(asked) != 1 {
		t.Fatalf("collector asked %v, want one joined collection", asked)
	}
	close(collector.release)

	output, err := runAuthUsage(t, ctx, "refresh-usage", "-db", dbPath, "-pool", "pool-a")
	if err != nil {
		t.Fatalf("refresh = %v (%s)", err, output)
	}
	var report usagecollect.Report
	if err := json.Unmarshal([]byte(output), &report); err != nil {
		t.Fatalf("decode %q: %v", output, err)
	}
	if report.Outcome != usagecollect.OutcomeCollected || report.Observation == nil ||
		len(report.Windows) != 1 || report.Windows[0].Freshness != usagecollect.FreshnessFresh {
		t.Fatalf("report = %s", output)
	}

	if _, err := runAuthUsage(t, ctx, "refresh-usage", "-db", dbPath, "-pool", "pool-claude"); err == nil ||
		!strings.Contains(err.Error(), "no usage collector") {
		t.Fatalf("refresh without a collector = %v", err)
	}

	output, err = runAuthUsage(t, ctx,
		"set-collector", "-db", dbPath, "-pool", "pool-a", "-enrollment", "enroll-second")
	if err != nil || !strings.Contains(output, `"recorded":true`) {
		t.Fatalf("set-collector = %s, %v", output, err)
	}
	// The reassignment starts a collection under the new collector.
	for asked := collector.asked(); asked[len(asked)-1] != "enroll-second"; asked = collector.asked() {
		<-collector.called
	}
	if _, err := runAuthUsage(t, ctx,
		"set-collector", "-db", dbPath, "-pool", "pool-a", "-enrollment", "enroll-b",
	); err == nil || !strings.Contains(err.Error(), "not on the usage pool") {
		t.Fatalf("forwarded refusal = %v", err)
	}
	if got := readPoolCollector(t, h.store, "pool-a"); got != "enroll-second" {
		t.Fatalf("collector = %s", got)
	}
}
