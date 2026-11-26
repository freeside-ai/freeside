package cordon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/gitenv"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

func transcriptSource() export.EvidenceSource {
	return export.EvidenceSource{
		Label: "agent-transcript", MediaType: "application/jsonl",
		Path: export.EvidenceWorkspaceDir + "/agent-transcript.jsonl", HeadBinding: export.EvidenceHeadIndependent,
		SensitivityClass: export.EvidenceSensitivitySensitive, ProducerInvocationID: "inv-1",
	}
}

func summarySource() export.EvidenceSource {
	return export.EvidenceSource{
		Label: export.SummaryEvidenceLabel, MediaType: "text/markdown",
		Path: export.SummaryEvidencePath, HeadBinding: export.EvidenceHeadIndependent,
		SensitivityClass: export.EvidenceSensitivitySensitive, ProducerInvocationID: "inv-1",
	}
}

func exportRequest(id string) ExportRequest {
	return ExportRequest{RunID: id, Transcript: transcriptSource(), Sources: []export.EvidenceSource{summarySource()}}
}

// writeIn writes body at rel under dir, creating its parents.
func writeIn(t *testing.T, dir, rel, body string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Error(err)
		return
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Error(err)
	}
}

// agentWriting is a fake agent that runs change in its worktree, prints a
// transcript line and a stderr line, and exits with code.
func agentWriting(code int, change func(worktree string)) agentScript {
	return func(spec processSpec, p *fakeProcess) {
		change(spec.dir)
		_, _ = io.WriteString(spec.stdout, "{\"type\":\"result\"}\n")
		_, _ = io.WriteString(spec.stderr, "a warning\n")
		p.exit(code)
	}
}

// launched runs one fake launch and returns its backend.
func launched(t *testing.T, cfg Config, id string, script agentScript) *Backend {
	t.Helper()
	b := fakeBackend(t, cfg, &fakeLauncher{host: newFakeHost(), script: script})
	if _, err := b.Launch(t.Context(), request(id)); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	return b
}

func evidenceLabels(m export.EvidenceManifest) []string {
	var labels []string
	for _, e := range m.Entries {
		labels = append(labels, e.Label)
	}
	return labels
}

func TestExportWritesTheHandoffOfACleanLaunch(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	synctest.Test(t, func(t *testing.T) {
		b := launched(t, cfg, "run-export", agentWriting(0, func(wt string) {
			writeIn(t, wt, "src/change.txt", "the change\n")
			writeIn(t, wt, export.SummaryEvidencePath, "# Summary\n")
			writeIn(t, wt, export.CommitPlanFilename, "{}")
		}))
		res, err := b.Export(t.Context(), exportRequest("run-export"))
		if err != nil {
			t.Fatalf("Export: %v", err)
		}
		l := layoutFor(cfg.RunsRoot, "run-export")
		if res.Dir != l.export || res.BaseSHA != request("run-export").BaseSHA {
			t.Fatalf("result dir %s base %s", res.Dir, res.BaseSHA)
		}
		if !slices.ContainsFunc(res.Manifest.Entries, func(e export.Entry) bool { return e.Path == "src/change.txt" }) {
			t.Fatalf("manifest = %+v, want src/change.txt", res.Manifest.Entries)
		}
		if !res.EvidencePresent || !res.CommitPlanPresent {
			t.Fatalf("evidence %v, commit plan %v; want both", res.EvidencePresent, res.CommitPlanPresent)
		}
		if got, want := evidenceLabels(res.Evidence), []string{"agent-transcript", export.SummaryEvidenceLabel}; !slices.Equal(got, want) {
			t.Fatalf("evidence labels = %q, want %q", got, want)
		}
		for _, name := range []string{export.ManifestFilename, export.EvidenceFilename, export.CommitPlanFilename} {
			if _, err := os.Stat(filepath.Join(res.Dir, name)); err != nil {
				t.Errorf("export lacks %s: %v", name, err)
			}
		}
		// The transcript evidence is the agent's stdout and nothing else.
		transcript, err := os.ReadFile(filepath.Join(l.worktree, transcriptSource().Path))
		if err != nil || string(transcript) != "{\"type\":\"result\"}\n" {
			t.Fatalf("transcript evidence = %q, %v", transcript, err)
		}
	})
}

