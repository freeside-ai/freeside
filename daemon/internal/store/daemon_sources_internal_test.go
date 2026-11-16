package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// daemonSource is one parsed non-test Go file of the daemon module. rel is
// its slash path relative to daemon/, such as "internal/store/pairing.go".
type daemonSource struct {
	rel  string
	file *ast.File
}

// daemonParse is every non-test Go file under daemon/, in WalkDir order,
// parsed with mode 0 into one file set. The file set records each file under
// its path from this package's directory ("../../internal/store/pairing.go"),
// so a test that prints a position under another name sets it itself.
type daemonParse struct {
	fset    *token.FileSet
	sources []daemonSource
}

// daemonSources parses the daemon's sources once per test process, for the
// AST ratchets to share; each ratchet keeps its own file filter. The ASTs
// are read-only: a test that changed one would change what the others check.
var daemonSources = sync.OnceValues(func() (daemonParse, error) {
	// go test runs with the package directory as the working directory.
	root := filepath.Join("..", "..")
	parsed := daemonParse{fset: token.NewFileSet()}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		file, err := parser.ParseFile(parsed.fset, path, nil, 0)
		if err != nil {
			return err
		}
		parsed.sources = append(parsed.sources, daemonSource{rel: filepath.ToSlash(relative), file: file})
		return nil
	})
	return parsed, err
})

// positionAs renders pos as file:line:column under filename, so a ratchet
// keeps the file name its messages used before it shared this parse.
func (p daemonParse) positionAs(pos token.Pos, filename string) string {
	position := p.fset.Position(pos)
	position.Filename = filename
	return position.String()
}
