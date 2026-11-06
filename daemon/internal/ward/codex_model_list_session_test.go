package ward

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/execfixture"
)

// fakeCodexAppServer answers the probe's requests as the app-server does:
// a notification before the initialize answer, then a model/list page per
// cursor, with a server-sent request reusing the second page's id ahead of
// its answer. At end of input it exits with $FAKE_EXIT; FAKE_CRASH makes it
// exit before reading anything.
const fakeCodexAppServer = `#!/bin/sh
[ -z "$FAKE_CRASH" ] || exit "$FAKE_CRASH"
while IFS= read -r line; do
	id="$(printf '%s' "$line" | jq -r '.id // empty')"
	method="$(printf '%s' "$line" | jq -r '.method')"
	case "$method" in
	initialize)
		printf '%s\n' '{"method":"remoteControl/status/changed","params":{}}'
		printf '{"id":%s,"result":{}}\n' "$id" ;;
	model/list)
		cursor="$(printf '%s' "$line" | jq -r '.params.cursor // empty')"
		if [ -z "$cursor" ]; then
			printf '{"id":%s,"result":{"data":[{"model":"gpt-a","displayName":"A","supportedReasoningEfforts":[{"reasoningEffort":"low"}]}],"nextCursor":"$(x) '"'"'q"}}\n' "$id"
		else
			printf '{"id":%s,"method":"item/tool/requestUserInput","params":{}}\n' "$id"
			sleep 1
			printf '{"id":%s,"result":{"data":[{"model":"gpt-b","displayName":"B","supportedReasoningEfforts":[]}],"nextCursor":null}}\n' "$id"
		fi ;;
	esac
done
exit "${FAKE_EXIT:-0}"
`

// runCodexModelListSession runs the probe's session script against the fake
// app-server, its container paths moved under a temporary root, and returns
// the output the export would carry and how long the script ran.
func runCodexModelListSession(t *testing.T, env ...string) (LineupProbeOutput, string, time.Duration) {
	t.Helper()
	for _, tool := range []string{"jq", "mkfifo"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("the session script needs %s", tool)
		}
	}
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	for _, dir := range []string{bin, filepath.Join(root, "out")} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	execfixture.Write(t, filepath.Join(bin, "codex"), []byte(fakeCodexAppServer), 0o700)
	script := strings.ReplaceAll(codexModelListSession(), lineupProbeRoot, root) +
		`printf '%s\n' "$status"`
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", script) //nolint:gosec // the generated probe script under test
	cmd.Dir = root
	cmd.Env = append(os.Environ(), append(env, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))...)
	started := time.Now()
	statusLine, err := cmd.Output()
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("session script: %v", err)
	}
	status, err := strconv.Atoi(strings.TrimSpace(string(statusLine)))
	if err != nil {
		t.Fatalf("status line %q", statusLine)
	}
	stdout, err := os.ReadFile(filepath.Join(root, "out", "stdout")) //nolint:gosec // test-owned temporary root
	if err != nil {
		t.Fatal(err)
	}
	return LineupProbeOutput{Version: []byte("codex-cli 0.162.1\n"), Stdout: stdout, ExitStatus: status},
		filepath.Join(root, "input"), elapsed
}

// TestCodexModelListSessionPagesTheAppServer runs the generated script, not
// only its golden: it waits for each answer past a notification and a
// server-sent request carrying the next page's id, passes a cursor holding
// shell syntax through as data, records the app-server's status, and
// removes its input pipe.
func TestCodexModelListSessionPagesTheAppServer(t *testing.T) {
	t.Parallel()
	out, input, _ := runCodexModelListSession(t)
	report, err := DecodeCodexModelReport(out, codexModelListCredential())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := []domain.DiscoveredModel{
		{ID: "gpt-a", DisplayName: "A", EffortLevels: []string{"low"}},
		{ID: "gpt-b", DisplayName: "B", EffortLevels: []string{}},
	}
	if !reflect.DeepEqual(report.Models, want) {
		t.Fatalf("models = %+v, want %+v", report.Models, want)
	}
	if _, err := os.Lstat(input); !os.IsNotExist(err) {
		t.Fatalf("the input pipe remains: %v", err)
	}

	out, _, _ = runCodexModelListSession(t, "FAKE_EXIT=3")
	if out.ExitStatus != 3 {
		t.Fatalf("status = %d, want the app-server's 3", out.ExitStatus)
	}
}

// TestCodexModelListSessionEndsWhenTheAppServerExits: an app-server that
// exits before answering ends the wait at once, well inside the answer
// budget, and its status is recorded.
func TestCodexModelListSessionEndsWhenTheAppServerExits(t *testing.T) {
	t.Parallel()
	out, _, elapsed := runCodexModelListSession(t, "FAKE_CRASH=7")
	if out.ExitStatus != 7 {
		t.Fatalf("status = %d, want the app-server's 7", out.ExitStatus)
	}
	if elapsed > 30*time.Second {
		t.Fatalf("the script waited %v on an app-server that had exited", elapsed)
	}
	if _, err := DecodeCodexModelReport(out, codexModelListCredential()); err == nil {
		t.Fatal("decoded a session with no answers")
	}
}
