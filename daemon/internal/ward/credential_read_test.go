package ward

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// runTokenReadScript runs the generated token-read observer script over a
// host directory standing in for the mounted volume. It returns the parsed
// proof, its raw text, and the path the script copies the token to.
func runTokenReadScript(t *testing.T, store string) (credProof, string, string) {
	t.Helper()
	shell := "sh"
	if dash, err := osexec.LookPath("dash"); err == nil {
		shell = dash // as runIntegrityScript: the image's shell is not bash
	}
	cfg := testConfig()
	cfg.CredProofPath = filepath.Join(t.TempDir(), "proof.txt")
	nonce := testOwnershipLabel().Value
	script := credTokenReadObserverScript(cfg, nonce, store)
	if strings.Contains(script, integritySentinelToken) {
		t.Fatal("the observer script carries the token")
	}
	cmd := osexec.Command(shell, "-c", script) //nolint:gosec // fixed shell and test-owned script
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("token-read observer script: %v: %s", err, out)
	}
	raw, err := os.ReadFile(cfg.CredProofPath)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := parseCredProof(raw, nonce, CredentialManifestOpaque, true)
	if err != nil {
		t.Fatalf("parseCredProof(script output) = %v", err)
	}
	return proof, string(raw), credTokenCopyPath(cfg)
}

// TestCredTokenReadObserverScript runs the generated script for real: the
// copy is the token file's bytes under a private mode, the proof is the
// integrity proof and never carries the token, an oversized file is copied
// only far enough to be refused, and a store with no regular token file
// leaves no copy.
func TestCredTokenReadObserverScript(t *testing.T) {
	store := func(t *testing.T, body []byte) string {
		t.Helper()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "token"), body, 0o400); err != nil {
			t.Fatal(err)
		}
		return dir
	}

	t.Run("copies the token beside the proof", func(t *testing.T) {
		token := []byte(integritySentinelToken + "\n")
		proof, raw, copyPath := runTokenReadScript(t, store(t, token))
		got, err := os.ReadFile(copyPath) //nolint:gosec // test-owned path
		if err != nil || !bytes.Equal(got, token) {
			t.Fatalf("token copy = %q, %v; want the token file's bytes", got, err)
		}
		if info, err := os.Stat(copyPath); err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("token copy mode = %v, %v; want 0600", info.Mode().Perm(), err)
		}
		if proof.tokenDigest != hexSHA256(token) {
			t.Errorf("token digest = %q, want the hash of the token file's bytes", proof.tokenDigest)
		}
		if strings.Contains(raw, integritySentinelToken) {
			t.Error("the proof carries the token")
		}
		stored, err := readCredTokenCopy(bytes.NewReader(tarOf(t, copyPath, got)), copyPath, proof.tokenDigest)
		if err != nil || string(stored.Reveal()) != integritySentinelToken {
			t.Errorf("the script's copy did not read back as the token: %v", err)
		}
	})
	t.Run("starts with the integrity script", func(t *testing.T) {
		cfg := testConfig()
		nonce := testOwnershipLabel().Value
		if !strings.HasPrefix(credTokenReadObserverScript(cfg, nonce, "/credentials"),
			credIntegrityObserverScript(cfg, nonce, "/credentials")) {
			t.Error("the token-read script does not begin with the integrity script")
		}
	})
	t.Run("caps an oversized file", func(t *testing.T) {
		big := bytes.Repeat([]byte("a"), 4*MaxSetupTokenBytes)
		proof, _, copyPath := runTokenReadScript(t, store(t, big))
		got, err := os.ReadFile(copyPath) //nolint:gosec // test-owned path
		if err != nil || len(got) != maxStoredSetupTokenBytes+1 {
			t.Fatalf("copy of an oversized file is %d bytes, %v; want the cap plus one", len(got), err)
		}
		if _, err := readCredTokenCopy(bytes.NewReader(tarOf(t, copyPath, got)), copyPath, proof.tokenDigest); err == nil {
			t.Error("an oversized token file was read")
		}
	})
	t.Run("no copy without a regular token file", func(t *testing.T) {
		_, _, copyPath := runTokenReadScript(t, t.TempDir())
		if _, err := os.Lstat(copyPath); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("an empty volume left a token copy: %v", err)
		}
		linked := t.TempDir()
		target := filepath.Join(t.TempDir(), "elsewhere")
		if err := os.WriteFile(target, []byte(integritySentinelToken), 0o400); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(linked, "token")); err != nil {
			t.Fatal(err)
		}
		_, _, copyPath = runTokenReadScript(t, linked)
		if _, err := os.Lstat(copyPath); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("a symlinked token was copied through the link: %v", err)
		}
	})
}

