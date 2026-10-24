package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/daemonlock"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec/claude"
	"github.com/freeside-ai/freeside/daemon/internal/gitenv"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

var deputyTestNow = func() time.Time { return time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC) }

// deputyLeaseFixture is a canonical deputy root with its own lease root, so
// a test never touches the host's production lease.
func deputyLeaseFixture(t *testing.T) (deputyHost, deputyPaths, loadedDeputySettings) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", filepath.Join(base, "home"))
	leaseRoot := filepath.Join(base, "rig-locks")
	stateRoot := filepath.Join(base, "root", "daemon")
	for _, dir := range []string{leaseRoot, stateRoot} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	var settings loadedDeputySettings
	settings.Listen = "127.0.0.1:8677"
	settings.SeedRoot = filepath.Join(base, "seed")
	host := deputyHost{
		rig: &fakeRigHost{}, leaseRoot: leaseRoot, now: deputyTestNow,
		tailscaleIPs: func() ([]netip.Addr, error) { return nil, nil },
	}
	return host, newDeputyPaths(stateRoot), settings
}

func TestAcquireDeputyLeaseRefusesLiveHolder(t *testing.T) {
	host, paths, settings := deputyLeaseFixture(t)
	holder, err := daemonlock.AcquireRig(daemonlock.RigAcquireConfig{
		Owner:     daemonlock.RigOwner{User: "other", Host: "host", PID: 1},
		StateRoot: paths.StateRoot, DatabasePath: paths.DB,
		ListenAddress: settings.Listen, SeedRoot: settings.SeedRoot, LeaseRoot: host.leaseRoot,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Close() }()
	_, _, err = acquireDeputyLease(context.Background(), host, paths, settings, io.Discard)
	if conflict := (*daemonlock.RigConflictError)(nil); !errors.As(err, &conflict) {
		t.Fatalf("live holder: %v, want a rig conflict naming the holder", err)
	}
}

// TestAcquireDeputyLeaseLeavesProdFreeToStart pins #2080: the deputy holds
// no lock of prod's, so prod can start, driverless, while the lease is held.
func TestAcquireDeputyLeaseLeavesProdFreeToStart(t *testing.T) {
	host, paths, settings := deputyLeaseFixture(t)
	lease, _, err := acquireDeputyLease(context.Background(), host, paths, settings, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Close() }()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	prodDaemon := filepath.Join(supervisedRoots(home)[environmentProd], "daemon")
	if err := os.MkdirAll(prodDaemon, 0o700); err != nil {
		t.Fatal(err)
	}
	prodLock, err := daemonlock.Acquire(filepath.Join(prodDaemon, "freeside.db"))
	if err != nil {
		t.Fatalf("prod's database lock under a held deputy lease: %v", err)
	}
	if err := prodLock.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAcquireDeputyLeaseRecoversItsOwnStaleLease(t *testing.T) {
	host, paths, settings := deputyLeaseFixture(t)
	prior, err := daemonlock.AcquireRig(daemonlock.RigAcquireConfig{
		Owner:     daemonlock.RigOwner{User: "operator", Host: "host", PID: 4242, Note: deputyOwnerNote},
		StateRoot: paths.StateRoot, DatabasePath: paths.DB,
		ListenAddress: settings.Listen, SeedRoot: settings.SeedRoot, LeaseRoot: host.leaseRoot,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Abandon keeps the manifests and drops the locks: a dead deputy.
	if err := prior.Abandon(); err != nil {
		t.Fatal(err)
	}
	lease, interrupted, err := acquireDeputyLease(context.Background(), host, paths, settings, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Close() }()
	if interrupted == nil || interrupted.Owner.PID != 4242 {
		t.Fatalf("interrupted run %+v, want the abandoned lease's manifest", interrupted)
	}
	if lease.Manifest().Owner.Note != deputyOwnerNote || lease.Manifest().Owner.PID != os.Getpid() {
		t.Fatalf("new lease owner %+v", lease.Manifest().Owner)
	}
}

// TestAcquireDeputyLeaseMigratesItsOwnRootsReviewJournal pins #2201: the
// deputy recovers its own root's review journal through the migrated schema,
// because its start migrates that database anyway.
func TestAcquireDeputyLeaseMigratesItsOwnRootsReviewJournal(t *testing.T) {
	host, paths, settings := deputyLeaseFixture(t)
	prior, err := daemonlock.AcquireRig(daemonlock.RigAcquireConfig{
		Owner:     daemonlock.RigOwner{User: "operator", Host: "host", PID: 4242, Note: deputyOwnerNote},
		StateRoot: paths.StateRoot, DatabasePath: paths.DB,
		ListenAddress: settings.Listen, SeedRoot: settings.SeedRoot, LeaseRoot: host.leaseRoot,
	})
	if err != nil {
		t.Fatal(err)
	}
	volume := ward.RuntimeResourceNamesFor(claude.RunIDFor("review-interrupted")).Volumes[0]
	if _, err := daemonlock.BindRigRuntimeResources(
		paths.StateRoot, prior.Token(), nil, []string{volume}, nil,
	); err != nil {
		t.Fatal(err)
	}
	if err := prior.Abandon(); err != nil {
		t.Fatal(err)
	}
	rig := &fakeRigHost{volumes: map[string]bool{volume: true}}
	rig.recoverReviews = func(context.Context, string, daemonlock.RigManifest) error {
		delete(rig.volumes, volume)
		return nil
	}
	host.rig = rig
	lease, _, err := acquireDeputyLease(context.Background(), host, paths, settings, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Close() }()
	if !slices.Equal(rig.migrates, []bool{true}) {
		t.Fatalf("review recovery migrate arguments = %v, want [true]", rig.migrates)
	}
}

func TestAcquireDeputyLeaseRefusesAnotherRootsStaleLease(t *testing.T) {
	host, paths, settings := deputyLeaseFixture(t)
	otherState := filepath.Join(filepath.Dir(filepath.Dir(paths.StateRoot)), "other", "daemon")
	if err := os.MkdirAll(otherState, 0o700); err != nil {
		t.Fatal(err)
	}
	prior, err := daemonlock.AcquireRig(daemonlock.RigAcquireConfig{
		Owner:     daemonlock.RigOwner{User: "operator", Host: "host", PID: 4242},
		StateRoot: otherState, DatabasePath: filepath.Join(otherState, "freeside.db"),
		ListenAddress: "127.0.0.1:8678", SeedRoot: filepath.Join(filepath.Dir(otherState), "seed"),
		LeaseRoot: host.leaseRoot,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := prior.Abandon(); err != nil {
		t.Fatal(err)
	}
	_, _, err = acquireDeputyLease(context.Background(), host, paths, settings, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "freesided rig recover -state-root "+otherState) {
		t.Fatalf("another root's stale lease: %v, want a refusal naming rig recover for %s", err, otherState)
	}
}

func TestAcquireDeputyLeaseRefusesBusyListenerAndReleases(t *testing.T) {
	host, paths, settings := deputyLeaseFixture(t)
	host.rig = &fakeRigHost{live: true, description: "freesided version=test"}
	if _, _, err := acquireDeputyLease(context.Background(), host, paths, settings, io.Discard); err == nil ||
		!strings.Contains(err.Error(), "already occupied") {
		t.Fatalf("busy listener: %v", err)
	}
	host.rig = &fakeRigHost{}
	lease, interrupted, err := acquireDeputyLease(context.Background(), host, paths, settings, io.Discard)
	if err != nil || interrupted != nil {
		t.Fatalf("after a refused start the lease must be free and clean: %v, %+v", err, interrupted)
	}
	_ = lease.Close()
}

func openDeputyTestStore(t *testing.T) *store.Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	seedTemplateStore(t, dbPath)
	st, _, err := openStoreWithTopicKey(context.Background(), dbPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func openDeputyItems(t *testing.T, st *store.Store, prefix string) []domain.AttentionItem {
	t.Helper()
	var matched []domain.AttentionItem
	if err := st.Read(context.Background(), func(tx *store.ReadTx) error {
		items, err := tx.ListOpenAttentionItems(context.Background(), domain.AttentionSystemHealth)
		for _, item := range items {
			if strings.HasPrefix(string(item.ID), prefix) {
				matched = append(matched, item)
			}
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return matched
}

func TestDeputyPreflightCardNamesFailedChecksAndClearsOnPass(t *testing.T) {
	ctx := context.Background()
	st := openDeputyTestStore(t)
	failed := deputySelfCheckResult{Failed: []string{"images", "codex_credential"}, Remediation: "pull the images"}
	for range 2 {
		if err := convergeDeputyPreflightCard(ctx, st, failed, deputyTestNow); err != nil {
			t.Fatal(err)
		}
	}
	items := openDeputyItems(t, st, deputyPreflightItemPrefix)
	if len(items) != 1 {
		t.Fatalf("open deputy preflight cards: %d, want one", len(items))
	}
	item := items[0]
	if item.HealthDiagnostic.Code != "deputy_preflight_failed" || *item.Posture != domain.HealthPostureBlocking ||
		!strings.Contains(item.Reason, "images, codex_credential") || !strings.Contains(item.Reason, "pull the images") {
		t.Fatalf("card %+v", item)
	}
	if err := convergeDeputyPreflightCard(ctx, st, deputySelfCheckResult{}, deputyTestNow); err != nil {
		t.Fatal(err)
	}
	if items := openDeputyItems(t, st, deputyPreflightItemPrefix); len(items) != 0 {
		t.Fatalf("a passing start left %d deputy preflight cards open", len(items))
	}
}

func TestDeputyInterruptedCardNamesThePriorRun(t *testing.T) {
	st := openDeputyTestStore(t)
	prior := daemonlock.RigManifest{
		Owner:      daemonlock.RigOwner{User: "operator", Host: "mac", PID: 4242},
		AcquiredAt: time.Date(2026, 10, 13, 9, 30, 0, 0, time.UTC),
	}
	if err := fileDeputyInterruptedCard(context.Background(), st, prior, deputyTestNow); err != nil {
		t.Fatal(err)
	}
	items := openDeputyItems(t, st, deputyInterruptedItemPrefix)
	if len(items) != 1 || items[0].HealthDiagnostic.Code != "deputy_run_interrupted" ||
		*items[0].Posture != domain.HealthPostureAdvisory ||
		!strings.Contains(items[0].Reason, "operator@mac pid 4242, started 2026-10-13T09:30:00Z") {
		t.Fatalf("interrupted cards %+v", items)
	}
}

func TestRecordDeputyIdentitiesRecordsOnceAndRefusesDrift(t *testing.T) {
	ctx := context.Background()
	st := openDeputyTestStore(t)
	snapshot := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(snapshot, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	var settings loadedDeputySettings
	settings.AgentTree.AuthIdentity, settings.AgentTree.ReviewAuthIdentity = "claude-1", "codex-1"
	settings.AgentTree.AuthVolume = "claude-auth"
	settings.Review.AuthSnapshot = snapshot
	for range 2 {
		if err := recordDeputyIdentities(ctx, st, settings, deputyTestNow); err != nil {
			t.Fatal(err)
		}
	}
	settings.AgentTree.AuthVolume = "another-volume"
	if err := recordDeputyIdentities(ctx, st, settings, deputyTestNow); err == nil ||
		!strings.Contains(err.Error(), "recorded auth identity claude-1 differs from the settings") {
		t.Fatalf("drifted settings: %v", err)
	}
}

func deputyTestGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", dir}, args...)...) //nolint:gosec // G204: fixed test arguments
	command.Env = append(gitenv.Scrubbed(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.test", "GIT_COMMITTER_NAME=t",
		"GIT_COMMITTER_EMAIL=t@example.test")
	out, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestAgentTreeMatchesComparesTheCommittedPolicyTree(t *testing.T) {
	ctx := context.Background()
	checkout := t.TempDir()
	deputyTestGit(t, checkout, "init", "-q")
	if err := os.MkdirAll(filepath.Join(checkout, "policy", "lineup"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "policy", "lineup", "roles.json"), []byte(`{"writer":"a"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "README.md"), []byte("outside the policy paths\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	deputyTestGit(t, checkout, "add", ".")
	deputyTestGit(t, checkout, "commit", "-q", "-m", "policy")
	adopted := deputyTestGit(t, checkout, "rev-parse", "HEAD")
	empty := deputyTestGit(t, checkout, "hash-object", "-t", "tree", "/dev/null")
	patch := filepath.Join(t.TempDir(), "agent-tree.patch")
	if err := os.WriteFile(patch, []byte(deputyTestGit(t, checkout, "diff", "--binary", empty, adopted, "--", "policy")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(t.TempDir(), "scratch")
	if ok, err := agentTreeMatches(ctx, patch, checkout, adopted, scratch); err != nil || !ok {
		t.Fatalf("committed tree: match=%t err=%v, want a match", ok, err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "policy", "lineup", "roles.json"), []byte(`{"writer":"b"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	deputyTestGit(t, checkout, "commit", "-q", "-am", "drift")
	drifted := deputyTestGit(t, checkout, "rev-parse", "HEAD")
	if ok, err := agentTreeMatches(ctx, patch, checkout, drifted, scratch); err != nil || ok {
		t.Fatalf("drifted tree: match=%t err=%v, want a mismatch", ok, err)
	}
	if ok, err := agentTreeMatches(ctx, patch, checkout, strings.Repeat("0", 40), scratch); err != nil || ok {
		t.Fatalf("unknown commit: match=%t err=%v, want a mismatch", ok, err)
	}
}

func TestDeputyEvidenceSitsAtTheStandingRoot(t *testing.T) {
	t.Parallel()
	paths := newDeputyPaths("/deputy/daemon")
	if want := "/deputy/production-evidence/composition"; paths.EvidenceDir != want {
		t.Fatalf("evidence dir = %q, want %q", paths.EvidenceDir, want)
	}
}

func TestDeputyDaemonConfigRunsAgentsOnlyAfterAPassingCheck(t *testing.T) {
	path, _ := writeDeputySettings(t, deputySettingsFixture(t), 0o600)
	settings, err := loadDeputySettings(path)
	if err != nil {
		t.Fatal(err)
	}
	paths := newDeputyPaths("/deputy/daemon")
	disabled := deputyDaemonConfig(settings, paths, deputyLaunch{}, nil)
	if disabled.Claude != nil || disabled.Environment != environmentEphemeral || disabled.DBPath != paths.DB ||
		disabled.DeputyLaunch == nil {
		t.Fatalf("failed check: %+v", disabled)
	}
	passing := deputyDaemonConfig(settings, paths, deputyLaunch{
		ManifestDigest: "sha256:m", Manifest: compositionManifest{JudgmentConfigurationDigest: "sha256:j"},
	}, nil)
	if passing.DeputyLaunch == nil || passing.DeputyLaunch.ManifestDigest != "sha256:m" {
		t.Fatalf("passing check binds launch %+v", passing.DeputyLaunch)
	}
	enabled := passing.Claude
	if enabled == nil || enabled.RigTokenFile != paths.Acquisition || !enabled.RunConformance ||
		enabled.OperatingMode != domain.ModeUnattended || enabled.StateRoot != "/prod/daemon" ||
		enabled.WriterStopTimeout != 20*time.Minute || enabled.Judgments != (judgmentConfig{}) {
		t.Fatalf("passing check: %+v", enabled)
	}
	// Exercise the configured judgment fields too: only the launch's digest
	// differs between the prospective and enabled driver configurations.
	settings.Judgment.ClaudeBin = "/bin/claude"
	settings.Judgment.ClaudeSHA256 = strings.Repeat("e", 64)
	settings.Judgment.Model = "claude-opus"
	settings.Judgment.Effort = domain.EffortMedium
	settings.Judgment.PublicationAuthorPrompt = "/prompts/author.md"
	prospective := deputyClaudeDriverConfig(settings, paths, "")
	passing = deputyDaemonConfig(settings, paths, deputyLaunch{
		ManifestDigest: "sha256:m", Manifest: compositionManifest{JudgmentConfigurationDigest: "sha256:j"},
	}, nil)
	if passing.Claude.Judgments.ExpectedDigest != "sha256:j" {
		t.Fatal("enabled driver lost the launch's judgment digest")
	}
	passing.Claude.Judgments.ExpectedDigest = ""
	if !reflect.DeepEqual(prospective, *passing.Claude) {
		t.Fatal("early validation and enabled driver configurations differ")
	}
}

func deputyDaemonSettingsFixture(t *testing.T) loadedDeputySettings {
	t.Helper()
	path, _ := writeDeputySettings(t, deputySettingsFixture(t), 0o600)
	settings, err := loadDeputySettings(path)
	if err != nil {
		t.Fatal(err)
	}
	prompts, _ := promptPackagesFixture(t)
	settings.Prompts.PromptPackage = prompts.PromptPackageFile
	settings.Prompts.SpecificationPromptPackage = prompts.SpecificationPromptPackageFile
	settings.Prompts.RemediationPromptPackage = prompts.RemediationPromptPackageFile
	return settings
}

func TestDeputyDaemonSettingsMatchesDownstreamRefusals(t *testing.T) {
	t.Parallel()
	noTailscale := func() ([]netip.Addr, error) { return nil, nil }
	manual := func(s loadedDeputySettings, _ deputyPaths) error {
		_, err := loadManualSubmissionConfig(s.ManualSubmissionConfig)
		return err
	}
	listener := func(s loadedDeputySettings, _ deputyPaths) error {
		_, err := resolveListenAddress(s.Listen, noTailscale)
		return err
	}
	claudeConfig := func(s loadedDeputySettings, paths deputyPaths) error {
		return deputyClaudeDriverConfig(s, paths, "").validate()
	}
	prompts := func(s loadedDeputySettings, paths deputyPaths) error {
		_, err := readPromptPackages(deputyClaudeDriverConfig(s, paths, ""))
		return err
	}
	type refusal struct {
		name       string
		edit       func(*testing.T, *loadedDeputySettings)
		downstream func(loadedDeputySettings, deputyPaths) error
		want       string
	}
	cases := []refusal{
		{"malformed manual config", func(t *testing.T, s *loadedDeputySettings) {
			s.ManualSubmissionConfig = filepath.Join(t.TempDir(), "manual.json")
			if err := os.WriteFile(s.ManualSubmissionConfig, []byte("{"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, manual, "manual submission config: EOF"},
		{"missing manual config", func(t *testing.T, s *loadedDeputySettings) {
			s.ManualSubmissionConfig = filepath.Join(t.TempDir(), "missing.json")
		}, manual, "no such file"},
		{"unsupported listener", func(_ *testing.T, s *loadedDeputySettings) {
			s.Listen = "192.0.2.10:7340"
		}, listener, "non-loopback, non-Tailscale"},
		{"unclean agent tree", func(_ *testing.T, s *loadedDeputySettings) {
			s.AgentTree.Checkout = "/src/../tree"
		}, claudeConfig, "-agent-tree must be a clean absolute path"},
		{"unclean review input", func(_ *testing.T, s *loadedDeputySettings) {
			s.Review.InputRoot = "/review/../inputs"
		}, claudeConfig, "-review-input-root must be a clean absolute path"},
		{"qualified base ref", func(_ *testing.T, s *loadedDeputySettings) {
			s.Repository.BaseRef = "refs/heads/main"
		}, claudeConfig, "-base-ref is not a valid transport branch"},
	}
	for _, field := range []struct {
		name string
		path func(*loadedDeputySettings) *string
	}{
		{"implementation", func(s *loadedDeputySettings) *string { return &s.Prompts.PromptPackage }},
		{"specification", func(s *loadedDeputySettings) *string { return &s.Prompts.SpecificationPromptPackage }},
		{"remediation", func(s *loadedDeputySettings) *string { return &s.Prompts.RemediationPromptPackage }},
	} {
		for _, failure := range []string{"missing", "unreadable", "empty", "wrong role"} {
			want := field.name + " prompt package"
			switch failure {
			case "missing":
				want = "no such file"
			case "unreadable":
				want = "is a directory"
			case "empty":
				want = "is empty"
			case "wrong role":
				switch field.name {
				case "implementation":
					want = "implementation prompt package enables"
				case "specification":
					want = "prompt package roles: specification prompt package does not enable"
				default:
					want = "remediation prompt package roles: specification prompt package does not enable"
				}
			}
			cases = append(cases, refusal{
				name: field.name + " " + failure, downstream: prompts, want: want,
				edit: func(t *testing.T, s *loadedDeputySettings) {
					path := field.path(s)
					switch failure {
					case "missing":
						*path = filepath.Join(t.TempDir(), "missing.md")
					case "unreadable":
						*path = t.TempDir()
					case "empty", "wrong role":
						body := []byte{}
						if failure == "wrong role" {
							body = []byte("No prior artifacts.\n")
							if field.name == "implementation" {
								body = []byte(priorArtifactsTestDirective + "Wrong role.\n")
							}
						}
						if err := os.WriteFile(*path, body, 0o600); err != nil {
							t.Fatal(err)
						}
					}
				},
			})
		}
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			settings := deputyDaemonSettingsFixture(t)
			paths := newDeputyPaths(filepath.Join(t.TempDir(), "daemon"))
			if err := checkDeputyDaemonSettings(settings, paths, noTailscale); err != nil {
				t.Fatalf("valid fixture refused: %v", err)
			}
			tc.edit(t, &settings)
			early := checkDeputyDaemonSettings(settings, paths, noTailscale)
			downstream := tc.downstream(settings, paths)
			for name, err := range map[string]error{"early": early, "downstream": downstream} {
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("%s error %v, want %q", name, err, tc.want)
				}
			}
			if !strings.HasPrefix(early.Error(), "settings: ") || !strings.Contains(early.Error(), downstream.Error()) {
				t.Fatalf("early refusal %v lost downstream reason %v", early, downstream)
			}
		})
	}
}

func TestDeputyDaemonSettingsAcceptsValidFiles(t *testing.T) {
	t.Parallel()
	for _, withManual := range []bool{false, true} {
		settings := deputyDaemonSettingsFixture(t)
		if withManual {
			settings.ManualSubmissionConfig = filepath.Join(t.TempDir(), "manual.json")
			writeManualConfig(t, settings.ManualSubmissionConfig, manualConfigFixture(t))
		}
		for _, address := range []string{"127.0.0.1:7340", "[::1]:7340", "100.64.0.7:7340"} {
			settings.Listen = address
			paths := newDeputyPaths(filepath.Join(t.TempDir(), "daemon"))
			if err := checkDeputyDaemonSettings(settings, paths, func() ([]netip.Addr, error) {
				return []netip.Addr{netip.MustParseAddr("100.64.0.7")}, nil
			}); err != nil {
				t.Fatalf("withManual=%v, listen=%s: %v", withManual, address, err)
			}
			if _, err := os.Stat(paths.StateRoot); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("early validation created state: %v", err)
			}
		}
	}
}

type deputyForbiddenLeaseHost struct {
	fakeRigHost
	t *testing.T
}

func (h *deputyForbiddenLeaseHost) ProbeDaemon(context.Context, string) (string, bool, error) {
	h.t.Fatal("settings refusal reached the lease host")
	return "", false, errors.New("unexpected lease probe")
}

func TestRunDeputyRefusesBeforeTheLease(t *testing.T) {
	host, paths, _ := deputyLeaseFixture(t)
	host.rig = &deputyForbiddenLeaseHost{t: t}
	settings := deputyDaemonSettingsFixture(t)
	settings.ManualSubmissionConfig = filepath.Join(t.TempDir(), "manual.json")
	if err := os.WriteFile(settings.ManualSubmissionConfig, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(settings.deputySettings)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.Settings, body, 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	err = runDeputy(t.Context(), func() {}, []string{"-root", filepath.Dir(paths.StateRoot)}, &stdout, &stderr, host)
	if !errors.Is(err, errDeputyUsage) || !strings.Contains(err.Error(), "manual_submission_config") {
		t.Fatalf("got %v, want a manual_submission_config usage refusal", err)
	}
	for _, path := range []string{paths.Acquisition, paths.Manifest, paths.EvidenceDir, paths.DB, paths.DB + ".blobs"} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("early refusal left %s: %v", path, err)
		}
	}
	entries, err := os.ReadDir(host.leaseRoot)
	if err != nil || len(entries) != 0 {
		t.Fatalf("early refusal touched lease root: %v, %v", entries, err)
	}
}

func TestPublishCompositionEvidenceIsNoClobber(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "composition")
	first, err := publishCompositionEvidence(dir, []byte("manifest\n"))
	if err != nil {
		t.Fatal(err)
	}
	again, err := publishCompositionEvidence(dir, []byte("manifest\n"))
	if err != nil || again != first {
		t.Fatalf("replay: %s, %v", again, err)
	}
	if err := os.WriteFile(first+".tmp", []byte("other"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(first+".tmp", first); err != nil {
		t.Fatal(err)
	}
	if _, err := publishCompositionEvidence(dir, []byte("manifest\n")); err == nil ||
		!strings.Contains(err.Error(), "collision") {
		t.Fatalf("replaced evidence: %v", err)
	}
}

// TestReleaseDeputyLeaseProvesTheRigClear binds runtime
// resources the way the in-process driver does, then ends the lease three
// ways: clean with the resources gone, clean with one left, and unclean.
func TestReleaseDeputyLeaseProvesTheRigClear(t *testing.T) {
	cases := map[string]struct {
		clean      bool
		leftVolume bool
		wantStale  bool
		wantErr    string
	}{
		"clean end, resources gone":   {clean: true},
		"clean end, a volume remains": {clean: true, leftVolume: true, wantStale: true, wantErr: "left the rig lease"},
		"unclean end":                 {clean: false, wantStale: true},
	}
	// Names in the owned namespace the lease manifest accepts.
	writer := "freeside-handoff-c" + strings.Repeat("a", 31) + "-agent"
	workspace := "freeside-handoff-c" + strings.Repeat("a", 31) + "-ws"
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			host, paths, settings := deputyLeaseFixture(t)
			rig := &fakeRigHost{
				containers: map[string]bool{writer: true},
				volumes:    map[string]bool{workspace: tc.leftVolume},
			}
			lease, err := daemonlock.AcquireRig(daemonlock.RigAcquireConfig{
				Owner:     daemonlock.RigOwner{User: "operator", Host: "host", PID: os.Getpid(), Note: deputyOwnerNote},
				StateRoot: paths.StateRoot, DatabasePath: paths.DB,
				ListenAddress: settings.Listen, SeedRoot: settings.SeedRoot, LeaseRoot: host.leaseRoot,
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := daemonlock.BindRigRuntimeResources(paths.StateRoot, lease.Token(),
				[]string{writer}, []string{workspace}, nil); err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{paths.Manifest, paths.Acquisition} {
				if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel() // the run's context is gone by the time the lease ends
			err = releaseDeputyLease(ctx, rig, paths, lease, tc.clean)
			if tc.wantErr == "" && err != nil || tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("release: %v, want error %q", err, tc.wantErr)
			}
			for _, path := range []string{paths.Manifest, paths.Acquisition} {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("%s survives the release: %v", path, err)
				}
			}
			if tc.clean != slices.Contains(rig.deleted, writer) {
				t.Fatalf("deleted %q on a clean=%v end", rig.deleted, tc.clean)
			}
			next, err := daemonlock.AcquireRig(daemonlock.RigAcquireConfig{
				Owner:     daemonlock.RigOwner{User: "operator", Host: "host", PID: os.Getpid()},
				StateRoot: paths.StateRoot, DatabasePath: paths.DB,
				ListenAddress: settings.Listen, SeedRoot: settings.SeedRoot, LeaseRoot: host.leaseRoot,
			})
			if stale := (*daemonlock.RigStaleError)(nil); errors.As(err, &stale) != tc.wantStale {
				t.Fatalf("next acquire: %v, want stale=%v", err, tc.wantStale)
			}
			if err == nil {
				_ = next.Close()
			}
		})
	}
}
