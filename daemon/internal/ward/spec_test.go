package ward

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
)

// scannerFunc adapts a function to OutputScanner for test fixtures.
type scannerFunc func(ctx context.Context, dir string) error

func (f scannerFunc) Scan(ctx context.Context, dir string) error { return f(ctx, dir) }

// testConfig is the fixed valid fixture configuration; tests copy and mutate
// it. The all-zero digest marks it as inert fixture data.
func testConfig() Config {
	return Config{
		ProviderEndpoints: []string{"provider.example:443"},
		ExporterImage:     "example.test/exporter@sha256:" + strings.Repeat("0", 64),
		ExporterCommand:   export.HelperCommand(),
		Scanner:           scannerFunc(func(context.Context, string) error { return nil }),
	}.withDefaults()
}

func testHandoffSpec() HandoffSpec {
	return HandoffSpec{
		RunID:           "golden-run",
		WorkspaceSizeMB: 64,
		Seed:            WorkspaceSeed{Mode: SeedBlank},
		Agent: AgentSpec{
			Image:         "example.test/agent@sha256:" + strings.Repeat("1", 64),
			Command:       []string{"sh", "-c", "true"},
			Env:           []string{"AGENT_MODE=fixture"},
			EgressProfile: domain.EgressProviderOnly,
			CredentialMounts: []CredentialMount{
				{Volume: "provider-cred", Target: "/credentials"},
			},
		},
	}
}

func testOwnershipLabel() Label {
	return Label{Key: ownershipLabelKey, Value: "00000000000000000000000000000000"}
}

func TestHandoffSpecValidate(t *testing.T) {
	if err := testHandoffSpec().validate(); err != nil {
		t.Fatalf("valid fixture: validate() = %v, want nil", err)
	}

	cases := []struct {
		name   string
		mutate func(*HandoffSpec)
	}{
		{"empty run id", func(s *HandoffSpec) { s.RunID = "" }},
		{"uppercase run id", func(s *HandoffSpec) { s.RunID = "Golden-Run" }},
		{"run id with slash", func(s *HandoffSpec) { s.RunID = "a/b" }},
		{"run id too long", func(s *HandoffSpec) { s.RunID = strings.Repeat("a", 33) }},
		{"zero workspace size", func(s *HandoffSpec) { s.WorkspaceSizeMB = 0 }},
		{"missing agent image", func(s *HandoffSpec) { s.Agent.Image = "" }},
		{"unpinned agent image", func(s *HandoffSpec) { s.Agent.Image = "example.test/agent:latest" }},
		{"short agent digest", func(s *HandoffSpec) { s.Agent.Image = "example.test/agent@sha256:abc" }},
		{"missing agent command", func(s *HandoffSpec) { s.Agent.Command = nil }},
		{"missing egress profile", func(s *HandoffSpec) { s.Agent.EgressProfile = "" }},
		{"unenforceable wider egress profile", func(s *HandoffSpec) { s.Agent.EgressProfile = domain.EgressProviderWebRead }},
		// The seed is part of the spec's caller-error surface, so its rejections
		// must reach ErrInvalidHandoffSpec through HandoffSpec.validate too.
		{"unset seed mode", func(s *HandoffSpec) { s.Seed = WorkspaceSeed{} }},
		{"unknown seed mode", func(s *HandoffSpec) { s.Seed = WorkspaceSeed{Mode: SeedMode("copy")} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := testHandoffSpec()
			tc.mutate(&s)
			if err := s.validate(); !errors.Is(err, ErrInvalidHandoffSpec) {
				t.Errorf("validate() = %v, want ErrInvalidHandoffSpec", err)
			}
		})
	}
}

func TestNamesFor(t *testing.T) {
	n := namesFor("run-1")
	want := handoffNames{
		Workspace: "freeside-handoff-run-1-ws",
		Seeder:    "freeside-handoff-run-1-seeder",
		Observer:  "freeside-handoff-run-1-observer",
		Agent:     "freeside-handoff-run-1-agent",
		Exporter:  "freeside-handoff-run-1-exporter",
		Network:   "freeside-handoff-run-1-egress",
	}
	if n != want {
		t.Errorf("namesFor(run-1) = %+v, want %+v", n, want)
	}
	if got := WorkspaceRef("run-1"); got != want.Workspace {
		t.Errorf("WorkspaceRef(run-1) = %q, want %q", got, want.Workspace)
	}
}

