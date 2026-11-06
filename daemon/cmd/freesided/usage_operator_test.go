package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// scriptedUsageCollector answers each call with the next availability sent
// on answers, signaling the call on called first. A supported answer carries
// one window that resets a week after it was read.
type scriptedUsageCollector struct {
	mu      sync.Mutex
	asked   []domain.ClientEnrollmentID
	called  chan domain.ClientEnrollmentID
	answers chan domain.AvailabilityAnswer
}

func (c *scriptedUsageCollector) CollectUsage(
	ctx context.Context, request ward.UsageCollectionRequest,
) (ward.UsageCollection, error) {
	c.mu.Lock()
	c.asked = append(c.asked, request.Enrollment.ID)
	c.mu.Unlock()
	c.called <- request.Enrollment.ID
	var answer domain.AvailabilityAnswer
	select {
	case answer = <-c.answers:
	case <-ctx.Done():
		return ward.UsageCollection{}, ctx.Err()
	}
	collection := ward.UsageCollection{Availability: answer}
	if answer == domain.AvailabilitySupported {
		now := time.Now().UTC()
		collection.Windows = []domain.WindowReading{{
			Window: "five_hour", EnrollmentID: request.Enrollment.ID, Generation: request.Generation,
			Status: domain.WindowStatusAllowed, UsedPercent: new(42), ResetsAt: new(now.Add(7 * 24 * time.Hour)),
			ObservedAt: now,
		}}
	}
	return collection, nil
}

func (c *scriptedUsageCollector) calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.asked)
}

// usageClient is a paired device of a running daemon.
type usageClient struct {
	t     *testing.T
	api   string
	grant signet.PairingGrant
}

// do sends one authenticated request and decodes the response into result.
func (c usageClient) do(method, path string, body any, result any) int {
	c.t.Helper()
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			c.t.Fatal(err)
		}
	}
	request, err := http.NewRequestWithContext(c.t.Context(), method, c.api+path, bytes.NewReader(payload))
	if err != nil {
		c.t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+c.grant.DeviceToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		c.t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if result != nil && response.StatusCode == http.StatusOK {
		if err := json.NewDecoder(response.Body).Decode(result); err != nil {
			c.t.Fatal(err)
		}
	}
	return response.StatusCode
}

// command submits one usage command and returns its status and, when
// accepted, the revision it committed at.
func (c usageClient) command(id string, payload map[string]string) (int, int64) {
	c.t.Helper()
	var result struct {
		Record   struct{ Kind domain.CommandKind } `json:"record"`
		Revision int64                             `json:"revision"`
	}
	status := c.do(http.MethodPost, "/commands", map[string]any{
		"command_id": id, "device_id": c.grant.Device.Device.ID, "payload": payload,
	}, &result)
	if status == http.StatusOK && string(result.Record.Kind) != payload["kind"] {
		c.t.Fatalf("command %s answered a %s record", id, result.Record.Kind)
	}
	return status, result.Revision
}

func (c usageClient) bootstrap() signet.BootstrapSnapshot {
	c.t.Helper()
	var snapshot signet.BootstrapSnapshot
	if status := c.do(http.MethodGet, "/sync/bootstrap", nil, &snapshot); status != http.StatusOK {
		c.t.Fatalf("bootstrap status %d", status)
	}
	return snapshot
}

// pool returns pool's usage snapshot in the bootstrap, whose AsOfRevision
// is the revision that wrote the pool's record.
func (c usageClient) pool(pool domain.UsagePoolID) signet.UsagePoolSnapshot {
	c.t.Helper()
	for _, usage := range c.bootstrap().UsagePools {
		if usage.Pool.ID == pool {
			return usage
		}
	}
	c.t.Fatalf("bootstrap has no usage pool %s", pool)
	return signet.UsagePoolSnapshot{}
}

