package claudeinference

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/inference"
	"github.com/freeside-ai/freeside/daemon/internal/inference/contract"
)

// TestDriverContract runs the shared inference contract against the Claude
// driver with a scripted CLI. The CLI runs with PATH=/usr/bin:/bin and a
// private HOME, so every path it touches is baked into its script.
func TestDriverContract(t *testing.T) {
	records := map[inference.Driver]string{}
	contract.Run(t, contract.Factory{
		New: func(t *testing.T, script contract.Script) (inference.Driver, <-chan struct{}) {
			t.Helper()
			switch {
			case script.Block:
				return blockingContractDriver(t)
			case script.Fail:
				return testDriver(t, "exit 7\n"), nil
			}
			dir := t.TempDir()
			answer := filepath.Join(dir, "completion.json")
			if err := os.WriteFile(answer, contractCompletion(t, script.Response), 0o600); err != nil {
				t.Fatal(err)
			}
			// Each call records its stdin and NUL-separated arguments under
			// its own process ID, then prints the scripted completion.
			driver := testDriver(t, "dir="+shellQuote(dir)+"\n"+
				`cat > "$dir/$$.stdin" || exit 3`+"\n"+
				`printf '%s\0' "$@" > "$dir/$$.args" || exit 4`+"\n"+
				`exec /bin/cat "$dir/completion.json"`+"\n")
			records[driver] = dir
			return driver, nil
		},
		Requests: func(t *testing.T, driver inference.Driver) []inference.Request {
			t.Helper()
			return recordedRequests(t, records[driver])
		},
	})
}

// contractCompletion is the CLI's account of an answer that decodes to
// response: its result text, its output tokens, and the one model that
// answered.
func contractCompletion(t *testing.T, response inference.Response) []byte {
	t.Helper()
	m := completion()
	m["result"] = string(response.Output)
	m["usage"].(map[string]any)["output_tokens"] = response.ComputeUnits
	m["modelUsage"] = map[string]any{response.Observed.ModelID: map[string]any{
		"outputTokens": response.Observed.OutputTokens, "webSearchRequests": 0,
	}}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// blockingContractDriver returns a driver whose CLI reports itself started
// through a FIFO and then sleeps until the driver kills its process group,
// as TestCompleteAndConfirmCancellationJoinsNativeGroup does.
func blockingContractDriver(t *testing.T) (inference.Driver, <-chan struct{}) {
	t.Helper()
	readyPath := filepath.Join(t.TempDir(), "ready")
	if err := syscall.Mkfifo(readyPath, 0o600); err != nil {
		t.Fatal(err)
	}
	ready, err := os.OpenFile(readyPath, os.O_RDWR, 0o600) // #nosec G304 -- private test FIFO.
	if err != nil {
		t.Fatal(err)
	}
	// On darwin Go doesn't poll a FIFO, so Close alone won't interrupt the
	// reader below when the CLI never ran; a line releases it first.
	t.Cleanup(func() {
		_, _ = ready.WriteString("\n")
		_ = ready.Close()
	})
	started := make(chan struct{})
	go func() {
		if _, err := bufio.NewReader(ready).ReadString('\n'); err == nil {
			close(started)
		}
	}()
	return testDriver(t, "printf 'ready\\n' > "+shellQuote(readyPath)+"\nexec /bin/sleep 600\n"), started
}

// recordedRequests rebuilds each call the scripted CLI recorded: its Fields
// from stdin and its Model from the argument after --model.
func recordedRequests(t *testing.T, dir string) []inference.Request {
	t.Helper()
	stdins, err := filepath.Glob(filepath.Join(dir, "*.stdin"))
	if err != nil {
		t.Fatal(err)
	}
	requests := make([]inference.Request, 0, len(stdins))
	for _, stdin := range stdins {
		body, err := os.ReadFile(stdin) // #nosec G304 -- the test's own record.
		if err != nil {
			t.Fatal(err)
		}
		var request inference.Request
		if err := json.Unmarshal(body, &request.Fields); err != nil {
			t.Fatal(err)
		}
		rawArgs, err := os.ReadFile(strings.TrimSuffix(stdin, ".stdin") + ".args") // #nosec G304 -- the test's own record.
		if err != nil {
			t.Fatal(err)
		}
		args := strings.Split(strings.TrimSuffix(string(rawArgs), "\x00"), "\x00")
		if i := slices.Index(args, "--model"); i >= 0 && i+1 < len(args) {
			request.Model = args[i+1]
		}
		requests = append(requests, request)
	}
	return requests
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