// TestExportRefusesALaunchWithoutACleanProvenEnd covers one row per end
// state Export refuses; each writes nothing.
func TestExportRefusesALaunchWithoutACleanProvenEnd(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		launch func(t *testing.T, cfg Config) *Backend
		want   error
		// canceled is whether the refusal also says the run ended canceled.
		canceled bool
	}{
		{"blocked", func(t *testing.T, cfg Config) *Backend {
			host := newFakeHost()
			host.killErr[201] = syscall.EPERM
			b := fakeBackend(t, cfg, &fakeLauncher{host: host, script: func(_ processSpec, p *fakeProcess) { p.exit(0) }})
			var blocked *BlockedError
			if _, err := b.Launch(t.Context(), request("run")); !errors.As(err, &blocked) {
				t.Fatalf("Launch = %v, want blocked", err)
			}
			return b
		}, ErrNoEndRecord, false},
		{"canceled", func(t *testing.T, cfg Config) *Backend {
			ctx, cancel := context.WithCancel(t.Context())
			b := fakeBackend(t, cfg, &fakeLauncher{host: newFakeHost(), script: func(processSpec, *fakeProcess) { cancel() }})
			if got, err := b.Launch(ctx, request("run")); err != nil || got.EndedBy != EndedByCanceled {
				t.Fatalf("Launch = %+v, %v", got, err)
			}
			return b
		}, ErrNotExportable, true},
		// A stop that raced the agent's clean exit: Launch returned exited 0,
		// and the marker landed before Export.
		{"canceled after a clean exit", func(t *testing.T, cfg Config) *Backend {
			b := launched(t, cfg, "run", agentWriting(0, func(string) {}))
			if err := b.RequestCancellation(t.Context(), "run"); err != nil {
				t.Fatalf("RequestCancellation: %v", err)
			}
			return b
		}, ErrNotExportable, true},
		{"stalled", func(t *testing.T, cfg Config) *Backend {
			b := fakeBackend(t, cfg, &fakeLauncher{host: newFakeHost(), obeyTerm: true, script: func(processSpec, *fakeProcess) {}})
			if got, err := b.Launch(t.Context(), request("run")); err != nil || got.EndedBy != EndedByStalled {
				t.Fatalf("Launch = %+v, %v", got, err)
			}
			return b
		}, ErrNotExportable, false},
		{"nonzero exit", func(t *testing.T, cfg Config) *Backend {
			return launched(t, cfg, "run", agentWriting(1, func(string) {}))
		}, ErrNotExportable, false},
		// A clean end whose request a forced update left in the old version.
		{"foreign request", func(t *testing.T, cfg Config) *Backend {
			b := launched(t, cfg, "run", agentWriting(0, func(string) {}))
			l := layoutFor(cfg.RunsRoot, "run")
			body, err := os.ReadFile(l.request)
			if err != nil {
				t.Fatal(err)
			}
			old := strings.Replace(string(body), requestVersion, "freeside.cordon.supervise/v1", 1)
			//nolint:gosec // G703: the test run's own request file under its temporary runs root.
			if err := os.WriteFile(l.request, []byte(old), 0o600); err != nil {
				t.Fatal(err)
			}
			return b
		}, ErrNotExportable, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := testConfig(t)
			synctest.Test(t, func(t *testing.T) {
				b := tc.launch(t, cfg)
				_, err := b.Export(t.Context(), exportRequest("run"))
				if !errors.Is(err, tc.want) || errors.Is(err, ErrCanceled) != tc.canceled {
					t.Fatalf("Export = %v, want %v, canceled %v", err, tc.want, tc.canceled)
				}
				l := layoutFor(cfg.RunsRoot, "run")
				for _, p := range []string{l.export, l.exportRecord, filepath.Join(l.worktree, export.EvidenceDescriptorPath)} {
					if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
						t.Errorf("%s exists after a refused export: %v", p, err)
					}
				}
			})
		})
	}
	// A request that fails to read isn't a refusal: Recover's caller
	// retries it, and the retry exports.
	t.Run("unreadable request", func(t *testing.T) {
		t.Parallel()
		cfg := testConfig(t)
		synctest.Test(t, func(t *testing.T) {
			b := launched(t, cfg, "run", agentWriting(0, func(string) {}))
			l := layoutFor(cfg.RunsRoot, "run")
			body, err := os.ReadFile(l.request)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(l.request); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(l.request, 0o700); err != nil {
				t.Fatal(err)
			}
			if _, err := b.Export(t.Context(), exportRequest("run")); err == nil || errors.Is(err, ErrNotExportable) {
				t.Fatalf("Export over an unreadable request = %v, want a retryable error", err)
			}
			if err := os.Remove(l.request); err != nil {
				t.Fatal(err)
			}
			//nolint:gosec // G703: the test run's own request file under its temporary runs root.
			if err := os.WriteFile(l.request, body, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := b.Export(t.Context(), exportRequest("run")); err != nil {
				t.Fatalf("Export once the request reads: %v", err)
			}
		})
	})
	t.Run("already exported", func(t *testing.T) {
		t.Parallel()
		cfg := testConfig(t)
		synctest.Test(t, func(t *testing.T) {
			b := launched(t, cfg, "run", agentWriting(0, func(string) {}))
			if _, err := b.Export(t.Context(), exportRequest("run")); err != nil {
				t.Fatalf("first Export: %v", err)
			}
			l := layoutFor(cfg.RunsRoot, "run")
			before, err := os.ReadFile(l.exportRecord)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := b.Export(t.Context(), exportRequest("run")); !errors.Is(err, ErrAlreadyExported) {
				t.Fatalf("second Export = %v, want ErrAlreadyExported", err)
			}
			if after, err := os.ReadFile(l.exportRecord); err != nil || !bytes.Equal(before, after) {
				t.Fatalf("the export record changed: %v", err)
			}
		})
	})
}

