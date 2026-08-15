package ward

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	osexec "os/exec"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

type fakeCodexReviewJournal struct {
	binding             CodexReviewJournalBinding
	workspaceBinding    CodexReviewWorkspaceBinding
	intent              *CodexReviewLaunchIntent
	extraIntents        map[string]*CodexReviewLaunchIntent
	failIntentByID      map[string]error
	mutate              func(*CodexReviewJournalBinding)
	onGet               func()
	onBegin             func(CodexReviewLaunchIntent) error
	requests            map[string]exec.ReviewRequest
	outcomes            map[string]CodexReviewSourceOutcome
	ready               map[string]bool
	readyMarkCalls      map[string]int
	failResourceMark    error
	failStarting        error
	failStarted         error
	failGetIntent       error
	failGetRequest      error
	failGetOutcome      error
	failGetBinding      error
	failDeleteWorkspace error
}

func (j *fakeCodexReviewJournal) PutCodexReviewRequest(
	_ context.Context, id string, request exec.ReviewRequest,
) error {
	if j.requests == nil {
		j.requests = make(map[string]exec.ReviewRequest)
	}
	j.requests[id] = request
	return nil
}

func (j *fakeCodexReviewJournal) GetCodexReviewRequest(
	_ context.Context, id string,
) (exec.ReviewRequest, error) {
	if j.failGetRequest != nil {
		return exec.ReviewRequest{}, j.failGetRequest
	}
	request, ok := j.requests[id]
	if !ok {
		return exec.ReviewRequest{}, exec.ErrUnknownInvocation
	}
	return request, nil
}

func (j *fakeCodexReviewJournal) PutCodexReviewOutcome(
	_ context.Context, id string, outcome CodexReviewSourceOutcome,
) error {
	if j.outcomes == nil {
		j.outcomes = make(map[string]CodexReviewSourceOutcome)
	}
	j.outcomes[id] = outcome
	return nil
}

func (j *fakeCodexReviewJournal) GetCodexReviewOutcome(
	_ context.Context, id string,
) (CodexReviewSourceOutcome, bool, error) {
	if j.failGetOutcome != nil {
		return CodexReviewSourceOutcome{}, false, j.failGetOutcome
	}
	outcome, ok := j.outcomes[id]
	return outcome, j.ready[id], func() error {
		if !ok {
			return ErrCodexReviewOutcomeNotFound
		}
		return nil
	}()
}