// TestNamesForFitsRuntimeIDLimit pins every role name against the longest
// valid run ID: Apple container 1.1.0 refuses an ID over 64 bytes, and the
// networkless-export work already had to shorten a role prefix once for
// exactly this reason. "observer" is the longest suffix, so a new role longer
// than it must re-check this bound.
func TestNamesForFitsRuntimeIDLimit(t *testing.T) {
	const runtimeIDLimit = 64
	longest := "a" + strings.Repeat("b", 31) // the max runIDPattern admits
	if !runIDPattern.MatchString(longest) {
		t.Fatalf("fixture run ID %q is not a valid run ID", longest)
	}
	n := namesFor(longest)
	for role, name := range map[string]string{
		"workspace": n.Workspace, "seeder": n.Seeder, "observer": n.Observer,
		"agent": n.Agent, "exporter": n.Exporter,
	} {
		if len(name) > runtimeIDLimit {
			t.Errorf("%s name %q is %d bytes, over the runtime's %d-byte limit", role, name, len(name), runtimeIDLimit)
		}
	}
}

// TestExporterSpecGolden pins check 4's generated allowlist: the exporter
// spec is the security contract the pre-execution inspection verifies
// against, so a drift in its shape must be a reviewed diff.
func TestExporterSpecGolden(t *testing.T) {
	cfg := testConfig()
	hs := testHandoffSpec()
	spec := buildExporterSpec(cfg, hs, namesFor(hs.RunID), testOwnershipLabel())
	got, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		t.Fatalf("marshal exporter spec: %v", err)
	}
	golden.Assert(t, "exporter-spec", append(got, '\n'))
}

// TestSeederSpecGolden pins the seeder's generated allowlist for the same
// reason the exporter's is pinned, and with one extra stake: the seeder is the
// only container before the writer that holds the workspace read-write, and
// its command is the only gate-authored payload that writes the workspace. A
// drift in either must be a reviewed diff.
func TestSeederSpecGolden(t *testing.T) {
	cfg := testConfig()
	hs := testHandoffSpec()
	spec := buildSeederSpec(cfg, hs, namesFor(hs.RunID), testOwnershipLabel())
	got, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		t.Fatalf("marshal seeder spec: %v", err)
	}
	golden.Assert(t, "seeder-spec", append(got, '\n'))
}

// TestObserverSpecGolden pins the attesting container's allowlist. The
// observer is where the unit's evidence comes from, so its topology and its
// proof-writing command must not drift unreviewed.
func TestObserverSpecGolden(t *testing.T) {
	cfg := testConfig()
	hs := testHandoffSpec()
	spec := buildObserverSpec(cfg, hs, namesFor(hs.RunID), testOwnershipLabel())
	got, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		t.Fatalf("marshal observer spec: %v", err)
	}
	golden.Assert(t, "observer-spec", append(got, '\n'))
}

// TestObserverSpecReadsOnly is the observer's whole argument in one assertion:
// it cannot write what it attests. A read-write observer would be the writer
// vouching for its own write.
func TestObserverSpecReadsOnly(t *testing.T) {
	cfg := testConfig()
	hs := testHandoffSpec()
	names := namesFor(hs.RunID)
	spec := buildObserverSpec(cfg, hs, names, testOwnershipLabel())

	if len(spec.Mounts) != 1 {
		t.Fatalf("Mounts = %+v, want exactly the workspace", spec.Mounts)
	}
	if !spec.Mounts[0].ReadOnly {
		t.Error("observer workspace mount is read-write; it must be read-only")
	}
	if spec.Mounts[0].Source != names.Workspace {
		t.Errorf("observer mounts %q, want the workspace %q", spec.Mounts[0].Source, names.Workspace)
	}
	if spec.Name == names.Seeder {
		t.Error("observer and seeder share a name; the attestation must come from a different VM")
	}
	if !spec.NetworkDisabled || len(spec.Env) != 0 {
		t.Errorf("observer is not credential-free and network-free: env=%q networkDisabled=%v", spec.Env, spec.NetworkDisabled)
	}
	// The nonce must reach the guest, or the proof could be any run's.
	if !strings.Contains(observerScript(cfg, testOwnershipLabel().Value), testOwnershipLabel().Value) {
		t.Error("observer script does not carry this invocation's nonce")
	}
}

