package signet_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

// catalogSync reads the bootstrap's catalog and the heartbeat's lineup
// revision from one service.
func catalogSync(t *testing.T, service *signet.Service) (signet.AgentCatalog, *domain.Digest) {
	t.Helper()
	bootstrap, err := service.Bootstrap(context.Background())
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	heartbeat, err := service.Revision(context.Background())
	if err != nil {
		t.Fatalf("Revision: %v", err)
	}
	return bootstrap.AgentCatalog, heartbeat.LineupRevision
}

// TestSyncServesComposedAgentCatalog serves the composed catalog on the
// bootstrap and its revision on the heartbeat, read at each call: the daemon
// binds the tree's half after the service is built, once the tree is
// loaded. The enrollments come from the store at the bootstrap's revision,
// each with its discovery record once one is recorded.
func TestSyncServesComposedAgentCatalog(t *testing.T) {
	t.Parallel()
	var current *signet.AgentCatalog
	st := storetest.Open(t, t.TempDir()+"/signet.db", store.Options{
		ApprovedCredentialModes: []domain.CredentialMode{domain.CredentialSubscriptionContained},
	})
	service := signet.NewService(st, signet.WithAgentCatalog(func() *signet.AgentCatalog { return current }))
	empty := signet.AgentCatalog{
		Enrollments: []signet.AgentCatalogEnrollment{}, Lineup: []signet.AgentCatalogLineupLine{},
	}
	if catalog, revision := catalogSync(t, service); !reflect.DeepEqual(catalog, empty) || revision != nil {
		t.Fatalf("before the tree loads: catalog = %+v, lineup_revision = %v, want empty lists and null both",
			catalog, revision)
	}

	rev := domain.Digest("sha256:aaaa")
	selection := domain.TaskLineSelection{EnrollmentID: "anthropic-a/claude_code", ModelID: "claude-opus-5-5", Effort: "high"}
	current = &signet.AgentCatalog{
		LineupRevision: &rev,
		Lineup:         []signet.AgentCatalogLineupLine{{Role: domain.RoleImplementer, Selection: selection}},
	}
	seedLineEnrollments(t, st)
	discovered := lineSelectionOf("codex")
	record := domain.DiscoveredModelCatalog{
		EnrollmentID: discovered.EnrollmentID, HarnessBuild: "codex-cli 0.50.0", ObservedAt: operatorAt,
		Models: []domain.DiscoveredModel{{ID: discovered.ModelID, DisplayName: "Codex", EffortLevels: []string{"high"}}},
	}
	if err := st.Write(t.Context(), func(tx *store.WriteTx) error {
		return tx.RecordDiscoveredModelCatalog(t.Context(), record)
	}); err != nil {
		t.Fatal(err)
	}
	catalog, revision := catalogSync(t, service)
	if !reflect.DeepEqual(catalog.Lineup, current.Lineup) ||
		catalog.LineupRevision == nil || *catalog.LineupRevision != rev {
		t.Fatalf("bootstrap catalog = %+v, want the tree half %+v", catalog, current)
	}
	if revision == nil || *revision != rev {
		t.Fatalf("heartbeat lineup_revision = %v, want %s", revision, rev)
	}
	if len(catalog.Enrollments) != len(testLineNames) {
		t.Fatalf("enrollments = %+v, want one per seeded enrollment", catalog.Enrollments)
	}
	for i, enrollment := range catalog.Enrollments {
		if i > 0 && catalog.Enrollments[i-1].EnrollmentID >= enrollment.EnrollmentID {
			t.Fatalf("enrollments out of id order at %d: %+v", i, catalog.Enrollments)
		}
		want := (*signet.AgentCatalogDiscovery)(nil)
		if enrollment.EnrollmentID == record.EnrollmentID {
			want = &signet.AgentCatalogDiscovery{
				HarnessBuild: record.HarnessBuild, ObservedAt: record.ObservedAt, Models: record.Models,
			}
		}
		if enrollment.HarnessClient != domain.HarnessClientCodexCLI || !reflect.DeepEqual(enrollment.Discovery, want) {
			t.Fatalf("enrollment %s = %+v, want discovery %+v", enrollment.EnrollmentID, enrollment, want)
		}
	}
}

// TestSyncServesCatalogWithoutTree is the fake-driver and scenario
// composition, which load no tree: the catalog is still present, with a
// null lineup revision and empty lists, and the heartbeat's revision is
// null.
func TestSyncServesCatalogWithoutTree(t *testing.T) {
	t.Parallel()
	service := signet.NewService(storetest.Open(t, t.TempDir()+"/signet.db", store.Options{}))
	if catalog, revision := catalogSync(t, service); catalog.LineupRevision != nil || revision != nil {
		t.Fatalf("catalog = %+v, lineup_revision = %v, want null both", catalog, revision)
	}
	bootstrap, err := service.Bootstrap(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	heartbeat, err := service.Revision(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	encodedBootstrap, err := json.Marshal(bootstrap)
	if err != nil {
		t.Fatal(err)
	}
	encodedHeartbeat, err := json.Marshal(heartbeat)
	if err != nil {
		t.Fatal(err)
	}
	const catalog = `"agent_catalog":{"lineup_revision":null,"enrollments":[],"lineup":[]}`
	if !strings.Contains(string(encodedBootstrap), catalog) {
		t.Fatalf("bootstrap = %s, want %s", encodedBootstrap, catalog)
	}
	// Required on the wire: an explicit null, never absent.
	if !strings.Contains(string(encodedHeartbeat), `"lineup_revision":null`) {
		t.Fatalf("lineup_revision is not an explicit null in %s", encodedHeartbeat)
	}
}
