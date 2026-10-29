package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/agentbaseline"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec/claude"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

type conformanceRecoveryGate struct {
	quiet, fail bool
	recovered   int
}

func (*conformanceRecoveryGate) HandoffStarted(context.Context, string) (bool, error) {
	return true, nil
}

func (g *conformanceRecoveryGate) HandoffQuiescent(context.Context, string) error {
	if !g.quiet {
		return errors.New("runtime objects remain")
	}
	return nil
}

func (g *conformanceRecoveryGate) Recover(context.Context, string, ward.HandoffSpec) (*ward.RecoveryResult, error) {
	g.recovered++
	if g.fail {
		return nil, errors.New("runtime cleanup unavailable")
	}
	g.quiet = true
	return &ward.RecoveryResult{Outcome: ward.RecoveryLoss}, nil
}

func TestAdapterConformanceRecoveryRetainsInputsUntilQuiescent(t *testing.T) {
	root := t.TempDir()
	seedRoot := filepath.Join(root, "seeds")
	seed := filepath.Join(seedRoot, "run")
	if err := os.MkdirAll(seed, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, "challenge"), []byte("keep until cleanup"), 0o600); err != nil {
		t.Fatal(err)
	}
	const id domain.InvocationID = "adapter-conformance-recovery"
	intent := adapterConformanceIntent{Version: 1, InvocationID: id, Spec: ward.HandoffSpec{
		RunID: claude.RunIDFor(id), Seed: ward.WorkspaceSeed{SourceDir: seed},
	}}
	path := filepath.Join(root, "adapter-conformance", string(id)+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(intent)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	gate := &conformanceRecoveryGate{fail: true}
	if err := retireAdapterConformance(t.Context(), gate, intent, path); err == nil {
		t.Fatal("cleanup failure passed")
	}
	for _, file := range []string{path, filepath.Join(seed, "challenge")} {
		if _, err := os.Stat(file); err != nil {
			t.Fatalf("lost recovery input %s: %v", file, err)
		}
	}
	st, err := store.Open(t.Context(), filepath.Join(root, "freeside.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := recordTestClaudeAdapterConformance(t.Context(), st, time.Now()); err != nil {
		t.Fatal(err)
	}
	gate.fail = false
	leaser := &adapterConformanceLeaser{store: st}
	if err := recoverAdapterConformance(t.Context(), st, gate, claudeDriverConfig{StateDir: root, SeedRoot: seedRoot}, leaser); err != nil {
		t.Fatal(err)
	}
	if gate.recovered != 2 {
		t.Fatalf("recovery calls = %d", gate.recovered)
	}
	for _, file := range []string{path, seed} {
		if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("cleanup retained %s: %v", file, err)
		}
	}
	adapter, err := agentbaseline.ClaudeWardAdapter()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
		record, _, err := tx.LatestAdapterConformance(t.Context(), adapter.Digest)
		if err == nil && record.Outcome != domain.ConformanceFailed {
			t.Fatal("recovered output retained an older proof")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestAdapterConformanceRecoveryRefusesSeedOutsideRoot(t *testing.T) {
	root, unrelated := t.TempDir(), t.TempDir()
	const id domain.InvocationID = "adapter-conformance-invalid"
	intent := adapterConformanceIntent{Version: 1, InvocationID: id, Spec: ward.HandoffSpec{
		RunID: claude.RunIDFor(id), Seed: ward.WorkspaceSeed{SourceDir: unrelated},
	}}
	dir := filepath.Join(root, "adapter-conformance")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(intent)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, string(id)+".json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	gate := &conformanceRecoveryGate{}
	if err := recoverAdapterConformance(t.Context(), nil, gate, claudeDriverConfig{StateDir: root, SeedRoot: filepath.Join(root, "seeds")}, nil); err == nil {
		t.Fatal("out-of-root seed accepted")
	}
	if gate.recovered != 0 {
		t.Fatal("invalid record reached runtime recovery")
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Fatal("unrelated directory was removed")
	}
}
