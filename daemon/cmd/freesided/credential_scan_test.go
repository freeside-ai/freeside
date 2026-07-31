package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
)

// TestClaudeStoreOptionsCarryTheSelectedModePolicy is the regression for a
// daemon whose own admissions the store would refuse: the persistence gate
// re-checks every recorded admission against the operator's policy, and an
// unset floor means "no policy configured", which fails closed.
func TestClaudeStoreOptionsCarryTheSelectedModePolicy(t *testing.T) {
	t.Parallel()
	tests := []struct {
		mode  domain.OperatingMode
		floor []domain.RunnerCapability
	}{
		{domain.ModeAttendedDev, []domain.RunnerCapability{
			domain.CapDetachableWorkspace,
			domain.CapPostExitExport,
			domain.CapReadOnlyRemount,
		}},
		{domain.ModeUnattended, []domain.RunnerCapability{
			domain.CapDetachableWorkspace,
			domain.CapPostExitExport,
			domain.CapReadOnlyRemount,
			domain.CapNetworklessExport,
			domain.CapEnforcedProviderEgress,
		}},
	}
	for _, test := range tests {
		test := test
		t.Run(string(test.mode), func(t *testing.T) {
			t.Parallel()
			cfg := config{
				DBPath: "/var/freeside/freeside.db",
				Claude: &claudeDriverConfig{OperatingMode: test.mode},
			}
			opts, err := cfg.storeOptions()
			if err != nil {
				t.Fatalf("storeOptions: %v", err)
			}
			floor, ok := opts.AdmissionFloors[test.mode]
			if !ok {
				t.Fatalf("claude mode configured no %s admission floor", test.mode)
			}
			if len(floor) != len(test.floor) {
				t.Fatalf("configured floor = %v, want %v", floor, test.floor)
			}
			for _, capability := range test.floor {
				if !floor.Has(capability) {
					t.Errorf("configured floor lacks %q", capability)
				}
			}
			if !slices.Equal(admissionFloor(test.mode), test.floor) {
				t.Errorf("engine floor = %v, want %v", admissionFloor(test.mode), test.floor)
			}
			if !slices.Contains(opts.ApprovedCredentialModes, domain.CredentialSubscriptionContained) {
				t.Errorf("approved credential modes = %v, want subscription_contained",
					opts.ApprovedCredentialModes)
			}
		})
	}
	if slices.Contains(attendedAdmissionFloor, exec.CapNetworklessExport) ||
		slices.Contains(attendedAdmissionFloor, exec.CapEnforcedProviderEgress) {
		t.Fatal("attended_dev floor includes unattended-only conformance capabilities")
	}

	// Fake mode keeps the walking skeleton's exact store policy.
	fakeOpts, err := (config{DBPath: "/var/freeside/freeside.db"}).storeOptions()
	if err != nil {
		t.Fatalf("fake storeOptions: %v", err)
	}
	if fakeOpts.AdmissionFloors != nil || fakeOpts.ApprovedCredentialModes != nil {
		t.Errorf("fake mode store options changed: %#v", fakeOpts)
	}
}

