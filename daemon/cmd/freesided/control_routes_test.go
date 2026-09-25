package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/daemonlock"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/observe"
	"github.com/freeside-ai/freeside/daemon/internal/observe/observedb"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func TestControlLegacySubmissionNearInputLimits(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "freeside.db")
	h, err := run(t.Context(), nil, config{Environment: environmentEphemeral, DBPath: dbPath, ListenAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	})
	taskPath, policyPath, publicationPath := writeSubmissionInputs(t, root)
	if err := os.WriteFile(taskPath, bytes.Repeat([]byte("x"), maxSubmissionFileBytes), 0o600); err != nil {
		t.Fatal(err)
	}
	var keys []domain.PolicyKey
	if err := json.Unmarshal([]byte(submissionPolicyBody("", strings.Repeat("ab", 32))), &keys); err != nil {
		t.Fatal(err)
	}
	emptyPolicy, err := json.Marshal(keys)
	if err != nil {
		t.Fatal(err)
	}
	// The raw file fits easily, but canonical JSON escapes each '&' as six
	// bytes. The accepted canonical policy and its three other representations
	// (Keys, ResolvedPolicy.Keys, and declared paths) approach the file cap.
	keys[0].Value = strings.Repeat("&", (maxSubmissionFileBytes-len(emptyPolicy))/6)
	var policy bytes.Buffer
	encoder := json.NewEncoder(&policy)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(keys); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(policyPath, policy.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	canonicalPolicy, err := json.Marshal(keys)
	if err != nil || len(canonicalPolicy) > maxSubmissionFileBytes || len(canonicalPolicy) < maxSubmissionFileBytes-6 {
		t.Fatalf("canonical policy size %d: %v", len(canonicalPolicy), err)
	}
	declaration := []byte(`{"completion_criterion":"bound_pr_merged","depends_on_issues":[`)
	for issue := int64(1_000_000_000_000_000_000); len(declaration)+22 < maxSubmissionFileBytes; issue++ {
		if declaration[len(declaration)-1] != '[' {
			declaration = append(declaration, ',')
		}
		declaration = strconv.AppendInt(declaration, issue, 10)
	}
	declaration = append(declaration, ']', '}')
	workUnitPath := filepath.Join(root, "work-unit.json")
	if err := os.WriteFile(workUnitPath, declaration, 0o600); err != nil {
		t.Fatal(err)
	}
	// Even without the envelope or declared paths, these transmitted fields
	// exceed the old 20 MiB cap. Both request decoding layers must admit them.
	minimumRequestBytes := 2*4*((maxSubmissionFileBytes-6)/3) + 2*len(canonicalPolicy) + len(declaration)
	if minimumRequestBytes <= 20<<20 {
		t.Fatalf("fixture no longer exceeds the old request cap: %d", minimumRequestBytes)
	}
	// Legacy production retries remain accepted without the newer 1 MiB
	// specification-queue contract. Seed through the production constructor,
	// then prove the same CLI input succeeds offline before trying the socket.
	spec, err := readSubmissionFile(taskPath)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := domain.NewResolvedPolicy("run-large-legacy", keys)
	if err != nil {
		t.Fatal(err)
	}
	policyBody, err := json.Marshal(resolved.Keys)
	if err != nil {
		t.Fatal(err)
	}
	specArtifact, err := engine.SubmissionArtifact(domain.ArtifactKindSpecification, spec.digest, domain.EvidenceMediaTextMarkdown, int64(len(spec.body)))
	if err != nil {
		t.Fatal(err)
	}
	policyArtifact, err := engine.SubmissionArtifact(domain.ArtifactKindPolicy, resolved.Digest, domain.EvidenceMediaApplicationJSON, int64(len(policyBody)))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.Write(t.Context(), func(tx *store.WriteTx) error {
		if err := tx.PutArtifact(t.Context(), specArtifact); err != nil {
			return err
		}
		return tx.PutArtifact(t.Context(), policyArtifact)
	}); err != nil {
		t.Fatal(err)
	}
	publicationFile, err := readSubmissionFile(publicationPath)
	if err != nil {
		t.Fatal(err)
	}
	var publication engine.ProductionPublication
	if err := json.Unmarshal(publicationFile.body, &publication); err != nil {
		t.Fatal(err)
	}
	var declared submittedWorkUnit
	if err := json.Unmarshal(declaration, &declared); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.SubmitProductionRun(t.Context(), h.store, engine.ProductionRunSpec{
		RunID: resolved.RunID, ProjectID: "project-control", SpecArtifactID: specArtifact.ID,
		PolicyArtifactID: policyArtifact.ID, ResolvedPolicy: resolved, Publication: publication,
		WorkUnit: &domain.WorkUnitDeclarationInput{
			CompletionCriterion: declared.CompletionCriterion, DependsOnIssues: declared.DependsOnIssues,
			DeclaredPaths: engine.DeclaredPathScope(keys),
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	cfg := submitCommandConfig{
		RunID: resolved.RunID, DBPath: dbPath, TaskPath: taskPath,
		PolicyPath: policyPath, PublicationPath: publicationPath, WorkUnitPath: workUnitPath,
		ProjectID: "project-control",
	}
	direct, err := runSubmitCommand(t.Context(), cfg)
	if err != nil {
		t.Fatalf("offline legacy replay: %v", err)
	}
	restarted, err := run(t.Context(), nil, config{Environment: environmentEphemeral, DBPath: dbPath, ListenAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	h = restarted
	ctx := context.WithValue(t.Context(), directStoreOpenerKey{}, directStoreOpener(func(context.Context, string, store.Options, storeOpenMode) (*store.Store, error) {
		return nil, errors.New("large live submission attempted a direct store open")
	}))
	result, err := runSubmitCommand(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if result.WorkUnitID == "" || !reflect.DeepEqual(result, direct) {
		t.Fatalf("socket replay = %+v; direct replay = %+v", result, direct)
	}
}

func TestControlRoutesUseRunningDaemonStore(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "freeside.db")
	h, err := run(t.Context(), nil, config{
		Environment: environmentEphemeral,
		DBPath:      dbPath, ListenAddr: "127.0.0.1:0",
		FakeDriverEnabled: true, SeedWalkingSkeleton: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	})
	address, err := os.Stat(dbPath + ".control.json")
	if err != nil || address.Mode().Perm() != 0o600 {
		t.Fatalf("control address: %v, mode %v", err, address)
	}
	var directOpens atomic.Int64
	ctx := context.WithValue(t.Context(), directStoreOpenerKey{}, directStoreOpener(func(context.Context, string, store.Options, storeOpenMode) (*store.Store, error) {
		directOpens.Add(1)
		return nil, errors.New("unexpected direct store open")
	}))
	ctx = context.WithValue(ctx, directObservationOpenerKey{}, directObservationOpener(func(context.Context, string, ...domain.Digest) (*observedb.Store, error) {
		directOpens.Add(1)
		return nil, errors.New("unexpected direct observation open")
	}))
	if _, err := h.workflow.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	var snapshot bytes.Buffer
	if err := observe.Run(ctx, []string{"-db", dbPath, "-run", string(defaultFakeRunID), "-snapshot"}, &snapshot, io.Discard, openObservation); err != nil {
		t.Fatal(err)
	}
	if !json.Valid(snapshot.Bytes()) {
		t.Fatal("follow snapshot is not JSON")
	}
	var measures bytes.Buffer
	if err := runComprehensionCommand(ctx, []string{"-db", dbPath}, &measures, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !json.Valid(measures.Bytes()) {
		t.Fatal("comprehension measures are not JSON")
	}
	taskPath, policyPath, publicationPath := writeSubmissionInputs(t, root)
	submitted, err := runSubmitCommand(ctx, submitCommandConfig{
		SubmissionID: "control-submit", DBPath: dbPath, TaskPath: taskPath,
		PolicyPath: policyPath, PublicationPath: publicationPath, ProjectID: "project-control",
	})
	if err != nil {
		t.Fatal(err)
	}
	if submitted.SpecificationRunID == "" {
		t.Fatal("submission did not create a specification run")
	}
	var submittedTask domain.TaskID
	if err := h.store.Read(ctx, func(tx *store.ReadTx) error {
		run, err := tx.GetRun(ctx, submitted.SpecificationRunID)
		if err == nil {
			submittedTask = run.TaskID
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if submittedTask == "" {
		t.Fatal("submission has no task")
	}
	if _, err := runAbandonCommand(ctx, abandonCommandConfig{DBPath: dbPath, TaskID: submittedTask}); err != nil {
		t.Fatal(err)
	}
	liveID := domain.RunID("run-control-resume")
	var liveTask domain.TaskID
	if err := h.store.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, domain.Run{ID: liveID, ProjectID: "project-control", SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy"}); err != nil {
			return err
		}
		created, err := tx.GetRun(ctx, liveID)
		if err != nil {
			return err
		}
		liveTask = created.TaskID
		invocation := domain.InvocationID("inv-control-resume")
		return tx.AppendRunMilestone(ctx, domain.RunMilestone{RunID: liveID, Kind: domain.MilestoneRunSubmitted, InvocationID: &invocation, RecordedAt: time.Now().UTC()})
	}); err != nil {
		t.Fatal(err)
	}
	var resumed bytes.Buffer
	if err := runResumeCommand(ctx, []string{"-db", dbPath, "-task", string(liveTask), "-once"}, &resumed, io.Discard); err != nil {
		t.Fatal(err)
	}
	if resumed.Len() == 0 {
		t.Fatal("resume did not print an observation")
	}
	var emptyTask domain.Task
	if err := h.store.Write(ctx, func(tx *store.WriteTx) error {
		var err error
		emptyTask, err = tx.GetOrCreateTask(ctx, "project-control", domain.SpecificationSource{
			Kind:         domain.SpecificationSourceIssueSubject,
			IssueSubject: &domain.IssueSubjectRef{Repo: "owner/repo", RepositoryID: 1, IssueNumber: 1510},
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	err = runResumeCommand(ctx, []string{"-db", dbPath, "-task", string(emptyTask.ID), "-once"}, io.Discard, io.Discard)
	if !errors.Is(err, observe.ErrUsage) || !strings.Contains(err.Error(), "has no runs") {
		t.Fatalf("empty task resume = %v", err)
	}
	err = runResumeCommand(ctx, []string{"-db", dbPath, "-task", "task-missing", "-once"}, io.Discard, io.Discard)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing task resume = %v", err)
	}
	var defects bytes.Buffer
	claim := "sha256:" + strings.Repeat("c", 64)
	var itemID domain.ItemID
	if err := h.store.Read(ctx, func(tx *store.ReadTx) error {
		items, err := tx.ListAttentionItems(ctx)
		if err == nil && len(items) > 0 {
			itemID = items[0].Value.ID
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if itemID == "" {
		t.Fatal("seeded daemon has no attention item")
	}
	if err := runComprehensionCommand(ctx, []string{"record-defect", "-db", dbPath, "-item", string(itemID), "-claim", claim, "-reason", "missed context"}, &defects, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !json.Valid(defects.Bytes()) {
		t.Fatal("defect result is not JSON")
	}
	profile := approveShadowReviewProfile(t)
	if err := h.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordTrustProfile(ctx, profile, time.Now().UTC())
	}); err != nil {
		t.Fatal(err)
	}
	var shadow bytes.Buffer
	shadowArgs := []string{profile.Repo, "-db", dbPath, "-source", string(domain.ShadowReviewClaudeLocal), "-configuration-digest", string(approveShadowReviewDigest("a"))}
	if err := runApproveShadowReviewCommand(ctx, shadowArgs, &shadow, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !json.Valid(shadow.Bytes()) {
		t.Fatal("shadow review result is not JSON")
	}
	var doctor bytes.Buffer
	digest := "sha256:" + strings.Repeat("a", 64)
	err = runDoctorCommand(ctx, []string{"-db", dbPath, "-backend-configuration-digest", digest}, &doctor, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "unhealthy") || !json.Valid(doctor.Bytes()) {
		t.Fatalf("doctor result = %s, err = %v", doctor.String(), err)
	}
	if directOpens.Load() != 0 {
		t.Fatalf("%d direct opens while daemon held the lock", directOpens.Load())
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	var direct bytes.Buffer
	if err := observe.Run(t.Context(), []string{"-db", dbPath, "-run", string(defaultFakeRunID), "-snapshot"}, &direct, io.Discard, openObservation); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(snapshot.Bytes(), direct.Bytes()) {
		t.Fatalf("socket and direct snapshots differ:\n%s\n%s", snapshot.String(), direct.String())
	}
}

func TestControlRefusesHeldLockWithoutSocket(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "freeside.db")
	lock, err := daemonlock.Acquire(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close() //nolint:errcheck // test lock is released at function end
	taskPath, policyPath, publicationPath := writeSubmissionInputs(t, root)
	recipePath := filepath.Join(root, "verify.json")
	if err := os.WriteFile(recipePath, []byte(`{"commands":[["go","test","./..."]],"capture":"none"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(t.Context(), directStoreOpenerKey{}, directStoreOpener(func(context.Context, string, store.Options, storeOpenMode) (*store.Store, error) {
		t.Fatal("direct store opener called behind held daemon lock")
		return nil, nil
	}))
	digest := "sha256:" + strings.Repeat("a", 64)
	for name, run := range map[string]func() error{
		"follow": func() error {
			return observe.Run(ctx, []string{"-db", dbPath, "-run", "run-1", "-snapshot"}, io.Discard, io.Discard, openObservation)
		},
		"resume": func() error {
			return runResumeCommand(ctx, []string{"-db", dbPath, "-run", "run-1", "-once"}, io.Discard, io.Discard)
		},
		"submit": func() error {
			_, err := runSubmitCommand(ctx, submitCommandConfig{DBPath: dbPath, TaskPath: taskPath, PolicyPath: policyPath, PublicationPath: publicationPath, ProjectID: "project-control", SubmissionID: "held-lock"})
			return err
		},
		"abandon": func() error {
			_, err := runAbandonCommand(ctx, abandonCommandConfig{DBPath: dbPath, TaskID: "task-1"})
			return err
		},
		"reattempt": func() error {
			_, err := runReattemptCommand(ctx, reattemptCommandConfig{DBPath: dbPath, ParentRunID: "run-1", Reason: "Retry"})
			return err
		},
		"comprehension measures": func() error { return runComprehensionCommand(ctx, []string{"-db", dbPath}, io.Discard, io.Discard) },
		"comprehension defect": func() error {
			return runComprehensionCommand(ctx, []string{"record-defect", "-db", dbPath, "-item", "item-1", "-claim", digest, "-reason", "reason"}, io.Discard, io.Discard)
		},
		"shadow review": func() error {
			return runApproveShadowReviewCommand(ctx, []string{"owner/repo", "-db", dbPath, "-source", string(domain.ShadowReviewClaudeLocal), "-configuration-digest", digest}, io.Discard, io.Discard)
		},
		"doctor": func() error {
			return runDoctorCommand(ctx, []string{"-db", dbPath, "-backend-configuration-digest", digest}, io.Discard, io.Discard)
		},
		"onboard": func() error {
			return runOnboardCommand(ctx, []string{
				"example/repo", "-db", dbPath,
				"-state-dir", filepath.Join(root, "state"),
				"-registration-id", "11", "-repository-id", "44",
				"-commit", "0123456789012345678901234567890123456789",
				"-base-ref", "main", "-base-image", "example.invalid/agent@sha256:test",
				"-base-build-ref", "local/agent:test", "-review-config-digest", "sha256:review",
				"-recipe", recipePath,
			}, io.Discard, io.Discard)
		},
		"renew-codex": func() error {
			return runRenewCodexCommand(ctx, []string{
				"-db", dbPath, "-auth-identity", "identity-1",
				"-auth-store-root", root, "-auth-store", filepath.Join(root, "auth.json"),
			}, io.Discard, io.Discard)
		},
		"enroll-codex": func() error {
			return runEnrollCodexCommand(ctx, []string{
				"-db", dbPath, "-project", "project-1", "-auth-identity", "identity-1",
				"-input-root", root, "-input-file", filepath.Join(root, "input.json"),
				"-auth-store-root", root, "-auth-store", filepath.Join(root, "auth.json"),
			}, io.Discard, io.Discard)
		},
	} {
		err := run()
		if err == nil || !strings.Contains(err.Error(), dbPath+".daemon.lock") {
			t.Fatalf("%s held lock error = %v", name, err)
		}
	}
	inspection := (productionPreflightEnvironment{}).InspectDatabase(ctx, preflightConfig{DBPath: dbPath}, "")
	if !errors.Is(inspection.OpenError, daemonlock.ErrAlreadyRunning) {
		t.Fatalf("preflight inspection error = %v", inspection.OpenError)
	}
	if _, err := runFakePublicationCommand(ctx, fakePublicationCommandConfig{DBPath: dbPath}); err == nil || !strings.Contains(err.Error(), "stop the daemon first") {
		t.Fatalf("fake publication conflict = %v", err)
	}
}

func TestControlReattemptUsesRunningDaemonStore(t *testing.T) {
	dbPath, st, parent := reattemptTaskFixture(t)
	terminal := domain.ObservedStatusFailed
	invocation := domain.InvocationID("inv-implement-" + string(parent.ID))
	if err := st.Write(t.Context(), func(tx *store.WriteTx) error {
		return tx.AppendRunMilestone(t.Context(), domain.RunMilestone{
			RunID: parent.ID, Kind: domain.MilestoneTerminalRecorded,
			InvocationID: &invocation, Terminal: &terminal, RecordedAt: time.Now().UTC(),
		})
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	h, err := run(t.Context(), nil, config{Environment: environmentEphemeral, DBPath: dbPath, ListenAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	})
	var directOpens atomic.Int64
	ctx := context.WithValue(t.Context(), directStoreOpenerKey{}, directStoreOpener(func(context.Context, string, store.Options, storeOpenMode) (*store.Store, error) {
		directOpens.Add(1)
		return nil, errors.New("unexpected direct store open")
	}))
	result, err := runReattemptCommand(ctx, reattemptCommandConfig{DBPath: dbPath, ParentRunID: parent.ID, Reason: "Repair the fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if result.ParentRunID != parent.ID || result.AttemptNumber != 2 || directOpens.Load() != 0 {
		t.Fatalf("reattempt result = %+v, direct opens = %d", result, directOpens.Load())
	}
}

func TestControlFindsDatabaseBehindDanglingSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target.db")
	alias := filepath.Join(root, "alias.db")
	if err := os.Symlink(target, alias); err != nil {
		t.Fatal(err)
	}
	h, err := run(t.Context(), nil, config{Environment: environmentEphemeral, DBPath: alias, ListenAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err := os.Stat(target + ".control.json"); err != nil {
		t.Fatalf("canonical control advertisement: %v", err)
	}
	source, err := openObservation(t.Context(), alias)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := source.(*remoteObservation); !ok {
		t.Fatal("live daemon did not select its control socket")
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestControlRejectsAnotherDatabaseAtSocket(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "daemon.db")
	h, err := run(t.Context(), nil, config{Environment: environmentEphemeral, DBPath: dbPath, ListenAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	})
	advertisement, err := os.ReadFile(dbPath + ".control.json") //nolint:gosec // test-owned path under TempDir
	if err != nil {
		t.Fatal(err)
	}
	otherDB := filepath.Join(root, "other.db")
	if err := os.WriteFile(otherDB+".control.json", advertisement, 0o600); err != nil { //nolint:gosec // test-owned path under TempDir
		t.Fatal(err)
	}
	client, err := newControlClient(otherDB)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var observed domain.RunObservation
	err = client.get(t.Context(), "/observe/runs/run-1", nil, &observed)
	if err == nil || !strings.Contains(err.Error(), "database path mismatch") {
		t.Fatalf("cross-database read = %v", err)
	}
	var abandoned abandonResult
	err = client.call(t.Context(), "/tasks/abandon", abandonCommandConfig{DBPath: otherDB, TaskID: "task-1"}, &abandoned)
	if err == nil || !strings.Contains(err.Error(), "database path mismatch") {
		t.Fatalf("cross-database write = %v", err)
	}
}

func TestControlArgumentNormalizationPreservesValues(t *testing.T) {
	args := []string{"record-defect", "-db", "relative.db", "-reason", "-db=notes", "--"}
	got := canonicalControlArgs(args, "/canonical/db")
	if got[len(got)-2] != "-db" || got[len(got)-1] != "/canonical/db" || got[4] != "-db=notes" || args[2] != "relative.db" {
		t.Fatalf("normalized arguments = %q; original = %q", got, args)
	}
}

type controlRoundTrip func(*http.Request) (*http.Response, error)

func (f controlRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestControlClientUsesCallerDeadline(t *testing.T) {
	for _, deadline := range []time.Duration{0, 10 * time.Second} {
		t.Run(deadline.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				dbPath := filepath.Join(t.TempDir(), "freeside.db")
				if err := os.WriteFile(dbPath+".control.json", []byte(`{"socket_path":"/unused.sock"}`), 0o600); err != nil {
					t.Fatal(err)
				}
				client, err := newControlClient(dbPath)
				if err != nil {
					t.Fatal(err)
				}
				defer client.Close()
				client.http.Transport = controlRoundTrip(func(r *http.Request) (*http.Response, error) {
					select {
					case <-r.Context().Done():
						return nil, r.Context().Err()
					case <-time.After(31 * time.Second):
						return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
					}
				})
				ctx := t.Context()
				if deadline != 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, deadline)
					defer cancel()
				}
				var result commandOutput
				err = client.call(ctx, "/doctor", []string{"-db", dbPath}, &result)
				if deadline == 0 && err != nil {
					t.Fatalf("long control operation: %v", err)
				}
				if deadline != 0 && !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("caller deadline: %v", err)
				}
			})
		})
	}
}

func TestControlSnapshotPreservesRequestedRecipes(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	recipeA := domain.Digest("sha256:" + strings.Repeat("a", 64))
	recipeB := domain.Digest("sha256:" + strings.Repeat("b", 64))
	approved := map[domain.Digest]bool{recipeA: true, recipeB: true}
	h, err := run(t.Context(), nil, config{
		Environment: environmentEphemeral,
		DBPath:      dbPath, ListenAddr: "127.0.0.1:0", ApprovedRecipes: approved,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	})
	runID := domain.RunID("run-recipe-scope")
	artifact, err := domain.NewArtifact(domain.ArtifactInput{
		ID: "artifact-recipe-b", Type: domain.ArtifactKindVerificationReport, Digest: "sha256:evidence",
		Provenance: domain.Provenance{
			ProducerClass: domain.ProducerVerifier, ProducerInvocationID: "inv-recipe-b",
			HeadBinding: domain.HeadIndependent, VerificationRecipeDigest: &recipeB,
			SensitivityClass: domain.SensitivityNormal,
		},
		Metadata: testRunEvidenceMetadata(1),
	}, approved)
	if err != nil {
		t.Fatal(err)
	}
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "item-recipe-b", ProjectID: "project-recipes",
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
		Type:    domain.AttentionExecutionFailure, Priority: domain.PriorityHigh,
		Reason: "recipe B evidence", RequestedDecision: []domain.Action{domain.ActionRetry, domain.ActionStop},
		EvidenceSnapshot: []domain.Artifact{artifact}, ItemVersion: 1,
		InterruptionClass: domain.InterruptionExceptional, Status: domain.StatusOpen,
	}, approved)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.Write(t.Context(), func(tx *store.WriteTx) error {
		if err := tx.PutRun(t.Context(), domain.Run{ID: runID, ProjectID: item.ProjectID, SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy"}); err != nil {
			return err
		}
		invocation := domain.InvocationID("inv-recipe-b")
		if err := tx.AppendRunMilestone(t.Context(), domain.RunMilestone{
			RunID: runID, Kind: domain.MilestoneRunSubmitted, InvocationID: &invocation, RecordedAt: time.Now().UTC(),
		}); err != nil {
			return err
		}
		return tx.PutAttentionItem(t.Context(), item)
	}); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		recipes []domain.Digest
		allowed bool
	}{
		{name: "none"},
		{name: "A", recipes: []domain.Digest{recipeA}},
		{name: "B", recipes: []domain.Digest{recipeB}, allowed: true},
		{name: "A+B", recipes: []domain.Digest{recipeA, recipeB}, allowed: true},
		{name: "B+compiled", recipes: []domain.Digest{recipeB, domain.EffectProposalRecipeDigest}, allowed: true},
	}
	readSnapshot := func(recipes []domain.Digest, allowed bool) []byte {
		t.Helper()
		source, err := openObservation(t.Context(), dbPath, recipes...)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := source.Close(); err != nil {
				t.Error(err)
			}
		}()
		snapshot, err := source.ObserveSnapshot(t.Context(), runID)
		if !allowed {
			if !errors.Is(err, domain.ErrUnapprovedRecipe) {
				t.Fatalf("scope %v accepted recipe B: %v", recipes, err)
			}
			return nil
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := json.Marshal(snapshot)
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	remote := make(map[string][]byte)
	for _, test := range tests {
		remote[test.name] = readSnapshot(test.recipes, test.allowed)
	}
	readSnapshot([]domain.Digest{domain.Digest("sha256:" + strings.Repeat("c", 64))}, false)
	if err := h.store.Read(t.Context(), func(tx *store.ReadTx) error {
		_, err := tx.GetAttentionItem(t.Context(), item.ID)
		return err
	}); err != nil {
		t.Fatalf("request altered daemon approvals: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	for _, test := range tests {
		direct := readSnapshot(test.recipes, test.allowed)
		if !bytes.Equal(remote[test.name], direct) {
			t.Fatalf("%s socket/direct snapshot mismatch:\n%s\n%s", test.name, remote[test.name], direct)
		}
	}
}

func TestControlDoctorUsesDaemonBlobStoreThroughSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target.db")
	alias := filepath.Join(root, "alias.db")
	if err := os.Symlink(target, alias); err != nil {
		t.Fatal(err)
	}
	h, err := run(t.Context(), nil, config{Environment: environmentEphemeral, DBPath: alias, ListenAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	})
	var out bytes.Buffer
	err = runDoctorCommand(t.Context(), []string{
		"-db", alias, "-backend-configuration-digest", "sha256:" + strings.Repeat("a", 64),
	}, &out, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "unhealthy") || !json.Valid(out.Bytes()) {
		t.Fatalf("routed Doctor = %s, %v", out.String(), err)
	}
	if _, err := os.Stat(alias + ".blobs"); err != nil {
		t.Fatalf("daemon blob sidecar: %v", err)
	}
	if _, err := os.Stat(target + ".blobs"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Doctor created a canonical-path blob sidecar: %v", err)
	}
}
