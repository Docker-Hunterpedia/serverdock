package auth

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// Agent mode links these packages, and S4 has under 1 MB of headroom, so they do
// no work at load: no init(), and no package-level call except errors.New and the
// sync.Once helpers (which only wrap a func).
func TestNoPackageLevelWorkInNewPackages(t *testing.T) {
	allowedCalls := map[string]bool{"errors.New": true, "sync.OnceValue": true, "sync.OnceValues": true, "sync.OnceFunc": true}
	// M3a step 4a adds the deploy packages: jobs, deploy, agentops (and policy, once
	// step 5 creates it: a missing directory is skipped, never silently passed).
	dirs := []string{".", "../secretbox", "../datadir", "../api", "../jobs", "../deploy", "../agentops", "../policy"}
	fset := token.NewFileSet()
	checked := 0
	for _, dir := range dirs {
		files, _ := filepath.Glob(filepath.Join(dir, "*.go"))
		// The schema reflector (M2 step 9) lives in internal/actions/schema*.go.
		extra, _ := filepath.Glob("../actions/schema*.go")
		if dir == "." {
			files = append(files, extra...)
		}
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			file, err := parser.ParseFile(fset, f, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			checked++
			for _, decl := range file.Decls {
				switch d := decl.(type) {
				case *ast.FuncDecl:
					if d.Recv == nil && d.Name.Name == "init" {
						t.Errorf("%s: init() runs at load", f)
					}
				case *ast.GenDecl:
					if d.Tok != token.VAR {
						continue
					}
					for _, spec := range d.Specs {
						for _, v := range spec.(*ast.ValueSpec).Values {
							ast.Inspect(v, func(n ast.Node) bool {
								if _, isLit := n.(*ast.FuncLit); isLit {
									return false // a func body is not run at load
								}
								call, ok := n.(*ast.CallExpr)
								if !ok {
									return true
								}
								name := callName(call.Fun)
								if !allowedCalls[name] && !isConversion(name) {
									t.Errorf("%s: package-level call %s runs at load", fset.Position(call.Pos()), name)
								}
								return true
							})
						}
					}
				}
			}
		}
	}
	for _, must := range []string{"../jobs", "../deploy", "../agentops"} {
		if files, _ := filepath.Glob(filepath.Join(must, "*.go")); len(files) == 0 {
			t.Errorf("%s has no files: the package moved, update this list", must)
		}
	}
	if checked < 5 {
		t.Fatalf("only %d files checked: the directories moved?", checked)
	}
}

func callName(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return callName(x.X) + "." + x.Sel.Name
	case *ast.ArrayType:
		return "[]"
	}
	return "?"
}

// isConversion: type conversions such as []byte("x") or time.Duration(5) are not work.
func isConversion(name string) bool {
	switch name {
	case "[]", "string", "int", "int64", "uint32", "float64", "time.Duration":
		return true
	}
	return false
}
