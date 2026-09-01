package sandbox

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Guard both runtime code and its fixtures: moving a host executor into a test
// helper must not silently reintroduce the execution path this cleanup removes.
// Ordinary build/SDK runner subprocesses outside these boundaries are unrelated.
func TestSandboxExecutionDoesNotSpawnHostProcesses(t *testing.T) {
	for _, root := range []string{".", "../agentruntime", "../temporal"} {
		err := filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || !strings.HasSuffix(name, ".go") {
				return nil
			}
			file, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
			if err != nil {
				return err
			}
			imports := map[string]string{}
			for _, item := range file.Imports {
				value, err := strconv.Unquote(item.Path.Value)
				if err != nil {
					return err
				}
				if value == "os/exec" {
					t.Errorf("%s imports a host command executor; sandbox commands belong in OpenSandbox", name)
				}
				alias := filepath.Base(value)
				if item.Name != nil {
					alias = item.Name.Name
				}
				imports[alias] = value
			}
			ast.Inspect(file, func(node ast.Node) bool {
				call, ok := node.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				qualifier, ok := call.X.(*ast.Ident)
				if !ok {
					return true
				}
				pkg := imports[qualifier.Name]
				if (pkg == "os" && call.Sel.Name == "StartProcess") || (pkg == "syscall" && (call.Sel.Name == "ForkExec" || call.Sel.Name == "Exec")) {
					t.Errorf("%s references host process execution: %s.%s", name, pkg, call.Sel.Name)
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
