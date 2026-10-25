package landing

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// activeSetWriters are the only functions that may assign a batch's active set: removeMember, which every drop goes
// through, the two freezes that set it from their members (a new batch's input, and a declined batch's successor
// from the members still ready), and the deep copy.
var activeSetWriters = map[string]bool{
	"removeMember":    true,
	"Run":             true,
	"createSuccessor": true,
	"cloneBatch":      true,
}

// TestOnlyDropMembersAssignsTheActiveSet fails when code outside the allowed functions assigns ActiveMembers, so a
// member cannot leave the active set without going through dropMembers.
func TestOnlyDropMembersAssignsTheActiveSet(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var stray []string
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || activeSetWriters[fn.Name.Name] {
				continue
			}
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				switch node := node.(type) {
				case *ast.AssignStmt:
					for _, lhs := range node.Lhs {
						if assignsActiveSet(lhs) {
							stray = append(stray, fset.Position(lhs.Pos()).String()+" in "+fn.Name.Name)
						}
					}
				case *ast.KeyValueExpr:
					if key, ok := node.Key.(*ast.Ident); ok && key.Name == "ActiveMembers" {
						stray = append(stray, fset.Position(node.Pos()).String()+" in "+fn.Name.Name)
					}
				}
				return true
			})
		}
	}
	sort.Strings(stray)
	if len(stray) > 0 {
		t.Fatalf("ActiveMembers is assigned outside dropMembers's helpers:\n%s", strings.Join(stray, "\n"))
	}
}

// assignsActiveSet reports whether an assignment target is a batch's ActiveMembers field.
func assignsActiveSet(lhs ast.Expr) bool {
	selector, ok := lhs.(*ast.SelectorExpr)
	return ok && selector.Sel.Name == "ActiveMembers"
}
