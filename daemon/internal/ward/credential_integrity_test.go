package ward

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

const (
	//nolint:gosec // G101 false positive: a fixture volume name, not a credential value.
	integrityCredentialVolume = "freeside-integrity-credential-volume-fixture"
	// integritySentinelToken is a planted credential value: no observation
	// result, error, or container spec may ever carry it.
	integritySentinelToken = "sk-ant-oat-INTEGRITY-SENTINEL-0123456789abcdefghijklmnop" //nolint:gosec // G101: test fixture
)

var integrityExporterImage = "registry.test/exporter@sha256:" + strings.Repeat("0", 64)

func hexSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// runIntegrityScript runs the generated integrity observer script over a
// host directory standing in for the mounted volume and returns the parsed
// proof and its raw text.
func runIntegrityScript(t *testing.T, store string) (credProof, string) {
	t.Helper()
	shell := "sh"
	if dash, err := osexec.LookPath("dash"); err == nil {
		// As TestCredObserverScriptAttestsSymlinks: prefer dash so the
		// generated command stays portable to the image's shell.
		shell = dash
	}
	cfg := testConfig()
	cfg.CredProofPath = filepath.Join(t.TempDir(), "proof.txt")
	nonce := testOwnershipLabel().Value
	script := credIntegrityObserverScript(cfg, nonce, store)
	cmd := osexec.Command(shell, "-c", script) //nolint:gosec // fixed shell and test-owned script
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("integrity observer script: %v: %s", err, out)
	}
	raw, err := os.ReadFile(cfg.CredProofPath)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := parseCredProof(raw, nonce, CredentialManifestOpaque, true)
	if err != nil {
		t.Fatalf("parseCredProof(script output) = %v", err)
	}
	return proof, string(raw)
}

// TestCredIntegrityObserverScript runs the generated script for real: the
// token digest is the hash of the file's bytes (the auth add convention),
// the length verdict follows MinSetupTokenBytes, an absent or symlinked
// token reports absent, and the proof never carries the token.
func TestCredIntegrityObserverScript(t *testing.T) {
	writeToken := func(t *testing.T, body []byte) string {
		t.Helper()
		store := t.TempDir()
		if err := os.WriteFile(filepath.Join(store, "token"), body, 0o400); err != nil {
			t.Fatal(err)
		}
		return store
	}

	t.Run("intact token", func(t *testing.T) {
		token := []byte(integritySentinelToken)
		proof, raw := runIntegrityScript(t, writeToken(t, token))
		if proof.tokenDigest != hexSHA256(token) {
			t.Errorf("token digest = %q, want the hash of the token bytes", proof.tokenDigest)
		}
		if !proof.tokenLengthOK {
			t.Error("a token past the minimum length reported short")
		}
		if strings.Contains(raw, integritySentinelToken) {
			t.Error("the proof carries the token")
		}
	})
	t.Run("length boundary", func(t *testing.T) {
		atBound := []byte(strings.Repeat("a", MinSetupTokenBytes))
		if proof, _ := runIntegrityScript(t, writeToken(t, atBound)); !proof.tokenLengthOK {
			t.Errorf("a %d-byte token reported short", MinSetupTokenBytes)
		}
		below := atBound[:MinSetupTokenBytes-1]
		proof, _ := runIntegrityScript(t, writeToken(t, below))
		if proof.tokenLengthOK {
			t.Errorf("a %d-byte token reported long enough", len(below))
		}
		if proof.tokenDigest != hexSHA256(below) {
			t.Errorf("short token digest = %q, want the hash of its bytes", proof.tokenDigest)
		}
	})
	t.Run("empty token", func(t *testing.T) {
		proof, _ := runIntegrityScript(t, writeToken(t, nil))
		if proof.tokenLengthOK || proof.tokenDigest != hexSHA256(nil) {
			t.Errorf("empty token = %+v, want short with the empty-input digest", proof)
		}
	})
	t.Run("absent token", func(t *testing.T) {
		proof, _ := runIntegrityScript(t, t.TempDir())
		if proof.tokenDigest != "" || proof.tokenLengthOK {
			t.Errorf("empty volume = %+v, want no token facts", proof)
		}
	})
	t.Run("symlinked token", func(t *testing.T) {
		store := t.TempDir()
		target := filepath.Join(t.TempDir(), "elsewhere")
		if err := os.WriteFile(target, []byte(integritySentinelToken), 0o400); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(store, "token")); err != nil {
			t.Fatal(err)
		}
		proof, _ := runIntegrityScript(t, store)
		if proof.tokenDigest != "" {
			t.Error("a symlinked token was hashed through the link")
		}
	})
}

