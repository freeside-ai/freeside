package ward

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
)

type testReviewInstructionArtifacts map[domain.Digest][]byte

var errTestReviewInstructionArtifactNotFound = errors.New("review instruction artifact not found")

func (a testReviewInstructionArtifacts) Open(digest domain.Digest) (io.ReadCloser, error) {
	body, ok := a[digest]
	if !ok {
		return nil, errTestReviewInstructionArtifactNotFound
	}
	return io.NopCloser(bytes.NewReader(body)), nil
}

type testReviewInstructionArtifactsFunc func(domain.Digest) (io.ReadCloser, error)

func (f testReviewInstructionArtifactsFunc) Open(digest domain.Digest) (io.ReadCloser, error) {
	return f(digest)
}

type testReviewInstructionReadCloser struct {
	io.Reader
	closeErr error
}

func (r testReviewInstructionReadCloser) Close() error {
	return r.closeErr
}

type testReviewInstructionErrorReader struct {
	err error
}

func (r testReviewInstructionErrorReader) Read([]byte) (int, error) {
	return 0, r.err
}

type testReviewInstructionBodyErrorReader struct {
	body []byte
	err  error
}

func (r *testReviewInstructionBodyErrorReader) Read(p []byte) (int, error) {
	n := copy(p, r.body)
	r.body = r.body[n:]
	if len(r.body) == 0 {
		return n, r.err
	}
	return n, nil
}

func testReviewInstructionBinding() exec.ReviewInstructionBinding {
	_, binding, _ := exec.ComposeCodexReviewInstructions(exec.ReviewHostInstructionInput{}, nil)
	return binding
}

func TestCodexReviewSourceReconstructsInstructionClosure(t *testing.T) {
	cfg, _ := testCodexReview(t)
	host := []byte("operator rules\n")
	repository := []byte("repository rules\n")
	bundle, binding, err := exec.ComposeCodexReviewInstructions(
		exec.ReviewHostInstructionInput{Present: true, Body: host},
		[]exec.ReviewInstructionSourceInput{{Path: "AGENTS.md", Body: repository}},
	)
	if err != nil {
		t.Fatal(err)
	}
	artifacts := testReviewInstructionArtifacts{
		*binding.HostDigest:                 host,
		binding.RepositorySources[0].Digest: repository,
		binding.ResultDigest:                bundle,
	}
	source := &CodexReviewSource{cfg: CodexReviewSourceConfig{
		Review: cfg, InstructionArtifacts: artifacts,
	}}
	instructions, path, err := source.materializeReviewInstructions(
		t.Context(), "review-closure-1", binding,
	)
	if err != nil {
		t.Fatal(err)
	}
	if instructions.Digest != binding.ResultDigest || !bytes.Equal(instructions.Body, bundle) {
		t.Fatal("reconstructed instructions lost the result binding")
	}
	_, got, err := readCodexReviewInput(cfg.InputRoot, path, domain.MaxVendorInstructionBytes)
	if err != nil || !bytes.Equal(got, bundle) {
		t.Fatalf("materialized instruction file = %q, %v", got, err)
	}

	delete(artifacts, binding.RepositorySources[0].Digest)
	if _, _, err := source.materializeReviewInstructions(
		t.Context(), "review-closure-2", binding,
	); err == nil || classifyCodexInstructionMaterializationFailure(err) != domain.ReviewFailureContradiction {
		t.Fatalf("missing repository source artifact classification = %v", err)
	}
	artifacts[binding.RepositorySources[0].Digest] = repository
	artifacts[binding.ResultDigest] = []byte("tampered result")
	if _, _, err := source.materializeReviewInstructions(
		t.Context(), "review-closure-3", binding,
	); err == nil || classifyCodexInstructionMaterializationFailure(err) != domain.ReviewFailureContradiction {
		t.Fatalf("tampered result artifact classification = %v", err)
	}
}

func TestCodexReviewSourceReplacesStaleInstructionSnapshot(t *testing.T) {
	cfg, _ := testCodexReview(t)
	source := &CodexReviewSource{cfg: CodexReviewSourceConfig{Review: cfg}}
	id := domain.InvocationID("review-existing-snapshot")
	directory := filepath.Dir(source.instructionFile(id))
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	target := source.instructionFile(id)
	if err := os.WriteFile(target, []byte("operator-controlled input"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := source.writeReviewInstructionFile(id, []byte("trusted bundle")); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(target) //nolint:gosec // test-controlled snapshot path
	if err != nil || string(got) != "trusted bundle" {
		t.Fatalf("existing instruction snapshot = %q, %v", got, err)
	}
}

func TestRemoveCodexReviewInstructionSnapshotRejectsInvalidID(t *testing.T) {
	cfg, _ := testCodexReview(t)
	root := codexReviewInstructionRoot(cfg.InputRoot)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "AGENTS.md")
	if err := os.WriteFile(target, []byte("operator-controlled input"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeCodexReviewInstructionSnapshot(cfg.InputRoot, ".."); err == nil {
		t.Fatal("invalid snapshot ID was accepted")
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("invalid snapshot ID removed root input: %v", err)
	}
}

func TestCodexReviewRecoveryRemovesInstructionSnapshot(t *testing.T) {
	backend, _, cfg, launch, journal := testCodexReviewLifecycle(t)
	owner := testOwnershipLabel()
	journal.intent = &CodexReviewLaunchIntent{
		RunID: launch.RunID, OwnershipToken: owner.Value,
		ShadowVolume: codexReviewShadowVolumeName(launch.RunID),
		Network:      codexReviewNetworkName(launch.RunID), ReviewContainer: codexReviewContainerName(launch.RunID),
		State: CodexReviewIntentClosed,
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
		InvocationID: domain.InvocationID(launch.RunID), FailureClass: domain.ReviewFailureTransient,
		Failure: "cleanup completed before the ready mark",
	}}
	source := &CodexReviewSource{cfg: CodexReviewSourceConfig{Review: cfg}}
	path, err := source.writeReviewInstructionFile(domain.InvocationID(launch.RunID), []byte("trusted bundle"))
	if err != nil {
		t.Fatal(err)
	}
	recovery, err := NewCodexReviewRecovery(backend, journal, cfg.VolumeLifecycleLeaser, cfg.InputRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := recovery.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovered instruction snapshot remains: %v", err)
	}
}

func TestCodexReviewRecoveryRemovesOrphanWorkspaceInstructionSnapshot(t *testing.T) {
	ctx := t.Context()
	fx := newHandoffFixture(t)
	seed := fx.seed(t).Seed
	backend := fx.backend(t)
	cfg, _ := testCodexReview(t)
	journal := &fakeCodexReviewJournal{}
	id := "review-orphaned-snapshot"
	if _, err := backend.PrepareCodexReviewWorkspace(
		ctx, journal, id, seed.SourceDir, seed.Base, 64,
	); err != nil {
		t.Fatal(err)
	}
	source := &CodexReviewSource{cfg: CodexReviewSourceConfig{Review: cfg}}
	path, err := source.writeReviewInstructionFile(domain.InvocationID(id), []byte("trusted bundle"))
	if err != nil {
		t.Fatal(err)
	}
	leaser, err := NewRuntimeCodexReviewVolumeLeaser(fx.rt)
	if err != nil {
		t.Fatal(err)
	}
	recovery, err := NewCodexReviewRecovery(backend, journal, leaser, cfg.InputRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := recovery.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphaned instruction snapshot remains: %v", err)
	}
}

func TestCodexReviewRecoveryKeepsOrphanWorkspaceWhenSnapshotRemovalFails(t *testing.T) {
	ctx := t.Context()
	fx := newHandoffFixture(t)
	seed := fx.seed(t).Seed
	backend := fx.backend(t)
	cfg, _ := testCodexReview(t)
	journal := &fakeCodexReviewJournal{}
	id := "review-orphaned-snapshot-failure"
	if _, err := backend.PrepareCodexReviewWorkspace(
		ctx, journal, id, seed.SourceDir, seed.Base, 64,
	); err != nil {
		t.Fatal(err)
	}
	source := &CodexReviewSource{cfg: CodexReviewSourceConfig{Review: cfg}}
	path, err := source.writeReviewInstructionFile(domain.InvocationID(id), []byte("trusted bundle"))
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(filepath.Dir(path))
	if err := os.Chmod(root, 0o755); err != nil { //nolint:gosec // test makes an owned temporary root invalid
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o700) }) //nolint:gosec // restore the owned temporary root
	leaser, err := NewRuntimeCodexReviewVolumeLeaser(fx.rt)
	if err != nil {
		t.Fatal(err)
	}
	recovery, err := NewCodexReviewRecovery(backend, journal, leaser, cfg.InputRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := recovery.Reconcile(ctx); err == nil {
		t.Fatal("recovery accepted an invalid instruction snapshot root")
	}
	if journal.workspaceBinding.SourceRunID != id {
		t.Fatalf("orphan workspace binding = %q, want %q", journal.workspaceBinding.SourceRunID, id)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("orphaned instruction snapshot = %v", err)
	}
}

func TestCodexReviewSourceRegatesInstructionClosureBeforeReadiness(t *testing.T) {
	id := domain.InvocationID("review-readiness-1")
	bundle, binding, err := exec.ComposeCodexReviewInstructions(
		exec.ReviewHostInstructionInput{},
		[]exec.ReviewInstructionSourceInput{{Path: "AGENTS.md", Body: []byte("trusted base\n")}},
	)
	if err != nil {
		t.Fatal(err)
	}
	request := exec.ReviewRequest{
		RunID: "run-1", Round: 1, Repo: "owner/repo", RepositoryID: 42, BaseRef: "main",
		BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40), Workspace: "/candidate",
		Verification: testReviewVerificationEvidence(), Instructions: binding, RequestedAt: codexReviewEpoch,
	}
	authority, err := request.AuthorityDigest()
	if err != nil {
		t.Fatal(err)
	}
	journal := &fakeCodexReviewJournal{
		requests: map[string]exec.ReviewRequest{string(id): request},
		outcomes: map[string]CodexReviewSourceOutcome{string(id): {
			InvocationID: id, FailureClass: domain.ReviewFailureContradiction,
			Failure: "closed test outcome",
		}},
		ready: map[string]bool{string(id): true},
	}
	artifacts := testReviewInstructionArtifacts{
		binding.RepositorySources[0].Digest: []byte("trusted base\n"),
		binding.ResultDigest:                bundle,
	}
	source := &CodexReviewSource{cfg: CodexReviewSourceConfig{
		Journal: journal, InstructionArtifacts: artifacts,
	}}
	if err := source.VerifyRequestAuthority(t.Context(), id, authority); err != nil {
		t.Fatal(err)
	}
	artifacts[binding.ResultDigest] = []byte("tampered after launch")
	var failure *exec.ReviewSourceFailure
	if err := source.VerifyRequestAuthority(t.Context(), id, authority); !errors.As(err, &failure) ||
		failure.Class != domain.ReviewFailureContradiction {
		t.Fatalf("tampered readiness instruction closure = %v", err)
	}

	artifactIO := &os.PathError{Op: "open", Path: "artifact", Err: syscall.EIO}
	unexpectedReconcile := errors.New("authority I/O must not reconcile the request")
	source.cfg.InstructionArtifacts = testReviewInstructionArtifactsFunc(
		func(domain.Digest) (io.ReadCloser, error) { return nil, artifactIO },
	)
	journal.failGetOutcome = unexpectedReconcile
	failure = nil
	if err := source.VerifyRequestAuthority(t.Context(), id, authority); !errors.As(err, &failure) ||
		failure.Class != domain.ReviewFailureTransient || !errors.Is(err, artifactIO) ||
		errors.Is(err, unexpectedReconcile) {
		t.Fatalf("operational readiness instruction closure = %v", err)
	}
}

func TestCodexReviewSourceCleansPreparedWorkspaceWhenInstructionClosureFails(t *testing.T) {
	tests := []struct {
		name        string
		directStart bool
		setup       func(*testing.T, CodexReviewSpec) (exec.ReviewInstructionBinding, CodexReviewInstructionArtifacts)
	}{
		{
			name: "missing source",
			setup: func(t *testing.T, _ CodexReviewSpec) (exec.ReviewInstructionBinding, CodexReviewInstructionArtifacts) {
				bundle, binding, err := exec.ComposeCodexReviewInstructions(
					exec.ReviewHostInstructionInput{},
					[]exec.ReviewInstructionSourceInput{{Path: "AGENTS.md", Body: []byte("trusted base\n")}},
				)
				if err != nil {
					t.Fatal(err)
				}
				return binding, testReviewInstructionArtifacts{binding.ResultDigest: bundle}
			},
		},
		{
			name: "oversized result",
			setup: func(_ *testing.T, request CodexReviewSpec) (exec.ReviewInstructionBinding, CodexReviewInstructionArtifacts) {
				return request.InstructionBinding, testReviewInstructionArtifacts{
					request.InstructionBinding.ResultDigest: bytes.Repeat(
						[]byte("x"), int(domain.MaxVendorInstructionBytes)+1,
					),
				}
			},
		},
		{
			name: "tampered result",
			setup: func(_ *testing.T, request CodexReviewSpec) (exec.ReviewInstructionBinding, CodexReviewInstructionArtifacts) {
				return request.InstructionBinding, testReviewInstructionArtifacts{
					request.InstructionBinding.ResultDigest: []byte("tampered result"),
				}
			},
		},
		{
			name: "invalid binding", directStart: true,
			setup: func(_ *testing.T, request CodexReviewSpec) (exec.ReviewInstructionBinding, CodexReviewInstructionArtifacts) {
				binding := request.InstructionBinding
				binding.CompositionVersion = "future"
				return binding, testReviewInstructionArtifacts{
					request.InstructionBinding.ResultDigest: request.Instructions.Body,
				}
			},
		},
		{
			name: "composition divergence",
			setup: func(_ *testing.T, request CodexReviewSpec) (exec.ReviewInstructionBinding, CodexReviewInstructionArtifacts) {
				binding := request.InstructionBinding
				binding.ResultDigest = domain.Digest("sha256:" + strings.Repeat("f", 64))
				return binding, testReviewInstructionArtifacts{
					binding.ResultDigest: request.Instructions.Body,
				}
			},
		},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			fx := newHandoffFixture(t)
			seedSpec := fx.seed(t)
			backend := fx.backend(t)
			cfg, requestSpec := testCodexReview(t)
			journal := &fakeCodexReviewJournal{}
			sourceConfig := codexReviewSourceConfigForTest(t, backend, cfg, requestSpec, journal)
			binding, artifacts := tc.setup(t, requestSpec)
			sourceConfig.InstructionArtifacts = artifacts
			source, err := NewCodexReviewSource(sourceConfig)
			if err != nil {
				t.Fatal(err)
			}
			id := domain.InvocationID(fmt.Sprintf("review-invalid-instructions-%d", i))
			request := exec.ReviewRequest{
				RunID: "run-invalid-instructions", Round: 1, Repo: seedSpec.Seed.Base.Repo,
				RepositoryID: seedSpec.Seed.Base.RepositoryID, BaseRef: seedSpec.Seed.Base.BaseRef,
				BaseSHA: strings.Repeat("a", 40), HeadSHA: seedSpec.Seed.Base.BaseSHA,
				Workspace: seedSpec.Seed.SourceDir, Verification: testReviewVerificationEvidence(),
				Instructions: binding, RequestedAt: codexReviewEpoch,
			}
			var failure *exec.ReviewSourceFailure
			var startErr error
			if tc.directStart {
				startErr = source.startRequestedReview(ctx, id, request)
			} else {
				startErr = source.RequestReview(ctx, id, request)
			}
			if !errors.As(startErr, &failure) ||
				failure.Class != domain.ReviewFailureContradiction {
				t.Fatalf("instruction refusal = %v", startErr)
			}
			if _, err := journal.GetCodexReviewWorkspaceBinding(ctx, string(id)); !errors.Is(err, ErrCodexReviewWorkspaceNotFound) {
				t.Fatalf("prepared review workspace survived instruction refusal: %v", err)
			}
			if journal.intent != nil {
				t.Fatal("credential-bearing review launch began after instruction refusal")
			}
		})
	}
}

