package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/agenttree"
	"github.com/freeside-ai/freeside/daemon/internal/daemonlock"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
	"github.com/freeside-ai/freeside/daemon/internal/projectimage"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

var prelaunchTestNow = time.Date(2026, 10, 24, 15, 0, 0, 0, time.UTC)

const prelaunchTestIssue = "https://github.com/acme/widgets/issues/7"

// prelaunchFixture is a deputy root with everything a launch needs: an
// adopted and committed agent tree, a database holding the identities, the
// repository's trust profile, and the agent image's provenance, and submit
// inputs for an issue of the managed repository.
type prelaunchFixture struct {
	root                                string
	paths                               deputyPaths
	settings                            map[string]any
	host                                prelaunchHost
	preflight                           *fakePreflightEnvironment
	prober                              *fakeLineupProber
	task, policy, publication, workUnit string
}

func newPrelaunchFixture(t *testing.T) *prelaunchFixture {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &prelaunchFixture{root: filepath.Join(base, "root")}
	f.paths = newDeputyPaths(filepath.Join(f.root, "daemon"))
	leaseRoot := filepath.Join(base, "rig-locks")
	for _, dir := range []string{leaseRoot, f.paths.StateRoot} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	adopt := newAuthAdoptFixture(t)
	// Adoption defaults its offer terms from the wall clock, and the tree's
	// lineup revision is in the golden record, so the fixture pins them to
	// what adoption derives at prelaunchTestNow.
	_, patch, err := adopt.run(t, adopt.args("-terms-basis-date", "2026-10-24", "-pricing-revision", "2026-10",
		"-offer-not-after", "2027-10-24T00:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	checkout, commit := commitAdoptedPatch(t, patch)
	f.settings = deputySettingsFixture(t)
	tree := f.settings["agent_tree"].(map[string]any)
	tree["checkout"], tree["commit"] = checkout, commit
	tree["auth_identity"], tree["review_auth_identity"] = "claude-main", "codex-review"
	tree["auth_volume"] = adoptClaudeVolume
	f.settings["review"].(map[string]any)["auth_snapshot"] = adopt.storePath
	f.settings["repository"].(map[string]any)["allowed_paths"] = []string{"daemon/**"}
	f.settings["seed_root"] = filepath.Join(base, "seed")
	f.writeSettings(t)

	f.task, f.policy, f.publication = writeSubmissionInputs(t, base)
	if err := os.WriteFile(f.task, []byte(prelaunchTestIssue+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	publication := `{"recipe":"freeside.client-publication/v2","source_issue":"` + prelaunchTestIssue +
		`","commit_author":{"app_slug":"freeside-test","bot_user_id":12345}}`
	if err := os.WriteFile(f.publication, []byte(publication), 0o600); err != nil {
		t.Fatal(err)
	}
	f.workUnit = declaredWorkUnit(t)

	settings := f.load(t)
	identities, err := deputyIdentities(settings)
	if err != nil {
		t.Fatal(err)
	}
	f.withStore(t, func(tx *store.InternalTx) error {
		for _, identity := range identities {
			if err := tx.RecordAuthIdentity(t.Context(), identity, prelaunchTestNow); err != nil {
				return err
			}
		}
		if err := tx.RecordTrustProfile(t.Context(), testPrelaunchTrustProfile(t), prelaunchTestNow); err != nil {
			return err
		}
		return tx.RecordProjectImage(t.Context(), prelaunchProjectImage(t, settings, settings.ApprovedRecipe))
	})

	_, f.preflight = preflightFixture(t)
	f.preflight.rig.Resources = daemonlock.RigResources{
		StateRoot: f.paths.StateRoot, DatabasePath: f.paths.DB, ListenAddress: settings.Listen,
		SeedRoot: settings.SeedRoot, LeaseRoot: leaseRoot,
	}
	f.preflight.database.ProfileRepo, f.preflight.database.ProfileRepositoryID = "acme/widgets", 42
	f.preflight.database.ProjectImage = prelaunchProjectImage(t, settings, settings.ApprovedRecipe)
	f.preflight.database.ReviewAuthStoreVolume = adopt.storePath
	f.prober = passingLineupProber()
	f.host = prelaunchHost{
		rig: &fakeRigHost{}, preflight: f.preflight, leaseRoot: leaseRoot,
		now: func() time.Time { return prelaunchTestNow }, build: "2dce6570ee23",
		preLease: func(loadedDeputySettings, deputyPaths) (deputyHomes, error) {
			home := filepath.Join(base, "home")
			return deputyHomes{home: home, account: home}, nil
		},
		prodAuthority: func(context.Context, deputyHomes, loadedDeputySettings, string, string) error { return nil },
		prober: func(*loadedDeputySettings, string, string) (lineupProber, error) {
			return f.prober, nil
		},
	}
	return f
}

func (f *prelaunchFixture) writeSettings(t *testing.T) {
	t.Helper()
	f.settings["writers"] = map[string]any{
		"specifier":   map[string]any{"model": "claude-fable-5-1", "effort": "high"},
		"implementer": map[string]any{"model": "claude-opus-5-5", "effort": "max"},
		"remediator":  map[string]any{"model": "claude-opus-5-5", "effort": "max"},
	}
	f.settings["review"].(map[string]any)["model"] = "gpt-6.1-sol"
	f.settings["review"].(map[string]any)["effort"] = "max"
	body, err := json.Marshal(f.settings)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.paths.Settings, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f *prelaunchFixture) load(t *testing.T) loadedDeputySettings {
	t.Helper()
	settings, err := loadDeputySettings(f.paths.Settings)
	if err != nil {
		t.Fatal(err)
	}
	return settings
}

func (f *prelaunchFixture) withStore(t *testing.T, write func(*store.InternalTx) error) {
	t.Helper()
	st := storetest.Open(t, f.paths.DB, store.Options{})
	if err := st.WriteInternal(t.Context(), write); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
}

func (f *prelaunchFixture) args(extra ...string) []string {
	return append([]string{
		"-root", f.root, "-task", f.task, "-policy", f.policy, "-publication", f.publication,
		"-work-unit", f.workUnit,
	}, extra...)
}

func (f *prelaunchFixture) run(t *testing.T, args []string) (prelaunchRecord, []byte, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	err := runPrelaunchCommand(t.Context(), args, &stdout, &stderr, f.host)
	var record prelaunchRecord
	if stdout.Len() != 0 {
		if jsonErr := json.Unmarshal(stdout.Bytes(), &record); jsonErr != nil {
			t.Fatalf("stdout is not a record: %v\n%s", jsonErr, stdout.String())
		}
	}
	return record, stdout.Bytes(), stderr.String(), err
}

func testPrelaunchTrustProfile(t *testing.T) domain.AutomationTrustProfile {
	t.Helper()
	profile, err := domain.NewAutomationTrustProfile(domain.AutomationTrustProfileInput{
		Repo: "acme/widgets", RepositoryID: 42,
		PRExecution:                domain.PRExecutionAuditedSameRepo,
		CandidateAutomationChanges: domain.AutomationChangesBlocked,
		PRGitHubTokenPermissions:   domain.TokenPermissionsReadOnly,
		CommitPlan:                 domain.CommitPlanSingleCommit,
		MessageRuleset:             domain.MessageRulesetGitHub1,
		WorkflowAuditDigest:        approveShadowReviewDigest("1"),
		Review: domain.ReviewSettings{
			Mode: domain.ReviewFreesideInvoked, ConfigDigest: approveShadowReviewDigest("2"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return profile
}

func prelaunchProjectImage(t *testing.T, settings loadedDeputySettings, recipe domain.Digest) domain.ProjectImage {
	t.Helper()
	image, err := domain.NewProjectImage(domain.ProjectImageInput{
		Repository: "acme/widgets", RepositoryID: 42, CommitSHA: settings.Repository.BaseSHA,
		RecipeDigest: recipe, PreparationCommand: []string{projectimage.PreparationPath},
		BaseImageRef: domain.ImageRef("registry.local/base@sha256:" + strings.Repeat("f", 64)),
		ImageRef:     domain.ImageRef(settings.Images.Agent),
	})
	if err != nil {
		t.Fatal(err)
	}
	return image
}

func findPrelaunchCheck(record prelaunchRecord, group prelaunchGroup, name string) prelaunchCheck {
	for _, check := range record.Checks {
		if check.Group == group && check.Name == name {
			return check
		}
	}
	return prelaunchCheck{}
}

func TestPrelaunchPassesOnAReadyRoot(t *testing.T) {
	t.Parallel()
	f := newPrelaunchFixture(t)
	record, body, stderr, err := f.run(t, f.args())
	if err != nil {
		t.Fatalf("prelaunch: %v\nstderr=%s\n%s", err, stderr, body)
	}
	if !record.Passed || record.FirstMissing != nil || record.ManifestDigest == nil {
		t.Fatalf("record %s", body)
	}
	written, err := os.ReadFile(filepath.Join(f.paths.StateRoot, prelaunchRecordName))
	if err != nil || !bytes.Equal(written, body) {
		t.Fatalf("prelaunch.json %q, %v; want the printed bytes", written, err)
	}
}

// TestPrelaunchRecordGolden pins prelaunch.json. The settings digest and the
// agent-tree commit name temporary paths and a commit time, so they and the
// manifest digest that covers them are normalized.
func TestPrelaunchRecordGolden(t *testing.T) {
	t.Parallel()
	f := newPrelaunchFixture(t)
	record, body, stderr, err := f.run(t, f.args())
	if err != nil {
		t.Fatalf("prelaunch: %v\nstderr=%s", err, stderr)
	}
	settings := f.load(t)
	normalized := strings.NewReplacer(
		string(settings.Digest), "sha256:<settings>",
		string(*record.ManifestDigest), "sha256:<manifest>",
		settings.AgentTree.Commit, "<agent-tree-commit>",
	).Replace(string(body))
	golden.Assert(t, "prelaunch_record", []byte(normalized))
}

func TestPrelaunchNamesTheFirstMissingPrerequisite(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		edit func(*testing.T, *prelaunchFixture)
		want prelaunchPrerequisite
		fix  string
	}{
		"fresh root": {func(t *testing.T, f *prelaunchFixture) {
			if err := os.Remove(f.paths.DB); err != nil {
				t.Fatal(err)
			}
		}, prelaunchNeedsAgentTree, "The First Start"},
		"commit not in the checkout": {func(t *testing.T, f *prelaunchFixture) {
			f.settings["agent_tree"].(map[string]any)["commit"] = strings.Repeat("d", 40)
			f.writeSettings(t)
		}, prelaunchNeedsAgentTree, "The First Start"},
		"no trust profile": {func(t *testing.T, f *prelaunchFixture) {
			f.resetStore(t, false, "")
		}, prelaunchNeedsTrustProfile, "freesided onboard acme/widgets -db "},
		"no project image": {func(t *testing.T, f *prelaunchFixture) {
			f.resetStore(t, true, "")
		}, prelaunchNeedsProjectImage, "project_image.image_ref"},
		"image for another recipe": {func(t *testing.T, f *prelaunchFixture) {
			f.resetStore(t, true, approveShadowReviewDigest("e"))
		}, prelaunchNeedsApprovedRecipe, string(approveShadowReviewDigest("e"))},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newPrelaunchFixture(t)
			tc.edit(t, f)
			record, body, stderr, err := f.run(t, f.args("-no-live"))
			if !errors.Is(err, errPrelaunchFailed) {
				t.Fatalf("err %v, want a failed prelaunch\n%s", err, body)
			}
			missing := record.FirstMissing
			if missing == nil || missing.Prerequisite != tc.want || !strings.Contains(missing.Fix, tc.fix) {
				t.Fatalf("first missing %+v, want %s with a fix naming %q", missing, tc.want, tc.fix)
			}
			if !strings.Contains(stderr, "first missing: "+string(tc.want)) {
				t.Fatalf("stderr %q does not name the first missing prerequisite", stderr)
			}
			// The walk names one step; every other check still reports.
			for _, check := range []struct {
				group prelaunchGroup
				name  string
			}{
				{prelaunchGroupSettings, "settings"},
				{prelaunchGroupInputs, "submission"},
				{prelaunchGroupRig, "rig_lease"},
				{prelaunchGroupRig, "installed_daemon"},
			} {
				if got := findPrelaunchCheck(record, check.group, check.name).Status; got != compositionPassed {
					t.Fatalf("%s %s = %q, want passed\n%s", check.group, check.name, got, body)
				}
			}
			if record.ManifestDigest == nil {
				t.Fatalf("composition did not run\n%s", body)
			}
		})
	}
}

// resetStore replaces the root's database with one holding the identities
// and, when profile is set, the trust profile, and when recipe is set, the
// agent image built for that recipe.
func (f *prelaunchFixture) resetStore(t *testing.T, profile bool, recipe domain.Digest) {
	t.Helper()
	for _, path := range []string{f.paths.DB, f.paths.DB + "-wal", f.paths.DB + "-shm"} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	settings := f.load(t)
	identities, err := deputyIdentities(settings)
	if err != nil {
		t.Fatal(err)
	}
	f.withStore(t, func(tx *store.InternalTx) error {
		for _, identity := range identities {
			if err := tx.RecordAuthIdentity(t.Context(), identity, prelaunchTestNow); err != nil {
				return err
			}
		}
		if profile {
			if err := tx.RecordTrustProfile(t.Context(), testPrelaunchTrustProfile(t), prelaunchTestNow); err != nil {
				return err
			}
		}
		if recipe != "" {
			return tx.RecordProjectImage(t.Context(), prelaunchProjectImage(t, settings, recipe))
		}
		return nil
	})
}

func TestPrelaunchFreshRootSkipsTheStateChecks(t *testing.T) {
	t.Parallel()
	f := newPrelaunchFixture(t)
	if err := os.Remove(f.paths.DB); err != nil {
		t.Fatal(err)
	}
	record, body, _, _ := f.run(t, f.args())
	for _, name := range []string{"state_database", "system_health", "identities"} {
		if got := findPrelaunchCheck(record, prelaunchGroupState, name).Status; got != compositionNotRun {
			t.Fatalf("state %s = %q, want not_run\n%s", name, got, body)
		}
	}
	if _, err := os.Stat(f.paths.DB); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("prelaunch created the database: %v", err)
	}
}

// TestPrelaunchLeavesTheRootAsItFound pins that prelaunch writes nothing a
// deputy start reads: the manifest binding submissions, the deputy's lease
// token, the state database's revision and identities, or the rig lease.
func TestPrelaunchLeavesTheRootAsItFound(t *testing.T) {
	t.Parallel()
	f := newPrelaunchFixture(t)
	manifest := []byte("{\"earlier\":\"manifest\"}\n")
	if err := os.WriteFile(f.paths.Manifest, manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	before := prelaunchStoreSnapshot(t, f.paths.DB)
	live, _, _, err := f.run(t, f.args())
	if err != nil {
		t.Fatal(err)
	}
	offline, _, _, err := f.run(t, f.args("-no-live"))
	if err != nil {
		t.Fatal(err)
	}
	if *live.ManifestDigest != *offline.ManifestDigest {
		t.Fatalf("manifest digest %s live, %s with -no-live; the live checks must not feed it",
			*live.ManifestDigest, *offline.ManifestDigest)
	}
	if offline.LineupProbe != nil {
		t.Fatalf("-no-live ran the lineup probe: %+v", offline.LineupProbe)
	}
	if got, err := os.ReadFile(f.paths.Manifest); err != nil || !bytes.Equal(got, manifest) {
		t.Fatalf("composition manifest %q, %v; want it untouched", got, err)
	}
	for _, path := range []string{f.paths.Acquisition, filepath.Join(f.paths.StateRoot, prelaunchAcquisitionName)} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("prelaunch left %s: %v", path, err)
		}
	}
	if after := prelaunchStoreSnapshot(t, f.paths.DB); !reflect.DeepEqual(before, after) {
		t.Fatalf("store changed: before %+v, after %+v", before, after)
	}
	// The lease was released: the deputy can take it.
	settings := f.load(t)
	acquire, err := deputyRigAcquireConfig(f.paths, settings, f.host.leaseRoot, deputyOwnerNote)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := daemonlock.AcquireRig(acquire)
	if err != nil {
		t.Fatalf("lease after prelaunch: %v", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
}

// prelaunchSnapshot is what a run must leave as it was: the revision the
// system-health read reports and the identity and enrollment rows.
type prelaunchSnapshot struct {
	AsOfRevision int64
	Identities   []domain.AuthIdentity
	Enrollments  []domain.ClientEnrollment
}

func prelaunchStoreSnapshot(t *testing.T, dbPath string) prelaunchSnapshot {
	t.Helper()
	st, err := store.OpenReadOnly(t.Context(), dbPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	health, err := readSystemHealthFrom(t.Context(), st)
	if err != nil {
		t.Fatal(err)
	}
	snap := prelaunchSnapshot{AsOfRevision: health.AsOfRevision}
	if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
		if snap.Identities, err = tx.ListAuthIdentities(t.Context()); err != nil {
			return err
		}
		for _, identity := range snap.Identities {
			enrollments, err := tx.ListClientEnrollments(t.Context(), identity.ID)
			if err != nil {
				return err
			}
			snap.Enrollments = append(snap.Enrollments, enrollments...)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(snap.Identities) != 2 {
		t.Fatalf("fixture identities %+v, want two", snap.Identities)
	}
	return snap
}

func TestPrelaunchReportsTheRigHolder(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		hold func(*testing.T, *prelaunchFixture) func()
		fix  func(*prelaunchFixture) string
	}{
		"live holder": {func(t *testing.T, f *prelaunchFixture) func() {
			lease := f.holdRig(t, f.paths.StateRoot, deputyOwnerNote)
			return func() { _ = lease.Close() }
		}, func(*prelaunchFixture) string { return "stop the holder" }},
		"this root's stale lease": {func(t *testing.T, f *prelaunchFixture) func() {
			if err := f.holdRig(t, f.paths.StateRoot, deputyOwnerNote).Abandon(); err != nil {
				t.Fatal(err)
			}
			return func() {}
		}, func(f *prelaunchFixture) string { return "this root's deputy start recovers" }},
		"another root's stale lease": {func(t *testing.T, f *prelaunchFixture) func() {
			other := filepath.Join(filepath.Dir(f.root), "other", "daemon")
			if err := os.MkdirAll(other, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := f.holdRig(t, other, "").Abandon(); err != nil {
				t.Fatal(err)
			}
			return func() {}
		}, func(f *prelaunchFixture) string {
			return "freesided rig recover -state-root " + filepath.Join(filepath.Dir(f.root), "other", "daemon")
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newPrelaunchFixture(t)
			defer tc.hold(t, f)()
			record, body, _, err := f.run(t, f.args())
			if !errors.Is(err, errPrelaunchFailed) {
				t.Fatalf("err %v, want a failed prelaunch", err)
			}
			lease := findPrelaunchCheck(record, prelaunchGroupRig, "rig_lease")
			if lease.Status != compositionFailed || !strings.Contains(lease.Fix, tc.fix(f)) {
				t.Fatalf("rig_lease %+v, want a failure whose fix names %q", lease, tc.fix(f))
			}
			for _, check := range []string{"composition", "lineup_probe"} {
				if !slices.ContainsFunc(record.Checks, func(c prelaunchCheck) bool {
					return c.Name == check && c.Status == compositionNotRun
				}) {
					t.Fatalf("%s ran without the lease\n%s", check, body)
				}
			}
			if got := findPrelaunchCheck(record, prelaunchGroupSettings, "settings").Status; got != compositionPassed {
				t.Fatalf("settings = %q, want passed", got)
			}
			// Prelaunch never recovers a lease: a stale one stays for its
			// own root's deputy or rig recover.
			if name == "this root's stale lease" {
				f.requireStaleLease(t)
			}
		})
	}
}

// holdRig takes a lease on the fixture's listener and seed root for
// stateRoot, as a deputy on that root would.
func (f *prelaunchFixture) holdRig(t *testing.T, stateRoot, note string) *daemonlock.RigLease {
	t.Helper()
	settings := f.load(t)
	lease, err := daemonlock.AcquireRig(daemonlock.RigAcquireConfig{
		Owner:     daemonlock.RigOwner{User: "operator", Host: "host", PID: 4242, Note: note},
		StateRoot: stateRoot, DatabasePath: filepath.Join(stateRoot, "freeside.db"),
		ListenAddress: settings.Listen, SeedRoot: settings.SeedRoot, LeaseRoot: f.host.leaseRoot,
	})
	if err != nil {
		t.Fatal(err)
	}
	return lease
}

func TestPrelaunchRefusesARootADaemonHolds(t *testing.T) {
	t.Parallel()
	f := newPrelaunchFixture(t)
	lock, err := daemonlock.Acquire(f.paths.DB)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()
	record, body, _, err := f.run(t, f.args())
	if !errors.Is(err, errPrelaunchFailed) {
		t.Fatalf("err %v, want a failed prelaunch", err)
	}
	if got := findPrelaunchCheck(record, prelaunchGroupState, "state_database").Status; got != compositionFailed {
		t.Fatalf("state_database = %q, want failed\n%s", got, body)
	}
}

func TestPrelaunchChecksTaskLinesAgainstTheTree(t *testing.T) {
	t.Parallel()
	f := newPrelaunchFixture(t)
	record, body, stderr, err := f.run(t, f.args("-no-live", "--task-line", "implementer=nobody"))
	if !errors.Is(err, errPrelaunchFailed) {
		t.Fatalf("err %v, want a failed prelaunch", err)
	}
	if got := findPrelaunchCheck(record, prelaunchGroupInputs, "task_lines").Status; got != compositionFailed ||
		!strings.Contains(stderr, "implementer=nobody") {
		t.Fatalf("task_lines = %q, stderr %q\n%s", got, stderr, body)
	}
	tree := loadAgentTreeAt(t, f.load(t))
	record, body, _, err = f.run(t, f.args("-no-live", "--task-line", "implementer="+tree.Agents[0].Name))
	if err != nil || findPrelaunchCheck(record, prelaunchGroupInputs, "task_lines").Status != compositionPassed {
		t.Fatalf("a task line naming %s: %v\n%s", tree.Agents[0].Name, err, body)
	}
}

func loadAgentTreeAt(t *testing.T, settings loadedDeputySettings) agenttree.Tree {
	t.Helper()
	files, err := agenttree.ReadCommit(t.Context(), settings.AgentTree.Checkout, t.TempDir(), settings.AgentTree.Commit)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := agenttree.Parse(files)
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

func TestPrelaunchNoLiveSkipsTheLineupProbe(t *testing.T) {
	t.Parallel()
	f := newPrelaunchFixture(t)
	record, body, _, err := f.run(t, f.args("-no-live"))
	if err != nil {
		t.Fatal(err)
	}
	if got := findPrelaunchCheck(record, prelaunchGroupLive, "lineup_probe").Status; got != compositionNotRun {
		t.Fatalf("lineup_probe = %q, want not_run\n%s", got, body)
	}
	if len(f.prober.calls) != 0 {
		t.Fatalf("-no-live probed %d calls", len(f.prober.calls))
	}
	if _, err := os.Stat(filepath.Join(f.paths.StateRoot, lineupProbeRecordName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("-no-live wrote the lineup record: %v", err)
	}
}

// TestPrelaunchWritesTheLineupRecordAsProbeLineupDoes pins that the probe
// under prelaunch leaves the bytes probe-lineup's golden pins, for the same
// lineup, build, and time; only the settings digest differs.
func TestPrelaunchWritesTheLineupRecordAsProbeLineupDoes(t *testing.T) {
	t.Parallel()
	f := newPrelaunchFixture(t)
	f.settings["judgment"] = map[string]any{
		"claude_bin": "/opt/claude/bin/claude", "claude_sha256": strings.Repeat("e", 64),
		"model": "claude-sonnet-5-5", "effort": "medium", "publication_author_prompt": "/prompts/author.md",
	}
	f.writeSettings(t)
	f.host.build = "freesided 0.0.0-test"
	f.host.now = func() time.Time { return probeLineupTime }
	record, body, _, _ := f.run(t, f.args())
	if record.LineupProbe == nil {
		t.Fatalf("the probe did not run\n%s", body)
	}
	got, err := os.ReadFile(filepath.Join(f.paths.StateRoot, lineupProbeRecordName))
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join("testdata", "lineup_probe_record.golden"))
	if err != nil {
		t.Fatal(err)
	}
	normalized := strings.ReplaceAll(string(got), string(*record.SettingsDigest), "sha256:"+strings.Repeat("f", 64))
	if normalized != string(want) {
		t.Fatalf("lineup-probe.json differs from probe-lineup's golden:\n%s", got)
	}
	embedded, err := json.MarshalIndent(record.LineupProbe, "", "  ")
	if err != nil || !bytes.Equal(append(embedded, '\n'), got) {
		t.Fatalf("the record's lineup_probe is not lineup-probe.json: %v", err)
	}
}

func TestPrelaunchLineupFailureFailsTheRun(t *testing.T) {
	t.Parallel()
	f := newPrelaunchFixture(t)
	f.prober.errs = map[string]error{fakeProbeKey(lineupSurfaceReviewImage, "gpt-6.1-sol"): errors.New("boom")}
	record, _, stderr, err := f.run(t, f.args())
	if !errors.Is(err, errPrelaunchFailed) ||
		findPrelaunchCheck(record, prelaunchGroupLive, "lineup_probe").Status != compositionFailed ||
		!strings.Contains(stderr, "live lineup_probe") {
		t.Fatalf("err %v, stderr %q; want a failed lineup probe", err, stderr)
	}
}

func TestPrelaunchRequiresTheWorkUnitAsSubmitDoes(t *testing.T) {
	t.Parallel()
	f := newPrelaunchFixture(t)
	args := []string{"-root", f.root, "-task", f.task, "-policy", f.policy, "-publication", f.publication, "-no-live"}
	record, body, _, err := f.run(t, args)
	submission := findPrelaunchCheck(record, prelaunchGroupInputs, "submission")
	if !errors.Is(err, errPrelaunchFailed) || submission.Status != compositionFailed ||
		!strings.Contains(submission.Evidence, errWorkUnitRequired.Error()) {
		t.Fatalf("err %v\n%s", err, body)
	}
}

func TestPrelaunchUsage(t *testing.T) {
	t.Parallel()
	f := newPrelaunchFixture(t)
	for name, args := range map[string][]string{
		"no root":       {"-task", f.task, "-policy", f.policy, "-publication", f.publication},
		"relative root": {"-root", "root", "-task", f.task, "-policy", f.policy, "-publication", f.publication},
		"no task":       {"-root", f.root, "-policy", f.policy, "-publication", f.publication},
		"extra":         append(f.args(), "extra"),
	} {
		if _, _, _, err := f.run(t, args); !errors.Is(err, errPrelaunchUsage) {
			t.Fatalf("%s: %v, want a usage error", name, err)
		}
	}
}

func TestPrelaunchUnreadableSettingsStopEverything(t *testing.T) {
	t.Parallel()
	f := newPrelaunchFixture(t)
	if err := os.WriteFile(f.paths.Settings, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	record, body, _, err := f.run(t, f.args())
	if !errors.Is(err, errPrelaunchFailed) || record.SettingsDigest != nil {
		t.Fatalf("err %v\n%s", err, body)
	}
	for _, check := range record.Checks[1:] {
		if check.Status != compositionNotRun {
			t.Fatalf("%s %s ran without settings", check.Group, check.Name)
		}
	}
}

func TestPrelaunchEnumRegistration(t *testing.T) {
	t.Parallel()
	groups := map[prelaunchGroup]bool{}
	for _, group := range AllPrelaunchGroups {
		if !group.valid() || groups[group] {
			t.Fatalf("group %q is invalid or registered twice", group)
		}
		groups[group] = true
	}
	prerequisites := map[prelaunchPrerequisite]bool{}
	for _, prerequisite := range AllPrelaunchPrerequisites {
		if !prerequisite.valid() || prerequisites[prerequisite] {
			t.Fatalf("prerequisite %q is invalid or registered twice", prerequisite)
		}
		prerequisites[prerequisite] = true
	}
	if prelaunchGroup("").valid() || prelaunchGroup("unknown").valid() ||
		prelaunchPrerequisite("").valid() || prelaunchPrerequisite("unknown").valid() {
		t.Fatal("an unregistered value passed validation")
	}
	// Every check the command records belongs to a registered group.
	f := newPrelaunchFixture(t)
	record, _, _, _ := f.run(t, f.args())
	for _, check := range record.Checks {
		if !groups[check.Group] || !check.Status.valid() {
			t.Fatalf("check %+v has an unregistered group or status", check)
		}
	}
}

func TestPrelaunchRefusesASourceAnUnfinishedTaskHolds(t *testing.T) {
	t.Parallel()
	f := newPrelaunchFixture(t)
	settings := f.load(t)
	// The fixture's store predates a topic key; a deputy start creates one.
	if _, err := loadOrCreateTopicKey(f.paths.DB, false); err != nil {
		t.Fatal(err)
	}
	if _, err := runSubmitCommand(t.Context(), submitCommandConfig{
		SubmissionID: "earlier", DBPath: f.paths.DB,
		TaskPath: f.task, PolicyPath: f.policy, WorkUnitPath: f.workUnit, PublicationPath: f.publication,
		ProjectID: settings.Repository.Project,
	}); err != nil {
		t.Fatal(err)
	}
	record, body, stderr, err := f.run(t, f.args("-no-live"))
	if !errors.Is(err, errPrelaunchFailed) {
		t.Fatalf("err %v, want a failed prelaunch\n%s", err, body)
	}
	check := findPrelaunchCheck(record, prelaunchGroupInputs, "duplicate_source")
	if check.Status != compositionFailed || !strings.Contains(check.Evidence, "task task-") ||
		!strings.Contains(stderr, "duplicate_source") {
		t.Fatalf("duplicate_source %+v, stderr %q; want a failure naming the unfinished task", check, stderr)
	}
}

func TestPrelaunchBindsTheCommitAuthorToTheLaunch(t *testing.T) {
	t.Parallel()
	f := newPrelaunchFixture(t)
	publication := `{"recipe":"freeside.client-publication/v2","source_issue":"` + prelaunchTestIssue +
		`","commit_author":{"app_slug":"someone-else","bot_user_id":999}}`
	if err := os.WriteFile(f.publication, []byte(publication), 0o600); err != nil {
		t.Fatal(err)
	}
	record, body, _, err := f.run(t, f.args("-no-live"))
	if !errors.Is(err, errPrelaunchFailed) {
		t.Fatalf("err %v, want a failed prelaunch\n%s", err, body)
	}
	check := findPrelaunchCheck(record, prelaunchGroupInputs, "launch_binding")
	if check.Status != compositionFailed || !strings.Contains(check.Evidence, "someone-else[bot]") ||
		!strings.Contains(check.Fix, `app_slug "freeside-test"`) {
		t.Fatalf("launch_binding %+v, want a failure naming both bots", check)
	}
	if got := findPrelaunchCheck(record, prelaunchGroupInputs, "submission").Status; got != compositionPassed {
		t.Fatalf("submission = %q; the inputs pass on their own", got)
	}
}

// TestPrelaunchAbandonsALeaseItCannotRelease pins that a rig prelaunch can't
// prove clear is left stale for this root's deputy start to recover, and
// that the failure says so.
func TestPrelaunchAbandonsALeaseItCannotRelease(t *testing.T) {
	t.Parallel()
	f := newPrelaunchFixture(t)
	rig := &fakeRigHost{}
	f.host.rig = rig
	f.host.prodAuthority = func(context.Context, deputyHomes, loadedDeputySettings, string, string) error {
		rig.runtimeCLI = true // a runtime CLI still running blocks the clear proof
		return nil
	}
	record, body, _, err := f.run(t, f.args("-no-live"))
	if !errors.Is(err, errPrelaunchFailed) {
		t.Fatalf("err %v, want a failed prelaunch\n%s", err, body)
	}
	lease := findPrelaunchCheck(record, prelaunchGroupRig, "rig_lease")
	if lease.Status != compositionFailed || !strings.Contains(lease.Evidence, "could not be released cleanly") ||
		!strings.Contains(lease.Fix, "recovers it") {
		t.Fatalf("rig_lease %+v, want the release failure", lease)
	}
	if _, err := os.Stat(filepath.Join(f.paths.StateRoot, prelaunchAcquisitionName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the lease token outlived the run: %v", err)
	}
	f.requireStaleLease(t)
}

// requireStaleLease fails unless the rig lease for this root is stale: held
// by no process and left for recovery.
func (f *prelaunchFixture) requireStaleLease(t *testing.T) {
	t.Helper()
	settings := f.load(t)
	lease, err := daemonlock.AcquireRig(daemonlock.RigAcquireConfig{
		Owner:     daemonlock.RigOwner{User: "operator", Host: "host", PID: 4242},
		StateRoot: f.paths.StateRoot, DatabasePath: f.paths.DB,
		ListenAddress: settings.Listen, SeedRoot: settings.SeedRoot, LeaseRoot: f.host.leaseRoot,
	})
	if stale := (*daemonlock.RigStaleError)(nil); !errors.As(err, &stale) {
		if lease != nil {
			_ = lease.Close()
		}
		t.Fatalf("acquire after prelaunch: %v, want the stale lease left for recovery", err)
	}
}

// TestPrelaunchNamesTheTreeMissingOnlyForAnUnresolvedLineup pins that the
// walk reads a failed agent_selection as a missing tree only when a role has
// no lineup line that resolves, not when the store read failed.
func TestPrelaunchNamesTheTreeMissingOnlyForAnUnresolvedLineup(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		err  error
		tree bool
	}{
		"unresolved role": {&roleAdmissionError{Role: domain.RoleReviewer, Err: engine.ErrAgentNotAdmissible}, true},
		"unreadable":      {errors.New("store unreadable"), false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newPrelaunchFixture(t)
			f.preflight.agentsError = tc.err
			record, body, _, err := f.run(t, f.args("-no-live"))
			if !errors.Is(err, errPrelaunchFailed) {
				t.Fatalf("err %v, want a failed prelaunch\n%s", err, body)
			}
			got := record.FirstMissing != nil && record.FirstMissing.Prerequisite == prelaunchNeedsAgentTree
			if got != tc.tree {
				t.Fatalf("first_missing %+v, want agent_tree %v\n%s", record.FirstMissing, tc.tree, body)
			}
		})
	}
}
