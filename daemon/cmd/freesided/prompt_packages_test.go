package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/daemoncompose"
)

const priorArtifactsTestDirective = "<!-- freeside:render-prior-artifacts=v1 -->\n"

func promptPackagesFixture(t *testing.T) (daemoncompose.ClaudeDriverConfig, daemoncompose.PromptPackages) {
	t.Helper()
	root := t.TempDir()
	cfg := daemoncompose.ClaudeDriverConfig{
		PromptPackageFile:              filepath.Join(root, "implementation.md"),
		SpecificationPromptPackageFile: filepath.Join(root, "specification.md"),
		RemediationPromptPackageFile:   filepath.Join(root, "remediation.md"),
	}
	bodies := daemoncompose.PromptPackages{
		Implementation: []byte("Implement exactly.\n\x00Keep these bytes.\n"),
		Specification:  []byte(priorArtifactsTestDirective + "Specify the work.\n"),
		Remediation:    []byte(priorArtifactsTestDirective + "Remediate the findings.\n"),
	}
	for _, file := range []struct {
		path string
		body []byte
	}{
		{cfg.PromptPackageFile, bodies.Implementation},
		{cfg.SpecificationPromptPackageFile, bodies.Specification},
		{cfg.RemediationPromptPackageFile, bodies.Remediation},
	} {
		if err := os.WriteFile(file.path, file.body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return cfg, bodies
}
