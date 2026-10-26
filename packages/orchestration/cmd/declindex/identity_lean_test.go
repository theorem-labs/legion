package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/orchestration/conceptgate"
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
	body := "tree: tree\nlib: .\nindex: idx\nstate: state\nroots: [A, B, UseA, UseB]\n"
	writeIndexConfig(t, config, body)
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

	writeIndexConfig(t, config, body+"lean:\n  export: true\n  jobs: 1\n")
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

// A compiled hash counts each constant that a statement names with the module that declares it. A proposal that
// defines B's `Value` again is not an exact duplicate of A's definition of A's `Value`, though both read `Value`:
// the gate adds it. One that defines A's `Value` again is: the gate refuses it.
func TestTheGateHashesAConstantWithItsModule(t *testing.T) {
	requireLake(t)
	config, tree, lean := leanRegistryTree(t, "A", "B")
	for _, m := range []struct{ name, source string }{
		{"A", "def Value : Nat := 1\ndef Original : Nat := Value\nexample : Original = 1 := rfl\n"},
		{"B", "def Value : Nat := 2\n"},
	} {
		writeTestFile(t, filepath.Join(tree, m.name+".lean"), m.source)
		if err := declindex.CompileModule(context.Background(), lean, nil, tree, m.name); err != nil {
			t.Fatal(err)
		}
	}
	land(t, config, tree, "add A and B")
	gateAddition(t, config, "different", "Different", "Different.lean", "import B\n\ndef Different : Nat := Value\nexample : Different = 2 := rfl\n")
	proposal := filepath.Join(t.TempDir(), "copy.json")
	writeTestFile(t, proposal, `{"id":"copy","concept":{"name":"Copy","home":"Copy.lean","meaning":"A copy of A's value."},`+
		`"source":"import A\n\ndef Copy : Nat := Value\n"}`)
	r := invoke(t, "--config", config, "gate", "--proposer", "role", proposal)
	var v conceptgate.Verdict
	if err := json.Unmarshal([]byte(r.out), &v); err != nil || r.code != 1 || v.Outcome != conceptgate.Refused || v.Existing != "Original" {
		t.Fatalf("gate a copy of A's Original: want exit 1, refused as Original, got %+v, %+v (%v)", r, v, err)
	}
}

// Lean imports two theorems of one name and statement from two modules as one constant, and keeps the module that
// loaded first. A reference to such a theorem has one identity however the index read it: B's `Original` names B's
// `dup`, which A declares too. A proposal that imports B and states `Original`'s statement again is an exact
// duplicate, after a refresh that read A and B in one exporter group and after one that read B alone.
func TestTheGateRefusesACopyOfAMergedTheoremReferenceHoweverTheIndexWasRead(t *testing.T) {
	requireLake(t)
	config, tree, lean := leanRegistryTree(t, "A", "B")
	compile := func(name, source string) {
		t.Helper()
		writeTestFile(t, filepath.Join(tree, name+".lean"), source)
		if err := declindex.CompileModule(context.Background(), lean, nil, tree, name); err != nil {
			t.Fatal(err)
		}
	}
	compile("A", "theorem dup : True := trivial\n")
	compile("B", "theorem dup : True := trivial\ntheorem Original : dup = dup := rfl\n")
	land(t, config, tree, "add A and B")
	proposal := filepath.Join(t.TempDir(), "copy.json")
	writeTestFile(t, proposal, `{"id":"copy","concept":{"name":"Different","home":"Different.lean","meaning":"B's Original again."},`+
		`"source":"import B\n\ntheorem Different : dup = dup := rfl\n"}`)
	refused := func(label string) {
		t.Helper()
		r := invoke(t, "--config", config, "gate", "--proposer", "role", proposal)
		var v conceptgate.Verdict
		if err := json.Unmarshal([]byte(r.out), &v); err != nil || r.code != 1 || v.Outcome != conceptgate.Refused || v.Existing != "Original" {
			t.Errorf("gate a copy of B's Original %s: want exit 1, refused as Original, got %+v, %+v (%v)", label, r, v, err)
		}
	}
	refused("after a refresh that read A and B together")
	compile("B", "theorem dup : True := trivial\ntheorem Original : dup = dup := rfl\ntheorem spare : True := trivial\n")
	land(t, config, tree, "add a theorem to B")
	refused("after a refresh that read B alone")
}

