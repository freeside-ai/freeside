package macverify_test

import (
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/macverify"
)

func testConfig() macverify.Config {
	return macverify.Config{
		ExecutorRoot:      "/var/freeside/macverify",
		SandboxProfile:    []byte("(version 1)(deny default)"),
		PacketFilterRules: []byte("block drop quick user slot1"),
		Slots:             []string{"slot1", "slot2"},
		Toolchain:         macverify.Toolchain{XcodeBuild: "17F42", Swift: "swiftlang-6.1.2.1.2"},
		PortBase:          macverify.DefaultPortBase,
	}
}

func newBackend(t *testing.T, cfg macverify.Config) *macverify.Backend {
	t.Helper()
	b, err := macverify.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return b
}

func passedRecord(t *testing.T, class domain.RunnerBackendClass, digest domain.Digest, caps domain.CapabilitySnapshot) domain.BackendConformance {
	t.Helper()
	record, err := domain.NewBackendConformance(domain.BackendConformanceInput{
		Backend:             class,
		Outcome:             domain.ConformancePassed,
		ConfigurationDigest: digest,
		Capabilities:        caps,
		ProvedAt:            time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("NewBackendConformance: %v", err)
	}
	record.Generation = 7
	return record
}

func TestNewRefusesInvalidConfig(t *testing.T) {
	t.Parallel()
	for name, mutate := range map[string]func(*macverify.Config){
		"empty root":          func(c *macverify.Config) { c.ExecutorRoot = "" },
		"relative root":       func(c *macverify.Config) { c.ExecutorRoot = "var/freeside" },
		"unclean root":        func(c *macverify.Config) { c.ExecutorRoot = "/var/freeside/../x" },
		"no profile":          func(c *macverify.Config) { c.SandboxProfile = nil },
		"no packet filter":    func(c *macverify.Config) { c.PacketFilterRules = nil },
		"no slots":            func(c *macverify.Config) { c.Slots = nil },
		"empty slot":          func(c *macverify.Config) { c.Slots = []string{""} },
		"unsorted slots":      func(c *macverify.Config) { c.Slots = []string{"slot2", "slot1"} },
		"duplicate slots":     func(c *macverify.Config) { c.Slots = []string{"slot1", "slot1"} },
		"no toolchain pinned": func(c *macverify.Config) { c.Toolchain = macverify.Toolchain{} },
		"no Xcode build":      func(c *macverify.Config) { c.Toolchain.XcodeBuild = "" },
		"Xcode version":       func(c *macverify.Config) { c.Toolchain.XcodeBuild = "Xcode 16.4" },
		"no Swift":            func(c *macverify.Config) { c.Toolchain.Swift = "" },
		"Swift version":       func(c *macverify.Config) { c.Toolchain.Swift = "6.1.2" },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testConfig()
			mutate(&cfg)
			if _, err := macverify.New(cfg); !errors.Is(err, macverify.ErrInvalidConfig) {
				t.Fatalf("New() = %v, want ErrInvalidConfig", err)
			}
		})
	}
}

// TestFreshBackendDeclaresNothing pins the empty base declaration: the
// executor proves nothing before its pass, so a fresh backend cannot clear
// an unattended minimum.
func TestFreshBackendDeclaresNothing(t *testing.T) {
	t.Parallel()
	b := newBackend(t, testConfig())
	if b.Name() != "credential_free_host_verification" {
		t.Errorf("Name() = %q", b.Name())
	}
	if caps := b.Capabilities(); len(caps) != 0 {
		t.Errorf("fresh Capabilities() = %v, want empty", caps)
	}
	if _, err := exec.CheckCapabilities(b, []exec.Capability{exec.CapCredentialFreeHostVerification}); !errors.Is(err, exec.ErrCapabilityRefused) {
		t.Errorf("fresh backend against the executor minimum = %v, want ErrCapabilityRefused", err)
	}
	var zero *macverify.Backend
	if zero.ConfigurationDigest() != "" || len(zero.Capabilities()) != 0 {
		t.Error("nil backend declares a digest or capabilities")
	}
}

