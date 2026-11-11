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

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/daemoncompose"
	"github.com/freeside-ai/freeside/daemon/internal/daemonlock"
	"github.com/freeside-ai/freeside/daemon/internal/daemonshell"
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

//nolint:paralleltest // Serial: deputyLeaseFixture sets HOME with t.Setenv.
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
//
//nolint:paralleltest // Serial: deputyLeaseFixture sets HOME with t.Setenv.
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

//nolint:paralleltest // Serial: deputyLeaseFixture sets HOME with t.Setenv.
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
//
//nolint:paralleltest // Serial: deputyLeaseFixture sets HOME with t.Setenv.
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

//nolint:paralleltest // Serial: deputyLeaseFixture sets HOME with t.Setenv.
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

//nolint:paralleltest // Serial: deputyLeaseFixture sets HOME with t.Setenv.
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// secondWriterDeputySettings is the daemon settings fixture with a second
// writer subscription on its own volume and a Codex reviewer whose store is
// a real file, so auth adopt can enroll every identity.
func secondWriterDeputySettings(t *testing.T) loadedDeputySettings {
	t.Helper()
	settings := deputyDaemonSettingsFixture(t)
	review, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	review = filepath.Join(review, "review")
	if err := os.Mkdir(review, 0o700); err != nil {
		t.Fatal(err)
	}
	var auth map[string]any
	if err := json.Unmarshal(commandCodexAuth("operator-refresh"), &auth); err != nil {
		t.Fatal(err)
	}
	auth["tokens"].(map[string]any)["account_id"] = "acct-deputy-codex"
	body, err := json.Marshal(auth)
	if err != nil {
		t.Fatal(err)
	}
	settings.Review.InputRoot, settings.Review.AuthSnapshot = review, filepath.Join(review, "auth.json")
	if err := os.WriteFile(settings.Review.AuthSnapshot, body, 0o600); err != nil {
		t.Fatal(err)
	}
	settings.AgentTree.SecondWriter = &deputySecondWriter{
		AuthIdentity: "claude-2", AuthVolume: "claude-auth-second",
		ClaudeAccount: "second@example.test", CostOwner: "owner",
	}
	return settings
}

// TestDeputyStartAdoptsASecondWriterOverItsExistingVolume pins #2653's
// fresh-root start: both writer identities are recorded on their own
// usage pools over volumes the start only observes, auth adopt enrolls
// both, the committed patch matches, and auth list shows both enrollments
// at generation 1.
//
//nolint:paralleltest // Serial: deputyLeaseFixture sets HOME with t.Setenv.
func TestDeputyStartAdoptsASecondWriterOverItsExistingVolume(t *testing.T) {
	ctx := t.Context()
	host, paths, _ := deputyLeaseFixture(t)
	settings := secondWriterDeputySettings(t)
	seedTemplateStore(t, paths.DB)
	checkout := t.TempDir()
	deputyTestGit(t, checkout, "init", "-q")
	deputyTestGit(t, checkout, "commit", "-q", "--allow-empty", "-m", "empty")
	settings.AgentTree.Checkout = checkout
	settings.AgentTree.Commit = deputyTestGit(t, checkout, "rev-parse", "HEAD")
	var observed []string
	host.inspectImage = func(context.Context, string) error { return nil }
	host.adopt = authAdoptDeps{
		observeVolume: func(string, string) func(context.Context, string) (domain.Digest, error) {
			return func(_ context.Context, volume string) (domain.Digest, error) {
				observed = append(observed, volume)
				return domain.Digest(contentaddr.Sum([]byte("tree of " + volume))), nil
			}
		},
		now: func() time.Time { return time.Now().UTC() },
	}

	if err := withDeputyTestStore(ctx, paths.DB, func(st *store.Store) error {
		return recordDeputyIdentities(ctx, st, settings, deputyTestNow)
	}); err != nil {
		t.Fatal(err)
	}
	// The start before the tree is committed emits the patch and refuses.
	if result, ok := adoptDeputyAgentTree(ctx, host, paths, settings); ok ||
		!strings.Contains(result.Remediation, "is not the tree auth adopt emitted") {
		log, _ := os.ReadFile(paths.AdoptLog)
		t.Fatalf("uncommitted tree: %+v, %t\n%s", result, ok, log)
	}
	deputyTestGit(t, checkout, "apply", paths.Patch)
	deputyTestGit(t, checkout, "add", ".")
	deputyTestGit(t, checkout, "commit", "-q", "-m", "agent tree")
	settings.AgentTree.Commit = deputyTestGit(t, checkout, "rev-parse", "HEAD")
	if err := withDeputyTestStore(ctx, paths.DB, func(st *store.Store) error {
		return recordDeputyIdentities(ctx, st, settings, deputyTestNow)
	}); err != nil {
		t.Fatalf("the restart records nothing new: %v", err)
	}
	if result, ok := adoptDeputyAgentTree(ctx, host, paths, settings); !ok {
		t.Fatalf("committed tree: %+v", result)
	}
	slices.Sort(observed)
	if got := slices.Compact(observed); !slices.Equal(got, []string{"claude-auth", "claude-auth-second"}) {
		t.Fatalf("observed volumes %v, want both writer volumes", got)
	}
	listed := runAuthListForTest(t, paths.DB)
	pools := map[domain.UsagePoolID]bool{}
	for _, id := range []domain.AuthIdentityID{"claude-1", "claude-2"} {
		i := slices.IndexFunc(listed.Identities, func(identity authListIdentity) bool { return identity.ID == id })
		if i < 0 {
			t.Fatalf("auth list lacks %s: %+v", id, listed)
		}
		identity := listed.Identities[i]
		if len(identity.Enrollments) != 1 || identity.Enrollments[0].Generation == nil ||
			*identity.Enrollments[0].Generation != 1 {
			t.Fatalf("identity %s enrollments %+v, want one at generation 1", id, identity.Enrollments)
		}
		pools[identity.UsagePool] = true
	}
	if len(pools) != 2 {
		t.Fatalf("writer usage pools %v, want two", pools)
	}
}

