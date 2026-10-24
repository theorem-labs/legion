package ledger

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestEveryViolationKindIsARefusalReason reads the package's source. Every Violation that the ledger builds names its
// kind with one of the refusal reasons that errors.go declares and the command line prints, and Land refuses a
// candidate with that same reason.
func TestEveryViolationKindIsARefusalReason(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	reasons := map[string]string{}
	kinds := map[string]token.Position{}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.ValueSpec:
				if file != "errors.go" || len(node.Values) != 1 || !strings.HasPrefix(node.Names[0].Name, "Reason") {
					return true
				}
				literal, ok := node.Values[0].(*ast.BasicLit)
				if !ok {
					t.Fatalf("%s is not a string literal", node.Names[0].Name)
				}
				value, err := strconv.Unquote(literal.Value)
				if err != nil {
					t.Fatal(err)
				}
				reasons[node.Names[0].Name] = value
			case *ast.CompositeLit:
				if name, ok := node.Type.(*ast.Ident); !ok || name.Name != "Violation" {
					return true
				}
				for _, element := range node.Elts {
					field, ok := element.(*ast.KeyValueExpr)
					if !ok || field.Key.(*ast.Ident).Name != "Kind" {
						continue
					}
					kind, ok := field.Value.(*ast.Ident)
					if !ok {
						t.Errorf("%s: a violation kind must be a named refusal reason", fset.Position(field.Pos()))
						continue
					}
					kinds[kind.Name] = fset.Position(kind.Pos())
				}
			}
			return true
		})
	}
	if len(reasons) == 0 || len(kinds) == 0 {
		t.Fatalf("found %d refusal reasons and %d violation kinds", len(reasons), len(kinds))
	}
	for kind, at := range kinds {
		reason, ok := reasons[kind]
		if !ok {
			t.Errorf("%s: violation kind %s is not a refusal reason in errors.go", at, kind)
			continue
		}
		var refusal *Refusal
		if err := refusalForViolation(Violation{Kind: reason, Message: "refused"}); !errors.As(err, &refusal) ||
			refusal.Reason != reason || refusal.Message != "refused" {
			t.Errorf("violation %s refuses as %v", reason, err)
		}
	}
}
