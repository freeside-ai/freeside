package claude

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"os"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/agentbaseline"
	"github.com/freeside-ai/freeside/daemon/internal/exec/stage"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// TestPinnedClaudeInputReadLive runs the production file_v6 agent line
// against the pinned CLI with a synthetic image in Ward's read-only input
// directory, outside the working directory, and an in-container mock API that
// asks the CLI's Read tool for it. It shows the current launch command can
// read an operator input file without a new flag, so attachment delivery
// needs no new PromptDelivery member. Register its single container with the
// caller's held rig; never acquire a second rig.
func TestPinnedClaudeInputReadLive(t *testing.T) {
	if os.Getenv("FREESIDE_INPUT_READ_LIVE") != "1" {
		t.Skip("set FREESIDE_INPUT_READ_LIVE=1, FREESIDE_PROMPT_IMAGE, FREESIDE_RIG_ACQUISITION and FREESIDE_STATE_DIR; requires a held rig and a cached image at the pinned Claude version")
	}
	fixture, err := os.ReadFile("testdata/input_read_probe.cjs")
	if err != nil {
		t.Fatal(err)
	}
	prompt := []byte("Read the attached screenshot.")
	sum := sha256.Sum256(prompt)
	// The specifier's command is file_v5's and the other stages' runs the
	// usage driver; both must read the input.
	for _, specification := range []bool{false, true} {
		t.Run(map[bool]string{false: "implementation", true: "specification"}[specification], func(t *testing.T) {
			hs, err := (claudeProvider{volumes: testAuthStoreVolumes{volume: "provider-volume"}}).HandoffSpec(
				context.Background(),
				handoffCommandCase{delivery: stage.PromptFileV6, specification: specification}.input())
			if err != nil {
				t.Fatal(err)
			}
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
				Digest        string `json:"digest"`
				Version       string `json:"version"`
				Command       string `json:"command"`
				InputPath     string `json:"inputPath"`
				Specification bool   `json:"specification"`
			}{
				hex.EncodeToString(sum[:]), strings.TrimPrefix(agentbaseline.ClaudeCodeHarnessBuild, "claude-code "),
				command, ward.InputFilesTarget + "/discussion/1/1.png", specification,
			})
			if err != nil {
				t.Fatal(err)
			}
			output := runNetworklessClaudeProbe(t, map[string][]byte{
				"probe.cjs": fixture, "prompt.txt": prompt, "config.json": config,
				"usage-driver.cjs": []byte(usageDriverScript), "image.png": syntheticPNG(t),
			})
			if !strings.Contains(string(output), `"passed":true`) {
				t.Fatalf("synthetic probe failed: %s", output)
			}
			t.Log(strings.TrimSpace(string(output)))
		})
	}
}

// syntheticPNG is a small two-color image the CLI has no reason to resize.
func syntheticPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 16, 16))
	for x := range 16 {
		for y := range 16 {
			img.Set(x, y, color.RGBA{R: uint8(x * 16), G: uint8(y * 16), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