// TestObserverScriptCreatesProofParent pins the nested-proof-path case:
// Config.validate accepts any clean absolute disjoint path, so the proof may
// sit below a directory the pinned image does not carry, and without a mkdir
// the redirect fails and no proof is exported at all.
func TestObserverScriptCreatesProofParent(t *testing.T) {
	cfg := testConfig()
	cfg.BaseProofPath = "/proof/nested/base.txt"
	script := observerScript(cfg, testOwnershipLabel().Value)
	if !strings.Contains(script, "mkdir -p '/proof/nested'") {
		t.Errorf("observer script does not create the proof's parent directory:\n%s", script)
	}
	// The default sits at the image root and needs no mkdir; emitting one would
	// be noise in the golden every reader has to discount.
	if root := observerScript(testConfig(), testOwnershipLabel().Value); strings.Contains(root, "mkdir -p") {
		t.Errorf("observer script creates a directory for a root-level proof path:\n%s", root)
	}
}

func TestObserverGitScriptComparesAgainstCommit(t *testing.T) {
	runWith := func(t *testing.T, checkout string, env []string) (string, string, string) {
		t.Helper()
		scratch := t.TempDir()
		script := "h=\"$(cat " + shellQuote(filepath.Join(checkout, ".git", "HEAD")) + ")\"; " +
			"d=yes; s=none; w=error; r=error; " +
			observerGitScript(checkout, filepath.Join(scratch, "git")) +
			"printf '%s\\n%s\\n%s\\n' \"$s\" \"$w\" \"$r\""
		shell := "sh"
		if dash, err := osexec.LookPath("dash"); err == nil {
			// macOS sh accepts extensions that Ubuntu's dash rejects. Prefer
			// dash when available so the generated command stays portable.
			shell = dash
		}
		cmd := osexec.Command(shell, "-c", script) //nolint:gosec // fixed shell and test-owned script
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("observer git script: %v: %s", err, out)
		}
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		if len(lines) != 3 {
			t.Fatalf("observer git output = %q, want sha, worktree state, and replacement state", out)
		}
		return lines[0], lines[1], lines[2]
	}
	run := func(t *testing.T, checkout string) (string, string, string) {
		t.Helper()
		return runWith(t, checkout, scrubbedLiveGitEnv())
	}

	root := t.TempDir()
	checkout := initLiveSeedCheckout(t, root)
	if err := os.WriteFile(filepath.Join(checkout, ".gitignore"), []byte("ignored.txt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, ".gitattributes"), []byte("README.md text eol=crlf\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "README.md"), []byte("clean\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "tab\tname.txt"), []byte("literal tab path\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"foo", `"foo"`} {
		if err := os.WriteFile(filepath.Join(checkout, name), []byte("same blob\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	base := commitLiveSeedCheckout(t, checkout)
	if sha, state, replacements := run(t, checkout); sha != base.BaseSHA || state != "clean" || replacements != "absent" {
		t.Fatalf("clean checkout = (%q, %q, %q), want (%q, clean, absent)", sha, state, replacements, base.BaseSHA)
	}

	if err := os.WriteFile(filepath.Join(checkout, `"foo"`), []byte("dirty quoted path\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, state, _ := run(t, checkout); state != "dirty" {
		t.Errorf("quoted-path edit reported %q, want dirty", state)
	}
	if err := os.WriteFile(filepath.Join(checkout, `"foo"`), []byte("same blob\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(checkout, "README.md"), []byte("dirty\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The source index is deliberately told to hide the edit. The observer's
	// raw blob comparison must still expose it.
	rungitLive(t, checkout, "update-index", "--assume-unchanged", "README.md")
	if _, state, _ := run(t, checkout); state != "dirty" {
		t.Errorf("assume-unchanged edit reported %q, want dirty", state)
	}
	rungitLive(t, checkout, "update-index", "--no-assume-unchanged", "README.md")
	if err := os.WriteFile(filepath.Join(checkout, "README.md"), []byte("clean\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "README.md"), []byte("clean\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, state, _ := run(t, checkout); state != "dirty" {
		t.Errorf("attribute-normalized CRLF bytes reported %q, want dirty", state)
	}
	if err := os.WriteFile(filepath.Join(checkout, "README.md"), []byte("clean\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rungitLive(t, checkout, "config", "core.filemode", "false")
	//nolint:gosec // the executable bit is the property under test
	if err := os.Chmod(filepath.Join(checkout, "README.md"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, state, _ := run(t, checkout); state != "dirty" {
		t.Errorf("mode-only edit under core.filemode=false reported %q, want dirty", state)
	}
	if err := os.Chmod(filepath.Join(checkout, "README.md"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "ignored.txt"), []byte("extra\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, state, _ := run(t, checkout); state != "dirty" {
		t.Errorf("ignored extra file reported %q, want dirty", state)
	}
	if err := os.Remove(filepath.Join(checkout, "ignored.txt")); err != nil {
		t.Fatal(err)
	}
	tree := strings.TrimSpace(rungitLive(t, checkout, "rev-parse", "HEAD^{tree}"))
	replacement := strings.TrimSpace(rungitLive(t, checkout, "commit-tree", tree, "-m", "replacement"))
	rungitLive(t, checkout, "replace", base.BaseSHA, replacement)
	if _, state, replacements := run(t, checkout); state != "error" || replacements != "present" {
		t.Errorf("replacement metadata = (%q, %q), want (error, present)", state, replacements)
	}
	rungitLive(t, checkout, "replace", "-d", base.BaseSHA)
	if err := os.WriteFile(filepath.Join(checkout, ".git", "info", "grafts"), []byte(base.BaseSHA+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, state, replacements := run(t, checkout); state != "error" || replacements != "present" {
		t.Errorf("legacy graft metadata = (%q, %q), want (error, present)", state, replacements)
	}
	if err := os.Remove(filepath.Join(checkout, ".git", "info", "grafts")); err != nil {
		t.Fatal(err)
	}
	rungitLive(t, checkout, "tag", "-a", "tag-object-head", "-m", "tag object head")
	tagObject := strings.TrimSpace(rungitLive(t, checkout, "rev-parse", "refs/tags/tag-object-head"))
	if err := os.WriteFile(filepath.Join(checkout, ".git", "HEAD"), []byte(tagObject+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if sha, state, replacements := run(t, checkout); sha != base.BaseSHA || state != "error" || replacements != "error" {
		t.Errorf("tag-object HEAD = (%q, %q, %q), want (%q, error, error)", sha, state, replacements, base.BaseSHA)
	}
	if err := os.WriteFile(filepath.Join(checkout, ".git", "HEAD"), []byte(base.BaseSHA+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	empty := initLiveSeedCheckout(t, t.TempDir())
	emptyBase := commitLiveSeedCheckout(t, empty)
	if sha, state, replacements := run(t, empty); sha != emptyBase.BaseSHA || state != "clean" || replacements != "absent" {
		t.Errorf("empty commit = (%q, %q, %q), want (%q, clean, absent)", sha, state, replacements, emptyBase.BaseSHA)
	}

	// The failed live observation from #349: a guest that cannot run git at
	// all must leave every git-derived value at its initialized failure state
	// rather than fabricate any of them, even over a pristine checkout.
	// base_sha=none is the shape the host gate then refuses; this pins the
	// emitter half of that refusal.
	stubBin := t.TempDir()
	//nolint:gosec // the stub must be executable to shadow git on PATH
	if err := os.WriteFile(filepath.Join(stubBin, "git"), []byte("#!/bin/sh\nexit 127\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := scrubbedLiveGitEnv()
	for i, entry := range env {
		if strings.HasPrefix(entry, "PATH=") {
			env[i] = "PATH=" + stubBin + string(os.PathListSeparator) + strings.TrimPrefix(entry, "PATH=")
		}
	}
	if sha, state, replacements := runWith(t, empty, env); sha != "none" || state != "error" || replacements != "error" {
		t.Errorf("git-less guest = (%q, %q, %q), want (none, error, error)", sha, state, replacements)
	}
}

// TestSeederSpecShape asserts the properties the golden would let drift
// silently if someone regenerated it: the seeder is credential-free,
// network-free, and holds the workspace read-write at the configured target.
func TestSeederSpecShape(t *testing.T) {
	cfg := testConfig()
	hs := testHandoffSpec()
	names := namesFor(hs.RunID)
	spec := buildSeederSpec(cfg, hs, names, testOwnershipLabel())

	if spec.Image != cfg.ExporterImage {
		t.Errorf("Image = %q, want the pinned exporter image %q", spec.Image, cfg.ExporterImage)
	}
	if !spec.NetworkDisabled {
		t.Error("seeder is not network-disabled")
	}
	if len(spec.Env) != 0 {
		t.Errorf("Env = %q, want none", spec.Env)
	}
	if len(spec.Mounts) != 1 {
		t.Fatalf("Mounts = %+v, want exactly the workspace", spec.Mounts)
	}
	m := spec.Mounts[0]
	if m.Type != MountVolume || m.Source != names.Workspace || m.Target != cfg.WorkspaceTarget {
		t.Errorf("mount = %+v, want the workspace volume at %q", m, cfg.WorkspaceTarget)
	}
	if m.ReadOnly {
		t.Error("seeder workspace mount is read-only; it must be read-write to place the checkout")
	}
	// The staging and sentinel destinations must stay outside the mount: a copy
	// aimed inside it is discarded silently by the reference runtime.
	script := seederScript(cfg)
	for _, inside := range []string{cfg.WorkspaceTarget + "/.git", cfg.WorkspaceTarget + "/seed"} {
		if strings.Contains(script, inside) {
			t.Errorf("seeder script references %q inside the workspace mount", inside)
		}
	}
	for _, want := range []string{cfg.SeedStageDir, cfg.SeedReadyDir, cfg.WorkspaceTarget} {
		if !strings.Contains(script, want) {
			t.Errorf("seeder script does not reference %q", want)
		}
	}
	// The guest's give-up budget must sit above the host bounds it races: both
	// staged copies are bounded at SeedTimeout each and both happen after the
	// seeder starts, so a guest deadline at or below one of them would abort a
	// large but legitimate copy.
	if got := seederGuestBudget(cfg.SeedTimeout); got <= 2*cfg.SeedTimeout {
		t.Errorf("seeder guest budget %s does not exceed the two host copy budgets (%s)", got, 2*cfg.SeedTimeout)
	}
}

// TestSeederScriptBudgetSurvivesSubsecondTimeouts pins the tick arithmetic
// against truncation. The guest loop's granularity is one `sleep 1`, so a
// budget converted with integer division collapses at subsecond timeouts:
// 600ms yields a 1.8s budget that truncates to a single tick, and the seeder
// would give up after about a second while the two host copies may
// legitimately take 1.2s — reinstating the race the budget exists to prevent.
func TestSeederScriptBudgetSurvivesSubsecondTimeouts(t *testing.T) {
	for _, seedTimeout := range []time.Duration{
		100 * time.Millisecond, 600 * time.Millisecond, 1500 * time.Millisecond,
		time.Second, 5 * time.Minute,
	} {
		cfg := testConfig()
		cfg.SeedTimeout = seedTimeout
		ticks := seederScriptTicks(cfg)
		// The guest must outlast both host copies, which are bounded at
		// SeedTimeout each, measured in whole seconds the loop can actually count.
		copies := 2 * seedTimeout
		if got := time.Duration(ticks) * time.Second; got < copies {
			t.Errorf("SeedTimeout %s: guest budget %s is under the two host copy budgets (%s)",
				seedTimeout, got, copies)
		}
	}
}

func TestBuildAgentSpec(t *testing.T) {
	cfg := testConfig()
	hs := testHandoffSpec()
	names := namesFor(hs.RunID)
	spec := buildAgentSpec(cfg, hs, names, testOwnershipLabel(), "http://127.0.0.1:12345")

	if spec.Name != names.Agent {
		t.Errorf("Name = %q, want %q", spec.Name, names.Agent)
	}
	if len(spec.Mounts) != 2 {
		t.Fatalf("len(Mounts) = %d, want 2", len(spec.Mounts))
	}
	ws := spec.Mounts[0]
	if ws.Source != names.Workspace || ws.Target != cfg.WorkspaceTarget || ws.ReadOnly {
		t.Errorf("workspace mount = %+v, want %q rw at %q", ws, names.Workspace, cfg.WorkspaceTarget)
	}
	cred := spec.Mounts[1]
	if cred.Source != "provider-cred" || cred.Target != "/credentials" || !cred.ReadOnly {
		t.Errorf("credential mount = %+v, want provider-cred ro at /credentials", cred)
	}
	// The generated spec passes its own gate.
	if err := validateAgentSpec(cfg, spec, names.Workspace); err != nil {
		t.Errorf("validateAgentSpec(generated) = %v, want nil", err)
	}
}