// The deputy passes the second writer's identity and volume to preflight,
// so a deputy start runs second_claude_credentials; without one it passes
// neither.
//
//nolint:paralleltest // Serial: deputyLeaseFixture sets HOME with t.Setenv.
func TestDeputyPreflightConfigCarriesTheSecondWriter(t *testing.T) {
	host, paths, leaseSettings := deputyLeaseFixture(t)
	settings := secondWriterDeputySettings(t)
	settings.Listen, settings.SeedRoot = leaseSettings.Listen, leaseSettings.SeedRoot
	lease, interrupted, err := acquireDeputyLease(t.Context(), host, paths, settings, io.Discard)
	if err != nil || interrupted != nil {
		t.Fatalf("acquire lease: %v, %+v", err, interrupted)
	}
	defer func() { _ = lease.Close() }()
	cfg := deputyPreflightConfig(settings, paths, lease)
	if cfg.SecondAuthIdentityID != "claude-2" || cfg.SecondAuthVolume != "claude-auth-second" {
		t.Fatalf("preflight second writer = %q on %q", cfg.SecondAuthIdentityID, cfg.SecondAuthVolume)
	}
	settings.AgentTree.SecondWriter = nil
	if cfg := deputyPreflightConfig(settings, paths, lease); cfg.SecondAuthIdentityID != "" || cfg.SecondAuthVolume != "" {
		t.Fatalf("one writer: preflight second writer = %q on %q", cfg.SecondAuthIdentityID, cfg.SecondAuthVolume)
	}
}

// withDeputyTestStore runs fn on the store at dbPath and closes it, so a
// later in-process command opens the database alone.
func withDeputyTestStore(ctx context.Context, dbPath string, fn func(*store.Store) error) error {
	st, _, err := openStoreWithTopicKey(ctx, dbPath, store.Options{})
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	return fn(st)
}

