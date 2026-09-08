package postgres

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// sqlEntryPoints are the GORM calls whose SQL argument is rendered into the
// statement VERBATIM. ParameterizedQueries only strips BOUND values, so a value
// concatenated or formatted into that argument survives into the log line — and
// is an injection besides (§30.17). The argument must be a string literal or a
// same-file const.
var sqlEntryPoints = map[string]bool{"Raw": true, "Exec": true, "Expr": true}

// internalRoot is the tree this guard owns: everything the service serves from.
const internalRoot = "../.."

func isLiteralSQL(e ast.Expr) bool {
	switch v := e.(type) {
	case *ast.BasicLit:
		return v.Kind == token.STRING
	case *ast.Ident:
		return v.Obj != nil && v.Obj.Kind == ast.Con
	case *ast.ParenExpr:
		return isLiteralSQL(v.X)
	}
	return false
}

func dynamicSQLSites(t *testing.T, root string) []string {
	t.Helper()
	var sites []string
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CallExpr:
				sel, ok := x.Fun.(*ast.SelectorExpr)
				// A zero-argument Raw() is not a SQL entry point (Vault's
				// vc.Raw() returns the raw client) — nothing is rendered.
				if !ok || !sqlEntryPoints[sel.Sel.Name] || len(x.Args) == 0 {
					return true
				}
				if !isLiteralSQL(x.Args[0]) {
					sites = append(sites, fset.Position(x.Args[0].Pos()).String())
				}
			case *ast.CompositeLit:
				sel, ok := x.Type.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Expr" {
					return true
				}
				for _, el := range x.Elts {
					kv, ok := el.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "SQL" && !isLiteralSQL(kv.Value) {
						sites = append(sites, fset.Position(kv.Value.Pos()).String())
					}
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return sites
}

// TestNoDynamicSQLStrings is the companion check to ParameterizedQueries: the
// flag redacts bound values, and this guard keeps values from being built into
// the statement text where no logger flag can reach them (D-396).
//
// Control: change any `Raw("…")` / `Exec("…")` in internal/ to a concatenation
// and this fails with that site's file:line:col.
func TestNoDynamicSQLStrings(t *testing.T) {
	root, err := filepath.Abs(internalRoot)
	if err != nil {
		t.Fatalf("resolve %s: %v", internalRoot, err)
	}
	// Positive control on the walk itself: a guard that scans nothing passes.
	scanned := 0
	if err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			scanned++
		}
		return nil
	}); err != nil {
		t.Fatalf("count %s: %v", root, err)
	}
	if scanned == 0 {
		t.Fatalf("guard scanned 0 files under %s — it cannot fail, so it proves nothing", root)
	}

	if sites := dynamicSQLSites(t, root); len(sites) > 0 {
		t.Fatalf("SQL built from a non-literal value (injection + un-redactable log line); use a string literal or a same-file const:\n  %s",
			strings.Join(sites, "\n  "))
	}
	t.Logf("scanned %d non-test .go files under %s", scanned, root)
}