// gateRefuses runs `gate` on a proposal of the concept name, with its home file and source, and requires exit 1, a
// refused verdict naming one of existing.
func gateRefuses(t *testing.T, config, label, name, source string, existing ...string) {
	t.Helper()
	raw, err := json.Marshal(conceptgate.Proposal{ID: "copy", Concept: conceptgate.Concept{Name: name, Home: name + ".lean",
		Meaning: "A copy of an existing statement."}, Source: source})
	if err != nil {
		t.Fatal(err)
	}
	proposal := filepath.Join(t.TempDir(), "copy.json")
	writeTestFile(t, proposal, string(raw))
	r := invoke(t, "--config", config, "gate", "--proposer", "role", proposal)
	var v conceptgate.Verdict
	if err := json.Unmarshal([]byte(r.out), &v); err != nil || r.code != 1 || v.Outcome != conceptgate.Refused ||
		!slices.Contains(existing, v.Existing) {
		t.Errorf("gate %s: want exit 1, refused as one of %q, got %+v, %+v (%v)", label, existing, r, v, err)
	}
}

// Lean abstracts a proof inside a definition's value into an auxiliary theorem (`foo._proof_2`), and in one module
// reuses an earlier one of the same type: `bar`'s value names `foo._proof_2`. A copy of `bar` gets its own
// abstracted proof, of the same type. The hash counts an abstracted proof by its type, so the copy is refused, with
// or without importing the module, as a copy of `foo` is.
func TestTheGateRefusesACopyThatUsesAnotherDeclarationsAbstractedProof(t *testing.T) {
	requireLake(t)
	config, tree, lean := leanRegistryTree(t, "A")
	writeTestFile(t, filepath.Join(tree, "A.lean"), "def foo (n : Nat) (h : 0 < n) : Fin n := ⟨n - 1, by omega⟩\n"+
		"def bar (n : Nat) (h : 0 < n) : Fin n := ⟨n - 1, Nat.sub_lt h Nat.one_pos⟩\n")
	if err := declindex.CompileModule(context.Background(), lean, nil, tree, "A"); err != nil {
		t.Fatal(err)
	}
	land(t, config, tree, "add A")
	const copyOfBar = "def Copy (n : Nat) (h : 0 < n) : Fin n := ⟨n - 1, Nat.sub_lt h Nat.one_pos⟩\n"
	gateRefuses(t, config, "a copy of bar", "Copy", copyOfBar, "bar", "foo")
	gateRefuses(t, config, "a copy of bar that imports A", "Copy", "import A\n\n"+copyOfBar, "bar", "foo")
	gateRefuses(t, config, "a copy of foo", "Copy", "def Copy (n : Nat) (h : 0 < n) : Fin n := ⟨n - 1, by omega⟩\n", "bar", "foo")
}

// Only a theorem whose name Lean generated for an abstracted proof (`X._proof_2`), or a matcher, is an auxiliary that
// the hash reads by its content. A user's constructor whose name merely looks like one (`Choice._proof_left`,
// `Pick.match_left`) keeps its own identity, so a definition that returns another constructor of the same type is a
// new statement: the gate adds it.
func TestTheGateAddsADefinitionOfAnotherConstructorWhoseNameLooksGenerated(t *testing.T) {
	requireLake(t)
	config, tree, lean := leanRegistryTree(t, "A")
	writeTestFile(t, filepath.Join(tree, "A.lean"), "inductive Choice : Type where\n  | _proof_left\n  | _proof_right\n\n"+
		"inductive Pick : Type where\n  | match_left\n  | match_right\n\n"+
		"def Original : Choice := Choice._proof_left\n\ndef Picked : Pick := Pick.match_left\n")
	if err := declindex.CompileModule(context.Background(), lean, nil, tree, "A"); err != nil {
		t.Fatal(err)
	}
	land(t, config, tree, "add A")
	for _, c := range []struct{ name, source string }{
		{"Different", "import A\n\ndef Different : Choice := Choice._proof_right\n"},
		{"Other", "import A\n\ndef Other : Pick := Pick.match_right\n"},
	} {
		raw, err := json.Marshal(conceptgate.Proposal{ID: c.name, Concept: conceptgate.Concept{Name: c.name, Home: c.name + ".lean",
			Meaning: "The other constructor."}, Source: c.source})
		if err != nil {
			t.Fatal(err)
		}
		proposal := filepath.Join(t.TempDir(), c.name+".json")
		writeTestFile(t, proposal, string(raw))
		r := invoke(t, "--config", config, "gate", "--proposer", "role", proposal)
		var v conceptgate.Verdict
		if err := json.Unmarshal([]byte(r.out), &v); err != nil || r.code != 0 || v.Outcome != conceptgate.Added {
			t.Errorf("gate %s: want exit 0, added, got %+v, %+v (%v)", c.name, r, v, err)
		}
	}
	// The control: a copy of Original, which returns the same constructor, is still refused.
	gateRefuses(t, config, "a copy of Original", "Same", "import A\n\ndef Same : Choice := Choice._proof_left\n", "Original")
}

