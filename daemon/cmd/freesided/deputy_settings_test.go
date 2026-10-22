package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// deputySettingsFixture is a complete, valid settings object as generic JSON,
// so a case can delete or replace any one key.
func deputySettingsFixture(t *testing.T) map[string]any {
	t.Helper()
	image := func(name string) string { return "registry.local/" + name + "@sha256:" + strings.Repeat("a", 64) }
	return map[string]any{
		"version":   deputySettingsVersion,
		"listen":    "127.0.0.1:7340",
		"seed_root": "/rig/seed",
		"images": map[string]any{
			"agent": image("agent"), "exporter": image("exporter"), "review": image("review"),
		},
		"approved_recipe": "sha256:" + strings.Repeat("b", 64),
		"repository": map[string]any{
			"repo": "acme/widgets", "repository_id": 42, "base_ref": "main",
			"base_sha": strings.Repeat("c", 40), "allowed_paths": []string{"src/**"},
			"checkout": "/src/widgets", "project": "project-widgets",
		},
		"prompts": map[string]any{
			"prompt_package": "/prompts/impl.json", "specification_prompt_package": "/prompts/spec.json",
			"remediation_prompt_package": "/prompts/rem.json", "vendor_instructions": "/prompts/CLAUDE.md",
		},
		"agent_tree": map[string]any{
			"checkout": "/src/freeside", "commit": strings.Repeat("d", 40),
			"auth_identity": "claude-1", "review_auth_identity": "codex-1",
			"cost_owner": "owner", "review_cost_owner": "owner", "claude_account": "me@example.test",
			"terms_basis_date": "2026-10-01", "pricing_revision": "2026-10",
			"offer_not_after": "2027-10-01T00:00:00Z", "auth_volume": "claude-auth",
		},
		"review": map[string]any{
			"input_root": "/review", "auth_mode": "subscription", "auth_snapshot": "/review/auth.json",
			"instructions": "/review/AGENTS.md", "model": "gpt-5", "effort": "high",
		},
		"writers": map[string]any{
			"specifier":   map[string]any{"model": "claude-fable", "effort": "high"},
			"implementer": map[string]any{"model": "claude-opus", "effort": "high"},
			"remediator":  map[string]any{"model": "claude-opus", "effort": "high"},
		},
		"judgment": map[string]any{
			"claude_bin": "", "claude_sha256": "", "model": "", "effort": "", "publication_author_prompt": "",
		},
		"prod": map[string]any{ //nolint:gosec // G101 false positive: fixture directory paths, not credentials.
			"app_state_dir": "/prod/daemon", "app_credentials_dir": "/prod/credentials",
			"installed_daemon": "/Applications/Freeside.app/Contents/MacOS/freesided",
		},
		"timeouts":                 map[string]any{"writer_stop": "20m", "writer_stall": "0s"},
		"manual_submission_config": "",
		"build":                    map[string]any{"proxy": "", "base_build_ref": "", "dns": []string{}},
	}
}

func writeDeputySettings(t *testing.T, settings map[string]any, mode os.FileMode) (string, []byte) {
	t.Helper()
	body, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path, body
}