// TestCredIntegrityObserverScriptKeepsTheGateScript pins the two properties
// the handoff gate relies on: the integrity script starts with the gate's
// opaque script byte for byte, so the gate's own command is unchanged, and
// both report the same tree digest for the same store.
func TestCredIntegrityObserverScriptKeepsTheGateScript(t *testing.T) {
	cfg := testConfig()
	nonce := testOwnershipLabel().Value
	gate := credObserverScript(cfg, nonce, "/credentials", CredentialManifestOpaque)
	integrity := credIntegrityObserverScript(cfg, nonce, "/credentials")
	if !strings.HasPrefix(integrity, gate+"; ") {
		t.Fatal("the integrity script does not start with the gate's opaque script")
	}

	store := t.TempDir()
	if err := os.WriteFile(filepath.Join(store, "token"), []byte(integritySentinelToken), 0o400); err != nil {
		t.Fatal(err)
	}
	integrityProof, _ := runIntegrityScript(t, store)

	cfg.CredProofPath = filepath.Join(t.TempDir(), "proof.txt")
	script := credObserverScript(cfg, nonce, store, CredentialManifestOpaque)
	if out, err := osexec.Command("sh", "-c", script).CombinedOutput(); err != nil { //nolint:gosec // test-owned script
		t.Fatalf("gate observer script: %v: %s", err, out)
	}
	raw, err := os.ReadFile(cfg.CredProofPath)
	if err != nil {
		t.Fatal(err)
	}
	gateTree, err := verifyCredProof(raw, nonce, CredentialManifestOpaque)
	if err != nil {
		t.Fatal(err)
	}
	if integrityProof.tree != gateTree {
		t.Errorf("integrity tree digest %q differs from the gate's %q", integrityProof.tree, gateTree)
	}
}

// TestParseCredProofIntegrity enumerates the integrity keys' input space.
// The proof comes out of an unscanned archive, so a shape the script cannot
// write is refused, and the handoff gate's mode keeps refusing both keys.
func TestParseCredProofIntegrity(t *testing.T) {
	const nonce = "00000000000000000000000000000000"
	tree := strings.Repeat("ab", 32)
	token := strings.Repeat("cd", 32)
	base := "nonce=" + nonce + "\ncred_tree=" + tree + "\n"
	present := base + "cred_token_sha256=" + token + "\ncred_token_length=ok\n"

	got, err := parseCredProof([]byte(present), nonce, CredentialManifestOpaque, true)
	if err != nil || got != (credProof{tree: tree, tokenDigest: token, tokenLengthOK: true}) {
		t.Fatalf("parseCredProof(present) = %+v, %v", got, err)
	}
	short := base + "cred_token_sha256=" + token + "\ncred_token_length=short\n"
	got, err = parseCredProof([]byte(short), nonce, CredentialManifestOpaque, true)
	if err != nil || got != (credProof{tree: tree, tokenDigest: token}) {
		t.Fatalf("parseCredProof(short) = %+v, %v", got, err)
	}
	absent := base + "cred_token_sha256=absent\ncred_token_length=absent\n"
	got, err = parseCredProof([]byte(absent), nonce, CredentialManifestOpaque, true)
	if err != nil || got != (credProof{tree: tree}) {
		t.Fatalf("parseCredProof(absent) = %+v, %v", got, err)
	}

	refused := []struct {
		name      string
		proof     string
		integrity bool
	}{
		{"integrity keys outside integrity mode", present, false},
		{"digest key alone outside integrity mode", base + "cred_token_sha256=" + token + "\n", false},
		{"no integrity keys in integrity mode", base, true},
		{"missing length", base + "cred_token_sha256=" + token + "\n", true},
		{"missing digest", base + "cred_token_length=ok\n", true},
		{"digest without a length", base + "cred_token_sha256=" + token + "\ncred_token_length=absent\n", true},
		{"length without a digest", base + "cred_token_sha256=absent\ncred_token_length=short\n", true},
		{"unreadable token leaves the digest empty", base + "cred_token_sha256=\ncred_token_length=ok\n", true},
		{"byte count the image could not produce", base + "cred_token_sha256=" + token + "\ncred_token_length=unknown\n", true},
		{"digest is not hex", base + "cred_token_sha256=" + strings.Repeat("zz", 32) + "\ncred_token_length=ok\n", true},
		{"repeated digest", present + "cred_token_sha256=" + token + "\n", true},
		{"unknown key", present + "cred_token_bytes=40\n", true},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseCredProof([]byte(tc.proof), nonce, CredentialManifestOpaque, tc.integrity)
			if !errors.Is(err, ErrConformance) {
				t.Fatalf("parseCredProof = %v, want ErrConformance", err)
			}
		})
	}
}

// integrityProofLines makes the fake runtime's synthesized credential proof
// an integrity proof by appending the two token keys.
func integrityProofLines(digest, length string) func(string, []byte) []byte {
	return func(_ string, proof []byte) []byte {
		return fmt.Appendf(proof, "%s=%s\n%s=%s\n",
			credProofTokenDigestKey, digest, credProofTokenLengthKey, length)
	}
}

