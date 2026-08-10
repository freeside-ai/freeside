package domain

import (
	"go/ast"
	"go/build"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"maps"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"testing"
)

const (
	daemonImportPath = "github.com/freeside-ai/freeside/daemon"
	domainImportPath = daemonImportPath + "/internal/domain"
)

var enumRegistrationExemptions = map[string]string{
	"UnboundBackendConfigurationDigest": "Digest sentinel, not an enum member",
}

type enumRegistration struct {
	identifier string
	members    map[string]struct{}
}

type domainSourceImporter struct {
	buildContext build.Context
	fset         *token.FileSet
	packages     map[string]*types.Package
	standard     types.Importer
}

func unparenExpression(expression ast.Expr) ast.Expr {
	for {
		parenthesized, ok := expression.(*ast.ParenExpr)
		if !ok {
			return expression
		}
		expression = parenthesized.X
	}
}

func matchingGoFiles(buildContext build.Context, directory string) ([]string, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		matches, matchErr := buildContext.MatchFile(directory, entry.Name())
		if matchErr != nil {
			return nil, matchErr
		}
		if matches {
			names = append(names, entry.Name())
		}
	}
	return names, nil
}

func activeGoFiles(t *testing.T, buildContext build.Context, directory string) []string {
	t.Helper()

	names, err := matchingGoFiles(buildContext, directory)
	if err != nil {
		t.Fatalf("match active Go files in %s: %v", directory, err)
	}
	return names
}

func currentBuildContext() build.Context {
	buildContext := build.Default
	buildInfo, ok := debug.ReadBuildInfo()
	if !ok {
		return buildContext
	}
	for _, setting := range buildInfo.Settings {
		switch {
		case setting.Key == "-tags":
			buildContext.BuildTags = strings.Fields(strings.ReplaceAll(setting.Value, ",", " "))
		case setting.Key == "CGO_ENABLED":
			buildContext.CgoEnabled = setting.Value == "1"
		case setting.Value == "true" && slices.Contains([]string{"-race", "-msan", "-asan"}, setting.Key):
			buildContext.ToolTags = append(buildContext.ToolTags, strings.TrimPrefix(setting.Key, "-"))
		}
	}
	return buildContext
}

func newDomainSourceImporter(buildContext build.Context, fset *token.FileSet) *domainSourceImporter {
	return &domainSourceImporter{
		buildContext: buildContext,
		fset:         fset,
		packages:     map[string]*types.Package{},
		standard:     importer.Default(),
	}
}

func (i *domainSourceImporter) Import(importPath string) (*types.Package, error) {
	if pkg := i.packages[importPath]; pkg != nil {
		return pkg, nil
	}
	pkg, err := i.standard.Import(importPath)
	if err == nil {
		return pkg, nil
	}
	if !strings.HasPrefix(importPath, daemonImportPath+"/") {
		return nil, err
	}

	relativePath := strings.TrimPrefix(importPath, daemonImportPath+"/")
	directory := filepath.Join("..", "..", filepath.FromSlash(relativePath))
	names, err := matchingGoFiles(i.buildContext, directory)
	if err != nil {
		return nil, err
	}
	var files []*ast.File
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, parseErr := parser.ParseFile(i.fset, filepath.Join(directory, name), nil, 0)
		if parseErr != nil {
			return nil, parseErr
		}
		files = append(files, file)
	}
	configuration := types.Config{IgnoreFuncBodies: true, Importer: i}
	pkg, err = configuration.Check(importPath, i.fset, files, nil)
	if err != nil {
		return nil, err
	}
	i.packages[importPath] = pkg
	return pkg, nil
}

type enumDeclarations struct {
	declaredConsts     map[string]map[string]struct{}
	declaredExemptions map[string]bool
	importer           *domainSourceImporter
	sourcePackage      *types.Package
	stringTypes        map[string]struct{}
}

func namedStringSliceElement(variable *types.Var) (*types.Named, bool) {
	sliceType, ok := variable.Type().Underlying().(*types.Slice)
	if !ok {
		return nil, false
	}
	elementType, ok := types.Unalias(sliceType.Elem()).(*types.Named)
	if !ok {
		return nil, false
	}
	underlying, ok := elementType.Underlying().(*types.Basic)
	return elementType, ok && underlying.Kind() == types.String
}

