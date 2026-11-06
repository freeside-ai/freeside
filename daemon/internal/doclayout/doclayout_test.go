package doclayout

import (
	"errors"
	"fmt"
	"go/doc/comment"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// requiredLayouts names the packages, relative to daemon/internal, that must
// carry a layout section. Any other package is checked only when it opts in
// by writing one, so this list is what stops a section from being deleted
// rather than corrected.
var requiredLayouts = []string{
	"domain",
	"engine",
	"exec",
	"exec/fake",
	"exec/fake/forge",
	"export",
	"importer",
	"verify",
	"ward",
}

// TestLayoutSectionsMatchFiles checks every daemon doc.go layout section
// against the files in its directory. It reads the directory rather than a
// build context so build-tagged files (_darwin.go, _linux.go) count on every
// platform.
func TestLayoutSectionsMatchFiles(t *testing.T) {
	// go test runs a package with its directory as the working directory;
	// under -trimpath, runtime.Caller returns import paths, not file paths.
	packageDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	internalDir := filepath.Dir(packageDir)
	daemonRoot := filepath.Dir(internalDir)

	checked := map[string]bool{}
	err = filepath.WalkDir(daemonRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			// The go tool ignores these directories, so their files are not
			// part of any package.
			name := entry.Name()
			if path != daemonRoot && (name == "testdata" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")) {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Name() != "doc.go" {
			return nil
		}
		dir := filepath.Dir(path)
		name, err := filepath.Rel(internalDir, dir)
		if err != nil || strings.HasPrefix(name, "..") {
			name, err = filepath.Rel(daemonRoot, dir)
			if err != nil {
				return err
			}
		}
		name = filepath.ToSlash(name)

		text, err := packageDocText(path)
		if err != nil {
			return err
		}
		listed, found, err := layoutFiles(text)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			return nil
		}
		if !found {
			return nil
		}
		checked[name] = true
		actual, err := packageFiles(dir)
		if err != nil {
			return err
		}
		missing, stale := compareLayout(listed, actual)
		for _, file := range missing {
			t.Errorf("%s: %s is not in the doc.go layout section", name, file)
		}
		for _, file := range stale {
			t.Errorf("%s: the doc.go layout section lists %s, which is not in the package", name, file)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", daemonRoot, err)
	}
	for _, name := range requiredLayouts {
		if !checked[name] {
			t.Errorf("%s: doc.go has no layout section (a paragraph starting with Layout followed by a list); this package is required to keep one", name)
		}
	}
}

func packageDocText(path string) (string, error) {
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.PackageClauseOnly|parser.ParseComments)
	if err != nil {
		return "", err
	}
	if file.Doc == nil {
		return "", nil
	}
	return file.Doc.Text(), nil
}

// packageFiles returns the directory's non-test .go files other than doc.go,
// sorted.
func packageFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || filepath.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") || name == "doc.go" {
			continue
		}
		files = append(files, name)
	}
	return files, nil
}

// layoutFiles finds the layout section in a package comment: a paragraph
// whose text starts with "Layout", followed directly by a list. It returns
// the .go file names that lead the list's items. found is false when the
// comment has no such section. An item that names no file, or a second
// layout section, is an error, so a malformed entry cannot hide a file.
func layoutFiles(docText string) (files []string, found bool, err error) {
	var p comment.Parser
	blocks := p.Parse(docText).Content
	for i, block := range blocks {
		para, ok := block.(*comment.Paragraph)
		if !ok || i+1 >= len(blocks) || !strings.HasPrefix(plainText(para.Text), "Layout") {
			continue
		}
		list, ok := blocks[i+1].(*comment.List)
		if !ok {
			continue
		}
		if found {
			return nil, false, errors.New("doc.go has more than one layout section")
		}
		found = true
		for _, item := range list.Items {
			names, err := itemFiles(item)
			if err != nil {
				return nil, false, err
			}
			files = append(files, names...)
		}
	}
	return files, found, nil
}

