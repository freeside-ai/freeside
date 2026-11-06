package ward

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// fakeInputBlobs serves stored bytes by digest; a test stores bytes under a
// digest they do not hash to, or omits one, to model a corrupt store.
type fakeInputBlobs map[domain.Digest][]byte

func (b fakeInputBlobs) OpenContext(_ context.Context, digest domain.Digest) (io.ReadCloser, error) {
	body, ok := b[digest]
	if !ok {
		return nil, fmt.Errorf("blob %s not found", digest)
	}
	return io.NopCloser(bytes.NewReader(body)), nil
}

// testInputs builds a manifest and its blob store from name/content pairs.
func testInputs(pairs ...string) ([]InputFile, fakeInputBlobs) {
	blobs := fakeInputBlobs{}
	var files []InputFile
	for i := 0; i+1 < len(pairs); i += 2 {
		body := []byte(pairs[i+1])
		sum := sha256.Sum256(body)
		digest := domain.Digest(contentaddr.Format(sum[:]))
		blobs[digest] = body
		files = append(files, InputFile{Name: domain.TaskAttachmentName(pairs[i]), Digest: digest, SizeBytes: int64(len(body))})
	}
	return files, blobs
}

// caseSensitive reports whether dir's filesystem keeps "A" and "a" apart.
func caseSensitive(t *testing.T, dir string) bool {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "A"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(filepath.Join(dir, "A")) //nolint:errcheck // probe file in a test-owned directory
	_, err := os.Stat(filepath.Join(dir, "a"))
	return errors.Is(err, os.ErrNotExist)
}

