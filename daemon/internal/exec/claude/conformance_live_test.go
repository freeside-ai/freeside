package claude

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/agentbaseline"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
	"github.com/freeside-ai/freeside/daemon/internal/wardstore"
)

type conformanceLiveScanner struct{ token []byte }

func (s conformanceLiveScanner) Scan(ctx context.Context, dir string) error {
	return filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		body, err := os.ReadFile(path) //nolint:gosec // gate-verified export in private test directory
		if err != nil {
			return err
		}
		if bytes.Contains(body, s.token) {
			return fmt.Errorf("%w: setup token appeared in export", ward.ErrOutputScanRefused)
		}
		return nil
	})
}

// This test makes one provider-backed conformance launch. Enrolling its
// disposable credential volume first also makes the normal enrollment probe.
func TestLiveClaudeAdapterConformance(t *testing.T) {
	if os.Getenv("FREESIDE_WARD_LIVE_TEST") != "1" {
		t.Skip("set FREESIDE_WARD_LIVE_TEST=1, FREESIDE_WARD_CLAUDE_AGENT_IMAGE, FREESIDE_WARD_EXPORTER_IMAGE, and CLAUDE_CODE_OAUTH_TOKEN; see daemon/README.md")
	}
	image, exporter := os.Getenv("FREESIDE_WARD_CLAUDE_AGENT_IMAGE"), os.Getenv("FREESIDE_WARD_EXPORTER_IMAGE")
	if !strings.Contains(image, "@sha256:") || !strings.Contains(exporter, "@sha256:") {
		t.Fatal("both live-test images must be digest pinned")
	}
	token, err := ward.NewSetupToken([]byte(os.Getenv(tokenEnv)))
	if err != nil {
		t.Fatal("CLAUDE_CODE_OAUTH_TOKEN must contain a setup token")
	}
	bin, err := exec.LookPath("container")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	runtime := ward.NewCLIRuntime(bin)
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	name := "freeside-adapter-live-" + hex.EncodeToString(nonce[:])
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	adapters, err := wardstore.New(st)
	if err != nil {
		t.Fatal(err)
	}
	enrolled, err := ward.EnrollClaudeSetupToken(ctx, ward.ClaudeAuthEnrollmentConfig{
		Token: token, AuthIdentityID: "live-claude", EnrollmentID: "live-claude/claude_code",
		Route: "claude-direct", CostOwner: "live-test", AccountBinding: "operator-live-test",
		Volume: name, Runtime: runtime, AgentImage: image, ExporterImage: exporter,
		Store: adapters.Claude, AuthStoreLeaser: adapters.Leaser,
	})
	if err != nil {
		t.Fatal(err)
	}
	before, err := runtime.InspectVolume(ctx, name)
	if err != nil || !before.LabelsObserved || before.CreationDate == "" {
		t.Fatalf("cannot establish credential-volume ownership: %s", name)
	}
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), time.Minute)
		defer done()
		now, err := runtime.InspectVolume(cleanup, name)
		if err != nil || !now.LabelsObserved || now.CreationDate != before.CreationDate || !slices.Equal(now.Labels, before.Labels) {
			t.Errorf("credential volume ownership changed; retained %s", name)
			return
		}
		if err := runtime.DeleteVolume(cleanup, name); err != nil {
			t.Errorf("remove test credential volume %s: %v", name, err)
		}
	})
	if enrolled.AuthStoreVolume != name {
		t.Fatal("enrollment volume differs")
	}
	seedRoot := t.TempDir()
	backend, err := ward.New(runtime, ward.Config{
		AgentImage: image, ExporterImage: exporter, ExporterCommand: export.HelperCommand(),
		SeedRoot: seedRoot, ExportRoot: t.TempDir(), ProviderEndpoints: []string{"api.anthropic.com:443"},
		Scanner: conformanceLiveScanner{token.Reveal()}, AuthStoreLeaser: adapters.Leaser, HandoffTimeout: 3 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	seed, err := os.MkdirTemp(seedRoot, "task-")
	if err != nil {
		t.Fatal(err)
	}
	task, err := NewConformanceTask(ctx, seed, domain.BaseRevision{
		Repo: "freeside-ai/conformance", RepositoryID: 1, BaseRef: "refs/heads/main",
	})
	if err != nil {
		t.Fatal(err)
	}
	id := domain.InvocationID(name)
	spec, err := ConformanceHandoff(ctx, ConformanceInput{
		Task: task, InvocationID: id, Image: domain.ImageRef(image), Identity: "live-claude", Volumes: adapters.Leaser,
		RouteModelID: "claude-sonnet-4-6", NativeEffort: "low",
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := backend.Handoff(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(result.ExportDir) }()
	observed, err := JudgeConformance(result, task, id, []string{"api.anthropic.com:443"})
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := agentbaseline.ClaudeWardAdapter()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(observed, adapter.LaunchCapabilities) {
		// Report schema facts only, never transcript content or credentials.
		for _, entry := range result.Evidence.Entries {
			if entry.Label != "agent-transcript" {
				continue
			}
			body, readErr := conformanceBlob(result.ExportDir, export.EvidenceBlobsDirname, entry.Digest, entry.Size)
			if readErr != nil {
				t.Fatal(readErr)
			}
			for _, line := range bytes.Split(body, []byte("\n")) {
				var record map[string]json.RawMessage
				if json.Unmarshal(line, &record) != nil || string(record["type"]) != `"system"` || string(record["subtype"]) != `"init"` {
					continue
				}
				var keys []string
				for key := range record {
					keys = append(keys, key)
				}
				slices.Sort(keys)
				var session string
				_ = json.Unmarshal(record["session_id"], &session)
				var servers, plugins []json.RawMessage
				serverErr, pluginErr := json.Unmarshal(record["mcp_servers"], &servers), json.Unmarshal(record["plugins"], &plugins)
				t.Logf("init keys=%v; session matches=%t; MCP array=%t count=%d; plugin array=%t count=%d", keys, session == sessionIDFor(id), serverErr == nil && servers != nil, len(servers), pluginErr == nil && plugins != nil, len(plugins))
			}
		}
		t.Fatalf("observed %v, require %v", observed, adapter.LaunchCapabilities)
	}
}