// TestExportRefusesAReplacedWorktree has the agent leave run/worktree, or
// run/ itself, a symlink to a directory outside the run that holds a file
// the agent could not read. The export must neither read nor write it.
func TestExportRefusesAReplacedWorktree(t *testing.T) {
	t.Parallel()
	for name, replaced := range map[string]func(worktree string) string{
		"worktree": func(worktree string) string { return worktree },
		"run":      filepath.Dir,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := testConfig(t)
			outside := t.TempDir()
			writeIn(t, outside, "secret.txt", "owner data")
			synctest.Test(t, func(t *testing.T) {
				b := launched(t, cfg, "run", agentWriting(0, func(worktree string) {
					p := replaced(worktree)
					if err := os.Rename(p, p+".moved"); err != nil {
						t.Error(err)
					}
					if err := os.Symlink(outside, p); err != nil {
						t.Error(err)
					}
				}))
				if _, err := b.Export(t.Context(), exportRequest("run")); !errors.Is(err, ErrWorktreeReplaced) {
					t.Fatalf("Export = %v, want ErrWorktreeReplaced", err)
				}
				l := layoutFor(cfg.RunsRoot, "run")
				for _, p := range []string{l.export, l.exportRecord, filepath.Join(outside, export.EvidenceWorkspaceDir)} {
					if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
						t.Errorf("%s exists after a refused export: %v", p, err)
					}
				}
			})
		})
	}
}