func TestConfigurationDigestTracksEveryField(t *testing.T) {
	t.Parallel()
	base := newBackend(t, testConfig()).ConfigurationDigest()
	if base == "" || base != newBackend(t, testConfig()).ConfigurationDigest() {
		t.Fatalf("digest %q is not stable across identical configurations", base)
	}
	for name, mutate := range map[string]func(*macverify.Config){
		"executor root":       func(c *macverify.Config) { c.ExecutorRoot = "/var/freeside/other" },
		"sandbox profile":     func(c *macverify.Config) { c.SandboxProfile = []byte("(version 1)(allow default)") },
		"packet filter rules": func(c *macverify.Config) { c.PacketFilterRules = []byte("pass all") },
		"slots":               func(c *macverify.Config) { c.Slots = []string{"slot1"} },
		"Xcode build":         func(c *macverify.Config) { c.Toolchain.XcodeBuild = "17F43" },
		"Swift":               func(c *macverify.Config) { c.Toolchain.Swift = "swiftlang-6.1.2.1.3" },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testConfig()
			mutate(&cfg)
			if got := newBackend(t, cfg).ConfigurationDigest(); got == base {
				t.Errorf("changing %s left the digest at %s", name, got)
			}
		})
	}
}

// TestNewDetachesCallerConfig proves a caller cannot change the proven
// configuration after New by mutating the slices it passed in.
func TestNewDetachesCallerConfig(t *testing.T) {
	t.Parallel()
	cfg := testConfig()
	b := newBackend(t, cfg)
	before := b.ConfigurationDigest()
	cfg.SandboxProfile[0] = 'X'
	cfg.PacketFilterRules[0] = 'X'
	cfg.Slots[0] = "slot0"
	if after := b.ConfigurationDigest(); after != before {
		t.Errorf("digest moved from %s to %s after the caller's config changed", before, after)
	}
}

func TestCapabilitiesAreDetached(t *testing.T) {
	t.Parallel()
	b := newBackend(t, testConfig())
	b.Capabilities()[exec.CapCredentialFreeHostVerification] = struct{}{}
	if b.Capabilities().Has(exec.CapCredentialFreeHostVerification) {
		t.Error("mutating the returned set changed the declaration")
	}
}

func TestRestoreConformance(t *testing.T) {
	t.Parallel()
	declared := domain.NewCapabilitySnapshot(domain.CapCredentialFreeHostVerification)

	t.Run("refusals", func(t *testing.T) {
		b := newBackend(t, testConfig())
		digest := b.ConfigurationDigest()
		wardCeiling, _ := domain.ProvableCapabilities(domain.BackendFreshVMReadOnlyVolumeHandoff)
		failed := passedRecord(t, domain.BackendCredentialFreeHostVerification, digest, declared)
		failed.Outcome, failed.Capabilities = domain.ConformanceFailed, nil
		unpersisted := passedRecord(t, domain.BackendCredentialFreeHostVerification, digest, declared)
		unpersisted.Generation = 0
		for name, record := range map[string]domain.BackendConformance{
			"another class":    passedRecord(t, domain.BackendFreshVMReadOnlyVolumeHandoff, digest, wardCeiling),
			"another digest":   passedRecord(t, domain.BackendCredentialFreeHostVerification, "sha256:2222222222222222222222222222222222222222222222222222222222222222", declared),
			"failed outcome":   failed,
			"generation zero":  unpersisted,
			"malformed record": {},
		} {
			if err := b.RestoreConformance(record); err == nil {
				t.Errorf("%s: RestoreConformance accepted the record", name)
			}
			if len(b.Capabilities()) != 0 {
				t.Fatalf("%s: refused record published %v", name, b.Capabilities())
			}
		}
	})

	t.Run("passed record for this configuration", func(t *testing.T) {
		b := newBackend(t, testConfig())
		record := passedRecord(t, domain.BackendCredentialFreeHostVerification, b.ConfigurationDigest(), declared)
		err := b.RestoreConformance(record)
		if runtime.GOOS != "darwin" {
			// Off macOS no record can vouch for the confinements.
			if !errors.Is(err, macverify.ErrHostUnsupported) {
				t.Fatalf("RestoreConformance off macOS = %v, want ErrHostUnsupported", err)
			}
			if len(b.Capabilities()) != 0 {
				t.Fatalf("off-macOS backend declared %v", b.Capabilities())
			}
			return
		}
		if err != nil {
			t.Fatalf("RestoreConformance: %v", err)
		}
		if _, err := exec.CheckCapabilities(b, []exec.Capability{exec.CapCredentialFreeHostVerification}); err != nil {
			t.Fatalf("restored backend against the executor minimum = %v, want admitted", err)
		}
		if got := b.Capabilities().Snapshot(); len(got) != 1 {
			t.Errorf("restored declaration = %v, want exactly the executor capability", got)
		}
		if err := b.RestoreConformance(record); err == nil {
			t.Error("a second restore was accepted after the first")
		}
	})
}
