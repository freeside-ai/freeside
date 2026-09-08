package claude

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/daemonlock"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// This probe uses only synthetic input and an in-container mock API. Register
// its single container with the caller's held rig; never acquire a second rig.
func TestPinnedClaudePromptStdinLive(t *testing.T) {
	if os.Getenv("FREESIDE_PROMPT_STDIN_LIVE") != "1" {
		t.Skip("set FREESIDE_PROMPT_STDIN_LIVE=1, FREESIDE_PROMPT_IMAGE, FREESIDE_RIG_ACQUISITION and FREESIDE_STATE_DIR; requires a held rig and cached pinned Claude 2.1.220 image")
	}
	image := os.Getenv("FREESIDE_PROMPT_IMAGE")
	if !strings.Contains(image, "@sha256:") {
		t.Fatal("a cached digest-pinned image is required")
	}
	acquisition, err := os.ReadFile(os.Getenv("FREESIDE_RIG_ACQUISITION")) //nolint:gosec // explicit operator-selected acquisition file for this opt-in probe
	if err != nil {
		t.Fatal("read rig acquisition:", err)
	}
	var rig struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(acquisition, &rig); err != nil {
		t.Fatal("decode rig acquisition:", err)
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	// This existing role also works with an older daemon holding the rig.
	name := "freeside-handoff-c" + hex.EncodeToString(random[:])[:31] + "-observer"
	if _, err := daemonlock.BindRigRuntimeResources(os.Getenv("FREESIDE_STATE_DIR"), rig.Token, []string{name}, nil, nil); err != nil {
		t.Fatal("register probe resource:", err)
	}
	authorization, err := daemonlock.AuthorizeRig(os.Getenv("FREESIDE_STATE_DIR"), rig.Token)
	if err != nil {
		t.Fatal("hold probe authorization:", err)
	}
	// Registered before the resource cleanup so LIFO cleanup releases the rig
	// gate only after the probe is gone, including failure/cancellation paths.
	t.Cleanup(func() {
		if err := authorization.Close(); err != nil {
			t.Error(err)
		}
	})
	bin, err := exec.LookPath("container")
	if err != nil {
		t.Fatal(err)
	}
	rt := ward.NewCLIRuntime(bin)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	labels := []ward.Label{{Key: "freeside.prompt-probe", Value: name}}
	spec := ward.ContainerSpec{
		Name: name, Image: image, NetworkDisabled: true, Labels: labels,
		Command: []string{"sh", "-c", "n=0; while [ ! -f /probe-ready/ready ]; do n=$((n+1)); [ \"$n\" -lt 90 ] || exit 1; sleep 1; done; exec node /probe/probe.cjs"},
	}
	if err := rt.CreateContainer(ctx, spec); err != nil {
		t.Fatal(err)
	}
	before, err := rt.Inspect(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	if !before.LabelsObserved || !slices.Equal(before.Labels, labels) || before.CreationDate == "" {
		t.Fatal("probe ownership unproved; leave resource for rig recovery")
	}
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), time.Minute)
		defer done()
		rep, err := rt.Inspect(cleanup, name)
		if err != nil || rep.CreationDate != before.CreationDate || !rep.LabelsObserved || !slices.Equal(rep.Labels, labels) {
			t.Error("probe ownership changed; leave resource for rig recovery")
			return
		}
		if rep.State != ward.StateStopped {
			if err := rt.StopContainer(cleanup, name); err != nil {
				t.Error(err)
				return
			}
		}
		if err := rt.DeleteContainer(cleanup, name); err != nil {
			t.Error(err)
			return
		}
		containers, err := rt.ListContainers(cleanup)
		if err != nil {
			t.Error(err)
			return
		}
		for _, container := range containers {
			if container.ID == name {
				t.Error("probe remains after cleanup")
			}
		}
	})
	if !before.AllowlistFieldsObserved || !before.NetworksObserved || before.NetworkAttachmentCount != 0 || len(before.Mounts) != 0 || before.SSH || len(before.PublishedPorts) != 0 || len(before.PublishedSockets) != 0 {
		t.Fatal("probe must have no network, mounts, forwarding or publication")
	}
	input, ready := t.TempDir(), t.TempDir()
	fixture, err := os.ReadFile("testdata/prompt_stdin_probe.cjs")
	if err != nil {
		t.Fatal(err)
	}
	prompt := []byte(strings.Repeat("Synthetic UTF-8 α ' $() \\n\n", 2500) + "end")
	sum := sha256.Sum256(prompt)
	command := agentCommandWithInput("< '/probe/prompt.txt'", "24a8a5f8-fc85-439f-bf98-002ea19ca53e", domain.InvocationID("inv-prompt-probe"), nil)[2]
	_, command, ok := strings.Cut(command, "setpriv --reuid=")
	if !ok {
		t.Fatal("production privilege drop missing")
	}
	command, _, ok = strings.Cut(command, "> "+shellQuote(transcriptPath))
	if !ok {
		t.Fatal("production transcript redirect missing")
	}
	command = "setpriv --reuid=" + strings.ReplaceAll(command, shellQuote(instructionBundlePath), "'/probe/instructions.txt'")
	config, err := json.Marshal(struct {
		Digest  string `json:"digest"`
		Command string `json:"command"`
	}{hex.EncodeToString(sum[:]), command})
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string][]byte{"probe.cjs": fixture, "prompt.txt": prompt, "config.json": config} {
		if err := os.WriteFile(filepath.Join(input, name), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(ready, "ready"), []byte("ready"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := rt.StartContainer(ctx, name); err != nil {
		t.Fatal(err)
	}
	if err := rt.CopyIntoContainer(ctx, name, input, "/probe"); err != nil {
		t.Fatal(err)
	}
	if err := rt.CopyIntoContainer(ctx, name, ready, "/probe-ready"); err != nil {
		t.Fatal(err)
	}
	// Apple container logs --follow can return before process completion.
	// Observe the external VM instead; real time only bounds this opt-in I/O
	// probe, rather than testing timer behavior in production code.
	poll := time.NewTicker(250 * time.Millisecond)
	defer poll.Stop()
	for {
		rep, err := rt.Inspect(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		if rep.CreationDate != before.CreationDate || !rep.LabelsObserved || !slices.Equal(rep.Labels, labels) {
			t.Fatal("probe ownership changed while awaiting completion")
		}
		if rep.State == ward.StateStopped {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("probe completion:", ctx.Err())
		case <-poll.C:
		}
	}
	output, err := exec.CommandContext(ctx, bin, "logs", name).CombinedOutput() //nolint:gosec // resolved CLI, fixed operation, fixture-owned container
	if err != nil {
		t.Fatalf("read synthetic probe: %v", err)
	}
	if !strings.Contains(string(output), `"passed":true`) {
		t.Fatalf("synthetic probe failed: %s", output)
	}
	t.Log(strings.TrimSpace(string(output)))
}
