package ownerharness

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"testing"
)

// The harness's dedupe-key rule is Envoy's own: MintedDedupeKeyPattern is the constant that packages/contracts
// generates into the Envoy module, read here from that generated file, so the two cannot drift apart.
func TestMintedDedupeKeyPatternIsEnvoys(t *testing.T) {
	path := filepath.Join("..", "..", "..", "envoy", "internal", "contracts", "generated.go")
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var generated string
	ast.Inspect(file, func(node ast.Node) bool {
		spec, ok := node.(*ast.ValueSpec)
		if !ok || len(spec.Names) != 1 || spec.Names[0].Name != "MintedDedupeKeyPattern" || len(spec.Values) != 1 {
			return true
		}
		if literal, ok := spec.Values[0].(*ast.BasicLit); ok && literal.Kind == token.STRING {
			generated, err = strconv.Unquote(literal.Value)
		}
		return false
	})
	if err != nil || generated == "" {
		t.Fatalf("%s holds no string constant MintedDedupeKeyPattern: %v", path, err)
	}
	if generated != MintedDedupeKeyPattern {
		t.Fatalf("MintedDedupeKeyPattern = %q, but Envoy's generated contract is %q", MintedDedupeKeyPattern, generated)
	}
}