// TestRecordDeputyIdentitiesRefusesAStrayClaudeIdentity pins #2653's
// refusal: an enabled Claude identity on a volume that the settings name
// as neither writer nor the reviewer refuses the start, by name, whether
// the second writer changed or was dropped or the first writer changed. A
// Claude reviewer the settings name is not stray.
func TestRecordDeputyIdentitiesRefusesAStrayClaudeIdentity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := secondWriterDeputySettings(t)
	for name, tc := range map[string]struct {
		edit  func(*loadedDeputySettings)
		stray string
	}{
		"second identity changed": {func(s *loadedDeputySettings) {
			changed := *s.AgentTree.SecondWriter
			changed.AuthIdentity, changed.AuthVolume = "claude-3", "claude-auth-third"
			s.AgentTree.SecondWriter = &changed
		}, "claude-2"},
		"second writer dropped": {func(s *loadedDeputySettings) { s.AgentTree.SecondWriter = nil }, "claude-2"},
		"first identity changed": {func(s *loadedDeputySettings) {
			s.AgentTree.AuthIdentity, s.AgentTree.AuthVolume = "claude-9", "claude-auth-ninth"
		}, "claude-1"},
		"unchanged": {func(*loadedDeputySettings) {}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			st := openDeputyTestStore(t)
			if err := recordDeputyIdentities(ctx, st, base, deputyTestNow); err != nil {
				t.Fatal(err)
			}
			settings := base
			tc.edit(&settings)
			err := recordDeputyIdentities(ctx, st, settings, deputyTestNow)
			if tc.stray == "" {
				if err != nil {
					t.Fatalf("unchanged settings: %v", err)
				}
				return
			}
			want := "recorded Claude auth identity " + tc.stray + " is neither writer nor the reviewer"
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("err = %v, want %q", err, want)
			}
		})
	}

	t.Run("named claude reviewer", func(t *testing.T) {
		st := openDeputyTestStore(t)
		if err := st.Write(ctx, func(tx *store.WriteTx) error {
			return tx.RecordAuthIdentity(ctx, domain.AuthIdentity{
				ID: "claude-review", Provider: "claude", AuthStoreMutationLease: true, Enabled: true,
				Interim: domain.InterimClientFacts{AuthStoreVolume: "claude-review-auth", RefreshStrategy: domain.RefreshOnDemand},
			}, deputyTestNow())
		}); err != nil {
			t.Fatal(err)
		}
		settings := base
		settings.AgentTree.ReviewHarness = domain.HarnessClientClaudeCode
		settings.AgentTree.ReviewAuthIdentity = "claude-review"
		if err := recordDeputyIdentities(ctx, st, settings, deputyTestNow); err != nil {
			t.Fatalf("a Claude reviewer the settings name: %v", err)
		}
	})
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
	t.Parallel()
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
	t.Parallel()
	path, _ := writeDeputySettings(t, deputySettingsFixture(t), 0o600)
	settings, err := loadDeputySettings(path)
	if err != nil {
		t.Fatal(err)
	}
	paths := newDeputyPaths("/deputy/daemon")
	disabled := deputyDaemonConfig(settings, paths, deputyLaunch{}, nil)
	if disabled.Claude != nil || disabled.Environment != environmentEphemeral || disabled.DBPath != paths.DB ||
		disabled.DeputyLaunch == nil || disabled.MemoryBudgetMiB != 8192 {
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
		enabled.WriterStopTimeout != 20*time.Minute || enabled.Judgments != (daemoncompose.JudgmentConfig{}) ||
		!reflect.DeepEqual(enabled.HostVerification, daemoncompose.MacverifyExecutorConfig{}) {
		t.Fatalf("passing check: %+v", enabled)
	}
	// Exercise the configured judgment fields too: only the launch's digest
	// differs between the prospective and enabled driver configurations.
	settings.Judgment.ClaudeBin = "/bin/claude"
	settings.Judgment.ClaudeSHA256 = strings.Repeat("e", 64)
	settings.Judgment.Model = "claude-opus"
	settings.Judgment.Effort = domain.EffortMedium
	settings.Judgment.PublicationAuthorPrompt = "/prompts/author.md"
	// And a configured executor, so the comparison covers HostVerification.
	settings.MacosVerification = &deputyMacverify{
		Root: "/Library/Freeside/macverify", Slots: []string{"fvslot1", "fvslot2"},
		XcodeBuild: "17F42", Swift: "swiftlang-6.1.2.1.2",
	}
	prospective := deputyClaudeDriverConfig(settings, paths, "")
	if prospective.HostVerification.Root != "/Library/Freeside/macverify" {
		t.Fatalf("prospective driver lost the executor: %+v", prospective.HostVerification)
	}
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
		_, err := daemonshell.ResolveListenAddress(s.Listen, noTailscale)
		return err
	}
	claudeConfig := func(s loadedDeputySettings, paths deputyPaths) error {
		return deputyClaudeDriverConfig(s, paths, "").Validate()
	}
	prompts := func(s loadedDeputySettings, paths deputyPaths) error {
		_, err := daemoncompose.ReadPromptPackages(deputyClaudeDriverConfig(s, paths, ""))
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
		}, manual, "manual submission config: unexpected end of JSON input"},
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

//nolint:paralleltest // Serial: deputyLeaseFixture sets HOME with t.Setenv.
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

// TestAdoptDeputyAgentTreeNamesAMissingExporter pins #2309: an exporter
// image the local container store no longer holds fails exporter_image,
// naming the reference and the reseed, before auth adopt runs against it.
//
//nolint:paralleltest // Serial: deputyLeaseFixture sets HOME with t.Setenv.
func TestAdoptDeputyAgentTreeNamesAMissingExporter(t *testing.T) {
	host, paths, _ := deputyLeaseFixture(t)
	settings := deputyDaemonSettingsFixture(t)
	settings.Images.Exporter = "127.0.0.1:5005/freeside-exporter@sha256:" + strings.Repeat("c", 64)
	host.inspectImage = func(_ context.Context, ref string) error {
		if ref != settings.Images.Exporter {
			t.Fatalf("inspected %s, want the exporter", ref)
		}
		return errors.New("inspect image " + ref + ": exit status 1\nError: image not found: " + ref)
	}
	// An earlier start's patch is cleared even though adopt does not run.
	if err := os.WriteFile(paths.Patch, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, ok := adoptDeputyAgentTree(t.Context(), host, paths, settings)
	if ok || !slices.Equal(result.Failed, []string{"exporter_image"}) {
		t.Fatalf("result %+v, %v; want exporter_image failed", result, ok)
	}
	for _, want := range []string{
		"images.exporter " + settings.Images.Exporter + " is not in the local container store",
		"container image pull --scheme http " + settings.Images.Exporter,
	} {
		if !strings.Contains(result.Remediation, want) {
			t.Fatalf("remediation %q lacks %q", result.Remediation, want)
		}
	}
	if strings.Contains(result.Remediation, "exit status") {
		t.Fatalf("remediation %q carries the CLI's output to the card", result.Remediation)
	}
	for _, path := range []string{paths.Patch, paths.AdoptReport, paths.AdoptLog} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s exists after an exporter failure (%v)", path, err)
		}
	}
}

func TestPublishCompositionEvidenceIsNoClobber(t *testing.T) {
	t.Parallel()
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
//
//nolint:paralleltest // Serial: deputyLeaseFixture sets HOME with t.Setenv.
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
