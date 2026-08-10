package publish

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// TestOnlyGitNetImportsExec pins the process-boundary discipline the
// importer established, for the publish lane: no publish package file
// may execute a subprocess except the single hardened transport
// runner. A new os/exec import anywhere else is the escape this test
// exists to catch.
func TestOnlyGitNetImportsExec(t *testing.T) {
	t.Parallel()
	sources, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, name := range sources {
		if strings.HasSuffix(name, "_test.go") {
			continue // test helpers legitimately run git to build fixtures
		}
		file, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range file.Imports {
			if imp.Path.Value == `"os/exec"` && filepath.Base(name) != "gitnet.go" {
				t.Errorf("%s imports os/exec; only gitnet.go may", name)
			}
		}
	}
}

// TestOnlyApprovedStreamingReaders keeps publish's structural JSON walker,
// unmarshal seams, and whole-reader paths attributable. Typed single-value
// decoders are centralized and ratcheted by internal/strictjson.
func TestOnlyApprovedStreamingReaders(t *testing.T) {
	t.Parallel()
	approved := map[string]map[string]int{
		"encoding/json.NewDecoder": {
			"janitor_snapshot.go.rejectDuplicateJSONKeys": 1,
		},
		"encoding/json.Unmarshal": {
			"janitor_snapshot.go.jsonArray":        1,
			"janitor_snapshot.go.jsonObject":       1,
			"keystore.go.loadAppFrom":              1,
			"keystore.go.loadLegacyAppFrom":        1,
			"store_intent.go.commitReservedIntent": 1,
		},
		"io.ReadAll": {
			"janitor_store.go.readFile":      1,
			"onboarding.go.ImportKey":        1,
			"transport.go.readRepoIDBinding": 1,
		},
	}
	observed := make(map[string]map[string]int)
	var sources []string
	for _, pattern := range []string{"*.go", "../publicationrecord/*.go"} {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatal(err)
		}
		sources = append(sources, matches...)
	}
	fset := token.NewFileSet()
	for _, name := range sources {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		imports := make(map[string]string)
		for _, spec := range file.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			importName := filepath.Base(path)
			if spec.Name != nil {
				importName = spec.Name.Name
			}
			if importName == "." && (path == "encoding/json" || path == "io") {
				t.Errorf("%s dot-imports %s; guarded calls must remain attributable", filepath.Base(name), path)
			}
			imports[importName] = path
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			location := filepath.Base(name) + "." + function.Name.Name
			if filepath.Base(filepath.Dir(name)) == "publicationrecord" {
				location = "publicationrecord/" + location
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				selector, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				packageName, ok := selector.X.(*ast.Ident)
				if !ok {
					return true
				}
				callName := imports[packageName.Name] + "." + selector.Sel.Name
				allowedLocations, guarded := approved[callName]
				if !guarded {
					return true
				}
				if _, allowed := allowedLocations[location]; !allowed {
					t.Errorf("%s calls %s outside the reviewed allowlist", fset.Position(call.Pos()), callName)
					return true
				}
				if observed[callName] == nil {
					observed[callName] = make(map[string]int)
				}
				observed[callName][location]++
				return true
			})
		}
	}
	for callName, allowedLocations := range approved {
		for location, want := range allowedLocations {
			if got := observed[callName][location]; got != want {
				t.Errorf("%s call count in %s = %d, want %d", callName, location, got, want)
			}
		}
	}
}

func TestFinalizeRejectsSubstitutedIntentCoordinates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "store.db"), store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	})

	authorizationID := domain.Digest("sha256:" + strings.Repeat("a", 64))
	candidate := Candidate{
		Repo:            "freeside-ai/repo",
		BaseRef:         "main",
		HeadSHA:         strings.Repeat("b", 40),
		Artifacts:       []domain.Artifact{{Digest: "sha256:artifact"}},
		InvocationID:    "publication-1",
		AuthorizationID: &authorizationID,
	}
	identity, err := deriveCandidateIdentity(candidate)
	if err != nil {
		t.Fatalf("derive identity: %v", err)
	}
	substituted := Intent{
		Identity:        identity.Digest(),
		InvocationID:    candidate.InvocationID,
		Repo:            "freeside-ai/foreign",
		BaseRef:         "release",
		SourceHeadSHA:   strings.Repeat("c", 40),
		AuthorizationID: "sha256:" + domain.Digest(strings.Repeat("d", 64)),
	}
	payload, err := substituted.Encode()
	if err != nil {
		t.Fatalf("encode substituted intent: %v", err)
	}
	key, err := IntentKey(candidate.InvocationID, IntentKindPublication)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, _, err := tx.EnqueueOutbox(ctx, key, IntentKindPublication, payload)
		return err
	}); err != nil {
		t.Fatalf("enqueue substituted intent: %v", err)
	}

	err = finalizePublicationResult(ctx, s, candidate, Result{
		Identity: identity, Branch: identity.BranchName(), PRNumber: 101,
	}, "")
	if !errors.Is(err, errPublicationIntentDiverged) {
		t.Fatalf("finalize substituted intent error = %v", err)
	}
	var pending []store.QueueEntry
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		pending, err = tx.ListPendingOutbox(ctx, IntentKindPublication)
		return err
	}); err != nil {
		t.Fatalf("list pending intents: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending intents after rejected finalization = %d, want 1", len(pending))
	}
}