// Lean reuses a matcher in one module: `bar`'s value names `foo.match_1`, since a matcher depends only on the
// patterns, not on the right-hand sides. A copy of `bar` gets its own matcher, with the same content. The exporter
// imports modules without loading their environment extensions, so the matcher table that `Meta.isMatcherCore` reads
// is empty there; a matcher is still read by its content, and the copy is refused as `bar`.
func TestTheGateRefusesACopyThatUsesAnotherDeclarationsMatcher(t *testing.T) {
	requireLake(t)
	config, tree, lean := leanRegistryTree(t, "A")
	writeTestFile(t, filepath.Join(tree, "A.lean"), "def foo (n : Nat) : Nat := match n with | 0 => 1 | k + 1 => k\n"+
		"def bar (n : Nat) : Nat := match n with | 0 => 7 | k + 1 => k + 2\n")
	if err := declindex.CompileModule(context.Background(), lean, nil, tree, "A"); err != nil {
		t.Fatal(err)
	}
	land(t, config, tree, "add A")
	const copy = "def Copy (n : Nat) : Nat := match n with | 0 => 7 | k + 1 => k + 2\n"
	gateRefuses(t, config, "a copy of bar", "Copy", copy, "bar")
	gateRefuses(t, config, "a copy of bar that imports A", "Copy", "import A\n\n"+copy, "bar")
}

// An abstracted proof in a theorem's type (`t3._proof_1`) is merged with the theorem when two modules declare it, so
// its identity, like the theorem's, does not depend on the modules that one exporter run read together. B's
// `Original` restates `t3`'s type; a proposal that imports B and states it again is refused after a refresh that read
// A and B together and after one that read B alone.
func TestTheGateRefusesACopyOfAStatementWithAMergedAbstractedProofHoweverTheIndexWasRead(t *testing.T) {
	requireLake(t)
	config, tree, lean := leanRegistryTree(t, "A", "B")
	compile := func(name, source string) {
		t.Helper()
		writeTestFile(t, filepath.Join(tree, name+".lean"), source)
		if err := declindex.CompileModule(context.Background(), lean, nil, tree, name); err != nil {
			t.Fatal(err)
		}
	}
	const t3 = "theorem t3 (n : Nat) (h : 0 < n) : (⟨n - 1, by omega⟩ : Fin n).val = n - 1 := rfl\n"
	const original = "def Original (n : Nat) (h : 0 < n) : (⟨n - 1, by omega⟩ : Fin n).val = n - 1 := t3 n h\n"
	compile("A", t3)
	compile("B", t3+original)
	land(t, config, tree, "add A and B")
	const copy = "import B\n\ndef Different (n : Nat) (h : 0 < n) : (⟨n - 1, by omega⟩ : Fin n).val = n - 1 := t3 n h\n"
	gateRefuses(t, config, "a copy of B's Original after a refresh that read A and B together", "Different", copy, "Original")
	compile("B", t3+original+"theorem spare : True := trivial\n")
	land(t, config, tree, "add a theorem to B")
	gateRefuses(t, config, "a copy of B's Original after a refresh that read B alone", "Different", copy, "Original")
}

