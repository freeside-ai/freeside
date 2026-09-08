package ward

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPromptScriptsHandleFreshVolumeAndRejectUnexpectedContents(t *testing.T) {
	for _, shape := range []string{"empty_lost_found", "populated_lost_found", "linked_lost_found", "wrong_mode", "extra_entry"} {
		t.Run(shape, func(t *testing.T) {
			root, input, ready := t.TempDir(), t.TempDir(), t.TempDir()
			lost := filepath.Join(root, "lost+found")
			if shape == "linked_lost_found" {
				if err := os.Symlink(t.TempDir(), lost); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Mkdir(lost, 0o700); err != nil {
				t.Fatal(err)
			}
			if shape != "linked_lost_found" {
				if err := os.Chown(lost, os.Getuid(), os.Getgid()); err != nil {
					t.Fatal(err)
				}
			}
			if shape == "populated_lost_found" {
				if err := os.WriteFile(filepath.Join(lost, "retained"), []byte("preserve"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if shape == "wrong_mode" {
				if err := os.Chmod(lost, 0o755); err != nil { //nolint:gosec // deliberately unsafe mode in a rejection fixture
					t.Fatal(err)
				}
			}
			if shape == "extra_entry" {
				if err := os.WriteFile(filepath.Join(root, "extra"), []byte("preserve"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			body := []byte("complete ' UTF-8 α\n")
			for name, data := range map[string][]byte{filepath.Join(input, "prompt.txt"): body, filepath.Join(ready, seedReadyFile): []byte("ready")} {
				if err := os.WriteFile(name, data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			proof := filepath.Join(t.TempDir(), "proof")
			// Run the generated scripts against real files. Normalize root to the
			// test owner and BSD stat on macOS; production remains root/GNU Linux.
			adapt := func(script string) string {
				script = strings.NewReplacer(promptVolumeTarget, root, promptStageDir, input, promptReadyDir, ready,
					stateProofPath, proof, "chown 0:0 ", fmt.Sprintf("chown %d:%d ", os.Getuid(), os.Getgid())).Replace(script)
				for _, mode := range []string{"700", "755", "400"} {
					script = strings.ReplaceAll(script, "'"+mode+":0:0'", fmt.Sprintf("'%s:%d:%d'", mode, os.Getuid(), os.Getgid()))
				}
				if runtime.GOOS == "darwin" {
					script = strings.ReplaceAll(script, "stat -c '%a:%u:%g'", "stat -f '%Lp:%u:%g'")
					script = strings.ReplaceAll(script, "-printf x", "-exec printf x \\;")
					script = strings.ReplaceAll(script, "sha256sum ", "shasum -a 256 ")
				}
				return script
			}
			hs := HandoffSpec{RunID: "script-probe"}
			owner := Label{Key: ownershipLabelKey, Value: "script-nonce"}
			seed := buildPromptSeederSpec(testConfig(), hs, namesFor(hs.RunID), owner).Command[2]
			out, seedErr := osexec.CommandContext(t.Context(), "sh", "-c", adapt(seed)).CombinedOutput() //nolint:gosec // generated script, test-owned paths
			if shape != "empty_lost_found" && shape != "extra_entry" {
				if seedErr == nil {
					t.Fatal("unsafe lost+found accepted")
				}
				if _, err := os.Lstat(lost); err != nil {
					t.Fatal("rejected entry was removed:", err)
				}
				return
			}
			if seedErr != nil {
				t.Fatalf("seed: %v: %s", seedErr, out)
			}
			observer := buildPromptObserverSpec(testConfig(), hs, namesFor(hs.RunID), owner).Command[2]
			out, observeErr := osexec.CommandContext(t.Context(), "sh", "-c", adapt(observer)).CombinedOutput() //nolint:gosec // generated script, test-owned paths
			if shape == "extra_entry" {
				if observeErr == nil {
					t.Fatal("extra volume entry accepted")
				}
				return
			}
			if observeErr != nil {
				t.Fatalf("observe: %v: %s", observeErr, out)
			}
			observed, err := os.ReadFile(proof) //nolint:gosec // proof output in a test-owned directory
			if err != nil {
				t.Fatal(err)
			}
			if err := verifyPromptProof(observed, owner.Value, NewPromptFile(body).Digest); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPromptFileHandoffBindsObservedBytesBeforeWriter(t *testing.T) {
	fx := newHandoffFixture(t)
	j := newFakeJournal()
	fx.cfg.Journal = j
	hs := fx.seed(t)
	hs.Agent.LaunchState = LaunchStateClaudeClean
	hs.Agent.PromptFile = NewPromptFile(bytes.Repeat([]byte("'α\n"), 20000))
	names := namesFor(hs.RunID)
	var started bool
	fx.rt.onCreateContainer = func(spec ContainerSpec) error {
		if spec.Name != names.Agent {
			return nil
		}
		started = true
		rec := j.snapshot(hs.RunID)
		if rec.State == nil || rec.State.PromptFingerprint == "" ||
			rec.State.PromptDigest != strings.TrimPrefix(string(hs.Agent.PromptFile.Digest), "sha256:") {
			t.Fatal("writer created without immutable observed prompt binding")
		}
		if !bytes.Equal(fx.rt.promptState[names.Prompt], hs.Agent.PromptFile.Body) {
			t.Fatal("seeder did not deliver exact prompt bytes")
		}
		found := false
		for _, m := range spec.Mounts {
			if m.Target == PromptFileTarget {
				found = m.ReadOnly && m.Source == names.Prompt && m.Type == MountVolume
			}
		}
		if !found {
			t.Fatal("writer omitted protected prompt mount")
		}
		return nil
	}
	result, err := fx.backend(t).Handoff(t.Context(), hs)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(result.ExportDir) })
	if !started {
		t.Fatal("writer never created")
	}
	fx.assertReaped(t)
}

func TestPromptPreparationFailuresNeverCreateWriter(t *testing.T) {
	for _, failure := range []string{"changed_copy", "missing_copy", "wrong_proof", "observer_mount", "interrupted_copy"} {
		t.Run(failure, func(t *testing.T) {
			fx := newHandoffFixture(t)
			hs := fx.seed(t)
			hs.Agent.LaunchState = LaunchStateClaudeClean
			hs.Agent.PromptFile = NewPromptFile([]byte("approved prompt"))
			names := namesFor(hs.RunID)
			fx.rt.onCopyIntoContainer = func(id, host, target string) error {
				if id != names.PromptSeeder || target != promptStageDir {
					return nil
				}
				switch failure {
				case "changed_copy":
					return os.WriteFile(filepath.Join(host, "prompt.txt"), []byte("substituted"), 0o600)
				case "missing_copy":
					return os.Remove(filepath.Join(host, "prompt.txt"))
				case "interrupted_copy":
					return context.Canceled
				}
				return nil
			}
			fx.rt.observerProof = func(id string, proof []byte) []byte {
				if failure == "wrong_proof" && id == names.PromptObserver {
					return []byte("foreign proof\n")
				}
				return proof
			}
			fx.rt.onCreateContainer = func(spec ContainerSpec) error {
				if spec.Name == names.Agent {
					t.Fatal("writer reached after failed prompt preparation")
				}
				if failure == "observer_mount" && spec.Name == names.PromptObserver {
					return errors.New("observer cannot mount prompt")
				}
				return nil
			}
			if result, err := fx.backend(t).Handoff(t.Context(), hs); err == nil || result != nil {
				t.Fatalf("unverified prompt accepted: %v", err)
			}
			fx.assertReaped(t)
		})
	}
}

func TestPromptFileValidationAndLegacyEncoding(t *testing.T) {
	hs := testHandoffSpec()
	body, err := json.Marshal(hs)
	if err != nil || bytes.Contains(body, []byte("PromptFile")) {
		t.Fatal("legacy handoff encoding acquired a prompt field")
	}
	for _, data := range [][]byte{nil, {0xff}, bytes.Repeat([]byte("x"), MaxPromptFileBytes+1)} {
		if NewPromptFile(data).Validate() == nil {
			t.Fatal("invalid prompt accepted")
		}
	}
	prompt := NewPromptFile([]byte("prompt"))
	copy := prompt.Clone()
	copy.Body[0] = 'X'
	if copy.Validate() == nil || prompt.Validate() != nil {
		t.Fatal("prompt cloning or digest validation failed")
	}
	hs.Agent.PromptFile = prompt
	if hs.validate() == nil {
		t.Fatal("state-free launch accepted prompt without journal topology")
	}
}

func TestPromptMountCannotBeRedirectedOrMadeWritable(t *testing.T) {
	hs := testHandoffSpec()
	hs.Agent.LaunchState = LaunchStateClaudeClean
	hs.Agent.PromptFile = NewPromptFile([]byte("prompt"))
	names := namesFor(hs.RunID)
	for _, mutation := range []string{"writable", "wrong_volume", "wrong_target", "duplicate", "missing"} {
		t.Run(mutation, func(t *testing.T) {
			spec := buildAgentSpec(testConfig(), hs, names, Label{}, "http://proxy:8080")
			for index := range spec.Mounts {
				if spec.Mounts[index].Target != PromptFileTarget {
					continue
				}
				switch mutation {
				case "writable":
					spec.Mounts[index].ReadOnly = false
				case "wrong_volume":
					spec.Mounts[index].Source = names.Instructions
				case "wrong_target":
					spec.Mounts[index].Target += "/nested"
				case "duplicate":
					spec.Mounts = append(spec.Mounts, spec.Mounts[index])
				case "missing":
					spec.Mounts = append(spec.Mounts[:index], spec.Mounts[index+1:]...)
				}
				break
			}
			if validateAgentSpec(testConfig(), spec, names, "", false, hs.Agent.LaunchState, true) == nil {
				t.Fatal("invalid prompt mount accepted")
			}
		})
	}
}

func TestPromptRecoveryRejectsMissingOrRetargetedProof(t *testing.T) {
	for _, corruption := range []string{"missing", "wrong_digest", "half_pair", "argument_with_proof", "legacy_digest"} {
		t.Run(corruption, func(t *testing.T) {
			fx := newHandoffFixture(t)
			j := newFakeJournal()
			fx.cfg.Journal = j
			hs := fx.seed(t)
			hs.Agent.LaunchState = LaunchStateClaudeClean
			hs.Agent.PromptFile = NewPromptFile([]byte("retained input"))
			result, err := fx.backend(t).Handoff(t.Context(), hs)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(result.ExportDir) })
			rec := j.records[hs.RunID]
			rec.Outcome = nil
			switch corruption {
			case "missing":
				rec.State.PromptFingerprint, rec.State.PromptDigest = "", ""
			case "wrong_digest":
				rec.State.PromptDigest = strings.Repeat("a", 64)
			case "half_pair":
				rec.State.PromptFingerprint = ""
			case "argument_with_proof":
				hs.Agent.PromptFile = nil
				rec.SpecDigest, err = specDigest(hs)
			case "legacy_digest":
				rec.SpecDigest, err = legacySpecDigest(hs)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := fx.backend(t).Recover(t.Context(), hs.RunID, hs); !errors.Is(err, ErrInvalidJournalRecord) {
				t.Fatalf("unauthenticated prompt recovery accepted: %v", err)
			}
			if j.records[hs.RunID].Outcome != nil {
				t.Fatal("rejected recovery changed journal outcome")
			}
		})
	}
}

func TestPromptRecoveryReapsInterruptedPreparation(t *testing.T) {
	fx := newRecoveryFixture(t)
	hs := fx.seed(t)
	hs.Agent.LaunchState = LaunchStateClaudeClean
	hs.Agent.PromptFile = NewPromptFile([]byte("retained input"))
	fx.openRecord(t, hs)
	names := namesFor(hs.RunID)
	fx.worldVolume(t, names.Workspace, fx.runLabels(hs.RunID))
	fx.worldVolume(t, names.Prompt, fx.runLabels(hs.RunID))
	seeder := buildPromptSeederSpec(fx.cfg, hs, names, Label{Key: ownershipLabelKey, Value: testRecoveryToken})
	fx.worldContainer(t, seeder, true)
	result, err := fx.recover(t, hs.RunID, hs)
	if err != nil || result == nil || result.Outcome != RecoveryLoss {
		t.Fatalf("interrupted prompt preparation did not settle as loss: %v", err)
	}
	fx.assertReaped(t)
	fx.wantClosed(t, hs.RunID, HandoffLoss)
}
