//go:build unix

package export_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/export"
)

// TestExportRefuseLinkedFiles shows the option refuses a hard-linked file
// wherever the export would read it, before any of its bytes reach the
// output, and that without the option the same workspaces export.
func TestExportRefuseLinkedFiles(t *testing.T) {
	const secret = "content only the link reaches\n"
	descriptor := `{"version":"` + export.EvidenceSourceVersion + `","sources":[{"label":"log","media_type":"text/plain",` +
		`"path":".freeside-evidence/log.txt","head_binding":"head_independent","sensitivity_class":"normal",` +
		`"producer_invocation_id":"inv-1"}]}`
	cases := map[string]func(t *testing.T, ws, outside string){
		"workspace file": func(t *testing.T, ws, outside string) {
			link(t, outside, filepath.Join(ws, "src", "copy.txt"))
		},
		"evidence descriptor": func(t *testing.T, ws, outside string) {
			if err := os.WriteFile(outside, []byte(descriptor), 0o600); err != nil {
				t.Fatal(err)
			}
			link(t, outside, filepath.Join(ws, export.EvidenceDescriptorPath))
			writeFile(t, filepath.Join(ws, ".freeside-evidence", "log.txt"), "log\n")
		},
		"evidence source": func(t *testing.T, ws, outside string) {
			writeFile(t, filepath.Join(ws, export.EvidenceDescriptorPath), descriptor)
			link(t, outside, filepath.Join(ws, ".freeside-evidence", "log.txt"))
		},
		"commit plan": func(t *testing.T, ws, outside string) {
			link(t, outside, filepath.Join(ws, export.CommitPlanFilename))
		},
	}
	for name, arrange := range cases {
		t.Run(name, func(t *testing.T) {
			ws, outside := t.TempDir(), filepath.Join(t.TempDir(), "secret.txt")
			writeFile(t, outside, secret)
			writeFile(t, filepath.Join(ws, "src", "main.txt"), "main\n")
			arrange(t, ws, outside)
			body, err := os.ReadFile(outside) //nolint:gosec // G304: the test's own file under TempDir
			if err != nil {
				t.Fatal(err)
			}

			out := filepath.Join(t.TempDir(), "out")
			opts := export.DefaultOptions()
			opts.RefuseLinkedFiles = true
			if _, err := export.Export(os.DirFS(ws), out, opts); !errors.Is(err, export.ErrLinkedFile) {
				t.Fatalf("Export = %v, want ErrLinkedFile", err)
			}
			if found := filesHolding(t, out, string(body)); len(found) != 0 {
				t.Fatalf("the linked file's bytes reached %v", found)
			}
			if _, err := export.Export(os.DirFS(ws), filepath.Join(t.TempDir(), "out"), export.DefaultOptions()); err != nil {
				t.Fatalf("Export without the option: %v", err)
			}
		})
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func link(t *testing.T, target, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(target, path); err != nil {
		t.Fatal(err)
	}
}

// filesHolding lists the files under dir whose content is body.
func filesHolding(t *testing.T, dir, body string) []string {
	t.Helper()
	var found []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		got, err := os.ReadFile(p) //nolint:gosec // G304: a file of the test's own export
		if err == nil && string(got) == body {
			found = append(found, p)
		}
		return err
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return found
}