// TestExportRunsNothingFromTheWorktree plants a git hook, a filter driver,
// and an fsmonitor, each of which would touch a sentinel if anything ran
// git or a script in the worktree.
func TestExportRunsNothingFromTheWorktree(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	sentinel := filepath.Join(t.TempDir(), "ran")
	script := "#!/bin/sh\ntouch " + sentinel + "\n"
	synctest.Test(t, func(t *testing.T) {
		b := launched(t, cfg, "run-hostile", agentWriting(0, func(wt string) {
			writeIn(t, wt, ".git/hooks/post-checkout", script)
			writeIn(t, wt, ".git/config", "[core]\n\tfsmonitor = "+filepath.Join(wt, "monitor.sh")+
				"\n[filter \"run\"]\n\tclean = "+filepath.Join(wt, "monitor.sh")+"\n\tsmudge = "+filepath.Join(wt, "monitor.sh")+"\n")
			writeIn(t, wt, ".gitattributes", "* filter=run\n")
			writeIn(t, wt, "monitor.sh", script)
			for _, p := range []string{".git/hooks/post-checkout", "monitor.sh"} {
				if err := os.Chmod(filepath.Join(wt, p), 0o700); err != nil { //nolint:gosec // G302: the planted hook and monitor must be executable
					t.Error(err)
				}
			}
		}))
		if _, err := b.Export(t.Context(), exportRequest("run-hostile")); err != nil {
			t.Fatalf("Export: %v", err)
		}
		if _, err := os.Stat(sentinel); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("something in the worktree ran: %v", err)
		}
	})
}

// TestExportWritesItsOwnEvidenceDescriptor shows an agent can't choose the
// evidence: its descriptor is replaced, a symlinked evidence directory is
// replaced without being written through, and a declared source the agent
// left as a symlink isn't declared.
func TestExportWritesItsOwnEvidenceDescriptor(t *testing.T) {
	t.Parallel()
	cases := map[string]func(t *testing.T, wt, outside string){
		"agent descriptor": func(t *testing.T, wt, _ string) {
			writeIn(t, wt, export.EvidenceDescriptorPath, `{"version":"`+export.EvidenceSourceVersion+`","sources":[{"label":"forged",`+
				`"media_type":"text/plain","path":".freeside-evidence/forged.txt","head_binding":"head_independent",`+
				`"sensitivity_class":"normal","producer_invocation_id":"inv-1"}]}`)
			writeIn(t, wt, ".freeside-evidence/forged.txt", "forged\n")
		},
		"symlinked directory": func(t *testing.T, wt, outside string) {
			if err := os.RemoveAll(filepath.Join(wt, export.EvidenceWorkspaceDir)); err != nil {
				t.Error(err)
			}
			if err := os.Symlink(outside, filepath.Join(wt, export.EvidenceWorkspaceDir)); err != nil {
				t.Error(err)
			}
		},
		"symlinked source": func(t *testing.T, wt, outside string) {
			writeIn(t, outside, "summary.md", "# Not the agent's\n")
			if err := os.Symlink(filepath.Join(outside, "summary.md"), filepath.Join(wt, export.SummaryEvidencePath)); err != nil {
				t.Error(err)
			}
		},
	}
	for name, arrange := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := testConfig(t)
			outside := t.TempDir()
			synctest.Test(t, func(t *testing.T) {
				b := launched(t, cfg, "run", agentWriting(0, func(wt string) { arrange(t, wt, outside) }))
				before, err := os.ReadDir(outside)
				if err != nil {
					t.Fatal(err)
				}
				res, err := b.Export(t.Context(), exportRequest("run"))
				if err != nil {
					t.Fatalf("Export: %v", err)
				}
				if got := evidenceLabels(res.Evidence); !slices.Equal(got, []string{"agent-transcript"}) {
					t.Fatalf("evidence labels = %q, want the transcript alone", got)
				}
				after, err := os.ReadDir(outside)
				if err != nil || len(after) != len(before) {
					t.Fatalf("Export wrote outside the worktree: %d entries became %d, %v", len(before), len(after), err)
				}
			})
		})
	}
}