func TestCodexReviewSourceRetriesInstructionMaterializationUnderSameInvocation(t *testing.T) {
	tests := []struct {
		name string
		fail func([]byte) (io.ReadCloser, error)
	}{
		{
			name: "open I/O",
			fail: func([]byte) (io.ReadCloser, error) {
				return nil, &os.PathError{Op: "open", Path: "artifact", Err: syscall.EIO}
			},
		},
		{
			name: "read I/O",
			fail: func([]byte) (io.ReadCloser, error) {
				return testReviewInstructionReadCloser{
					Reader: testReviewInstructionErrorReader{err: syscall.EIO},
				}, nil
			},
		},
		{
			name: "close I/O",
			fail: func(body []byte) (io.ReadCloser, error) {
				return testReviewInstructionReadCloser{
					Reader: bytes.NewReader(body), closeErr: syscall.EIO,
				}, nil
			},
		},
		{
			name: "cancellation",
			fail: func([]byte) (io.ReadCloser, error) {
				return nil, context.Canceled
			},
		},
		{
			name: "deadline",
			fail: func([]byte) (io.ReadCloser, error) {
				return nil, context.DeadlineExceeded
			},
		},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			fx := newHandoffFixture(t)
			seedSpec := fx.seed(t)
			backend := fx.backend(t)
			cfg, requestSpec := testCodexReview(t)
			journal := &fakeCodexReviewJournal{}
			sourceConfig := codexReviewSourceConfigForTest(t, backend, cfg, requestSpec, journal)
			artifacts := sourceConfig.InstructionArtifacts
			failing := true
			sourceConfig.InstructionArtifacts = testReviewInstructionArtifactsFunc(
				func(digest domain.Digest) (io.ReadCloser, error) {
					reader, err := artifacts.Open(digest)
					if err != nil || !failing {
						return reader, err
					}
					body, readErr := io.ReadAll(reader)
					closeErr := reader.Close()
					if readErr != nil || closeErr != nil {
						return nil, errors.Join(readErr, closeErr)
					}
					return tc.fail(body)
				},
			)
			source, err := NewCodexReviewSource(sourceConfig)
			if err != nil {
				t.Fatal(err)
			}
			id := domain.InvocationID(fmt.Sprintf("review-instruction-%d", i))
			request := exec.ReviewRequest{
				RunID: "run-instruction-retry", Round: 1, Repo: seedSpec.Seed.Base.Repo,
				RepositoryID: seedSpec.Seed.Base.RepositoryID, BaseRef: seedSpec.Seed.Base.BaseRef,
				BaseSHA: strings.Repeat("a", 40), HeadSHA: seedSpec.Seed.Base.BaseSHA,
				Workspace: seedSpec.Seed.SourceDir, Verification: testReviewVerificationEvidence(),
				Instructions: requestSpec.InstructionBinding, RequestedAt: codexReviewEpoch,
			}
			var failure *exec.ReviewSourceFailure
			requestErr := source.RequestReview(ctx, id, request)
			if !errors.As(requestErr, &failure) ||
				failure.Class != domain.ReviewFailureTransient {
				t.Fatalf("materialization failure = %v", requestErr)
			}
			if strings.Contains(requestErr.Error(), "persisted review instruction result is invalid") {
				t.Fatalf("artifact read failure was collapsed into divergence: %v", requestErr)
			}
			workspace, err := journal.GetCodexReviewWorkspaceBinding(ctx, string(id))
			if err != nil {
				t.Fatalf("retry workspace binding = %v", err)
			}
			if _, err := fx.rt.InspectVolume(ctx, workspace.Volume); err != nil {
				t.Fatalf("retry workspace was removed: %v", err)
			}
			if journal.intent != nil {
				t.Fatal("credential-bearing review launch began after materialization failure")
			}
			if _, err := os.Stat(source.instructionFile(id)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("partial instruction snapshot survived: %v", err)
			}

			failing = false
			status, err := source.Inspect(ctx, id)
			if err != nil || status != exec.StatusRunning {
				t.Fatalf("retried materialization = %q, %v", status, err)
			}
			source.mu.Lock()
			launch := source.launches[id]
			delete(source.launches, id)
			source.mu.Unlock()
			if launch != nil {
				_ = launch.Close()
			}
			if err := backend.AbortCodexReview(ctx, source.cfg.Review, string(id)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCodexReviewInstructionMaterializationRejectsDivergentSnapshotTopology(t *testing.T) {
	ctx := context.Background()
	fx := newHandoffFixture(t)
	seedSpec := fx.seed(t)
	backend := fx.backend(t)
	cfg, requestSpec := testCodexReview(t)
	journal := &fakeCodexReviewJournal{}
	sourceConfig := codexReviewSourceConfigForTest(t, backend, cfg, requestSpec, journal)
	sourceConfig.InstructionArtifacts = testReviewInstructionArtifactsFunc(
		func(domain.Digest) (io.ReadCloser, error) {
			return nil, &os.PathError{Op: "open", Path: "artifact", Err: syscall.EIO}
		},
	)
	source, err := NewCodexReviewSource(sourceConfig)
	if err != nil {
		t.Fatal(err)
	}
	id := domain.InvocationID("review-instruction-cleanup")
	if _, err := source.writeReviewInstructionFile(id, []byte("partial snapshot")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(source.instructionFile(id)), "extra"),
		[]byte("prevents directory removal"), 0o600); err != nil {
		t.Fatal(err)
	}
	request := exec.ReviewRequest{
		RunID: "run-instruction-cleanup", Round: 1, Repo: seedSpec.Seed.Base.Repo,
		RepositoryID: seedSpec.Seed.Base.RepositoryID, BaseRef: seedSpec.Seed.Base.BaseRef,
		BaseSHA: strings.Repeat("a", 40), HeadSHA: seedSpec.Seed.Base.BaseSHA,
		Workspace: seedSpec.Seed.SourceDir, Verification: testReviewVerificationEvidence(),
		Instructions: requestSpec.InstructionBinding, RequestedAt: codexReviewEpoch,
	}
	var failure *exec.ReviewSourceFailure
	err = source.RequestReview(ctx, id, request)
	if !errors.As(err, &failure) || failure.Class != domain.ReviewFailureContradiction ||
		!errors.Is(err, ErrConformance) {
		t.Fatalf("materialization plus cleanup failure = %v", err)
	}
	if _, err := journal.GetCodexReviewWorkspaceBinding(ctx, string(id)); err != nil {
		t.Fatalf("snapshot contradiction removed unreaped workspace: %v", err)
	}
	if journal.intent != nil {
		t.Fatal("credential-bearing review launch began after cleanup failure")
	}
	failure = nil
	if _, err := source.Inspect(ctx, id); !errors.As(err, &failure) ||
		failure.Class != domain.ReviewFailureContradiction || !errors.Is(err, ErrConformance) {
		t.Fatalf("repeated snapshot divergence = %v", err)
	}
}

func TestCodexReviewInstructionMaterializationFailsClosed(t *testing.T) {
	oversized := bytes.Repeat([]byte("x"), int(domain.MaxVendorInstructionBytes)+1)
	source := &CodexReviewSource{cfg: CodexReviewSourceConfig{
		InstructionArtifacts: testReviewInstructionArtifacts{digestBody(oversized): oversized},
	}}
	if _, err := source.readReviewInstructionArtifact(t.Context(), digestBody(oversized)); classifyCodexInstructionMaterializationFailure(err) != domain.ReviewFailureContradiction ||
		!errors.Is(err, ErrConformance) {
		t.Fatalf("oversized artifact classification = %v", err)
	}
	source.cfg.InstructionArtifacts = testReviewInstructionArtifactsFunc(
		func(domain.Digest) (io.ReadCloser, error) {
			return testReviewInstructionReadCloser{
				Reader: &testReviewInstructionBodyErrorReader{body: slices.Clone(oversized), err: syscall.EIO},
			}, nil
		},
	)
	if _, err := source.readReviewInstructionArtifact(t.Context(), digestBody(oversized)); classifyCodexInstructionMaterializationFailure(err) != domain.ReviewFailureContradiction ||
		!errors.Is(err, ErrConformance) {
		t.Fatalf("oversized artifact plus I/O classification = %v", err)
	}

	tamperedDigest := digestBody([]byte("trusted"))
	source.cfg.InstructionArtifacts = testReviewInstructionArtifactsFunc(
		func(domain.Digest) (io.ReadCloser, error) {
			return testReviewInstructionReadCloser{
				Reader: bytes.NewReader([]byte("tampered")), closeErr: syscall.EIO,
			}, nil
		},
	)
	if _, err := source.readReviewInstructionArtifact(t.Context(), tamperedDigest); classifyCodexInstructionMaterializationFailure(err) != domain.ReviewFailureContradiction ||
		!errors.Is(err, ErrConformance) {
		t.Fatalf("tampered artifact plus close I/O classification = %v", err)
	}

	_, binding, err := exec.ComposeCodexReviewInstructions(exec.ReviewHostInstructionInput{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	binding.ResultDigest = domain.Digest("sha256:" + strings.Repeat("f", 64))
	source.cfg.InstructionArtifacts = testReviewInstructionArtifacts{
		binding.ResultDigest: []byte("different persisted result"),
	}
	if _, err := source.reconstructReviewInstructions(t.Context(), binding); classifyCodexInstructionMaterializationFailure(err) != domain.ReviewFailureContradiction {
		t.Fatalf("composition divergence classification = %v", err)
	}

	for _, err := range []error{
		errTestReviewInstructionArtifactNotFound,
		errors.New("unknown artifact failure"),
		fmt.Errorf("missing: %w", os.ErrNotExist),
	} {
		if got := classifyCodexInstructionMaterializationFailure(err); got != domain.ReviewFailureContradiction {
			t.Fatalf("unrecognized materialization error %v = %q", err, got)
		}
	}
}

func TestCodexReviewInstructionOpenClassification(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"path I/O", &os.PathError{Op: "open", Path: "artifact", Err: syscall.EIO}, true},
		{"raw I/O", syscall.EMFILE, true},
		{"missing path", &os.PathError{Op: "open", Path: "artifact", Err: syscall.ENOENT}, false},
		{"missing sentinel", os.ErrNotExist, false},
		{"unknown", errors.New("artifact unavailable"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := codexReviewInstructionOpenIsOperational(tc.err); got != tc.want {
				t.Fatalf("operational = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCodexReviewInstructionWriteClassification(t *testing.T) {
	missingRoot := filepath.Join(t.TempDir(), "missing")
	source := &CodexReviewSource{cfg: CodexReviewSourceConfig{
		Review: CodexReviewConfig{InputRoot: missingRoot},
	}}
	_, err := source.writeReviewInstructionFile("review-write-io", []byte("trusted"))
	if classifyCodexInstructionMaterializationFailure(err) != domain.ReviewFailureTransient ||
		!errors.Is(err, ErrCodexReviewOperational) {
		t.Fatalf("write I/O classification = %v", err)
	}

	invalidRoot := t.TempDir()
	if err := os.Chmod(invalidRoot, 0o755); err != nil { //nolint:gosec // test makes the owned root non-private
		t.Fatal(err)
	}
	source.cfg.Review.InputRoot = invalidRoot
	_, err = source.writeReviewInstructionFile("review-write-invariant", []byte("trusted"))
	if classifyCodexInstructionMaterializationFailure(err) != domain.ReviewFailureContradiction ||
		errors.Is(err, ErrCodexReviewOperational) {
		t.Fatalf("write invariant classification = %v", err)
	}
}

func testReviewVerificationEvidence() exec.ReviewVerificationEvidence {
	return exec.ReviewVerificationEvidence{
		Outcome:                domain.VerificationPassed,
		RecipeDigest:           domain.Digest("sha256:" + strings.Repeat("c", 64)),
		EvidenceSnapshotDigest: domain.Digest("sha256:" + strings.Repeat("d", 64)),
	}
}

func codexReviewSourceConfigForTest(
	t *testing.T,
	backend *Backend,
	cfg CodexReviewConfig,
	request CodexReviewSpec,
	journal CodexReviewJournal,
) CodexReviewSourceConfig {
	t.Helper()
	leaser, err := NewRuntimeCodexReviewVolumeLeaser(backend.rt)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Journal = journal
	cfg.ProxyURL = ""
	cfg.VolumeLifecycleLeaser = leaser
	sourceConfig := CodexReviewSourceConfig{
		Backend: backend, Review: cfg, Journal: journal, WorkspaceSizeMB: 64,
		AuthMode: request.AuthMode, AuthIdentityID: request.AuthIdentityID,
		AuthSnapshot: request.AuthSnapshot,
		InstructionArtifacts: testReviewInstructionArtifacts{
			request.InstructionBinding.ResultDigest: request.Instructions.Body,
		},
		CostOwner: "subscription:owner", Now: func() time.Time { return codexReviewEpoch },
	}
	sourceConfig.ConfigurationDigest, err = CodexReviewConfigurationDigest(
		cfg, sourceConfig.WorkspaceSizeMB, sourceConfig.AuthMode, sourceConfig.AuthIdentityID,
		sourceConfig.CostOwner,
	)
	if err != nil {
		t.Fatal(err)
	}
	return sourceConfig
}

func TestNewCodexReviewSourceRejectsUninitializedBackend(t *testing.T) {
	fx := newHandoffFixture(t)
	backend := fx.backend(t)
	cfg, request := testCodexReview(t)
	sourceConfig := codexReviewSourceConfigForTest(t, backend, cfg, request, &fakeCodexReviewJournal{})
	sourceConfig.Backend = &Backend{}

	if _, err := NewCodexReviewSource(sourceConfig); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("NewCodexReviewSource(uninitialized backend) = %v, want ErrInvalidConfig", err)
	}
}

func TestCodexReviewSourceRunsWardLifecycleAndCleansBeforePoll(t *testing.T) {
	ctx := context.Background()
	fx := newHandoffFixture(t)
	seedSpec := fx.seed(t)
	backend := fx.backend(t)
	cfg, requestSpec := testCodexReview(t)
	journal := &fakeCodexReviewJournal{}
	sourceConfig := codexReviewSourceConfigForTest(t, backend, cfg, requestSpec, journal)
	source, err := NewCodexReviewSource(sourceConfig)
	if err != nil {
		t.Fatal(err)
	}
	id := domain.InvocationID("review-run-1-1")
	request := exec.ReviewRequest{
		RunID: "run-1", Round: 1, Repo: seedSpec.Seed.Base.Repo,
		RepositoryID: seedSpec.Seed.Base.RepositoryID, BaseRef: seedSpec.Seed.Base.BaseRef,
		BaseSHA: strings.Repeat("a", 40), HeadSHA: seedSpec.Seed.Base.BaseSHA,
		Workspace: seedSpec.Seed.SourceDir, Verification: testReviewVerificationEvidence(), Instructions: testReviewInstructionBinding(),
		RequestedAt: codexReviewEpoch.Add(-time.Minute),
	}
	if err := source.RequestReview(ctx, id, request); err != nil {
		t.Fatal(err)
	}
	resultArchive := buildTar(t, []tarEntry{
		{name: strings.TrimPrefix(codexReviewStatusPath, "/"), body: []byte("0\n")},
		{name: strings.TrimPrefix(codexReviewEventsPath, "/"), body: []byte("terminal\n")},
		{name: strings.TrimPrefix(codexReviewResultPath, "/"), body: []byte(`{"findings":[]}`)},
	})
	fx.rt.exportTarPath = resultArchive
	state, err := backend.InspectCodexReview(ctx, sourceConfig.Review, string(id))
	if err != nil {
		t.Fatal(err)
	}
	if state == StateRunning {
		state, err = backend.InspectCodexReview(ctx, sourceConfig.Review, string(id))
	}
	if err != nil || state != StateStopped {
		t.Fatalf("review runtime state = %q, %v", state, err)
	}
	collection, err := backend.CollectCodexReview(ctx, sourceConfig.Review, string(id))
	if err != nil {
		t.Fatal(err)
	}
	outcome := source.normalizeCollection(id, request, collection)
	if err := journal.PutCodexReviewOutcome(ctx, string(id), outcome); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash after durable collection but before cleanup. The new
	// source has no live launch handle and must finish teardown from journals.
	source.mu.Lock()
	_ = source.launches[id].Close()
	delete(source.launches, id)
	source.mu.Unlock()
	// Refute the destructive cleanup boundary too: model a first cleanup that
	// deleted the container and shadow volume, then crashed before the
	// workspace, network, journal transition, or lease release.
	binding, err := journal.GetCodexReviewBinding(ctx, string(id))
	if err != nil {
		t.Fatal(err)
	}
	intent, err := journal.GetCodexReviewIntent(ctx, string(id))
	if err != nil {
		t.Fatal(err)
	}
	if err := fx.rt.DeleteContainer(ctx, binding.ReviewContainer); err != nil {
		t.Fatal(err)
	}
	if err := fx.rt.DeleteVolume(ctx, intent.ShadowVolume); err != nil {
		t.Fatal(err)
	}
	restartedConfig := source.cfg
	restartedConfig.Review.VolumeLifecycleLeaser, err = NewRuntimeCodexReviewVolumeLeaser(fx.rt)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := NewCodexReviewSource(restartedConfig)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Poll(ctx, id); !errors.Is(err, exec.ErrResultNotReady) {
		t.Fatalf("poll before cleanup = %v", err)
	}
	cleanupFailure := errors.New("runtime cleanup temporarily unavailable")
	failedCleanup := false
	fx.rt.onDeleteVolume = func(name string) (bool, error) {
		if name == binding.WorkspaceVolume && !failedCleanup {
			failedCleanup = true
			return true, cleanupFailure
		}
		return false, nil
	}
	status, err := restarted.Inspect(ctx, id)
	if err != nil || status != exec.StatusPending {
		t.Fatalf("transient cleanup status = %q, %v", status, err)
	}
	if _, err := restarted.Poll(ctx, id); !errors.Is(err, exec.ErrResultNotReady) {
		t.Fatalf("poll after transient cleanup = %v", err)
	}
	fx.rt.onDeleteVolume = nil
	status, err = restarted.Inspect(ctx, id)
	if err != nil || status != exec.StatusCompleted {
		t.Fatalf("restart cleanup status = %q, %v", status, err)
	}
	result, err := restarted.Poll(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if result.BaseSHA != request.BaseSHA || result.HeadSHA != request.HeadSHA || len(result.Findings) != 0 {
		t.Fatalf("review result = %#v", result)
	}
	if err := restarted.Verify(ctx, id, request.BaseSHA, request.HeadSHA); err != nil {
		t.Fatal(err)
	}
	containers, _ := fx.rt.ListContainers(ctx)
	volumes, _ := fx.rt.ListVolumes(ctx)
	networks, _ := fx.rt.ListNetworks(ctx)
	if len(containers) != 0 || len(volumes) != 0 || len(networks) != 0 {
		t.Fatalf("review topology leaked: containers=%v volumes=%v networks=%v", containers, volumes, networks)
	}
	if _, err := os.Stat(resultArchive); err != nil {
		t.Fatal(err)
	}
}

func TestCodexReviewSourcePersistsInvalidCollectedResultAndCleans(t *testing.T) {
	ctx := context.Background()
	fx := newHandoffFixture(t)
	seedSpec := fx.seed(t)
	backend := fx.backend(t)
	cfg, requestSpec := testCodexReview(t)
	journal := &fakeCodexReviewJournal{}
	sourceConfig := codexReviewSourceConfigForTest(t, backend, cfg, requestSpec, journal)
	source, err := NewCodexReviewSource(sourceConfig)
	if err != nil {
		t.Fatal(err)
	}
	id := domain.InvocationID("review-run-1-1")
	request := exec.ReviewRequest{
		RunID: "run-1", Round: 1, Repo: seedSpec.Seed.Base.Repo,
		RepositoryID: seedSpec.Seed.Base.RepositoryID, BaseRef: seedSpec.Seed.Base.BaseRef,
		BaseSHA: strings.Repeat("a", 40), HeadSHA: seedSpec.Seed.Base.BaseSHA,
		Workspace: seedSpec.Seed.SourceDir, Verification: testReviewVerificationEvidence(), Instructions: testReviewInstructionBinding(),
		RequestedAt: codexReviewEpoch.Add(-time.Minute),
	}
	if err := source.RequestReview(ctx, id, request); err != nil {
		t.Fatal(err)
	}
	// Schema-valid but semantically invalid: two identical findings normalize
	// to a duplicated finding identity, which the result contract rejects. The
	// contradiction must still persist durably and finish authenticated
	// cleanup instead of terminalizing around a leaked topology.
	duplicated := `{"findings":[` +
		`{"severity":"P1","location":"daemon/main.go:12","explanation":"unsafe transition"},` +
		`{"severity":"P1","location":"daemon/main.go:12","explanation":"unsafe transition"}]}`
	resultArchive := buildTar(t, []tarEntry{
		{name: strings.TrimPrefix(codexReviewStatusPath, "/"), body: []byte("0\n")},
		{name: strings.TrimPrefix(codexReviewEventsPath, "/"), body: []byte("terminal\n")},
		{name: strings.TrimPrefix(codexReviewResultPath, "/"), body: []byte(duplicated)},
	})
	fx.rt.exportTarPath = resultArchive
	status, err := source.Inspect(ctx, id)
	for err == nil && status == exec.StatusRunning {
		status, err = source.Inspect(ctx, id)
	}
	if err != nil || status != exec.StatusFailed {
		t.Fatalf("invalid collected result status = %q, %v", status, err)
	}
	outcome, ready, err := journal.GetCodexReviewOutcome(ctx, string(id))
	if err != nil || !ready || outcome.Result != nil ||
		outcome.FailureClass != domain.ReviewFailureContradiction ||
		!strings.Contains(outcome.Failure, "invalid collected result") {
		t.Fatalf("persisted outcome = %#v, ready=%v, %v", outcome, ready, err)
	}
	var failure *exec.ReviewSourceFailure
	if _, err := source.Poll(ctx, id); !errors.As(err, &failure) ||
		failure.Class != domain.ReviewFailureContradiction {
		t.Fatalf("poll after invalid collection = %v", err)
	}
	containers, _ := fx.rt.ListContainers(ctx)
	volumes, _ := fx.rt.ListVolumes(ctx)
	networks, _ := fx.rt.ListNetworks(ctx)
	if len(containers) != 0 || len(volumes) != 0 || len(networks) != 0 {
		t.Fatalf("invalid-result topology leaked: containers=%v volumes=%v networks=%v",
			containers, volumes, networks)
	}
}

func TestCodexReviewSourcePersistsMalformedRawOutputAndCleans(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries []tarEntry
	}{
		{
			name: "missing status",
			entries: []tarEntry{
				{name: strings.TrimPrefix(codexReviewEventsPath, "/"), body: []byte("terminal\n")},
			},
		},
		{
			name: "invalid status",
			entries: []tarEntry{
				{name: strings.TrimPrefix(codexReviewStatusPath, "/"), body: []byte("invalid\n")},
				{name: strings.TrimPrefix(codexReviewEventsPath, "/"), body: []byte("terminal\n")},
			},
		},
		{
			name: "missing events",
			entries: []tarEntry{
				{name: strings.TrimPrefix(codexReviewStatusPath, "/"), body: []byte("0\n")},
				{name: strings.TrimPrefix(codexReviewResultPath, "/"), body: []byte(`{"findings":[]}`)},
			},
		},
		{
			name: "missing result",
			entries: []tarEntry{
				{name: strings.TrimPrefix(codexReviewStatusPath, "/"), body: []byte("0\n")},
				{name: strings.TrimPrefix(codexReviewEventsPath, "/"), body: []byte("terminal\n")},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			fx := newHandoffFixture(t)
			seedSpec := fx.seed(t)
			backend := fx.backend(t)
			cfg, requestSpec := testCodexReview(t)
			journal := &fakeCodexReviewJournal{}
			sourceConfig := codexReviewSourceConfigForTest(t, backend, cfg, requestSpec, journal)
			source, err := NewCodexReviewSource(sourceConfig)
			if err != nil {
				t.Fatal(err)
			}
			id := domain.InvocationID("review-run-raw-1")
			request := exec.ReviewRequest{
				RunID: "run-raw", Round: 1, Repo: seedSpec.Seed.Base.Repo,
				RepositoryID: seedSpec.Seed.Base.RepositoryID, BaseRef: seedSpec.Seed.Base.BaseRef,
				BaseSHA: strings.Repeat("a", 40), HeadSHA: seedSpec.Seed.Base.BaseSHA,
				Workspace: seedSpec.Seed.SourceDir, Verification: testReviewVerificationEvidence(), Instructions: testReviewInstructionBinding(),
				RequestedAt: codexReviewEpoch.Add(-time.Minute),
			}
			if err := source.RequestReview(ctx, id, request); err != nil {
				t.Fatal(err)
			}
			fx.rt.exportTarPath = buildTar(t, tc.entries)
			binding, err := journal.GetCodexReviewBinding(ctx, string(id))
			if err != nil {
				t.Fatal(err)
			}
			failedCleanup := false
			fx.rt.onDeleteVolume = func(name string) (bool, error) {
				if name == binding.WorkspaceVolume && !failedCleanup {
					failedCleanup = true
					return true, errors.New("runtime cleanup temporarily unavailable")
				}
				return false, nil
			}
			status, err := source.Inspect(ctx, id)
			for attempts := 0; err == nil && status != exec.StatusFailed && attempts < 5; attempts++ {
				status, err = source.Inspect(ctx, id)
			}
			if err != nil || status != exec.StatusFailed {
				t.Fatalf("malformed raw output status = %q, %v", status, err)
			}
			outcome, ready, err := journal.GetCodexReviewOutcome(ctx, string(id))
			if err != nil || !ready || outcome.Result != nil ||
				outcome.FailureClass != domain.ReviewFailureContradiction ||
				!strings.Contains(outcome.Failure, "invalid raw output") {
				t.Fatalf("persisted malformed-output outcome = %#v, ready=%v, %v", outcome, ready, err)
			}
			containers, _ := fx.rt.ListContainers(ctx)
			volumes, _ := fx.rt.ListVolumes(ctx)
			networks, _ := fx.rt.ListNetworks(ctx)
			if len(containers) != 0 || len(volumes) != 0 || len(networks) != 0 {
				t.Fatalf("malformed-output topology leaked: containers=%v volumes=%v networks=%v",
					containers, volumes, networks)
			}
		})
	}
}

func TestCodexReviewSourceRetriesOperationalBindingRead(t *testing.T) {
	ctx := context.Background()
	fx := newHandoffFixture(t)
	seedSpec := fx.seed(t)
	backend := fx.backend(t)
	cfg, requestSpec := testCodexReview(t)
	journal := &fakeCodexReviewJournal{}
	sourceConfig := codexReviewSourceConfigForTest(t, backend, cfg, requestSpec, journal)
	source, err := NewCodexReviewSource(sourceConfig)
	if err != nil {
		t.Fatal(err)
	}
	id := domain.InvocationID("review-run-binding-read-1")
	request := exec.ReviewRequest{
		RunID: "run-binding-read", Round: 1, Repo: seedSpec.Seed.Base.Repo,
		RepositoryID: seedSpec.Seed.Base.RepositoryID, BaseRef: seedSpec.Seed.Base.BaseRef,
		BaseSHA: strings.Repeat("a", 40), HeadSHA: seedSpec.Seed.Base.BaseSHA,
		Workspace: seedSpec.Seed.SourceDir, Verification: testReviewVerificationEvidence(), Instructions: testReviewInstructionBinding(),
		RequestedAt: codexReviewEpoch.Add(-time.Minute),
	}
	if err := source.RequestReview(ctx, id, request); err != nil {
		t.Fatal(err)
	}
	journal.failGetBinding = errors.New("SQLite temporarily unavailable")
	var failure *exec.ReviewSourceFailure
	if _, err := source.Inspect(ctx, id); !errors.As(err, &failure) ||
		failure.Class != domain.ReviewFailureTransient ||
		!errors.Is(failure.Err, ErrCodexReviewOperational) {
		t.Fatalf("operational binding read = %v", err)
	}
	if len(fx.rt.ctrs) == 0 {
		t.Fatal("operational binding read tore down the retryable review topology")
	}
	journal.failGetBinding = errors.Join(ErrConformance, errors.New("decoded binding is invalid"))
	if _, err := source.Inspect(ctx, id); !errors.As(err, &failure) ||
		failure.Class != domain.ReviewFailureContradiction ||
		errors.Is(failure.Err, ErrCodexReviewOperational) {
		t.Fatalf("rejected binding row = %v", err)
	}
	if len(fx.rt.ctrs) == 0 {
		t.Fatal("rejected binding row tore down unauthenticated topology")
	}
	journal.failGetBinding = nil
	if _, err := source.Inspect(ctx, id); err != nil {
		t.Fatalf("binding read retry = %v", err)
	}
}

func TestCodexReviewSourceCleansBeforeRejectingMalformedOutcome(t *testing.T) {
	ctx := context.Background()
	fx := newHandoffFixture(t)
	seedSpec := fx.seed(t)
	backend := fx.backend(t)
	cfg, requestSpec := testCodexReview(t)
	journal := &fakeCodexReviewJournal{}
	sourceConfig := codexReviewSourceConfigForTest(t, backend, cfg, requestSpec, journal)
	source, err := NewCodexReviewSource(sourceConfig)
	if err != nil {
		t.Fatal(err)
	}
	id := domain.InvocationID("review-run-outcome-row-1")
	request := exec.ReviewRequest{
		RunID: "run-outcome-row", Round: 1, Repo: seedSpec.Seed.Base.Repo,
		RepositoryID: seedSpec.Seed.Base.RepositoryID, BaseRef: seedSpec.Seed.Base.BaseRef,
		BaseSHA: strings.Repeat("a", 40), HeadSHA: seedSpec.Seed.Base.BaseSHA,
		Workspace: seedSpec.Seed.SourceDir, Verification: testReviewVerificationEvidence(), Instructions: testReviewInstructionBinding(),
		RequestedAt: codexReviewEpoch.Add(-time.Minute),
	}
	if err := source.RequestReview(ctx, id, request); err != nil {
		t.Fatal(err)
	}
	if err := journal.PutCodexReviewOutcome(ctx, string(id), CodexReviewSourceOutcome{
		InvocationID: id, FailureClass: domain.ReviewFailureContradiction,
		Failure: "collected outcome was corrupted before cleanup", AbortRequired: true,
	}); err != nil {
		t.Fatal(err)
	}
	binding, err := journal.GetCodexReviewBinding(ctx, string(id))
	if err != nil {
		t.Fatal(err)
	}
	failedCleanup := false
	fx.rt.onDeleteVolume = func(name string) (bool, error) {
		if name == binding.WorkspaceVolume && !failedCleanup {
			failedCleanup = true
			return true, errors.New("runtime cleanup temporarily unavailable")
		}
		return false, nil
	}
	journal.failGetOutcome = errors.Join(
		ErrCodexReviewOutcomeRejected, errors.New("decode persisted outcome"))
	var failure *exec.ReviewSourceFailure
	if status, err := source.Inspect(ctx, id); err != nil || status != exec.StatusPending {
		t.Fatalf("malformed outcome cleanup retry = %q, %v", status, err)
	}
	if _, err := source.Inspect(ctx, id); !errors.As(err, &failure) ||
		failure.Class != domain.ReviewFailureContradiction ||
		!errors.Is(failure.Err, ErrCodexReviewOutcomeRejected) {
		t.Fatalf("malformed persisted outcome = %v", err)
	}
	if !journal.ready[string(id)] {
		t.Fatal("malformed persisted outcome was not marked ready after cleanup")
	}
	containers, _ := fx.rt.ListContainers(ctx)
	volumes, _ := fx.rt.ListVolumes(ctx)
	networks, _ := fx.rt.ListNetworks(ctx)
	if len(containers) != 0 || len(volumes) != 0 || len(networks) != 0 {
		t.Fatalf("malformed-outcome topology leaked: containers=%v volumes=%v networks=%v",
			containers, volumes, networks)
	}
}

func TestCodexReviewSourceAbortsStartedInvocationForRejectedRequest(t *testing.T) {
	ctx := context.Background()
	fx := newHandoffFixture(t)
	seedSpec := fx.seed(t)
	backend := fx.backend(t)
	cfg, requestSpec := testCodexReview(t)
	journal := &fakeCodexReviewJournal{}
	sourceConfig := codexReviewSourceConfigForTest(t, backend, cfg, requestSpec, journal)
	source, err := NewCodexReviewSource(sourceConfig)
	if err != nil {
		t.Fatal(err)
	}
	id := domain.InvocationID("review-run-rejected-1")
	request := exec.ReviewRequest{
		RunID: "run-rejected", Round: 1, Repo: seedSpec.Seed.Base.Repo,
		RepositoryID: seedSpec.Seed.Base.RepositoryID, BaseRef: seedSpec.Seed.Base.BaseRef,
		BaseSHA: strings.Repeat("a", 40), HeadSHA: seedSpec.Seed.Base.BaseSHA,
		Workspace: seedSpec.Seed.SourceDir, Verification: testReviewVerificationEvidence(), Instructions: testReviewInstructionBinding(),
		RequestedAt: codexReviewEpoch.Add(-time.Minute),
	}
	if err := source.RequestReview(ctx, id, request); err != nil {
		t.Fatal(err)
	}
	authority, err := request.AuthorityDigest()
	if err != nil {
		t.Fatal(err)
	}
	if err := source.VerifyRequestAuthority(ctx, id, authority); err != nil {
		t.Fatalf("authentic request authority = %v", err)
	}
	// Rewrite the persisted request to a still-valid body for another head:
	// exactly the tamper the engine's pre-Inspect gate rejects. The rejection
	// must abort the review the original request already started instead of
	// stranding the credential-bearing topology behind the terminal
	// contradiction.
	rewritten := request
	rewritten.HeadSHA = strings.Repeat("b", 40)
	if err := journal.PutCodexReviewRequest(ctx, string(id), rewritten); err != nil {
		t.Fatal(err)
	}
	intent, err := journal.GetCodexReviewIntent(ctx, string(id))
	if err != nil {
		t.Fatal(err)
	}
	// The first rejection meets a failing runtime: the abort stays durable and
	// transient, then converges on the retry instead of leaking.
	failedCleanup := false
	fx.rt.onDeleteContainer = func(container string) (bool, error) {
		if container == intent.ReviewContainer && !failedCleanup {
			failedCleanup = true
			return true, errors.New("runtime cleanup temporarily unavailable")
		}
		return false, nil
	}
	var failure *exec.ReviewSourceFailure
	if err := source.VerifyRequestAuthority(ctx, id, authority); !errors.As(err, &failure) ||
		failure.Class != domain.ReviewFailureTransient {
		t.Fatalf("rejection with failing teardown = %v", err)
	}
	outcome, ready, err := journal.GetCodexReviewOutcome(ctx, string(id))
	if err != nil || ready || !outcome.AbortRequired ||
		outcome.FailureClass != domain.ReviewFailureContradiction ||
		!strings.Contains(outcome.Failure, "rejected after launch") {
		t.Fatalf("persisted rejection outcome = %#v, ready=%v, %v", outcome, ready, err)
	}
	fx.rt.onDeleteContainer = nil
	if err := source.VerifyRequestAuthority(ctx, id, authority); !errors.As(err, &failure) ||
		failure.Class != domain.ReviewFailureContradiction ||
		!errors.Is(failure.Err, domain.ErrParentKeyMismatch) {
		t.Fatalf("rejection after teardown = %v", err)
	}
	if _, ready, err = journal.GetCodexReviewOutcome(ctx, string(id)); err != nil || !ready {
		t.Fatalf("rejection outcome ready = %v, %v", ready, err)
	}
	containers, _ := fx.rt.ListContainers(ctx)
	volumes, _ := fx.rt.ListVolumes(ctx)
	networks, _ := fx.rt.ListNetworks(ctx)
	if len(containers) != 0 || len(volumes) != 0 || len(networks) != 0 {
		t.Fatalf("rejected topology leaked: containers=%v volumes=%v networks=%v",
			containers, volumes, networks)
	}
}

func TestCodexReviewSourceAbortsPreHandoffLaunchForRejectedRequest(t *testing.T) {
	ctx := context.Background()
	fx := newHandoffFixture(t)
	seedSpec := fx.seed(t)
	backend := fx.backend(t)
	cfg, requestSpec := testCodexReview(t)
	journal := &fakeCodexReviewJournal{}
	sourceConfig := codexReviewSourceConfigForTest(t, backend, cfg, requestSpec, journal)
	source, err := NewCodexReviewSource(sourceConfig)
	if err != nil {
		t.Fatal(err)
	}
	id := domain.InvocationID("review-run-rejected-3")
	request := exec.ReviewRequest{
		RunID: "run-rejected", Round: 1, Repo: seedSpec.Seed.Base.Repo,
		RepositoryID: seedSpec.Seed.Base.RepositoryID, BaseRef: seedSpec.Seed.Base.BaseRef,
		BaseSHA: strings.Repeat("a", 40), HeadSHA: seedSpec.Seed.Base.BaseSHA,
		Workspace: seedSpec.Seed.SourceDir, Verification: testReviewVerificationEvidence(), Instructions: testReviewInstructionBinding(),
		RequestedAt: codexReviewEpoch.Add(-time.Minute),
	}
	if err := source.RequestReview(ctx, id, request); err != nil {
		t.Fatal(err)
	}
	authority, err := request.AuthorityDigest()
	if err != nil {
		t.Fatal(err)
	}
	rewritten := request
	rewritten.HeadSHA = strings.Repeat("b", 40)
	if err := journal.PutCodexReviewRequest(ctx, string(id), rewritten); err != nil {
		t.Fatal(err)
	}
	// A crash between container start and the started handoff record leaves
	// the intent in `starting` with the durable binding already persisted.
	// The rejection must still abort through that binding rather than
	// stranding the running review.
	journal.intent.State = CodexReviewIntentStarting
	var failure *exec.ReviewSourceFailure
	if err := source.VerifyRequestAuthority(ctx, id, authority); !errors.As(err, &failure) ||
		failure.Class != domain.ReviewFailureContradiction ||
		!errors.Is(failure.Err, domain.ErrParentKeyMismatch) {
		t.Fatalf("pre-handoff rejection = %v", err)
	}
	outcome, ready, err := journal.GetCodexReviewOutcome(ctx, string(id))
	if err != nil || !ready || !outcome.AbortRequired ||
		outcome.FailureClass != domain.ReviewFailureContradiction {
		t.Fatalf("pre-handoff rejection outcome = %#v, ready=%v, %v", outcome, ready, err)
	}
	containers, _ := fx.rt.ListContainers(ctx)
	volumes, _ := fx.rt.ListVolumes(ctx)
	networks, _ := fx.rt.ListNetworks(ctx)
	if len(containers) != 0 || len(volumes) != 0 || len(networks) != 0 {
		t.Fatalf("pre-handoff rejected topology leaked: containers=%v volumes=%v networks=%v",
			containers, volumes, networks)
	}
}

func TestCodexReviewSourceRejectedRequestFencesAndRecoversBeforeBinding(t *testing.T) {
	ctx := context.Background()
	fx := newHandoffFixture(t)
	seedSpec := fx.seed(t)
	backend := fx.backend(t)
	cfg, requestSpec := testCodexReview(t)
	journal := &fakeCodexReviewJournal{}
	sourceConfig := codexReviewSourceConfigForTest(t, backend, cfg, requestSpec, journal)
	source, err := NewCodexReviewSource(sourceConfig)
	if err != nil {
		t.Fatal(err)
	}
	id := domain.InvocationID("review-run-rejected-4")
	request := exec.ReviewRequest{
		RunID: "run-rejected", Round: 1, Repo: seedSpec.Seed.Base.Repo,
		RepositoryID: seedSpec.Seed.Base.RepositoryID, BaseRef: seedSpec.Seed.Base.BaseRef,
		BaseSHA: strings.Repeat("a", 40), HeadSHA: seedSpec.Seed.Base.BaseSHA,
		Workspace: seedSpec.Seed.SourceDir, Verification: testReviewVerificationEvidence(), Instructions: testReviewInstructionBinding(),
		RequestedAt: codexReviewEpoch.Add(-time.Minute),
	}
	if err := source.RequestReview(ctx, id, request); err != nil {
		t.Fatal(err)
	}
	authority, err := request.AuthorityDigest()
	if err != nil {
		t.Fatal(err)
	}
	rewritten := request
	rewritten.HeadSHA = strings.Repeat("b", 40)
	if err := journal.PutCodexReviewRequest(ctx, string(id), rewritten); err != nil {
		t.Fatal(err)
	}
	// A rejection before the binding is durable fences the prepared transition
	// through the outcome row, then cleans the pre-start topology from the
	// independently authenticated intent.
	journal.intent.State = CodexReviewIntentPreparing
	journal.binding = CodexReviewJournalBinding{}
	var failure *exec.ReviewSourceFailure
	err = source.VerifyRequestAuthority(ctx, id, authority)
	if !errors.As(err, &failure) || failure.Class != domain.ReviewFailureContradiction ||
		!errors.Is(failure.Err, domain.ErrParentKeyMismatch) {
		t.Fatalf("pre-binding rejection = %v", err)
	}
	if outcome, ready, err := journal.GetCodexReviewOutcome(ctx, string(id)); err != nil ||
		!ready || !outcome.AbortRequired {
		t.Fatalf("pre-binding rejection outcome = %#v, ready=%v, %v", outcome, ready, err)
	}
	containers, _ := fx.rt.ListContainers(ctx)
	volumes, _ := fx.rt.ListVolumes(ctx)
	networks, _ := fx.rt.ListNetworks(ctx)
	if len(containers) != 0 || len(volumes) != 0 || len(networks) != 0 {
		t.Fatalf("pre-binding rejected topology leaked: containers=%v volumes=%v networks=%v",
			containers, volumes, networks)
	}
}

func TestCodexReviewSourceRejectedPreparingRequestAbortsWhenBindingExists(t *testing.T) {
	ctx := context.Background()
	fx := newHandoffFixture(t)
	seedSpec := fx.seed(t)
	backend := fx.backend(t)
	cfg, requestSpec := testCodexReview(t)
	journal := &fakeCodexReviewJournal{}
	sourceConfig := codexReviewSourceConfigForTest(t, backend, cfg, requestSpec, journal)
	source, err := NewCodexReviewSource(sourceConfig)
	if err != nil {
		t.Fatal(err)
	}
	id := domain.InvocationID("review-run-rejected-binding")
	request := exec.ReviewRequest{
		RunID: "run-rejected", Round: 1, Repo: seedSpec.Seed.Base.Repo,
		RepositoryID: seedSpec.Seed.Base.RepositoryID, BaseRef: seedSpec.Seed.Base.BaseRef,
		BaseSHA: strings.Repeat("a", 40), HeadSHA: seedSpec.Seed.Base.BaseSHA,
		Workspace: seedSpec.Seed.SourceDir, Verification: testReviewVerificationEvidence(), Instructions: testReviewInstructionBinding(),
		RequestedAt: codexReviewEpoch.Add(-time.Minute),
	}
	if err := source.RequestReview(ctx, id, request); err != nil {
		t.Fatal(err)
	}
	authority, err := request.AuthorityDigest()
	if err != nil {
		t.Fatal(err)
	}
	rewritten := request
	rewritten.HeadSHA = strings.Repeat("b", 40)
	if err := journal.PutCodexReviewRequest(ctx, string(id), rewritten); err != nil {
		t.Fatal(err)
	}
	// #605 keeps the intent preparing through final reconstruction, after the
	// binding is durable. The rejection outcome fences preparation while the
	// intent and binding authenticate abort cleanup.
	journal.intent.State = CodexReviewIntentPreparing
	releaseRun, err := backend.acquireCodexReviewRun(ctx, string(id))
	if err != nil {
		t.Fatal(err)
	}
	blockedCtx, cancel := context.WithCancel(ctx)
	cancel()
	var failure *exec.ReviewSourceFailure
	err = source.VerifyRequestAuthority(blockedCtx, id, authority)
	if !errors.As(err, &failure) || failure.Class != domain.ReviewFailureTransient ||
		!errors.Is(failure.Err, ErrCodexReviewOperational) {
		t.Fatalf("rejection cleanup while launch active = %v, want transient wait", err)
	}
	outcome, ready, err := journal.GetCodexReviewOutcome(ctx, string(id))
	if err != nil || ready || !outcome.AbortRequired {
		t.Fatalf("blocked rejection outcome = %#v, ready=%v, %v", outcome, ready, err)
	}
	containers, _ := fx.rt.ListContainers(ctx)
	if len(containers) == 0 {
		t.Fatal("rejection cleanup ran before the active launch gate released")
	}
	releaseRun()

	failure = nil
	err = source.VerifyRequestAuthority(ctx, id, authority)
	if !errors.As(err, &failure) || failure.Class != domain.ReviewFailureContradiction ||
		!errors.Is(failure.Err, domain.ErrParentKeyMismatch) {
		t.Fatalf("binding-present preparing rejection = %v", err)
	}
	outcome, ready, err = journal.GetCodexReviewOutcome(ctx, string(id))
	if err != nil || !ready || !outcome.AbortRequired ||
		outcome.FailureClass != domain.ReviewFailureContradiction {
		t.Fatalf("binding-present rejection outcome = %#v, ready=%v, %v", outcome, ready, err)
	}
	containers, _ = fx.rt.ListContainers(ctx)
	volumes, _ := fx.rt.ListVolumes(ctx)
	networks, _ := fx.rt.ListNetworks(ctx)
	if len(containers) != 0 || len(volumes) != 0 || len(networks) != 0 {
		t.Fatalf("binding-present rejected topology leaked: containers=%v volumes=%v networks=%v",
			containers, volumes, networks)
	}
}

func TestCodexReviewSourceInspectAbortsInvocationForInvalidPersistedRequest(t *testing.T) {
	ctx := context.Background()
	fx := newHandoffFixture(t)
	seedSpec := fx.seed(t)
	backend := fx.backend(t)
	cfg, requestSpec := testCodexReview(t)
	journal := &fakeCodexReviewJournal{}
	sourceConfig := codexReviewSourceConfigForTest(t, backend, cfg, requestSpec, journal)
	source, err := NewCodexReviewSource(sourceConfig)
	if err != nil {
		t.Fatal(err)
	}
	id := domain.InvocationID("review-run-rejected-5")
	request := exec.ReviewRequest{
		RunID: "run-rejected", Round: 1, Repo: seedSpec.Seed.Base.Repo,
		RepositoryID: seedSpec.Seed.Base.RepositoryID, BaseRef: seedSpec.Seed.Base.BaseRef,
		BaseSHA: strings.Repeat("a", 40), HeadSHA: seedSpec.Seed.Base.BaseSHA,
		Workspace: seedSpec.Seed.SourceDir, Verification: testReviewVerificationEvidence(), Instructions: testReviewInstructionBinding(),
		RequestedAt: codexReviewEpoch.Add(-time.Minute),
	}
	if err := source.RequestReview(ctx, id, request); err != nil {
		t.Fatal(err)
	}
	// Model the production adapter rejecting a corrupt decoded row before it can
	// return a ReviewRequest. Inspect must route that sentinel through the same
	// authenticated teardown used for a validly decoded authority mismatch.
	journal.failGetRequest = errors.Join(
		ErrCodexReviewRequestRejected, errors.New("decode persisted request"))
	var failure *exec.ReviewSourceFailure
	if _, err := source.Inspect(ctx, id); !errors.As(err, &failure) ||
		failure.Class != domain.ReviewFailureContradiction {
		t.Fatalf("inspect of invalid persisted request = %v", err)
	}
	outcome, ready, err := journal.GetCodexReviewOutcome(ctx, string(id))
	if err != nil || !ready || !outcome.AbortRequired ||
		outcome.FailureClass != domain.ReviewFailureContradiction {
		t.Fatalf("inspect rejection outcome = %#v, ready=%v, %v", outcome, ready, err)
	}
	containers, _ := fx.rt.ListContainers(ctx)
	volumes, _ := fx.rt.ListVolumes(ctx)
	networks, _ := fx.rt.ListNetworks(ctx)
	if len(containers) != 0 || len(volumes) != 0 || len(networks) != 0 {
		t.Fatalf("inspect-rejected topology leaked: containers=%v volumes=%v networks=%v",
			containers, volumes, networks)
	}
}

func TestCodexReviewSourcePreservesLegacyRequestThroughRejectedOutcome(t *testing.T) {
	ctx := context.Background()
	fx := newHandoffFixture(t)
	seedSpec := fx.seed(t)
	backend := fx.backend(t)
	cfg, requestSpec := testCodexReview(t)
	journal := &fakeCodexReviewJournal{}
	source, err := NewCodexReviewSource(
		codexReviewSourceConfigForTest(t, backend, cfg, requestSpec, journal),
	)
	if err != nil {
		t.Fatal(err)
	}
	id := domain.InvocationID("review-legacy-rejected-outcome")
	request := exec.ReviewRequest{
		RunID: "run-rejected", Round: 1, Repo: seedSpec.Seed.Base.Repo,
		RepositoryID: seedSpec.Seed.Base.RepositoryID, BaseRef: seedSpec.Seed.Base.BaseRef,
		BaseSHA: strings.Repeat("a", 40), HeadSHA: seedSpec.Seed.Base.BaseSHA,
		Workspace: seedSpec.Seed.SourceDir, Verification: testReviewVerificationEvidence(), Instructions: testReviewInstructionBinding(),
		RequestedAt: codexReviewEpoch.Add(-time.Minute),
	}
	if err := source.RequestReview(ctx, id, request); err != nil {
		t.Fatal(err)
	}
	journal.failGetRequest = errors.Join(ErrCodexReviewRequestRejected, exec.ErrLegacyReviewRequest)
	journal.failGetOutcome = ErrCodexReviewOutcomeRejected
	if err := source.VerifyReviewRequestSupersession(ctx, id, request); !errors.Is(err, exec.ErrLegacyReviewRequest) {
		t.Fatalf("rejected legacy request outcome = %v, want ErrLegacyReviewRequest", err)
	}
}

func TestCodexReviewSourceReapsPreparedWorkspaceForRejectedUnstartedRequest(t *testing.T) {
	ctx := context.Background()
	fx := newHandoffFixture(t)
	seedSpec := fx.seed(t)
	backend := fx.backend(t)
	cfg, requestSpec := testCodexReview(t)
	journal := &fakeCodexReviewJournal{}
	sourceConfig := codexReviewSourceConfigForTest(t, backend, cfg, requestSpec, journal)
	source, err := NewCodexReviewSource(sourceConfig)
	if err != nil {
		t.Fatal(err)
	}
	id := domain.InvocationID("review-run-rejected-2")
	request := exec.ReviewRequest{
		RunID: "run-rejected", Round: 1, Repo: seedSpec.Seed.Base.Repo,
		RepositoryID: seedSpec.Seed.Base.RepositoryID, BaseRef: seedSpec.Seed.Base.BaseRef,
		BaseSHA: strings.Repeat("a", 40), HeadSHA: seedSpec.Seed.Base.BaseSHA,
		Workspace: seedSpec.Seed.SourceDir, Verification: testReviewVerificationEvidence(), Instructions: testReviewInstructionBinding(),
		RequestedAt: codexReviewEpoch.Add(-time.Minute),
	}
	// Persist and prepare without ever launching: the window between workspace
	// preparation and container start. A rejection here durably fences any
	// concurrent launch before reaping the prepared volume.
	if err := journal.PutCodexReviewRequest(ctx, string(id), request); err != nil {
		t.Fatal(err)
	}
	candidate := domain.BaseRevision{
		Repo: request.Repo, RepositoryID: request.RepositoryID,
		BaseRef: request.BaseRef, BaseSHA: request.HeadSHA,
	}
	if _, err := backend.PrepareCodexReviewWorkspace(
		ctx, journal, string(id), request.Workspace, candidate, sourceConfig.WorkspaceSizeMB,
	); err != nil {
		t.Fatal(err)
	}
	var failure *exec.ReviewSourceFailure
	err = source.VerifyRequestAuthority(ctx, id, domain.Digest("sha256:"+strings.Repeat("e", 64)))
	if !errors.As(err, &failure) || failure.Class != domain.ReviewFailureContradiction ||
		!errors.Is(failure.Err, domain.ErrParentKeyMismatch) {
		t.Fatalf("unstarted rejection = %v", err)
	}
	if outcome, ready, err := journal.GetCodexReviewOutcome(ctx, string(id)); err != nil ||
		!ready || !outcome.AbortRequired {
		t.Fatalf("unstarted rejection outcome = %#v, ready=%v, %v", outcome, ready, err)
	}
	volumes, _ := fx.rt.ListVolumes(ctx)
	if len(volumes) != 0 {
		t.Fatalf("prepared workspace leaked: %v", volumes)
	}
	// A rejected request with no workspace at all tolerates the absence and
	// still reports the rejection.
	bare := domain.InvocationID("review-run-rejected-bare")
	if err := journal.PutCodexReviewRequest(ctx, string(bare), request); err != nil {
		t.Fatal(err)
	}
	err = source.VerifyRequestAuthority(ctx, bare, domain.Digest("sha256:"+strings.Repeat("e", 64)))
	if !errors.As(err, &failure) || failure.Class != domain.ReviewFailureContradiction ||
		!errors.Is(failure.Err, domain.ErrParentKeyMismatch) {
		t.Fatalf("bare rejection = %v", err)
	}
	if outcome, ready, err := journal.GetCodexReviewOutcome(ctx, string(bare)); err != nil ||
		!ready || !outcome.AbortRequired {
		t.Fatalf("bare rejection outcome = %#v, ready=%v, %v", outcome, ready, err)
	}
	failure = nil
	if err := source.startRequestedReview(ctx, bare, request); !errors.As(err, &failure) ||
		failure.Class != domain.ReviewFailureContradiction {
		t.Fatalf("launch after bare rejection fence = %v, want contradiction", err)
	}
}

func TestCodexReviewCleanupRefusesRedirectedWorkspaceBinding(t *testing.T) {
	ctx := context.Background()
	fx := newHandoffFixture(t)
	seedSpec := fx.seed(t)
	backend := fx.backend(t)
	cfg, requestSpec := testCodexReview(t)
	journal := &fakeCodexReviewJournal{}
	sourceConfig := codexReviewSourceConfigForTest(t, backend, cfg, requestSpec, journal)
	source, err := NewCodexReviewSource(sourceConfig)
	if err != nil {
		t.Fatal(err)
	}
	id := domain.InvocationID("review-run-redirect-1")
	request := exec.ReviewRequest{
		RunID: "run-redirect", Round: 1, Repo: seedSpec.Seed.Base.Repo,
		RepositoryID: seedSpec.Seed.Base.RepositoryID, BaseRef: seedSpec.Seed.Base.BaseRef,
		BaseSHA: strings.Repeat("a", 40), HeadSHA: seedSpec.Seed.Base.BaseSHA,
		Workspace: seedSpec.Seed.SourceDir, Verification: testReviewVerificationEvidence(), Instructions: testReviewInstructionBinding(),
		RequestedAt: codexReviewEpoch.Add(-time.Minute),
	}
	if err := source.RequestReview(ctx, id, request); err != nil {
		t.Fatal(err)
	}
	// Rewrite the durable binding so its workspace fields identify a sibling
	// invocation's prepared volume, complete with the sibling's own valid
	// ownership evidence. Cleanup must refuse the redirection instead of
	// deleting the sibling's volume.
	siblingVolume := namesFor("review-run-sibling").Workspace
	siblingOwner := testOwnershipLabel()
	fx.rt.vols[siblingVolume] = &fakeVol{
		labels: append(runLabels("review-run-sibling"), siblingOwner), created: "sibling-created",
	}
	binding, err := journal.GetCodexReviewBinding(ctx, string(id))
	if err != nil {
		t.Fatal(err)
	}
	binding.WorkspaceSourceRunID = "review-run-sibling"
	binding.WorkspaceVolume = siblingVolume
	journal.binding = binding
	journal.workspaceBinding = CodexReviewWorkspaceBinding{
		SourceRunID: "review-run-sibling", Volume: siblingVolume,
		OwnershipToken: siblingOwner.Value, CreationFingerprint: "sibling-created",
	}
	if err := backend.AbortCodexReview(ctx, sourceConfig.Review, string(id)); !errors.Is(err, ErrConformance) {
		t.Fatalf("abort with redirected workspace binding = %v", err)
	}
	if _, ok := fx.rt.vols[siblingVolume]; !ok {
		t.Fatal("redirected cleanup deleted the sibling invocation's workspace volume")
	}
}

func TestCodexReviewCleanupRefusesSubstitutedIntentResources(t *testing.T) {
	ctx := context.Background()
	fx := newHandoffFixture(t)
	seedSpec := fx.seed(t)
	backend := fx.backend(t)
	cfg, requestSpec := testCodexReview(t)
	journal := &fakeCodexReviewJournal{}
	sourceConfig := codexReviewSourceConfigForTest(t, backend, cfg, requestSpec, journal)
	source, err := NewCodexReviewSource(sourceConfig)
	if err != nil {
		t.Fatal(err)
	}
	id := domain.InvocationID("review-run-redirect-2")
	request := exec.ReviewRequest{
		RunID: "run-redirect", Round: 1, Repo: seedSpec.Seed.Base.Repo,
		RepositoryID: seedSpec.Seed.Base.RepositoryID, BaseRef: seedSpec.Seed.Base.BaseRef,
		BaseSHA: strings.Repeat("a", 40), HeadSHA: seedSpec.Seed.Base.BaseSHA,
		Workspace: seedSpec.Seed.SourceDir, Verification: testReviewVerificationEvidence(), Instructions: testReviewInstructionBinding(),
		RequestedAt: codexReviewEpoch.Add(-time.Minute),
	}
	if err := source.RequestReview(ctx, id, request); err != nil {
		t.Fatal(err)
	}
	// Rewrite the intent so its shadow volume and network carry unused
	// CLI-safe substitute names. Cleanup must refuse rather than treat the
	// substitutes as already absent and close the intent while the real
	// resources stay live.
	realShadow := journal.intent.ShadowVolume
	realNetwork := journal.intent.Network
	journal.intent.ShadowVolume = "freeside-review-substitute-agents"
	journal.intent.Network = "freeside-review-substitute-egress"
	if err := backend.AbortCodexReview(ctx, sourceConfig.Review, string(id)); !errors.Is(err, ErrConformance) {
		t.Fatalf("abort with substituted intent resources = %v", err)
	}
	if _, ok := fx.rt.vols[realShadow]; !ok {
		t.Fatal("substituted-intent cleanup lost the real shadow volume")
	}
	if _, ok := fx.rt.nets[realNetwork]; !ok {
		t.Fatal("substituted-intent cleanup lost the real network")
	}
}

func TestCodexReviewCleanupRefusesMissingIntentAuthority(t *testing.T) {
	ctx := context.Background()
	fx := newHandoffFixture(t)
	seedSpec := fx.seed(t)
	backend := fx.backend(t)
	cfg, requestSpec := testCodexReview(t)
	journal := &fakeCodexReviewJournal{}
	sourceConfig := codexReviewSourceConfigForTest(t, backend, cfg, requestSpec, journal)
	source, err := NewCodexReviewSource(sourceConfig)
	if err != nil {
		t.Fatal(err)
	}
	id := domain.InvocationID("review-run-intent-authority-1")
	request := exec.ReviewRequest{
		RunID: "run-intent-authority", Round: 1, Repo: seedSpec.Seed.Base.Repo,
		RepositoryID: seedSpec.Seed.Base.RepositoryID, BaseRef: seedSpec.Seed.Base.BaseRef,
		BaseSHA: strings.Repeat("a", 40), HeadSHA: seedSpec.Seed.Base.BaseSHA,
		Workspace: seedSpec.Seed.SourceDir, Verification: testReviewVerificationEvidence(), Instructions: testReviewInstructionBinding(),
		RequestedAt: codexReviewEpoch.Add(-time.Minute),
	}
	if err := source.RequestReview(ctx, id, request); err != nil {
		t.Fatal(err)
	}
	journal.failGetIntent = errors.Join(ErrConformance, errors.New("intent body authority mismatch"))
	deleteCalls := 0
	fx.rt.onDeleteContainer = func(string) (bool, error) { deleteCalls++; return false, nil }
	fx.rt.onDeleteVolume = func(string) (bool, error) { deleteCalls++; return false, nil }
	fx.rt.onDeleteNetwork = func(string) (bool, error) { deleteCalls++; return false, nil }
	if err := backend.AbortCodexReview(ctx, sourceConfig.Review, string(id)); !errors.Is(err, ErrConformance) || errors.Is(err, ErrCodexReviewOperational) {
		t.Fatalf("abort without intent authority = %v", err)
	}
	if deleteCalls != 0 {
		t.Fatalf("cleanup issued %d deletes without intent authority", deleteCalls)
	}
}

func TestCodexReviewCleanupSurfacesForeignLeaseAsContradiction(t *testing.T) {
	ctx := context.Background()
	fx := newHandoffFixture(t)
	seedSpec := fx.seed(t)
	backend := fx.backend(t)
	cfg, requestSpec := testCodexReview(t)
	journal := &fakeCodexReviewJournal{}
	sourceConfig := codexReviewSourceConfigForTest(t, backend, cfg, requestSpec, journal)
	source, err := NewCodexReviewSource(sourceConfig)
	if err != nil {
		t.Fatal(err)
	}
	id := domain.InvocationID("review-run-lease-1")
	request := exec.ReviewRequest{
		RunID: "run-lease", Round: 1, Repo: seedSpec.Seed.Base.Repo,
		RepositoryID: seedSpec.Seed.Base.RepositoryID, BaseRef: seedSpec.Seed.Base.BaseRef,
		BaseSHA: strings.Repeat("a", 40), HeadSHA: seedSpec.Seed.Base.BaseSHA,
		Workspace: seedSpec.Seed.SourceDir, Verification: testReviewVerificationEvidence(), Instructions: testReviewInstructionBinding(),
		RequestedAt: codexReviewEpoch.Add(-time.Minute),
	}
	if err := source.RequestReview(ctx, id, request); err != nil {
		t.Fatal(err)
	}
	// An authenticated lease refusal at terminal cleanup is a contradiction,
	// not operational I/O: wrapping it transient would retry silently forever.
	tampered := sourceConfig.Review
	tampered.VolumeLifecycleLeaser = &fakeCodexReviewVolumeLeaser{
		rt: fx.rt, recoverErr: ErrCodexReviewVolumeLeaseForeignOwner,
	}
	if err := backend.AbortCodexReview(ctx, tampered, string(id)); !errors.Is(err, ErrConformance) {
		t.Fatalf("abort with foreign terminal lease = %v", err)
	}
	tampered.VolumeLifecycleLeaser = &fakeCodexReviewVolumeLeaser{
		rt: fx.rt, recoverErr: ErrCodexReviewVolumeLeaseTransferred,
	}
	if err := backend.AbortCodexReview(ctx, tampered, string(id)); !errors.Is(err, ErrConformance) {
		t.Fatalf("abort with still-transferred terminal lease = %v", err)
	}
	// A genuine operational refusal still stays retryable.
	tampered.VolumeLifecycleLeaser = &fakeCodexReviewVolumeLeaser{
		rt: fx.rt, recoverErr: errors.New("runtime lease bookkeeping unavailable"),
	}
	err = backend.AbortCodexReview(ctx, tampered, string(id))
	if !errors.Is(err, ErrCodexReviewOperational) || errors.Is(err, ErrConformance) {
		t.Fatalf("abort with operational lease failure = %v", err)
	}
}

func TestCodexReviewRecoveryAbortsLegacyRunningInvocationWithLostProxy(t *testing.T) {
	ctx := context.Background()
	fx := newHandoffFixture(t)
	seedSpec := fx.seed(t)
	backend := fx.backend(t)
	cfg, requestSpec := testCodexReview(t)
	journal := &fakeCodexReviewJournal{}
	sourceConfig := codexReviewSourceConfigForTest(t, backend, cfg, requestSpec, journal)
	source, err := NewCodexReviewSource(sourceConfig)
	if err != nil {
		t.Fatal(err)
	}
	id := domain.InvocationID("review-run-restart-1")
	request := exec.ReviewRequest{
		RunID: "run-restart", Round: 1, Repo: seedSpec.Seed.Base.Repo,
		RepositoryID: seedSpec.Seed.Base.RepositoryID, BaseRef: seedSpec.Seed.Base.BaseRef,
		BaseSHA: strings.Repeat("a", 40), HeadSHA: seedSpec.Seed.Base.BaseSHA,
		Workspace: seedSpec.Seed.SourceDir, Verification: testReviewVerificationEvidence(), Instructions: testReviewInstructionBinding(),
		RequestedAt: codexReviewEpoch.Add(-time.Minute),
	}
	if err := source.RequestReview(ctx, id, request); err != nil {
		t.Fatal(err)
	}
	source.mu.Lock()
	_ = source.launches[id].Close()
	delete(source.launches, id)
	source.mu.Unlock()
	journal.binding.InstructionCompositionVersion = ""
	journal.binding.HostInstructionDigest = nil
	journal.binding.RepositoryInstructionSources = nil
	if err := journal.binding.validateForTeardown(); err != nil {
		t.Fatalf("legacy binding fixture: %v", err)
	}
	volumeLeaser, err := NewRuntimeCodexReviewVolumeLeaser(fx.rt)
	if err != nil {
		t.Fatal(err)
	}
	recovery, err := NewCodexReviewRecovery(backend, journal, volumeLeaser, cfg.InputRoot)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := journal.GetCodexReviewIntent(ctx, string(id))
	if err != nil {
		t.Fatal(err)
	}
	failedCleanup := false
	fx.rt.onDeleteContainer = func(container string) (bool, error) {
		if container == intent.ReviewContainer && !failedCleanup {
			failedCleanup = true
			return true, errors.New("runtime cleanup temporarily unavailable")
		}
		return false, nil
	}
	if err := recovery.Reconcile(ctx); !errors.Is(err, ErrCodexReviewOperational) {
		t.Fatalf("restart transient abort = %v", err)
	}
	if _, ready, err := journal.GetCodexReviewOutcome(ctx, string(id)); err != nil || ready {
		t.Fatalf("outcome after transient abort = ready %v, %v", ready, err)
	}
	fx.rt.onDeleteContainer = nil
	if err := recovery.Reconcile(ctx); err != nil {
		t.Fatalf("restart recovery retry = %v", err)
	}
	outcome, ready, err := journal.GetCodexReviewOutcome(ctx, string(id))
	if err != nil || !ready || outcome.FailureClass != domain.ReviewFailureTransient || !outcome.AbortRequired {
		t.Fatalf("restart outcome = %#v, ready %v, %v", outcome, ready, err)
	}
	containers, _ := fx.rt.ListContainers(ctx)
	volumes, _ := fx.rt.ListVolumes(ctx)
	networks, _ := fx.rt.ListNetworks(ctx)
	if len(containers) != 0 || len(volumes) != 0 || len(networks) != 0 {
		t.Fatalf("aborted topology leaked: containers=%v volumes=%v networks=%v", containers, volumes, networks)
	}
}

func TestCodexReviewSourceRestartAbortsRunningInvocationWithLostProxy(t *testing.T) {
	ctx := context.Background()
	fx := newHandoffFixture(t)
	seedSpec := fx.seed(t)
	backend := fx.backend(t)
	cfg, requestSpec := testCodexReview(t)
	journal := &fakeCodexReviewJournal{}
	sourceConfig := codexReviewSourceConfigForTest(t, backend, cfg, requestSpec, journal)
	source, err := NewCodexReviewSource(sourceConfig)
	if err != nil {
		t.Fatal(err)
	}
	id := domain.InvocationID("review-run-restart-source")
	request := exec.ReviewRequest{
		RunID: "run-restart-source", Round: 1, Repo: seedSpec.Seed.Base.Repo,
		RepositoryID: seedSpec.Seed.Base.RepositoryID, BaseRef: seedSpec.Seed.Base.BaseRef,
		BaseSHA: strings.Repeat("a", 40), HeadSHA: seedSpec.Seed.Base.BaseSHA,
		Workspace: seedSpec.Seed.SourceDir, Verification: testReviewVerificationEvidence(), Instructions: testReviewInstructionBinding(),
		RequestedAt: codexReviewEpoch.Add(-time.Minute),
	}
	if err := source.RequestReview(ctx, id, request); err != nil {
		t.Fatal(err)
	}
	source.mu.Lock()
	_ = source.launches[id].Close()
	delete(source.launches, id)
	source.mu.Unlock()
	restartedConfig := source.cfg
	restartedConfig.Review.VolumeLifecycleLeaser, err = NewRuntimeCodexReviewVolumeLeaser(fx.rt)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := NewCodexReviewSource(restartedConfig)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := journal.GetCodexReviewIntent(ctx, string(id))
	if err != nil {
		t.Fatal(err)
	}
	failedCleanup := false
	fx.rt.onDeleteContainer = func(container string) (bool, error) {
		if container == intent.ReviewContainer && !failedCleanup {
			failedCleanup = true
			return true, errors.New("runtime cleanup temporarily unavailable")
		}
		return false, nil
	}
	status, err := restarted.Inspect(ctx, id)
	if err != nil || status != exec.StatusPending {
		t.Fatalf("restart transient abort = %q, %v", status, err)
	}
	if _, err := restarted.Poll(ctx, id); !errors.Is(err, exec.ErrResultNotReady) {
		t.Fatalf("poll after transient abort = %v", err)
	}
	fx.rt.onDeleteContainer = nil
	status, err = restarted.Inspect(ctx, id)
	if err != nil || status != exec.StatusFailed {
		t.Fatalf("restart inspect = %q, %v", status, err)
	}
	_, err = restarted.Poll(ctx, id)
	var failure *exec.ReviewSourceFailure
	if !errors.As(err, &failure) || failure.Class != domain.ReviewFailureTransient {
		t.Fatalf("restart poll failure = %v", err)
	}
}

func TestCodexReviewRecoveryCleansBeforeReportingRejectedOutcome(t *testing.T) {
	ctx := context.Background()
	fx := newHandoffFixture(t)
	seedSpec := fx.seed(t)
	backend := fx.backend(t)
	cfg, requestSpec := testCodexReview(t)
	journal := &fakeCodexReviewJournal{}
	source, err := NewCodexReviewSource(
		codexReviewSourceConfigForTest(t, backend, cfg, requestSpec, journal),
	)
	if err != nil {
		t.Fatal(err)
	}
	id := domain.InvocationID("review-run-rejected-outcome")
	request := exec.ReviewRequest{
		RunID: "run-rejected", Round: 1, Repo: seedSpec.Seed.Base.Repo,
		RepositoryID: seedSpec.Seed.Base.RepositoryID, BaseRef: seedSpec.Seed.Base.BaseRef,
		BaseSHA: strings.Repeat("a", 40), HeadSHA: seedSpec.Seed.Base.BaseSHA,
		Workspace: seedSpec.Seed.SourceDir, Verification: testReviewVerificationEvidence(), Instructions: testReviewInstructionBinding(),
		RequestedAt: codexReviewEpoch.Add(-time.Minute),
	}
	if err := source.RequestReview(ctx, id, request); err != nil {
		t.Fatal(err)
	}
	source.mu.Lock()
	_ = source.launches[id].Close()
	delete(source.launches, id)
	source.mu.Unlock()
	journal.failGetOutcome = ErrCodexReviewOutcomeRejected
	volumeLeaser, err := NewRuntimeCodexReviewVolumeLeaser(fx.rt)
	if err != nil {
		t.Fatal(err)
	}
	recovery, err := NewCodexReviewRecovery(backend, journal, volumeLeaser, cfg.InputRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := recovery.Reconcile(ctx); !errors.Is(err, ErrCodexReviewOutcomeRejected) {
		t.Fatalf("rejected outcome recovery = %v", err)
	}
	if journal.intent.State != CodexReviewIntentClosed {
		t.Fatalf("rejected outcome left intent %q, want closed", journal.intent.State)
	}
	containers, _ := fx.rt.ListContainers(ctx)
	volumes, _ := fx.rt.ListVolumes(ctx)
	networks, _ := fx.rt.ListNetworks(ctx)
	if len(containers) != 0 || len(volumes) != 0 || len(networks) != 0 {
		t.Fatalf("rejected outcome leaked topology: containers=%v volumes=%v networks=%v",
			containers, volumes, networks)
	}
	if err := recovery.Reconcile(ctx); err != nil {
		t.Fatalf("closed rejected outcome kept recovery wedged: %v", err)
	}
}

func TestCodexReviewRecoveryDoesNotTrustClosedRejectedLegacyOutcome(t *testing.T) {
	for _, ready := range []bool{false, true} {
		t.Run(fmt.Sprintf("ready=%t", ready), func(t *testing.T) {
			fx := newHandoffFixture(t)
			backend := fx.backend(t)
			id := "review-legacy-closed"
			owner := strings.Repeat("b", 32)
			shadow := codexReviewShadowVolumeName(id)
			network := codexReviewNetworkName(id)
			review := codexReviewContainerName(id)
			journal := &fakeCodexReviewJournal{
				intent: &CodexReviewLaunchIntent{
					RunID: id, SpecDigest: strings.Repeat("a", 64),
					OwnershipToken: owner, ShadowVolume: shadow,
					Network: network, ReviewContainer: review, State: CodexReviewIntentClosed,
					Resources: []CodexReviewIntentResource{
						{Name: codexReviewWorkspaceObserverName(id)},
						{Name: codexReviewShadowInitializerName(id), OwnershipToken: owner},
						{Name: codexReviewShadowObserverName(id)},
						{Name: shadow, OwnershipToken: owner},
						{Name: network, OwnershipToken: owner},
						{Name: review, OwnershipToken: owner},
					},
				},
				ready:          map[string]bool{id: ready},
				failGetOutcome: ErrCodexReviewOutcomeRejected,
			}
			leaser, err := NewRuntimeCodexReviewVolumeLeaser(fx.rt)
			if err != nil {
				t.Fatal(err)
			}
			recovery, err := NewCodexReviewRecovery(backend, journal, leaser, "")
			if err != nil {
				t.Fatal(err)
			}
			if err := recovery.Reconcile(t.Context()); err != nil {
				t.Fatalf("closed legacy outcome blocked topology recovery: %v", err)
			}
		})
	}
}

func TestCodexReviewSourceNormalizesStructuredFindings(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	source := &CodexReviewSource{cfg: CodexReviewSourceConfig{
		Review:              CodexReviewConfig{Model: "gpt-codex", ReasoningEffort: "high"},
		ConfigurationDigest: domain.Digest("sha256:" + strings.Repeat("c", 64)),
		CostOwner:           "subscription:owner", Now: func() time.Time { return now },
	}}
	request := exec.ReviewRequest{
		RunID: "run-1", Round: 1, Repo: "owner/repo", RepositoryID: 42,
		BaseRef: "main", BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40),
		Workspace: "/seed/candidate", Verification: testReviewVerificationEvidence(), Instructions: testReviewInstructionBinding(),
		RequestedAt: now.Add(-time.Minute),
	}
	collection := CodexReviewCollection{
		Result: []byte(`{"findings":[{"severity":"P2","location":"daemon/main.go:12","explanation":"unchecked error"}]}`),
		Events: []byte("terminal event\n"),
	}
	first := source.normalizeCollection("review-run-1-1", request, collection)
	second := source.normalizeCollection("review-run-1-1", request, collection)
	if err := first.Validate(); err != nil {
		t.Fatal(err)
	}
	if first.Result == nil || len(first.Result.Findings) != 1 {
		t.Fatalf("normalized outcome = %#v", first)
	}
	finding := first.Result.Findings[0]
	if finding.ID != second.Result.Findings[0].ID || finding.RunID != request.RunID ||
		finding.Source != "codex_local" || finding.Severity != "P2" ||
		first.Result.BaseSHA != request.BaseSHA || first.Result.HeadSHA != request.HeadSHA ||
		first.Result.Provider != "openai" || first.Result.ModelConfiguration != "gpt-codex/high" ||
		first.Result.CompletionEvidence == "" {
		t.Fatalf("normalized result = %#v", first.Result)
	}
}

func TestCodexReviewSourceRejectsInvalidFindingsEnvelope(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	source := &CodexReviewSource{cfg: CodexReviewSourceConfig{
		Review:              CodexReviewConfig{Model: "gpt-codex", ReasoningEffort: "high"},
		ConfigurationDigest: domain.Digest("sha256:" + strings.Repeat("c", 64)),
		CostOwner:           "subscription:owner", Now: func() time.Time { return now },
	}}
	request := exec.ReviewRequest{
		RunID: "run-1", Round: 1, Repo: "owner/repo", RepositoryID: 42,
		BaseRef: "main", BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40),
		Workspace: "/seed/candidate", Verification: testReviewVerificationEvidence(), Instructions: testReviewInstructionBinding(),
		RequestedAt: now.Add(-time.Minute),
	}
	for _, tc := range []struct {
		name, result, failure string
	}{
		{"missing", `{}`, "required findings array"},
		{"null", `{"findings":null}`, "required findings array"},
		{
			"duplicate",
			`{"findings":[{"severity":"P1","location":"main.go:1","explanation":"unsafe"}],"findings":[]}`,
			"malformed structured output",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			outcome := source.normalizeCollection("review-run-1-1", request, CodexReviewCollection{
				Result: []byte(tc.result), Events: []byte("terminal event\n"),
			})
			if outcome.Result != nil || outcome.FailureClass != domain.ReviewFailureContradiction ||
				!strings.Contains(outcome.Failure, tc.failure) {
				t.Fatalf("result %s normalized as %#v", tc.result, outcome)
			}
		})
	}
	clean := source.normalizeCollection("review-run-1-1", request, CodexReviewCollection{
		Result: []byte(`{"findings":[]}`), Events: []byte("terminal event\n"),
	})
	if clean.Result == nil || len(clean.Result.Findings) != 0 || clean.FailureClass != "" {
		t.Fatalf("empty findings array = %#v", clean)
	}
}

func TestCodexReviewSourceFindingIdentityIsInvocationScoped(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	source := &CodexReviewSource{cfg: CodexReviewSourceConfig{
		Review:              CodexReviewConfig{Model: "gpt-codex", ReasoningEffort: "high"},
		ConfigurationDigest: domain.Digest("sha256:" + strings.Repeat("c", 64)),
		CostOwner:           "owner", Now: func() time.Time { return now },
	}}
	request := exec.ReviewRequest{
		RunID: "run-1", Round: 1, Repo: "owner/repo", RepositoryID: 42, BaseRef: "main",
		BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40), Workspace: "/candidate",
		Verification: testReviewVerificationEvidence(), Instructions: testReviewInstructionBinding(), RequestedAt: now,
	}
	collection := CodexReviewCollection{Result: []byte(
		`{"findings":[{"severity":"P2","location":"daemon/main.go:12","explanation":"unchecked error"}]}`,
	)}
	first := source.normalizeCollection("review-invocation-1", request, collection)
	request.RunID = "run-2"
	second := source.normalizeCollection("review-invocation-2", request, collection)
	if first.Result.Findings[0].ID == second.Result.Findings[0].ID {
		t.Fatalf("finding identity crossed invocations: %q", first.Result.Findings[0].ID)
	}
}

func TestCodexReviewSourceClassifiesTerminalFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		events string
		want   domain.ReviewFailureClass
	}{
		{"quota", "rate limit exceeded", domain.ReviewFailureQuota},
		{"configuration", "authentication failed", domain.ReviewFailureConfiguration},
		{"transient", "connection reset", domain.ReviewFailureTransient},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyCodexTerminalFailure([]byte(tc.events)); got != tc.want {
				t.Fatalf("class = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCodexReviewSourceClassifiesOutcomeWritesAndCleanup(t *testing.T) {
	journalErr := errors.New("journal temporarily unavailable")
	var failure *exec.ReviewSourceFailure
	if err := codexReviewOutcomeWriteFailure(journalErr); !errors.As(err, &failure) ||
		failure.Class != domain.ReviewFailureTransient || !errors.Is(err, journalErr) {
		t.Fatalf("outcome write failure = %v", err)
	}

	status, err := codexReviewCleanupStatus(errors.New("runtime temporarily unavailable"))
	if err != nil || status != exec.StatusPending {
		t.Fatalf("operational cleanup = %q, %v", status, err)
	}
	conformanceErr := fmt.Errorf("foreign cleanup topology: %w", ErrConformance)
	status, err = codexReviewCleanupStatus(conformanceErr)
	failure = nil
	if status != "" || !errors.As(err, &failure) ||
		failure.Class != domain.ReviewFailureContradiction || !errors.Is(err, conformanceErr) {
		t.Fatalf("contradictory cleanup = %q, %v", status, err)
	}
}

func TestCodexReviewLaunchCleanupFailureDoesNotTerminalizeUnreapedWorkspace(t *testing.T) {
	launchErr := fmt.Errorf("invalid review launch: %w", ErrInvalidCodexReviewSpec)
	transient := codexReviewLaunchCleanupFailure(launchErr, errors.New("runtime temporarily unavailable"))
	if exec.ClassifyReviewSourceFailure(transient) != domain.ReviewFailureTransient ||
		!errors.Is(transient, launchErr) {
		t.Fatalf("transient cleanup classification = %v", transient)
	}
	contradiction := codexReviewLaunchCleanupFailure(launchErr, fmt.Errorf("foreign volume: %w", ErrConformance))
	if exec.ClassifyReviewSourceFailure(contradiction) != domain.ReviewFailureContradiction ||
		!errors.Is(contradiction, launchErr) {
		t.Fatalf("contradictory cleanup classification = %v", contradiction)
	}
	operationalContradiction := codexReviewLaunchCleanupFailure(
		fmt.Errorf("invalid review authority: %w", ErrConformance),
		errors.Join(ErrCodexReviewOperational, fmt.Errorf("workspace cleanup: %w", ErrConformance)),
	)
	if exec.ClassifyReviewSourceFailure(operationalContradiction) != domain.ReviewFailureTransient {
		t.Fatalf("operational cleanup did not outrank contradiction: %v", operationalContradiction)
	}
}

func TestCodexReviewSourceJournalReadsKeepResultPending(t *testing.T) {
	id := domain.InvocationID("review-journal-read-1")
	request := exec.ReviewRequest{
		RunID: "run-journal-read", Round: 1, Repo: "owner/repo", RepositoryID: 42,
		BaseRef: "main", BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40),
		Workspace: "/seed/candidate", Verification: testReviewVerificationEvidence(), Instructions: testReviewInstructionBinding(),
		RequestedAt: codexReviewEpoch,
	}
	journal := &fakeCodexReviewJournal{requests: map[string]exec.ReviewRequest{string(id): request}}
	source := &CodexReviewSource{cfg: CodexReviewSourceConfig{Journal: journal}}
	readErr := errors.New("journal temporarily unavailable")
	journal.failGetOutcome = readErr
	if _, err := source.Poll(context.Background(), id); !errors.Is(err, exec.ErrResultNotReady) ||
		exec.ClassifyReviewSourceFailure(err) != domain.ReviewFailureTransient || !errors.Is(err, readErr) {
		t.Fatalf("poll journal failure = %v", err)
	}
	journal.failGetOutcome = nil
	journal.failGetRequest = readErr
	err := source.Verify(context.Background(), id, request.BaseSHA, request.HeadSHA)
	if exec.ClassifyReviewSourceFailure(err) != domain.ReviewFailureTransient || !errors.Is(err, readErr) {
		t.Fatalf("verify journal failure = %v", err)
	}
}

func TestCodexReviewSourceRecoversBeforeLaunchIntent(t *testing.T) {
	for _, preparation := range []string{"absent", "pending", "finalized"} {
		t.Run(preparation, func(t *testing.T) {
			ctx := context.Background()
			fx := newHandoffFixture(t)
			seedSpec := fx.seed(t)
			backend := fx.backend(t)
			cfg, requestSpec := testCodexReview(t)
			journal := &fakeCodexReviewJournal{}
			id := domain.InvocationID("review-recover-" + preparation)
			request := exec.ReviewRequest{
				RunID: "run-recover", Round: 1, Repo: seedSpec.Seed.Base.Repo,
				RepositoryID: seedSpec.Seed.Base.RepositoryID, BaseRef: seedSpec.Seed.Base.BaseRef,
				BaseSHA: strings.Repeat("a", 40), HeadSHA: seedSpec.Seed.Base.BaseSHA,
				Workspace: seedSpec.Seed.SourceDir, Verification: testReviewVerificationEvidence(), Instructions: testReviewInstructionBinding(),
				RequestedAt: codexReviewEpoch.Add(-time.Minute),
			}
			if err := journal.PutCodexReviewRequest(ctx, string(id), request); err != nil {
				t.Fatal(err)
			}
			var priorFingerprint string
			switch preparation {
			case "pending":
				owner := testOwnershipLabel()
				binding := CodexReviewWorkspaceBinding{
					SourceRunID: string(id), Volume: namesFor(string(id)).Workspace,
					OwnershipToken: owner.Value,
				}
				if err := journal.PutCodexReviewWorkspaceBinding(ctx, binding); err != nil {
					t.Fatal(err)
				}
				if err := fx.rt.CreateVolume(ctx, binding.Volume, 64,
					append(runLabels(string(id)), owner)); err != nil {
					t.Fatal(err)
				}
				view, err := fx.rt.InspectVolume(ctx, binding.Volume)
				if err != nil {
					t.Fatal(err)
				}
				priorFingerprint = view.CreationDate
			case "finalized":
				binding, err := backend.PrepareCodexReviewWorkspace(
					ctx, journal, string(id), request.Workspace,
					domain.BaseRevision{
						Repo: request.Repo, RepositoryID: request.RepositoryID,
						BaseRef: request.BaseRef, BaseSHA: request.HeadSHA,
					}, 64,
				)
				if err != nil {
					t.Fatal(err)
				}
				priorFingerprint = binding.CreationFingerprint
			}
			source, err := NewCodexReviewSource(
				codexReviewSourceConfigForTest(t, backend, cfg, requestSpec, journal),
			)
			if err != nil {
				t.Fatal(err)
			}
			status, err := source.Inspect(ctx, id)
			if err != nil || status != exec.StatusRunning {
				t.Fatalf("recovered status = %q, %v", status, err)
			}
			workspace, err := journal.GetCodexReviewWorkspaceBinding(ctx, string(id))
			if err != nil || workspace.CreationFingerprint == "" {
				t.Fatalf("recovered workspace = %#v, %v", workspace, err)
			}
			if preparation == "pending" && workspace.CreationFingerprint == priorFingerprint {
				t.Fatal("pending partial workspace was adopted instead of reconstructed")
			}
			if preparation == "finalized" && workspace.CreationFingerprint != priorFingerprint {
				t.Fatal("finalized workspace was unnecessarily reconstructed")
			}
			intent, err := journal.GetCodexReviewIntent(ctx, string(id))
			if err != nil || intent.State != CodexReviewIntentStarted {
				t.Fatalf("recovered intent = %#v, %v", intent, err)
			}
			source.mu.Lock()
			launch := source.launches[id]
			delete(source.launches, id)
			source.mu.Unlock()
			if launch != nil {
				_ = launch.Close()
			}
			if err := backend.AbortCodexReview(ctx, source.cfg.Review, string(id)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCodexReviewSourceRetriesTransientPreparationUnderSameInvocation(t *testing.T) {
	ctx := context.Background()
	fx := newHandoffFixture(t)
	seedSpec := fx.seed(t)
	backend := fx.backend(t)
	cfg, requestSpec := testCodexReview(t)
	journal := &fakeCodexReviewJournal{}
	source, err := NewCodexReviewSource(
		codexReviewSourceConfigForTest(t, backend, cfg, requestSpec, journal),
	)
	if err != nil {
		t.Fatal(err)
	}
	id := domain.InvocationID("review-prep-retry")
	request := exec.ReviewRequest{
		RunID: "run-retry", Round: 1, Repo: seedSpec.Seed.Base.Repo,
		RepositoryID: seedSpec.Seed.Base.RepositoryID, BaseRef: seedSpec.Seed.Base.BaseRef,
		BaseSHA: strings.Repeat("a", 40), HeadSHA: seedSpec.Seed.Base.BaseSHA,
		Workspace: seedSpec.Seed.SourceDir, Verification: testReviewVerificationEvidence(), Instructions: testReviewInstructionBinding(),
		RequestedAt: codexReviewEpoch.Add(-time.Minute),
	}
	fx.rt.onCreateVolume = func(name string) error {
		if name == namesFor(string(id)).Workspace {
			return errors.New("runtime temporarily unavailable")
		}
		return nil
	}
	if err := source.RequestReview(ctx, id, request); err == nil ||
		exec.ClassifyReviewSourceFailure(err) != domain.ReviewFailureTransient {
		t.Fatalf("transient preparation = %v", err)
	}
	fx.rt.onCreateVolume = nil
	status, err := source.Inspect(ctx, id)
	if err != nil || status != exec.StatusRunning {
		t.Fatalf("retried preparation = %q, %v", status, err)
	}
	source.mu.Lock()
	launch := source.launches[id]
	delete(source.launches, id)
	source.mu.Unlock()
	if launch != nil {
		_ = launch.Close()
	}
	if err := backend.AbortCodexReview(ctx, source.cfg.Review, string(id)); err != nil {
		t.Fatal(err)
	}
}

func TestCodexReviewSourceRetriesTransientLaunchUnderSameInvocation(t *testing.T) {
	ctx := context.Background()
	fx := newHandoffFixture(t)
	seedSpec := fx.seed(t)
	backend := fx.backend(t)
	cfg, requestSpec := testCodexReview(t)
	journal := &fakeCodexReviewJournal{}
	source, err := NewCodexReviewSource(
		codexReviewSourceConfigForTest(t, backend, cfg, requestSpec, journal),
	)
	if err != nil {
		t.Fatal(err)
	}
	id := domain.InvocationID("review-launch-retry")
	request := exec.ReviewRequest{
		RunID: "run-retry", Round: 1, Repo: seedSpec.Seed.Base.Repo,
		RepositoryID: seedSpec.Seed.Base.RepositoryID, BaseRef: seedSpec.Seed.Base.BaseRef,
		BaseSHA: strings.Repeat("a", 40), HeadSHA: seedSpec.Seed.Base.BaseSHA,
		Workspace: seedSpec.Seed.SourceDir, Verification: testReviewVerificationEvidence(), Instructions: testReviewInstructionBinding(),
		RequestedAt: codexReviewEpoch.Add(-time.Minute),
	}
	fx.rt.onCreateVolume = func(name string) error {
		if name == codexReviewShadowVolumeName(string(id)) {
			return errors.New("runtime temporarily unavailable")
		}
		return nil
	}
	if err := source.RequestReview(ctx, id, request); err == nil ||
		exec.ClassifyReviewSourceFailure(err) != domain.ReviewFailureTransient {
		t.Fatalf("transient launch = %v", err)
	}
	workspace, err := journal.GetCodexReviewWorkspaceBinding(ctx, string(id))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fx.rt.InspectVolume(ctx, workspace.Volume); err != nil {
		t.Fatalf("transient launch removed retry workspace: %v", err)
	}
	fx.rt.onCreateVolume = nil
	status, err := source.Inspect(ctx, id)
	if err != nil || status != exec.StatusRunning {
		t.Fatalf("retried launch = %q, %v", status, err)
	}
	source.mu.Lock()
	launch := source.launches[id]
	delete(source.launches, id)
	source.mu.Unlock()
	if launch != nil {
		_ = launch.Close()
	}
	if err := backend.AbortCodexReview(ctx, source.cfg.Review, string(id)); err != nil {
		t.Fatal(err)
	}
}

func TestCodexReviewRestartRecoversLegacyRoundAndRelaunchesSameRequest(t *testing.T) {
	ctx := context.Background()
	fx := newHandoffFixture(t)
	seedSpec := fx.seed(t)
	backend := fx.backend(t)
	cfg, requestSpec := testCodexReview(t)
	journal := &fakeCodexReviewJournal{}
	sourceConfig := codexReviewSourceConfigForTest(t, backend, cfg, requestSpec, journal)
	source, err := NewCodexReviewSource(sourceConfig)
	if err != nil {
		t.Fatal(err)
	}
	id := domain.InvocationID("review-0dcd5c691adcaec0c353993a")
	request := exec.ReviewRequest{
		RunID: "run-bc28d74f7774a86464f1cc823da168d69b9f4b2a2de56d4a0abb6f5171e5c165",
		Round: 2, Repo: seedSpec.Seed.Base.Repo,
		RepositoryID: seedSpec.Seed.Base.RepositoryID, BaseRef: seedSpec.Seed.Base.BaseRef,
		BaseSHA: "9595682ebad1610833660ba469e8fc18b5ed8cab",
		HeadSHA: seedSpec.Seed.Base.BaseSHA, Workspace: seedSpec.Seed.SourceDir,
		Verification: testReviewVerificationEvidence(), Instructions: testReviewInstructionBinding(),
		RequestedAt: time.Date(2026, 8, 7, 3, 44, 13, 821738000, time.UTC),
	}
	if err := journal.PutCodexReviewRequest(ctx, string(id), request); err != nil {
		t.Fatal(err)
	}
	workspace, err := backend.PrepareCodexReviewWorkspace(
		ctx, journal, string(id), request.Workspace,
		domain.BaseRevision{
			Repo: request.Repo, RepositoryID: request.RepositoryID,
			BaseRef: request.BaseRef, BaseSHA: request.HeadSHA,
		}, sourceConfig.WorkspaceSizeMB,
	)
	if err != nil {
		t.Fatal(err)
	}
	instructions, instructionFile, err := source.materializeReviewInstructions(ctx, id, request.Instructions)
	if err != nil {
		t.Fatal(err)
	}
	launchSpec := CodexReviewLaunchSpec{
		RunID: string(id), Image: sourceConfig.Review.ApprovedImage,
		WorkspaceSourceRunID: string(id), WorkspaceVolume: workspace.Volume,
		ExpectedHead: request.HeadSHA, Prompt: codexProductionReviewPrompt(request),
		Boundary: CodexReviewFreshStart, AuthMode: sourceConfig.AuthMode,
		AuthIdentityID: sourceConfig.AuthIdentityID, AuthSnapshot: sourceConfig.AuthSnapshot,
		Instructions: instructions, InstructionFile: instructionFile,
		InstructionBinding: request.Instructions,
	}
	digest, err := codexReviewIntentDigest(sourceConfig.Review, launchSpec)
	if err != nil {
		t.Fatal(err)
	}
	owner := testOwnershipLabel()
	journal.intent = legacyCodexReviewIntentForTest(string(id), digest, owner.Value)
	legacyObserver := legacyCodexReviewNames(string(id)).workspaceObserver
	fx.rt.ctrs[legacyObserver] = &fakeCtr{
		spec:    ContainerSpec{Name: legacyObserver, Labels: append(runLabels(string(id)), owner)},
		created: "legacy-observer",
	}
	recovery, err := NewCodexReviewRecovery(
		backend, journal, sourceConfig.Review.VolumeLifecycleLeaser, sourceConfig.Review.InputRoot,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := recovery.Reconcile(ctx); err != nil {
		t.Fatalf("restart recovery: %v", err)
	}
	if journal.intent.State != CodexReviewIntentClosed {
		t.Fatalf("recovered intent state = %q, want closed before retry", journal.intent.State)
	}
	persisted, err := journal.GetCodexReviewRequest(ctx, string(id))
	if err != nil || !reflect.DeepEqual(persisted, request) {
		t.Fatalf("recovery changed persisted round-2 request: %#v, %v", persisted, err)
	}
	if _, exists := fx.rt.ctrs[legacyObserver]; exists {
		t.Fatal("restart recovery left the authenticated legacy observer")
	}
	restarted, err := NewCodexReviewSource(sourceConfig)
	if err != nil {
		t.Fatal(err)
	}
	status, err := restarted.Inspect(ctx, id)
	if err != nil || status != exec.StatusRunning {
		t.Fatalf("same-request restart launch = %q, %v", status, err)
	}
	if journal.intent.State != CodexReviewIntentStarted ||
		journal.intent.Resources[0].Name != codexReviewWorkspaceObserverName(string(id)) {
		t.Fatalf("restart launch intent = %#v, want current started topology", journal.intent)
	}
	restarted.mu.Lock()
	launch := restarted.launches[id]
	delete(restarted.launches, id)
	restarted.mu.Unlock()
	if launch != nil {
		_ = launch.Close()
	}
	if err := backend.AbortCodexReview(ctx, restarted.cfg.Review, string(id)); err != nil {
		t.Fatal(err)
	}
}

func TestCodexReviewSourceVerifyRejectsSwappedInvocation(t *testing.T) {
	id := domain.InvocationID("review-run-1-1")
	request := exec.ReviewRequest{
		RunID: "run-1", Round: 1, Repo: "owner/repo", RepositoryID: 42, BaseRef: "main",
		BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40), Workspace: "/candidate",
		Verification: testReviewVerificationEvidence(), Instructions: testReviewInstructionBinding(), RequestedAt: codexReviewEpoch,
	}
	result := exec.ReviewResult{
		InvocationID: "review-run-1-2", BaseSHA: request.BaseSHA, HeadSHA: request.HeadSHA,
		Provider: "openai", ModelConfiguration: "gpt-codex/high",
		ConfigurationDigest: domain.Digest("sha256:" + strings.Repeat("c", 64)),
		CostOwner:           "owner", CompletedAt: codexReviewEpoch,
	}
	collectionEvidence := domain.Digest("sha256:" + strings.Repeat("e", 64))
	result.CompletionEvidence, _ = CodexReviewResultEvidence(result, collectionEvidence)
	journal := &fakeCodexReviewJournal{
		requests: map[string]exec.ReviewRequest{string(id): request},
		outcomes: map[string]CodexReviewSourceOutcome{
			string(id): {InvocationID: id, Result: &result, CollectionEvidence: collectionEvidence},
		},
		ready: map[string]bool{string(id): true},
	}
	source := &CodexReviewSource{cfg: CodexReviewSourceConfig{Journal: journal}}
	if err := source.Verify(t.Context(), id, request.BaseSHA, request.HeadSHA); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("swapped invocation verification = %v", err)
	}
}

func TestCodexReviewSourcePollRejectsSwappedFailureOutcome(t *testing.T) {
	id := domain.InvocationID("review-run-failure-1")
	foreign := domain.InvocationID("review-run-failure-2")
	journal := &fakeCodexReviewJournal{
		outcomes: map[string]CodexReviewSourceOutcome{
			string(id): {
				InvocationID: foreign, FailureClass: domain.ReviewFailureConfiguration,
				Failure: "foreign review configuration failure",
			},
		},
		ready: map[string]bool{string(id): true},
	}
	source := &CodexReviewSource{cfg: CodexReviewSourceConfig{Journal: journal}}
	if _, err := source.Poll(t.Context(), id); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("swapped failure outcome poll = %v", err)
	}
}

func TestCodexReviewSourcePollMarksPersistedFailureTerminal(t *testing.T) {
	for _, class := range []domain.ReviewFailureClass{
		domain.ReviewFailureTransient,
		domain.ReviewFailureConfiguration,
		domain.ReviewFailureQuota,
		domain.ReviewFailureContradiction,
	} {
		t.Run(string(class), func(t *testing.T) {
			id := domain.InvocationID("review-run-terminal-failure-" + string(class))
			journal := &fakeCodexReviewJournal{
				outcomes: map[string]CodexReviewSourceOutcome{
					string(id): {
						InvocationID: id, FailureClass: class,
						Failure: "terminal source failure",
					},
				},
				ready: map[string]bool{string(id): true},
			}
			source := &CodexReviewSource{cfg: CodexReviewSourceConfig{Journal: journal}}
			for range 2 {
				_, err := source.Poll(t.Context(), id)
				if !errors.Is(err, exec.ErrNoResult) ||
					exec.ClassifyReviewSourceFailure(err) != class {
					t.Fatalf("terminal %s outcome = %v", class, err)
				}
			}
		})
	}
}

func TestCodexReviewSourceVerifyRejectsSwappedRequestAuthority(t *testing.T) {
	fx := newHandoffFixture(t)
	backend := fx.backend(t)
	review, _ := testCodexReview(t)
	id := domain.InvocationID("review-run-authority-1")
	request := exec.ReviewRequest{
		RunID: "run-1", Round: 1, Repo: "owner/repo", RepositoryID: 42, BaseRef: "main",
		BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40), Workspace: "/candidate",
		Verification: testReviewVerificationEvidence(), Instructions: testReviewInstructionBinding(), RequestedAt: codexReviewEpoch,
	}
	expected, err := request.AuthorityDigest()
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*exec.ReviewRequest){
		"ownership": func(swapped *exec.ReviewRequest) {
			swapped.RunID = "run-2"
			swapped.Round = 2
		},
		"cross repository": func(swapped *exec.ReviewRequest) {
			swapped.Repo = "other/repo"
			swapped.RepositoryID = 84
		},
		"stale base": func(swapped *exec.ReviewRequest) {
			swapped.BaseSHA = strings.Repeat("e", 40)
		},
		"instruction source": func(swapped *exec.ReviewRequest) {
			swapped.Instructions.RepositorySources = []exec.ReviewInstructionSource{{
				Path: "AGENTS.md", Digest: domain.Digest("sha256:" + strings.Repeat("f", 64)),
			}}
		},
		"instruction result": func(swapped *exec.ReviewRequest) {
			swapped.Instructions.ResultDigest = domain.Digest("sha256:" + strings.Repeat("1", 64))
		},
		"workspace": func(swapped *exec.ReviewRequest) {
			swapped.Workspace = "/swapped-candidate"
		},
		"verification": func(swapped *exec.ReviewRequest) {
			swapped.Verification.EvidenceSnapshotDigest = domain.Digest("sha256:" + strings.Repeat("f", 64))
		},
	} {
		t.Run(name, func(t *testing.T) {
			swapped := request
			mutate(&swapped)
			journal := &fakeCodexReviewJournal{
				requests: map[string]exec.ReviewRequest{string(id): swapped},
			}
			source := &CodexReviewSource{cfg: CodexReviewSourceConfig{
				Backend: backend, Review: review, Journal: journal,
			}}
			if err := source.VerifyRequestAuthority(t.Context(), id, expected); !errors.Is(err, domain.ErrParentKeyMismatch) {
				t.Fatalf("swapped request authority verification = %v", err)
			}
		})
	}
}

func TestCodexReviewSourceOutcomeRejectsFindingCorruption(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	source := &CodexReviewSource{cfg: CodexReviewSourceConfig{
		Review:              CodexReviewConfig{Model: "gpt-codex", ReasoningEffort: "high"},
		ConfigurationDigest: domain.Digest("sha256:" + strings.Repeat("c", 64)),
		CostOwner:           "owner", Now: func() time.Time { return now },
	}}
	request := exec.ReviewRequest{
		RunID: "run-1", Round: 1, Repo: "owner/repo", RepositoryID: 42, BaseRef: "main",
		BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40), Workspace: "/candidate",
		Verification: testReviewVerificationEvidence(), Instructions: testReviewInstructionBinding(), RequestedAt: now,
	}
	outcome := source.normalizeCollection("review-invocation-1", request, CodexReviewCollection{
		Result: []byte(`{"findings":[{"severity":"P1","location":"main.go:1","explanation":"unsafe"}]}`),
	})
	if err := outcome.Validate(); err != nil {
		t.Fatal(err)
	}
	outcome.Result.Findings = nil
	if err := outcome.Validate(); !errors.Is(err, domain.ErrInvalidReviewCompletionEvidence) {
		t.Fatalf("corrupted outcome validation = %v", err)
	}
}

func TestCodexReviewConfigurationDigestBindsEffectiveInputs(t *testing.T) {
	cfg, request := testCodexReview(t)
	digest := func(
		config CodexReviewConfig,
		size int64,
		authMode CodexAuthMode,
		identity domain.AuthIdentityID,
		costOwner string,
	) domain.Digest {
		t.Helper()
		got, err := CodexReviewConfigurationDigest(
			config, size, authMode, identity, costOwner,
		)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	base := digest(cfg, 64, request.AuthMode, request.AuthIdentityID, "subscription:owner")
	reordered := cfg
	reordered.ProviderEndpoints = slices.Clone(cfg.ProviderEndpoints)
	slices.Reverse(reordered.ProviderEndpoints)
	if got := digest(reordered, 64, request.AuthMode, request.AuthIdentityID,
		"subscription:owner"); got != base {
		t.Fatalf("endpoint order changed digest: %q != %q", got, base)
	}
	mutated := cfg
	mutated.Model += "-different"
	if got := digest(mutated, 64, request.AuthMode, request.AuthIdentityID,
		"subscription:owner"); got == base {
		t.Fatal("model change did not change configuration digest")
	}
	if got := digest(cfg, 65, request.AuthMode, request.AuthIdentityID,
		"subscription:owner"); got == base {
		t.Fatal("workspace size change did not change configuration digest")
	}
	if got := digest(cfg, 64, request.AuthMode, request.AuthIdentityID,
		"different-owner"); got == base {
		t.Fatal("cost owner change did not change configuration digest")
	}
}

func TestClassifyCodexLaunchFailureKeepsRuntimePreparationRetryable(t *testing.T) {
	if got := classifyCodexLaunchFailure(errors.New("runtime unavailable")); got != domain.ReviewFailureTransient {
		t.Fatalf("runtime failure class = %q", got)
	}
	if got := classifyCodexLaunchFailure(ErrInvalidCodexReviewSpec); got != domain.ReviewFailureConfiguration {
		t.Fatalf("invalid spec class = %q", got)
	}
	if got := classifyCodexLaunchFailure(fmt.Errorf("%w: create volume", ErrCodexReviewOperational)); got != domain.ReviewFailureTransient {
		t.Fatalf("operational preparation class = %q", got)
	}
	operationalConformance := codexReviewOperationalCheckf(
		CheckControlPlaneIsolation, "create volume: %v", errors.New("runtime unavailable"),
	)
	if !errors.Is(operationalConformance, ErrConformance) {
		t.Fatal("operational failure lost its check context")
	}
	if got := classifyCodexLaunchFailure(operationalConformance); got != domain.ReviewFailureTransient {
		t.Fatalf("operational check class = %q", got)
	}
	if got := classifyCodexObservationFailure(errors.New("runtime inspect failed")); got != domain.ReviewFailureTransient {
		t.Fatalf("operational observation class = %q", got)
	}
	if got := classifyCodexObservationFailure(ErrConformance); got != domain.ReviewFailureContradiction {
		t.Fatalf("authenticated observation contradiction class = %q", got)
	}
}

func TestRuntimeCodexReviewVolumeLeaseTransfersAtomically(t *testing.T) {
	ctx := context.Background()
	runtime := newFakeRuntime(t)
	for _, volume := range []string{"workspace", "shadow"} {
		if err := runtime.CreateVolume(ctx, volume, 2, nil); err != nil {
			t.Fatal(err)
		}
	}
	leaser, err := NewRuntimeCodexReviewVolumeLeaser(runtime)
	if err != nil {
		t.Fatal(err)
	}
	volumes := []string{"workspace", "shadow"}
	lease, err := leaser.AcquireCodexReviewVolumeLease(ctx, "owner", volumes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := leaser.AcquireCodexReviewVolumeLease(ctx, "foreign", volumes); !errors.Is(err, ErrCodexReviewVolumeLeaseForeignOwner) {
		t.Fatalf("foreign acquire = %v", err)
	}
	if err := runtime.CreateContainer(ctx, ContainerSpec{
		Name: "review", Image: "image", Command: []string{"true"},
		Labels: []Label{{Key: ownershipLabelKey, Value: "owner"}},
		Mounts: []Mount{
			{Type: MountVolume, Source: "workspace", Target: "/workspace", ReadOnly: true},
			{Type: MountVolume, Source: "shadow", Target: "/.agents", ReadOnly: true},
		},
	}); err != nil {
		t.Fatal(err)
	}
	createdRestart, err := NewRuntimeCodexReviewVolumeLeaser(runtime)
	if err != nil {
		t.Fatal(err)
	}
	if _, transfer, err := createdRestart.RecoverCodexReviewVolumeLease(
		ctx, "owner", volumes,
	); !errors.Is(err, ErrCodexReviewVolumeLeaseTransferred) || transfer.Container != "review" {
		t.Fatalf("created attachment recovery = %#v, %v", transfer, err)
	}
	if err := lease.StartCodexReviewContainer(ctx, "review"); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewRuntimeCodexReviewVolumeLeaser(runtime)
	if err != nil {
		t.Fatal(err)
	}
	if _, transfer, err := restarted.RecoverCodexReviewVolumeLease(ctx, "owner", volumes); !errors.Is(err, ErrCodexReviewVolumeLeaseTransferred) || transfer.Container != "review" {
		t.Fatalf("transferred recovery = %#v, %v", transfer, err)
	}
	runtime.onInspect = func(id string, report InspectReport) (InspectReport, error) {
		if id == "review" {
			report.Labels = []Label{{Key: ownershipLabelKey, Value: "foreign"}}
		}
		return report, nil
	}
	if _, _, err := restarted.RecoverCodexReviewVolumeLease(ctx, "owner", volumes); !errors.Is(err, ErrCodexReviewVolumeLeaseForeignOwner) {
		t.Fatalf("cached transfer accepted foreign replacement: %v", err)
	}
	runtime.onInspect = nil
	_, _ = runtime.Inspect(ctx, "review")
	_, _ = runtime.Inspect(ctx, "review")
	if err := runtime.DeleteContainer(ctx, "review"); err != nil {
		t.Fatal(err)
	}
	recovered, _, err := restarted.RecoverCodexReviewVolumeLease(ctx, "owner", volumes)
	if err != nil {
		t.Fatal(err)
	}
	if err := recovered.ReleaseCodexReviewVolumeLease(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestRuntimeCodexReviewVolumeLeaseRecoversMultiTargetShadowContainer exercises
// the exact realized #591 review-container mount shape through recovery: the
// workspace mounted once, the snapshot once, and the .agents shadow at every
// in-container ancestor target. A leased volume attached many times and a total
// volume-mount count far above the leased-set size must still authenticate as
// the atomic transfer; the pre-#591 reconstruct rejected this shape outright.
func TestRuntimeCodexReviewVolumeLeaseRecoversMultiTargetShadowContainer(t *testing.T) {
	ctx := context.Background()
	runtime := newFakeRuntime(t)
	volumes := []string{"workspace", "shadow", "snapshot"}
	for _, volume := range volumes {
		if err := runtime.CreateVolume(ctx, volume, 2, nil); err != nil {
			t.Fatal(err)
		}
	}
	mounts := []Mount{
		{Type: MountVolume, Source: "workspace", Target: "/workspace/project", ReadOnly: true},
		{Type: MountVolume, Source: "snapshot", Target: codexReviewSnapshotTarget, ReadOnly: true},
	}
	for _, target := range codexAgentsShadowTargets("/workspace/project", codexWorkspaceAgentsDir) {
		mounts = append(mounts, Mount{Type: MountVolume, Source: "shadow", Target: target, ReadOnly: true})
	}
	if err := runtime.CreateContainer(ctx, ContainerSpec{
		Name: "review", Image: "image", Command: []string{"true"},
		Labels: []Label{{Key: ownershipLabelKey, Value: "owner"}},
		Mounts: mounts,
	}); err != nil {
		t.Fatal(err)
	}
	leaser, err := NewRuntimeCodexReviewVolumeLeaser(runtime)
	if err != nil {
		t.Fatal(err)
	}
	_, transfer, err := leaser.RecoverCodexReviewVolumeLease(ctx, "owner", volumes)
	if !errors.Is(err, ErrCodexReviewVolumeLeaseTransferred) {
		t.Fatalf("multi-target-shadow recovery = %v, want transferred", err)
	}
	if transfer.Container != "review" || !slices.Equal(transfer.Volumes, volumes) {
		t.Fatalf("transfer = %#v, want container review over %v", transfer, volumes)
	}
	// A container attaching a proper subset of the leased volumes is not the
	// atomic transfer and must fail closed, even alongside the multi-mount
	// shadow container that does attach the full set.
	if err := runtime.CreateContainer(ctx, ContainerSpec{
		Name: "partial", Image: "image", Command: []string{"true"},
		Labels: []Label{{Key: ownershipLabelKey, Value: "owner"}},
		Mounts: []Mount{
			{Type: MountVolume, Source: "workspace", Target: "/workspace/project", ReadOnly: true},
		},
	}); err != nil {
		t.Fatal(err)
	}
	fresh, err := NewRuntimeCodexReviewVolumeLeaser(runtime)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := fresh.RecoverCodexReviewVolumeLease(ctx, "owner", volumes); !errors.Is(err, ErrCodexReviewVolumeLeaseForeignOwner) {
		t.Fatalf("subset attachment = %v, want foreign refusal", err)
	}
}

// TestClassifyCodexLaunchFailure pins the launch-path failure classification
// and its precedence (operational > contradiction > configuration). The
// classifier keys only off error-class sentinels and never inspects
// ConformanceFailure.Check, so each audit-named contradiction family below is
// an equivalence-class representative: every ErrConformance failure reachable
// before start classifies identically as a contradiction (#499).
func TestClassifyCodexLaunchFailure(t *testing.T) {
	conformance := func(c Check) error {
		return &ConformanceFailure{Backend: BackendName, Check: c, Reason: "test"}
	}
	cases := []struct {
		name string
		err  error
		want domain.ReviewFailureClass
	}{
		{"operational alone", ErrCodexReviewOperational, domain.ReviewFailureTransient},
		{"operational wrapped", fmt.Errorf("read: %w", ErrCodexReviewOperational), domain.ReviewFailureTransient},
		{
			"operational joined with conformance context",
			errors.Join(ErrCodexReviewOperational, conformance(CheckWorkspaceSeeding)),
			domain.ReviewFailureTransient,
		},
		{
			"operational joined with spec",
			errors.Join(ErrCodexReviewOperational, ErrInvalidCodexReviewSpec),
			domain.ReviewFailureTransient,
		},
		{"bare conformance sentinel", ErrConformance, domain.ReviewFailureContradiction},
		{"changed auth/instruction snapshot", conformance(CheckCredentialSeparation), domain.ReviewFailureContradiction},
		{"command/mount divergence", conformance(CheckObservedBaseIdentity), domain.ReviewFailureContradiction},
		{"invalid or divergent journal binding", conformance(CheckWorkspaceSeeding), domain.ReviewFailureContradiction},
		{"foreign/unprovable owned object", conformance(CheckTeardown), domain.ReviewFailureContradiction},
		{"persisted binding disagreement", conformance(CheckControlPlaneIsolation), domain.ReviewFailureContradiction},
		{
			"conformance joined with spec takes the loud branch",
			errors.Join(conformance(CheckWorkspaceSeeding), ErrInvalidCodexReviewSpec),
			domain.ReviewFailureContradiction,
		},
		{"invalid static spec", ErrInvalidCodexReviewSpec, domain.ReviewFailureConfiguration},
		{"invalid static spec wrapped", fmt.Errorf("%w: bad prompt", ErrInvalidCodexReviewSpec), domain.ReviewFailureConfiguration},
		{"unknown error", errors.New("boom"), domain.ReviewFailureTransient},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyCodexLaunchFailure(tc.err); got != tc.want {
				t.Fatalf("classifyCodexLaunchFailure(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// TestCodexReviewSourceLaunchConformanceIsContradiction proves the wiring:
// a conformance failure produced during workspace preparation (a divergent
// persisted workspace binding) surfaces to the engine as a
// ReviewFailureContradiction, not the repairable ReviewFailureConfiguration
// the launch classifier previously emitted for every conformance failure.
func TestCodexReviewSourceLaunchConformanceIsContradiction(t *testing.T) {
	ctx := context.Background()
	fx := newHandoffFixture(t)
	seedSpec := fx.seed(t)
	backend := fx.backend(t)
	cfg, requestSpec := testCodexReview(t)
	journal := &fakeCodexReviewJournal{}
	sourceConfig := codexReviewSourceConfigForTest(t, backend, cfg, requestSpec, journal)
	source, err := NewCodexReviewSource(sourceConfig)
	if err != nil {
		t.Fatal(err)
	}
	id := domain.InvocationID("review-divergent-binding-1")
	// A persisted workspace binding whose stored Volume disagrees with the
	// deterministic name for this invocation. Its empty CreationFingerprint
	// routes startRequestedReview into PrepareCodexReviewWorkspace, which fails
	// the CheckWorkspaceSeeding conformance gate on the divergence.
	journal.workspaceBinding = CodexReviewWorkspaceBinding{
		SourceRunID: string(id), Volume: "codex-review-foreign-volume",
		OwnershipToken: "stored-token",
	}
	request := exec.ReviewRequest{
		RunID: "run-1", Round: 1, Repo: seedSpec.Seed.Base.Repo,
		RepositoryID: seedSpec.Seed.Base.RepositoryID, BaseRef: seedSpec.Seed.Base.BaseRef,
		BaseSHA: strings.Repeat("a", 40), HeadSHA: seedSpec.Seed.Base.BaseSHA,
		Workspace: seedSpec.Seed.SourceDir, Verification: testReviewVerificationEvidence(),
		Instructions: testReviewInstructionBinding(), RequestedAt: codexReviewEpoch,
	}
	var failure *exec.ReviewSourceFailure
	if err := source.RequestReview(ctx, id, request); !errors.As(err, &failure) ||
		failure.Class != domain.ReviewFailureContradiction {
		t.Fatalf("launch conformance failure = %v (class %v), want contradiction",
			err, reviewSourceFailureClass(err))
	}
	if !errors.Is(failure.Err, ErrConformance) {
		t.Fatalf("launch failure did not carry the conformance sentinel: %v", failure.Err)
	}
}

func reviewSourceFailureClass(err error) domain.ReviewFailureClass {
	var failure *exec.ReviewSourceFailure
	if errors.As(err, &failure) {
		return failure.Class
	}
	return ""
}

// TestReconcileRejectedRequestDispatchEquivalence measures that the no-default
// switch classifying a rejected intent's launch state (reconcileRejectedRequest,
// codex_review_source.go) decides identically to the if/else it replaced on
// every input that reaches the switch, and records the intended divergence on
// the inputs the closed/not-found branch and validateIdentity make unreachable
// before it. Per the trust-boundary refactor discipline this is a harness over
// a corpus, not a diff-read: the dispatch owns credential-bearing teardown.
func TestReconcileRejectedRequestDispatchEquivalence(t *testing.T) {
	type branch int
	const (
		branchContradictionPreparing branch = iota
		branchRejectedTeardown
		branchFailClosed
	)
	// oldDispatch reconstructs the pre-refactor if/else: the preparing state
	// returned the pre-binding contradiction, every other value fell through to
	// the rejected-teardown arm.
	oldDispatch := func(s CodexReviewIntentState) branch {
		if s == CodexReviewIntentPreparing {
			return branchContradictionPreparing
		}
		return branchRejectedTeardown
	}
	// newDispatch mirrors the no-default switch now in reconcileRejectedRequest,
	// including the empty closed case that falls to the fail-closed return.
	newDispatch := func(s CodexReviewIntentState) branch {
		switch s {
		case CodexReviewIntentPreparing:
			return branchContradictionPreparing
		case CodexReviewIntentPrepared, CodexReviewIntentStarting, CodexReviewIntentStarted:
			return branchRejectedTeardown
		case CodexReviewIntentClosed:
		}
		return branchFailClosed
	}
	// Reachable inputs: validateIdentity guarantees State is a registered
	// member, and the closed and not-found cases return before the switch, so
	// only the four pre-terminal states arrive. Old and new must agree here.
	for _, s := range []CodexReviewIntentState{
		CodexReviewIntentPreparing, CodexReviewIntentPrepared,
		CodexReviewIntentStarting, CodexReviewIntentStarted,
	} {
		if oldDispatch(s) != newDispatch(s) {
			t.Errorf("reachable state %q: old=%d new=%d", s, oldDispatch(s), newDispatch(s))
		}
	}
	// Unreachable inputs: a closed intent settles above; the zero value and any
	// unregistered token are rejected upstream by validateIdentity. The refactor
	// intentionally routes them to the fail-closed return rather than the old
	// rejected-teardown fall-through. Assert both halves so the divergence is
	// recorded and guarded, not accidental.
	for _, s := range []CodexReviewIntentState{CodexReviewIntentClosed, "", "resuming", "STARTED"} {
		if oldDispatch(s) != branchRejectedTeardown {
			t.Errorf("guard premise broke: old dispatch for %q = %d, want rejected-teardown",
				s, oldDispatch(s))
		}
		if newDispatch(s) != branchFailClosed {
			t.Errorf("unreachable state %q: new dispatch = %d, want fail-closed", s, newDispatch(s))
		}
	}
}
