package main

import (
	"path/filepath"
	"testing"
)

// writeModuleFiles writes a hand-made reference file and its source for the module at rel below tree.
func writeModuleFiles(t *testing.T, tree, rel, ilean, source string) {
	t.Helper()
	writeTestFile(t, filepath.Join(tree, rel+".ilean"), ilean)
	writeTestFile(t, filepath.Join(tree, rel+".lean"), source)
}

// Lean records a constant that it makes where it is used (an equation lemma such as `f.eq_1`) under the module
// that uses it. Such a use belongs to the declaration it was made from: the one of the module that the user imports,
// when two modules declare that name.
func TestAPartLeanMadeInTheUsingModuleBelongsToTheDeclarationItImports(t *testing.T) {
	dir := t.TempDir()
	tree := filepath.Join(dir, "tree")
	for _, m := range []string{"P", "Q"} {
		writeModuleFiles(t, tree, m, `{"version":5,"module":"`+m+`","directImports":[],"decls":{"f":[0,0,0,16,0,4,0,5]},"references":{}}`,
			"def f : Nat := 1\n")
	}
	writeModuleFiles(t, tree, "U", `{"version":5,"module":"U","directImports":[["Q",false,false,false]],`+
		`"decls":{"u":[0,0,0,27,0,8,0,9]},"references":{`+
		`"{\"c\":{\"m\":\"Q\",\"n\":\"f\"}}":{"definition":null,"usages":[[0,12,0,13,"u"]]},`+
		`"{\"c\":{\"m\":\"U\",\"n\":\"f.eq_1\"}}":{"definition":null,"usages":[[0,21,0,27,"u"]]}}}`,
		"theorem u : f = 1 := f.eq_1\n")
	config := filepath.Join(dir, "index.yaml")
	writeTestFile(t, config, "tree: tree\nlib: .\ndb: state/index.sqlite\nroots: [P, Q, U]\n")
	if r := invoke(t, "--config", config, "refresh"); r.code != 0 {
		t.Fatalf("refresh: %+v", r)
	}
	var users struct {
		Answers []struct {
			Module string `json:"module"`
			Direct struct {
				Uses         int `json:"uses"`
				Declarations int `json:"declarations"`
			} `json:"direct"`
			Reach struct {
				StatementsChanged int `json:"statements_changed"`
			} `json:"reach"`
		} `json:"answers"`
	}
	decode(t, invoke(t, "--config", config, "--json", "users", "f"), &users)
	if len(users.Answers) != 2 {
		t.Fatalf("users f: %+v; want an answer for each of P's and Q's", users.Answers)
	}
	p, q := users.Answers[0], users.Answers[1]
	if p.Module != "P" || p.Direct.Uses != 0 || p.Reach.StatementsChanged != 0 {
		t.Errorf("P's f: %+v; want no use: U imports Q", p)
	}
	if q.Module != "Q" || q.Direct.Uses != 2 || q.Direct.Declarations != 1 || q.Reach.StatementsChanged != 1 {
		t.Errorf("Q's f: %+v; want u's two uses, of f and of its equation lemma, and u in its reach", q)
	}
}