func TestExportRefusesAHardLinkedFile(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	const secret = "a file the account can read\n" //nolint:gosec // G101: a stand-in for a host file, not a credential
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	synctest.Test(t, func(t *testing.T) {
		b := launched(t, cfg, "run-link", agentWriting(0, func(wt string) {
			if err := os.Link(outside, filepath.Join(wt, "linked")); err != nil {
				t.Error(err)
			}
		}))
		if _, err := b.Export(t.Context(), exportRequest("run-link")); !errors.Is(err, export.ErrLinkedFile) {
			t.Fatalf("Export = %v, want ErrLinkedFile", err)
		}
		l := layoutFor(cfg.RunsRoot, "run-link")
		filesUnder(t, l.export, func(p string, body []byte) {
			if string(body) == secret {
				t.Errorf("the linked file's bytes reached %s", p)
			}
		})
		if _, err := os.Lstat(l.exportRecord); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("a refused export has a record: %v", err)
		}
	})
}

func TestExportRefusesWhatTheCredentialScanMatches(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	key := "-----BEGIN " + "OPENSSH PRIVATE KEY-----\nabc\n"
	synctest.Test(t, func(t *testing.T) {
		b := launched(t, cfg, "run-scan", agentWriting(0, func(wt string) { writeIn(t, wt, "id_ed25519", key) }))
		_, err := b.Export(t.Context(), exportRequest("run-scan"))
		var refusal *ScanRefusalError
		if !errors.As(err, &refusal) || !errors.Is(err, ward.ErrOutputScanRefused) {
			t.Fatalf("Export = %v, want a ScanRefusalError", err)
		}
		if want := `worktree file "id_ed25519" matched credential rule pem_private_key`; !strings.Contains(refusal.Reason, want) ||
			strings.Contains(refusal.Reason, "BEGIN") {
			t.Fatalf("reason = %q, want it to name %q and not the key", refusal.Reason, want)
		}
		if _, err := os.Lstat(layoutFor(cfg.RunsRoot, "run-scan").exportRecord); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("a refused export has a record: %v", err)
		}
	})
}

func TestAuthenticateReleasedExport(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	synctest.Test(t, func(t *testing.T) {
		script := agentWriting(0, func(wt string) { writeIn(t, wt, export.CommitPlanFilename, "{}") })
		b := launched(t, cfg, "run-auth", script)
		res, err := b.Export(t.Context(), exportRequest("run-auth"))
		if err != nil {
			t.Fatalf("Export: %v", err)
		}
		if err := b.AuthenticateReleasedExport(t.Context(), "run-auth", res.Dir); err != nil {
			t.Fatalf("AuthenticateReleasedExport of the released export: %v", err)
		}
		// Another launch with no export, and a directory not the run's.
		if _, err := b.Launch(t.Context(), request("run-other")); err != nil {
			t.Fatal(err)
		}
		other := layoutFor(cfg.RunsRoot, "run-other").export
		if err := os.MkdirAll(other, 0o700); err != nil {
			t.Fatal(err)
		}
		refusals := map[string]struct{ runID, dir string }{
			"another directory":  {"run-auth", other},
			"no export record":   {"run-other", other},
			"a malformed run id": {"../run-auth", res.Dir},
			"an unclean path":    {"run-auth", res.Dir + "/"},
		}
		for name, r := range refusals {
			if err := b.AuthenticateReleasedExport(t.Context(), r.runID, r.dir); !errors.Is(err, ErrExportUnauthenticated) {
				t.Errorf("%s: AuthenticateReleasedExport = %v, want ErrExportUnauthenticated", name, err)
			}
		}
		for _, name := range []string{export.ManifestFilename, export.EvidenceFilename, export.CommitPlanFilename} {
			p := filepath.Join(res.Dir, name)
			body, err := os.ReadFile(p) //nolint:gosec // G304: a member of the test's own export
			if err != nil {
				t.Fatal(err)
			}
			changed := slices.Clone(body)
			changed[len(changed)-1] ^= 1
			if err := os.WriteFile(p, changed, 0o600); err != nil { //nolint:gosec // G703: a member of the test's own export
				t.Fatal(err)
			}
			if err := b.AuthenticateReleasedExport(t.Context(), "run-auth", res.Dir); !errors.Is(err, ErrExportUnauthenticated) {
				t.Errorf("%s changed: AuthenticateReleasedExport = %v, want ErrExportUnauthenticated", name, err)
			}
			if err := os.WriteFile(p, body, 0o600); err != nil { //nolint:gosec // G703: a member of the test's own export
				t.Fatal(err)
			}
		}
	})
}

