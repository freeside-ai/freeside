package inference_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
)

// declaredSiteIDs reads every string constant named *SiteID from this
// package's non-test source, so a site added later is checked without anyone
// remembering to list it here.
func declaredSiteIDs(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var ids []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				value := spec.(*ast.ValueSpec)
				for i, name := range value.Names {
					if !strings.HasSuffix(name.Name, "SiteID") || i >= len(value.Values) {
						continue
					}
					lit, ok := value.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						t.Fatalf("site id constant %s is not a string literal", name.Name)
					}
					id, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatal(err)
					}
					ids = append(ids, id)
				}
			}
		}
	}
	slices.Sort(ids)
	return ids
}

// TestEverySiteBelongsToExactlyOneRole holds domain's role-to-site map and
// this package's site ids together (plan §5.13: every site belongs to exactly
// one role). domain cannot import this package, so it names sites as strings.
func TestEverySiteBelongsToExactlyOneRole(t *testing.T) {
	t.Parallel()
	registered := []string{
		inference.AdjudicatorSiteID, inference.AttentionDiscussionSiteID,
		inference.ClassifierSiteID, inference.DiagnosticSiteID, inference.DriftAuditorSiteID,
		inference.PublicationAuthorExplainSiteID, inference.PublicationAuthorProposeSiteID,
		inference.TaskNamerSiteID,
	}
	slices.Sort(registered)
	if declared := declaredSiteIDs(t); !slices.Equal(declared, registered) {
		t.Fatalf("declared site ids %v, this test lists %v", declared, registered)
	}
	var named []string
	for _, role := range domain.AllRoleNames {
		for _, site := range role.Sites() {
			if role.LaunchShape() != domain.LaunchShapeWardless {
				t.Fatalf("ward role %s names site %q", role, site)
			}
			named = append(named, site)
		}
	}
	slices.Sort(named)
	// Equal sorted lists prove both directions at once: each registered site
	// has a role, each site a role names exists, and none appears twice.
	if !slices.Equal(named, registered) {
		t.Fatalf("roles name sites %v, registered sites are %v", named, registered)
	}
}
