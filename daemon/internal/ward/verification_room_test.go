package ward

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
)

func verificationProjectImage(t *testing.T, preparation []string) domain.ProjectImage {
	t.Helper()
	image, err := domain.NewProjectImage(domain.ProjectImageInput{
		Repository: "owner/repo", RepositoryID: 42, CommitSHA: strings.Repeat("a", 40),
		RecipeDigest:       domain.Digest("sha256:" + strings.Repeat("b", 64)),
		PreparationCommand: preparation,
		BaseImageRef:       domain.ImageRef("ghcr.io/owner/base@sha256:" + strings.Repeat("c", 64)),
		ImageRef:           domain.ImageRef("ghcr.io/owner/project@sha256:" + strings.Repeat("d", 64)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return image
}

func verificationProjectImageWithRecipe(t *testing.T, recipe []byte) domain.ProjectImage {
	t.Helper()
	image, err := domain.NewProjectImage(domain.ProjectImageInput{
		Repository: "owner/repo", RepositoryID: 42, CommitSHA: strings.Repeat("a", 40),
		RecipeDigest:       verify.RecipeDigest(recipe),
		PreparationCommand: []string{"prepare", "--offline"},
		BaseImageRef:       domain.ImageRef("ghcr.io/owner/base@sha256:" + strings.Repeat("c", 64)),
		ImageRef:           domain.ImageRef("ghcr.io/owner/project@sha256:" + strings.Repeat("d", 64)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return image
}

func verificationRunner(
	t *testing.T,
	runtime *fakeRuntime,
	results []verify.StepResult,
	calls *[][]string,
) verificationCommand {
	t.Helper()
	return func(_ context.Context, _ string, args []string, _ int64) (verify.StepResult, error) {
		call := append([]string{}, args...)
		*calls = append(*calls, call)
		var cidPath, token string
		for index := 0; index+1 < len(args); index++ {
			switch args[index] {
			case "--cidfile":
				cidPath = args[index+1]
			case "--label":
				token = strings.TrimPrefix(args[index+1], ownershipLabelKey+"=")
			}
		}
		id := fmt.Sprintf("verification-%d", len(*calls))
		if err := os.WriteFile(cidPath, []byte(id+"\n"), 0o600); err != nil {
			return verify.StepResult{}, err
		}
		runtime.mu.Lock()
		runtime.ctrs[id] = &fakeCtr{spec: ContainerSpec{
			Labels: []Label{{Key: ownershipLabelKey, Value: token}},
		}, stopped: true, created: "created-" + id}
		runtime.mu.Unlock()
		return results[len(*calls)-1], nil
	}
}

func TestProjectImageRoomRunsPreparationAndRecipeNetworkless(t *testing.T) {
	runtime := newFakeRuntime(t)
	var calls [][]string
	room := newProjectImageRoom(
		"container", verificationProjectImage(t, []string{"prepare", "--offline"}),
		runtime, verificationRunner(t, runtime, []verify.StepResult{{}, {}}, &calls),
		nil,
		verify.DefaultMaxRoomOutputBytes,
	)
	workspace := t.TempDir()
	t.Setenv("AWS_SECRET_ACCESS_KEY", "must-not-enter-container")
	resolved, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}
	result, err := room.Run(t.Context(), workspace, []string{"verify", "argument with spaces"})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("Run = %#v, %v", result, err)
	}
	if len(calls) != 2 {
		t.Fatalf("run calls = %d, want 2", len(calls))
	}
	for _, call := range calls {
		for _, required := range []string{
			"--cidfile", "--label", "--network", "none",
			"--volume", resolved + ":/workspace", "--workdir", "/workspace",
			"--env", "HOME=/tmp/freeside-home", "LC_ALL=C", "--",
			string(room.image.ImageRef),
		} {
			if !slices.Contains(call, required) {
				t.Errorf("call lacks %q: %q", required, call)
			}
		}
		if slices.Contains(call, "must-not-enter-container") {
			t.Fatal("host credential entered verification container argv")
		}
	}
	if !slices.Equal(calls[0][len(calls[0])-2:], []string{"prepare", "--offline"}) {
		t.Errorf("preparation argv = %q", calls[0])
	}
	if !slices.Equal(calls[1][len(calls[1])-2:], []string{"verify", "argument with spaces"}) {
		t.Errorf("recipe argv = %q", calls[1])
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if len(runtime.ctrs) != 0 {
		t.Fatalf("owned verification containers survived: %v", runtime.ctrs)
	}
}

func TestProjectImageRoomReadsBoundRecipeWithoutWorkspace(t *testing.T) {
	recipe := []byte(`{"commands":[["npm","test"]],"capture":"none"}`)
	runtime := newFakeRuntime(t)
	var calls [][]string
	var maxOutput int64
	runner := verificationRunner(
		t, runtime, []verify.StepResult{{Output: recipe}}, &calls,
	)
	room := newProjectImageRoom(
		"container", verificationProjectImageWithRecipe(t, recipe), runtime, nil,
		func(ctx context.Context, path string, args []string, max int64) (verify.StepResult, error) {
			maxOutput = max
			return runner(ctx, path, args, max)
		}, verify.DefaultMaxRoomOutputBytes,
	)
	got, err := room.ReadRecipe(t.Context())
	if err != nil || !bytes.Equal(got, recipe) {
		t.Fatalf("ReadRecipe = %q, %v", got, err)
	}
	if len(calls) != 1 {
		t.Fatalf("recipe extraction calls = %d, want 1", len(calls))
	}
	call := calls[0]
	for _, forbidden := range []string{"--volume", "--workdir", "--env"} {
		if slices.Contains(call, forbidden) {
			t.Fatalf("recipe extraction carries %q: %q", forbidden, call)
		}
	}
	for _, required := range []string{"--network", "none", "--", string(room.image.ImageRef)} {
		if !slices.Contains(call, required) {
			t.Errorf("recipe extraction lacks %q: %q", required, call)
		}
	}
	if !slices.Equal(call[len(call)-2:], []string{"/bin/cat", ProjectRecipePath}) {
		t.Errorf("recipe extraction argv = %q", call)
	}
	if maxOutput != verify.DefaultMaxRecipeBytes {
		t.Errorf("recipe extraction output cap = %d, want %d", maxOutput, verify.DefaultMaxRecipeBytes)
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if len(runtime.ctrs) != 0 {
		t.Fatalf("owned recipe-extraction containers survived: %v", runtime.ctrs)
	}
}

func TestProjectImageRoomRejectsUnauthenticatedRecipeOutput(t *testing.T) {
	recipe := []byte(`{"commands":[["npm","test"]],"capture":"none"}`)
	tests := []struct {
		name   string
		result verify.StepResult
	}{
		{"digest mismatch", verify.StepResult{Output: []byte(`{"commands":[["other"]],"capture":"none"}`)}},
		{"truncated", verify.StepResult{Output: recipe, Truncated: true}},
		{"nonzero exit", verify.StepResult{ExitCode: 7, Output: []byte("cat failed")}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runtime := newFakeRuntime(t)
			var calls [][]string
			room := newProjectImageRoom(
				"container", verificationProjectImageWithRecipe(t, recipe), runtime, nil,
				verificationRunner(t, runtime, []verify.StepResult{tc.result}, &calls),
				verify.DefaultMaxRoomOutputBytes,
			)
			if _, err := room.ReadRecipe(t.Context()); err == nil {
				t.Fatal("ReadRecipe accepted unauthenticated output")
			}
		})
	}
}

func TestProjectImageRoomRecipeCancellationStillReapsOwnedContainer(t *testing.T) {
	recipe := []byte(`{"commands":[["npm","test"]],"capture":"none"}`)
	runtime := newFakeRuntime(t)
	var calls [][]string
	ctx, cancel := context.WithCancel(t.Context())
	runner := verificationRunner(t, runtime, []verify.StepResult{{ExitCode: -1}}, &calls)
	room := newProjectImageRoom(
		"container", verificationProjectImageWithRecipe(t, recipe), runtime, nil,
		func(runCtx context.Context, path string, args []string, max int64) (verify.StepResult, error) {
			result, err := runner(runCtx, path, args, max)
			cancel()
			return result, err
		}, verify.DefaultMaxRoomOutputBytes,
	)
	if _, err := room.ReadRecipe(ctx); err == nil {
		t.Fatal("canceled recipe extraction returned no error")
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if len(runtime.ctrs) != 0 {
		t.Fatal("canceled recipe extraction left its owned container")
	}
}

func TestVerificationOutputIsBounded(t *testing.T) {
	output := &verificationOutput{max: 8}
	if _, err := output.Write([]byte("0123456789abcdef")); err != nil {
		t.Fatal(err)
	}
	if got := output.buf.String(); got != "01234567" || !output.truncated {
		t.Fatalf("bounded output = %q, truncated=%t", got, output.truncated)
	}
}

func TestRecipeReadCommandSeparatesRuntimeDiagnostics(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime")
	if err := os.WriteFile( //nolint:gosec // test-owned executable runtime fixture
		path, []byte("#!/bin/sh\nprintf 'runtime progress\\n' >&2\nprintf 'recipe bytes'\n"), 0o700,
	); err != nil {
		t.Fatal(err)
	}
	result, err := runRecipeReadCommand(t.Context(), path, nil, 1<<10)
	if err != nil || result.ExitCode != 0 || result.Truncated || string(result.Output) != "recipe bytes" {
		t.Fatalf("runRecipeReadCommand = %#v, %v", result, err)
	}
}

func TestProjectImageRoomStopsAfterPreparationFailure(t *testing.T) {
	runtime := newFakeRuntime(t)
	var calls [][]string
	room := newProjectImageRoom(
		"container", verificationProjectImage(t, []string{"prepare"}), runtime,
		verificationRunner(t, runtime, []verify.StepResult{{ExitCode: 7, Output: []byte("failed")}}, &calls),
		nil,
		verify.DefaultMaxRoomOutputBytes,
	)
	result, err := room.Run(t.Context(), t.TempDir(), []string{"verify"})
	if err != nil || result.ExitCode != 7 || string(result.Output) != "failed" {
		t.Fatalf("Run = %#v, %v", result, err)
	}
	if len(calls) != 1 {
		t.Fatalf("run calls = %d, want preparation only", len(calls))
	}
}

func TestProjectImageRoomCancellationStillReapsOwnedContainer(t *testing.T) {
	runtime := newFakeRuntime(t)
	var calls [][]string
	ctx, cancel := context.WithCancel(t.Context())
	runner := verificationRunner(t, runtime, []verify.StepResult{{ExitCode: -1}}, &calls)
	room := newProjectImageRoom(
		"container", verificationProjectImage(t, []string{"prepare"}), runtime,
		func(runCtx context.Context, path string, args []string, max int64) (verify.StepResult, error) {
			result, err := runner(runCtx, path, args, max)
			cancel()
			return result, err
		}, nil, verify.DefaultMaxRoomOutputBytes,
	)
	result, err := room.Run(ctx, t.TempDir(), []string{"verify"})
	if err != nil || result.ExitCode != -1 {
		t.Fatalf("Run = %#v, %v", result, err)
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if len(runtime.ctrs) != 0 {
		t.Fatal("canceled verification left its owned container")
	}
}

func TestProjectImageRoomRefusesForeignRuntimeIdentity(t *testing.T) {
	runtime := newFakeRuntime(t)
	runner := func(_ context.Context, _ string, args []string, _ int64) (verify.StepResult, error) {
		var cidPath string
		for index := 0; index+1 < len(args); index++ {
			if args[index] == "--cidfile" {
				cidPath = args[index+1]
			}
		}
		if err := os.WriteFile(cidPath, []byte("foreign-container\n"), 0o600); err != nil {
			return verify.StepResult{}, err
		}
		runtime.mu.Lock()
		runtime.ctrs["foreign-container"] = &fakeCtr{spec: ContainerSpec{
			Labels: []Label{{Key: ownershipLabelKey, Value: "foreign"}},
		}, stopped: true, created: "foreign"}
		runtime.mu.Unlock()
		return verify.StepResult{}, nil
	}
	room := newProjectImageRoom(
		"container", verificationProjectImage(t, []string{"prepare"}), runtime,
		runner, nil, verify.DefaultMaxRoomOutputBytes,
	)
	if _, err := room.Run(t.Context(), t.TempDir(), []string{"verify"}); err == nil ||
		!strings.Contains(err.Error(), "foreign") {
		t.Fatalf("foreign identity error = %v", err)
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if _, exists := runtime.ctrs["foreign-container"]; !exists {
		t.Fatal("foreign container was deleted")
	}
}
