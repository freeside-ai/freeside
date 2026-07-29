package exec_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
)

var errInputMissing = errors.New("fixture input missing")

type inputSource map[domain.Digest][]byte

func (s inputSource) OpenContext(
	_ context.Context, digest domain.Digest,
) (io.ReadCloser, error) {
	body, ok := s[digest]
	if !ok {
		return nil, fmt.Errorf("%s: %w", digest, errInputMissing)
	}
	return io.NopCloser(bytes.NewReader(slices.Clone(body))), nil
}

type emptyInputSource struct{}

func (emptyInputSource) OpenContext(
	context.Context, domain.Digest,
) (io.ReadCloser, error) {
	return nil, nil
}

type inputSourceFunc func(context.Context, domain.Digest) (io.ReadCloser, error)

func (f inputSourceFunc) OpenContext(
	ctx context.Context, digest domain.Digest,
) (io.ReadCloser, error) {
	return f(ctx, digest)
}

type blockingOpenSource struct {
	body    []byte
	started chan struct{}
	once    sync.Once
}

func (s *blockingOpenSource) OpenContext(
	ctx context.Context, _ domain.Digest,
) (io.ReadCloser, error) {
	s.once.Do(func() { close(s.started) })
	<-ctx.Done()
	return io.NopCloser(bytes.NewReader(slices.Clone(s.body))), nil
}

type gatedInputSource struct {
	source  inputSource
	started chan struct{}
	release chan struct{}
	once    sync.Once
	mu      sync.Mutex
	opens   int
}

func (s *gatedInputSource) OpenContext(
	ctx context.Context, digest domain.Digest,
) (io.ReadCloser, error) {
	s.mu.Lock()
	s.opens++
	s.mu.Unlock()
	s.once.Do(func() {
		close(s.started)
		select {
		case <-s.release:
		case <-ctx.Done():
		}
	})
	return s.source.OpenContext(ctx, digest)
}

func (s *gatedInputSource) openCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.opens
}

type lateReadCloser struct {
	body    []byte
	started chan struct{}
	release chan struct{}
	once    sync.Once
	read    bool
}

func (r *lateReadCloser) Read(p []byte) (int, error) {
	if r.read {
		return 0, io.EOF
	}
	r.once.Do(func() { close(r.started) })
	<-r.release
	r.read = true
	return copy(p, r.body), io.EOF
}

func (*lateReadCloser) Close() error { return nil }

type failingInputBody struct {
	readErr  error
	closeErr error
}

func (b failingInputBody) Read([]byte) (int, error) {
	if b.readErr != nil {
		return 0, b.readErr
	}
	return 0, io.EOF
}

func (b failingInputBody) Close() error { return b.closeErr }

func contentDigest(body []byte) domain.Digest {
	return domain.Digest(fmt.Sprintf("sha256:%x", sha256.Sum256(body)))
}

type recordingMaterializedDriver struct {
	mu      sync.Mutex
	started int
	inputs  exec.StageInputs
	intents map[domain.InvocationID]bool
}

func (d *recordingMaterializedDriver) StartWithInputs(
	ctx context.Context, id domain.InvocationID, _ exec.StartSpec, load exec.StageInputLoader,
) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.intents == nil {
		d.intents = make(map[domain.InvocationID]bool)
	}
	if d.intents[id] {
		return exec.ErrDuplicateStart
	}
	inputs, err := load(ctx)
	if err != nil {
		return err
	}
	d.intents[id] = true
	d.started++
	d.inputs = inputs
	return nil
}

func (d *recordingMaterializedDriver) Inspect(
	context.Context, domain.InvocationID,
) (exec.Status, error) {
	return exec.StatusRunning, nil
}

func (d *recordingMaterializedDriver) Stream(
	context.Context, domain.InvocationID,
) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(nil)), nil
}

func (d *recordingMaterializedDriver) Cancel(context.Context, domain.InvocationID) error {
	return nil
}

func (d *recordingMaterializedDriver) Collect(
	context.Context, domain.InvocationID,
) (exec.StageResult, error) {
	return exec.StageResult{}, nil
}

type materializeFixture struct {
	source    inputSource
	admission domain.ExecutionAdmission
	spec      exec.StartSpec
	bodies    map[string][]byte
}