func (j *fakeCodexReviewJournal) ListCodexReviewOutcomeIDs(_ context.Context) ([]string, error) {
	ids := make([]string, 0, len(j.outcomes))
	for id := range j.outcomes {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids, nil
}

func (j *fakeCodexReviewJournal) MarkCodexReviewOutcomeReady(
	_ context.Context, id string,
) error {
	if j.ready == nil {
		j.ready = make(map[string]bool)
	}
	if j.readyMarkCalls == nil {
		j.readyMarkCalls = make(map[string]int)
	}
	j.ready[id] = true
	j.readyMarkCalls[id]++
	return nil
}

var errFakeCodexReviewVolumeLeaseHeld = errors.New("fake Codex review volume lease is held")

// fakeCodexReviewVolumeLeaser models the deployment-owned runtime coordinator.
// Its Start keeps the lease held during the underlying runtime StartContainer
// call, which lets adversarial tests prove an intervening attachment request
// cannot win the final-observation-to-start window.
type fakeCodexReviewVolumeLeaser struct {
	rt *fakeRuntime

	mu                  sync.Mutex
	held                bool
	starting            bool
	transferred         bool
	released            bool
	holder              string
	volumes             []string
	container           string
	afterStartErr       error
	afterStartTransfers bool
	releaseErr          error
	recoverErr          error
}

func (l *fakeCodexReviewVolumeLeaser) AcquireCodexReviewVolumeLease(
	_ context.Context, holder string, volumes []string,
) (CodexReviewVolumeLifecycleLease, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.held || l.starting || l.transferred {
		return nil, errFakeCodexReviewVolumeLeaseHeld
	}
	l.held = true
	l.holder = holder
	l.volumes = slices.Clone(volumes)
	return l, nil
}

func (l *fakeCodexReviewVolumeLeaser) RecoverCodexReviewVolumeLease(
	_ context.Context, holder string, volumes []string,
) (CodexReviewVolumeLifecycleLease, CodexReviewVolumeLeaseTransfer, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.recoverErr != nil {
		return nil, CodexReviewVolumeLeaseTransfer{}, l.recoverErr
	}
	if l.transferred {
		// The coordinator itself observes the transfer target: while its
		// container exists, the window is still transferred; once the target is
		// gone, the exact durable owner adopts the lease back as held.
		if _, exists := l.rt.ctrs[l.container]; exists {
			return nil, CodexReviewVolumeLeaseTransfer{Holder: l.holder, Volumes: slices.Clone(l.volumes), Container: l.container}, ErrCodexReviewVolumeLeaseTransferred
		}
		if l.holder != holder || !slices.Equal(l.volumes, volumes) {
			return nil, CodexReviewVolumeLeaseTransfer{}, ErrCodexReviewVolumeLeaseForeignOwner
		}
		l.transferred = false
		l.container = ""
		l.held = true
		return l, CodexReviewVolumeLeaseTransfer{}, nil
	}
	if l.held && (l.holder != holder || !slices.Equal(l.volumes, volumes)) {
		return nil, CodexReviewVolumeLeaseTransfer{}, ErrCodexReviewVolumeLeaseForeignOwner
	}
	l.held = true
	l.holder = holder
	l.volumes = slices.Clone(volumes)
	return l, CodexReviewVolumeLeaseTransfer{}, nil
}

func (l *fakeCodexReviewVolumeLeaser) StartCodexReviewContainer(ctx context.Context, container string) error {
	l.mu.Lock()
	if !l.held || l.starting || l.transferred {
		l.mu.Unlock()
		return errors.New("fake Codex review volume lease is not held for start")
	}
	l.starting = true
	l.mu.Unlock()

	err := l.rt.StartContainer(ctx, container)

	l.mu.Lock()
	defer l.mu.Unlock()
	l.starting = false
	if err != nil {
		return err
	}
	if l.afterStartErr != nil {
		if l.afterStartTransfers {
			l.held = false
			l.transferred = true
			l.container = container
		}
		return l.afterStartErr
	}
	l.held = false
	l.transferred = true
	l.container = container
	return nil
}

func (l *fakeCodexReviewVolumeLeaser) ReleaseCodexReviewVolumeLease(context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.transferred {
		return errors.New("fake Codex review volume lease was already transferred")
	}
	l.held = false
	l.released = true
	return l.releaseErr
}

func (j *fakeCodexReviewJournal) GetCodexReviewWorkspaceBinding(
	_ context.Context, sourceRunID string,
) (CodexReviewWorkspaceBinding, error) {
	if j.workspaceBinding.SourceRunID != sourceRunID {
		return CodexReviewWorkspaceBinding{}, ErrCodexReviewWorkspaceNotFound
	}
	return j.workspaceBinding, nil
}

func (j *fakeCodexReviewJournal) ListCodexReviewWorkspaceIDs(_ context.Context) ([]string, error) {
	if j.workspaceBinding.SourceRunID == "" {
		return nil, nil
	}
	return []string{j.workspaceBinding.SourceRunID}, nil
}

func (j *fakeCodexReviewJournal) DeleteCodexReviewWorkspaceBinding(
	_ context.Context, binding CodexReviewWorkspaceBinding,
) error {
	if j.failDeleteWorkspace != nil {
		return j.failDeleteWorkspace
	}
	if j.workspaceBinding.SourceRunID == "" {
		return nil
	}
	if j.workspaceBinding != binding {
		return ErrConformance
	}
	j.workspaceBinding = CodexReviewWorkspaceBinding{}
	return nil
}

func (j *fakeCodexReviewJournal) PutCodexReviewWorkspaceBinding(
	_ context.Context, binding CodexReviewWorkspaceBinding,
) error {
	j.workspaceBinding = binding
	return nil
}

func (j *fakeCodexReviewJournal) BeginCodexReviewIntent(_ context.Context, intent CodexReviewLaunchIntent) error {
	if j.onBegin != nil {
		if err := j.onBegin(intent); err != nil {
			return err
		}
	}
	if j.intent != nil && j.intent.State != CodexReviewIntentClosed {
		return errors.New("Codex review intent already open")
	}
	copy := intent
	copy.Resources = slices.Clone(intent.Resources)
	j.intent = &copy
	return nil
}

func (j *fakeCodexReviewJournal) GetCodexReviewIntent(_ context.Context, runID string) (CodexReviewLaunchIntent, error) {
	if err := j.failIntentByID[runID]; err != nil {
		return CodexReviewLaunchIntent{}, err
	}
	if j.failGetIntent != nil {
		return CodexReviewLaunchIntent{}, j.failGetIntent
	}
	intent := j.intent
	if extra := j.extraIntents[runID]; extra != nil {
		intent = extra
	}
	if intent == nil || intent.RunID != runID {
		return CodexReviewLaunchIntent{}, ErrCodexReviewIntentNotFound
	}
	copy := *intent
	copy.Resources = slices.Clone(intent.Resources)
	return copy, nil
}

func (j *fakeCodexReviewJournal) ListCodexReviewIntentIDs(_ context.Context) ([]string, error) {
	if j.failGetIntent != nil {
		return nil, j.failGetIntent
	}
	if j.intent == nil {
		if len(j.extraIntents) == 0 {
			return nil, nil
		}
	}
	ids := make([]string, 0, len(j.extraIntents)+1)
	if j.intent != nil {
		ids = append(ids, j.intent.RunID)
	}
	for id := range j.extraIntents {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids, nil
}

func (j *fakeCodexReviewJournal) MarkCodexReviewIntentResource(
	_ context.Context, runID string, resource CodexReviewIntentResource,
) error {
	if j.failResourceMark != nil {
		return j.failResourceMark
	}
	if j.intent == nil || j.intent.RunID != runID {
		return ErrCodexReviewIntentNotFound
	}
	if j.intent.State != CodexReviewIntentPreparing {
		return errors.Join(ErrConformance, store.ErrImmutableConflict)
	}
	for i := range j.intent.Resources {
		if j.intent.Resources[i].Name == resource.Name {
			j.intent.Resources[i] = resource
			return nil
		}
	}
	return errors.Join(ErrConformance, domain.ErrParentKeyMismatch)
}

func TestFakeCodexReviewJournalRejectsResourceMutationAfterPreparing(t *testing.T) {
	for _, state := range AllCodexReviewIntentStates {
		t.Run(string(state), func(t *testing.T) {
			journal := &fakeCodexReviewJournal{intent: &CodexReviewLaunchIntent{
				RunID: "review-1", State: state,
				Resources: []CodexReviewIntentResource{{Name: "resource"}},
			}}
			err := journal.MarkCodexReviewIntentResource(context.Background(), "review-1",
				CodexReviewIntentResource{Name: "resource", Fingerprint: "fresh"})
			if state == CodexReviewIntentPreparing {
				if err != nil || journal.intent.Resources[0].Fingerprint != "fresh" {
					t.Fatalf("preparing resource mutation = %v, intent %+v; want success", err, journal.intent)
				}
				return
			}
			if !errors.Is(err, ErrConformance) || !errors.Is(err, store.ErrImmutableConflict) {
				t.Fatalf("%s resource mutation = %v, want conformance immutable conflict", state, err)
			}
		})
	}
}

func (j *fakeCodexReviewJournal) transitionCodexReviewIntent(
	runID string, from []CodexReviewIntentState, to CodexReviewIntentState,
) error {
	if j.intent == nil || j.intent.RunID != runID {
		return ErrCodexReviewIntentNotFound
	}
	if j.intent.State == to {
		return nil
	}
	if !slices.Contains(from, j.intent.State) {
		return errors.Join(ErrConformance, store.ErrImmutableConflict)
	}
	j.intent.State = to
	return nil
}

func (j *fakeCodexReviewJournal) MarkCodexReviewIntentPrepared(_ context.Context, runID string) error {
	if j.intent == nil || j.intent.RunID != runID {
		return ErrCodexReviewIntentNotFound
	}
	if _, rejected := j.outcomes[runID]; rejected {
		return errors.Join(ErrConformance, store.ErrImmutableConflict)
	}
	return j.transitionCodexReviewIntent(
		runID, []CodexReviewIntentState{CodexReviewIntentPreparing}, CodexReviewIntentPrepared,
	)
}

func TestBackendCodexReviewDoesNotPrepareAfterRejectedOutcome(t *testing.T) {
	backend, rt, cfg, launch, journal := testCodexReviewLifecycle(t)
	journal.onGet = func() {
		if journal.outcomes == nil {
			journal.outcomes = make(map[string]CodexReviewSourceOutcome)
		}
		journal.outcomes[launch.RunID] = CodexReviewSourceOutcome{
			InvocationID: domain.InvocationID(launch.RunID),
			FailureClass: domain.ReviewFailureContradiction,
			Failure:      "request rejected",
		}
	}
	if got, err := backend.CodexReview(context.Background(), cfg, launch); got != nil ||
		!errors.Is(err, ErrConformance) || errors.Is(err, ErrCodexReviewOperational) {
		t.Fatalf("CodexReview after rejection fence = (%v, %v), want conformance refusal", got, err)
	}
	if journal.intent == nil || journal.intent.State != CodexReviewIntentPreparing {
		t.Fatalf("intent = %+v, want preparing", journal.intent)
	}
	if rt.callIndex("start-container "+codexReviewContainerName(launch.RunID)) != -1 {
		t.Fatal("review container started after the rejection fence")
	}
}

func (j *fakeCodexReviewJournal) MarkCodexReviewIntentStarting(_ context.Context, runID string) error {
	if j.failStarting != nil {
		return j.failStarting
	}
	return j.transitionCodexReviewIntent(
		runID, []CodexReviewIntentState{CodexReviewIntentPrepared}, CodexReviewIntentStarting,
	)
}

func (j *fakeCodexReviewJournal) MarkCodexReviewIntentStarted(_ context.Context, runID string) error {
	if j.failStarted != nil {
		return j.failStarted
	}
	return j.transitionCodexReviewIntent(
		runID, []CodexReviewIntentState{CodexReviewIntentStarting}, CodexReviewIntentStarted,
	)
}

func (j *fakeCodexReviewJournal) CloseCodexReviewIntent(_ context.Context, runID string) error {
	return j.transitionCodexReviewIntent(runID, []CodexReviewIntentState{
		CodexReviewIntentPreparing, CodexReviewIntentPrepared,
		CodexReviewIntentStarting, CodexReviewIntentStarted,
	}, CodexReviewIntentClosed)
}

func (j *fakeCodexReviewJournal) PutCodexReviewBinding(
	_ context.Context, binding CodexReviewJournalBinding,
) error {
	j.binding = cloneCodexReviewBinding(binding)
	if j.mutate != nil {
		j.mutate(&j.binding)
	}
	return nil
}

func (j *fakeCodexReviewJournal) GetCodexReviewBinding(
	_ context.Context, runID string,
) (CodexReviewJournalBinding, error) {
	if j.failGetBinding != nil {
		return CodexReviewJournalBinding{}, j.failGetBinding
	}
	if j.onGet != nil {
		j.onGet()
	}
	if j.binding.RunID != runID {
		return CodexReviewJournalBinding{}, ErrCodexReviewBindingNotFound
	}
	return cloneCodexReviewBinding(j.binding), nil
}

var codexReviewEpoch = time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)

const (
	testCodexReviewHead       = "0123456789abcdef0123456789abcdef01234567"
	testCodexReviewTreeDigest = "1111111111111111111111111111111111111111111111111111111111111111"
)

func codexReviewJWT(t *testing.T, expires time.Time) string {
	t.Helper()
	payload, err := json.Marshal(map[string]int64{"exp": expires.Unix()})
	if err != nil {
		t.Fatal(err)
	}
	return "header." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func writeCodexReviewFile(t *testing.T, root, name string, body []byte) string {
	t.Helper()
	file := filepath.Join(root, name)
	if err := os.WriteFile(file, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

func testCodexReviewShadow(
	t *testing.T,
	cfg CodexReviewConfig,
	runID, volume, observerFingerprint string,
) CodexReviewShadowObservation {
	t.Helper()
	owner := testOwnershipLabel()
	spec, err := BuildCodexReviewShadowObserverSpec(cfg, runID, volume, owner)
	if err != nil {
		t.Fatal(err)
	}
	volumeReport := VolumeSummary{
		Name: volume, Labels: slices.Clone(spec.Labels), LabelsObserved: true,
		CreationDate: "2026-08-03T12:00:00Z",
	}
	observerReport := InspectReport{
		ID: spec.Name, ImageReference: spec.Image, Command: slices.Clone(spec.Command),
		WorkingDirectory: "/", State: StateStopped, AllowlistFieldsObserved: true,
		Mounts: slices.Clone(spec.Mounts), Env: []string{fixedContainerPathEnv},
		NetworksObserved: true, Labels: slices.Clone(spec.Labels), LabelsObserved: true,
		CreationDate: observerFingerprint,
	}
	proof := []byte(fmt.Sprintf(
		"nonce=%s\nempty=yes\ntree=%s\n", owner.Value, emptyCodexShadowDigest,
	))
	observation, err := ObserveCodexReviewShadow(
		cfg, runID, volume, owner, owner, volumeReport, observerReport, proof,
	)
	if err != nil {
		t.Fatal(err)
	}
	return observation
}

func testCodexReviewSnapshot(
	t *testing.T,
	cfg CodexReviewConfig,
	runID, volume string,
	authBody, instructionBody []byte,
	observerFingerprint string,
) CodexReviewSnapshotObservation {
	t.Helper()
	owner := testOwnershipLabel()
	spec, err := BuildCodexReviewSnapshotObserverSpec(cfg, runID, volume, owner)
	if err != nil {
		t.Fatal(err)
	}
	volumeReport := VolumeSummary{
		Name: volume, Labels: slices.Clone(spec.Labels), LabelsObserved: true,
		CreationDate: "2026-08-03T12:00:02Z",
	}
	observerReport := InspectReport{
		ID: spec.Name, ImageReference: spec.Image, Command: slices.Clone(spec.Command),
		WorkingDirectory: "/", State: StateStopped, AllowlistFieldsObserved: true,
		Mounts: slices.Clone(spec.Mounts), Env: []string{fixedContainerPathEnv},
		NetworksObserved: true, Labels: slices.Clone(spec.Labels), LabelsObserved: true,
		CreationDate: observerFingerprint,
	}
	authSum := sha256.Sum256(authBody)
	instrSum := sha256.Sum256(instructionBody)
	proof := []byte(fmt.Sprintf(
		"nonce=%s\nvalid=valid\nauth=sha256:%x\ninstr=sha256:%x\n", owner.Value, authSum, instrSum,
	))
	observation, err := ObserveCodexReviewSnapshot(
		cfg, runID, volume, owner, owner, volumeReport, observerReport, proof,
	)
	if err != nil {
		t.Fatal(err)
	}
	return observation
}

func testCodexReviewWorkspace(
	t *testing.T,
	cfg CodexReviewConfig,
	runID, volume, observerFingerprint string,
) CodexReviewWorkspaceObservation {
	t.Helper()
	return testCodexReviewWorkspaceWithAgents(
		t, cfg, runID, volume, observerFingerprint, codexWorkspaceAgentsDir,
	)
}

func testCodexReviewWorkspaceWithAgents(
	t *testing.T,
	cfg CodexReviewConfig,
	runID, volume, observerFingerprint, agentsEntry string,
) CodexReviewWorkspaceObservation {
	t.Helper()
	workspaceOwner := testOwnershipLabel()
	observerOwner := testOwnershipLabel()
	spec, err := BuildCodexReviewWorkspaceObserverSpec(cfg, runID, volume, observerOwner)
	if err != nil {
		t.Fatal(err)
	}
	volumeReport := VolumeSummary{
		Name: volume, Labels: []Label{workspaceOwner}, LabelsObserved: true,
		CreationDate: "2026-08-03T11:59:59Z",
	}
	observerReport := InspectReport{
		ID: spec.Name, ImageReference: spec.Image, Command: slices.Clone(spec.Command),
		WorkingDirectory: "/", State: StateStopped, AllowlistFieldsObserved: true,
		Mounts: slices.Clone(spec.Mounts), Env: []string{fixedContainerPathEnv},
		NetworksObserved: true, Labels: slices.Clone(spec.Labels), LabelsObserved: true,
		CreationDate: observerFingerprint,
	}
	proof := []byte(fmt.Sprintf(
		"nonce=%s\ngit_dir=present\nhead_detached=yes\nbase_sha=%s\nworktree=clean\n"+
			"git_replacements=absent\nirregular=absent\ntree_sha256=%s\n"+
			codexWorkspaceAgentsKey+"=%s\n",
		observerOwner.Value, testCodexReviewHead, testCodexReviewTreeDigest,
		agentsEntry,
	))
	observation, err := ObserveCodexReviewWorkspace(
		cfg, runID, volume, testCodexReviewHead, workspaceOwner, observerOwner,
		volumeReport, observerReport, proof,
	)
	if err != nil {
		t.Fatal(err)
	}
	return observation
}

func testCodexReviewNetwork(
	t *testing.T,
	cfg CodexReviewConfig,
	runID string,
) CodexReviewNetworkObservation {
	t.Helper()
	owner := testOwnershipLabel()
	observation, err := ObserveCodexReviewNetwork(cfg, runID, owner, NetworkReport{
		NetworkSummary: NetworkSummary{
			Name: codexReviewNetworkName(runID), Mode: NetworkHostOnly,
			Labels: []Label{owner}, LabelsObserved: true, CreationDate: "2026-08-03T11:59:58Z",
		},
		IPv4Gateway: "127.0.0.1", IPv4Subnet: "127.0.0.0/24",
	})
	if err != nil {
		t.Fatal(err)
	}
	return observation
}

func testCodexReview(t *testing.T) (CodexReviewConfig, CodexReviewSpec) {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil { //nolint:gosec // fixture is a private directory, not a file
		t.Fatal(err)
	}
	auth, err := json.Marshal(map[string]any{
		"OPENAI_API_KEY": nil,
		"tokens": map[string]any{
			"id_token":      "fixture-id-token",
			"access_token":  codexReviewJWT(t, codexReviewEpoch.Add(2*time.Hour)),
			"refresh_token": "",
		},
		"last_refresh": codexReviewEpoch.Format(time.RFC3339),
	})
	if err != nil {
		t.Fatal(err)
	}
	instructionBody, instructionBinding, err := exec.ComposeCodexReviewInstructions(
		exec.ReviewHostInstructionInput{}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	digest := digestBody(instructionBody)
	cfg := CodexReviewConfig{
		Model: "gpt-5.2-codex", ReasoningEffort: "high",
		InputRoot:                root,
		WorkspaceTarget:          "/workspace/project",
		ProviderEndpoints:        []string{"chatgpt.com:443"},
		ProxyURL:                 "http://127.0.0.1:43123",
		ApprovedImage:            "example.test/codex@sha256:" + strings.Repeat("a", 64),
		ObserverImage:            "example.test/exporter@sha256:" + strings.Repeat("c", 64),
		AccessTokenLifetimeFloor: time.Hour,
		Now:                      func() time.Time { return codexReviewEpoch },
	}
	shadow := testCodexReviewShadow(
		t, cfg, "review-1", "freeside-review-review-1-agents", "2026-08-03T12:00:01Z",
	)
	snapshot := testCodexReviewSnapshot(
		t, cfg, "review-1", codexReviewSnapshotVolumeName("review-1"), auth, instructionBody, "2026-08-03T12:00:02Z",
	)
	workspace := testCodexReviewWorkspace(
		t, cfg, "review-1", namesFor("review-1").Workspace, "2026-08-03T12:00:03Z",
	)
	req := CodexReviewSpec{
		RunID:                "review-1",
		Image:                "example.test/codex@sha256:" + strings.Repeat("a", 64),
		WorkspaceSourceRunID: "review-1",
		WorkspaceVolume:      namesFor("review-1").Workspace,
		Workspace:            workspace,
		Network:              testCodexReviewNetwork(t, cfg, "review-1"),
		Prompt:               "Review the exact candidate head.",
		Boundary:             CodexReviewFreshStart,
		AuthMode:             CodexAuthSubscription,
		AuthIdentityID:       "codex-reviewer",
		AuthSnapshot:         writeCodexReviewFile(t, root, "auth.json", auth),
		Instructions: VendorInstructions{
			Vendor: domain.AgentVendorCodex, Delivery: domain.VendorInstructionDeliveryAppendFile,
			Present: true, Digest: digest, Body: instructionBody,
		},
		InstructionFile:    writeCodexReviewFile(t, root, "AGENTS.md", instructionBody),
		InstructionBinding: instructionBinding,
		AgentsShadow:       shadow,
		Snapshot:           snapshot,
	}
	return cfg, req
}

func testCodexReviewLifecycle(
	t *testing.T,
) (*CodexReviewLifecycle, *fakeRuntime, CodexReviewConfig, CodexReviewLaunchSpec, *fakeCodexReviewJournal) {
	t.Helper()
	cfg, req := testCodexReview(t)
	journal := &fakeCodexReviewJournal{}
	cfg.Journal = journal
	cfg.ProxyURL = ""
	cfg.AuthStoreLeaser = &fakeLeaser{volume: req.AuthSnapshot}
	cfg.AuthRefresher = &fakeCodexAuthRefresher{}
	cfg.AuthState = &fakeCodexAuthState{}
	launch := CodexReviewLaunchSpec{
		RunID: req.RunID, WorkflowRunID: domain.RunID(req.RunID),
		Image: req.Image, WorkspaceVolume: req.WorkspaceVolume,
		WorkspaceSourceRunID: req.RunID,
		ExpectedHead:         testCodexReviewHead, Prompt: req.Prompt, Boundary: req.Boundary,
		AuthMode: req.AuthMode, AuthIdentityID: req.AuthIdentityID,
		AuthSnapshot: req.AuthSnapshot, Instructions: req.Instructions,
		InstructionFile: req.InstructionFile, InstructionBinding: req.InstructionBinding,
	}
	fx := newHandoffFixture(t)
	cfg.VolumeLifecycleLeaser = &fakeCodexReviewVolumeLeaser{rt: fx.rt}
	workspaceOwner := testOwnershipLabel()
	fx.rt.vols[launch.WorkspaceVolume] = &fakeVol{
		labels: append(runLabels(req.RunID), workspaceOwner), created: "workspace-created",
	}
	journal.workspaceBinding = CodexReviewWorkspaceBinding{
		SourceRunID: launch.WorkspaceSourceRunID, Volume: launch.WorkspaceVolume,
		OwnershipToken: workspaceOwner.Value, CreationFingerprint: "workspace-created",
	}
	fx.rt.volBase[launch.WorkspaceVolume] = testCodexReviewHead
	fx.rt.volTree[launch.WorkspaceVolume] = testCodexReviewTreeDigest
	return fx.codexReviewLifecycle(t), fx.rt, cfg, launch, journal
}

func retargetCodexReviewLifecycle(
	t *testing.T,
	rt *fakeRuntime,
	launch *CodexReviewLaunchSpec,
	journal *fakeCodexReviewJournal,
	runID string,
) {
	t.Helper()
	oldVolume := launch.WorkspaceVolume
	workspaceOwner := Label{Key: ownershipLabelKey, Value: journal.workspaceBinding.OwnershipToken}
	delete(rt.vols, oldVolume)
	delete(rt.volBase, oldVolume)
	delete(rt.volTree, oldVolume)
	launch.RunID = runID
	launch.WorkspaceSourceRunID = runID
	launch.WorkspaceVolume = namesFor(runID).Workspace
	rt.vols[launch.WorkspaceVolume] = &fakeVol{
		labels: append(runLabels(runID), workspaceOwner), created: "workspace-created",
	}
	rt.volBase[launch.WorkspaceVolume] = testCodexReviewHead
	rt.volTree[launch.WorkspaceVolume] = testCodexReviewTreeDigest
	journal.workspaceBinding = CodexReviewWorkspaceBinding{
		SourceRunID: runID, Volume: launch.WorkspaceVolume,
		OwnershipToken: workspaceOwner.Value, CreationFingerprint: "workspace-created",
	}
}

func legacyCodexReviewIntentForTest(
	runID, digest, owner string,
) *CodexReviewLaunchIntent {
	names := legacyCodexReviewNames(runID)
	return &CodexReviewLaunchIntent{
		RunID: runID, SpecDigest: digest, OwnershipToken: owner,
		ShadowVolume: names.shadowVolume, Network: names.network,
		ReviewContainer: names.reviewContainer, State: CodexReviewIntentPreparing,
		Resources: []CodexReviewIntentResource{
			{Name: names.workspaceObserver},
			{Name: names.shadowInitializer, OwnershipToken: owner},
			{Name: names.shadowObserver},
			{Name: names.reviewContainer, OwnershipToken: owner},
			{Name: names.shadowVolume, OwnershipToken: owner},
			{Name: names.network, OwnershipToken: owner},
		},
	}
}

func TestCodexReviewResourceNamesFitReferenceRuntime(t *testing.T) {
	maximumID := strings.Repeat("a", 32)
	if !runIDPattern.MatchString(maximumID) {
		t.Fatalf("maximum invocation ID %q is not admitted", maximumID)
	}
	names := codexReviewNames(maximumID)
	resources := map[string]string{
		"workspace volume":   namesFor(maximumID).Workspace,
		"workspace observer": names.workspaceObserver,
		"shadow initializer": names.shadowInitializer,
		"shadow observer":    names.shadowObserver,
		"review container":   names.reviewContainer,
		"shadow volume":      names.shadowVolume,
		"network":            names.network,
	}
	seen := make(map[string]string, len(resources))
	for role, name := range resources {
		if err := validateRuntimeResourceName(name); err != nil {
			t.Errorf("%s name %q is not valid at the runtime boundary: %v", role, name, err)
		}
		if prior, exists := seen[name]; exists {
			t.Errorf("%s and %s share runtime name %q", prior, role, name)
		}
		seen[name] = role
	}
	other := codexReviewNames("b" + maximumID[1:])
	if other.workspaceObserver == names.workspaceObserver || other.shadowVolume == names.shadowVolume {
		t.Fatal("distinct invocation IDs collided after resource-name derivation")
	}
}

func TestBackendCodexReviewMaximumInvocationReachesRuntimeLaunch(t *testing.T) {
	backend, rt, cfg, launchSpec, journal := testCodexReviewLifecycle(t)
	retargetCodexReviewLifecycle(t, rt, &launchSpec, journal, strings.Repeat("a", 32))
	rt.onCreateVolume = validateRuntimeResourceName
	rt.onCreateNetwork = validateRuntimeResourceName
	rt.onCreateContainer = func(spec ContainerSpec) error {
		return validateRuntimeResourceName(spec.Name)
	}
	launch, err := backend.CodexReview(context.Background(), cfg, launchSpec)
	if err != nil {
		t.Fatalf("maximum invocation failed at the runtime boundary: %v", err)
	}
	t.Cleanup(func() { _ = launch.Close() })
	if rt.callIndex("start-container "+codexReviewContainerName(launchSpec.RunID)) < 0 {
		t.Fatal("maximum invocation topology did not reach review launch")
	}
}

// TestBackendCodexReviewShadowTopologyFollowsObservedAgentsEntry drives the
// full launch lifecycle for both attested workspace shapes: a candidate with
// a .agents directory keeps the workspace-local shadow, and a candidate
// proven to lack one launches without that mount, the topology Apple
// container can actually start (errno 30 regression).
func TestBackendCodexReviewShadowTopologyFollowsObservedAgentsEntry(t *testing.T) {
	for _, tc := range []struct {
		name                string
		entry               string
		wantWorkspaceShadow bool
	}{
		{"observed directory keeps the workspace shadow", codexWorkspaceAgentsDir, true},
		{"proven absent omits only the workspace shadow", codexWorkspaceAgentsAbsent, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend, rt, cfg, launchSpec, _ := testCodexReviewLifecycle(t)
			rt.volAgents[launchSpec.WorkspaceVolume] = tc.entry
			launch, err := backend.CodexReview(context.Background(), cfg, launchSpec)
			if err != nil {
				t.Fatalf("CodexReview with %s workspace .agents: %v", tc.entry, err)
			}
			t.Cleanup(func() { _ = launch.Close() })
			reviewName := codexReviewContainerName(launchSpec.RunID)
			if rt.callIndex("start-container "+reviewName) < 0 {
				t.Fatal("review container was not started")
			}
			review, exists := rt.ctrs[reviewName]
			if !exists {
				t.Fatal("review container missing from runtime")
			}
			workspaceLocal := path.Join(cfg.WorkspaceTarget, ".agents")
			mounted := false
			for _, mount := range review.spec.Mounts {
				if mount.Target == workspaceLocal {
					mounted = true
				}
			}
			if mounted != tc.wantWorkspaceShadow {
				t.Errorf("workspace-local shadow mounted = %v, want %v (mounts %+v)",
					mounted, tc.wantWorkspaceShadow, review.spec.Mounts)
			}
			if launch.Binding.WorkspaceAgentsEntry != tc.entry {
				t.Errorf("durable binding entry = %q, want %q",
					launch.Binding.WorkspaceAgentsEntry, tc.entry)
			}
		})
	}
}

// TestBackendCodexReviewRefusesNonDirectoryWorkspaceAgents pins the
// fail-closed arm: a .agents entry that is neither a directory nor absent
// (a symlink or regular file) refuses the launch before any credential-
// bearing container exists.
func TestBackendCodexReviewRefusesNonDirectoryWorkspaceAgents(t *testing.T) {
	backend, rt, cfg, launchSpec, _ := testCodexReviewLifecycle(t)
	rt.volAgents[launchSpec.WorkspaceVolume] = "other"
	_, err := backend.CodexReview(context.Background(), cfg, launchSpec)
	wantCheckFailure(t, err, CheckControlPlaneIsolation)
	if _, exists := rt.ctrs[codexReviewContainerName(launchSpec.RunID)]; exists {
		t.Fatal("review container was created for a refused workspace .agents entry")
	}
}

func TestBackendCodexReviewReapsAuthenticatedLegacyOversizedObserverBeforeRelaunch(t *testing.T) {
	backend, rt, cfg, launchSpec, journal := testCodexReviewLifecycle(t)
	const runID = "review-0dcd5c691adcaec0c353993a"
	retargetCodexReviewLifecycle(t, rt, &launchSpec, journal, runID)
	digest, err := codexReviewIntentDigest(cfg, launchSpec)
	if err != nil {
		t.Fatal(err)
	}
	owner := testOwnershipLabel()
	journal.intent = legacyCodexReviewIntentForTest(runID, digest, owner.Value)
	legacyObserver := legacyCodexReviewNames(runID).workspaceObserver
	observerOwner := Label{Key: ownershipLabelKey, Value: strings.Repeat("2", 32)}
	setCodexReviewIntentResourceEvidenceForTest(
		t, journal.intent, legacyObserver, observerOwner.Value, "legacy-observer",
	)
	if err := validateRuntimeResourceName(legacyObserver); err == nil {
		t.Fatal("production regression fixture is not oversized")
	}
	rt.ctrs[legacyObserver] = &fakeCtr{
		spec:    ContainerSpec{Name: legacyObserver, Labels: append(runLabels(runID), observerOwner)},
		created: "legacy-observer",
	}
	launch, err := backend.CodexReview(context.Background(), cfg, launchSpec)
	if err != nil {
		t.Fatalf("same-invocation relaunch after legacy recovery: %v", err)
	}
	t.Cleanup(func() { _ = launch.Close() })
	if _, exists := rt.ctrs[legacyObserver]; exists {
		t.Fatal("authenticated legacy oversized observer survived recovery")
	}
	if journal.intent.State != CodexReviewIntentStarted ||
		journal.intent.Resources[0].Name != codexReviewWorkspaceObserverName(runID) {
		t.Fatalf("relaunch intent = %#v, want current started topology", journal.intent)
	}
}

func TestRecoverCodexReviewRefusesForeignLegacyOversizedObserver(t *testing.T) {
	backend, rt, cfg, launchSpec, journal := testCodexReviewLifecycle(t)
	const runID = "review-0dcd5c691adcaec0c353993a"
	retargetCodexReviewLifecycle(t, rt, &launchSpec, journal, runID)
	digest, err := codexReviewIntentDigest(cfg, launchSpec)
	if err != nil {
		t.Fatal(err)
	}
	owner := testOwnershipLabel()
	journal.intent = legacyCodexReviewIntentForTest(runID, digest, owner.Value)
	legacyObserver := legacyCodexReviewNames(runID).workspaceObserver
	rt.ctrs[legacyObserver] = &fakeCtr{
		spec: ContainerSpec{Name: legacyObserver, Labels: []Label{{
			Key: ownershipLabelKey, Value: strings.Repeat("f", 32),
		}}},
		created: "foreign-observer",
	}
	if err := backend.RecoverCodexReview(context.Background(), cfg, launchSpec); err == nil ||
		!errors.Is(err, ErrConformance) {
		t.Fatalf("foreign legacy recovery = %v, want conformance refusal", err)
	}
	if _, exists := rt.ctrs[legacyObserver]; !exists {
		t.Fatal("recovery deleted a foreign legacy oversized observer")
	}
	if journal.intent.State != CodexReviewIntentPreparing {
		t.Fatalf("foreign legacy recovery closed intent: %q", journal.intent.State)
	}
}

func TestCodexReviewIntentRejectsMixedResourceNameGenerations(t *testing.T) {
	runID := strings.Repeat("a", 32)
	owner := strings.Repeat("1", 32)
	intent := legacyCodexReviewIntentForTest(runID, strings.Repeat("a", 64), owner)
	intent.Resources[0].Name = codexReviewNames(runID).workspaceObserver
	if err := intent.validateIdentity(runID); err == nil {
		t.Fatal("intent accepted a mixed legacy/current resource topology")
	}
}

// preSnapshotCodexReviewIntentForTest builds the #587..#590 host-bind
// generation: the current short names, six resources, and no snapshot volume,
// exactly the shape run 482 persisted at its round-2 pause before #591.
func preSnapshotCodexReviewIntentForTest(runID, digest, owner string) *CodexReviewLaunchIntent {
	names := preSnapshotCodexReviewNames(runID)
	return &CodexReviewLaunchIntent{
		RunID: runID, SpecDigest: digest, OwnershipToken: owner,
		ShadowVolume: names.shadowVolume, Network: names.network,
		ReviewContainer: names.reviewContainer, State: CodexReviewIntentPreparing,
		Resources: []CodexReviewIntentResource{
			{Name: names.workspaceObserver},
			{Name: names.shadowInitializer, OwnershipToken: owner},
			{Name: names.shadowObserver},
			{Name: names.reviewContainer, OwnershipToken: owner},
			{Name: names.shadowVolume, OwnershipToken: owner},
			{Name: names.network, OwnershipToken: owner},
		},
	}
}

func currentCodexReviewIntentForTest(runID, digest, owner string) *CodexReviewLaunchIntent {
	names := codexReviewNames(runID)
	return &CodexReviewLaunchIntent{
		RunID: runID, SpecDigest: digest, OwnershipToken: owner,
		ShadowVolume: names.shadowVolume, Network: names.network,
		ReviewContainer: names.reviewContainer, SnapshotVolume: names.snapshotVolume,
		State: CodexReviewIntentPreparing,
		Resources: []CodexReviewIntentResource{
			{Name: names.workspaceObserver},
			{Name: names.shadowInitializer, OwnershipToken: owner},
			{Name: names.shadowObserver},
			{Name: names.reviewContainer, OwnershipToken: owner},
			{Name: names.shadowVolume, OwnershipToken: owner},
			{Name: names.network, OwnershipToken: owner},
			{Name: names.snapshotVolume, OwnershipToken: owner},
			{Name: names.snapshotSeeder, OwnershipToken: owner},
			{Name: names.snapshotObserver},
		},
	}
}

// TestCodexReviewIntentAcceptsPreSnapshotGenerationForCleanup pins the #591
// restart-safety criterion: run 482's persisted six-resource, host-bind intent
// authenticates as a cleanup-only legacy generation (no snapshot resources),
// while the current generation carries nine resources and the snapshot volume,
// and a six-resource intent claiming a snapshot volume is rejected.
func TestCodexReviewIntentAcceptsPreSnapshotGenerationForCleanup(t *testing.T) {
	runID := strings.Repeat("a", 32)
	owner := strings.Repeat("1", 32)
	digest := strings.Repeat("a", 64)

	preSnapshot := preSnapshotCodexReviewIntentForTest(runID, digest, owner)
	names, err := preSnapshot.validatedResourceNames(runID)
	if err != nil {
		t.Fatalf("pre-snapshot intent rejected: %v", err)
	}
	if names.snapshotVolume != "" || names.snapshotSeeder != "" || names.snapshotObserver != "" {
		t.Fatalf("pre-snapshot generation carried snapshot resources: %#v", names)
	}

	poisoned := preSnapshotCodexReviewIntentForTest(runID, digest, owner)
	poisoned.SnapshotVolume = codexReviewSnapshotVolumeName(runID)
	if err := poisoned.validateIdentity(runID); err == nil {
		t.Fatal("six-resource intent accepted a snapshot volume")
	}

	current := currentCodexReviewIntentForTest(runID, digest, owner)
	currentNames, err := current.validatedResourceNames(runID)
	if err != nil {
		t.Fatalf("current intent rejected: %v", err)
	}
	if currentNames.snapshotVolume != codexReviewSnapshotVolumeName(runID) {
		t.Fatalf("current generation missing snapshot volume: %#v", currentNames)
	}

	truncated := currentCodexReviewIntentForTest(runID, digest, owner)
	truncated.Resources = truncated.Resources[:6]
	if err := truncated.validateIdentity(runID); err == nil {
		t.Fatal("nine-resource intent accepted with only six resources")
	}
}

func TestBackendCodexReviewAuthenticatesAndReconstructsBeforeStart(t *testing.T) {
	backend, rt, cfg, launchSpec, journal := testCodexReviewLifecycle(t)
	workspaceObserver := codexReviewWorkspaceObserverName(launchSpec.RunID)
	shadowObserver := codexReviewShadowObserverName(launchSpec.RunID)
	rt.onInspect = func(id string, rep InspectReport) (InspectReport, error) {
		if id == workspaceObserver || id == shadowObserver {
			rep.CreationDate = "2026-08-03T12:00:00Z"
		}
		return rep, nil
	}
	startedAfterDurableBinding := false
	rt.onStart = func(id string) error {
		if id != codexReviewContainerName(launchSpec.RunID) {
			return nil
		}
		if journal.binding.ReviewOwnershipToken == "" {
			t.Fatal("review container started before its binding was durable")
		}
		ctr := rt.ctrs[id]
		if ctr == nil || !slices.Contains(ctr.spec.Labels, Label{
			Key: ownershipLabelKey, Value: journal.binding.ReviewOwnershipToken,
		}) {
			t.Fatal("review container started without its minted ownership label")
		}
		startedAfterDurableBinding = true
		return nil
	}
	launch, err := backend.CodexReview(context.Background(), cfg, launchSpec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = launch.Close() })
	if !startedAfterDurableBinding {
		t.Fatal("review container was not started")
	}
	if err := launch.Binding.validateShape(); err != nil {
		t.Fatalf("started binding = %v", err)
	}
	rt.mu.Lock()
	calls := strings.Join(slices.Clone(rt.calls), "\n")
	rt.mu.Unlock()
	for _, observer := range []string{
		workspaceObserver,
		shadowObserver,
	} {
		if got := strings.Count(calls, "create-container "+observer); got != 3 {
			t.Errorf("%s observation count = %d, want 3 (initial, pre-persist, reconstruction)", observer, got)
		}
	}
	initializer := "create-container " + codexReviewShadowInitializerName(launchSpec.RunID)
	firstShadowObservation := "create-container " + codexReviewShadowObserverName(launchSpec.RunID)
	if initAt, observeAt := strings.Index(calls, initializer), strings.Index(calls, firstShadowObservation); initAt < 0 || observeAt < 0 || initAt > observeAt {
		t.Errorf("shadow initializer must run before the literal empty-volume observation:\n%s", calls)
	}
}

func TestBackendCodexReviewPersistsIntentBeforeLeaseOrRuntime(t *testing.T) {
	backend, rt, cfg, launch, journal := testCodexReviewLifecycle(t)
	journal.onBegin = func(intent CodexReviewLaunchIntent) error {
		if intent.OwnershipToken == "" || intent.SpecDigest == "" || intent.State != CodexReviewIntentPreparing {
			t.Fatal("intent was incomplete at durable begin")
		}
		rt.mu.Lock()
		defer rt.mu.Unlock()
		if len(rt.calls) != 0 {
			t.Fatalf("runtime call before durable intent: %v", rt.calls)
		}
		return errors.New("stop after intent")
	}
	if got, err := backend.CodexReview(context.Background(), cfg, launch); err == nil || got != nil {
		t.Fatalf("CodexReview = (%v, %v), want begin refusal", got, err)
	}
}

func TestBackendCodexReviewValidatesPriorIntentBeforeStateBranch(t *testing.T) {
	backend, rt, cfg, launch, journal := testCodexReviewLifecycle(t)
	journal.intent = &CodexReviewLaunchIntent{
		RunID: launch.RunID,
		State: CodexReviewIntentClosed,
	}
	if got, err := backend.CodexReview(context.Background(), cfg, launch); got != nil ||
		!errors.Is(err, ErrConformance) || errors.Is(err, ErrCodexReviewOperational) {
		t.Fatalf("CodexReview = (%v, %v), want non-operational conformance refusal", got, err)
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if len(rt.calls) != 0 {
		t.Fatalf("runtime calls before validating prior intent: %v", rt.calls)
	}
}

func TestBackendCodexReviewRetainsRecoveryIntentWhenResourceJournalUpdateFails(t *testing.T) {
	backend, _, cfg, launch, journal := testCodexReviewLifecycle(t)
	journal.failResourceMark = errors.New("journal unavailable")
	if got, err := backend.CodexReview(context.Background(), cfg, launch); err == nil || got != nil {
		t.Fatalf("CodexReview = (%v, %v), want resource-journal failure", got, err)
	}
	if journal.intent == nil || journal.intent.State != CodexReviewIntentPreparing {
		t.Fatal("failed resource update discarded the durable recovery intent")
	}
}

func TestBackendCodexReviewPreservesResourceMutationConformance(t *testing.T) {
	backend, _, cfg, launch, journal := testCodexReviewLifecycle(t)
	journal.failResourceMark = errors.Join(ErrConformance, errors.New("malformed intent row"))
	if got, err := backend.CodexReview(context.Background(), cfg, launch); got != nil ||
		!errors.Is(err, ErrConformance) || errors.Is(err, ErrCodexReviewOperational) {
		t.Fatalf("CodexReview = (%v, %v), want non-operational conformance refusal", got, err)
	}
}

func TestRecoverCodexReviewReapsOnlyDurablyOwnedPreStartObjects(t *testing.T) {
	backend, rt, cfg, launch, journal := testCodexReviewLifecycle(t)
	owner := testOwnershipLabel()
	digest, err := codexReviewIntentDigest(cfg, launch)
	if err != nil {
		t.Fatal(err)
	}
	shadow := codexReviewShadowVolumeName(launch.RunID)
	network := codexReviewNetworkName(launch.RunID)
	review := codexReviewContainerName(launch.RunID)
	journal.intent = &CodexReviewLaunchIntent{
		RunID: launch.RunID, SpecDigest: digest, OwnershipToken: owner.Value,
		ShadowVolume: shadow, Network: network, ReviewContainer: review,
		State: CodexReviewIntentPreparing,
		Resources: []CodexReviewIntentResource{
			{Name: codexReviewWorkspaceObserverName(launch.RunID)},
			{Name: codexReviewShadowInitializerName(launch.RunID), OwnershipToken: owner.Value},
			{Name: codexReviewShadowObserverName(launch.RunID)},
			{Name: shadow, OwnershipToken: owner.Value, Fingerprint: "shadow"},
			{Name: network, OwnershipToken: owner.Value, Fingerprint: "network"},
			{Name: review, OwnershipToken: owner.Value, Fingerprint: "review"},
		},
	}
	rt.vols[shadow] = &fakeVol{labels: []Label{owner}, created: "shadow"}
	rt.nets[network] = &fakeNetwork{labels: []Label{owner}, created: "network"}
	rt.ctrs[review] = &fakeCtr{spec: ContainerSpec{Name: review, Labels: []Label{owner}}, created: "review"}
	if err := backend.RecoverCodexReview(context.Background(), cfg, launch); err != nil {
		t.Fatal(err)
	}
	if journal.intent.State != CodexReviewIntentClosed {
		t.Fatalf("intent state = %q, want closed", journal.intent.State)
	}
	if _, ok := rt.vols[shadow]; ok {
		t.Fatal("recovery left owned shadow volume")
	}
	if _, ok := rt.nets[network]; ok {
		t.Fatal("recovery left owned network")
	}
	if _, ok := rt.ctrs[review]; ok {
		t.Fatal("recovery left owned review container")
	}
	if err := backend.RecoverCodexReview(context.Background(), cfg, launch); err != nil {
		t.Fatalf("closed recovery = %v, want idempotent nil", err)
	}
}

func TestRecoverCodexReviewKeepsRuntimeListingFailuresTransient(t *testing.T) {
	backend, rt, cfg, launch, journal := testCodexReviewLifecycle(t)
	owner := testOwnershipLabel()
	digest, err := codexReviewIntentDigest(cfg, launch)
	if err != nil {
		t.Fatal(err)
	}
	shadow := codexReviewShadowVolumeName(launch.RunID)
	journal.intent = &CodexReviewLaunchIntent{
		RunID: launch.RunID, SpecDigest: digest, OwnershipToken: owner.Value,
		ShadowVolume: shadow, Network: codexReviewNetworkName(launch.RunID),
		ReviewContainer: codexReviewContainerName(launch.RunID), State: CodexReviewIntentPreparing,
		Resources: []CodexReviewIntentResource{
			{Name: codexReviewWorkspaceObserverName(launch.RunID)},
			{Name: codexReviewShadowInitializerName(launch.RunID), OwnershipToken: owner.Value},
			{Name: codexReviewShadowObserverName(launch.RunID)},
			{Name: shadow, OwnershipToken: owner.Value, Fingerprint: "shadow"},
			{Name: codexReviewNetworkName(launch.RunID), OwnershipToken: owner.Value},
			{Name: codexReviewContainerName(launch.RunID), OwnershipToken: owner.Value},
		},
	}
	rt.vols[shadow] = &fakeVol{labels: []Label{owner}, created: "shadow"}
	failed := false
	rt.onListContainers = func(list []ContainerSummary) ([]ContainerSummary, error) {
		if !failed {
			failed = true
			return nil, errors.New("runtime temporarily unavailable")
		}
		return list, nil
	}
	err = backend.RecoverCodexReview(context.Background(), cfg, launch)
	if !errors.Is(err, ErrCodexReviewOperational) || !errors.Is(err, ErrConformance) {
		t.Fatalf("runtime listing recovery error = %v, want operational conformance", err)
	}
	if journal.intent.State != CodexReviewIntentPreparing {
		t.Fatalf("intent state = %q, want retryable preparing", journal.intent.State)
	}
	if err := backend.RecoverCodexReview(context.Background(), cfg, launch); err != nil {
		t.Fatalf("recovery retry = %v", err)
	}
}

func TestRecoverCodexReviewAdoptsOnlyRecordedOwnerLease(t *testing.T) {
	setup := func(t *testing.T) (*CodexReviewLifecycle, *fakeRuntime, CodexReviewConfig, CodexReviewLaunchSpec, *fakeCodexReviewJournal, Label) {
		t.Helper()
		backend, rt, cfg, launch, journal := testCodexReviewLifecycle(t)
		owner := testOwnershipLabel()
		digest, err := codexReviewIntentDigest(cfg, launch)
		if err != nil {
			t.Fatal(err)
		}
		shadow := codexReviewShadowVolumeName(launch.RunID)
		journal.intent = &CodexReviewLaunchIntent{RunID: launch.RunID, SpecDigest: digest, OwnershipToken: owner.Value, ShadowVolume: shadow, Network: codexReviewNetworkName(launch.RunID), ReviewContainer: codexReviewContainerName(launch.RunID), State: CodexReviewIntentPrepared, Resources: []CodexReviewIntentResource{{Name: codexReviewWorkspaceObserverName(launch.RunID)}, {Name: codexReviewShadowInitializerName(launch.RunID), OwnershipToken: owner.Value}, {Name: codexReviewShadowObserverName(launch.RunID)}, {Name: shadow, OwnershipToken: owner.Value, Fingerprint: "shadow"}, {Name: codexReviewNetworkName(launch.RunID), OwnershipToken: owner.Value}, {Name: codexReviewContainerName(launch.RunID), OwnershipToken: owner.Value}}}
		rt.vols[shadow] = &fakeVol{labels: []Label{owner}, created: "shadow"}
		return backend, rt, cfg, launch, journal, owner
	}

	t.Run("same owner adopts and releases", func(t *testing.T) {
		backend, rt, cfg, launch, journal, owner := setup(t)
		leaser := cfg.VolumeLifecycleLeaser.(*fakeCodexReviewVolumeLeaser)
		if _, err := leaser.AcquireCodexReviewVolumeLease(context.Background(), owner.Value, []string{launch.WorkspaceVolume, codexReviewShadowVolumeName(launch.RunID)}); err != nil {
			t.Fatal(err)
		}
		recovery, err := NewCodexReviewRecovery(backend, journal, cfg.VolumeLifecycleLeaser, cfg.InputRoot)
		if err != nil {
			t.Fatal(err)
		}
		if err := recovery.Reconcile(context.Background()); err != nil {
			t.Fatal(err)
		}
		if !leaser.released || journal.intent.State != CodexReviewIntentClosed {
			t.Fatal("same-owner recovery did not adopt, release, and close")
		}
		if _, exists := rt.vols[launch.WorkspaceVolume]; exists {
			t.Fatal("attended pre-start recovery left the candidate workspace behind")
		}
		if journal.workspaceBinding.SourceRunID != "" {
			t.Fatal("attended pre-start recovery left the candidate workspace binding reusable")
		}
	})

	// plantStage simulates a daemon killed mid-seed: the deterministic host stage
	// under the trusted ExportRoot still holds a plaintext auth.json that the
	// seeder's defer never wiped. It names no runtime resource, so only the
	// recovery stage sweep can reap it.
	plantStage := func(t *testing.T, backend *CodexReviewLifecycle, runID string) string {
		t.Helper()
		stage := codexReviewSnapshotStagePath(backend.cfg.ExportRoot, runID)
		if err := os.Mkdir(stage, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(stage, codexReviewSnapshotAuthName), []byte("SECRET-KEY"), 0o400); err != nil {
			t.Fatal(err)
		}
		return stage
	}

	t.Run("reaps a crashed run's credential stage", func(t *testing.T) {
		backend, _, cfg, launch, journal, owner := setup(t)
		leaser := cfg.VolumeLifecycleLeaser.(*fakeCodexReviewVolumeLeaser)
		if _, err := leaser.AcquireCodexReviewVolumeLease(context.Background(), owner.Value,
			[]string{launch.WorkspaceVolume, codexReviewShadowVolumeName(launch.RunID)}); err != nil {
			t.Fatal(err)
		}
		stage := plantStage(t, backend, launch.RunID)
		recovery, err := NewCodexReviewRecovery(backend, journal, cfg.VolumeLifecycleLeaser, cfg.InputRoot)
		if err != nil {
			t.Fatal(err)
		}
		if err := recovery.Reconcile(context.Background()); err != nil {
			t.Fatal(err)
		}
		if journal.intent.State != CodexReviewIntentClosed {
			t.Fatal("recovery did not complete")
		}
		if _, err := os.Stat(stage); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("recovery left the crashed run's credential stage behind: stat err = %v", err)
		}
	})

	// The stage path derives from the trusted daemon-owned ExportRoot, not the
	// mutable -review-input-root, so recovery reaps it regardless of any
	// review-input-root change across the restart, including an empty root (valid
	// in attended_dev). This is the R1 reframe: the reap no longer depends on the
	// recovery-time InputRoot or on any journal-persisted root.
	for _, recoveryRoot := range []string{t.TempDir(), ""} {
		name := "changed review-input-root"
		if recoveryRoot == "" {
			name = "empty attended_dev review-input-root"
		}
		t.Run("reaps the ExportRoot stage across a "+name, func(t *testing.T) {
			backend, _, cfg, launch, journal, owner := setup(t)
			if recoveryRoot != "" && recoveryRoot == cfg.InputRoot {
				t.Fatalf("test setup: recovery root must differ from the launch InputRoot %q", cfg.InputRoot)
			}
			leaser := cfg.VolumeLifecycleLeaser.(*fakeCodexReviewVolumeLeaser)
			if _, err := leaser.AcquireCodexReviewVolumeLease(context.Background(), owner.Value,
				[]string{launch.WorkspaceVolume, codexReviewShadowVolumeName(launch.RunID)}); err != nil {
				t.Fatal(err)
			}
			stage := plantStage(t, backend, launch.RunID)
			recovery, err := NewCodexReviewRecovery(backend, journal, cfg.VolumeLifecycleLeaser, recoveryRoot)
			if err != nil {
				t.Fatal(err)
			}
			if err := recovery.Reconcile(context.Background()); err != nil {
				t.Fatal(err)
			}
			if journal.intent.State != CodexReviewIntentClosed {
				t.Fatal("recovery did not complete")
			}
			if _, err := os.Stat(stage); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("recovery under a %s left the ExportRoot credential stage behind: stat err = %v", name, err)
			}
		})
	}

	t.Run("closed intent retries binding reset", func(t *testing.T) {
		backend, rt, cfg, launch, journal, owner := setup(t)
		leaser := cfg.VolumeLifecycleLeaser.(*fakeCodexReviewVolumeLeaser)
		if _, err := leaser.AcquireCodexReviewVolumeLease(context.Background(), owner.Value,
			[]string{launch.WorkspaceVolume, codexReviewShadowVolumeName(launch.RunID)}); err != nil {
			t.Fatal(err)
		}
		journal.failDeleteWorkspace = errors.New("journal temporarily unavailable")
		recovery, err := NewCodexReviewRecovery(backend, journal, cfg.VolumeLifecycleLeaser, cfg.InputRoot)
		if err != nil {
			t.Fatal(err)
		}
		if err := recovery.Reconcile(context.Background()); err == nil {
			t.Fatal("recovery accepted a failed workspace-binding reset")
		}
		if journal.intent.State != CodexReviewIntentClosed || journal.workspaceBinding.SourceRunID == "" {
			t.Fatal("failed binding reset did not retain its closed-intent retry evidence")
		}
		if _, exists := rt.vols[launch.WorkspaceVolume]; exists {
			t.Fatal("failed binding reset left the candidate volume live")
		}
		journal.failDeleteWorkspace = nil
		if err := recovery.Reconcile(context.Background()); err != nil {
			t.Fatalf("closed-intent binding reset retry = %v", err)
		}
		if journal.workspaceBinding.SourceRunID != "" {
			t.Fatal("closed intent suppressed orphan binding cleanup on retry")
		}
	})

	t.Run("foreign owner blocks cleanup", func(t *testing.T) {
		backend, rt, cfg, launch, journal, _ := setup(t)
		leaser := cfg.VolumeLifecycleLeaser.(*fakeCodexReviewVolumeLeaser)
		if _, err := leaser.AcquireCodexReviewVolumeLease(context.Background(), strings.Repeat("1", 32), []string{launch.WorkspaceVolume, codexReviewShadowVolumeName(launch.RunID)}); err != nil {
			t.Fatal(err)
		}
		if err := backend.RecoverCodexReview(context.Background(), cfg, launch); err == nil {
			t.Fatal("recovery adopted a foreign lifecycle lease")
		}
		if _, exists := rt.vols[codexReviewShadowVolumeName(launch.RunID)]; !exists || journal.intent.State != CodexReviewIntentPrepared {
			t.Fatal("foreign lease recovery mutated pre-start objects or intent")
		}
	})

	t.Run("wipes the host credential stage even when the lease is blocked", func(t *testing.T) {
		backend, _, cfg, launch, journal, _ := setup(t)
		// A crash in the seeder window leaves both a plaintext host credential
		// stage and a leftover single-volume prep container; that partial-attachment
		// container makes RecoverCodexReviewVolumeLease fail closed (ForeignOwner), a
		// pre-existing recovery block tracked separately. A foreign holder occupying
		// the lease reproduces that same pre-lease block here. Recovery must still
		// have wiped the daemon's own host stage first, since the early wipe runs
		// before the lease gate and needs no lease/claim/owner proof.
		stage := plantStage(t, backend, launch.RunID)
		leaser := cfg.VolumeLifecycleLeaser.(*fakeCodexReviewVolumeLeaser)
		if _, err := leaser.AcquireCodexReviewVolumeLease(context.Background(), strings.Repeat("1", 32),
			[]string{launch.WorkspaceVolume, codexReviewShadowVolumeName(launch.RunID)}); err != nil {
			t.Fatal(err)
		}
		if err := backend.RecoverCodexReview(context.Background(), cfg, launch); err == nil {
			t.Fatal("expected recovery to be blocked by the occupied lease")
		}
		if journal.intent.State == CodexReviewIntentClosed {
			t.Fatal("a lease-blocked recovery must not close the intent")
		}
		if _, err := os.Stat(stage); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("lease-blocked recovery left the host credential stage behind: stat err = %v", err)
		}
	})

	t.Run("keeps the intent open when the credential wipe fails", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root bypasses the directory permission that forces RemoveAll to fail")
		}
		backend, _, cfg, launch, journal, owner := setup(t)
		leaser := cfg.VolumeLifecycleLeaser.(*fakeCodexReviewVolumeLeaser)
		if _, err := leaser.AcquireCodexReviewVolumeLease(context.Background(), owner.Value,
			[]string{launch.WorkspaceVolume, codexReviewShadowVolumeName(launch.RunID)}); err != nil {
			t.Fatal(err)
		}
		// A crash-residue stage whose directory is made unwritable, so the early
		// RemoveAll of the plaintext auth.json fails. Recovery must fail closed:
		// return an error and leave the intent open for a later reconcile to retry,
		// never close the intent over a credential it could not wipe (the
		// closed-intent path never revisits the wipe).
		stage := plantStage(t, backend, launch.RunID)
		if err := os.Chmod(stage, 0o500); err != nil { //nolint:gosec // G302: directory perms; the execute bit is needed to traverse a dir, and 0500 removes only write to force the RemoveAll failure
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(stage, 0o700) }) //nolint:gosec // G302: restore directory perms so t.TempDir cleanup can remove it

		if err := backend.RecoverCodexReview(context.Background(), cfg, launch); err == nil {
			t.Fatal("expected recovery to fail closed when the credential wipe fails")
		}
		if journal.intent.State == CodexReviewIntentClosed {
			t.Fatal("a failed credential wipe must not close the intent")
		}
		if _, err := os.Stat(filepath.Join(stage, codexReviewSnapshotAuthName)); err != nil {
			t.Fatalf("the un-wiped credential should remain for a retry: %v", err)
		}
	})
}

