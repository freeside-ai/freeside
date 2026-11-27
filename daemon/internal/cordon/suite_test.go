package cordon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// The process that runs these tests may hold an approval itself (a
// terminal often holds accessibility), which would refuse every launch, and
// off macOS with cgo the reader always refuses. The approval check has its
// own tests, which give each backend the facts they need.
func init() {
	hostFactsReader = func() (HostFacts, error) { return HostFacts{}, nil }
}

type fakeRecorder struct {
	mu       sync.Mutex
	outcomes []domain.ConformanceOutcome
	fail     error
}

func (r *fakeRecorder) RecordBackendConformance(_ context.Context, record domain.BackendConformance) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail != nil {
		return r.fail
	}
	r.outcomes = append(r.outcomes, record.Outcome)
	return nil
}

func (r *fakeRecorder) recorded() []domain.ConformanceOutcome {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.outcomes)
}

// pinnedCLI writes a stand-in agent CLI and pins cfg to it.
func pinnedCLI(t *testing.T, cfg *Config) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(cfg.AgentCLI), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.AgentCLI, []byte("the agent CLI"), 0o600); err != nil {
		t.Fatal(err)
	}
	digest, err := fileSHA256(cfg.AgentCLI)
	if err != nil {
		t.Fatal(err)
	}
	cfg.AgentCLISHA256 = digest
}

// testSuite is a suite over b whose daemon environment and arguments are
// clean and whose managed settings are absent, so only what a test changes
// can fail a precondition.
func testSuite(t *testing.T, b *Backend, opts ...SuiteOption) *Suite {
	t.Helper()
	s, err := NewSuite(b, opts...)
	if err != nil {
		t.Fatal(err)
	}
	s.environ = func() []string { return []string{"PATH=/usr/bin:/bin", "HOME=/Users/owner"} }
	s.args = []string{"freesided", "prod"}
	s.managedSettings = filepath.Join(t.TempDir(), "managed-settings.json")
	return s
}