// awaitAttempt waits until pool's last attempt is newer than after, the
// collection a client sees land after its command.
func (c usageClient) awaitAttempt(pool domain.UsagePoolID, after time.Time) signet.UsagePoolSnapshot {
	c.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		snapshot := c.pool(pool)
		if attempt := snapshot.Pool.LastAttempt; attempt != nil && attempt.At.After(after) {
			return snapshot
		}
		if time.Now().After(deadline) {
			c.t.Fatalf("no collection landed for %s; last attempt %+v", pool, snapshot.Pool.LastAttempt)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func refreshPayload(pool string) map[string]string {
	return map[string]string{"kind": "refresh_usage", "pool": pool}
}

func reassignPayload(pool, enrollment string) map[string]string {
	return map[string]string{"kind": "reassign_collector", "pool": pool, "enrollment_id": enrollment}
}

// TestClientUsageCommandsDriveCollection runs the composed daemon: a paired
// client's usage commands start collections through the daemon's
// coordinator, and the result reaches the client through the bootstrap and
// the revision heartbeat.
func TestClientUsageCommandsDriveCollection(t *testing.T) {
	t.Parallel()
	collector := &scriptedUsageCollector{
		called: make(chan domain.ClientEnrollmentID, 8), answers: make(chan domain.AvailabilityAnswer),
	}
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	seedTemplateStore(t, dbPath)
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
	client := usageClient{
		t: t, api: h.readiness().APIURL, grant: pairingTestRedeem(t, h.readiness().APIURL, h.readiness().PairingCode),
	}
	staleAge := 2 * defaultDoctorInterval

	// A refresh answers before the collector does: the collector is still
	// blocked when the receipt arrives.
	status, refreshedAt := client.command("refresh-supported", refreshPayload("pool-a"))
	if status != http.StatusOK {
		t.Fatalf("refresh status %d", status)
	}
	if asked := <-collector.called; asked != "enroll-first" {
		t.Fatalf("refresh asked %s, want the default collector", asked)
	}
	collector.answers <- domain.AvailabilitySupported
	collected := client.awaitAttempt("pool-a", time.Time{})
	usage := collected.Pool
	if collected.AsOfRevision <= refreshedAt || usage.LastAttempt.Status != domain.CollectionAttemptSucceeded ||
		len(usage.Windows) != 1 {
		t.Fatalf("after the refresh at %d: record at %d, pool %+v", refreshedAt, collected.AsOfRevision, usage)
	}
	window := usage.Windows[0]
	if window.ObservedAt.IsZero() || window.StaleAt == nil || !window.StaleAt.Equal(window.ObservedAt.Add(staleAge)) {
		t.Fatalf("window observed %s stale %v, want stale %s later", window.ObservedAt, window.StaleAt, staleAge)
	}
	var heartbeat signet.ServerRevision
	if client.do(http.MethodGet, "/sync/revision", nil, &heartbeat); heartbeat.Revision < collected.AsOfRevision {
		t.Fatalf("heartbeat revision %d, before the record's %d", heartbeat.Revision, collected.AsOfRevision)
	}

	// An unsupported answer replaces the readings with none.
	supportedAt := usage.LastAttempt.At
	if status, _ := client.command("refresh-unsupported", refreshPayload("pool-a")); status != http.StatusOK {
		t.Fatalf("refresh status %d", status)
	}
	<-collector.called
	collector.answers <- domain.AvailabilityUnsupported
	usage = client.awaitAttempt("pool-a", supportedAt).Pool
	if usage.LastAttempt.Status != domain.CollectionAttemptUnsupported || len(usage.Windows) != 0 {
		t.Fatalf("after an unsupported answer: %+v", usage)
	}

	// A reassignment commits the new collector with its receipt, before the
	// new collector answers, and the next collection asks it.
	status, reassignedAt := client.command("reassign", reassignPayload("pool-a", "enroll-second"))
	if status != http.StatusOK {
		t.Fatalf("reassign status %d", status)
	}
	if asked := <-collector.called; asked != "enroll-second" {
		t.Fatalf("reassignment collected through %s, want the new collector", asked)
	}
	if reassigned := client.pool("pool-a"); reassigned.AsOfRevision != reassignedAt ||
		reassigned.Pool.Collector == nil || *reassigned.Pool.Collector != "enroll-second" {
		t.Fatalf("record at %d (receipt %d) collector %v, want enroll-second at the receipt's",
			reassigned.AsOfRevision, reassignedAt, reassigned.Pool.Collector)
	}
	collector.answers <- domain.AvailabilitySupported
	settled := client.awaitAttempt("pool-a", time.Time{})
	usage = settled.Pool
	if usage.LastAttempt.Status != domain.CollectionAttemptSucceeded || len(usage.Windows) != 1 ||
		usage.Windows[0].EnrollmentID != "enroll-second" {
		t.Fatalf("after the new collector's collection: %+v", usage)
	}

	// Nothing that committed nothing new starts a collection: a replay, an
	// unchanged collector, and every refusal. The replays here only check the
	// wire; the signet tests prove a replay never runs the operator.
	calls := collector.calls()
	for _, tc := range []struct {
		id      string
		payload map[string]string
		want    int
	}{
		{"refresh-supported", refreshPayload("pool-a"), http.StatusOK},
		{"reassign", reassignPayload("pool-a", "enroll-second"), http.StatusOK},
		{"reassign-unchanged", reassignPayload("pool-a", "enroll-second"), http.StatusOK},
		{"refresh-unknown", refreshPayload("pool-missing"), http.StatusNotFound},
		{"reassign-unknown", reassignPayload("pool-missing", "enroll-first"), http.StatusNotFound},
		{"reassign-foreign", reassignPayload("pool-a", "enroll-b"), http.StatusBadRequest},
		{"refresh-supported", refreshPayload("pool-b"), http.StatusBadRequest},
		{"refresh-supported", reassignPayload("pool-a", "enroll-first"), http.StatusBadRequest},
	} {
		if status, _ := client.command(tc.id, tc.payload); status != tc.want {
			t.Fatalf("%s %v status %d, want %d", tc.id, tc.payload, status, tc.want)
		}
	}
	// A collection starts its call at once, so one these commands started
	// would show within the wait.
	select {
	case asked := <-collector.called:
		t.Fatalf("a command that committed nothing new asked %s", asked)
	case <-time.After(300 * time.Millisecond):
	}
	if final := client.pool("pool-a"); collector.calls() != calls || final.AsOfRevision != settled.AsOfRevision ||
		final.Pool.Collector == nil || *final.Pool.Collector != "enroll-second" {
		t.Fatalf("calls %d (want %d), record at %d (want %d), collector %v",
			collector.calls(), calls, final.AsOfRevision, settled.AsOfRevision, final.Pool.Collector)
	}
}