// A name whose source writes it with a leading `_` is a declaration like any other: two modules that each declare
// `_Value` are never imported together, and a refresh with the compiled part reads them in two exporter groups, as
// it reads two modules that each declare `Value`.
func TestTwoModulesThatEachDeclareAnUnderscoreNameAreReadApart(t *testing.T) {
	requireLake(t)
	ctx := context.Background()
	lean, err := declindex.FindLean(ctx, t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	tree := filepath.Join(dir, "tree")
	for _, m := range []struct{ name, source string }{
		{"A", "def _Value : Nat := 1\ndef Original : Nat := _Value\n"},
		{"B", "def _Value : Nat := 2\n"},
	} {
		writeTestFile(t, filepath.Join(tree, m.name+".lean"), m.source)
		if err := declindex.CompileModule(ctx, lean, nil, tree, m.name); err != nil {
			t.Fatal(err)
		}
	}
	config := filepath.Join(dir, "index.yaml")
	writeIndexConfig(t, config, "tree: tree\nlib: .\nindex: idx\nstate: state\nroots: [A, B]\nlean:\n  export: true\n  jobs: 1\n")
	if r := invoke(t, "--config", config, "refresh"); r.code != 0 || !strings.Contains(r.errOut, "2 of 2 module groups") {
		t.Fatalf("refresh with the compiled part: want exit 0 after two exporter groups, got %+v", r)
	}
	var where []declindex.Found
	decode(t, invoke(t, "--config", config, "--json", "where", "_Value"), &where)
	if len(where) != 2 || where[0].Module != "A" || where[1].Module != "B" {
		t.Fatalf("where _Value: %+v; want A's and B's declarations", where)
	}
}

// `states` counts the declarations that state a concept's fact, each by its module and its name: A's `shared` and
// B's are two, and a structure whose constructor and field state the fact is its one declaration. Its JSON gives the
// module of each statement's declaration.
func TestStatesCountsEachDeclarationByItsModuleAndName(t *testing.T) {
	requireLake(t)
	ctx := context.Background()
	lean, err := declindex.FindLean(ctx, t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	tree := filepath.Join(dir, "tree")
	for _, m := range []struct{ name, source string }{
		{"A", "theorem shared (n : Nat) : n = n := rfl\nstructure S where\n  x : Nat\n  h : x = x\n"},
		{"B", "theorem shared (n : Nat) : n = n := rfl\n"},
	} {
		writeTestFile(t, filepath.Join(tree, m.name+".lean"), m.source)
		if err := declindex.CompileModule(ctx, lean, nil, tree, m.name); err != nil {
			t.Fatal(err)
		}
	}
	writeTestFile(t, filepath.Join(tree, "registry", "registry.yaml"), "version: 2\n")
	writeTestFile(t, filepath.Join(tree, "registry", "concepts", "Reflexive.yaml"), "name: Reflexive\nhome: A.lean\n"+
		"meaning: A natural number equals itself.\nforms:\n  - \"fun (n : Nat) => n = n\"\n")
	config := filepath.Join(dir, "index.yaml")
	writeIndexConfig(t, config, "tree: tree\nlib: .\nindex: idx\nstate: state\nroots: [A, B]\nregistry: registry\nlean:\n  export: true\n  jobs: 1\n")
	if r := invoke(t, "--config", config, "refresh"); r.code != 0 {
		t.Fatalf("refresh with the compiled part: %+v", r)
	}
	if r := invoke(t, "--config", config, "states", "Reflexive"); r.code != 0 || !strings.HasPrefix(r.out, "Reflexive: 3 declarations\n") {
		t.Errorf("states Reflexive: %+v; want 3 declarations: A's shared and S, and B's shared", r)
	}
	var states map[string][]struct {
		Declaration string `json:"declaration"`
		Module      string `json:"declaration_module"`
	}
	decode(t, invoke(t, "--config", config, "--json", "states", "Reflexive"), &states)
	var got []string
	for _, s := range states["Reflexive"] {
		if d := s.Module + "." + s.Declaration; !slices.Contains(got, d) {
			got = append(got, d)
		}
	}
	slices.Sort(got)
	if want := []string{"A.S", "A.shared", "B.shared"}; !slices.Equal(got, want) {
		t.Errorf("states --json Reflexive: declarations %q of %+v, want %q", got, states, want)
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
