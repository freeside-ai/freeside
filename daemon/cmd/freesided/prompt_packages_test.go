package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
)

const priorArtifactsTestDirective = "<!-- freeside:render-prior-artifacts=v1 -->\n"

func promptPackagesFixture(t *testing.T) (claudeDriverConfig, promptPackages) {
	t.Helper()
	root := t.TempDir()
	cfg := claudeDriverConfig{
		PromptPackageFile:              filepath.Join(root, "implementation.md"),
		SpecificationPromptPackageFile: filepath.Join(root, "specification.md"),
		RemediationPromptPackageFile:   filepath.Join(root, "remediation.md"),
	}
	bodies := promptPackages{
		implementation: []byte("Implement exactly.\n\x00Keep these bytes.\n"),
		specification:  []byte(priorArtifactsTestDirective + "Specify the work.\n"),
		remediation:    []byte(priorArtifactsTestDirective + "Remediate the findings.\n"),
	}
	for _, file := range []struct {
		path string
		body []byte
	}{
		{cfg.PromptPackageFile, bodies.implementation},
		{cfg.SpecificationPromptPackageFile, bodies.specification},
		{cfg.RemediationPromptPackageFile, bodies.remediation},
	} {
		if err := os.WriteFile(file.path, file.body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return cfg, bodies
}

func TestReadPromptPackagesStoresExactValidatedBytes(t *testing.T) {
	t.Parallel()
	cfg, want := promptPackagesFixture(t)
	packages, err := readPromptPackages(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// A file replacement after validation must not change what composition
	// persists, or the role check would attest different bytes.
	for _, path := range []string{cfg.PromptPackageFile, cfg.SpecificationPromptPackageFile, cfg.RemediationPromptPackageFile} {
		if err := os.WriteFile(path, []byte("replacement"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	blobs, err := signet.NewBlobStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		got  []byte
		want []byte
	}{
		{"implementation", packages.implementation, want.implementation},
		{"specification", packages.specification, want.specification},
		{"remediation", packages.remediation, want.remediation},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !bytes.Equal(tc.got, tc.want) {
				t.Fatal("loader changed prompt bytes")
			}
			digest, err := storePromptPackage(blobs, tc.got)
			if err != nil {
				t.Fatal(err)
			}
			if digest != domain.Digest(contentaddr.Sum(tc.want)) {
				t.Fatalf("digest %s does not identify the validated bytes", digest)
			}
			reader, err := blobs.Open(digest)
			if err != nil {
				t.Fatal(err)
			}
			body, readErr := io.ReadAll(reader)
			if err := errors.Join(readErr, reader.Close()); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(body, tc.want) {
				t.Fatal("stored bytes differ from validated bytes")
			}
		})
	}
}

func TestStorePromptPackagePropagatesWriteFailure(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "blobs")
	blobs, err := signet.NewBlobStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(root); err != nil {
		t.Fatal(err)
	}
	digest, err := storePromptPackage(blobs, []byte("prompt"))
	if !errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), "store prompt package") || digest != "" {
		t.Fatalf("digest %q, error %v, want a contextual write failure", digest, err)
	}
}
