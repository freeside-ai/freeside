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
func catalogSync(t *testing.T, service *signet.Service) (*signet.AgentCatalog, *domain.Digest) {
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
// binds the catalog after the service is built, once the tree is loaded.
func TestSyncServesComposedAgentCatalog(t *testing.T) {
	var current *signet.AgentCatalog
	service := signet.NewService(
		storetest.Open(t, t.TempDir()+"/signet.db", store.Options{}),
		signet.WithAgentCatalog(func() *signet.AgentCatalog { return current }),
	)
	if catalog, revision := catalogSync(t, service); catalog != nil || revision != nil {
		t.Fatalf("before the tree loads: catalog = %+v, lineup_revision = %v, want null both", catalog, revision)
	}

	current = &signet.AgentCatalog{
		LineupRevision: "sha256:aaaa",
		Agents: []signet.AgentCatalogEntry{{
			Name: "claude-a", EnrollmentID: "anthropic-a/claude_code",
			Model: "claude-opus-5-5", Effort: domain.EffortHigh,
		}},
		Lineup: []signet.AgentCatalogLineupLine{{Role: domain.RoleImplementer, Agent: "claude-a"}},
	}
	catalog, revision := catalogSync(t, service)
	if !reflect.DeepEqual(catalog, current) {
		t.Fatalf("bootstrap catalog = %+v, want %+v", catalog, current)
	}
	if revision == nil || *revision != current.LineupRevision {
		t.Fatalf("heartbeat lineup_revision = %v, want %s", revision, current.LineupRevision)
	}
}

// TestSyncServesNullCatalogWithoutTree is the fake-driver and scenario
// composition, which load no tree and supply no catalog.
func TestSyncServesNullCatalogWithoutTree(t *testing.T) {
	service := signet.NewService(storetest.Open(t, t.TempDir()+"/signet.db", store.Options{}))
	if catalog, revision := catalogSync(t, service); catalog != nil || revision != nil {
		t.Fatalf("catalog = %+v, lineup_revision = %v, want null both", catalog, revision)
	}
	// Both fields are required on the wire: an explicit null, never absent.
	bootstrap, err := service.Bootstrap(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	heartbeat, err := service.Revision(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for field, value := range map[string]any{"agent_catalog": bootstrap, "lineup_revision": heartbeat} {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(encoded), `"`+field+`":null`) {
			t.Fatalf("%s is not an explicit null in %s", field, encoded)
		}
	}
}