func tarOf(t *testing.T, path string, body []byte) []byte {
	t.Helper()
	var archive bytes.Buffer
	if err := writeFilesTar(&archive, tarFile{path, body}); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}

// tokenReadFixture is a fake runtime holding one credential volume whose
// token-read observer exports the given copy under a proof attesting
// proofDigest, with the host temporary directory pointed somewhere the test
// can inspect.
type tokenReadFixture struct {
	rt    *fakeRuntime
	tmp   string
	specs []ContainerSpec
}

func newTokenReadFixture(t *testing.T, copied []byte, proofDigest string) *tokenReadFixture {
	t.Helper()
	fx := &tokenReadFixture{rt: newFakeRuntime(t), tmp: t.TempDir()}
	t.Setenv("TMPDIR", fx.tmp)
	if err := fx.rt.CreateVolume(context.Background(), integrityCredentialVolume, 1, nil); err != nil {
		t.Fatal(err)
	}
	fx.rt.credState[integrityCredentialVolume] = "store-state"
	length := credProofTokenLengthOK
	if proofDigest == credProofTokenAbsent {
		length = credProofTokenAbsent // the script reports both keys absent together
	}
	fx.rt.observerProof = integrityProofLines(proofDigest, length)
	if copied != nil {
		fx.rt.credTokenCopy = map[string][]byte{integrityCredentialVolume: copied}
	}
	fx.rt.onCreateContainer = func(spec ContainerSpec) error {
		fx.specs = append(fx.specs, spec)
		return nil
	}
	return fx
}

// assertNothingLeft fails when the read left a container in the runtime or
// anything in the host temporary directory.
func (fx *tokenReadFixture) assertNothingLeft(t *testing.T) {
	t.Helper()
	fx.rt.mu.Lock()
	left := len(fx.rt.ctrs)
	fx.rt.mu.Unlock()
	if left != 0 {
		t.Errorf("the read left %d container(s) behind", left)
	}
	entries, err := os.ReadDir(fx.tmp)
	if err != nil || len(entries) != 0 {
		t.Errorf("the read left %d entries in the temporary directory (%v)", len(entries), err)
	}
}

// assertNoToken fails when any rendering of a value carries the sentinel.
func assertNoToken(t *testing.T, what string, value any) {
	t.Helper()
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x"} {
		rendered := fmt.Sprintf(verb, value)
		// The decimal form is what a byte slice prints as where fmt cannot
		// call the type's own formatter.
		if strings.Contains(rendered, integritySentinelToken) ||
			strings.Contains(rendered, fmt.Sprintf("%x", integritySentinelToken)) ||
			strings.Contains(rendered, strings.Trim(fmt.Sprintf("%d", []byte(integritySentinelToken)), "[]")) {
			t.Errorf("%s rendered with %s carries the token", what, verb)
		}
	}
}

// TestReadSetupToken drives the read through the fake runtime: it returns
// the stored token with both digests, through one networkless read-only
// observer whose name was authorized first, and leaves no container, no
// file, and no token in anything it hands back but the token itself.
func TestReadSetupToken(t *testing.T) {
	longest := integritySentinelToken + strings.Repeat("a", MaxSetupTokenBytes-len(integritySentinelToken))
	for name, stored := range map[string][]byte{
		"as auth add writes it":                 []byte(integritySentinelToken),
		"with a line ending":                    []byte(integritySentinelToken + "\n"),
		"the longest token, with a line ending": []byte(longest + "\n"),
	} {
		t.Run(name, func(t *testing.T) {
			fx := newTokenReadFixture(t, stored, hexSHA256(stored))
			want := strings.TrimRight(string(stored), "\n")
			synctest.Test(t, func(t *testing.T) {
				authorized := 0
				authorize := func(context.Context, RuntimeResourceNames) error {
					authorized++
					if len(fx.specs) != 0 {
						t.Error("the observer was created before its name was authorized")
					}
					return nil
				}
				got, err := ReadSetupToken(
					context.Background(), fx.rt, integrityExporterImage, integrityCredentialVolume, authorize)
				if err != nil {
					t.Fatalf("ReadSetupToken = %v", err)
				}
				if string(got.Token.Reveal()) != want {
					t.Error("the read did not return the stored token")
				}
				wantTree, _ := contentaddr.FromHex(credStateDigest("store-state"))
				if got.TreeDigest != domain.Digest(wantTree) ||
					got.TokenDigest != domain.Digest(contentaddr.Sum(stored)) {
					t.Errorf("digests = %s, %s; want the store's tree and token file digests",
						got.TreeDigest, got.TokenDigest)
				}
				assertNoToken(t, "the read result", got)
				if authorized != 1 || len(fx.specs) != 1 {
					t.Fatalf("authorized %d names and created %d containers, want one of each",
						authorized, len(fx.specs))
				}
				spec := fx.specs[0]
				if !spec.NetworkDisabled || len(spec.Env) != 0 || len(spec.Mounts) != 1 ||
					!spec.Mounts[0].ReadOnly || spec.Mounts[0].Source != integrityCredentialVolume ||
					spec.Image != integrityExporterImage {
					t.Errorf("observer spec is not the pinned image on a networkless read-only mount: %+v", spec)
				}
				assertNoToken(t, "the observer spec", spec)
			})
			fx.assertNothingLeft(t)
		})
	}
}