// gitSeeder seeds a worktree by cloning a local base repository at its
// base commit, as the daemon's transport seeds a real one.
type gitSeeder struct{ base string }

func (s gitSeeder) FetchBaseWorktree(_ context.Context, _, _, baseSHA, dir string) error {
	if _, err := runGit("", "clone", "-q", "--no-checkout", s.base, dir); err != nil {
		return err
	}
	_, err := runGit(dir, "checkout", "-q", "--detach", baseSHA)
	return err
}

func runGit(dir string, args ...string) (string, error) {
	if dir != "" {
		args = append([]string{"-C", dir}, args...)
	}
	cmd := exec.Command("git", args...) //nolint:gosec // G204: test fixture running git over test-owned repositories
	cmd.Env = append(gitenv.Scrubbed(),
		"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_SYSTEM="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=fixture", "GIT_AUTHOR_EMAIL=fixture@test.invalid", "GIT_AUTHOR_DATE=1700000000 +0000",
		"GIT_COMMITTER_NAME=fixture", "GIT_COMMITTER_EMAIL=fixture@test.invalid", "GIT_COMMITTER_DATE=1700000000 +0000",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %v: %w\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := runGit(dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestACordonedExportImports hands a cordoned export to importer.Import, the
// entry point the stage driver calls. The importer's own gates are its
// tests'.
func TestACordonedExportImports(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	base := t.TempDir()
	mustGit(t, base, "init", "-q", "-b", "main")
	writeIn(t, base, "README.md", "before\n")
	mustGit(t, base, "add", "README.md")
	mustGit(t, base, "commit", "-q", "-m", "base")
	baseSHA := mustGit(t, base, "rev-parse", "HEAD")
	checkout := filepath.Join(t.TempDir(), "checkout")
	mustGit(t, "", "clone", "-q", base, checkout)

	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{host: newFakeHost(), script: agentWriting(0, func(wt string) { writeIn(t, wt, "README.md", "after\n") })}
		b, err := New(cfg, gitSeeder{base: base})
		if err != nil {
			t.Fatal(err)
		}
		useFakes(b, l)
		req := request("run-import")
		req.BaseSHA = baseSHA
		if _, err := b.Launch(t.Context(), req); err != nil {
			t.Fatalf("Launch: %v", err)
		}
		res, err := b.Export(t.Context(), exportRequest("run-import"))
		if err != nil {
			t.Fatalf("Export: %v", err)
		}
		got, err := importer.Import(t.Context(), res.Dir, checkout, importer.Options{
			BaseSHA: res.BaseSHA, CommitDate: time.Unix(1_700_000_000, 0),
		})
		if err != nil {
			t.Fatalf("Import: %v", err)
		}
		if got.CommitSHA == "" || len(got.Changes) != 1 || got.Changes[0].Path != "README.md" {
			t.Fatalf("Import = commit %q changes %+v, want one commit changing README.md", got.CommitSHA, got.Changes)
		}
	})
}

// filesUnder calls visit with every regular file under dir; a missing dir
// visits nothing.
func filesUnder(t *testing.T, dir string, visit func(p string, body []byte)) {
	t.Helper()
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, err := os.ReadFile(p) //nolint:gosec // G304: the test's own export directory
		if err != nil {
			return err
		}
		visit(p, body)
		return nil
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}