// TestObserveSetupTokenIntegrity drives the observation through the fake
// runtime: it reports both digest conventions and the length verdict, runs
// one read-only networkless observer and reaps it, and neither its result
// nor its container spec carries a credential value.
func TestObserveSetupTokenIntegrity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFakeRuntime(t)
		if err := f.CreateVolume(context.Background(), integrityCredentialVolume, 1, nil); err != nil {
			t.Fatal(err)
		}
		f.credState[integrityCredentialVolume] = "store-state"
		tokenHex := hexSHA256([]byte(integritySentinelToken))
		f.observerProof = integrityProofLines(tokenHex, credProofTokenLengthOK)
		var specs []ContainerSpec
		f.onCreateContainer = func(spec ContainerSpec) error {
			specs = append(specs, spec)
			return nil
		}
		authorized := 0
		authorize := func(context.Context, RuntimeResourceNames) error {
			authorized++
			if len(specs) != 0 {
				t.Error("the observer was created before its name was authorized")
			}
			return nil
		}

		got, err := ObserveSetupTokenIntegrity(
			context.Background(), f, integrityExporterImage, integrityCredentialVolume, authorize)
		if err != nil {
			t.Fatalf("ObserveSetupTokenIntegrity = %v", err)
		}
		wantTree, _ := contentaddr.FromHex(credStateDigest("store-state"))
		wantToken, _ := contentaddr.FromHex(tokenHex)
		want := SetupTokenIntegrity{TreeDigest: domain.Digest(wantTree), TokenDigest: domain.Digest(wantToken)}
		if got != want {
			t.Errorf("observation = %+v, want %+v", got, want)
		}
		if authorized != 1 || len(specs) != 1 {
			t.Fatalf("authorized %d names and created %d containers, want one of each", authorized, len(specs))
		}
		spec := specs[0]
		if !spec.NetworkDisabled || len(spec.Env) != 0 || len(spec.Mounts) != 1 ||
			!spec.Mounts[0].ReadOnly || spec.Mounts[0].Source != integrityCredentialVolume {
			t.Errorf("observer spec is not a networkless read-only mount of the volume: %+v", spec)
		}
		if strings.Contains(fmt.Sprintf("%+v", spec), integritySentinelToken) {
			t.Error("the observer spec carries the token")
		}
		f.mu.Lock()
		left := len(f.ctrs)
		f.mu.Unlock()
		if left != 0 {
			t.Errorf("the observation left %d container(s) behind", left)
		}

		f.observerProof = integrityProofLines(tokenHex, credProofTokenLengthShort)
		got, err = ObserveSetupTokenIntegrity(
			context.Background(), f, integrityExporterImage, integrityCredentialVolume, nil)
		if err != nil || !got.Truncated {
			t.Errorf("short token observation = %+v, %v; want Truncated", got, err)
		}
	})
}

// TestObserveSetupTokenIntegrityNeverReportsAnUnobservedStore covers the
// cases the probe must report as not checked: each is an error, never a
// truncation verdict.
func TestObserveSetupTokenIntegrityNeverReportsAnUnobservedStore(t *testing.T) {
	t.Run("missing volume starts no observer", func(t *testing.T) {
		f := newFakeRuntime(t)
		_, err := ObserveSetupTokenIntegrity(
			context.Background(), f, integrityExporterImage, integrityCredentialVolume, nil)
		if !errors.Is(err, ErrCredentialStoreAbsent) {
			t.Fatalf("missing volume = %v, want ErrCredentialStoreAbsent", err)
		}
		if f.callIndex("list-volumes") != 0 || f.callCount() != 1 {
			t.Errorf("a missing volume drove %d runtime calls, want only the volume list", f.callCount())
		}
	})
	t.Run("unpinned exporter image touches no runtime", func(t *testing.T) {
		f := newFakeRuntime(t)
		_, err := ObserveSetupTokenIntegrity(
			context.Background(), f, "registry.test/exporter", integrityCredentialVolume, nil)
		if err == nil || f.callCount() != 0 {
			t.Fatalf("unpinned image = %v after %d runtime calls, want a refusal before any", err, f.callCount())
		}
	})
	t.Run("volume without a token file", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f := newFakeRuntime(t)
			if err := f.CreateVolume(context.Background(), integrityCredentialVolume, 1, nil); err != nil {
				t.Fatal(err)
			}
			f.observerProof = integrityProofLines(credProofTokenAbsent, credProofTokenAbsent)
			_, err := ObserveSetupTokenIntegrity(
				context.Background(), f, integrityExporterImage, integrityCredentialVolume, nil)
			if !errors.Is(err, ErrCredentialStoreAbsent) {
				t.Fatalf("token-less volume = %v, want ErrCredentialStoreAbsent", err)
			}
		})
	})
	t.Run("proof without the token facts", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f := newFakeRuntime(t)
			if err := f.CreateVolume(context.Background(), integrityCredentialVolume, 1, nil); err != nil {
				t.Fatal(err)
			}
			// The fake's unmodified proof is the handoff gate's: no token keys.
			got, err := ObserveSetupTokenIntegrity(
				context.Background(), f, integrityExporterImage, integrityCredentialVolume, nil)
			if !errors.Is(err, ErrConformance) || got != (SetupTokenIntegrity{}) {
				t.Fatalf("gate-shaped proof = %+v, %v; want ErrConformance and no observation", got, err)
			}
			f.mu.Lock()
			left := len(f.ctrs)
			f.mu.Unlock()
			if left != 0 {
				t.Errorf("a failed observation left %d container(s) behind", left)
			}
		})
	})
}