func TestCredentialScannerBlocksLeakedMaterial(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
	}{
		{"anthropic key", "config: sk-ant-api03-" + strings.Repeat("A", 40)},
		{"github token", "remote: https://ghp_" + strings.Repeat("b", 36) + "@github.com/x/y"},
		{"github fine-grained token", "token=github_pat_" + strings.Repeat("c", 24)},
		{"slack token", "SLACK_TOKEN=xoxb-" + strings.Repeat("d", 24)},
		{"pem private key", "-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n"},
		{"pem private key with numeric label", "-----BEGIN PKCS8 PRIVATE KEY-----\nabc\n"},
		{"aws access key", "AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE"},
		{"gcp service-account key id", `"private_key_id":"` + strings.Repeat("e", 40) + `"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			// Nested, so the walk is exercised rather than a flat listing.
			nested := filepath.Join(dir, "blobs", "sha256")
			if err := os.MkdirAll(nested, 0o700); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			leaked := filepath.Join(nested, "leaked.txt")
			if err := os.WriteFile(leaked, []byte(tc.body), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			err := (credentialScanner{}).Scan(context.Background(), dir)
			if err == nil {
				t.Fatal("scanner admitted an export carrying credential material")
			}
			// The error locates the leak without reproducing the secret: an
			// error a client or an attention item can carry must not become a
			// second copy of the credential.
			if strings.Contains(err.Error(), tc.body) {
				t.Fatalf("scanner error echoes the matched material: %v", err)
			}
			if !strings.Contains(err.Error(), "leaked.txt") {
				t.Fatalf("scanner error does not name the file: %v", err)
			}
		})
	}
}

// TestCredentialScannerReadsPastTheChunkBoundary is the regression for a
// truncating scanner: the export's largest member is the agent transcript,
// which is both the most likely to be large and the most likely to echo auth
// material, so material beyond the first chunk must still be caught.
func TestCredentialScannerReadsPastTheChunkBoundary(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	secret := "sk-ant-oat01-" + strings.Repeat("Z", 40)
	tests := []struct {
		name string
		body string
	}{
		{"far past the first chunk", strings.Repeat("x", 3*scanChunkBytes) + secret},
		// Straddling a chunk boundary: caught only because each pass carries
		// the previous chunk's tail forward.
		{"straddling a boundary", strings.Repeat("x", scanChunkBytes-len(secret)/2) + secret},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(dir, strings.ReplaceAll(tc.name, " ", "-")+".jsonl")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			matched, err := scanFile(context.Background(), path)
			if err != nil {
				t.Fatalf("scanFile: %v", err)
			}
			if matched == nil {
				t.Fatal("scanner reported a file carrying credential material as clean")
			}
		})
	}
}

func TestCredentialScannerAdmitsOrdinaryOutput(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	files := map[string]string{
		"manifest.json": `{"version":"freeside.export.manifest/v1","entries":[]}`,
		// Prose that mentions credentials without carrying any: the scanner
		// must not fire on vocabulary, or every honest run tripping it would
		// train the operator to ignore it.
		"README.md": "Set the API key in the keychain; never commit a private key.\n",
		"code.go":   "const tokenHeader = \"Authorization\"\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := (credentialScanner{}).Scan(context.Background(), dir); err != nil {
		t.Fatalf("scanner rejected ordinary output: %v", err)
	}
}

func TestClaudeDriverConfigRequiresExplicitBindings(t *testing.T) {
	t.Parallel()
	valid := claudeDriverConfig{ //nolint:gosec // G101: fixture paths and digests, no credential material
		AgentImage:         domain.ImageRef("ghcr.io/x/agent@sha256:" + strings.Repeat("a", 64)),
		ExporterImage:      "ghcr.io/x/exporter@sha256:" + strings.Repeat("b", 64),
		SeedRoot:           "/var/freeside/seeds",
		StateDir:           "/var/freeside/state",
		ProviderEndpoints:  []string{"api.anthropic.com:443"},
		PromptPackageFile:  "/Users/operator/prompt-package.md",
		VendorInstructions: "/Users/operator/CLAUDE.md",
		Repo:               "freeside-ai/candidate", RepositoryID: 42,
		BaseRef: "main", BaseSHA: strings.Repeat("d", 40),
		AuthIdentityID: "auth-claude-owner",
		AllowedPaths:   []string{"daemon/**", "docs/**"},
		StateRoot:      "/var/freeside/app", CredentialsDir: "/var/freeside/creds",
	}
	if err := valid.validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	// Every binding that lands in a durable admission record must be
	// operator-supplied: a default would record an unaudited value.
	tests := map[string]func(*claudeDriverConfig){
		"agent image":    func(c *claudeDriverConfig) { c.AgentImage = "" },
		"exporter image": func(c *claudeDriverConfig) { c.ExporterImage = "" },
		"seed root":      func(c *claudeDriverConfig) { c.SeedRoot = "" },
		"state dir":      func(c *claudeDriverConfig) { c.StateDir = "" },
		"endpoints":      func(c *claudeDriverConfig) { c.ProviderEndpoints = nil },
		"prompt package": func(c *claudeDriverConfig) { c.PromptPackageFile = "" },
		"instructions":   func(c *claudeDriverConfig) { c.VendorInstructions = "" },
		"repository id":  func(c *claudeDriverConfig) { c.RepositoryID = 0 },
		"base sha":       func(c *claudeDriverConfig) { c.BaseSHA = "" },
		"repository shape": func(c *claudeDriverConfig) {
			c.Repo = ".unsafe/repo"
		},
		"base ref shape": func(c *claudeDriverConfig) {
			c.BaseRef = "refs/heads/main"
		},
		"base sha shape": func(c *claudeDriverConfig) {
			c.BaseSHA = strings.Repeat("D", 40)
		},
		"auth identity":   func(c *claudeDriverConfig) { c.AuthIdentityID = "" },
		"credentials dir": func(c *claudeDriverConfig) { c.CredentialsDir = "" },
		"allowed paths":   func(c *claudeDriverConfig) { c.AllowedPaths = nil },
		// Match-everything equivalents do not name the paths unattended work
		// may rewrite, even when their spelling is not the literal "**".
		"match-everything allowlist": func(c *claudeDriverConfig) { c.AllowedPaths = []string{"**/*"} },
		"malformed allowlist":        func(c *claudeDriverConfig) { c.AllowedPaths = []string{"daemon/[abc"} },
	}
	for name, edit := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := valid
			edit(&cfg)
			if err := cfg.validate(); err == nil {
				t.Fatalf("config missing %s was accepted", name)
			}
		})
	}
}

func TestClaudeDriverConfigRejectsUnscopedAllowlistGlobs(t *testing.T) {
	t.Parallel()
	for _, pattern := range []string{
		"**", "**/*", "**/**", "*/**", "?*", "[a-z]*", "/daemon/**", "../daemon/**",
	} {
		t.Run(pattern, func(t *testing.T) {
			if explicitAllowedPaths([]string{pattern}) {
				t.Fatalf("explicitAllowedPaths(%q) = true", pattern)
			}
		})
	}
	for _, pattern := range []string{"daemon/**", "docs/*.md", "README.md"} {
		t.Run("accept "+pattern, func(t *testing.T) {
			if !explicitAllowedPaths([]string{pattern}) {
				t.Fatalf("explicitAllowedPaths(%q) = false", pattern)
			}
		})
	}
}