// TestObserveSetupTokenIntegrityExportsNoToken pins that only the token-read
// observer brings a token out: the integrity probe's command names no copy.
func TestObserveSetupTokenIntegrityExportsNoToken(t *testing.T) {
	cfg := testConfig()
	for _, command := range [][]string{
		credObserverCommand(cfg, "nonce", "/credentials", CredentialManifestSetupToken),
		credIntegrityObserverCommand(cfg, "nonce", "/credentials"),
	} {
		if strings.Contains(strings.Join(command, " "), credTokenCopyPath(cfg)) {
			t.Errorf("an observer that reads no token names the token copy path: %v", command)
		}
	}
}

// TestReadSetupTokenFailsClosed covers every read that must return no token.
// Each also leaves no container and no file behind, and its error carries no
// byte of the store.
func TestReadSetupTokenFailsClosed(t *testing.T) {
	token := []byte(integritySentinelToken)
	cut := token[:len(token)-1]
	spaced := []byte(integritySentinelToken[:20] + " " + integritySentinelToken[20:])
	apiKey := []byte("sk-ant-api" + integritySentinelToken[10:])
	long := bytes.Repeat([]byte("a"), maxStoredSetupTokenBytes+1)
	short := []byte(integritySentinelToken[:MinSetupTokenBytes-1])
	tooLong := []byte(integritySentinelToken + strings.Repeat("a", MaxSetupTokenBytes+1-len(integritySentinelToken)))
	tooLongLine := append(bytes.Clone(tooLong), '\n')
	cases := map[string]struct {
		copied      []byte // nil: the observer leaves no copy
		proofDigest string
		absent      bool // want ErrCredentialStoreAbsent
	}{
		"no token file in the volume":   {nil, credProofTokenAbsent, true},
		"a copy the proof never names":  {nil, hexSHA256(token), false},
		"a cut-off copy":                {cut, hexSHA256(token), false},
		"a substituted copy":            {[]byte(strings.ToUpper(integritySentinelToken)), hexSHA256(token), false},
		"a token shorter than a token":  {short, hexSHA256(short), false},
		"a file longer than a token":    {long, hexSHA256(long), false},
		"a token one byte too long":     {tooLong, hexSHA256(tooLong), false},
		"that token with a line ending": {tooLongLine, hexSHA256(tooLongLine), false},
		"a token with a space in it":    {spaced, hexSHA256(spaced), false},
		"an API key":                    {apiKey, hexSHA256(apiKey), false},
		"an empty file":                 {[]byte{}, hexSHA256(nil), false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fx := newTokenReadFixture(t, tc.copied, tc.proofDigest)
			synctest.Test(t, func(t *testing.T) {
				got, err := ReadSetupToken(
					context.Background(), fx.rt, integrityExporterImage, integrityCredentialVolume, nil)
				if err == nil || len(got.Token.Reveal()) != 0 || got.TreeDigest != "" || got.TokenDigest != "" {
					t.Fatalf("read = %v with a result; want an error and nothing read", err)
				}
				if errors.Is(err, ErrCredentialStoreAbsent) != tc.absent {
					t.Errorf("read = %v; ErrCredentialStoreAbsent wanted: %v", err, tc.absent)
				}
				assertNoToken(t, "the error", err)
				assertNoToken(t, "the error text", err.Error())
				if len(tc.copied) > 8 && strings.Contains(err.Error(), string(tc.copied[:8])) {
					t.Errorf("the error quotes the stored file: %v", err)
				}
			})
			fx.assertNothingLeft(t)
		})
	}

	t.Run("a missing volume starts no observer", func(t *testing.T) {
		f := newFakeRuntime(t)
		_, err := ReadSetupToken(context.Background(), f, integrityExporterImage, integrityCredentialVolume, nil)
		if !errors.Is(err, ErrCredentialStoreAbsent) || f.callCount() != 1 {
			t.Fatalf("missing volume = %v after %d runtime calls, want absent after the volume list", err, f.callCount())
		}
	})
	t.Run("an unpinned exporter image touches no runtime", func(t *testing.T) {
		f := newFakeRuntime(t)
		_, err := ReadSetupToken(context.Background(), f, "registry.test/exporter", integrityCredentialVolume, nil)
		if err == nil || f.callCount() != 0 {
			t.Fatalf("unpinned image = %v after %d runtime calls, want a refusal before any", err, f.callCount())
		}
	})
}