type codexReviewPrepRecoveryFixture struct {
	backend *CodexReviewLifecycle
	rt      *fakeRuntime
	cfg     CodexReviewConfig
	launch  CodexReviewLaunchSpec
	journal *fakeCodexReviewJournal
	leaser  *RuntimeCodexReviewVolumeLeaser
	owner   Label
	names   codexReviewResourceNames
}

func testCodexReviewPrepRecoveryFixture(t *testing.T) codexReviewPrepRecoveryFixture {
	t.Helper()
	backend, rt, cfg, launch, journal := testCodexReviewLifecycle(t)
	digest, err := codexReviewIntentDigest(cfg, launch)
	if err != nil {
		t.Fatal(err)
	}
	owner := testOwnershipLabel()
	names := codexReviewNames(launch.RunID)
	journal.intent = currentCodexReviewIntentForTest(launch.RunID, digest, owner.Value)
	for index := range journal.intent.Resources {
		switch journal.intent.Resources[index].Name {
		case names.shadowVolume:
			journal.intent.Resources[index].Fingerprint = "shadow-created"
		case names.snapshotVolume:
			journal.intent.Resources[index].Fingerprint = "snapshot-created"
		}
	}
	rt.vols[names.shadowVolume] = &fakeVol{
		labels: append(runLabels(launch.RunID), owner), created: "shadow-created",
	}
	rt.vols[names.snapshotVolume] = &fakeVol{
		labels: append(runLabels(launch.RunID), owner), created: "snapshot-created",
	}
	leaser, err := NewRuntimeCodexReviewVolumeLeaser(rt)
	if err != nil {
		t.Fatal(err)
	}
	cfg.VolumeLifecycleLeaser = leaser
	return codexReviewPrepRecoveryFixture{
		backend: backend, rt: rt, cfg: cfg, launch: launch, journal: journal,
		leaser: leaser, owner: owner, names: names,
	}
}

