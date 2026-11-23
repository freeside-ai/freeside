package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/daemoncompose"
	"github.com/freeside-ai/freeside/daemon/internal/operations"
)

func parseCordonFlags(t *testing.T, args ...string) daemoncompose.CordonBackendConfig {
	t.Helper()
	flags := flag.NewFlagSet("freesided", flag.ContinueOnError)
	config := cordonBackendFlags(flags)
	if err := flags.Parse(args); err != nil {
		t.Fatal(err)
	}
	return config()
}

func TestCordonFlagsUnsetConfigureNone(t *testing.T) {
	t.Parallel()
	got := parseCordonFlags(t)
	if got.AgentCLI != "" {
		t.Errorf("no flags configure a cordon: %+v", got)
	}
	if got.StallLimit != 10*time.Minute || got.EndGrace != 10*time.Second || got.EndProofTimeout != time.Minute {
		t.Errorf("default durations = %v, %v, %v", got.StallLimit, got.EndGrace, got.EndProofTimeout)
	}
}

func TestCordonFlagsConfigureTheBackend(t *testing.T) {
	t.Parallel()
	got := parseCordonFlags(t, "-cordon-agent-cli", "/opt/claude", "-cordon-agent-cli-sha256", "abc",
		"-cordon-projects", "/p", "-cordon-protected-root", "/a", "-cordon-protected-root", "/b",
		"-cordon-stall-limit", "1s")
	if got.AgentCLI != "/opt/claude" || got.AgentCLISHA256 != "abc" || got.Projects != "/p" ||
		!slices.Equal(got.ProtectedRoots, []string{"/a", "/b"}) || got.StallLimit != time.Second {
		t.Errorf("flags configure %+v", got)
	}
}

func TestDoctorCommandReportsTheCordonedClass(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	seedTemplateStore(t, dbPath)
	backend := "sha256:" + strings.Repeat("a", 64)
	cordoned := "sha256:" + strings.Repeat("c", 64)
	if err := runDoctorCommand(t.Context(), []string{
		"-db", dbPath, "-backend-configuration-digest", backend, "-cordon-configuration-digest", "c",
	}, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "-cordon-configuration-digest") {
		t.Fatalf("doctor with a malformed cordon digest = %v", err)
	}
	var out bytes.Buffer
	err := runDoctorCommand(t.Context(), []string{
		"-db", dbPath, "-backend-configuration-digest", backend, "-cordon-configuration-digest", cordoned,
	}, &out, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "unhealthy") {
		t.Fatalf("doctor = %v, want an unhealthy report", err)
	}
	var report operations.DoctorReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(report.Findings, func(f operations.DoctorFinding) bool { return f.Code == "cordoned_conformance" })
	if i < 0 || report.Findings[i].Healthy || !strings.Contains(report.Findings[i].Detail, cordoned) {
		t.Fatalf("findings = %+v, want an unhealthy cordoned_conformance naming %s", report.Findings, cordoned)
	}
}