// exportSeam wraps the fake so a test sees the context an export runs under.
type exportSeam struct {
	*fakeRuntime
	export func(ctx context.Context, id string, dest io.Writer, limit int64) error
}

func (s exportSeam) ExportRootFS(ctx context.Context, id string, dest io.Writer, limit int64) error {
	return s.export(ctx, id, dest, limit)
}

// archiveDir returns the one directory the read made under the host
// temporary directory, after checking it and the archive in it are private.
func (fx *tokenReadFixture) archiveDir(t *testing.T) string {
	t.Helper()
	entries, err := os.ReadDir(fx.tmp)
	if err != nil || len(entries) != 1 {
		t.Fatalf("temporary directory holds %d entries (%v), want the read's one directory", len(entries), err)
	}
	dir := filepath.Join(fx.tmp, entries[0].Name())
	archive := filepath.Join(dir, "observer.tar")
	dirInfo, dirErr := os.Stat(dir)
	info, err := os.Stat(archive)
	if err != nil || dirErr != nil || info.Mode().Perm() != 0o600 || dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("%s is not a 0600 file in a 0700 directory (%v, %v)", archive, err, dirErr)
	}
	return dir
}

// TestReadSetupTokenFinishesItsExportWhenCancelled cancels the read while
// the runtime is exporting. A killed export leaves the runtime's own archive
// of the token behind, so the export runs on under its own bound; the read
// then fails, and the daemon's archive and the observer are both gone.
func TestReadSetupTokenFinishesItsExportWhenCancelled(t *testing.T) {
	token := []byte(integritySentinelToken)
	fx := newTokenReadFixture(t, token, hexSHA256(token))
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		exported := false
		rt := exportSeam{fx.rt, func(exportCtx context.Context, id string, dest io.Writer, limit int64) error {
			cancel()
			if err := exportCtx.Err(); err != nil {
				t.Errorf("the caller's cancellation reached the export: %v", err)
			}
			if deadline, ok := exportCtx.Deadline(); !ok || time.Until(deadline) != credTokenExportTimeout {
				t.Errorf("export deadline = %v, %v; want %s from its start", deadline, ok, credTokenExportTimeout)
			}
			if err := fx.rt.ExportRootFS(exportCtx, id, dest, limit); err != nil {
				return err
			}
			// The token is on the host disk when the export returns.
			archive, err := os.ReadFile(filepath.Join(fx.archiveDir(t), "observer.tar"))
			exported = err == nil && bytes.Contains(archive, token)
			return nil
		}}
		got, err := ReadSetupToken(ctx, rt, integrityExporterImage, integrityCredentialVolume, nil)
		if !exported {
			t.Fatal("the export did not finish with the token archived")
		}
		if err == nil || !strings.Contains(err.Error(), context.Canceled.Error()) || len(got.Token.Reveal()) != 0 {
			t.Fatalf("cancelled read = %v with a token; want the cancellation and none", err)
		}
		assertNoToken(t, "the error", err)
	})
	fx.assertNothingLeft(t)
}

// A wedged export is cut off at its own bound, not held open by a caller
// that never cancels.
func TestReadSetupTokenBoundsAWedgedExport(t *testing.T) {
	token := []byte(integritySentinelToken)
	fx := newTokenReadFixture(t, token, hexSHA256(token))
	synctest.Test(t, func(t *testing.T) {
		var waited time.Duration
		rt := exportSeam{fx.rt, func(exportCtx context.Context, _ string, _ io.Writer, _ int64) error {
			start := time.Now()
			<-exportCtx.Done()
			waited = time.Since(start)
			return exportCtx.Err()
		}}
		got, err := ReadSetupToken(context.Background(), rt, integrityExporterImage, integrityCredentialVolume, nil)
		if err == nil || len(got.Token.Reveal()) != 0 || waited != credTokenExportTimeout {
			t.Fatalf("wedged export = %v after %s; want a failure after %s", err, waited, credTokenExportTimeout)
		}
	})
	fx.assertNothingLeft(t)
}