// codexIntegrityStore writes body as a private auth.json under a private
// root and returns the resolved root and path.
func codexIntegrityStore(t *testing.T, body []byte) (root, path string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o700); err != nil { //nolint:gosec // a private directory is the fixture
		t.Fatal(err)
	}
	path = filepath.Join(root, "auth.json")
	if body != nil {
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root, path
}

// TestObserveCodexStoreIntegrity: truncation is an empty file or one that
// ends before its JSON document closes. Bytes that are not JSON are not
// truncation, and the digest is the hash of the file.
func TestObserveCodexStoreIntegrity(t *testing.T) {
	complete := `{"tokens":{"access_token":"` + integritySentinelToken + `"}}`
	cases := []struct {
		name      string
		body      string
		truncated bool
	}{
		{"complete document", complete, false},
		{"complete document with a trailing newline", complete + "\n", false},
		{"empty file", "", true},
		{"whitespace only", " \n", true},
		{"cut off inside a string", complete[:len(complete)-8], true},
		{"cut off before the closing brace", complete[:len(complete)-1], true},
		{"cut off inside a literal", `{"refresh":tru`, true},
		{"not JSON", "not json at all", false},
		{"closed document followed by a cut-off one", complete + `{"tokens":`, false},
		{"closed document followed by other bytes", complete + "trailing", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, path := codexIntegrityStore(t, []byte(tc.body))
			got, err := ObserveCodexStoreIntegrity(root, path)
			if err != nil {
				t.Fatalf("ObserveCodexStoreIntegrity = %v", err)
			}
			want := CodexStoreIntegrity{
				ContentDigest: domain.Digest(contentaddr.Sum([]byte(tc.body))),
				Truncated:     tc.truncated,
			}
			if got != want {
				t.Errorf("observation = %+v, want %+v", got, want)
			}
		})
	}
}

// TestObserveCodexStoreIntegrityNeverReportsAnUnobservedStore: a store the
// reader refuses is an error, never a truncation verdict, and no error
// echoes file content.
func TestObserveCodexStoreIntegrityNeverReportsAnUnobservedStore(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		root, path := codexIntegrityStore(t, nil)
		_, err := ObserveCodexStoreIntegrity(root, path)
		if !errors.Is(err, ErrCredentialStoreAbsent) {
			t.Fatalf("missing file = %v, want ErrCredentialStoreAbsent", err)
		}
	})
	t.Run("group-readable file", func(t *testing.T) {
		root, path := codexIntegrityStore(t, []byte(integritySentinelToken))
		if err := os.Chmod(path, 0o640); err != nil { //nolint:gosec // the loosened mode is what the reader must refuse
			t.Fatal(err)
		}
		got, err := ObserveCodexStoreIntegrity(root, path)
		if err == nil || errors.Is(err, ErrCredentialStoreAbsent) || got != (CodexStoreIntegrity{}) {
			t.Fatalf("group-readable store = %+v, %v; want a refusal that is not absence", got, err)
		}
		if strings.Contains(err.Error(), integritySentinelToken) {
			t.Error("the refusal echoes file content")
		}
	})
	t.Run("path outside the root", func(t *testing.T) {
		root, _ := codexIntegrityStore(t, nil)
		_, outside := codexIntegrityStore(t, []byte(`{}`))
		if _, err := ObserveCodexStoreIntegrity(root, outside); err == nil {
			t.Fatal("a store outside the private root was observed")
		}
	})
	t.Run("root that is not private", func(t *testing.T) {
		root, path := codexIntegrityStore(t, []byte(`{}`))
		if err := os.Chmod(root, 0o755); err != nil { //nolint:gosec // the loosened mode is what the resolver must refuse
			t.Fatal(err)
		}
		if _, err := ObserveCodexStoreIntegrity(root, path); err == nil {
			t.Fatal("a store under a non-private root was observed")
		}
	})
}
