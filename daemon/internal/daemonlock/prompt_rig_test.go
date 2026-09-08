package daemonlock

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestRigBindsProtectedPromptNamespace(t *testing.T) {
	root := t.TempDir()
	cfg := rigTestConfig(t, filepath.Join(root, "state"), filepath.Join(root, "seed"))
	lease, err := AcquireRig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Close() })
	prefix := "freeside-handoff-c" + strings.Repeat("a", 31) + "-prompt"
	if _, err := BindRigRuntimeResources(cfg.StateRoot, lease.Token(),
		[]string{prefix + "-seed", prefix + "-check"}, []string{prefix}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := BindRigRuntimeResources(cfg.StateRoot, lease.Token(), []string{prefix + "-shell"}, nil, nil); err == nil {
		t.Fatal("unknown prompt role accepted")
	}
}