func collectDeclaredEnumConsts(
	t *testing.T,
	buildContext build.Context,
	fset *token.FileSet,
	files []*ast.File,
) enumDeclarations {
	t.Helper()

	packageImporter := newDomainSourceImporter(buildContext, fset)
	typeInfo := &types.Info{Defs: map[*ast.Ident]types.Object{}}
	configuration := types.Config{
		IgnoreFuncBodies: true,
		Importer:         packageImporter,
	}
	checkedPackage, err := configuration.Check(domainImportPath, fset, files, typeInfo)
	if err != nil {
		t.Fatalf("type-check domain package: %v", err)
	}
	packageImporter.packages[domainImportPath] = checkedPackage

	stringTypes := map[string]struct{}{}
	for _, name := range checkedPackage.Scope().Names() {
		typeName, ok := checkedPackage.Scope().Lookup(name).(*types.TypeName)
		if !ok || typeName.IsAlias() {
			continue
		}
		namedType, ok := typeName.Type().(*types.Named)
		if !ok {
			continue
		}
		underlying, ok := namedType.Underlying().(*types.Basic)
		if ok && underlying.Kind() == types.String {
			stringTypes[name] = struct{}{}
		}
	}

	declaredConsts := map[string]map[string]struct{}{}
	declaredExemptions := map[string]bool{}
	for _, file := range files {
		for _, declaration := range file.Decls {
			general, ok := declaration.(*ast.GenDecl)
			if !ok || general.Tok != token.CONST {
				continue
			}
			for _, specification := range general.Specs {
				valueSpec := specification.(*ast.ValueSpec)
				for _, name := range valueSpec.Names {
					constant, ok := typeInfo.Defs[name].(*types.Const)
					if !ok {
						continue
					}
					namedType, ok := types.Unalias(constant.Type()).(*types.Named)
					if !ok || namedType.Obj().Pkg() != checkedPackage {
						continue
					}
					typeName := namedType.Obj().Name()
					if _, ok := stringTypes[typeName]; !ok {
						continue
					}
					if _, exempt := enumRegistrationExemptions[name.Name]; exempt {
						declaredExemptions[name.Name] = true
						continue
					}
					if declaredConsts[typeName] == nil {
						declaredConsts[typeName] = map[string]struct{}{}
					}
					declaredConsts[typeName][name.Name] = struct{}{}
				}
			}
		}
	}
	return enumDeclarations{
		declaredConsts:     declaredConsts,
		declaredExemptions: declaredExemptions,
		importer:           packageImporter,
		sourcePackage:      checkedPackage,
		stringTypes:        stringTypes,
	}
}

func TestEnumRegistrationEffectiveTypes(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "inferred.go", `package domain

type StateBase string
type State StateBase
type Wrapped (string)
type Alias = string
type StateAlias = State
type States []StateAlias

const (
	StateFirst State = "first"
	StateAliased StateAlias = "aliased"
	StateCopy = StateFirst
	StateRepeated
	WrappedFirst Wrapped = "wrapped"
)

var (
	AllStates = States{StateFirst}
	AllParenthesized = [](State){StateFirst}
)
`, 0)
	if err != nil {
		t.Fatalf("parse inferred const fixture: %v", err)
	}
	declarations := collectDeclaredEnumConsts(
		t,
		currentBuildContext(),
		fset,
		[]*ast.File{file},
	)
	for _, name := range []string{"StateBase", "State", "Wrapped"} {
		if _, ok := declarations.stringTypes[name]; !ok {
			t.Errorf("named string type %s was not resolved from its effective underlying type", name)
		}
	}
	if _, ok := declarations.stringTypes["Alias"]; ok {
		t.Error("string alias was treated as a named string type")
	}
	for _, name := range []string{"StateFirst", "StateAliased", "StateCopy", "StateRepeated"} {
		if _, ok := declarations.declaredConsts["State"][name]; !ok {
			t.Errorf("State const %s was not resolved from its effective type", name)
		}
	}
	if _, ok := declarations.declaredConsts["Wrapped"]["WrappedFirst"]; !ok {
		t.Error("Wrapped const was not resolved through its parenthesized underlying type")
	}
	for _, name := range []string{"AllStates", "AllParenthesized"} {
		registrationVariable, ok := declarations.sourcePackage.Scope().Lookup(name).(*types.Var)
		if !ok {
			t.Fatalf("%s was not resolved as a package variable", name)
		}
		elementType, ok := namedStringSliceElement(registrationVariable)
		if !ok || elementType.Obj().Name() != "State" {
			t.Errorf("%s element type = %v, want State", name, elementType)
		}
	}
}