func setCodexReviewIntentResourceEvidenceForTest(
	t *testing.T,
	intent *CodexReviewLaunchIntent,
	name, token, fingerprint string,
) {
	t.Helper()
	for index := range intent.Resources {
		if intent.Resources[index].Name == name {
			intent.Resources[index].OwnershipToken = token
			intent.Resources[index].Fingerprint = fingerprint
			return
		}
	}
	t.Fatalf("intent resource %q is not journaled", name)
}

func TestRecoverCodexReviewReapsPartialPreparationAttachmentsForRelaunch(t *testing.T) {
	for _, tc := range []struct {
		name          string
		containerName func(codexReviewResourceNames) string
		volumeName    func(codexReviewResourceNames, CodexReviewLaunchSpec) string
		owner         func(Label) Label
		seededAuth    bool
	}{
		{
			name:          "intent-token snapshot seeder",
			containerName: func(names codexReviewResourceNames) string { return names.snapshotSeeder },
			volumeName:    func(names codexReviewResourceNames, _ CodexReviewLaunchSpec) string { return names.snapshotVolume },
			owner:         func(owner Label) Label { return owner },
			seededAuth:    true,
		},
		{
			name:          "per-resource-token workspace observer",
			containerName: func(names codexReviewResourceNames) string { return names.workspaceObserver },
			volumeName:    func(_ codexReviewResourceNames, launch CodexReviewLaunchSpec) string { return launch.WorkspaceVolume },
			owner: func(Label) Label {
				return Label{Key: ownershipLabelKey, Value: strings.Repeat("2", 32)}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := testCodexReviewPrepRecoveryFixture(t)
			container := tc.containerName(fixture.names)
			volume := tc.volumeName(fixture.names, fixture.launch)
			containerOwner := tc.owner(fixture.owner)
			setCodexReviewIntentResourceEvidenceForTest(
				t, fixture.journal.intent, container, containerOwner.Value, "prep-created",
			)
			fixture.rt.ctrs[container] = &fakeCtr{
				spec: ContainerSpec{
					Name: container, Labels: append(runLabels(fixture.launch.RunID), containerOwner),
					Mounts: []Mount{{Type: MountVolume, Source: volume, Target: "/prep"}},
				},
				created: "prep-created",
			}
			if tc.seededAuth {
				fixture.rt.snapshotFiles[fixture.names.snapshotVolume] = map[string][]byte{
					codexReviewSnapshotAuthName: []byte("SECRET-KEY"),
				}
			}

			if err := fixture.backend.RecoverCodexReview(
				context.Background(), fixture.cfg, fixture.launch,
			); err != nil {
				t.Fatalf("partial preparation recovery = %v, want convergence", err)
			}
			if _, exists := fixture.rt.ctrs[container]; exists {
				t.Fatalf("recovery left preparation container %q", container)
			}
			for _, volume := range []string{fixture.names.shadowVolume, fixture.names.snapshotVolume} {
				if _, exists := fixture.rt.vols[volume]; exists {
					t.Fatalf("recovery left owned volume %q", volume)
				}
			}
			if fixture.journal.intent.State != CodexReviewIntentClosed {
				t.Fatalf("intent state = %q, want closed", fixture.journal.intent.State)
			}
			if len(fixture.leaser.holders) != 0 || len(fixture.leaser.transfers) != 0 {
				t.Fatalf("recovery left volume lease state: holders=%v transfers=%v",
					fixture.leaser.holders, fixture.leaser.transfers)
			}
			relaunched, err := fixture.backend.CodexReview(context.Background(), fixture.cfg, fixture.launch)
			if err != nil {
				t.Fatalf("relaunch after partial preparation recovery = %v", err)
			}
			t.Cleanup(func() { _ = relaunched.Close() })
		})
	}
}

func TestRecoverCodexReviewReapsLegacyPartialPreparationAttachmentForRelaunch(t *testing.T) {
	backend, rt, cfg, launch, journal := testCodexReviewLifecycle(t)
	digest, err := codexReviewIntentDigest(cfg, launch)
	if err != nil {
		t.Fatal(err)
	}
	owner := testOwnershipLabel()
	names := preSnapshotCodexReviewNames(launch.RunID)
	observerOwner := Label{Key: ownershipLabelKey, Value: strings.Repeat("2", 32)}
	journal.intent = preSnapshotCodexReviewIntentForTest(launch.RunID, digest, owner.Value)
	setCodexReviewIntentResourceEvidenceForTest(
		t, journal.intent, names.workspaceObserver, observerOwner.Value, "legacy-prep-created",
	)
	setCodexReviewIntentResourceEvidenceForTest(
		t, journal.intent, names.shadowVolume, owner.Value, "legacy-shadow-created",
	)
	rt.vols[names.shadowVolume] = &fakeVol{
		labels: append(runLabels(launch.RunID), owner), created: "legacy-shadow-created",
	}
	rt.ctrs[names.workspaceObserver] = &fakeCtr{
		spec: ContainerSpec{
			Name:   names.workspaceObserver,
			Labels: append(runLabels(launch.RunID), observerOwner),
			Mounts: []Mount{{
				Type: MountVolume, Source: launch.WorkspaceVolume, Target: "/legacy-prep", ReadOnly: true,
			}},
		},
		created: "legacy-prep-created",
	}
	leaser, err := NewRuntimeCodexReviewVolumeLeaser(rt)
	if err != nil {
		t.Fatal(err)
	}
	cfg.VolumeLifecycleLeaser = leaser

	if err := backend.RecoverCodexReview(context.Background(), cfg, launch); err != nil {
		t.Fatalf("legacy partial preparation recovery = %v, want convergence", err)
	}
	if _, exists := rt.ctrs[names.workspaceObserver]; exists {
		t.Fatal("legacy recovery left the workspace-only observer")
	}
	if _, exists := rt.vols[names.shadowVolume]; exists {
		t.Fatal("legacy recovery left the owned shadow volume")
	}
	if journal.intent.State != CodexReviewIntentClosed || len(leaser.holders) != 0 {
		t.Fatalf("legacy recovery did not close and release: state=%q holders=%v",
			journal.intent.State, leaser.holders)
	}
	relaunched, err := backend.CodexReview(context.Background(), cfg, launch)
	if err != nil {
		t.Fatalf("relaunch after legacy partial preparation recovery = %v", err)
	}
	t.Cleanup(func() { _ = relaunched.Close() })
}

func TestRecoverCodexReviewLeavesUnauthenticatedPartialPreparationAttachments(t *testing.T) {
	for _, tc := range []struct {
		name string
		add  func(*testing.T, codexReviewPrepRecoveryFixture) string
	}{
		{
			name: "observer token never journaled",
			add: func(_ *testing.T, fixture codexReviewPrepRecoveryFixture) string {
				name := fixture.names.workspaceObserver
				fixture.rt.ctrs[name] = &fakeCtr{
					spec: ContainerSpec{
						Name: name, Labels: []Label{fixture.owner},
						Mounts: []Mount{{Type: MountVolume, Source: fixture.launch.WorkspaceVolume, Target: "/prep"}},
					},
					created: "forged-observer-created",
				}
				return name
			},
		},
		{
			name: "empty observer ownership label",
			add: func(_ *testing.T, fixture codexReviewPrepRecoveryFixture) string {
				name := fixture.names.workspaceObserver
				fixture.rt.ctrs[name] = &fakeCtr{
					spec: ContainerSpec{
						Name: name, Labels: []Label{{Key: ownershipLabelKey}},
						Mounts: []Mount{{Type: MountVolume, Source: fixture.launch.WorkspaceVolume, Target: "/prep"}},
					},
					created: "empty-owner-observer-created",
				}
				return name
			},
		},
		{
			name: "wrong recorded token",
			add: func(t *testing.T, fixture codexReviewPrepRecoveryFixture) string {
				name := fixture.names.snapshotSeeder
				setCodexReviewIntentResourceEvidenceForTest(
					t, fixture.journal.intent, name, fixture.owner.Value, "prep-created",
				)
				fixture.rt.ctrs[name] = &fakeCtr{
					spec: ContainerSpec{
						Name:   name,
						Labels: []Label{{Key: ownershipLabelKey, Value: strings.Repeat("f", 32)}},
						Mounts: []Mount{{Type: MountVolume, Source: fixture.names.snapshotVolume, Target: "/prep"}},
					},
					created: "prep-created",
				}
				return name
			},
		},
		{
			name: "forged label on replacement",
			add: func(t *testing.T, fixture codexReviewPrepRecoveryFixture) string {
				name := fixture.names.snapshotSeeder
				setCodexReviewIntentResourceEvidenceForTest(
					t, fixture.journal.intent, name, fixture.owner.Value, "journal-created",
				)
				fixture.rt.ctrs[name] = &fakeCtr{
					spec: ContainerSpec{
						Name: name, Labels: []Label{fixture.owner},
						Mounts: []Mount{{Type: MountVolume, Source: fixture.names.snapshotVolume, Target: "/prep"}},
					},
					created: "replacement-created",
				}
				return name
			},
		},
		{
			name: "unjournaled container",
			add: func(_ *testing.T, fixture codexReviewPrepRecoveryFixture) string {
				name := "foreign-prep"
				fixture.rt.ctrs[name] = &fakeCtr{
					spec: ContainerSpec{
						Name: name, Labels: []Label{fixture.owner},
						Mounts: []Mount{{Type: MountVolume, Source: fixture.names.snapshotVolume, Target: "/prep"}},
					},
					created: "foreign-created",
				}
				return name
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := testCodexReviewPrepRecoveryFixture(t)
			container := tc.add(t, fixture)
			stage := codexReviewSnapshotStagePath(fixture.backend.cfg.ExportRoot, fixture.launch.RunID)
			if err := os.Mkdir(stage, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(
				filepath.Join(stage, codexReviewSnapshotAuthName), []byte("SECRET-KEY"), 0o400,
			); err != nil {
				t.Fatal(err)
			}

			err := fixture.backend.RecoverCodexReview(context.Background(), fixture.cfg, fixture.launch)
			if !errors.Is(err, ErrConformance) ||
				!strings.Contains(err.Error(), ErrCodexReviewVolumeLeaseForeignOwner.Error()) {
				t.Fatalf("unauthenticated partial recovery = %v, want foreign-owner conformance refusal", err)
			}
			if _, exists := fixture.rt.ctrs[container]; !exists {
				t.Fatalf("recovery deleted unauthenticated container %q", container)
			}
			if _, exists := fixture.rt.vols[fixture.names.snapshotVolume]; !exists {
				t.Fatal("foreign-owner refusal deleted the snapshot volume")
			}
			if fixture.journal.intent.State == CodexReviewIntentClosed {
				t.Fatal("foreign-owner refusal closed the intent")
			}
			if _, err := os.Stat(stage); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("foreign-owner refusal left the host credential stage: stat err = %v", err)
			}
		})
	}
}

