package astx

import (
	"fmt"
	"go/ast"
	"go/token"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/tools/go/packages"
)

// Entry pairs a loaded package with a single file's syntax tree.
type Entry struct {
	Pkg  *packages.Package
	File *ast.File
}

// Loader is a one-shot loader over a directory's Go packages, indexed by
// absolute file path and by package path.
type Loader struct {
	Fset   *token.FileSet
	byFile map[string]Entry
	byPath map[string]*packages.Package
}

// ByPath returns the loaded package for the given package path (PkgPath), or
// nil when the path is not in the loaded set. Without NeedDeps, packages
// reachable through pkg.Imports carry only an ID, so callers needing GoFiles
// or Module must look them up here.
func (l *Loader) ByPath(p string) *packages.Package {
	return l.byPath[p]
}

// NewLoader loads "./..." rooted at dir using go/packages and indexes every
// compiled Go file by its absolute path.
func NewLoader(dir string) (*Loader, error) {
	fset := token.NewFileSet()
	cfg := &packages.Config{
		Mode: packages.NeedName |
			packages.NeedFiles |
			packages.NeedCompiledGoFiles |
			packages.NeedSyntax |
			packages.NeedTypes |
			packages.NeedTypesInfo |
			packages.NeedImports |
			packages.NeedModule,
		Dir:  dir,
		Fset: fset,
		// Load the target project as its own module, independent of any ambient
		// go.work workspace. A user's project (or a test fixture) is a standalone
		// module; without GOWORK=off, an enclosing workspace makes go/packages
		// reject `./...` with "directory prefix . does not contain modules listed
		// in go.work".
		Env: append(os.Environ(), "GOWORK=off"),
	}

	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		return nil, fmt.Errorf("astx: packages.Load: %w", err)
	}

	byFile := make(map[string]Entry)
	byPath := make(map[string]*packages.Package, len(pkgs))
	var errMsgs []string

	for _, pkg := range pkgs {
		if len(pkg.Errors) > 0 {
			for _, e := range pkg.Errors {
				errMsgs = append(errMsgs, fmt.Sprintf("%s: %s", pkg.PkgPath, e.Error()))
			}
			continue
		}
		byPath[pkg.PkgPath] = pkg
		// Index-align CompiledGoFiles with Syntax.
		n := len(pkg.CompiledGoFiles)
		if len(pkg.Syntax) < n {
			n = len(pkg.Syntax)
		}
		for i := 0; i < n; i++ {
			abs, absErr := filepath.Abs(pkg.CompiledGoFiles[i])
			if absErr != nil {
				return nil, fmt.Errorf("astx: filepath.Abs(%q): %w", pkg.CompiledGoFiles[i], absErr)
			}
			byFile[abs] = Entry{Pkg: pkg, File: pkg.Syntax[i]}
		}
	}

	if len(errMsgs) > 0 {
		return nil, fmt.Errorf("astx: package load errors:\n%s", strings.Join(errMsgs, "\n"))
	}

	return &Loader{Fset: fset, byFile: byFile, byPath: byPath}, nil
}

// Get returns the loaded Entry for an absolute file path.
func (l *Loader) Get(absPath string) (Entry, error) {
	e, ok := l.byFile[absPath]
	if !ok {
		return Entry{}, fmt.Errorf("astx: no entry for %q", absPath)
	}
	return e, nil
}