// A token that was read is still not returned when the read cannot finish
// cleanly: the archive that held it could not be removed, the observer could
// not be deleted, or its absence could not be shown.
func TestReadSetupTokenDropsTheTokenWhenItCannotCleanUp(t *testing.T) {
	token := []byte(integritySentinelToken)
	refused := func(t *testing.T, fx *tokenReadFixture, rt Runtime, want string) {
		t.Helper()
		synctest.Test(t, func(t *testing.T) {
			got, err := ReadSetupToken(context.Background(), rt, integrityExporterImage, integrityCredentialVolume, nil)
			if err == nil || !strings.Contains(err.Error(), want) ||
				len(got.Token.Reveal()) != 0 || got.TokenDigest != "" {
				t.Fatalf("read = %v with a result; want %q and nothing read", err, want)
			}
			assertNoToken(t, "the error", err)
		})
	}
	t.Run("the archive directory is not removed", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root removes a file from a read-only directory")
		}
		fx := newTokenReadFixture(t, token, hexSHA256(token))
		rt := exportSeam{fx.rt, func(ctx context.Context, id string, dest io.Writer, limit int64) error {
			if err := fx.rt.ExportRootFS(ctx, id, dest, limit); err != nil {
				return err
			}
			dir := fx.archiveDir(t)
			t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) //nolint:gosec // test-owned directory
			return os.Chmod(dir, 0o500)                    //nolint:gosec // test-owned directory
		}}
		refused(t, fx, rt, "remove credential archive directory")
		fx.rt.mu.Lock()
		defer fx.rt.mu.Unlock()
		if len(fx.rt.ctrs) != 0 {
			t.Errorf("the read left %d container(s) behind", len(fx.rt.ctrs))
		}
	})
	t.Run("the observer is not deleted", func(t *testing.T) {
		fx := newTokenReadFixture(t, token, hexSHA256(token))
		failed := false
		fx.rt.onDeleteContainer = func(string) (bool, error) {
			if failed {
				return false, nil
			}
			failed = true
			return true, errors.New("delete refused")
		}
		refused(t, fx, fx.rt, "delete stopped credential observer")
		fx.assertNothingLeft(t)
	})
	t.Run("the observer's absence is not shown", func(t *testing.T) {
		fx := newTokenReadFixture(t, token, hexSHA256(token))
		deleted, failed := false, false
		fx.rt.onDeleteContainer = func(string) (bool, error) {
			deleted = true
			return false, nil
		}
		fx.rt.onListContainers = func(list []ContainerSummary) ([]ContainerSummary, error) {
			if deleted && !failed {
				failed = true
				return nil, errors.New("list refused")
			}
			return list, nil
		}
		refused(t, fx, fx.rt, "list refused")
		fx.assertNothingLeft(t)
	})
}

// Every registered observer is valid, and the zero value starts nothing.
func TestCredObserversAreRegistered(t *testing.T) {
	for _, observer := range allCredObservers {
		if !observer.valid() {
			t.Errorf("registered observer %q is not valid", observer)
		}
	}
	token := []byte(integritySentinelToken)
	fx := newTokenReadFixture(t, token, hexSHA256(token))
	synctest.Test(t, func(t *testing.T) {
		seen, stored, err := observeSetupTokenStore(context.Background(), fx.rt,
			integrityExporterImage, integrityCredentialVolume, nil, credObserver(""))
		if err == nil || len(stored.Reveal()) != 0 || seen != (SetupTokenIntegrity{}) || len(fx.specs) != 0 {
			t.Fatalf("zero observer = %v after %d containers; want a refusal before any", err, len(fx.specs))
		}
	})
	fx.assertNothingLeft(t)
}

// A setup token renders as its placeholder under every verb; Reveal is the
// one way to its bytes, and it hands out a copy.
func TestSetupTokenRevealIsTheOnlyWayOut(t *testing.T) {
	token, err := NewSetupToken([]byte(integritySentinelToken))
	if err != nil {
		t.Fatal(err)
	}
	assertNoToken(t, "a setup token", token)
	assertNoToken(t, "a setup token in a struct", SetupTokenRead{Token: token})
	revealed := token.Reveal()
	revealed[0] = 'X'
	if string(token.Reveal()) != integritySentinelToken {
		t.Error("Reveal hands out the token's own backing bytes")
	}
	if len((SetupToken{}).Reveal()) != 0 {
		t.Error("the zero token reveals bytes")
	}
}