// TestCreatePrivateStageDirDefeatsSymlinkPreattack covers the R1 stage
// creation guard: because ExportRoot can default to a shared temp dir, the
// credential stage must survive a pre-created symlink or stale residue at its
// path and end up a fresh, private, real directory the caller can safely write
// 0400 secrets into.
func TestCreatePrivateStageDirDefeatsSymlinkPreattack(t *testing.T) {
	base := t.TempDir()

	// Happy path: a fresh path becomes a real private directory.
	fresh := filepath.Join(base, "fresh")
	if err := createPrivateStageDir(fresh); err != nil {
		t.Fatalf("fresh stage: %v", err)
	}
	if info, err := os.Lstat(fresh); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("fresh stage is not a real directory: info=%v err=%v", info, err)
	}

	// Symlink pre-attack: an attacker pre-creates the stage path as a symlink to
	// a directory they control. createPrivateStageDir must unlink the symlink
	// (never its target) and install a real directory in its place, so the later
	// 0400 write lands in the daemon-owned stage, not the attacker's directory.
	target := t.TempDir()
	sentinel := filepath.Join(target, "attacker-owned")
	if err := os.WriteFile(sentinel, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(base, "linked")
	if err := os.Symlink(target, linked); err != nil {
		t.Fatal(err)
	}
	if err := createPrivateStageDir(linked); err != nil {
		t.Fatalf("symlinked stage: %v", err)
	}
	info, err := os.Lstat(linked)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("stage is still a symlink or not a directory: info=%v err=%v", info, err)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("attacker target was followed and disturbed: %v", err)
	}
	if entries, err := os.ReadDir(linked); err != nil || len(entries) != 0 {
		t.Fatalf("replacement stage is not a fresh empty directory: entries=%v err=%v", entries, err)
	}

	// Residue: a pre-existing directory with stale content is removed+recreated.
	residue := filepath.Join(base, "residue")
	if err := os.Mkdir(residue, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(residue, "stale"), []byte("old"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := createPrivateStageDir(residue); err != nil {
		t.Fatalf("residue stage: %v", err)
	}
	if entries, err := os.ReadDir(residue); err != nil || len(entries) != 0 {
		t.Fatalf("residue stage was not cleared: entries=%v err=%v", entries, err)
	}
}

func TestCodexReviewRecoveryMarksClosedOutcomeReady(t *testing.T) {
	backend, _, cfg, launch, journal := testCodexReviewLifecycle(t)
	owner := testOwnershipLabel()
	journal.intent = &CodexReviewLaunchIntent{
		RunID: launch.RunID, OwnershipToken: owner.Value,
		ShadowVolume:    codexReviewShadowVolumeName(launch.RunID),
		Network:         codexReviewNetworkName(launch.RunID),
		ReviewContainer: codexReviewContainerName(launch.RunID),
		State:           CodexReviewIntentClosed,
		Resources: []CodexReviewIntentResource{
			{Name: codexReviewWorkspaceObserverName(launch.RunID)},
			{Name: codexReviewShadowInitializerName(launch.RunID), OwnershipToken: owner.Value},
			{Name: codexReviewShadowObserverName(launch.RunID)},
			{Name: codexReviewShadowVolumeName(launch.RunID), OwnershipToken: owner.Value},
			{Name: codexReviewNetworkName(launch.RunID), OwnershipToken: owner.Value},
			{Name: codexReviewContainerName(launch.RunID), OwnershipToken: owner.Value},
		},
	}
	journal.outcomes = map[string]CodexReviewSourceOutcome{launch.RunID: {
		InvocationID: domain.InvocationID(launch.RunID),
		FailureClass: domain.ReviewFailureTransient,
		Failure:      "cleanup completed before the ready mark",
	}}
	recovery, err := NewCodexReviewRecovery(backend, journal, cfg.VolumeLifecycleLeaser, cfg.InputRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := recovery.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !journal.ready[launch.RunID] {
		t.Fatal("closed collected outcome remained not ready")
	}
}

func TestCodexReviewRecoveryCleansWorkspaceWithoutLaunchIntent(t *testing.T) {
	for _, preparation := range []string{"pending", "finalized"} {
		t.Run(preparation, func(t *testing.T) {
			ctx := context.Background()
			fx := newHandoffFixture(t)
			seed := fx.seed(t).Seed
			backend := fx.codexReviewLifecycle(t)
			journal := &fakeCodexReviewJournal{}
			runID := "review-orphan-" + preparation
			volume := namesFor(runID).Workspace
			switch preparation {
			case "pending":
				owner := testOwnershipLabel()
				if err := journal.PutCodexReviewWorkspaceBinding(ctx, CodexReviewWorkspaceBinding{
					SourceRunID: runID, Volume: volume, OwnershipToken: owner.Value,
				}); err != nil {
					t.Fatal(err)
				}
				if err := fx.rt.CreateVolume(ctx, volume, 64, append(runLabels(runID), owner)); err != nil {
					t.Fatal(err)
				}
			case "finalized":
				if _, err := backend.PrepareCodexReviewWorkspace(
					ctx, journal, runID, seed.SourceDir, seed.Base, 64,
				); err != nil {
					t.Fatal(err)
				}
			}
			leaser, err := NewRuntimeCodexReviewVolumeLeaser(fx.rt)
			if err != nil {
				t.Fatal(err)
			}
			recovery, err := NewCodexReviewRecovery(backend, journal, leaser, "")
			if err != nil {
				t.Fatal(err)
			}
			if err := recovery.Reconcile(ctx); err != nil {
				t.Fatal(err)
			}
			if _, exists := fx.rt.vols[volume]; exists {
				t.Fatal("startup recovery left a prepared workspace without a launch intent")
			}
			if journal.workspaceBinding.SourceRunID != "" {
				t.Fatal("startup recovery left the workspace binding reusable")
			}
			binding, err := backend.PrepareCodexReviewWorkspace(
				ctx, journal, runID, seed.SourceDir, seed.Base, 64,
			)
			if err != nil || binding.CreationFingerprint == "" {
				t.Fatalf("prepare same invocation after recovery = %#v, %v", binding, err)
			}
		})
	}
}

func TestCodexReviewRecoveryMarksFenceOnlyRejectedOutcomeReady(t *testing.T) {
	ctx := context.Background()
	fx := newHandoffFixture(t)
	backend := fx.codexReviewLifecycle(t)
	cfg, _ := testCodexReview(t)
	leaser, err := NewRuntimeCodexReviewVolumeLeaser(fx.rt)
	if err != nil {
		t.Fatal(err)
	}
	const runID = "review-fence-only-rejection"
	journal := &fakeCodexReviewJournal{outcomes: map[string]CodexReviewSourceOutcome{
		runID: {
			InvocationID: domain.InvocationID(runID), FailureClass: domain.ReviewFailureContradiction,
			Failure: "rejected request fenced before launch intent", AbortRequired: true,
		},
	}}
	recovery, err := NewCodexReviewRecovery(backend, journal, leaser, cfg.InputRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := recovery.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if !journal.ready[runID] {
		t.Fatal("fence-only rejected outcome remained unready after recovery")
	}
}

func TestCodexReviewRecoveryDoesNotLetInvalidIntentStarveLaterCleanup(t *testing.T) {
	backend, _, cfg, _, journal := testCodexReviewLifecycle(t)
	validID := "review-2"
	owner := testOwnershipLabel()
	valid := &CodexReviewLaunchIntent{
		RunID: validID, OwnershipToken: owner.Value,
		ShadowVolume: codexReviewShadowVolumeName(validID), Network: codexReviewNetworkName(validID),
		ReviewContainer: codexReviewContainerName(validID), State: CodexReviewIntentClosed,
		Resources: []CodexReviewIntentResource{
			{Name: codexReviewWorkspaceObserverName(validID)},
			{Name: codexReviewShadowInitializerName(validID), OwnershipToken: owner.Value},
			{Name: codexReviewShadowObserverName(validID)},
			{Name: codexReviewShadowVolumeName(validID), OwnershipToken: owner.Value},
			{Name: codexReviewNetworkName(validID), OwnershipToken: owner.Value},
			{Name: codexReviewContainerName(validID), OwnershipToken: owner.Value},
		},
	}
	journal.intent = nil
	journal.extraIntents = map[string]*CodexReviewLaunchIntent{
		"review-1": valid,
		validID:    valid,
	}
	journal.failIntentByID = map[string]error{"review-1": ErrConformance}
	journal.outcomes = map[string]CodexReviewSourceOutcome{validID: {
		InvocationID: domain.InvocationID(validID), FailureClass: domain.ReviewFailureTransient,
		Failure: "cleanup completed before the ready mark",
	}}
	recovery, err := NewCodexReviewRecovery(backend, journal, cfg.VolumeLifecycleLeaser, cfg.InputRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := recovery.Reconcile(context.Background()); !errors.Is(err, ErrConformance) {
		t.Fatalf("recovery error = %v, want invalid first intent", err)
	}
	if !journal.ready[validID] {
		t.Fatal("invalid first intent starved a later recoverable outcome")
	}
}

func TestCodexReviewRecoveryFailsClosedOnRewrittenIntentKey(t *testing.T) {
	backend, rt, cfg, launch, journal := testCodexReviewLifecycle(t)
	rewrittenID := launch.RunID + "-rewritten"
	journal.intent = nil
	journal.extraIntents = map[string]*CodexReviewLaunchIntent{
		rewrittenID: {RunID: launch.RunID},
	}
	recovery, err := NewCodexReviewRecovery(backend, journal, cfg.VolumeLifecycleLeaser, cfg.InputRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := recovery.Reconcile(context.Background()); err == nil {
		t.Fatal("recovery accepted a rewritten intent primary key")
	}
	if _, exists := rt.vols[launch.WorkspaceVolume]; !exists {
		t.Fatal("orphan pass deleted a workspace still owned by an intent")
	}
	if journal.workspaceBinding.SourceRunID != launch.RunID {
		t.Fatal("orphan pass deleted a binding still owned by an intent")
	}
}

func TestRecoverCodexReviewRefusesForeignSameNameReviewContainer(t *testing.T) {
	backend, rt, cfg, launch, journal := testCodexReviewLifecycle(t)
	owner := testOwnershipLabel()
	digest, err := codexReviewIntentDigest(cfg, launch)
	if err != nil {
		t.Fatal(err)
	}
	review := codexReviewContainerName(launch.RunID)
	journal.intent = &CodexReviewLaunchIntent{
		RunID: launch.RunID, SpecDigest: digest, OwnershipToken: owner.Value,
		ShadowVolume: codexReviewShadowVolumeName(launch.RunID), Network: codexReviewNetworkName(launch.RunID),
		ReviewContainer: review, State: CodexReviewIntentPreparing,
		Resources: []CodexReviewIntentResource{
			{Name: codexReviewWorkspaceObserverName(launch.RunID)},
			{Name: codexReviewShadowInitializerName(launch.RunID), OwnershipToken: owner.Value},
			{Name: codexReviewShadowObserverName(launch.RunID)},
			{Name: codexReviewShadowVolumeName(launch.RunID), OwnershipToken: owner.Value},
			{Name: codexReviewNetworkName(launch.RunID), OwnershipToken: owner.Value},
			{Name: review, OwnershipToken: owner.Value, Fingerprint: "original"},
		},
	}
	rt.ctrs[review] = &fakeCtr{spec: ContainerSpec{Name: review, Labels: []Label{{
		Key: ownershipLabelKey, Value: strings.Repeat("1", 32),
	}}}, created: "replacement"}
	if err := backend.RecoverCodexReview(context.Background(), cfg, launch); err == nil {
		t.Fatal("recovery accepted a foreign same-name review container")
	}
	if _, ok := rt.ctrs[review]; !ok {
		t.Fatal("recovery deleted a foreign same-name review container")
	}
}

func TestRecoverCodexReviewRejectsForgedFixedResourceOwner(t *testing.T) {
	backend, rt, cfg, launch, journal := testCodexReviewLifecycle(t)
	owner := testOwnershipLabel()
	digest, err := codexReviewIntentDigest(cfg, launch)
	if err != nil {
		t.Fatal(err)
	}
	shadow := codexReviewShadowVolumeName(launch.RunID)
	journal.intent = &CodexReviewLaunchIntent{
		RunID: launch.RunID, SpecDigest: digest, OwnershipToken: owner.Value,
		ShadowVolume: shadow, Network: codexReviewNetworkName(launch.RunID),
		ReviewContainer: codexReviewContainerName(launch.RunID), State: CodexReviewIntentPreparing,
		Resources: []CodexReviewIntentResource{
			{Name: codexReviewWorkspaceObserverName(launch.RunID)},
			{Name: codexReviewShadowInitializerName(launch.RunID), OwnershipToken: owner.Value},
			{Name: codexReviewShadowObserverName(launch.RunID)},
			{Name: shadow, OwnershipToken: strings.Repeat("1", 32), Fingerprint: "shadow"},
			{Name: codexReviewNetworkName(launch.RunID), OwnershipToken: owner.Value},
			{Name: codexReviewContainerName(launch.RunID), OwnershipToken: owner.Value},
		},
	}
	rt.vols[shadow] = &fakeVol{labels: []Label{owner}, created: "shadow"}
	if err := backend.RecoverCodexReview(context.Background(), cfg, launch); err == nil {
		t.Fatal("recovery accepted a forged fixed-resource ownership token")
	}
	if _, ok := rt.vols[shadow]; !ok {
		t.Fatal("recovery deleted a resource after rejecting its forged owner")
	}
}

func TestRecoverCodexReviewKeepsIntentOpenWhenShadowDeletionLies(t *testing.T) {
	backend, rt, cfg, launch, journal := testCodexReviewLifecycle(t)
	owner := testOwnershipLabel()
	digest, err := codexReviewIntentDigest(cfg, launch)
	if err != nil {
		t.Fatal(err)
	}
	shadow := codexReviewShadowVolumeName(launch.RunID)
	journal.intent = &CodexReviewLaunchIntent{
		RunID: launch.RunID, SpecDigest: digest, OwnershipToken: owner.Value,
		ShadowVolume: shadow, Network: codexReviewNetworkName(launch.RunID),
		ReviewContainer: codexReviewContainerName(launch.RunID), State: CodexReviewIntentPreparing,
		Resources: []CodexReviewIntentResource{
			{Name: codexReviewWorkspaceObserverName(launch.RunID)},
			{Name: codexReviewShadowInitializerName(launch.RunID), OwnershipToken: owner.Value},
			{Name: codexReviewShadowObserverName(launch.RunID)},
			{Name: shadow, OwnershipToken: owner.Value, Fingerprint: "shadow"},
			{Name: codexReviewNetworkName(launch.RunID), OwnershipToken: owner.Value},
			{Name: codexReviewContainerName(launch.RunID), OwnershipToken: owner.Value},
		},
	}
	rt.vols[shadow] = &fakeVol{labels: []Label{owner}, created: "shadow"}
	rt.onDeleteVolume = func(name string) (bool, error) { return name == shadow, nil }
	if err := backend.RecoverCodexReview(context.Background(), cfg, launch); err == nil {
		t.Fatal("recovery closed an intent after a lying shadow delete")
	}
	if journal.intent.State != CodexReviewIntentPreparing {
		t.Fatalf("intent state = %q, want open preparing", journal.intent.State)
	}
}

func TestRecoverCodexReviewRetriesLeaseReleaseBeforeClosingIntent(t *testing.T) {
	backend, rt, cfg, launch, journal := testCodexReviewLifecycle(t)
	owner := testOwnershipLabel()
	digest, _ := codexReviewIntentDigest(cfg, launch)
	shadow := codexReviewShadowVolumeName(launch.RunID)
	journal.intent = &CodexReviewLaunchIntent{RunID: launch.RunID, SpecDigest: digest, OwnershipToken: owner.Value, ShadowVolume: shadow, Network: codexReviewNetworkName(launch.RunID), ReviewContainer: codexReviewContainerName(launch.RunID), State: CodexReviewIntentPreparing, Resources: []CodexReviewIntentResource{{Name: codexReviewWorkspaceObserverName(launch.RunID)}, {Name: codexReviewShadowInitializerName(launch.RunID), OwnershipToken: owner.Value}, {Name: codexReviewShadowObserverName(launch.RunID)}, {Name: shadow, OwnershipToken: owner.Value, Fingerprint: "shadow"}, {Name: codexReviewNetworkName(launch.RunID), OwnershipToken: owner.Value}, {Name: codexReviewContainerName(launch.RunID), OwnershipToken: owner.Value}}}
	rt.vols[shadow] = &fakeVol{labels: []Label{owner}, created: "shadow"}
	leaser := cfg.VolumeLifecycleLeaser.(*fakeCodexReviewVolumeLeaser)
	leaser.releaseErr = errors.New("release failed")
	if err := backend.RecoverCodexReview(context.Background(), cfg, launch); !errors.Is(err, ErrCodexReviewOperational) ||
		!errors.Is(err, ErrConformance) || journal.intent.State != CodexReviewIntentPreparing {
		t.Fatalf("release failure = %v, state = %q; want operational retry", err, journal.intent.State)
	}
	leaser.releaseErr = nil
	if err := backend.RecoverCodexReview(context.Background(), cfg, launch); err != nil || journal.intent.State != CodexReviewIntentClosed {
		t.Fatalf("retry = %v, state = %q", err, journal.intent.State)
	}
}

func TestBackendCodexReviewRefusesRuntimeSpecMutation(t *testing.T) {
	tests := []struct {
		name   string
		target func(string) string
	}{
		{name: "observer", target: codexReviewWorkspaceObserverName},
		{name: "shadow initializer", target: codexReviewShadowInitializerName},
		{name: "review container", target: codexReviewContainerName},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			backend, rt, cfg, launchSpec, _ := testCodexReviewLifecycle(t)
			target := tc.target(launchSpec.RunID)
			rt.onCreateContainer = func(spec ContainerSpec) error {
				if spec.Name == target {
					spec.Command[0] = "runtime-mutated"
				}
				return nil
			}
			if launch, err := backend.CodexReview(context.Background(), cfg, launchSpec); err == nil {
				_ = launch.Close()
				t.Fatal("CodexReview admitted a runtime-mutated container spec")
			}
		})
	}
}

func TestBackendCodexReviewRequiresPriorWorkspaceProvenance(t *testing.T) {
	const forgedToken = "11111111111111111111111111111111"
	t.Run("forged durable token", func(t *testing.T) {
		backend, rt, cfg, launchSpec, journal := testCodexReviewLifecycle(t)
		journal.workspaceBinding.OwnershipToken = forgedToken
		if launch, err := backend.CodexReview(context.Background(), cfg, launchSpec); err == nil {
			_ = launch.Close()
			t.Fatal("CodexReview admitted workspace provenance that did not match the live volume")
		}
		rt.mu.Lock()
		defer rt.mu.Unlock()
		if len(rt.calls) != 1 || rt.calls[0] != "inspect-volume "+launchSpec.WorkspaceVolume {
			t.Fatalf("forged provenance reached observer/runtime mutation: %v", rt.calls)
		}
	})

	t.Run("self-labeled replacement", func(t *testing.T) {
		backend, rt, cfg, launchSpec, _ := testCodexReviewLifecycle(t)
		rt.vols[launchSpec.WorkspaceVolume] = &fakeVol{
			labels: append(runLabels("attacker-run"), Label{
				Key: ownershipLabelKey, Value: forgedToken,
			}),
			created: "attacker-created",
		}
		if launch, err := backend.CodexReview(context.Background(), cfg, launchSpec); err == nil {
			_ = launch.Close()
			t.Fatal("CodexReview adopted a self-labeled replacement workspace")
		}
	})
}

func TestBackendCodexReviewRejectsConcurrentWorkspaceAttachment(t *testing.T) {
	backend, rt, cfg, launchSpec, _ := testCodexReviewLifecycle(t)
	rt.ctrs["foreign-writer"] = &fakeCtr{
		spec: ContainerSpec{
			Name: "foreign-writer", Image: cfg.ObserverImage,
			Mounts: []Mount{{
				Type: MountVolume, Source: launchSpec.WorkspaceVolume,
				Target: cfg.WorkspaceTarget,
			}},
		},
		created: "foreign-writer-created",
	}
	if launch, err := backend.CodexReview(context.Background(), cfg, launchSpec); err == nil {
		_ = launch.Close()
		t.Fatal("CodexReview started while another container retained the candidate workspace")
	}
}

func TestBackendCodexReviewLifecycleLeaseClosesFinalAttachmentWindow(t *testing.T) {
	backend, rt, cfg, launchSpec, _ := testCodexReviewLifecycle(t)
	leaser, ok := cfg.VolumeLifecycleLeaser.(*fakeCodexReviewVolumeLeaser)
	if !ok {
		t.Fatal("lifecycle fixture did not install the volume leaser")
	}
	attackerRefused := false
	rt.onStart = func(id string) error {
		if id != codexReviewContainerName(launchSpec.RunID) {
			return nil
		}
		_, err := leaser.AcquireCodexReviewVolumeLease(
			context.Background(), "attacker", []string{launchSpec.WorkspaceVolume, codexReviewShadowVolumeName(launchSpec.RunID)},
		)
		if !errors.Is(err, errFakeCodexReviewVolumeLeaseHeld) {
			t.Fatalf("attach-mutate-detach attempt acquired the final-window lease: %v", err)
		}
		attackerRefused = true
		return nil
	}
	launch, err := backend.CodexReview(context.Background(), cfg, launchSpec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = launch.Close() })
	if !attackerRefused || !leaser.transferred {
		t.Fatal("review did not atomically transfer the exclusive lifecycle lease into start")
	}
	if want := []string{
		launchSpec.WorkspaceVolume,
		codexReviewShadowVolumeName(launchSpec.RunID),
		codexReviewSnapshotVolumeName(launchSpec.RunID),
	}; !slices.Equal(leaser.volumes, want) {
		t.Fatalf("lease volumes = %v, want %v", leaser.volumes, want)
	}
}

func TestBackendCodexReviewProxyOutlivesLaunchContext(t *testing.T) {
	backend, _, cfg, launchSpec, _ := testCodexReviewLifecycle(t)
	ctx, cancel := context.WithCancel(context.Background())
	launch, err := backend.CodexReview(ctx, cfg, launchSpec)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = launch.Close() })
	cancel()
	if err := launch.proxy.ctx.Err(); err != nil {
		t.Fatalf("review proxy inherited the cancelled launch context: %v", err)
	}
}

func TestBackendCodexReviewObserverDeleteRequiresAbsence(t *testing.T) {
	backend, rt, cfg, launchSpec, _ := testCodexReviewLifecycle(t)
	observer := codexReviewWorkspaceObserverName(launchSpec.RunID)
	subverted := false
	rt.onDeleteContainer = func(id string) (bool, error) {
		if id == observer && !subverted {
			subverted = true
			return true, nil
		}
		return false, nil
	}
	if launch, err := backend.CodexReview(context.Background(), cfg, launchSpec); err == nil {
		_ = launch.Close()
		t.Fatal("CodexReview accepted an observer delete that left the observer listed")
	}
}

func TestBackendCodexReviewRetainsLeaseWhenReviewContainerAbsenceIsUnproven(t *testing.T) {
	backend, rt, cfg, launchSpec, journal := testCodexReviewLifecycle(t)
	leaser := cfg.VolumeLifecycleLeaser.(*fakeCodexReviewVolumeLeaser)
	journal.mutate = func(binding *CodexReviewJournalBinding) {
		binding.WorkspaceFingerprint = "forged-runtime-fingerprint"
	}
	review := codexReviewContainerName(launchSpec.RunID)
	rt.onDeleteContainer = func(id string) (bool, error) { return id == review, nil }
	if launch, err := backend.CodexReview(context.Background(), cfg, launchSpec); err == nil {
		_ = launch.Close()
		t.Fatal("CodexReview accepted a forged persisted fingerprint")
	}
	if leaser.released || !leaser.held {
		t.Fatal("lifecycle lease was released although the review container remained unproven absent")
	}
}

func TestBackendCodexReviewRecoversAmbiguousStartOutcome(t *testing.T) {
	t.Run("untransferred start error cleans up after owner adoption", func(t *testing.T) {
		backend, rt, cfg, launch, journal := testCodexReviewLifecycle(t)
		leaser := cfg.VolumeLifecycleLeaser.(*fakeCodexReviewVolumeLeaser)
		leaser.afterStartErr = errors.New("start response lost")
		var proxyURL string
		rt.onStart = func(id string) error {
			if id == codexReviewContainerName(launch.RunID) {
				for _, entry := range rt.ctrs[id].spec.Env {
					if value, ok := strings.CutPrefix(entry, "HTTP_PROXY="); ok {
						proxyURL = value
					}
				}
			}
			return nil
		}
		if got, err := backend.CodexReview(context.Background(), cfg, launch); err == nil || got != nil {
			t.Fatalf("CodexReview = (%v, %v), want ambiguous start failure", got, err)
		}
		if _, exists := rt.ctrs[codexReviewContainerName(launch.RunID)]; exists || !leaser.released ||
			journal.intent.State != CodexReviewIntentClosed {
			t.Fatal("absence-proven ambiguous start did not clean up through adopted lease recovery")
		}
		if conn, err := net.DialTimeout("tcp", strings.TrimPrefix(proxyURL, "http://"), 100*time.Millisecond); err == nil {
			_ = conn.Close()
			t.Fatal("absence-proven start recovery leaked the credential proxy")
		}
		if err := backend.RecoverCodexReview(context.Background(), cfg, launch); err != nil {
			t.Fatalf("closed recovery = %v, want nil", err)
		}
	})

	t.Run("transferred start error reaps the started review for a fresh retry", func(t *testing.T) {
		backend, rt, cfg, launch, journal := testCodexReviewLifecycle(t)
		leaser := cfg.VolumeLifecycleLeaser.(*fakeCodexReviewVolumeLeaser)
		leaser.afterStartErr = errors.New("start response lost")
		leaser.afterStartTransfers = true
		if got, err := backend.CodexReview(context.Background(), cfg, launch); err == nil || got != nil {
			t.Fatalf("CodexReview = (%v, %v), want ambiguous start failure", got, err)
		}
		if journal.intent.State != CodexReviewIntentClosed {
			t.Fatalf("intent state = %q, want closed", journal.intent.State)
		}
		if _, exists := rt.ctrs[codexReviewContainerName(launch.RunID)]; exists {
			t.Fatal("transferred ambiguous start left the credential-bearing review running")
		}
		if !leaser.released || leaser.transferred || leaser.held {
			t.Fatal("transferred ambiguous start did not return the lifecycle lease")
		}
		leaser.afterStartErr = nil
		leaser.afterStartTransfers = false
		relaunched, err := backend.CodexReview(context.Background(), cfg, launch)
		if err != nil {
			t.Fatalf("relaunch after reaped transferred start = %v, want success", err)
		}
		_ = relaunched.Close()
	})
}

