package projectimage

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// buildLogs lists the step log files and timing records under a log
// directory, keyed by path relative to it.
func buildLogs(t *testing.T, logDir string) map[string]os.FileInfo {
	t.Helper()
	found := map[string]os.FileInfo{}
	err := filepath.Walk(logDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(logDir, path)
		if err != nil {
			return err
		}
		found[relative] = info
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

func TestBuildKeepsEachCommandsWholeOutputInPrivateFiles(t *testing.T) {
	t.Parallel()
	backend := newFakeBackend()
	backend.failRecipe = "npm"
	backend.recipeOutput = []byte("--- FAIL: TestEarly (0.00s)\nFAIL\tgithub.com/example/early\t0.1s\n" +
		strings.Repeat("ok  \tgithub.com/example/fine\t0.1s\n", 100_000))
	logDir := filepath.Join(t.TempDir(), "project-image-logs")
	var log bytes.Buffer
	builder := newBuilder(&fakeSource{}, backend, t.TempDir())
	builder.log, builder.logDir = &log, logDir

	_, err := builder.Build(t.Context(), validRequest())
	if !errors.Is(err, ErrProofFailed) {
		t.Fatalf("Build = %v, want ErrProofFailed", err)
	}
	imageDir := filepath.Join(logDir, "freeside-project-freeasinbird-gh-imgup")
	builds, readErr := os.ReadDir(imageDir)
	if readErr != nil || len(builds) != 1 {
		t.Fatalf("build directories = %v, %v; want one", builds, readErr)
	}
	buildDir := filepath.Join(imageDir, builds[0].Name())
	recipeLog := filepath.Join(buildDir, "07-positive-recipe-command-0.log")
	for _, want := range []string{
		"failing Go tests:\n--- FAIL: TestEarly (0.00s)\nFAIL\tgithub.com/example/early\t0.1s\noutput tail:\n",
		"\nfull output: " + recipeLog + "): ",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Build error = %.600q..., want it to contain %q", err, want)
		}
	}
	if got, readErr := os.ReadFile(recipeLog); readErr != nil || !bytes.Equal(got, backend.recipeOutput) { //nolint:gosec // G304: a file the test's own build wrote under its temp directory
		t.Fatalf("recipe log holds %d bytes (%v), want the whole %d-byte output", len(got), readErr, len(backend.recipeOutput))
	}
	if got, _ := os.ReadFile(filepath.Join(buildDir, "02-container-build.log")); string(got) != "STEP 1/9: FROM base\n" { //nolint:gosec // G304: a file the test's own build wrote under its temp directory
		t.Fatalf("container build log = %q", got)
	}
	for path, info := range buildLogs(t, logDir) {
		want := os.FileMode(0o600)
		if info.IsDir() {
			want = os.ModeDir | 0o700
		}
		if info.Mode() != want {
			t.Errorf("%s mode = %v, want %v", path, info.Mode(), want)
		}
	}
	for _, want := range []string{
		"project-image: logs for this build: " + buildDir + "\n",
		"project-image: source materialization started: freeasinbird/gh-imgup at " + testCommit + "\n",
		"project-image: container build started: freeside-project-freeasinbird-gh-imgup:",
		"project-image: positive recipe command 0 started: [\"npm\" \"run\" \"lint\"]; output in " + recipeLog + "\n",
	} {
		if !strings.Contains(log.String(), want) {
			t.Fatalf("log = %s\nwant line %q", log.String(), want)
		}
	}

	var record timingRecord
	body, readErr := os.ReadFile(filepath.Join(buildDir, timingFileName)) //nolint:gosec // G304: the record the test's own build wrote
	if readErr != nil {
		t.Fatal(readErr)
	}
	if err := json.Unmarshal(body, &record); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, step := range record.Steps {
		names = append(names, step.Name+"="+step.Outcome)
	}
	want := "source materialization=passed container build=passed provenance=passed allowlist=passed " +
		"offline preflight=passed positive hydration 0=passed positive recipe command 0=failed"
	if strings.Join(names, " ") != want {
		t.Fatalf("timing record steps = %q, want %q", names, want)
	}
}

func TestBuildFailsBeforeAnyStepWhenItCannotCreateItsLogDirectory(t *testing.T) {
	t.Parallel()
	// A regular file where the log directory's parent belongs fails the
	// mkdir for every user; a read-only directory would not stop root, and
	// the project-image proof runs this package's tests as root (#3543).
	notADirectory := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notADirectory, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	source, backend := &fakeSource{}, newFakeBackend()
	builder := newBuilder(source, backend, t.TempDir())
	builder.logDir = filepath.Join(notADirectory, "logs")

	if _, err := builder.Build(t.Context(), validRequest()); err == nil ||
		!strings.Contains(err.Error(), "create project-image log directory") {
		t.Fatalf("Build = %v, want a log directory failure", err)
	}
	if source.fetches+source.copies != 0 || len(backend.builds) != 0 {
		t.Fatalf("source calls %d, builds %d; want none", source.fetches+source.copies, len(backend.builds))
	}
}

// A log that can't be written must never fail the command writing to it,
// which would turn a proof verdict into a runtime fault.
func TestStepWriterSwallowsALogWriteFailure(t *testing.T) {
	t.Parallel()
	log := openStepLog(t.TempDir(), 1, "positive recipe command 0")
	if err := log.file.Close(); err != nil {
		t.Fatal(err)
	}
	writer := &stepWriter{file: log}
	payload := []byte("--- FAIL: TestKept (0.00s)\n")
	if n, err := writer.Write(payload); n != len(payload) || err != nil {
		t.Fatalf("Write = %d, %v; want %d, nil", n, err, len(payload))
	}
	step := &buildStep{output: writer}
	if note := step.logNote(); !strings.HasPrefix(note, "full output incomplete: "+log.path+": ") {
		t.Fatalf("log note = %q, want it to say the log is incomplete", note)
	}
	if got := step.proofOutput(nil); !strings.Contains(got, "--- FAIL: TestKept (0.00s)") {
		t.Fatalf("proof output = %q, want the failing test despite the log failure", got)
	}
}

func TestStepLogStopsAtItsLimitWithAClosingLine(t *testing.T) {
	t.Parallel()
	log := openStepLog(t.TempDir(), 1, "container build")
	log.limit = 8
	log.write([]byte("12345"))
	log.write([]byte("6789abc"))
	log.write([]byte("never written"))
	log.close()
	got, err := os.ReadFile(log.path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "12345678\n[freeside: log stopped at 8 bytes; the step's output went on]\n" || log.err != nil {
		t.Fatalf("log = %q (%v)", got, log.err)
	}
	step := &buildStep{output: &stepWriter{file: log}}
	if note := step.logNote(); note != "full output, cut at 8 bytes: "+log.path {
		t.Fatalf("log note = %q", note)
	}
}

// The heartbeat runs on a real time.Ticker, so it is tested in a synctest
// bubble: the fake build sleeps two and a half intervals of fake time.
func TestBuildHeartbeatNamesTheStepAndTheLastBuildsTime(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		logDir := t.TempDir()
		build := func() string {
			backend := newFakeBackend()
			backend.buildDelay = 150 * time.Second
			var log bytes.Buffer
			builder := newBuilder(&fakeSource{}, backend, t.TempDir())
			builder.log, builder.logDir, builder.heartbeat = &log, logDir, time.Minute
			if _, err := builder.Build(t.Context(), validRequest()); err != nil {
				t.Fatal(err)
			}
			var beats []string
			for line := range strings.Lines(log.String()) {
				if strings.HasPrefix(line, "project-image: still ") {
					beats = append(beats, strings.TrimSpace(line))
				}
			}
			return strings.Join(beats, "\n")
		}

		first := "project-image: still container build: 1m0s elapsed; no earlier build on record\n" +
			"project-image: still container build: 2m0s elapsed; no earlier build on record"
		if got := build(); got != first {
			t.Fatalf("first build heartbeats =\n%s\nwant\n%s", got, first)
		}
		// The bubble's clock moves only when every goroutine is blocked, so a
		// ticker left running would leave Build's caller waiting here forever
		// and fail the test as a deadlock.
		synctest.Wait()
		time.Sleep(time.Second)
		second := "project-image: still container build: 1m0s elapsed; it took 2m30s in the last recorded build\n" +
			"project-image: still container build: 2m0s elapsed; it took 2m30s in the last recorded build"
		if got := build(); got != second {
			t.Fatalf("second build heartbeats =\n%s\nwant\n%s", got, second)
		}
	})
}
