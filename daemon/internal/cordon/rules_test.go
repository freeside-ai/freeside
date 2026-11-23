package cordon

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// testConfig is a valid configuration under a fresh, resolved directory.
func testConfig(t *testing.T) Config {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	daemonRoot := filepath.Join(root, "daemon")
	home := filepath.Join(root, "home")
	for _, dir := range []string{daemonRoot, home} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return Config{
		RunsRoot:          filepath.Join(daemonRoot, "cordon"),
		Home:              home,
		OwnerClaudeConfig: filepath.Join(home, ".config", "claude"),
		Projects:          filepath.Join(home, "Code"),
		ProtectedRoots:    []string{daemonRoot, filepath.Join(root, "other-instance")},
		UserTemp:          filepath.Join(root, "user", "T"),
		UserCache:         filepath.Join(root, "user", "C"),
		ReservedPorts:     []int{47100, 47101},
		AgentCLI:          filepath.Join(root, "bin", "agent"),
		Path:              []string{"/usr/bin", "/bin"},
		StallLimit:        time.Minute,
		EndGrace:          time.Second,
		EndProofTimeout:   10 * time.Second,
	}
}

func TestRenderRulesIsFixedPerConfiguration(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	if !bytes.Equal(RenderRules(cfg), RenderRules(cfg.clone())) {
		t.Fatal("two renders of one configuration differ")
	}
	for name, change := range map[string]func(*Config){
		"Home":           func(c *Config) { c.Home += "2" },
		"ProtectedRoots": func(c *Config) { c.ProtectedRoots = append(c.ProtectedRoots, "/zz") },
		"ReservedPorts":  func(c *Config) { c.ReservedPorts = append(c.ReservedPorts, 47102) },
	} {
		other := cfg.clone()
		change(&other)
		if RuleDigest(other) == RuleDigest(cfg) {
			t.Errorf("changing %s left the rule digest unchanged", name)
		}
	}
}

// TestRenderRulesOrdersTheReopeningRules pins the two ordering rules
// Seatbelt's last-match semantics need: the run root's allow follows every
// protected-root denial, and the tag rule is the last rule of all.
func TestRenderRulesOrdersTheReopeningRules(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	rules := string(RenderRules(cfg))
	allow := strings.Index(rules, `(subpath (param "RUN_ROOT"))`)
	if allow < 0 {
		t.Fatal("no run-root allow")
	}
	for _, root := range cfg.ProtectedRoots {
		denial := strings.LastIndex(rules, sbString(root))
		if denial < 0 || denial > allow {
			t.Fatalf("protected root %s is denied at %d, not before the run-root allow at %d", root, denial, allow)
		}
	}
	var lastRule string
	for line := range strings.SplitSeq(rules, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" && !strings.HasPrefix(trimmed, ";") {
			lastRule = trimmed
		}
	}
	if !strings.HasSuffix(rules, "(literal (param \"LAUNCH_TAG\")))\n") || lastRule != `(literal (param "LAUNCH_TAG")))` {
		t.Fatalf("the tag rule is not last; last rule line %q", lastRule)
	}
}

func TestSandboxParamsQuoteRegexInputs(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	cfg.Projects = "/Users/first.last/Code"
	params := sandboxParams(cfg, "/run", "/tag")
	if !slices.Contains(params, `PROJECTS_RE=/Users/first\.last/Code`) {
		t.Fatalf("PROJECTS_RE is not quoted: %q", params)
	}
}

func TestConfigValidateRefusals(t *testing.T) {
	t.Parallel()
	base := testConfig(t)
	link := filepath.Join(filepath.Dir(base.Home), "home-link")
	if err := os.Symlink(base.Home, link); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*Config){
		"relative path":      func(c *Config) { c.Home = "home" },
		"unclean path":       func(c *Config) { c.Projects = c.Home + "/./Code" },
		"symlinked path":     func(c *Config) { c.Home = link },
		"symlinked ancestor": func(c *Config) { c.OwnerClaudeConfig = filepath.Join(link, "missing", "claude") },
		"no protected root":  func(c *Config) { c.ProtectedRoots = nil },
		"unsorted roots":     func(c *Config) { slices.Reverse(c.ProtectedRoots) },
		"root inside runs":   func(c *Config) { c.ProtectedRoots = append(c.ProtectedRoots[:1], c.RunsRoot+"/x") },
		"runs root unprotected": func(c *Config) {
			c.ProtectedRoots = []string{filepath.Join(filepath.Dir(c.Home), "unrelated")}
		},
		"relative PATH entry": func(c *Config) { c.Path = []string{"bin"} },
		"no stall limit":      func(c *Config) { c.StallLimit = 0 },
		"unsorted ports":      func(c *Config) { slices.Reverse(c.ReservedPorts) },
		"port out of range":   func(c *Config) { c.ReservedPorts = []int{0} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := base.clone()
			mutate(&cfg)
			if err := cfg.validate(); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("validate = %v, want ErrInvalidConfig", err)
			}
		})
	}
	if err := base.validate(); err != nil {
		t.Fatalf("base configuration: %v", err)
	}
}