func TestLoadDeputySettingsAcceptsCompleteFile(t *testing.T) {
	t.Parallel()
	path, body := writeDeputySettings(t, deputySettingsFixture(t), 0o600)
	loaded, err := loadDeputySettings(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := domain.Digest(contentaddr.Sum(body)); loaded.Digest != want {
		t.Fatalf("digest %s, want the file bytes' %s", loaded.Digest, want)
	}
	if loaded.Repository.Repo != "acme/widgets" || loaded.stopTime.String() != "20m0s" || loaded.stallFor != 0 {
		t.Fatalf("decoded %+v", loaded)
	}
}

func TestLoadDeputySettingsRefusesNamingTheField(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		edit func(map[string]any)
		want string
	}{
		"missing top-level key": {func(s map[string]any) { delete(s, "listen") }, "listen is required"},
		"missing nested key": {func(s map[string]any) {
			delete(s["agent_tree"].(map[string]any), "commit")
		}, "agent_tree.commit is required"},
		"null value":  {func(s map[string]any) { s["manual_submission_config"] = nil }, "manual_submission_config is required"},
		"unknown key": {func(s map[string]any) { s["diagnostic_dir"] = "/tmp" }, `unknown field "diagnostic_dir"`},
		"unknown nested key": {func(s map[string]any) {
			s["review"].(map[string]any)["reasoning"] = "high"
		}, `unknown field "reasoning"`},
		"wrong version": {func(s map[string]any) { s["version"] = "freeside-deputy-settings-v0" }, "version"},
		"tag-only image": {func(s map[string]any) {
			s["images"].(map[string]any)["review"] = "registry.local/review:latest"
		}, "images.review is not a digest-pinned image reference"},
		"relative path": {func(s map[string]any) {
			s["prod"].(map[string]any)["app_state_dir"] = "prod/daemon"
		}, "prod.app_state_dir must be an absolute path"},
		"partial judgment": {func(s map[string]any) {
			s["judgment"].(map[string]any)["model"] = "claude-opus"
		}, "judgment fields must be all empty or all set"},
		"same identity twice": {func(s map[string]any) {
			s["agent_tree"].(map[string]any)["review_auth_identity"] = "claude-1"
		}, "agent_tree.review_auth_identity must differ"},
		"bad auth mode": {func(s map[string]any) {
			s["review"].(map[string]any)["auth_mode"] = "oauth"
		}, "review.auth_mode"},
		"unsupported api key": {func(s map[string]any) {
			s["review"].(map[string]any)["auth_mode"] = "api_key"
		}, "review.auth_mode must be subscription; api_key enrollment is not supported yet (#1677)"},
		"subscription with a Claude reviewer": {func(s map[string]any) {
			s["agent_tree"].(map[string]any)["review_harness"] = "claude_code"
		}, "review.auth_mode must be setup_token with agent_tree.review_harness claude_code"},
		"setup token with a Codex reviewer": {func(s map[string]any) {
			s["agent_tree"].(map[string]any)["review_harness"] = "codex_cli"
			s["review"].(map[string]any)["auth_mode"] = "setup_token"
		}, "review.auth_mode must be subscription"},
		"setup token with the harness absent": {func(s map[string]any) {
			s["review"].(map[string]any)["auth_mode"] = "setup_token"
		}, "review.auth_mode must be subscription"},
		"unknown review harness": {func(s map[string]any) {
			s["agent_tree"].(map[string]any)["review_harness"] = "gemini_cli"
		}, `agent_tree.review_harness "gemini_cli" is not codex_cli or claude_code`},
		"null review harness": {func(s map[string]any) {
			s["agent_tree"].(map[string]any)["review_harness"] = nil
		}, "agent_tree.review_harness is required"},
		"root vendor instructions": {func(s map[string]any) {
			s["prompts"].(map[string]any)["vendor_instructions"] = "/"
		}, "prompts.vendor_instructions must not be the root directory"},
		"bad duration": {func(s map[string]any) {
			s["timeouts"].(map[string]any)["writer_stop"] = "twenty minutes"
		}, "timeouts.writer_stop"},
		"dns without base": {func(s map[string]any) {
			s["build"].(map[string]any)["dns"] = []string{"1.1.1.1"}
		}, "build.dns requires build.base_build_ref"},
		"empty allowed paths": {func(s map[string]any) {
			s["repository"].(map[string]any)["allowed_paths"] = []string{}
		}, "repository.allowed_paths is empty"},
		"match-everything paths": {func(s map[string]any) {
			s["repository"].(map[string]any)["allowed_paths"] = []string{"**"}
		}, "repository.allowed_paths must name explicit declared paths"},
		"no specifier model": {func(s map[string]any) {
			s["writers"].(map[string]any)["specifier"].(map[string]any)["model"] = ""
		}, "writers.specifier.model and writers.specifier.effort are required"},
		"no remediator effort": {func(s map[string]any) {
			s["writers"].(map[string]any)["remediator"].(map[string]any)["effort"] = ""
		}, "writers.remediator.model and writers.remediator.effort are required"},
		"no implementer key": {func(s map[string]any) {
			delete(s["writers"].(map[string]any), "implementer")
		}, "writers.implementer is required"},
		"judgment effort alone": {func(s map[string]any) {
			s["judgment"].(map[string]any)["effort"] = "medium"
		}, "judgment fields must be all empty or all set"},
	}
	for _, field := range []string{
		"seed_root", "repository.checkout",
		"prompts.prompt_package", "prompts.specification_prompt_package", "prompts.remediation_prompt_package",
		"prompts.vendor_instructions", "agent_tree.checkout", "review.input_root", "review.auth_snapshot",
		"review.instructions", "prod.app_state_dir", "prod.app_credentials_dir", "prod.installed_daemon",
		"judgment.claude_bin", "judgment.publication_author_prompt", "manual_submission_config",
	} {
		for _, value := range []string{"relative/path", "/configured/../path", "/configured//path", "/configured/path/"} {
			want := field + " must be a clean path"
			if !filepath.IsAbs(value) {
				want = field + " must be an absolute path"
			}
			cases[field+" "+value] = struct {
				edit func(map[string]any)
				want string
			}{
				edit: func(s map[string]any) {
					if strings.HasPrefix(field, "judgment.") {
						s["judgment"] = map[string]any{
							"claude_bin": "/bin/claude", "claude_sha256": strings.Repeat("a", 64),
							"model": "claude-opus", "effort": "medium", "publication_author_prompt": "/prompts/author.md",
						}
					}
					object, key := s, field
					if group, nested, ok := strings.Cut(field, "."); ok {
						object, key = s[group].(map[string]any), nested
					}
					object[key] = value
				},
				want: want,
			}
		}
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			settings := deputySettingsFixture(t)
			tc.edit(settings)
			path, _ := writeDeputySettings(t, settings, 0o600)
			_, err := loadDeputySettings(path)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v, want one naming %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "\n") {
				t.Fatalf("refusal spans lines: %q", err)
			}
		})
	}
}