func TestPreconditionsRefuse(t *testing.T) {
	t.Parallel()
	const token = "ghp_" + "0123456789abcdefghijklmnopqrstuvwxyzAB"
	cases := map[string]struct {
		change    func(t *testing.T, s *Suite)
		wantCheck string
	}{
		"a secret-named variable": {
			change:    func(_ *testing.T, s *Suite) { s.environ = func() []string { return []string{"GH_TOKEN=" + token} } },
			wantCheck: "daemon environment",
		},
		"a credential under another name": {
			change:    func(_ *testing.T, s *Suite) { s.environ = func() []string { return []string{"NOTE=" + token} } },
			wantCheck: "daemon environment",
		},
		"a secret-named argument": {
			change:    func(_ *testing.T, s *Suite) { s.args = []string{"freesided", "-api-key=k3y"} },
			wantCheck: "daemon arguments",
		},
		"a credential argument": {
			change:    func(_ *testing.T, s *Suite) { s.args = []string{"freesided", "-token", token} },
			wantCheck: "daemon arguments",
		},
		"managed settings that set the sandbox": {
			change: func(t *testing.T, s *Suite) {
				if err := os.WriteFile(s.managedSettings, []byte(`{"sandbox":{"enabled":true}}`), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			wantCheck: "agent CLI sandbox",
		},
		"unreadable managed settings": {
			change: func(t *testing.T, s *Suite) {
				if err := os.WriteFile(s.managedSettings, []byte(`{`), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			wantCheck: "agent CLI sandbox",
		},
		"a CLI off its pin": {
			change: func(t *testing.T, s *Suite) {
				if err := os.WriteFile(s.b.cfg.AgentCLI, []byte("another build"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			wantCheck: "agent CLI pin",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := testConfig(t)
			pinnedCLI(t, &cfg)
			b, err := New(cfg, &fakeSeeder{})
			if err != nil {
				t.Fatal(err)
			}
			s := testSuite(t, b)
			if err := s.preconditions(); err != nil {
				t.Fatalf("clean preconditions: %v", err)
			}
			c.change(t, s)
			err = s.preconditions()
			var f *ConformanceFailure
			if !errors.As(err, &f) || f.Check != c.wantCheck {
				t.Fatalf("preconditions = %v, want a %q failure", err, c.wantCheck)
			}
			if strings.Contains(err.Error(), token) {
				t.Fatalf("the failure prints the credential: %v", err)
			}
		})
	}
}

// TestPreconditionsAcceptWhereACredentialLives shows a secret-named flag
// or variable whose value is a path passes: it names where a credential
// lives, not the credential.
func TestPreconditionsAcceptWhereACredentialLives(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	pinnedCLI(t, &cfg)
	b, err := New(cfg, &fakeSeeder{})
	if err != nil {
		t.Fatal(err)
	}
	s := testSuite(t, b)
	s.args = []string{"freesided", "prod", "-publication-credentials-dir=/var/freeside/app", "-rig-token-file=/var/freeside/rig"}
	s.environ = func() []string { return []string{"PATH=/usr/bin:/bin", "FREESIDE_TOKEN_FILE=/var/freeside/token"} }
	if err := s.preconditions(); err != nil {
		t.Fatalf("preconditions = %v, want a path accepted", err)
	}
}

func TestEnvironmentFailureNamesOnlyTheVariable(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	pinnedCLI(t, &cfg)
	b, err := New(cfg, &fakeSeeder{})
	if err != nil {
		t.Fatal(err)
	}
	s := testSuite(t, b)
	s.environ = func() []string { return []string{"ANTHROPIC_API_KEY=sk-not-printed", "PATH=/bin"} }
	err = s.preconditions()
	if err == nil || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") || strings.Contains(err.Error(), "sk-not-printed") {
		t.Fatalf("preconditions = %v", err)
	}
}

// TestFullRecordsAFailedPassAndDeclaresNothing shows a failing pass
// supersedes the standing proof, records its failure, and leaves the class
// undeclared.
func TestFullRecordsAFailedPassAndDeclaresNothing(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	pinnedCLI(t, &cfg)
	cfg.AgentCLISHA256 = strings.Repeat("0", 64)
	b, err := New(cfg, &fakeSeeder{})
	if err != nil {
		t.Fatal(err)
	}
	rec := &fakeRecorder{}
	s := testSuite(t, b, WithConformanceRecorder(rec))
	err = s.Full(t.Context())
	var f *ConformanceFailure
	if !errors.As(err, &f) || f.Check != "agent CLI pin" || errors.Is(err, ErrConformanceRecord) {
		t.Fatalf("Full = %v", err)
	}
	if got, want := rec.recorded(), []domain.ConformanceOutcome{domain.ConformanceSuperseded, domain.ConformanceFailed}; !slices.Equal(got, want) {
		t.Fatalf("recorded %v, want %v", got, want)
	}
	if declares(b) {
		t.Fatal("a failed pass declares the class")
	}
}

func TestFullWrapsARecordFailure(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	pinnedCLI(t, &cfg)
	b, err := New(cfg, &fakeSeeder{})
	if err != nil {
		t.Fatal(err)
	}
	s := testSuite(t, b, WithConformanceRecorder(&fakeRecorder{fail: errors.New("disk full")}))
	if err := s.Full(t.Context()); !errors.Is(err, ErrConformanceRecord) {
		t.Fatalf("Full = %v, want ErrConformanceRecord", err)
	}
	if declares(b) {
		t.Fatal("a pass whose marker failed declares the class")
	}
}

func TestProbeBeforeJobRefusesAnUndeclaredBackend(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	pinnedCLI(t, &cfg)
	b, err := New(cfg, &fakeSeeder{})
	if err != nil {
		t.Fatal(err)
	}
	s := testSuite(t, b)
	var f *ConformanceFailure
	if err := s.ProbeBeforeJob(t.Context()); !errors.As(err, &f) || f.Check != "declaration" {
		t.Fatalf("ProbeBeforeJob = %v", err)
	}
}

func TestASecondSuiteIsRefused(t *testing.T) {
	t.Parallel()
	b, err := New(testConfig(t), &fakeSeeder{})
	if err != nil {
		t.Fatal(err)
	}
	testSuite(t, b)
	if _, err := NewSuite(b); err == nil {
		t.Fatal("a second suite attached")
	}
}

// TestRequireProofRefusesBeforeStartingAnything shows a launch that
// requires proof starts no process when nothing proves the class.
func TestRequireProofRefusesBeforeStartingAnything(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	pinnedCLI(t, &cfg)
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{host: newFakeHost(), script: func(_ processSpec, p *fakeProcess) { p.exit(0) }}
		b := fakeBackend(t, cfg, l)
		req := request("run-unproven")
		req.RequireProof = true
		if _, err := b.Launch(t.Context(), req); !errors.Is(err, ErrConformance) {
			t.Fatalf("Launch without a suite = %v", err)
		}
		testSuite(t, b)
		if _, err := b.Launch(t.Context(), req); !errors.Is(err, ErrConformance) {
			t.Fatalf("Launch with an undeclared backend = %v", err)
		}
		if len(l.specs) != 0 {
			t.Fatalf("a refused launch started %d processes", len(l.specs))
		}
	})
}

// TestSecretPathsAreTheRuleFilesHomePaths pins the suite's lists to the
// rule file, so a denial added to rules.sb can't go unprobed.
func TestSecretPathsAreTheRuleFilesHomePaths(t *testing.T) {
	t.Parallel()
	var inRules []string
	for _, m := range homePath.FindAllStringSubmatch(ruleTemplate, -1) {
		if !slices.Contains(inRules, m[1]) {
			inRules = append(inRules, m[1])
		}
	}
	// The global preferences and .claude rules are regexes, escaped in the
	// rule file.
	for i, p := range inRules {
		inRules[i] = strings.ReplaceAll(p, `\\`, "")
	}
	listed := append(slices.Clone(secretReadPaths), writeOnlyPaths...)
	slices.Sort(inRules)
	slices.Sort(listed)
	if !slices.Equal(inRules, listed) {
		t.Fatalf("rules.sb denies %q, the suite lists %q", inRules, listed)
	}
}

// preferenceDomain finds each preference domain rules.sb names.
var preferenceDomain = regexp.MustCompile(`\(preference-domain "([^"]+)"\)`)

// TestDeniedPreferenceDomainsAreTheRuleFilesDomains pins the suite's
// domains to the rule file, so a domain rule can't be added or dropped
// without the probe following.
func TestDeniedPreferenceDomainsAreTheRuleFilesDomains(t *testing.T) {
	t.Parallel()
	var inRules []string
	for _, m := range preferenceDomain.FindAllStringSubmatch(ruleTemplate, -1) {
		inRules = append(inRules, m[1])
	}
	listed := slices.Clone(deniedPreferenceDomains)
	slices.Sort(inRules)
	slices.Sort(listed)
	if len(inRules) == 0 || !slices.Equal(inRules, listed) {
		t.Fatalf("rules.sb names preference domains %q, the suite lists %q", inRules, listed)
	}
}

func TestOwnTargetRefusesAnotherJob(t *testing.T) {
	t.Parallel()
	for _, target := range []string{
		"gui/501/com.apple.Finder", "gui/501", "system/" + probeLabelPrefix + "x", "gui/501/x" + probeLabelPrefix,
	} {
		if err := ownTarget(target); err == nil {
			t.Errorf("ownTarget(%q) accepted", target)
		}
	}
	if err := ownTarget("gui/501/" + probeLabelPrefix + "abc"); err != nil {
		t.Fatal(err)
	}
}

// passingProbe is a fake probe that proves every check of its spec.
func passingProbe(spec processSpec, p *fakeProcess) {
	var probe probeSpec
	_ = json.Unmarshal([]byte(spec.argv[len(spec.argv)-1]), &probe)
	for _, check := range probe.checks() {
		_, _ = spec.stdout.Write([]byte("ok " + check + "\n"))
	}
	p.exit(0)
}

// TestProbeLaunchChecksEffectsBeforeItRemovesTheLaunch shows the effect
// checks run while the launch's directory, where the stand-ins leave their
// traces, still exists, and that the directory is gone afterwards. The
// probe shares the writer's launch path, supervisor included, but gets no
// token and no token descriptor, and its request names it a probe, so it
// never exports.
func TestProbeLaunchChecksEffectsBeforeItRemovesTheLaunch(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	synctest.Test(t, func(t *testing.T) {
		launcher := &fakeLauncher{host: newFakeHost(), script: passingProbe}
		b := fakeBackend(t, cfg, launcher)
		s := testSuite(t, b)
		var l layout
		err := s.probeLaunch(t.Context(), "N0NCE", func(launched layout) (probeSpec, error) {
			l = launched
			return probeSpec{Reads: []probeRead{{Path: "/absent"}}}, nil
		}, func(*probeStandIn) error {
			if _, err := os.Stat(l.ctl); err != nil {
				t.Errorf("the launch's directory is gone before the effects: %v", err)
			}
			_, exportErr := b.Export(t.Context(), exportRequest(filepath.Base(l.dir)))
			if !errors.Is(exportErr, ErrNotExportable) || errors.Is(exportErr, ErrCanceled) {
				t.Errorf("Export of the probe launch = %v, want ErrNotExportable and not ErrCanceled", exportErr)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("probeLaunch: %v", err)
		}
		if _, err := os.Lstat(filepath.Join(cfg.RunsRoot, "conformance-N0NCE")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the probe launch's directory is left: %v", err)
		}
		for _, spec := range launcher.specs {
			if spec.secret != nil || slices.ContainsFunc(spec.env, func(v string) bool {
				return strings.HasPrefix(v, "CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR=")
			}) {
				t.Fatalf("a probe launch process got a token or its descriptor: %q", spec.argv)
			}
		}
	})
}

// TestProbeLaunchRemovesOnlyWhatItClaimed shows a failed probe launch
// removes the directory it claimed, and never one that was already there.
func TestProbeLaunchRemovesOnlyWhatItClaimed(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	synctest.Test(t, func(t *testing.T) {
		b := fakeBackend(t, cfg, &fakeLauncher{host: newFakeHost(), script: passingProbe})
		s := testSuite(t, b)
		b.daemonDigest = strings.Repeat("0", 64) // the staged copy won't match
		build := func(layout) (probeSpec, error) { return probeSpec{Reads: []probeRead{{Path: "/absent"}}}, nil }
		if err := s.probeLaunch(t.Context(), "STAGE", build, nil); err == nil {
			t.Fatal("a probe launch whose copy differs ran")
		}
		if _, err := os.Lstat(filepath.Join(cfg.RunsRoot, "conformance-STAGE")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("a failed probe launch left its directory: %v", err)
		}
		existing := filepath.Join(cfg.RunsRoot, "conformance-TAKEN")
		if err := os.MkdirAll(filepath.Join(existing, "keep"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := s.probeLaunch(t.Context(), "TAKEN", build, nil); err == nil {
			t.Fatal("a probe launch reused an existing directory")
		}
		if _, err := os.Stat(filepath.Join(existing, "keep")); err != nil {
			t.Fatalf("a probe launch removed a directory it didn't claim: %v", err)
		}
	})
}

// TestReadTargetsProveTheOwnersClaudeConfigurationOnce covers the
// daemon's own configuration, whose owner Claude configuration is also one
// of the rule file's home paths and one of the home's .claude entries: the
// report proves each check once, and every .claude entry the home holds is
// probed but the CLI's atomic-write temporaries.
func TestReadTargetsProveTheOwnersClaudeConfigurationOnce(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	cfg.OwnerClaudeConfig = cfg.Home + "/.claude"
	if !slices.Contains(secretReadPaths, "/.claude") {
		t.Fatal("the rule file no longer denies ~/.claude; this test needs another shared path")
	}
	for _, dir := range []string{"/.claude", "/.claude-bnw", "/Xclaude-x"} {
		if err := os.Mkdir(cfg.Home+dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range []string{"/.claude.json", "/" + claudeConfigTemporaryPrefix + "1.ab"} {
		if err := os.WriteFile(cfg.Home+file, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	b, err := New(cfg, &fakeSeeder{})
	if err != nil {
		t.Fatal(err)
	}
	reads, err := testSuite(t, b).readTargets()
	if err != nil {
		t.Fatal(err)
	}
	checks := probeSpec{Reads: reads}.checks()
	if n := len(slices.Compact(slices.Sorted(slices.Values(checks)))); n != len(checks) {
		t.Fatalf("read checks repeat: %q", checks)
	}
	for _, path := range []string{cfg.OwnerClaudeConfig, cfg.Home + "/.claude-bnw", cfg.Home + "/.claude.json"} {
		if !slices.Contains(checks, checkRead(path)) {
			t.Fatalf("read checks %q leave out %s", checks, path)
		}
	}
	if slices.Contains(checks, checkRead(cfg.Home+"/Xclaude-x")) {
		t.Fatalf("read checks %q probe a home entry the rule file doesn't deny", checks)
	}
	// A temporary the owner's CLI may rename away before the launch opens it.
	if slices.Contains(checks, checkRead(cfg.Home+"/"+claudeConfigTemporaryPrefix+"1.ab")) {
		t.Fatalf("read checks %q probe the CLI's atomic-write temporary", checks)
	}
}

// TestCreateTargetsProveTheClaudePrefixOnAnyHome shows the create probe
// tries a new ~/.claude-<nonce> directory, so the rule file's .claude
// prefix is proved on a home that holds no second Claude configuration.
func TestCreateTargetsProveTheClaudePrefixOnAnyHome(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	b, err := New(cfg, &fakeSeeder{})
	if err != nil {
		t.Fatal(err)
	}
	creates, err := testSuite(t, b).createTargets("n0nce")
	if err != nil {
		t.Fatal(err)
	}
	want := probeCreate{Path: cfg.Home + "/.claude-freeside-probe-n0nce", Dir: true}
	if !slices.Contains(creates, want) {
		t.Fatalf("creates %v leave out %v", creates, want)
	}
}