func TestBackendCodexReviewStartingStatePrecedesRuntimeStart(t *testing.T) {
	backend, rt, cfg, launch, journal := testCodexReviewLifecycle(t)
	journal.failStarting = errors.New("journal unavailable")
	if got, err := backend.CodexReview(context.Background(), cfg, launch); err == nil || got != nil {
		t.Fatalf("CodexReview = (%v, %v), want starting transition failure", got, err)
	}
	if journal.intent.State != CodexReviewIntentPrepared {
		t.Fatalf("intent state = %q, want prepared", journal.intent.State)
	}
	if rt.callIndex("start-container "+codexReviewContainerName(launch.RunID)) != -1 {
		t.Fatal("runtime start ran without a durable starting transition")
	}
}

func TestBackendCodexReviewReapsStartedReviewWhenHandoffJournalFails(t *testing.T) {
	backend, rt, cfg, launch, journal := testCodexReviewLifecycle(t)
	journal.failStarted = errors.New("journal response lost")
	if got, err := backend.CodexReview(context.Background(), cfg, launch); err == nil || got != nil {
		t.Fatalf("CodexReview = (%v, %v), want post-start journal failure", got, err)
	}
	leaser := cfg.VolumeLifecycleLeaser.(*fakeCodexReviewVolumeLeaser)
	if journal.intent.State != CodexReviewIntentClosed {
		t.Fatalf("intent state = %q, want closed", journal.intent.State)
	}
	if _, exists := rt.ctrs[codexReviewContainerName(launch.RunID)]; exists {
		t.Fatal("failed handoff record left the credential-bearing review running")
	}
	if !leaser.released || leaser.transferred || leaser.held {
		t.Fatal("failed handoff record did not return the lifecycle lease")
	}
	if err := backend.RecoverCodexReview(context.Background(), cfg, launch); err != nil {
		t.Fatalf("closed recovery = %v, want nil", err)
	}
	journal.failStarted = nil
	relaunched, err := backend.CodexReview(context.Background(), cfg, launch)
	if err != nil {
		t.Fatalf("relaunch after failed handoff record = %v, want success", err)
	}
	_ = relaunched.Close()
}

// crashedTransferredCodexReview drives a launch to the post-Start,
// pre-`started` crash window and freezes it there: the handoff record fails
// and the in-process recovery is refused at the coordinator, modeling a
// process death before recovery could run. The caller gets the durable
// `starting` state, the transferred lease, and the running review container.
func crashedTransferredCodexReview(
	t *testing.T,
) (*CodexReviewLifecycle, *fakeRuntime, CodexReviewConfig, CodexReviewLaunchSpec, *fakeCodexReviewJournal, *fakeCodexReviewVolumeLeaser) {
	t.Helper()
	backend, rt, cfg, launch, journal := testCodexReviewLifecycle(t)
	leaser := cfg.VolumeLifecycleLeaser.(*fakeCodexReviewVolumeLeaser)
	journal.failStarted = errors.New("journal response lost")
	leaser.recoverErr = errors.New("coordinator unavailable")
	if got, err := backend.CodexReview(context.Background(), cfg, launch); err == nil || got != nil {
		t.Fatalf("CodexReview = (%v, %v), want post-start journal failure", got, err)
	}
	if journal.intent.State != CodexReviewIntentStarting || !leaser.transferred {
		t.Fatal("crashed launch fixture did not leave a transferred starting state")
	}
	if _, exists := rt.ctrs[codexReviewContainerName(launch.RunID)]; !exists {
		t.Fatal("crashed launch fixture lost its running review container")
	}
	journal.failStarted = nil
	leaser.recoverErr = nil
	return backend, rt, cfg, launch, journal, leaser
}

func TestRecoverCodexReviewReapsTransferredAttachmentForRelaunch(t *testing.T) {
	for _, state := range []CodexReviewIntentState{
		CodexReviewIntentPreparing, CodexReviewIntentPrepared, CodexReviewIntentStarting,
	} {
		t.Run(string(state), func(t *testing.T) {
			backend, rt, cfg, launch, journal, leaser := crashedTransferredCodexReview(t)
			journal.intent.State = state
			if state != CodexReviewIntentStarting {
				rt.ctrs[codexReviewContainerName(launch.RunID)].started = false
			}
			if state == CodexReviewIntentPreparing {
				for i := range journal.intent.Resources {
					if journal.intent.Resources[i].Name == codexReviewContainerName(launch.RunID) {
						journal.intent.Resources[i].Fingerprint = ""
					}
				}
			}
			if err := backend.RecoverCodexReview(context.Background(), cfg, launch); err != nil {
				t.Fatalf("transferred %s recovery = %v, want reaped cleanup", state, err)
			}
			if journal.intent.State != CodexReviewIntentClosed {
				t.Fatalf("intent state = %q, want closed", journal.intent.State)
			}
			if _, exists := rt.ctrs[codexReviewContainerName(launch.RunID)]; exists {
				t.Fatalf("transferred %s recovery left the credential-bearing review attached", state)
			}
			if !leaser.released || leaser.transferred || leaser.held {
				t.Fatalf("transferred %s recovery did not return the lifecycle lease", state)
			}
			relaunched, err := backend.CodexReview(context.Background(), cfg, launch)
			if err != nil {
				t.Fatalf("relaunch after transferred %s recovery = %v, want success", state, err)
			}
			_ = relaunched.Close()
		})
	}
}

func TestRecoverCodexReviewRejectsForgedTransferredLease(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*fakeCodexReviewVolumeLeaser, *fakeRuntime, CodexReviewLaunchSpec)
	}{
		{"foreign holder", func(l *fakeCodexReviewVolumeLeaser, _ *fakeRuntime, _ CodexReviewLaunchSpec) {
			l.holder = strings.Repeat("1", 32)
		}},
		{"foreign volume set", func(l *fakeCodexReviewVolumeLeaser, _ *fakeRuntime, _ CodexReviewLaunchSpec) {
			l.volumes = []string{"foreign-volume"}
		}},
		{"foreign target", func(l *fakeCodexReviewVolumeLeaser, rt *fakeRuntime, _ CodexReviewLaunchSpec) {
			l.container = "foreign-review"
			rt.ctrs["foreign-review"] = &fakeCtr{
				spec: ContainerSpec{Name: "foreign-review"}, started: true, created: "foreign-created",
			}
		}},
	} {
		for _, state := range []CodexReviewIntentState{
			CodexReviewIntentPreparing, CodexReviewIntentPrepared, CodexReviewIntentStarting,
		} {
			t.Run(tc.name+"/"+string(state), func(t *testing.T) {
				backend, rt, cfg, launch, journal, leaser := crashedTransferredCodexReview(t)
				journal.intent.State = state
				leaser.mu.Lock()
				tc.mutate(leaser, rt, launch)
				leaser.mu.Unlock()
				if err := backend.RecoverCodexReview(context.Background(), cfg, launch); err == nil {
					t.Fatal("recovery accepted forged transferred lease evidence")
				}
				if journal.intent.State != state {
					t.Fatalf("intent state = %q, want %q", journal.intent.State, state)
				}
				if _, exists := rt.ctrs[codexReviewContainerName(launch.RunID)]; !exists {
					t.Fatal("forged transfer recovery deleted a same-name review container")
				}
			})
		}
	}
}

func TestBackendCodexReviewReleasesLifecycleLeaseOnPreStartRefusal(t *testing.T) {
	backend, _, cfg, launchSpec, journal := testCodexReviewLifecycle(t)
	leaser := cfg.VolumeLifecycleLeaser.(*fakeCodexReviewVolumeLeaser)
	journal.mutate = func(binding *CodexReviewJournalBinding) {
		binding.WorkspaceFingerprint = "forged-runtime-fingerprint"
	}
	if launch, err := backend.CodexReview(context.Background(), cfg, launchSpec); err == nil {
		_ = launch.Close()
		t.Fatal("CodexReview started from a forged persisted fingerprint")
	}
	if !leaser.released || leaser.held || leaser.transferred {
		t.Fatal("pre-start refusal did not release the lifecycle lease")
	}
}

func TestBackendCodexReviewRechecksTokenLifetimeImmediatelyBeforeStart(t *testing.T) {
	backend, rt, cfg, launchSpec, journal := testCodexReviewLifecycle(t)
	now := codexReviewEpoch
	cfg.Now = func() time.Time { return now }
	afterReload := false
	journal.onGet = func() { afterReload = true }
	exclusivityPasses := 0
	rt.onListContainers = func(list []ContainerSummary) ([]ContainerSummary, error) {
		if afterReload && slices.ContainsFunc(list, func(container ContainerSummary) bool {
			return container.ID == codexReviewContainerName(launchSpec.RunID)
		}) {
			exclusivityPasses++
			if exclusivityPasses == 2 {
				now = codexReviewEpoch.Add(61 * time.Minute)
			}
		}
		return list, nil
	}
	started := false
	rt.onStart = func(id string) error {
		if id == codexReviewContainerName(launchSpec.RunID) {
			started = true
		}
		return nil
	}
	if launch, err := backend.CodexReview(context.Background(), cfg, launchSpec); err == nil {
		_ = launch.Close()
		t.Fatal("CodexReview started after preparation consumed the token lifetime floor")
	}
	if started {
		t.Fatal("review container started before the current-clock lifetime refusal")
	}
	if exclusivityPasses < 2 {
		t.Fatalf("pre-start review-container attachment checks = %d, want at least 2", exclusivityPasses)
	}
}

