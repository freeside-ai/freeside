package claude

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/agentbaseline"
	"github.com/freeside-ai/freeside/daemon/internal/exec/stage"
	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// TestPinnedClaudeUsageDriverLive runs the production file_v6 agent line,
// driver and all, against the pinned CLI with only synthetic input and an
// in-container mock API. It is the fixture run that shows the usage read
// adds no request beyond the turn. Register its single container with the
// caller's held rig; never acquire a second rig.
func TestPinnedClaudeUsageDriverLive(t *testing.T) {
	if os.Getenv("FREESIDE_USAGE_DRIVER_LIVE") != "1" {
		t.Skip("set FREESIDE_USAGE_DRIVER_LIVE=1, FREESIDE_PROMPT_IMAGE, FREESIDE_RIG_ACQUISITION and FREESIDE_STATE_DIR; requires a held rig and a cached image at the pinned Claude version")
	}
	fixture, err := os.ReadFile("testdata/usage_driver_probe.cjs")
	if err != nil {
		t.Fatal(err)
	}
	prompt := []byte(strings.Repeat("Synthetic UTF-8 α ' $() \\n\n", 2500) + "end")
	sum := sha256.Sum256(prompt)
	hs, err := (claudeProvider{volumes: testAuthStoreVolumes{volume: "provider-volume"}}).
		HandoffSpec(context.Background(), handoffCommandCase{delivery: stage.PromptFileV6}.input())
	if err != nil {
		t.Fatal(err)
	}
	// From the privilege drop to the transcript redirect: the driver, the
	// CLI, and every flag production passes it. The probe supplies the
	// environment the root shell would, with an API key in place of the token.
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
		Digest  string `json:"digest"`
		Version string `json:"version"`
		Command string `json:"command"`
	}{hex.EncodeToString(sum[:]), strings.TrimPrefix(agentbaseline.ClaudeCodeHarnessBuild, "claude-code "), command})
	if err != nil {
		t.Fatal(err)
	}
	output := runNetworklessClaudeProbe(t, map[string][]byte{
		"probe.cjs": fixture, "prompt.txt": prompt, "config.json": config, "usage-driver.cjs": []byte(usageDriverScript),
	})
	if !strings.Contains(string(output), `"passed":true`) {
		t.Fatalf("synthetic probe failed: %s", output)
	}
	t.Log(strings.TrimSpace(string(output)))
}

// TestLiveClaudeWriterUsage makes one provider-backed writer turn, the
// conformance launch on file_v6, under an enrolled setup token against the
// pinned agent image, and reads the usage capture from its exported
// transcript as the stage pipeline does. A refresh or any other auth-store
// write would change the credential volume's tree, so the tree digest is
// compared across the turn. Run only with the owner's consent: the turn
// spends real subscription usage.
func TestLiveClaudeWriterUsage(t *testing.T) {
	if os.Getenv("FREESIDE_CLAUDE_USAGE_LIVE") != "1" {
		t.Skip("set FREESIDE_CLAUDE_USAGE_LIVE=1, FREESIDE_WARD_CLAUDE_AGENT_IMAGE, FREESIDE_WARD_EXPORTER_IMAGE, and CLAUDE_CODE_OAUTH_TOKEN; spends one real writer turn; see daemon/README.md")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	launch := newLiveClaudeLaunch(ctx, t)
	observe := func() ward.SetupTokenIntegrity {
		t.Helper()
		seen, err := ward.ObserveSetupTokenIntegrity(ctx, launch.runtime, launch.exporter, launch.volume, nil)
		if err != nil {
			t.Fatal("observe credential volume:", err)
		}
		return seen
	}
	before := observe()
	observedAt := time.Now().UTC()
	result := launch.run(ctx, t)
	defer func() { _ = os.RemoveAll(result.ExportDir) }()
	if after := observe(); after != before {
		t.Fatal("the writer turn changed the credential volume: an auth-store write or refresh")
	}
	var transcript []byte
	for _, entry := range result.Evidence.Entries {
		if entry.Label != "agent-transcript" {
			continue
		}
		body, err := conformanceBlob(result.ExportDir, export.EvidenceBlobsDirname, entry.Digest, entry.Size)
		if err != nil {
			t.Fatal(err)
		}
		transcript = body
	}
	if transcript == nil {
		t.Fatal("no exported agent transcript")
	}
	// Report shapes and numbers only, never transcript content.
	capture := ExtractUsageCapture(transcript, observedAt)
	if capture == nil {
		t.Fatalf("no usage capture; %d usage measurements", len(ExtractUsage(transcript, observedAt)))
	}
	windows := map[string]int{}
	for _, window := range capture.Windows {
		windows[window.Window] = window.UsedPercent
	}
	events := []string{}
	for _, event := range capture.Events {
		events = append(events, event.Window+":"+event.Status)
	}
	t.Logf("capture windows (percent used) %v; events %v", windows, events)
	if _, ok := windows["seven_day"]; !ok {
		t.Fatal("the capture has no weekly share")
	}
}