// TestInputScriptsDeliverManifestTreeAndRejectChanges runs the generated
// seeder and observer against real files: the manifest's tree must prove
// exactly, and any change between seeding and observation must not. CI runs
// it with GNU tools; the exporter image's BusyBox is proved by running this
// test's binary inside that image (daemon/README.md). On macOS the stat and
// hash commands are translated, and the case pair is skipped on a
// case-insensitive temp directory.
func TestInputScriptsDeliverManifestTreeAndRejectChanges(t *testing.T) {
	t.Parallel()
	segment := strings.Repeat("s", domain.MaxTaskAttachmentSegmentBytes)
	longPath := "task/" + strings.Repeat(strings.Repeat("l", 200)+"/", 4) + "end.txt"
	pairs := []string{
		"task/screenshot.png", "\x89PNG\r\n\x1a\nnot really",
		"task/docs/nested/deep/readme.md", "# nested\n",
		"task/α ü 'q' \"d\" $x `b` \\back %s.txt", "odd name\n",
		"task/" + segment, "long segment",
		longPath, "long path",
		"task/empty.bin", "",
		"task/-dash", "leading dash",
		"discussion/3/1.png", "\x89PNG\r\n\x1a\nmessage",
	}
	probe := t.TempDir()
	if caseSensitive(t, probe) {
		pairs = append(pairs, "task/A.png", "upper", "task/a.png", "lower")
	} else {
		t.Log("temp directory is case-insensitive; skipping the A.png/a.png pair here")
	}
	files, blobs := testInputs(pairs...)
	uid, gid := os.Getuid(), os.Getgid()
	adapt := func(script, root, input, ready, proof string) string {
		script = strings.NewReplacer(inputVolumeTarget, root, inputStageDir, input, inputReadyDir, ready,
			stateProofPath, proof, "chown 0:0 ", fmt.Sprintf("chown %d:%d ", uid, gid),
			"chown -R 0:0 ", fmt.Sprintf("chown -R %d:%d ", uid, gid)).Replace(script)
		script = strings.ReplaceAll(script, "'700:0:0'", fmt.Sprintf("'700:%d:%d'", uid, gid))
		script = strings.ReplaceAll(script, `:0:0")`, fmt.Sprintf(`:%d:%d")`, uid, gid))
		script = strings.ReplaceAll(script, `:0:0"|`, fmt.Sprintf(`:%d:%d"|`, uid, gid))
		if runtime.GOOS == "darwin" {
			script = strings.NewReplacer(
				"stat -c '%a:%u:%g'", "stat -f '%Lp:%u:%g'",
				`stat -c "%F:%a:%u:%g"`, `stat -f "%HT:%Lp:%u:%g"`,
				`"directory:`, `"Directory:`, `"regular file:`, `"Regular File:`,
				"sha256sum", "shasum -a 256",
			).Replace(script)
		}
		return script
	}
	hs := HandoffSpec{RunID: "script-probe"}
	hs.Agent.Inputs = files
	owner := Label{Key: ownershipLabelKey, Value: "script-nonce"}
	cfg := testConfig()
	cfg.InputBlobs = blobs
	for _, change := range []string{"none", "changed_byte", "missing_file", "extra_entry", "symlink", "writable_file", "writable_dir"} {
		t.Run(change, func(t *testing.T) {
			root, input, ready := t.TempDir(), t.TempDir(), t.TempDir()
			// BSD filesystems give a new entry its parent's group, which
			// under /tmp is wheel; the seeder checks lost+found's owner and
			// group, so give the tree the test's own.
			if err := os.Chown(root, os.Getuid(), os.Getgid()); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(root, "lost+found"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := writeInputArchive(t.Context(), filepath.Join(input, inputArchiveName), files, blobs); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(ready, seedReadyFile), []byte("ready"), 0o600); err != nil {
				t.Fatal(err)
			}
			proof := filepath.Join(t.TempDir(), "proof")
			seed := buildInputSeederSpec(cfg, handoffInputRoles(cfg, hs, namesFor(hs.RunID)), owner).Command[2]
			if out, err := osexec.CommandContext(t.Context(), "sh", "-c", adapt(seed, root, input, ready, proof)).CombinedOutput(); err != nil { //nolint:gosec // generated script, test-owned paths
				t.Fatalf("seed: %v: %s", err, out)
			}
			for _, f := range files {
				got, err := os.ReadFile(filepath.Join(root, string(f.Name))) //nolint:gosec // test-owned tree
				if err != nil {
					t.Fatal(err)
				}
				if want := blobs[f.Digest]; !bytes.Equal(got, want) {
					t.Fatalf("%q arrived as %q, want %q", f.Name, got, want)
				}
			}
			victim := filepath.Join(root, "task", "docs", "nested", "deep", "readme.md")
			switch change {
			case "changed_byte":
				if err := os.Chmod(victim, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(victim, []byte("# nesteD\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(victim, 0o444); err != nil { //nolint:gosec // the mode the seeder leaves, restored after the edit
					t.Fatal(err)
				}
			case "missing_file":
				if err := os.Remove(victim); err != nil {
					t.Fatal(err)
				}
			case "extra_entry":
				if err := os.WriteFile(filepath.Join(root, "task", "extra"), nil, 0o444); err != nil { //nolint:gosec // an otherwise valid extra file in a rejection fixture
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Remove(victim); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(root, "task", "screenshot.png"), victim); err != nil {
					t.Fatal(err)
				}
			case "writable_file":
				if err := os.Chmod(victim, 0o644); err != nil { //nolint:gosec // deliberately writable mode in a rejection fixture
					t.Fatal(err)
				}
			case "writable_dir":
				if err := os.Chmod(filepath.Dir(victim), 0o775); err != nil { //nolint:gosec // deliberately unsafe mode in a rejection fixture
					t.Fatal(err)
				}
			}
			observer := buildInputObserverSpec(handoffInputRoles(cfg, hs, namesFor(hs.RunID)), owner).Command[2]
			if out, err := osexec.CommandContext(t.Context(), "sh", "-c", adapt(observer, root, input, ready, proof)).CombinedOutput(); err != nil { //nolint:gosec // generated script, test-owned paths
				t.Fatalf("observe: %v: %s", err, out)
			}
			observed, err := os.ReadFile(proof) //nolint:gosec // proof output in a test-owned directory
			if err != nil {
				t.Fatal(err)
			}
			err = verifyInputProof(observed, owner.Value, files)
			if change == "none" && err != nil {
				t.Fatal(err)
			}
			if change != "none" && err == nil {
				t.Fatal("changed input tree accepted")
			}
		})
	}
}

func TestInputHandoffBindsObservedTreeBeforeWriter(t *testing.T) {
	t.Parallel()
	fx := newHandoffFixture(t)
	j := newFakeJournal()
	fx.cfg.Journal = j
	files, blobs := testInputs("task/A.png", "upper", "task/a.png", "lower", "task/docs/notes.md", "nested", "discussion/2/1.png", "shot")
	fx.cfg.InputBlobs = blobs
	hs := fx.seed(t)
	hs.Agent.LaunchState = LaunchStateClaudeClean
	hs.Agent.PromptFile = NewPromptFile([]byte("prompt"))
	hs.Agent.Inputs = files
	names := namesFor(hs.RunID)
	var started bool
	fx.rt.onCreateContainer = func(spec ContainerSpec) error {
		if spec.Name != names.Agent {
			return nil
		}
		started = true
		rec := j.snapshot(hs.RunID)
		if rec.State == nil || rec.State.InputsFingerprint == "" || rec.State.InputsDigest != inputListingDigest(files) {
			t.Fatal("writer created without an observed input binding")
		}
		listing := fx.rt.inputListing[names.Inputs]
		for _, f := range files {
			sum := sha256.Sum256(blobs[f.Digest])
			if !slices.Contains(listing, fmt.Sprintf("f %x ./%s", sum, f.Name)) {
				t.Fatalf("%q did not arrive intact: %q", f.Name, listing)
			}
		}
		mounts := 0
		for _, m := range spec.Mounts {
			if m.Target == InputFilesTarget {
				mounts++
				if !m.ReadOnly || m.Source != names.Inputs || m.Type != MountVolume {
					t.Fatalf("input mount is not the read-only input volume: %+v", m)
				}
			}
		}
		if mounts != 1 {
			t.Fatalf("writer has %d input mounts, want 1", mounts)
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

func TestInputPreparationFailuresNeverCreateWriter(t *testing.T) {
	t.Parallel()
	for _, failure := range []string{
		"blob_bytes", "blob_short", "blob_long", "blob_missing", "changed_copy",
		"tampered_tree", "extra_entry", "wrong_proof", "interrupted_copy",
	} {
		t.Run(failure, func(t *testing.T) {
			fx := newHandoffFixture(t)
			files, blobs := testInputs("task/shot.png", "pixels", "task/dir/notes.md", "notes")
			fx.cfg.InputBlobs = blobs
			hs := fx.seed(t)
			hs.Agent.LaunchState = LaunchStateClaudeClean
			hs.Agent.PromptFile = NewPromptFile([]byte("prompt"))
			hs.Agent.Inputs = files
			names := namesFor(hs.RunID)
			digest := files[0].Digest
			switch failure {
			case "blob_bytes":
				blobs[digest] = []byte("pixelz")
			case "blob_short":
				blobs[digest] = []byte("pixel")
			case "blob_long":
				blobs[digest] = []byte("pixels!")
			case "blob_missing":
				delete(blobs, digest)
			}
			fx.rt.onCopyIntoContainer = func(id, host, target string) error {
				if id != names.InputsSeeder || target != inputStageDir {
					return nil
				}
				switch failure {
				case "changed_copy":
					return rewriteArchiveFile(filepath.Join(host, inputArchiveName), "task/shot.png", []byte("pixelz"))
				case "interrupted_copy":
					return context.Canceled
				}
				return nil
			}
			fx.rt.observerProof = func(id string, proof []byte) []byte {
				if failure == "wrong_proof" && id == names.InputsObserver {
					return []byte("foreign proof\n")
				}
				return proof
			}
			fx.rt.onCreateContainer = func(spec ContainerSpec) error {
				switch {
				case spec.Name == names.Agent:
					t.Fatal("writer reached after failed input preparation")
				case spec.Name == names.InputsObserver && failure == "tampered_tree":
					listing := fx.rt.inputListing[names.Inputs]
					shot := slices.IndexFunc(listing, func(l string) bool { return strings.HasSuffix(l, " ./task/shot.png") })
					listing[shot] = "f " + strings.Repeat("0", 64) + " ./task/shot.png"
				case spec.Name == names.InputsObserver && failure == "extra_entry":
					fx.rt.inputListing[names.Inputs] = append(fx.rt.inputListing[names.Inputs], "x ./task/link")
				}
				return nil
			}
			result, err := fx.backend(t).Handoff(t.Context(), hs)
			if err == nil || result != nil {
				t.Fatalf("unverified inputs accepted: %v", err)
			}
			if strings.HasPrefix(failure, "blob_") && failure != "blob_missing" && !errors.Is(err, ErrInputBlobMismatch) {
				t.Fatalf("blob mismatch not reported as one: %v", err)
			}
			fx.assertReaped(t)
		})
	}
}

// rewriteArchiveFile replaces one regular file's bytes in a staged archive,
// keeping its size so only the content differs.
func rewriteArchiveFile(path, name string, body []byte) error {
	in, err := os.ReadFile(path) //nolint:gosec // test-owned snapshot
	if err != nil {
		return err
	}
	var out bytes.Buffer
	tr, tw := tar.NewReader(bytes.NewReader(in)), tar.NewWriter(&out)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		content, err := io.ReadAll(tr)
		if err != nil {
			return err
		}
		if hdr.Name == name {
			content = body
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if _, err := tw.Write(content); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return os.WriteFile(path, out.Bytes(), 0o600)
}

func TestInputHandoffWithoutBlobSourceCreatesNothing(t *testing.T) {
	t.Parallel()
	fx := newHandoffFixture(t)
	files, _ := testInputs("task/shot.png", "pixels")
	hs := fx.seed(t)
	hs.Agent.LaunchState = LaunchStateClaudeClean
	hs.Agent.PromptFile = NewPromptFile([]byte("prompt"))
	hs.Agent.Inputs = files
	before := fx.rt.callCount()
	if result, err := fx.backend(t).Handoff(t.Context(), hs); err == nil || result != nil {
		t.Fatalf("inputs accepted without a blob source: %v", err)
	}
	if fx.rt.callCount() != before {
		t.Fatal("refused handoff touched the runtime")
	}
}

func TestInputManifestValidation(t *testing.T) {
	t.Parallel()
	plain := testHandoffSpec()
	body, err := json.Marshal(plain)
	if err != nil || bytes.Contains(body, []byte("Inputs")) {
		t.Fatal("handoff without inputs acquired an inputs field, changing its spec digest")
	}
	valid, _ := testInputs("task/A.png", "a", "task/a.png", "b")
	seeded := newHandoffFixture(t).seed(t)
	withPrompt := func(files []InputFile) HandoffSpec {
		hs := seeded
		hs.Agent.LaunchState = LaunchStateClaudeClean
		hs.Agent.PromptFile = NewPromptFile([]byte("prompt"))
		hs.Agent.Inputs = files
		return hs
	}
	if err := withPrompt(valid).validate(); err != nil {
		t.Fatalf("case-distinct names refused: %v", err)
	}
	stateFree := withPrompt(valid)
	stateFree.Agent.LaunchState = ""
	stateFree.Agent.PromptFile = nil
	noPrompt := withPrompt(valid)
	noPrompt.Agent.PromptFile = nil
	many := make([]InputFile, domain.MaxTaskAttachments+1)
	for i := range many {
		many[i] = valid[0]
		many[i].Name = domain.TaskAttachmentName(fmt.Sprintf("task/%d", i))
	}
	big := slices.Clone(valid)
	big[0].SizeBytes = domain.MaxTaskAttachmentsTotalBytes
	cases := map[string]HandoffSpec{
		"state_free":    stateFree,
		"no_prompt":     noPrompt,
		"absolute":      withPrompt([]InputFile{{Name: "/etc/passwd", Digest: valid[0].Digest, SizeBytes: 1}}),
		"parent":        withPrompt([]InputFile{{Name: "task/../x", Digest: valid[0].Digest, SizeBytes: 1}}),
		"control":       withPrompt([]InputFile{{Name: "task/a\nb", Digest: valid[0].Digest, SizeBytes: 1}}),
		"duplicate":     withPrompt([]InputFile{valid[0], valid[0]}),
		"file_and_dir":  withPrompt([]InputFile{{Name: "task", Digest: valid[0].Digest, SizeBytes: 1}, valid[0]}),
		"bad_digest":    withPrompt([]InputFile{{Name: "task/x", Digest: "sha256:zz", SizeBytes: 1}}),
		"negative_size": withPrompt([]InputFile{{Name: "task/x", Digest: valid[0].Digest, SizeBytes: -1}}),
		"too_many":      withPrompt(many),
		"too_large":     withPrompt(big),
	}
	for name, hs := range cases {
		t.Run(name, func(t *testing.T) {
			if err := hs.validate(); !errors.Is(err, ErrInvalidHandoffSpec) {
				t.Fatalf("invalid input manifest accepted: %v", err)
			}
		})
	}
}

func TestInputMountCannotBeRedirectedOrMadeWritable(t *testing.T) {
	t.Parallel()
	files, _ := testInputs("task/shot.png", "pixels")
	hs := testHandoffSpec()
	hs.Agent.LaunchState = LaunchStateClaudeClean
	hs.Agent.PromptFile = NewPromptFile([]byte("prompt"))
	hs.Agent.Inputs = files
	names := namesFor(hs.RunID)
	if err := validateAgentSpec(testConfig(), buildAgentSpec(testConfig(), hs, names, Label{}, "http://proxy:8080"),
		names, "", false, hs.Agent.LaunchState, true, true); err != nil {
		t.Fatalf("built input mount refused: %v", err)
	}
	for _, mutation := range []string{"writable", "wrong_volume", "wrong_target", "in_workspace", "duplicate", "missing", "unexpected"} {
		t.Run(mutation, func(t *testing.T) {
			spec := buildAgentSpec(testConfig(), hs, names, Label{}, "http://proxy:8080")
			hasInputs := true
			for index := range spec.Mounts {
				if spec.Mounts[index].Target != InputFilesTarget {
					continue
				}
				switch mutation {
				case "writable":
					spec.Mounts[index].ReadOnly = false
				case "wrong_volume":
					spec.Mounts[index].Source = names.Instructions
				case "wrong_target":
					spec.Mounts[index].Target += "/nested"
				case "in_workspace":
					spec.Mounts[index].Target = testConfig().WorkspaceTarget + "/inputs"
				case "duplicate":
					spec.Mounts = append(spec.Mounts, spec.Mounts[index])
				case "missing":
					spec.Mounts = append(spec.Mounts[:index], spec.Mounts[index+1:]...)
				case "unexpected":
					hasInputs = false
				}
				break
			}
			if validateAgentSpec(testConfig(), spec, names, "", false, hs.Agent.LaunchState, true, hasInputs) == nil {
				t.Fatal("invalid input mount accepted")
			}
		})
	}
}

func TestInputRecoveryRejectsMissingOrRetargetedProof(t *testing.T) {
	t.Parallel()
	for _, corruption := range []string{"missing", "wrong_digest", "half_pair", "no_inputs_with_proof", "legacy_digest"} {
		t.Run(corruption, func(t *testing.T) {
			fx := newHandoffFixture(t)
			j := newFakeJournal()
			fx.cfg.Journal = j
			files, blobs := testInputs("task/shot.png", "pixels")
			fx.cfg.InputBlobs = blobs
			hs := fx.seed(t)
			hs.Agent.LaunchState = LaunchStateClaudeClean
			hs.Agent.PromptFile = NewPromptFile([]byte("prompt"))
			hs.Agent.Inputs = files
			result, err := fx.backend(t).Handoff(t.Context(), hs)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(result.ExportDir) })
			rec := j.records[hs.RunID]
			rec.Outcome = nil
			switch corruption {
			case "missing":
				rec.State.InputsFingerprint, rec.State.InputsDigest = "", ""
			case "wrong_digest":
				rec.State.InputsDigest = strings.Repeat("a", 64)
			case "half_pair":
				rec.State.InputsFingerprint = ""
			case "no_inputs_with_proof":
				hs.Agent.Inputs = nil
				rec.SpecDigest, err = specDigest(hs)
			case "legacy_digest":
				rec.SpecDigest, err = legacySpecDigest(hs)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := fx.backend(t).Recover(t.Context(), hs.RunID, hs); !errors.Is(err, ErrInvalidJournalRecord) {
				t.Fatalf("unauthenticated input recovery accepted: %v", err)
			}
			if j.records[hs.RunID].Outcome != nil {
				t.Fatal("rejected recovery changed journal outcome")
			}
		})
	}
}

func TestInputRecoveryReapsInterruptedPreparation(t *testing.T) {
	t.Parallel()
	fx := newRecoveryFixture(t)
	files, _ := testInputs("task/shot.png", "pixels")
	hs := fx.seed(t)
	hs.Agent.LaunchState = LaunchStateClaudeClean
	hs.Agent.PromptFile = NewPromptFile([]byte("prompt"))
	hs.Agent.Inputs = files
	fx.openRecord(t, hs)
	names := namesFor(hs.RunID)
	fx.worldVolume(t, names.Workspace, fx.runLabels(hs.RunID))
	fx.worldVolume(t, names.Prompt, fx.runLabels(hs.RunID))
	fx.worldVolume(t, names.Inputs, fx.runLabels(hs.RunID))
	seeder := buildInputSeederSpec(fx.cfg, handoffInputRoles(fx.cfg, hs, names), Label{Key: ownershipLabelKey, Value: testRecoveryToken})
	fx.worldContainer(t, seeder, true)
	result, err := fx.recover(t, hs.RunID, hs)
	if err != nil || result == nil || result.Outcome != RecoveryLoss {
		t.Fatalf("interrupted input preparation did not settle as loss: %v", err)
	}
	fx.assertReaped(t)
	fx.wantClosed(t, hs.RunID, HandoffLoss)
}
