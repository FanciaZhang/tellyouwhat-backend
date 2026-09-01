package architecture

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// TestHTTPRoutesRemainGeneratedGin prevents a second, handwritten route table
// from being introduced beside the canonical OpenAPI documents.
func TestHTTPRoutesRemainGeneratedGin(t *testing.T) {
	t.Parallel()

	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve architecture test location")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
	for _, directory := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, directory), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || strings.HasSuffix(path, "generated.go") {
				return nil
			}
			checkHTTPFile(t, path)
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", directory, err)
		}
	}
}

func checkHTTPFile(t *testing.T, path string) {
	t.Helper()
	set := token.NewFileSet()
	file, err := parser.ParseFile(set, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	aliases := map[string]string{}
	for _, spec := range file.Imports {
		importPath, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		name := filepath.Base(importPath)
		if spec.Name != nil {
			name = spec.Name.Name
		}
		aliases[name] = importPath
	}
	ast.Inspect(file, func(node ast.Node) bool {
		selector, ok := node.(*ast.SelectorExpr)
		if ok {
			identifier, qualified := selector.X.(*ast.Ident)
			if qualified {
				importPath := aliases[identifier.Name]
				forbidden := importPath == "net/http" && (selector.Sel.Name == "NewServeMux" || selector.Sel.Name == "ServeMux" || selector.Sel.Name == "Handle" || selector.Sel.Name == "HandleFunc" || selector.Sel.Name == "HandlerFunc") ||
					importPath == "github.com/gin-gonic/gin" && (selector.Sel.Name == "WrapH" || selector.Sel.Name == "WrapF")
				if forbidden {
					position := set.Position(selector.Pos())
					t.Errorf("handwritten HTTP route adapter %s.%s at %s:%d", identifier.Name, selector.Sel.Name, path, position.Line)
				}
			}
		}
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok = call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch selector.Sel.Name {
		case "GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "HEAD", "Any", "Handle":
			position := set.Position(call.Pos())
			t.Errorf("handwritten route registration %s at %s:%d", selector.Sel.Name, path, position.Line)
		}
		return true
	})
}
