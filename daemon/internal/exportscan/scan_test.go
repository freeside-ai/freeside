package exportscan_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/exportscan"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

func TestCredentialScannerBlocksLeakedMaterial(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
	}{
		{"anthropic key", "config: sk-ant-api03-" + strings.Repeat("A", 40)},
		{"github token", "remote: https://ghp_" + strings.Repeat("b", 36) + "@github.com/x/y"},
		{"github fine-grained token", "token=github_pat_" + strings.Repeat("c", 24)},
		{"slack token", "SLACK_TOKEN=xoxb-" + strings.Repeat("d", 24)},
		{"pem private key", "-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n"},
		{"pem private key with numeric label", "-----BEGIN PKCS8 PRIVATE KEY-----\nabc\n"},
		{"aws access key", "AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE"},
		{"gcp service-account key id", `"private_key_id":"` + strings.Repeat("e", 40) + `"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			// Nested, so the walk is exercised rather than a flat listing.
			nested := filepath.Join(dir, "blobs", "sha256")
			if err := os.MkdirAll(nested, 0o700); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			leaked := filepath.Join(nested, "leaked.txt")
			if err := os.WriteFile(leaked, []byte(tc.body), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			err := (exportscan.Scanner{}).Scan(context.Background(), dir)
			if !errors.Is(err, ward.ErrOutputScanRefused) {
				t.Fatalf("scanner did not classify credential material as a policy refusal: %v", err)
			}
			// The error locates the leak without reproducing the secret: an
			// error a client or an attention item can carry must not become a
			// second copy of the credential.
			if strings.Contains(err.Error(), tc.body) {
				t.Fatalf("scanner error echoes the matched material: %v", err)
			}
			if !strings.Contains(err.Error(), "leaked.txt") {
				t.Fatalf("scanner error does not name the file: %v", err)
			}
		})
	}
}

func TestCredentialScannerIOFailureIsNotPolicyRefusal(t *testing.T) {
	t.Parallel()
	err := (exportscan.Scanner{}).Scan(t.Context(), filepath.Join(t.TempDir(), "missing"))
	if !errors.Is(err, os.ErrNotExist) || errors.Is(err, ward.ErrOutputScanRefused) {
		t.Fatalf("operational scan failure classified incorrectly: %v", err)
	}
}

func TestCredentialScannerAdmitsOrdinaryOutput(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	files := map[string]string{
		"manifest.json": `{"version":"freeside.export.manifest/v1","entries":[]}`,
		// Prose that mentions credentials without carrying any: the scanner
		// must not fire on vocabulary, or every honest run tripping it would
		// train the operator to ignore it.
		"README.md": "Set the API key in the keychain; never commit a private key.\n",
		"code.go":   "const tokenHeader = \"Authorization\"\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := (exportscan.Scanner{}).Scan(context.Background(), dir); err != nil {
		t.Fatalf("scanner rejected ordinary output: %v", err)
	}
}