func newMaterializeFixture(t *testing.T) materializeFixture {
	t.Helper()
	promptPath := filepath.Join("..", "..", "..", "prompts", "phase-1a", "implementer.md")
	prompt, err := os.ReadFile(promptPath) //nolint:gosec // fixed repository fixture path, never input-derived
	if err != nil {
		t.Fatalf("read Phase 1A implementer prompt: %v", err)
	}
	bodies := map[string][]byte{
		"spec":         []byte("# Approved specification\nImplement the feature.\n"),
		"prompt":       prompt,
		"policy":       []byte("scope: daemon/internal/exec\n"),
		"vendor":       []byte("# Host instructions\nPreserve existing work.\n"),
		"conversation": []byte(`{"version":"freeside.conversation.prefix/v1","conversation_id":"conv-1","through_sequence":1,"messages":[]}`),
		"prior":        []byte("prior evidence\n"),
		"image":        {0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'},
	}
	source := inputSource{}
	for _, body := range bodies {
		source[contentDigest(body)] = slices.Clone(body)
	}
	inputDigest := contentDigest([]byte("logical invocation inputs"))
	conversationDigest := contentDigest(bodies["conversation"])
	vendorDigest := contentDigest(bodies["vendor"])
	snapshot, err := domain.NewStageInputSnapshot(domain.StageInputSnapshotInput{
		InputDigest:         inputDigest,
		SpecificationDigest: contentDigest(bodies["spec"]),
		PromptPackageDigest: contentDigest(bodies["prompt"]),
		PolicyDigest:        contentDigest(bodies["policy"]),
		VendorInstructions: &domain.VendorInstructionSnapshot{
			Vendor: domain.AgentVendorClaude,
			Digest: &vendorDigest,
		},
		ConversationDigest:   &conversationDigest,
		PriorArtifactDigests: []domain.Digest{contentDigest(bodies["prior"])},
		ImageInputDigests:    []domain.Digest{contentDigest(bodies["image"])},
	})
	if err != nil {
		t.Fatal(err)
	}
	identity := domain.AuthIdentityID("auth-claude-owner")
	admission, err := domain.NewExecutionAdmission(domain.ExecutionAdmissionInput{
		InvocationID: "inv-1", RunID: "run-1", StageID: "stage-1", AttemptID: "attempt-1",
		Backend:        "fresh_vm_read_only_volume_handoff",
		Capabilities:   domain.NewCapabilitySnapshot(domain.CapPostExitExport),
		OperatingMode:  domain.ModeAttendedDev,
		CredentialMode: domain.CredentialSubscriptionContained,
		EgressProfile:  domain.EgressProviderOnly,
		ImageRef: domain.ImageRef(
			"ghcr.io/freeside-ai/agent@sha256:" + strings.Repeat("ab", 32)),
		SpecDigest:   snapshot.SpecificationDigest,
		PolicyDigest: snapshot.PolicyDigest,
		InputDigest:  snapshot.InputDigest,
		StageInputs:  &snapshot,
		Base: domain.BaseRevision{
			Repo: "owner/repo", RepositoryID: 424242,
			BaseRef: "refs/heads/main", BaseSHA: "deadbeef",
		},
		Workspace: "freeside-handoff-run-1-ws", AuthIdentityID: &identity,
		AdmittedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	return materializeFixture{
		source: source, admission: admission,
		spec: exec.StartSpecFromAdmission(admission), bodies: bodies,
	}
}

func newTestMaterializer(t *testing.T, source exec.InputSource) *exec.Materializer {
	t.Helper()
	materializer, err := exec.NewMaterializer(source, exec.MaterializerOptions{
		MaxInputBytes: 1 << 20,
		MaxTotalBytes: 4 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	return materializer
}

func TestMaterializeStageInputs(t *testing.T) {
	fixture := newMaterializeFixture(t)
	bundle, err := newTestMaterializer(t, fixture.source).Materialize(t.Context(), fixture.spec)
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Digest() != fixture.spec.StageInputs.ID {
		t.Fatalf("bundle digest = %s, want %s", bundle.Digest(), fixture.spec.StageInputs.ID)
	}
	if fixture.admission.StageInputs.PromptPackageDigest != contentDigest(fixture.bodies["prompt"]) {
		t.Fatal("admission did not bind the approved default-branch prompt bytes")
	}
	if !bytes.Equal(bundle.Specification().Bytes(), fixture.bodies["spec"]) ||
		!bytes.Equal(bundle.PromptPackage().Bytes(), fixture.bodies["prompt"]) ||
		!bytes.Equal(bundle.Policy().Bytes(), fixture.bodies["policy"]) {
		t.Fatal("materialized core input differs from admitted bytes")
	}
	vendor, ok := bundle.VendorInstructions()
	if !ok || vendor.Vendor() != domain.AgentVendorClaude {
		t.Fatal("materialized bundle lost its vendor-instruction role")
	}
	vendorContent, ok := vendor.Content()
	if !ok || !bytes.Equal(vendorContent.Bytes(), fixture.bodies["vendor"]) {
		t.Fatal("materialized vendor instructions differ from admitted bytes")
	}
	conversation, ok := bundle.ConversationPrefix()
	if !ok || !bytes.Equal(conversation.Bytes(), fixture.bodies["conversation"]) {
		t.Fatal("materialized conversation prefix differs from admitted bytes")
	}
	if got := bundle.PriorArtifacts(); len(got) != 1 ||
		!bytes.Equal(got[0].Bytes(), fixture.bodies["prior"]) {
		t.Fatalf("prior artifacts = %v", got)
	}
	if got := bundle.ImageInputs(); len(got) != 1 ||
		!bytes.Equal(got[0].Bytes(), fixture.bodies["image"]) {
		t.Fatalf("image inputs = %v", got)
	}

	changed := bundle.PromptPackage().Bytes()
	changed[0] ^= 0xff
	if bytes.Equal(changed, bundle.PromptPackage().Bytes()) {
		t.Fatal("Bytes exposed mutable bundle storage")
	}
}

func TestMaterializeExplicitVendorInstructionAbsence(t *testing.T) {
	fixture := newMaterializeFixture(t)
	snapshot := *fixture.spec.StageInputs
	vendor := *snapshot.VendorInstructions
	vendor.Digest = nil
	snapshot.VendorInstructions = &vendor
	id, err := snapshot.ComputeID()
	if err != nil {
		t.Fatal(err)
	}
	snapshot.ID = id
	fixture.spec.StageInputs = &snapshot

	bundle, err := newTestMaterializer(t, fixture.source).Materialize(
		t.Context(), fixture.spec,
	)
	if err != nil {
		t.Fatal(err)
	}
	instructions, ok := bundle.VendorInstructions()
	if !ok {
		t.Fatal("explicit vendor-instruction absence became a legacy snapshot")
	}
	if _, present := instructions.Content(); present {
		t.Fatal("explicitly absent vendor instructions materialized content")
	}
}

func TestMaterializeMissingContentFailsClosed(t *testing.T) {
	fixture := newMaterializeFixture(t)
	delete(fixture.source, fixture.spec.StageInputs.PromptPackageDigest)
	_, err := newTestMaterializer(t, fixture.source).Materialize(t.Context(), fixture.spec)
	if !errors.Is(err, errInputMissing) {
		t.Fatalf("Materialize = %v, want missing input", err)
	}
}

func TestMaterializeMissingVendorInstructionsFailsClosed(t *testing.T) {
	fixture := newMaterializeFixture(t)
	delete(fixture.source, *fixture.spec.StageInputs.VendorInstructions.Digest)
	_, err := newTestMaterializer(t, fixture.source).Materialize(
		t.Context(), fixture.spec,
	)
	if !errors.Is(err, errInputMissing) {
		t.Fatalf("Materialize = %v, want missing vendor instructions", err)
	}
}

func TestMaterializeCorruptVendorInstructionsFailsClosed(t *testing.T) {
	fixture := newMaterializeFixture(t)
	vendorDigest := *fixture.spec.StageInputs.VendorInstructions.Digest
	fixture.source[vendorDigest] = []byte("mutated vendor instructions\n")
	_, err := newTestMaterializer(t, fixture.source).Materialize(
		t.Context(), fixture.spec,
	)
	if !errors.Is(err, exec.ErrInputDigestMismatch) {
		t.Fatalf("Materialize = %v, want %v", err, exec.ErrInputDigestMismatch)
	}
}

func TestMaterializeMissingBodyFailsClosed(t *testing.T) {
	fixture := newMaterializeFixture(t)
	_, err := newTestMaterializer(t, emptyInputSource{}).Materialize(t.Context(), fixture.spec)
	if !errors.Is(err, exec.ErrInputBodyMissing) {
		t.Fatalf("Materialize = %v, want %v", err, exec.ErrInputBodyMissing)
	}
}

func TestMaterializeDigestMismatchFailsClosed(t *testing.T) {
	fixture := newMaterializeFixture(t)
	fixture.source[fixture.spec.StageInputs.PolicyDigest] = []byte("mutated policy\n")
	_, err := newTestMaterializer(t, fixture.source).Materialize(t.Context(), fixture.spec)
	if !errors.Is(err, exec.ErrInputDigestMismatch) {
		t.Fatalf("Materialize = %v, want %v", err, exec.ErrInputDigestMismatch)
	}
}

func TestMaterializeCancellationAfterOpenFailsClosed(t *testing.T) {
	fixture := newMaterializeFixture(t)
	source := &blockingOpenSource{
		body: fixture.bodies["spec"], started: make(chan struct{}),
	}
	materializer := newTestMaterializer(t, source)
	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() {
		_, err := materializer.Materialize(ctx, fixture.spec)
		result <- err
	}()
	<-source.started
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("Materialize = %v, want %v", err, context.Canceled)
	}
}

func TestMaterializeCancellationAfterReadFailsClosed(t *testing.T) {
	fixture := newMaterializeFixture(t)
	reader := &lateReadCloser{
		body: fixture.bodies["spec"], started: make(chan struct{}), release: make(chan struct{}),
	}
	source := inputSourceFunc(func(
		ctx context.Context, digest domain.Digest,
	) (io.ReadCloser, error) {
		if digest == fixture.spec.StageInputs.SpecificationDigest {
			return reader, nil
		}
		return fixture.source.OpenContext(ctx, digest)
	})
	materializer := newTestMaterializer(t, source)
	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() {
		_, err := materializer.Materialize(ctx, fixture.spec)
		result <- err
	}()
	<-reader.started
	cancel()
	close(reader.release)
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("Materialize = %v, want %v", err, context.Canceled)
	}
}

func TestMaterializeReplaysAdmittedSnapshotAcrossSourceDrift(t *testing.T) {
	fixture := newMaterializeFixture(t)
	materializer := newTestMaterializer(t, fixture.source)
	first, err := materializer.Materialize(t.Context(), fixture.spec)
	if err != nil {
		t.Fatal(err)
	}

	newPrompt := []byte("# Newer prompt\nDo something else.\n")
	fixture.source[contentDigest(newPrompt)] = newPrompt
	second, err := materializer.Materialize(t.Context(), fixture.spec)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.PromptPackage().Bytes(), second.PromptPackage().Bytes()) ||
		!bytes.Equal(second.PromptPackage().Bytes(), fixture.bodies["prompt"]) {
		t.Fatal("replay silently selected newer prompt content")
	}
}

func TestMaterializeReplaysAfterArtifactStoreReopen(t *testing.T) {
	fixture := newMaterializeFixture(t)
	dir := t.TempDir()
	blobs, err := signet.NewBlobStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	for digest, body := range fixture.source {
		if _, err := blobs.Put(digest, bytes.NewReader(body)); err != nil {
			t.Fatalf("put %s: %v", digest, err)
		}
	}
	first, err := newTestMaterializer(t, blobs).Materialize(t.Context(), fixture.spec)
	if err != nil {
		t.Fatal(err)
	}

	newPrompt := []byte("# Newer prompt\nDo something else.\n")
	if _, err := blobs.Put(contentDigest(newPrompt), bytes.NewReader(newPrompt)); err != nil {
		t.Fatal(err)
	}
	reopened, err := signet.NewBlobStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := newTestMaterializer(t, reopened).Materialize(t.Context(), fixture.spec)
	if err != nil {
		t.Fatal(err)
	}
	if first.Digest() != replayed.Digest() ||
		!bytes.Equal(first.PromptPackage().Bytes(), replayed.PromptPackage().Bytes()) {
		t.Fatal("artifact-store reopen changed the admitted bundle")
	}
}

func TestMaterializeRefusesLegacyOrOversizedInputs(t *testing.T) {
	fixture := newMaterializeFixture(t)
	if _, err := newTestMaterializer(t, fixture.source).Materialize(
		t.Context(), exec.StartSpec{},
	); !errors.Is(err, exec.ErrStageInputsMissing) {
		t.Fatalf("legacy Materialize = %v, want %v", err, exec.ErrStageInputsMissing)
	}

	materializer, err := exec.NewMaterializer(fixture.source, exec.MaterializerOptions{
		MaxInputBytes: 4,
		MaxTotalBytes: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := materializer.Materialize(t.Context(), fixture.spec); !errors.Is(err, exec.ErrInputTooLarge) {
		t.Fatalf("oversized Materialize = %v, want %v", err, exec.ErrInputTooLarge)
	}
}

func TestMaterializeRejectsNonCanonicalDigestBeforeSourceLookup(t *testing.T) {
	fixture := newMaterializeFixture(t)
	snapshot := *fixture.spec.StageInputs
	snapshot.SpecificationDigest = "../../../operator-secret"
	id, err := snapshot.ComputeID()
	if err != nil {
		t.Fatal(err)
	}
	snapshot.ID = id
	fixture.spec.SpecDigest = snapshot.SpecificationDigest
	fixture.spec.StageInputs = &snapshot

	_, err = newTestMaterializer(t, fixture.source).Materialize(t.Context(), fixture.spec)
	if !errors.Is(err, domain.ErrStageInputsNotCanonical) {
		t.Fatalf("Materialize = %v, want %v", err, domain.ErrStageInputsNotCanonical)
	}
}

func TestMaterializingStageDriverStartsOnlyAfterVerification(t *testing.T) {
	fixture := newMaterializeFixture(t)
	process := &recordingMaterializedDriver{}
	driver, err := exec.NewMaterializingStageDriver(
		newTestMaterializer(t, fixture.source), process,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := driver.Start(t.Context(), "inv-1", fixture.spec); err != nil {
		t.Fatal(err)
	}
	if process.started != 1 || process.inputs.Digest() != fixture.spec.StageInputs.ID {
		t.Fatalf("process starts = %d, bundle = %s", process.started, process.inputs.Digest())
	}

	delete(fixture.source, fixture.spec.StageInputs.PromptPackageDigest)
	if err := driver.Start(t.Context(), "inv-1", fixture.spec); !errors.Is(err, exec.ErrDuplicateStart) {
		t.Fatalf("duplicate Start = %v, want %v", err, exec.ErrDuplicateStart)
	}
	if err := driver.Start(t.Context(), "inv-2", fixture.spec); !errors.Is(err, errInputMissing) {
		t.Fatalf("failed Start = %v, want missing input", err)
	}
	if process.started != 1 {
		t.Fatalf("process started after failed materialization: %d starts", process.started)
	}
}

func TestMaterializeClassifiesOpenedInputIOAsRetryable(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		body failingInputBody
	}{
		{"read", failingInputBody{readErr: errors.New("fixture read failure")}},
		{"close", failingInputBody{closeErr: errors.New("fixture close failure")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newMaterializeFixture(t)
			source := inputSourceFunc(func(context.Context, domain.Digest) (io.ReadCloser, error) {
				return tc.body, nil
			})
			materializer, err := exec.NewMaterializer(source, exec.MaterializerOptions{
				MaxInputBytes: 4 << 20, MaxTotalBytes: 32 << 20,
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := materializer.Materialize(t.Context(), fixture.spec); !errors.Is(err, exec.ErrInputUnavailable) {
				t.Fatalf("Materialize = %v, want retryable input class", err)
			}
		})
	}
}

func TestMaterializingStageDriverSerializesDuplicateBeforeInputIO(t *testing.T) {
	fixture := newMaterializeFixture(t)
	source := &gatedInputSource{
		source: fixture.source, started: make(chan struct{}), release: make(chan struct{}),
	}
	process := &recordingMaterializedDriver{}
	driver, err := exec.NewMaterializingStageDriver(newTestMaterializer(t, source), process)
	if err != nil {
		t.Fatal(err)
	}

	first := make(chan error, 1)
	go func() { first <- driver.Start(t.Context(), "inv-1", fixture.spec) }()
	<-source.started
	second := make(chan error, 1)
	go func() { second <- driver.Start(t.Context(), "inv-1", fixture.spec) }()
	close(source.release)

	if err := <-first; err != nil {
		t.Fatalf("first Start = %v", err)
	}
	if err := <-second; !errors.Is(err, exec.ErrDuplicateStart) {
		t.Fatalf("concurrent duplicate Start = %v, want %v", err, exec.ErrDuplicateStart)
	}
	if got, want := source.openCount(), len(fixture.source); got != want {
		t.Fatalf("source opens = %d, want one materialization (%d)", got, want)
	}
}