func TestActiveGoFilesHonorsBuildConstraints(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	files := map[string]string{
		"active.go":   "package fixture\n",
		"inactive.go": "//go:build ignore\n\npackage fixture\n",
	}
	for name, contents := range files {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(contents), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	buildContext := build.Default
	buildContext.BuildTags = []string{"custom"}
	if err := os.WriteFile(
		filepath.Join(directory, "custom.go"),
		[]byte("//go:build custom\n\npackage fixture\n"),
		0o600,
	); err != nil {
		t.Fatalf("write custom.go: %v", err)
	}
	if names := activeGoFiles(t, buildContext, directory); !slices.Equal(names, []string{"active.go", "custom.go"}) {
		t.Errorf("active Go files = %v, want [active.go custom.go]", names)
	}
}

func TestUnparenExpression(t *testing.T) {
	t.Parallel()

	expression, err := parser.ParseExpr("([]State{(StateReady)})")
	if err != nil {
		t.Fatalf("parse parenthesized registration fixture: %v", err)
	}
	literal, ok := unparenExpression(expression).(*ast.CompositeLit)
	if !ok {
		t.Fatalf("unparenthesized value = %#v, want composite literal", unparenExpression(expression))
	}
	member, ok := unparenExpression(literal.Elts[0]).(*ast.Ident)
	if !ok || member.Name != "StateReady" {
		t.Errorf("unparenthesized member = %#v, want StateReady identifier", unparenExpression(literal.Elts[0]))
	}
}

func TestEnumRegistration(t *testing.T) {
	t.Parallel()

	buildContext := currentBuildContext()
	fset := token.NewFileSet()
	var sourceFiles, testFiles []*ast.File
	for _, name := range activeGoFiles(t, buildContext, ".") {
		file, parseErr := parser.ParseFile(fset, name, nil, 0)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", name, parseErr)
		}
		if strings.HasSuffix(name, "_test.go") {
			testFiles = append(testFiles, file)
		} else {
			sourceFiles = append(sourceFiles, file)
		}
	}

	declarations := collectDeclaredEnumConsts(t, buildContext, fset, sourceFiles)
	stringTypes := declarations.stringTypes
	declaredConsts := declarations.declaredConsts
	declaredExemptions := declarations.declaredExemptions
	registrations := map[string]enumRegistration{}
	for _, file := range sourceFiles {
		for _, declaration := range file.Decls {
			general, ok := declaration.(*ast.GenDecl)
			if !ok {
				continue
			}
			if general.Tok == token.CONST {
				continue
			}
			if general.Tok != token.VAR {
				continue
			}
			for _, specification := range general.Specs {
				valueSpec := specification.(*ast.ValueSpec)
				for index, name := range valueSpec.Names {
					if !strings.HasPrefix(name.Name, "All") || index >= len(valueSpec.Values) {
						continue
					}
					literal, ok := unparenExpression(valueSpec.Values[index]).(*ast.CompositeLit)
					if !ok {
						continue
					}
					variable, ok := declarations.sourcePackage.Scope().Lookup(name.Name).(*types.Var)
					if !ok {
						continue
					}
					elementType, ok := namedStringSliceElement(variable)
					if !ok {
						continue
					}
					typeName := elementType.Obj().Name()
					if _, ok := stringTypes[typeName]; !ok || elementType.Obj().Pkg() != declarations.sourcePackage {
						continue
					}
					if existing, duplicate := registrations[typeName]; duplicate {
						t.Errorf("%s: registration slices %s and %s both exist", typeName, existing.identifier, name.Name)
						continue
					}

					registration := enumRegistration{
						identifier: name.Name,
						members:    map[string]struct{}{},
					}
					for _, element := range literal.Elts {
						member, ok := unparenExpression(element).(*ast.Ident)
						if !ok {
							t.Errorf("%s: %s contains a non-identifier member at %s", typeName, name.Name, fset.Position(element.Pos()))
							continue
						}
						if _, duplicate := registration.members[member.Name]; duplicate {
							t.Errorf("%s: %s contains duplicate member %s", typeName, name.Name, member.Name)
							continue
						}
						registration.members[member.Name] = struct{}{}
					}
					registrations[typeName] = registration
				}
			}
		}
	}

	for name, reason := range enumRegistrationExemptions {
		if strings.TrimSpace(reason) == "" {
			t.Errorf("%s: enum-registration exemption needs a reason", name)
		}
		if !declaredExemptions[name] {
			t.Errorf("%s: enum-registration exemption does not name a package-local typed string const", name)
		}
	}

	typeNames := map[string]struct{}{}
	for typeName := range declaredConsts {
		typeNames[typeName] = struct{}{}
	}
	for typeName := range registrations {
		typeNames[typeName] = struct{}{}
	}
	for _, typeName := range slices.Sorted(maps.Keys(typeNames)) {
		registration, registered := registrations[typeName]
		if !registered {
			for _, name := range slices.Sorted(maps.Keys(declaredConsts[typeName])) {
				t.Errorf("%s: const %s has no registration slice; register it or add an exemption with a reason", typeName, name)
			}
			continue
		}
		for _, name := range slices.Sorted(maps.Keys(declaredConsts[typeName])) {
			if _, ok := registration.members[name]; !ok {
				t.Errorf("%s: const %s is not in %s; register it or add an exemption with a reason", typeName, name, registration.identifier)
			}
		}
		for _, name := range slices.Sorted(maps.Keys(registration.members)) {
			if _, ok := declaredConsts[typeName][name]; !ok {
				t.Errorf("%s: %s member %s is not a declared %s const", typeName, registration.identifier, name, typeName)
			}
		}
	}

	registrationIdentifiers := map[string]bool{}
	for _, registration := range registrations {
		registrationIdentifiers[registration.identifier] = true
	}

	testReferences := map[string]bool{}
	recordTestReferences := func(packagePath string, files []*ast.File) {
		typeInfo := &types.Info{Uses: map[*ast.Ident]types.Object{}}
		configuration := types.Config{Importer: declarations.importer}
		if _, err := configuration.Check(packagePath, fset, files, typeInfo); err != nil {
			t.Fatalf("type-check %s tests: %v", packagePath, err)
		}
		for identifier, object := range typeInfo.Uses {
			if !strings.HasSuffix(fset.PositionFor(identifier.Pos(), false).Filename, "_test.go") {
				continue
			}
			variable, ok := object.(*types.Var)
			if !ok || variable.Pkg() == nil || variable.Pkg().Path() != domainImportPath ||
				variable.Parent() != variable.Pkg().Scope() {
				continue
			}
			if registrationIdentifiers[variable.Name()] {
				testReferences[variable.Name()] = true
			}
		}
	}

	var internalTests, externalTests []*ast.File
	for _, file := range testFiles {
		if file.Name.Name == "domain" {
			internalTests = append(internalTests, file)
		} else {
			externalTests = append(externalTests, file)
		}
	}
	if len(internalTests) > 0 {
		recordTestReferences(domainImportPath, append(slices.Clone(sourceFiles), internalTests...))
	}
	if len(externalTests) > 0 {
		recordTestReferences(domainImportPath+"_test", externalTests)
	}
	for typeName, registration := range registrations {
		if !testReferences[registration.identifier] {
			t.Errorf("%s: %s is not referenced by any package test", typeName, registration.identifier)
		}
	}
}