func TestBackendCodexReviewRejectsCallerInputBeforeRuntime(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *CodexReviewConfig, *CodexReviewLaunchSpec)
	}{
		{
			name: "empty prompt",
			mutate: func(_ *testing.T, _ *CodexReviewConfig, launch *CodexReviewLaunchSpec) {
				launch.Prompt = ""
			},
		},
		{
			name: "oversized prompt",
			mutate: func(_ *testing.T, _ *CodexReviewConfig, launch *CodexReviewLaunchSpec) {
				launch.Prompt = strings.Repeat("p", maxCodexReviewPromptBytes+1)
			},
		},
		{
			name: "caller image mismatches deployment pin",
			mutate: func(_ *testing.T, _ *CodexReviewConfig, launch *CodexReviewLaunchSpec) {
				launch.Image = "example.test/untrusted-codex@sha256:" + strings.Repeat("b", 64)
			},
		},
		{
			name: "invalid instructions",
			mutate: func(_ *testing.T, _ *CodexReviewConfig, launch *CodexReviewLaunchSpec) {
				launch.Instructions.Body = nil
			},
		},
		{
			name: "auth outside input root",
			mutate: func(t *testing.T, cfg *CodexReviewConfig, launch *CodexReviewLaunchSpec) {
				launch.AuthSnapshot = writeCodexReviewFile(t, t.TempDir(), "auth.json", []byte("{}"))
				if filepath.Dir(launch.AuthSnapshot) == cfg.InputRoot {
					t.Fatal("outside-root fixture unexpectedly reused the input root")
				}
			},
		},
		{
			name: "malformed auth",
			mutate: func(t *testing.T, _ *CodexReviewConfig, launch *CodexReviewLaunchSpec) {
				if err := os.WriteFile(launch.AuthSnapshot, []byte("{"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			backend, rt, cfg, launchSpec, _ := testCodexReviewLifecycle(t)
			tc.mutate(t, &cfg, &launchSpec)
			if launch, err := backend.CodexReview(context.Background(), cfg, launchSpec); !errors.Is(err, ErrInvalidCodexReviewSpec) {
				if launch != nil {
					_ = launch.Close()
				}
				t.Fatalf("CodexReview = %v, %v; want typed refusal", launch, err)
			}
			rt.mu.Lock()
			defer rt.mu.Unlock()
			if len(rt.calls) != 0 {
				t.Fatalf("invalid launch reached runtime: %v", rt.calls)
			}
		})
	}
}

func TestBackendCodexReviewRefusesForgedPersistedFingerprint(t *testing.T) {
	backend, _, cfg, launchSpec, journal := testCodexReviewLifecycle(t)
	journal.mutate = func(binding *CodexReviewJournalBinding) {
		binding.WorkspaceFingerprint = "forged-runtime-fingerprint"
	}
	if launch, err := backend.CodexReview(context.Background(), cfg, launchSpec); err == nil {
		_ = launch.Close()
		t.Fatal("CodexReview started from a forged persisted fingerprint")
	}
}

func TestBackendCodexReviewRefusesSameNameReplacementBeforeStart(t *testing.T) {
	backend, rt, cfg, launchSpec, journal := testCodexReviewLifecycle(t)
	reviewName := codexReviewContainerName(launchSpec.RunID)
	journal.onGet = func() {
		rt.mu.Lock()
		defer rt.mu.Unlock()
		original := rt.ctrs[reviewName]
		rt.ctrs[reviewName] = &fakeCtr{
			spec: ContainerSpec{
				Name: reviewName, Image: original.spec.Image,
				Labels: runLabels(launchSpec.RunID),
			},
			created: "replacement-created",
		}
	}
	if launch, err := backend.CodexReview(context.Background(), cfg, launchSpec); err == nil {
		_ = launch.Close()
		t.Fatal("CodexReview started a same-name replacement container")
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.ctrs[reviewName] == nil || rt.ctrs[reviewName].started {
		t.Fatal("replacement container was deleted or started")
	}
}

func TestBuildCodexReviewAgentSpecConforms(t *testing.T) {
	cfg, req := testCodexReview(t)
	spec, binding, err := BuildCodexReviewAgentSpec(cfg, req)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateCodexReviewAgentSpec(cfg, req, spec, binding); err != nil {
		t.Fatalf("generated topology fails its conformance check: %v", err)
	}
	if err := binding.validateShape(); err == nil {
		t.Fatal("prepared journal binding passed reconstruction before the pre-start observation")
	}
	finalBinding := binding
	finalBinding.AgentsShadowPreStartObserverFingerprint = "2026-08-03T12:00:02Z"
	finalBinding.WorkspacePreStartObserverFingerprint = "2026-08-03T12:00:04Z"
	finalBinding.SnapshotPreStartObserverFingerprint = "2026-08-03T12:00:06Z"
	finalBinding.ReviewContainer = codexReviewContainerName(req.RunID)
	finalBinding.ReviewContainerFingerprint = "2026-08-03T12:00:05Z"
	finalBinding.ReviewOwnershipToken = testOwnershipLabel().Value
	if err := finalBinding.validateShape(); err != nil {
		t.Fatalf("complete journal binding fails reconstruction validation: %v", err)
	}
	wrongComposition := finalBinding
	wrongComposition.InstructionCompositionVersion = "codex_explicit_bundle_v0"
	if err := wrongComposition.validateShape(); err == nil {
		t.Error("journal binding accepted a different instruction composition version")
	}
	wrongSources := binding
	digest := domain.Digest("sha256:" + strings.Repeat("f", 64))
	wrongSources.HostInstructionDigest = &digest
	if err := validateCodexReviewAgentSpec(cfg, req, spec, wrongSources); err == nil {
		t.Error("journal binding accepted instruction sources unrelated to the request")
	}
	reusedBinding := finalBinding
	reusedBinding.AgentsShadowPreStartObserverFingerprint = reusedBinding.AgentsShadowObserverFingerprint
	if err := reusedBinding.validateShape(); err == nil {
		t.Error("journal binding accepted a reused observer fingerprint")
	}
	widenedNetwork := finalBinding
	widenedNetwork.ProviderNetworkHostOnly = false
	if err := widenedNetwork.validateShape(); err == nil {
		t.Error("journal binding accepted a non-host-only provider network")
	}
	reboundNetwork := finalBinding
	reboundNetwork.ProviderNetworkGateway = "127.0.0.2"
	reboundNetwork.ProviderProxyAuthority = "127.0.0.2:43123"
	if err := reboundNetwork.validateShape(); err == nil {
		t.Error("journal binding accepted a proxy rebound away from the attested gateway")
	}
	for _, target := range []string{
		"/var/lib/freeside",
		"/review/.agents/skills/canary",
		"/workspace/project,readonly=false",
	} {
		poisonedBinding := finalBinding
		poisonedBinding.WorkspaceTarget = target
		poisonedBinding.AgentsShadowTargets = codexAgentsShadowTargets(target, codexWorkspaceAgentsDir)
		if err := poisonedBinding.validateShape(); err == nil {
			t.Errorf("journal binding accepted unsafe workspace target %q", target)
		}
	}
	if !slices.Equal(binding.ProviderEndpoints, []string{"chatgpt.com:443"}) ||
		binding.RefreshEndpointReachable || binding.PublicationCredentials {
		t.Errorf("egress/credential binding = %+v", binding)
	}
	if binding.AccessTokenExpiresAt == nil || !binding.AccessTokenExpiresAt.Equal(codexReviewEpoch.Add(2*time.Hour)) {
		t.Errorf("access-token expiry = %v", binding.AccessTokenExpiresAt)
	}
	if spec.Mounts[0].Target != cfg.WorkspaceTarget || !spec.Mounts[0].ReadOnly {
		t.Errorf("workspace mount = %+v, want read-only candidate", spec.Mounts[0])
	}
	if got, want := binding.AgentsShadowTargets, []string{
		"/.agents", "/var/lib/freeside/home/.agents", "/workspace/.agents", "/workspace/project/.agents",
	}; !reflect.DeepEqual(got, want) {
		t.Errorf("shadow targets = %q, want %q", got, want)
	}
	for _, forbidden := range []string{"OPENAI_API_KEY=", "GITHUB_TOKEN=", "GH_TOKEN="} {
		if strings.Contains(strings.Join(spec.Env, "\n"), forbidden) {
			t.Errorf("minimal launcher environment contains %q: %q", forbidden, spec.Env)
		}
	}
	joinedCommand := strings.Join(spec.Command, " ")
	for _, required := range []string{
		"--ephemeral", "-s read-only", "project_doc_max_bytes=0", "--ignore-user-config", "--ignore-rules",
	} {
		if !strings.Contains(joinedCommand, required) {
			t.Errorf("command misses fixed argument %q: %q", required, spec.Command)
		}
	}
	goldenSpec := cloneContainerSpec(spec)
	got, err := json.MarshalIndent(struct {
		Spec    ContainerSpec             `json:"spec"`
		Binding CodexReviewJournalBinding `json:"binding"`
	}{goldenSpec, binding}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden.Assert(t, "codex-review-spec", append(got, '\n'))
}

// TestCodexAgentsShadowTargetsFollowObservedWorkspaceEntry pins the
// conditional topology: the workspace-local shadow exists exactly when the
// observed tree carries a .agents directory, while the ambient shadows (the
// reviewer's home and every workspace ancestor) never depend on it. Apple
// container cannot create a missing mountpoint under the read-only workspace,
// so mounting the shadow over an absent .agents is what broke production.
func TestCodexAgentsShadowTargetsFollowObservedWorkspaceEntry(t *testing.T) {
	withDir := codexAgentsShadowTargets("/workspace/project", codexWorkspaceAgentsDir)
	withoutDir := codexAgentsShadowTargets("/workspace/project", codexWorkspaceAgentsAbsent)
	const workspaceLocal = "/workspace/project/.agents"
	if !slices.Contains(withDir, workspaceLocal) {
		t.Errorf("dir targets %q omit the workspace-local shadow", withDir)
	}
	if slices.Contains(withoutDir, workspaceLocal) {
		t.Errorf("absent targets %q still shadow the missing workspace mountpoint", withoutDir)
	}
	ambient := slices.DeleteFunc(slices.Clone(withDir), func(target string) bool {
		return target == workspaceLocal
	})
	if !slices.Equal(ambient, withoutDir) {
		t.Errorf("ambient shadows diverge: dir-derived %q, absent %q", ambient, withoutDir)
	}
	for _, want := range []string{
		"/.agents", "/workspace/.agents", path.Join(CodexContainerHomeTarget, ".agents"),
	} {
		if !slices.Contains(withoutDir, want) {
			t.Errorf("absent targets %q omit ambient shadow %q", withoutDir, want)
		}
	}
}

// TestCodexWorkspaceAgentsProbeScriptClassifiesEntries executes the actual
// probe shell text against real filesystem shapes, so a quoting or
// classification bug in the script surfaces here instead of only in the
// opt-in live suite. The fake runtime synthesizes this line in Go; this test
// is what ties that synthesis to the script's real behaviour.
func TestCodexWorkspaceAgentsProbeScriptClassifiesEntries(t *testing.T) {
	shPath, err := osexec.LookPath("sh")
	if err != nil {
		t.Skipf("sh not available: %v", err)
	}
	cases := []struct {
		name  string
		setup func(t *testing.T, workspace string)
		want  string
	}{
		{"directory", func(t *testing.T, workspace string) {
			t.Helper()
			if err := os.Mkdir(filepath.Join(workspace, ".agents"), 0o700); err != nil {
				t.Fatal(err)
			}
		}, codexWorkspaceAgentsDir},
		{"absent", func(*testing.T, string) {}, codexWorkspaceAgentsAbsent},
		{"regular file", func(t *testing.T, workspace string) {
			t.Helper()
			if err := os.WriteFile(filepath.Join(workspace, ".agents"), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, "other"},
		{"symlink to directory", func(t *testing.T, workspace string) {
			t.Helper()
			if err := os.Symlink(workspace, filepath.Join(workspace, ".agents")); err != nil {
				t.Fatal(err)
			}
		}, "other"},
		{"dangling symlink", func(t *testing.T, workspace string) {
			t.Helper()
			if err := os.Symlink("no-such-target", filepath.Join(workspace, ".agents")); err != nil {
				t.Fatal(err)
			}
		}, "other"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			workspace := t.TempDir()
			tc.setup(t, workspace)
			proofPath := filepath.Join(t.TempDir(), "proof.txt")
			script := codexWorkspaceAgentsProbeScript(workspace, proofPath)
			if out, runErr := osexec.Command(shPath, "-c", script).CombinedOutput(); runErr != nil { //nolint:gosec // fixture paths in a fixed probe script
				t.Fatalf("probe script: %v: %s", runErr, out)
			}
			proof, readErr := os.ReadFile(proofPath) //nolint:gosec // test fixture path
			if readErr != nil {
				t.Fatal(readErr)
			}
			if want := codexWorkspaceAgentsKey + "=" + tc.want + "\n"; string(proof) != want {
				t.Errorf("probe proof = %q, want %q", proof, want)
			}
		})
	}
}

func TestCodexReviewProofWorkspaceAgentsFailsClosed(t *testing.T) {
	base := "nonce=n\ntree_sha256=" + strings.Repeat("1", 64) + "\n"
	cases := []struct {
		name  string
		proof string
		check Check
	}{
		{"omitted entry", base, CheckObservedBaseIdentity},
		{"repeated entry", base + codexWorkspaceAgentsKey + "=dir\n" + codexWorkspaceAgentsKey + "=dir\n", CheckObservedBaseIdentity},
		{"symlink or non-directory entry", base + codexWorkspaceAgentsKey + "=other\n", CheckControlPlaneIsolation},
		{"unknown entry kind", base + codexWorkspaceAgentsKey + "=file\n", CheckControlPlaneIsolation},
		{"empty entry value", base + codexWorkspaceAgentsKey + "=\n", CheckControlPlaneIsolation},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entry, err := codexReviewProofWorkspaceAgents([]byte(tc.proof))
			wantCheckFailure(t, err, tc.check)
			if entry != "" {
				t.Errorf("rejected proof still yielded entry %q", entry)
			}
		})
	}
	for _, valid := range []string{codexWorkspaceAgentsDir, codexWorkspaceAgentsAbsent} {
		entry, err := codexReviewProofWorkspaceAgents([]byte(base + codexWorkspaceAgentsKey + "=" + valid + "\n"))
		if err != nil || entry != valid {
			t.Errorf("valid entry %q = %q, %v", valid, entry, err)
		}
	}
}

// TestBuildCodexReviewAgentSpecOmitsWorkspaceShadowWhenAgentsAbsent is the
// unit regression for the production errno 30 failure: a candidate proven to
// carry no .agents directory must launch without the workspace-local shadow
// mount, with the reduced target set durably bound and self-consistent.
func TestBuildCodexReviewAgentSpecOmitsWorkspaceShadowWhenAgentsAbsent(t *testing.T) {
	cfg, req := testCodexReview(t)
	req.Workspace = testCodexReviewWorkspaceWithAgents(
		t, cfg, req.RunID, req.WorkspaceVolume, "2026-08-03T12:00:03Z", codexWorkspaceAgentsAbsent,
	)
	spec, binding, err := BuildCodexReviewAgentSpec(cfg, req)
	if err != nil {
		t.Fatalf("BuildCodexReviewAgentSpec: %v", err)
	}
	workspaceLocal := path.Join(cfg.WorkspaceTarget, ".agents")
	for _, mount := range spec.Mounts {
		if mount.Target == workspaceLocal {
			t.Errorf("spec still mounts the shadow over the absent %q", workspaceLocal)
		}
	}
	if binding.WorkspaceAgentsEntry != codexWorkspaceAgentsAbsent {
		t.Errorf("binding workspace agents entry = %q, want %q",
			binding.WorkspaceAgentsEntry, codexWorkspaceAgentsAbsent)
	}
	if want := codexAgentsShadowTargets(cfg.WorkspaceTarget, codexWorkspaceAgentsAbsent); !slices.Equal(binding.AgentsShadowTargets, want) {
		t.Errorf("binding shadow targets = %q, want %q", binding.AgentsShadowTargets, want)
	}
	if err := binding.validatePrepared(); err != nil {
		t.Errorf("prepared binding invalid: %v", err)
	}
	if err := validateCodexReviewAgentSpec(cfg, req, spec, binding); err != nil {
		t.Errorf("self-validation: %v", err)
	}
}

// TestCodexReviewBindingRejectsTopologyEntryMismatch holds the durable intent
// to the observation that justified it: a binding whose stored target set
// disagrees with its recorded workspace .agents entry, or whose entry
// disagrees with the live observation, must fail closed, so reconstruction
// can never choose a different topology than the one bound at launch.
func TestCodexReviewBindingRejectsTopologyEntryMismatch(t *testing.T) {
	cfg, req := testCodexReview(t)
	_, binding, err := BuildCodexReviewAgentSpec(cfg, req)
	if err != nil {
		t.Fatal(err)
	}
	flipped := binding
	flipped.WorkspaceAgentsEntry = codexWorkspaceAgentsAbsent
	if err := flipped.validatePrepared(); err == nil {
		t.Error("binding accepted an entry that disagrees with its stored target set")
	}
	reduced := binding
	reduced.AgentsShadowTargets = codexAgentsShadowTargets(cfg.WorkspaceTarget, codexWorkspaceAgentsAbsent)
	if err := reduced.validatePrepared(); err == nil {
		t.Error("binding accepted a target set that disagrees with its recorded entry")
	}
	empty := binding
	empty.WorkspaceAgentsEntry = ""
	if err := empty.validatePrepared(); err == nil {
		t.Error("binding accepted an unrecorded workspace .agents entry")
	}

	consistent := binding
	consistent.WorkspaceAgentsEntry = codexWorkspaceAgentsAbsent
	consistent.AgentsShadowTargets = codexAgentsShadowTargets(cfg.WorkspaceTarget, codexWorkspaceAgentsAbsent)
	spec, err := func() (ContainerSpec, error) {
		s, _, buildErr := BuildCodexReviewAgentSpec(cfg, req)
		return s, buildErr
	}()
	if err != nil {
		t.Fatal(err)
	}
	if err := validateCodexReviewAgentSpec(cfg, req, spec, consistent); err == nil {
		t.Error("spec validation accepted a binding whose entry disagrees with the observation")
	}
}

// TestCodexReviewLegacyV2BindingAuthenticatesTeardownOnly pins the upgrade
// posture: a v2 binding without the observed .agents entry may still
// authenticate cleanup of a pre-upgrade review, but can never validate for
// launch or result collection under the v3 contract.
func TestCodexReviewLegacyV2BindingAuthenticatesTeardownOnly(t *testing.T) {
	cfg, req := testCodexReview(t)
	_, binding, err := BuildCodexReviewAgentSpec(cfg, req)
	if err != nil {
		t.Fatal(err)
	}
	binding.ReviewContainer = codexReviewContainerName(binding.RunID)
	binding.ReviewContainerFingerprint = "fingerprint"
	binding.ReviewOwnershipToken = strings.Repeat("b", 32)
	binding.WorkspacePreStartObserverFingerprint = "pre-start-workspace"
	binding.SnapshotPreStartObserverFingerprint = "pre-start-snapshot"
	binding.AgentsShadowPreStartObserverFingerprint = "pre-start-shadow"
	if err := binding.validateShape(); err != nil {
		t.Fatalf("v3 binding shape: %v", err)
	}

	legacy := binding
	legacy.TopologyVersion = codexReviewTopologyVersionV2
	legacy.WorkspaceAgentsEntry = ""
	if err := legacy.validateForTeardown(); err != nil {
		t.Errorf("legacy v2 binding failed teardown authentication: %v", err)
	}
	if err := legacy.validateShape(); err == nil {
		t.Error("legacy v2 binding validated outside teardown")
	}
	downgraded := binding
	downgraded.TopologyVersion = codexReviewTopologyVersionV2
	if err := downgraded.validateShape(); err == nil {
		t.Error("v2 topology version validated outside teardown")
	}
	unversioned := binding
	unversioned.WorkspaceAgentsEntry = ""
	if err := unversioned.validateForTeardown(); err == nil {
		t.Error("v3 binding without a recorded entry authenticated teardown")
	}
}

func TestCodexReviewRefusesResumeAndContinuity(t *testing.T) {
	for _, boundary := range []CodexReviewBoundary{CodexReviewResume, CodexReviewContinuity} {
		t.Run(string(boundary), func(t *testing.T) {
			cfg, req := testCodexReview(t)
			req.Boundary = boundary
			_, _, err := BuildCodexReviewAgentSpec(cfg, req)
			if !errors.Is(err, ErrInvalidCodexReviewSpec) || !errors.Is(err, ErrCodexReviewContinuityRefused) {
				t.Fatalf("Build = %v, want typed continuity refusal", err)
			}
		})
	}
}

func TestCodexReviewAuthSnapshotFailsClosed(t *testing.T) {
	tests := []struct {
		name string
		auth func(*testing.T) []byte
	}{
		{
			name: "token below lifetime floor",
			auth: func(t *testing.T) []byte {
				return []byte(fmt.Sprintf(`{"OPENAI_API_KEY":null,"tokens":{"id_token":"id","access_token":%q,"refresh_token":""}}`,
					codexReviewJWT(t, codexReviewEpoch.Add(59*time.Minute))))
			},
		},
		{
			name: "mixed API key",
			auth: func(t *testing.T) []byte {
				return []byte(fmt.Sprintf(`{"OPENAI_API_KEY":"not-a-real-key","tokens":{"id_token":"id","access_token":%q,"refresh_token":""}}`,
					codexReviewJWT(t, codexReviewEpoch.Add(2*time.Hour))))
			},
		},
		{
			name: "refresh token aliased into id token",
			auth: func(t *testing.T) []byte {
				return []byte(fmt.Sprintf(`{"OPENAI_API_KEY":null,"tokens":{"id_token":"prefix-family-revoking","access_token":%q,"refresh_token":"family-revoking"}}`,
					codexReviewJWT(t, codexReviewEpoch.Add(2*time.Hour))))
			},
		},
		{
			name: "refresh token field omitted",
			auth: func(t *testing.T) []byte {
				return []byte(fmt.Sprintf(`{"OPENAI_API_KEY":null,"tokens":{"id_token":"id","access_token":%q}}`,
					codexReviewJWT(t, codexReviewEpoch.Add(2*time.Hour))))
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, req := testCodexReview(t)
			req.AuthSnapshot = writeCodexReviewFile(t, cfg.InputRoot, "bad-auth.json", tc.auth(t))
			if _, _, err := BuildCodexReviewAgentSpec(cfg, req); !errors.Is(err, ErrInvalidCodexReviewSpec) {
				t.Fatalf("Build = %v, want typed refusal", err)
			}
		})
	}
}

func TestCodexReviewDerivesAccessOnlySnapshotFromHostStore(t *testing.T) {
	cfg, req := testCodexReview(t)
	hostBody := []byte(fmt.Sprintf(
		`{"OPENAI_API_KEY":null,"tokens":{"id_token":"id","access_token":%q,"refresh_token":"family-revoking"}}`,
		codexReviewJWT(t, codexReviewEpoch.Add(2*time.Hour)),
	))
	req.AuthSnapshot = writeCodexReviewFile(t, cfg.InputRoot, "host-auth.json", hostBody)
	snapshotBody, _, err := codexReviewAgentAuthSnapshot(req.AuthMode, hostBody)
	if err != nil {
		t.Fatal(err)
	}
	req.Snapshot.authDigest = string(digestBody(snapshotBody))

	_, binding, err := BuildCodexReviewAgentSpec(cfg, req)
	if err != nil {
		t.Fatalf("Build = %v, want access-only snapshot", err)
	}
	if binding.AuthSnapshotDigest != string(digestBody(snapshotBody)) {
		t.Fatalf("binding auth digest = %q, want sanitized digest", binding.AuthSnapshotDigest)
	}
	if bytes.Contains(snapshotBody, []byte("family-revoking")) {
		t.Fatal("derived agent snapshot carries the host refresh token")
	}
	if _, err := inspectCodexAuthSnapshot(req.AuthMode, snapshotBody); err != nil {
		t.Fatalf("derived agent snapshot = %v", err)
	}
	stored, err := os.ReadFile(req.AuthSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, hostBody) {
		t.Fatal("deriving the agent snapshot mutated the host store")
	}
}

func TestCodexReviewProviderOnlyIsExact(t *testing.T) {
	for _, endpoints := range [][]string{
		{"chatgpt.com:443", "auth.openai.com:443"},
		{"chatgpt.com:443", "api.github.com:443"},
		{"api.openai.com:443"},
		nil,
	} {
		cfg, req := testCodexReview(t)
		cfg.ProviderEndpoints = endpoints
		if _, _, err := BuildCodexReviewAgentSpec(cfg, req); !errors.Is(err, ErrInvalidCodexReviewSpec) {
			t.Errorf("endpoints %q accepted: %v", endpoints, err)
		}
	}
}

func TestCodexReviewRejectsCredentialBearingProxyURL(t *testing.T) {
	cfg, req := testCodexReview(t)
	cfg.ProxyURL = "http://user:secret@127.0.0.1:43123"
	if _, _, err := BuildCodexReviewAgentSpec(cfg, req); !errors.Is(err, ErrInvalidCodexReviewSpec) {
		t.Fatalf("credential-bearing proxy URL accepted: %v", err)
	}
}

func TestCodexReviewPromptBound(t *testing.T) {
	cfg, req := testCodexReview(t)
	req.Prompt = strings.Repeat("p", maxCodexReviewPromptBytes)
	if _, _, err := BuildCodexReviewAgentSpec(cfg, req); err != nil {
		t.Fatalf("maximum prompt rejected: %v", err)
	}
	req.Prompt += "p"
	if _, _, err := BuildCodexReviewAgentSpec(cfg, req); !errors.Is(err, ErrInvalidCodexReviewSpec) {
		t.Fatalf("oversized prompt accepted: %v", err)
	}
}

func TestCodexReviewRejectsWorkspaceControlPathOverlap(t *testing.T) {
	for _, target := range []string{
		"/var/lib/freeside",
		CodexHomeTarget,
		CodexAuthFileTarget,
		CodexContainerHomeTarget + "/nested",
		"/.agents",
		"/review/.agents/skills/canary",
		"/workspace/project,readonly=false",
		"/workspace/project\nother",
		"/usr",
		"/usr/local",
		"/bin/review",
		"/lib64",
		"/etc/ssl",
		"/proc/review",
		"/run",
	} {
		cfg, req := testCodexReview(t)
		cfg.WorkspaceTarget = target
		if _, _, err := BuildCodexReviewAgentSpec(cfg, req); !errors.Is(err, ErrInvalidCodexReviewSpec) {
			t.Errorf("workspace target %q accepted: %v", target, err)
		}
	}
}

func TestCodexReviewInputUIDMustMatchDaemon(t *testing.T) {
	stat := &syscall.Stat_t{Uid: 42}
	if !codexReviewUIDMatches(stat, 42) {
		t.Fatal("matching daemon UID was rejected")
	}
	for _, euid := range []int{41, 43, -1} {
		if codexReviewUIDMatches(stat, euid) {
			t.Errorf("foreign daemon UID %d was accepted", euid)
		}
	}
	if codexReviewUIDMatches(nil, 42) {
		t.Fatal("missing ownership metadata was accepted")
	}
}

func TestObserveCodexReviewShadowFailsClosed(t *testing.T) {
	cfg, req := testCodexReview(t)
	owner := testOwnershipLabel()
	spec, err := BuildCodexReviewShadowObserverSpec(
		cfg, req.RunID, req.AgentsShadow.volume, owner,
	)
	if err != nil {
		t.Fatal(err)
	}
	volume := VolumeSummary{
		Name: req.AgentsShadow.volume, Labels: slices.Clone(spec.Labels), LabelsObserved: true,
		CreationDate: req.AgentsShadow.fingerprint,
	}
	report := InspectReport{
		ID: spec.Name, ImageReference: spec.Image, Command: slices.Clone(spec.Command),
		WorkingDirectory: "/", State: StateStopped, AllowlistFieldsObserved: true,
		Mounts: slices.Clone(spec.Mounts), Env: []string{fixedContainerPathEnv},
		NetworksObserved: true, Labels: slices.Clone(spec.Labels), LabelsObserved: true,
		CreationDate: "2026-08-03T12:00:01Z",
	}
	proof := []byte(fmt.Sprintf(
		"nonce=%s\nempty=yes\ntree=%s\n", owner.Value, emptyCodexShadowDigest,
	))

	foreign := volume
	foreign.Labels = runLabels(req.RunID)
	if _, err := ObserveCodexReviewShadow(
		cfg, req.RunID, req.AgentsShadow.volume, owner, owner, foreign, report, proof,
	); !errors.Is(err, ErrConformance) {
		t.Errorf("foreign volume = %v, want conformance failure", err)
	}

	nonempty := []byte(fmt.Sprintf(
		"nonce=%s\nempty=no\ntree=%s\n", owner.Value, emptyCodexShadowDigest,
	))
	if _, err := ObserveCodexReviewShadow(
		cfg, req.RunID, req.AgentsShadow.volume, owner, owner, volume, report, nonempty,
	); !errors.Is(err, ErrConformance) {
		t.Errorf("nonempty volume = %v, want conformance failure", err)
	}

	writable := report
	writable.Mounts = slices.Clone(report.Mounts)
	writable.Mounts[0].ReadOnly = false
	if _, err := ObserveCodexReviewShadow(
		cfg, req.RunID, req.AgentsShadow.volume, owner, owner, volume, writable, proof,
	); !errors.Is(err, ErrConformance) {
		t.Errorf("writable observer = %v, want conformance failure", err)
	}

	req.AgentsShadow = CodexReviewShadowObservation{}
	if _, _, err := BuildCodexReviewAgentSpec(cfg, req); !errors.Is(err, ErrInvalidCodexReviewSpec) {
		t.Errorf("unobserved shadow = %v, want typed refusal", err)
	}
}

func TestObserveCodexReviewNetworkFailsClosed(t *testing.T) {
	cfg, req := testCodexReview(t)
	owner := testOwnershipLabel()
	base := NetworkReport{
		NetworkSummary: NetworkSummary{
			Name: codexReviewNetworkName(req.RunID), Mode: NetworkHostOnly,
			Labels: []Label{owner}, LabelsObserved: true, CreationDate: "2026-08-03T11:59:58Z",
		},
		IPv4Gateway: "127.0.0.1", IPv4Subnet: "127.0.0.0/24",
	}
	tests := []struct {
		name   string
		mutate func(*NetworkReport)
	}{
		{"NAT mode", func(report *NetworkReport) { report.Mode = NetworkNAT }},
		{"wrong gateway", func(report *NetworkReport) { report.IPv4Gateway = "127.0.0.2" }},
		{"foreign ownership", func(report *NetworkReport) { report.Labels = runLabels(req.RunID) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			report := base
			report.Labels = slices.Clone(base.Labels)
			tc.mutate(&report)
			_, err := ObserveCodexReviewNetwork(cfg, req.RunID, owner, report)
			if !errors.Is(err, ErrConformance) {
				t.Fatalf("Observe = %v, want conformance failure", err)
			}
		})
	}
	invalidOwner := Label{Key: "freeside.handoff", Value: req.RunID}
	invalidOwnershipReport := base
	invalidOwnershipReport.Labels = []Label{invalidOwner}
	if _, err := ObserveCodexReviewNetwork(
		cfg, req.RunID, invalidOwner, invalidOwnershipReport,
	); !errors.Is(err, ErrConformance) {
		t.Fatalf("predictable ownership claim = %v, want conformance failure", err)
	}
}

func TestCodexReviewShadowObserverScriptProvesEmptiness(t *testing.T) {
	root := t.TempDir()
	proofPath := filepath.Join(t.TempDir(), "proof")
	run := func() []byte {
		t.Helper()
		cmd := osexec.Command("sh", "-c", codexReviewShadowObserverScript( //nolint:gosec // test-owned paths exercise the fixed script
			testOwnershipLabel().Value, root, proofPath,
		))
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("observer script: %v: %s", err, output)
		}
		proof, err := os.ReadFile(proofPath) //nolint:gosec // test-owned path
		if err != nil {
			t.Fatal(err)
		}
		return proof
	}
	wantEmpty := fmt.Sprintf(
		"nonce=%s\nempty=yes\ntree=%s\n", testOwnershipLabel().Value, emptyCodexShadowDigest,
	)
	if got := string(run()); got != wantEmpty {
		t.Fatalf("empty proof = %q, want %q", got, wantEmpty)
	}
	if err := os.WriteFile(filepath.Join(root, "skill"), []byte("poison"), 0o600); err != nil {
		t.Fatal(err)
	}
	wantNonempty := fmt.Sprintf(
		"nonce=%s\nempty=no\ntree=%s\n", testOwnershipLabel().Value, emptyCodexShadowDigest,
	)
	if got := string(run()); got != wantNonempty {
		t.Fatalf("nonempty proof = %q, want %q", got, wantNonempty)
	}
}

func TestCodexReviewSingleFileBindsStayUnderTrustedRoot(t *testing.T) {
	cfg, req := testCodexReview(t)
	outside := writeCodexReviewFile(t, t.TempDir(), "outside-auth.json", []byte(`{}`))
	req.AuthSnapshot = outside
	if _, _, err := BuildCodexReviewAgentSpec(cfg, req); !errors.Is(err, ErrInvalidCodexReviewSpec) {
		t.Fatalf("outside auth snapshot = %v, want typed refusal", err)
	}

	cfg, req = testCodexReview(t)
	link := filepath.Join(cfg.InputRoot, "linked-auth.json")
	if err := os.Symlink(req.AuthSnapshot, link); err != nil {
		t.Fatal(err)
	}
	req.AuthSnapshot = link
	if _, _, err := BuildCodexReviewAgentSpec(cfg, req); !errors.Is(err, ErrInvalidCodexReviewSpec) {
		t.Fatalf("symlink auth snapshot = %v, want typed refusal", err)
	}

	cfg, req = testCodexReview(t)
	linked := filepath.Join(cfg.InputRoot, "hardlinked-auth.json")
	if err := os.Link(req.AuthSnapshot, linked); err != nil {
		t.Fatal(err)
	}
	req.AuthSnapshot = linked
	if _, _, err := BuildCodexReviewAgentSpec(cfg, req); !errors.Is(err, ErrInvalidCodexReviewSpec) {
		t.Fatalf("hard-linked auth snapshot = %v, want typed refusal", err)
	}

	cfg, req = testCodexReview(t)
	if err := os.Chmod(req.AuthSnapshot, 0o644); err != nil { //nolint:gosec // adversarial shared-file fixture
		t.Fatal(err)
	}
	if _, _, err := BuildCodexReviewAgentSpec(cfg, req); !errors.Is(err, ErrInvalidCodexReviewSpec) {
		t.Fatalf("shared auth snapshot = %v, want typed refusal", err)
	}

	cfg, req = testCodexReview(t)
	if err := os.Chmod(cfg.InputRoot, 0o755); err != nil { //nolint:gosec // adversarial shared-directory fixture
		t.Fatal(err)
	}
	if _, _, err := BuildCodexReviewAgentSpec(cfg, req); !errors.Is(err, ErrInvalidCodexReviewSpec) {
		t.Fatalf("shared input root = %v, want typed refusal", err)
	}

	cfg, req = testCodexReview(t)
	req.InstructionFile = writeCodexReviewFile(
		t, cfg.InputRoot, "wrong-AGENTS.md", []byte("candidate-controlled replacement\n"),
	)
	if _, _, err := BuildCodexReviewAgentSpec(cfg, req); !errors.Is(err, ErrInvalidCodexReviewSpec) {
		t.Fatalf("instruction digest mismatch = %v, want typed refusal", err)
	}
}

func TestCodexReviewAPIKeyStaysOutOfEnvironment(t *testing.T) {
	cfg, req := testCodexReview(t)
	req.AuthMode = CodexAuthAPIKey
	cfg.ProviderEndpoints = []string{"api.openai.com:443"}
	apiAuth := []byte(`{"auth_mode":"api_key","OPENAI_API_KEY":"not-a-real-key","tokens":null}`)
	req.AuthSnapshot = writeCodexReviewFile(t, cfg.InputRoot, "api-auth.json", apiAuth)
	req.Snapshot = testCodexReviewSnapshot(
		t, cfg, req.RunID, req.Snapshot.volume, apiAuth, req.Instructions.Body, "2026-08-03T12:00:02Z",
	)
	spec, binding, err := BuildCodexReviewAgentSpec(cfg, req)
	if err != nil {
		t.Fatal(err)
	}
	if binding.AccessTokenExpiresAt != nil {
		t.Errorf("API key topology carries an access-token expiry: %v", binding.AccessTokenExpiresAt)
	}
	if strings.Contains(strings.Join(spec.Env, "\n"), "OPENAI_API_KEY") {
		t.Errorf("API key leaked into child-inherited environment: %q", spec.Env)
	}
}

func TestCodexReviewConformanceRejectsTopologyDrift(t *testing.T) {
	cfg, req := testCodexReview(t)
	base, binding, err := BuildCodexReviewAgentSpec(cfg, req)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(*ContainerSpec, *CodexReviewJournalBinding)
	}{
		{"writable workspace", func(s *ContainerSpec, _ *CodexReviewJournalBinding) { s.Mounts[0].ReadOnly = false }},
		{"writable snapshot", func(s *ContainerSpec, _ *CodexReviewJournalBinding) { s.Mounts[1].ReadOnly = false }},
		{"missing ancestor shadow", func(s *ContainerSpec, _ *CodexReviewJournalBinding) { s.Mounts = s.Mounts[:len(s.Mounts)-1] }},
		{"extra child environment", func(s *ContainerSpec, _ *CodexReviewJournalBinding) { s.Env = append(s.Env, "OPENAI_API_KEY=secret") }},
		{"severance removed", func(s *ContainerSpec, _ *CodexReviewJournalBinding) {
			s.Command[2] = strings.Replace(s.Command[2], " --ignore-user-config --ignore-rules", "", 1)
		}},
		{"continuity journalled", func(_ *ContainerSpec, b *CodexReviewJournalBinding) { b.ContinuityMounted = true }},
		{"lease rule dropped", func(_ *ContainerSpec, b *CodexReviewJournalBinding) { b.AuthStoreMutationLeaseRequired = false }},
		{"shadow digest changed", func(_ *ContainerSpec, b *CodexReviewJournalBinding) { b.AgentsShadowDigest = strings.Repeat("d", 64) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := cloneContainerSpec(base)
			gotBinding := binding
			gotBinding.AgentsShadowTargets = slices.Clone(binding.AgentsShadowTargets)
			tc.mutate(&spec, &gotBinding)
			if err := validateCodexReviewAgentSpec(cfg, req, spec, gotBinding); !errors.Is(err, ErrConformance) {
				t.Fatalf("validate = %v, want conformance failure", err)
			}
		})
	}
}

