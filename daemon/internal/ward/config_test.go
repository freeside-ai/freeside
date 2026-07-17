package ward

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestConfigDefaults(t *testing.T) {
	cfg := Config{}.withDefaults()
	if cfg.WorkspaceTarget != "/workspace" || cfg.HandoffDir != "/handoff" || cfg.ProofPath != "/handoff-proof.txt" {
		t.Errorf("path defaults = %q %q %q, want /workspace /handoff /handoff-proof.txt",
			cfg.WorkspaceTarget, cfg.HandoffDir, cfg.ProofPath)
	}
	if cfg.WriterStopTimeout == 0 || cfg.ExporterTimeout == 0 || cfg.PollInterval == 0 || cfg.Sleep == nil {
		t.Error("timing defaults not filled")
	}
	if cfg.MaxExportBytes == 0 || cfg.MaxArchiveBytes == 0 || cfg.MaxExportEntries == 0 || cfg.MaxManifestBytes == 0 {
		t.Error("resource-limit defaults not filled")
	}
}

func TestConfigValidate(t *testing.T) {
	if err := testConfig().validate(); err != nil {
		t.Fatalf("valid fixture: validate() = %v, want nil", err)
	}

	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{"missing exporter image", func(c *Config) { c.ExporterImage = "" }},
		{"tag-only exporter image", func(c *Config) { c.ExporterImage = "example.test/exporter:latest" }},
		{"short exporter digest", func(c *Config) { c.ExporterImage = "example.test/exporter@sha256:abc" }},
		{"missing exporter command", func(c *Config) { c.ExporterCommand = nil }},
		{"relative workspace target", func(c *Config) { c.WorkspaceTarget = "workspace" }},
		{"relative handoff dir", func(c *Config) { c.HandoffDir = "handoff" }},
		{"relative proof path", func(c *Config) { c.ProofPath = "handoff-proof.txt" }},
		{"nil scanner", func(c *Config) { c.Scanner = nil }},
		{"proof inside workspace", func(c *Config) { c.ProofPath = c.WorkspaceTarget + "/proof.txt" }},
		{"handoff inside workspace", func(c *Config) { c.HandoffDir = c.WorkspaceTarget + "/out" }},
		{"proof equals handoff", func(c *Config) { c.ProofPath = c.HandoffDir }},
		{"workspace inside handoff", func(c *Config) { c.WorkspaceTarget = c.HandoffDir + "/ws" }},
		{"comma in workspace target", func(c *Config) { c.WorkspaceTarget = "/workspace,type=bind" }},
		{"negative writer timeout", func(c *Config) { c.WriterStopTimeout = -time.Second }},
		{"negative exporter timeout", func(c *Config) { c.ExporterTimeout = -time.Second }},
		{"negative poll interval", func(c *Config) { c.PollInterval = -time.Millisecond }},
		{"negative teardown timeout", func(c *Config) { c.TeardownTimeout = -time.Second }},
		{"negative max export bytes", func(c *Config) { c.MaxExportBytes = -1 }},
		{"negative max archive bytes", func(c *Config) { c.MaxArchiveBytes = -1 }},
		{"negative max export entries", func(c *Config) { c.MaxExportEntries = -1 }},
		{"negative max manifest bytes", func(c *Config) { c.MaxManifestBytes = -1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig()
			tc.mutate(&cfg)
			if err := cfg.validate(); !errors.Is(err, ErrInvalidConfig) {
				t.Errorf("validate() = %v, want ErrInvalidConfig", err)
			}
		})
	}
}

func TestSleepContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepContext(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("sleepContext on cancelled ctx = %v, want context.Canceled", err)
	}
	if err := sleepContext(context.Background(), time.Nanosecond); err != nil {
		t.Errorf("sleepContext(1ns) = %v, want nil", err)
	}
}
