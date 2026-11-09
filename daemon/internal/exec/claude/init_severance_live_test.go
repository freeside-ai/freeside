package claude

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/agentbaseline"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// TestPinnedClaudeInitSeveranceLive runs the agent line every new writer
// launch selects, for a specification and a non-specification stage,
// against the pinned CLI on the agent image with only synthetic input and an
// in-container mock API, and fails unless the conformance judge reports
// context severance on its init event (#2922). The image's managed settings
// seat the policy-only cc-plugin-sec-default without a login, so this
// credential-free run loads the plugin that broke severance on 2.1.319
// unless the same file disables it. Register each container with the
// caller's held rig; never acquire a second rig.
func TestPinnedClaudeInitSeveranceLive(t *testing.T) {
	if os.Getenv("FREESIDE_INIT_SEVERANCE_LIVE") != "1" {
		t.Skip("set FREESIDE_INIT_SEVERANCE_LIVE=1, FREESIDE_PROMPT_IMAGE, FREESIDE_RIG_ACQUISITION and FREESIDE_STATE_DIR; requires a held rig and a cached image at the pinned Claude version")
	}
	fixture, err := os.ReadFile("testdata/init_severance_probe.cjs")
	if err != nil {
		t.Fatal(err)
	}
	provider := claudeProvider{volumes: testAuthStoreVolumes{volume: "provider-volume"}}
	for _, specification := range []bool{true, false} {
		// A subtest per stage releases each probe's rig authorization and
		// container before the next one binds.
		t.Run(map[bool]string{false: "implementation", true: "specification"}[specification], func(t *testing.T) {
			in := handoffCommandCase{delivery: provider.PromptDelivery(), specification: specification}.input()
			hs, err := provider.HandoffSpec(context.Background(), in)
			if err != nil {
				t.Fatal(err)
			}
			// From the privilege drop to the transcript redirect, as the usage
			// driver probe cuts it; the probe supplies the environment the root
			// shell would, with an API key in place of the token.
			_, command, ok := strings.Cut(hs.Agent.Command[2], "setpriv --reuid=")
			if !ok {
				t.Fatal("production privilege drop missing")
			}
			command, _, ok = strings.Cut(command, "> "+shellQuote(launchTranscriptPath))
			if !ok {
				t.Fatal("production transcript redirect missing")
			}
			command = "setpriv --reuid=" + strings.NewReplacer(
				shellQuote(instructionBundlePath), "'/probe/instructions.txt'",
				shellQuote(usageDriverPath), "'/probe/usage-driver.cjs'",
				shellQuote(ward.PromptFilePath), "'/probe/prompt.txt'",
			).Replace(command)
			config, err := json.Marshal(struct {
				Version string `json:"version"`
				Command string `json:"command"`
			}{strings.TrimPrefix(agentbaseline.ClaudeCodeHarnessBuild, "claude-code "), command})
			if err != nil {
				t.Fatal(err)
			}
			output := runNetworklessClaudeProbe(t, map[string][]byte{
				"probe.cjs": fixture, "prompt.txt": []byte("Reply with one word."), "config.json": config,
				"usage-driver.cjs": []byte(usageDriverScript),
			})
			transcript, status := initSeveranceProbeTranscript(t, output)
			severed, completed, err := judgeConformanceTranscript(transcript, sessionIDFor(in.InvocationID))
			if err != nil {
				t.Fatal(err)
			}
			if !severed {
				t.Fatalf("the init event does not prove context severance: %s", initEvent(transcript))
			}
			// The turn's outcome is recorded, not asserted: severance is decided
			// by the init event alone.
			t.Logf("severed; exit %s, turn completed %t", status, completed)
		})
	}
}

// initSeveranceProbeTranscript finds the fixture's report among the
// container's log lines and returns the decoded launch stdout and its exit.
func initSeveranceProbeTranscript(t *testing.T, output []byte) ([]byte, json.RawMessage) {
	t.Helper()
	for line := range strings.Lines(string(output)) {
		if !strings.HasPrefix(line, `{"probe":"init-severance"`) {
			continue
		}
		var report struct {
			Status     json.RawMessage `json:"status"`
			Transcript string          `json:"transcript"`
		}
		if err := json.Unmarshal([]byte(line), &report); err != nil {
			t.Fatal("decode probe report:", err)
		}
		transcript, err := base64.StdEncoding.DecodeString(report.Transcript)
		if err != nil {
			t.Fatal("decode probe transcript:", err)
		}
		return transcript, report.Status
	}
	t.Fatalf("synthetic probe failed: %s", output)
	return nil, nil
}

// initEvent returns the transcript's init record's plugin and MCP server
// lists, for a failure message that names what loaded.
func initEvent(transcript []byte) string {
	scanner := bufio.NewScanner(bytes.NewReader(transcript))
	scanner.Buffer(make([]byte, 4096), 16<<20)
	for scanner.Scan() {
		var record struct {
			Type       string          `json:"type"`
			Subtype    string          `json:"subtype"`
			MCPServers json.RawMessage `json:"mcp_servers"`
			Plugins    json.RawMessage `json:"plugins"`
		}
		if json.Unmarshal(scanner.Bytes(), &record) == nil && record.Type == "system" && record.Subtype == "init" {
			return "plugins " + string(record.Plugins) + ", mcp_servers " + string(record.MCPServers)
		}
	}
	return "no init event"
}