// itemFiles returns the leading run of comma-separated .go file names in a
// list item, such as "a.go, b.go, c.go the shared concept".
func itemFiles(item *comment.ListItem) ([]string, error) {
	var text string
	if len(item.Content) > 0 {
		if para, ok := item.Content[0].(*comment.Paragraph); ok {
			text = plainText(para.Text)
		}
	}
	var files []string
	for _, field := range strings.Fields(text) {
		name, more := strings.CutSuffix(field, ",")
		if !strings.HasSuffix(name, ".go") {
			break
		}
		files = append(files, name)
		if !more {
			break
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("layout item %q does not start with a .go file name", firstLine(text))
	}
	return files, nil
}

func plainText(texts []comment.Text) string {
	var b strings.Builder
	for _, t := range texts {
		switch t := t.(type) {
		case comment.Plain:
			b.WriteString(string(t))
		case comment.Italic:
			b.WriteString(string(t))
		case *comment.Link:
			b.WriteString(plainText(t.Text))
		case *comment.DocLink:
			b.WriteString(plainText(t.Text))
		}
	}
	return b.String()
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// compareLayout returns the actual files the layout does not list and the
// listed files that do not exist, each sorted.
func compareLayout(listed, actual []string) (missing, stale []string) {
	for _, file := range actual {
		if !slices.Contains(listed, file) {
			missing = append(missing, file)
		}
	}
	for _, file := range listed {
		if !slices.Contains(actual, file) && !slices.Contains(stale, file) {
			stale = append(stale, file)
		}
	}
	slices.Sort(missing)
	slices.Sort(stale)
	return missing, stale
}

func TestCompareLayout(t *testing.T) {
	tests := []struct {
		name        string
		listed      []string
		actual      []string
		wantMissing []string
		wantStale   []string
	}{
		{
			name:   "match",
			listed: []string{"b.go", "a.go"},
			actual: []string{"a.go", "b.go"},
		},
		{
			name:        "missing file",
			listed:      []string{"a.go"},
			actual:      []string{"a.go", "new.go"},
			wantMissing: []string{"new.go"},
		},
		{
			name:      "stale entry",
			listed:    []string{"a.go", "gone.go"},
			actual:    []string{"a.go"},
			wantStale: []string{"gone.go"},
		},
		{
			name:        "both",
			listed:      []string{"gone.go", "a.go"},
			actual:      []string{"z.go", "a.go"},
			wantMissing: []string{"z.go"},
			wantStale:   []string{"gone.go"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			missing, stale := compareLayout(tt.listed, tt.actual)
			if !slices.Equal(missing, tt.wantMissing) || !slices.Equal(stale, tt.wantStale) {
				t.Fatalf("compareLayout() = missing %v, stale %v; want missing %v, stale %v",
					missing, stale, tt.wantMissing, tt.wantStale)
			}
		})
	}
}

func TestLayoutFiles(t *testing.T) {
	tests := []struct {
		name      string
		doc       string
		wantFiles []string
		wantFound bool
		wantErr   string
	}{
		{
			name: "multi-file item and continuation lines",
			doc: `Package p does things.

Layout, by concept:

  - enums.go        named-string enumerations
  - attention_item.go, attention_delivery.go, attention_conclusion.go
    the attention model and how a closed item concluded
  - artifact.go     artifacts, provenance,
    and the evidence gate

See docs/plan.md.
`,
			wantFiles: []string{"enums.go", "attention_item.go", "attention_delivery.go", "attention_conclusion.go", "artifact.go"},
			wantFound: true,
		},
		{
			name: "bare Layout heading",
			doc: `Layout:

  - forge.go      Forge
  - transport.go  Transport
`,
			wantFiles: []string{"forge.go", "transport.go"},
			wantFound: true,
		},
		{
			name: "list without a layout heading is not a section",
			doc: `Three files are exceptions:

  - review_instruction.go: provider code
`,
		},
		{
			name: "layout paragraph without a list is not a section",
			doc:  "Layout follows the plan.\n",
		},
		{
			name: "item without a file name",
			doc: `Layout:

  - the shared helpers
`,
			wantErr: `layout item "the shared helpers" does not start with a .go file name`,
		},
		{
			name: "second layout section",
			doc: `Layout:

  - a.go  one

Layout, again:

  - b.go  two
`,
			wantErr: "doc.go has more than one layout section",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files, found, err := layoutFiles(tt.doc)
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("layoutFiles() error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("layoutFiles() error = %v", err)
			}
			if found != tt.wantFound || !slices.Equal(files, tt.wantFiles) {
				t.Fatalf("layoutFiles() = %v, found %v; want %v, found %v", files, found, tt.wantFiles, tt.wantFound)
			}
		})
	}
}
