package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/orchestration/declindex"
)

// Two modules that are never imported together can each declare `shared`. Each declaration is its own: `where`,
// `hash`, `users` and `same` answer each one with its own module, place, hash, users, reach and statements, never
// the other's. A refresh with the compiled part reads both, though no Lean environment can hold both.
func TestSameNamedDeclarationsOfTwoModulesAreAnsweredApart(t *testing.T) {
	requireLake(t)
	ctx := context.Background()
	lean, err := declindex.FindLean(ctx, t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	tree := filepath.Join(dir, "tree")
	for _, m := range []struct{ name, source string }{
		{"A", "def shared (x : Nat) : Nat := x + 1\ntheorem bound (x : Nat) : x < x + 1 := by omega\n"},
		{"B", "def shared (x : Nat) : Nat := x + 2\ntheorem bound (x : Nat) : x ≤ x := Nat.le_refl x\n"},
		{"UseA", "import A\ndef useA : Nat := shared 1\ntheorem lowA (y : Nat) : y < y + 1 := by omega\n"},
		{"UseB", "import B\ndef useB : Nat := shared 2\ntheorem lowB (y : Nat) : y ≤ y := Nat.le_refl y\n"},
	} {
		writeTestFile(t, filepath.Join(tree, m.name+".lean"), m.source)
		if err := declindex.CompileModule(ctx, lean, nil, tree, m.name); err != nil {
			t.Fatal(err)
		}
	}
	config := filepath.Join(dir, "index.yaml")
	body := "tree: tree\nlib: .\ndb: state/index.sqlite\nroots: [A, B, UseA, UseB]\n"
	writeTestFile(t, config, body)
	if r := invoke(t, "--config", config, "refresh"); r.code != 0 {
		t.Fatalf("refresh: %+v", r)
	}

	var where []declindex.Found
	decode(t, invoke(t, "--config", config, "--json", "where", "shared"), &where)
	if len(where) != 2 || where[0].Module != "A" || where[1].Module != "B" || where[0].Path != "A.lean" ||
		where[1].Path != "B.lean" || where[0].StmtHash == where[1].StmtHash {
		t.Fatalf("where shared: %+v; want A's and B's declarations, each with its own hash", where)
	}
	hash := map[string]string{"A": where[0].StmtHash, "B": where[1].StmtHash}

	var hashed []struct{ Declaration, Hash, At string }
	decode(t, invoke(t, "--config", config, "--json", "hash", "shared"), &hashed)
	if len(hashed) != 2 || hashed[0].Hash != hash["A"] || hashed[0].At != "A.lean:1" || hashed[1].Hash != hash["B"] ||
		hashed[1].At != "B.lean:1" {
		t.Errorf("hash shared: %+v; want A's hash at A.lean:1 and B's at B.lean:1", hashed)
	}

	var users struct {
		Answers []struct {
			Module   string `json:"module"`
			At       string `json:"at"`
			StmtHash string `json:"stmt_hash"`
			Direct   struct {
				Declarations int `json:"declarations"`
			} `json:"direct"`
			Reach struct {
				StatementsChanged int `json:"statements_changed"`
			} `json:"reach"`
			Lines []string `json:"lines"`
		} `json:"answers"`
	}
	decode(t, invoke(t, "--config", config, "--json", "users", "--lines", "shared"), &users)
	if len(users.Answers) != 2 {
		t.Fatalf("users shared: %d answers, want 2: %+v", len(users.Answers), users.Answers)
	}
	for i, want := range []struct{ module, at, user string }{{"A", "A.lean:1", "UseA.lean"}, {"B", "B.lean:1", "UseB.lean"}} {
		a := users.Answers[i]
		if a.Module != want.module || a.At != want.at || a.StmtHash != hash[want.module] {
			t.Errorf("users shared, answer %d: module %q at %s hash %s; want %s's declaration at %s, hash %s", i,
				a.Module, a.At, a.StmtHash, want.module, want.at, hash[want.module])
		}
		if a.Direct.Declarations != 1 || len(a.Lines) != 1 || !strings.HasPrefix(a.Lines[0], want.user+":") {
			t.Errorf("users of %s's shared: %d declarations, lines %q; want its one user in %s", want.module,
				a.Direct.Declarations, a.Lines, want.user)
		}
		if a.Reach.StatementsChanged != 1 {
			t.Errorf("reach of %s's shared: %+v; want the one definition that uses it", want.module, a.Reach)
		}
	}

	writeTestFile(t, config, body+"lean:\n  export: true\n  jobs: 1\n")
	if r := invoke(t, "--config", config, "refresh"); r.code != 0 {
		t.Fatalf("refresh with the compiled part: %+v", r)
	}
	var same []struct {
		Module string            `json:"module"`
		Same   []declindex.Found `json:"same"`
	}
	decode(t, invoke(t, "--config", config, "--json", "same", "bound"), &same)
	got := map[string][]string{}
	for _, s := range same {
		for _, f := range s.Same {
			got[s.Module] = append(got[s.Module], f.Module+"."+f.Name)
		}
	}
	if len(same) != 2 || !slices.Equal(got["A"], []string{"UseA.lowA"}) || !slices.Equal(got["B"], []string{"UseB.lowB"}) {
		t.Errorf("same bound: %+v; want A's bound to share an atom with UseA's lowA only, and B's with UseB's lowB only", same)
	}
}

// decode reads a command's JSON output into v, failing the test unless the command exited 0.
func decode(t *testing.T, r result, v any) {
	t.Helper()
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.errOut)
	}
	if err := json.Unmarshal([]byte(r.out), v); err != nil {
		t.Fatalf("output %q: %v", r.out, err)
	}
}