func TestCodexReviewBindingUsesCanonicalEnvironmentDigest(t *testing.T) {
	cfg, req := testCodexReview(t)
	spec, binding, err := BuildCodexReviewAgentSpec(cfg, req)
	if err != nil {
		t.Fatal(err)
	}
	reordered := slices.Clone(spec.Env)
	slices.Reverse(reordered)
	if got, want := binding.LauncherEnvironmentDigest, digestEnvironment(reordered); got != want {
		t.Fatalf("launcher environment digest = %q, canonical reordered digest = %q", got, want)
	}
	if err := validateCodexReviewAgentSpec(cfg, req, spec, binding); err != nil {
		t.Fatalf("canonical launcher environment binding rejected: %v", err)
	}

	legacy := binding
	legacy.LauncherEnvironmentDigest = digestStrings(spec.Env)
	if legacy.LauncherEnvironmentDigest == binding.LauncherEnvironmentDigest {
		t.Fatal("fixture environment order does not distinguish legacy and canonical digests")
	}
	if err := validateCodexReviewAgentSpec(cfg, req, spec, legacy); !errors.Is(err, ErrConformance) {
		t.Fatalf("legacy order-sensitive environment digest = %v, want conformance failure", err)
	}
}

func TestCodexReviewPostStartEnvironmentAuthentication(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func([]string) []string
		wantErr bool
	}{
		{
			name:   "pre-start shape",
			mutate: func(environment []string) []string { return environment },
		},
		{
			name: "post-start permutation with PATH in the middle",
			mutate: func(environment []string) []string {
				permuted := slices.Clone(environment[1:])
				slices.Reverse(permuted)
				return slices.Insert(permuted, len(permuted)/2, environment[0])
			},
		},
		{
			name: "added entry",
			mutate: func(environment []string) []string {
				return append(environment, "EXTRA=inert")
			},
			wantErr: true,
		},
		{
			name: "removed entry",
			mutate: func(environment []string) []string {
				return append(environment[:1:1], environment[2:]...)
			},
			wantErr: true,
		},
		{
			name: "duplicated exact entry",
			mutate: func(environment []string) []string {
				return append(environment, environment[1])
			},
			wantErr: true,
		},
		{
			name: "changed value",
			mutate: func(environment []string) []string {
				environment[1] += "-changed"
				return environment
			},
			wantErr: true,
		},
		{
			name:    "missing runtime PATH",
			mutate:  func(environment []string) []string { return environment[1:] },
			wantErr: true,
		},
		{
			name: "duplicated runtime PATH",
			mutate: func(environment []string) []string {
				return append(environment, fixedContainerPathEnv)
			},
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			backend, rt, cfg, launchSpec, _ := testCodexReviewLifecycle(t)
			launch, err := backend.CodexReview(context.Background(), cfg, launchSpec)
			if err != nil {
				t.Fatalf("launch: %v", err)
			}
			if err := launch.Close(); err != nil {
				t.Fatalf("close proxy: %v", err)
			}
			reviewContainer := codexReviewContainerName(launchSpec.RunID)
			rt.onInspect = func(id string, report InspectReport) (InspectReport, error) {
				if id == reviewContainer {
					report.Env = tc.mutate(slices.Clone(report.Env))
				}
				return report, nil
			}
			_, err = backend.InspectCodexReview(context.Background(), cfg, launchSpec.RunID)
			if tc.wantErr {
				if !errors.Is(err, ErrConformance) {
					t.Fatalf("InspectCodexReview = %v, want conformance failure", err)
				}
			} else if err != nil {
				t.Fatalf("InspectCodexReview = %v, want authenticated environment", err)
			}

			rt.onInspect = nil
			if err := backend.AbortCodexReview(context.Background(), cfg, launchSpec.RunID); err != nil {
				t.Fatalf("AbortCodexReview: %v", err)
			}
			if len(rt.ctrs) != 0 || len(rt.vols) != 0 || len(rt.nets) != 0 {
				t.Fatalf("runtime residue after abort: containers=%d volumes=%d networks=%d",
					len(rt.ctrs), len(rt.vols), len(rt.nets))
			}
		})
	}
}

func TestCodexReviewAllowlistShapeChecksRealizedSpec(t *testing.T) {
	cfg, req := testCodexReview(t)
	freshShadow := testCodexReviewShadow(
		t, cfg, req.RunID, req.AgentsShadow.volume, "2026-08-03T12:00:02Z",
	)
	freshWorkspace := testCodexReviewWorkspace(
		t, cfg, req.RunID, req.Workspace.volume, "2026-08-03T12:00:04Z",
	)
	authBytes, err := os.ReadFile(req.AuthSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	freshSnapshot := testCodexReviewSnapshot(
		t, cfg, req.RunID, req.Snapshot.volume, authBytes, req.Instructions.Body, "2026-08-03T12:00:06Z",
	)
	currentNetwork := testCodexReviewNetwork(t, cfg, req.RunID)
	spec, binding, err := BuildCodexReviewAgentSpec(cfg, req)
	if err != nil {
		t.Fatal(err)
	}
	rep := InspectReport{
		ID:                      spec.Name,
		ImageReference:          spec.Image,
		Command:                 slices.Clone(spec.Command),
		WorkingDirectory:        "/",
		State:                   StateStopped,
		AllowlistFieldsObserved: true,
		Mounts:                  slices.Clone(spec.Mounts),
		Env:                     append([]string{fixedContainerPathEnv}, spec.Env...),
		Networks:                []string{spec.Network},
		NetworkAttachmentCount:  1,
		NetworksObserved:        true,
	}
	binding.ReviewContainer = spec.Name
	binding.ReviewContainerFingerprint = "2026-08-03T12:00:05Z"
	binding.ReviewOwnershipToken = testOwnershipLabel().Value
	finalBinding, err := verifyCodexReviewAllowlistShape(
		cfg, req, binding, freshShadow, freshWorkspace, freshSnapshot, currentNetwork, rep, spec,
	)
	if err != nil {
		t.Fatalf("conforming realized spec rejected: %v", err)
	}
	if finalBinding.AgentsShadowObserverFingerprint != req.AgentsShadow.observerFingerprint ||
		finalBinding.AgentsShadowPreStartObserverFingerprint != freshShadow.observerFingerprint ||
		finalBinding.WorkspaceObserverFingerprint != req.Workspace.observerFingerprint ||
		finalBinding.WorkspacePreStartObserverFingerprint != freshWorkspace.observerFingerprint ||
		finalBinding.SnapshotObserverFingerprint != req.Snapshot.observerFingerprint ||
		finalBinding.SnapshotPreStartObserverFingerprint != freshSnapshot.observerFingerprint {
		t.Error("final binding did not journal the pre-start observations")
	}
	rep.Mounts[1].ReadOnly = false
	if _, err := verifyCodexReviewAllowlistShape(
		cfg, req, binding, freshShadow, freshWorkspace, freshSnapshot, currentNetwork, rep, spec,
	); !errors.Is(err, ErrConformance) {
		t.Fatalf("writable realized snapshot mount = %v, want conformance failure", err)
	}
	rep.Mounts[1].ReadOnly = true
	if _, err := verifyCodexReviewAllowlistShape(
		cfg, req, binding, req.AgentsShadow, freshWorkspace, freshSnapshot, currentNetwork, rep, spec,
	); !errors.Is(err, ErrConformance) {
		t.Fatalf("reused shadow proof = %v, want conformance failure", err)
	}
	freshShadow.fingerprint = "replacement"
	if _, err := verifyCodexReviewAllowlistShape(
		cfg, req, binding, freshShadow, freshWorkspace, freshSnapshot, currentNetwork, rep, spec,
	); !errors.Is(err, ErrConformance) {
		t.Fatalf("replaced shadow volume = %v, want conformance failure", err)
	}
	freshShadow = testCodexReviewShadow(
		t, cfg, req.RunID, req.AgentsShadow.volume, "2026-08-03T12:00:02Z",
	)
	changedWorkspace := freshWorkspace
	changedWorkspace.treeDigest = strings.Repeat("2", 64)
	if _, err := verifyCodexReviewAllowlistShape(
		cfg, req, binding, freshShadow, changedWorkspace, freshSnapshot, currentNetwork, rep, spec,
	); !errors.Is(err, ErrConformance) {
		t.Fatalf("changed workspace tree = %v, want conformance failure", err)
	}
	replacedNetwork := currentNetwork
	replacedNetwork.fingerprint = "replacement"
	if _, err := verifyCodexReviewAllowlistShape(
		cfg, req, binding, freshShadow, freshWorkspace, freshSnapshot, replacedNetwork, rep, spec,
	); !errors.Is(err, ErrConformance) {
		t.Fatalf("replaced provider network = %v, want conformance failure", err)
	}
}

func TestCodexReviewEnums(t *testing.T) {
	for _, boundary := range AllCodexReviewBoundaries {
		if !boundary.valid() {
			t.Errorf("registered boundary %q reports invalid", boundary)
		}
	}
	for _, mode := range AllCodexAuthModes {
		if !mode.valid() {
			t.Errorf("registered auth mode %q reports invalid", mode)
		}
	}
	for _, state := range AllCodexReviewIntentStates {
		if !state.valid() {
			t.Errorf("registered intent state %q reports invalid", state)
		}
	}
	if CodexReviewBoundary("").valid() || CodexAuthMode("").valid() ||
		CodexReviewIntentState("").valid() {
		t.Error("zero enum value reports valid")
	}
}

// TestCodexReviewIntentStateTokens pins the persisted string token of every
// registered state. A drift here is a stored-JSON compatibility break: the
// tokens are the on-disk representation of the durable launch intent.
func TestCodexReviewIntentStateTokens(t *testing.T) {
	tokens := map[CodexReviewIntentState]string{
		CodexReviewIntentPreparing: "preparing",
		CodexReviewIntentPrepared:  "prepared",
		CodexReviewIntentStarting:  "starting",
		CodexReviewIntentStarted:   "started",
		CodexReviewIntentClosed:    "closed",
	}
	if len(tokens) != len(AllCodexReviewIntentStates) {
		t.Fatalf("token map has %d entries, registration point has %d",
			len(tokens), len(AllCodexReviewIntentStates))
	}
	for _, state := range AllCodexReviewIntentStates {
		want, ok := tokens[state]
		if !ok {
			t.Errorf("registered state %q missing from token expectations", state)
			continue
		}
		if string(state) != want {
			t.Errorf("state token = %q, want %q", string(state), want)
		}
	}
}

// TestCodexReviewIntentStateValidEquivalence proves the valid() predicate that
// replaced validateIdentity's inline membership switch decides the full input
// space identically to that switch: exactly the five registered tokens are
// valid, and the zero value plus adversarial near-members (case, spacing,
// prefix/suffix, substring) are rejected.
func TestCodexReviewIntentStateValidEquivalence(t *testing.T) {
	// oldMembership is the inline switch validateIdentity carried before the
	// registration point existed, reconstructed here as the equivalence oracle.
	oldMembership := func(s CodexReviewIntentState) bool {
		switch s {
		case CodexReviewIntentPreparing, CodexReviewIntentPrepared,
			CodexReviewIntentStarting, CodexReviewIntentStarted, CodexReviewIntentClosed:
			return true
		default:
			return false
		}
	}
	cases := []CodexReviewIntentState{
		CodexReviewIntentPreparing, CodexReviewIntentPrepared,
		CodexReviewIntentStarting, CodexReviewIntentStarted, CodexReviewIntentClosed,
		"", "Preparing", "PREPARED", " starting", "started ", "start",
		"startedx", "xclosed", "prepared\n", "prep", "closed closed", "STARTED",
	}
	for _, s := range cases {
		if got, want := s.valid(), oldMembership(s); got != want {
			t.Errorf("valid(%q) = %v, inline membership = %v", string(s), got, want)
		}
	}
}

func digestBody(body []byte) domain.Digest {
	sum := sha256.Sum256(body)
	return domain.Digest(fmt.Sprintf("sha256:%x", sum))
}