func TestLoadDeputySettingsAcceptsConfiguredOptionalPaths(t *testing.T) {
	t.Parallel()
	settings := deputySettingsFixture(t)
	settings["judgment"] = map[string]any{
		"claude_bin": "/bin/claude", "claude_sha256": strings.Repeat("a", 64),
		"model": "claude-opus", "effort": "medium", "publication_author_prompt": "/prompts/author.md",
	}
	settings["manual_submission_config"] = "/config/manual.json"
	path, _ := writeDeputySettings(t, settings, 0o600)
	if _, err := loadDeputySettings(path); err != nil {
		t.Fatal(err)
	}
}

func TestLoadDeputySettingsRefusesWrongModeAndDuplicateKeys(t *testing.T) {
	t.Parallel()
	path, _ := writeDeputySettings(t, deputySettingsFixture(t), 0o644)
	if _, err := loadDeputySettings(path); err == nil || !strings.Contains(err.Error(), "want 0600") {
		t.Fatalf("0644 file: %v", err)
	}
	duplicate := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(duplicate, []byte(`{"version":"a","version":"b"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadDeputySettings(duplicate); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate key: %v", err)
	}
	link := filepath.Join(t.TempDir(), "settings.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := loadDeputySettings(link); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("symlink: %v", err)
	}
	fifo := filepath.Join(t.TempDir(), "settings.json")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadDeputySettings(fifo); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("fifo: %v", err)
	}
}

// agent_tree.review_harness may be absent, which reads as codex_cli, the
// harness every settings file before #397 ran; each harness is accepted with
// the one auth mode the deputy adopts for it, and adoption is asked for that
// harness.
func TestLoadDeputySettingsReviewHarness(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		harness, mode string
		want          domain.HarnessClientKind
		storeRoot     bool
	}{
		"absent":      {"", "subscription", domain.HarnessClientCodexCLI, true},
		"codex_cli":   {"codex_cli", "subscription", domain.HarnessClientCodexCLI, true},
		"claude_code": {"claude_code", "setup_token", domain.HarnessClientClaudeCode, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			settings := deputySettingsFixture(t)
			if tc.harness != "" {
				settings["agent_tree"].(map[string]any)["review_harness"] = tc.harness
			}
			settings["review"].(map[string]any)["auth_mode"] = tc.mode
			path, _ := writeDeputySettings(t, settings, 0o600)
			loaded, err := loadDeputySettings(path)
			if err != nil {
				t.Fatal(err)
			}
			if loaded.reviewHarness() != tc.want {
				t.Fatalf("review harness = %s, want %s", loaded.reviewHarness(), tc.want)
			}
			args := deputyAdoptArgs(loaded, newDeputyPaths(t.TempDir()))
			harness := slices.Index(args, "-review-harness")
			if harness < 0 || args[harness+1] != string(tc.want) ||
				slices.Contains(args, "-auth-store-root") != tc.storeRoot {
				t.Fatalf("adopt args = %v", args)
			}
			if _, err := parseAuthAdoptConfig(args, io.Discard, time.Now()); err != nil {
				t.Fatalf("auth adopt refuses the deputy's args: %v", err)
			}
		})
	}
}
