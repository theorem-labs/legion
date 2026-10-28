package declindex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeModule writes a hand-made reference file and its source below dir.
func writeModule(t *testing.T, dir, rel, ilean, src string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p+".ilean", []byte(ilean), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p+".lean", []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A use of `A.X.wp` that Lean recorded under module A is a use of A's constant: it belongs to A.X while A declares no
// `A.X.wp`, also once a later landing adds a module B that declares its own `A.X.wp`.
//
// A constant that Lean made in the module that uses it (`A.X.eq_1`, recorded under U) belongs to the declaration of
// its name in U's imports. When a later landing adds a module C that declares another `A.X`, which U does not
// import, U's use stays with A's, though U was not read again: an incremental refresh answers as a full one does.
func TestEdgeOwnersFollowTheModuleOfEachConstant(t *testing.T) {
	dir := t.TempDir()
	writeModule(t, dir, "A", `{"version":5,"module":"A","decls":{"A.X":[0,0,0,19,0,4,0,7]},"references":{}}`,
		"def A.X : Prop := True\n")
	writeModule(t, dir, "U", `{"version":5,"module":"U","directImports":[["A",false,false,false]],`+
		`"decls":{"U.u":[0,0,0,42,0,8,0,11],"U.v":[1,0,1,44,1,8,1,11]},"references":{`+
		`"{\"c\":{\"m\":\"A\",\"n\":\"A.X.wp\"}}":{"definition":null,"usages":[[0,17,0,23,"U.u"]]},`+
		`"{\"c\":{\"m\":\"U\",\"n\":\"A.X.eq_1\"}}":{"definition":null,"usages":[[1,17,1,25,"U.v"]]}}}`,
		"theorem U.u (h : A.X.wp) : True := trivial\ntheorem U.v (h : A.X.eq_1) : True := trivial\n")
	tree := Tree{Source: dir, Lib: dir, Roots: Roots{"A": true, "U": true, "B": true, "C": true}}
	s := openStore(t)
	ctx := context.Background()
	if _, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "one"}); err != nil {
		t.Fatal(err)
	}
	if a, err := answerFor(t, s, "A.X", AnswerOptions{}); err != nil || a.Direct.Declarations != 2 || a.Reach.StatementsChanged != 2 {
		t.Fatalf("before: %+v %+v %v", a.Direct, a.Reach, err)
	}
	writeModule(t, dir, "B", `{"version":5,"module":"B","decls":{"A.X.wp":[0,0,0,24,0,8,0,14]},"references":{}}`,
		"theorem A.X.wp : True := trivial\n")
	writeModule(t, dir, "C", `{"version":5,"module":"C","decls":{"A.X":[0,0,0,19,0,4,0,7]},"references":{}}`,
		"def A.X : Prop := True\n")
	if _, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "two"}); err != nil {
		t.Fatal(err)
	}
	full := refreshed(t, tree)
	for _, store := range []*Store{s, full} {
		found, err := store.Resolve(ctx, "A.X")
		if err != nil || len(found) != 2 || found[0].Module != "A" || found[1].Module != "C" {
			t.Fatalf("A.X resolves to %+v, %v; want A's and C's", found, err)
		}
		for i, want := range []int{2, 0} {
			a, err := store.Answer(ctx, found[i], AnswerOptions{})
			if err != nil || a.Direct.Declarations != want || a.Reach.StatementsChanged != want {
				t.Errorf("%s's A.X: %+v %+v %v; want %d users", found[i].Module, a.Direct, a.Reach, err, want)
			}
		}
		if a, err := answerFor(t, store, "A.X.wp", AnswerOptions{}); err != nil || a.Direct.Uses != 0 || a.Reach.StatementsChanged != 0 {
			t.Errorf("B's A.X.wp: %+v %+v %v; want no use: U uses A's", a.Direct, a.Reach, err)
		}
	}
}

// A reference file older than its source records positions that no longer hold the declarations. Their
// statement hashes are unknown, never the hash of whatever text now sits at those positions.
func TestStaleReferenceFileGivesNoStatementHash(t *testing.T) {
	ilean, src, _ := readGolden(t)
	shifted := append([]byte("-- a line added after the build\n"), src...)
	got, err := ParseModule("Golden", ilean, shifted, Roots{"Golden": true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Stale == 0 {
		t.Fatal("no stale declaration reported")
	}
	for _, d := range got.Decls {
		if d.StmtHash != "" {
			t.Errorf("%s has hash %s from a source newer than its reference file", d.Name, d.StmtHash)
		}
	}
	fresh, err := ParseModule("Golden", ilean, src, Roots{"Golden": true})
	if err != nil || fresh.Stale != 0 {
		t.Fatalf("the built source is stale: %d %v", fresh.Stale, err)
	}
}

func TestLoadOwnersChecksTheConfiguredFormat(t *testing.T) {
	p := filepath.Join(t.TempDir(), "owners.json")
	body := `{"format":"graph/owners/v3","owners":[{"id":"function:frame_at","contract":"Sub"}]}`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if o, err := LoadOwners(p, "graph/owners/v3"); err != nil || o["Sub"] != "function:frame_at" {
		t.Fatalf("the configured format: %v %v", o, err)
	}
	if _, err := LoadOwners(p, ""); err != nil {
		t.Fatalf("no configured format: %v", err)
	}
	if _, err := LoadOwners(p, "graph/owners/v4"); err == nil || !strings.Contains(err.Error(), "graph/owners/v4") {
		t.Fatalf("another format was accepted: %v", err)
	}
}

// A use inside a declaration that Lean records but does not declare (an auxiliary such as `A.f.go`), of that
// same name, is a use of the declaration it belongs to: an edge from A.f to A.f.go.
func TestAUseOfItsOwnUndeclaredParentIsAnEdgeFromItsOwner(t *testing.T) {
	dir := t.TempDir()
	writeModule(t, dir, "A", `{"version":5,"module":"A","decls":{"A.f":[0,0,0,15,0,4,0,7]},`+
		`"references":{"{\"c\":{\"m\":\"A\",\"n\":\"A.f.go\"}}":{"definition":null,"usages":[[0,11,0,13,"A.f.go"]]}}}`,
		"def A.f := go\n")
	s := refreshed(t, Tree{Source: dir, Lib: dir, Roots: Roots{"A": true}})
	a, err := answerFor(t, s, "A.f", AnswerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if a.Reach.StatementsChanged+a.Reach.ProofsToRedo != 1 {
		t.Fatalf("reach %+v, want the auxiliary", a.Reach)
	}
}

// Lean names the declarations of some commands itself (an elaborator's auxiliary, an option's initializer); their
// recorded selection is the command. They are not evidence that the source moved, and a `_root_.` name is its own.
func TestSelectionHoldsForNamesLeanMade(t *testing.T) {
	text := "/-- doc -/\nelab_rules : tactic | `(tactic| t) => pure ()\ndef _root_.A.b : Nat := 1\nregister_option x : Bool := {}\n"
	if !selectionHolds(text, "M._aux_M___elabRules_M_tacticT_1", 0, 10) {
		t.Error("an elaborator's auxiliary (internal name) was taken for a moved declaration")
	}
	at := strings.Index(text, "_root_")
	if !selectionHolds(text, "A.b", at, at+len("_root_.A.b")) {
		t.Error("a `_root_.` name was taken for a moved declaration")
	}
	if !selectionHolds(text, "_private.M.0.initFn._@.M.1._hyg.4", strings.Index(text, "register"), len(text)) {
		t.Error("an option's initializer (private hygienic name) was taken for a moved declaration")
	}
	if selectionHolds(text, "_private.M.0.A.c", at, at+len("_root_.A.b")) {
		t.Error("a private user declaration at another name's position was taken as current")
	}
	if selectionHolds(text, "A.z", at, at+len("_root_.A.b")) {
		t.Error("a declaration at another name's position was taken as current")
	}
}

// A module whose source moved since its build is unreadable: the refresh says so (exit 3 in the command), and
// the index refuses to answer until a build brings it back.
func TestRefreshCountsAModuleNewerThanItsBuildAsUnreadable(t *testing.T) {
	tree := goldenTree(t)
	src := filepath.Join(tree.Source, "Golden.lean")
	data, _ := os.ReadFile(src)
	if err := os.WriteFile(src, append([]byte("-- edited after the build\n"), data...), 0o644); err != nil {
		t.Fatal(err)
	}
	s := openStore(t)
	st, err := s.Refresh(context.Background(), tree, RefreshOptions{Commit: "one"})
	if err != nil {
		t.Fatal(err)
	}
	if st.Unreadable != 1 || st.Reread != 1 {
		t.Fatalf("stats %+v: want the moved module unreadable and the other read", st)
	}
	if n, _ := s.Meta(context.Background(), "errors"); n != "1" {
		t.Fatalf("errors meta %q", n)
	}
}

// Lean quotes an auxiliary's name when it holds symbols (`M.«_aux_M___macroRules_M_term_↦ₘ__1»`); its recorded
// selection is the command or its docstring, and it is as much Lean's as the unquoted kind.
func TestSelectionHoldsForQuotedNamesLeanMade(t *testing.T) {
	text := "/-- The points-to. -/\nnotation:50 a:50 \" ↦ₘ \" v:50 => f a v\n"
	for _, name := range []string{
		"MachX.«_aux_MachX_Ctx___macroRules_MachX_term_↦ₘ__1»",
		"_private.M.0.N.«_aux_M___macroRules__private_M_0_N_termRot(_)_1»",
	} {
		if !selectionHolds(text, name, 0, len("/-- The points-to. -/")) {
			t.Errorf("%s was taken for a moved declaration", name)
		}
	}
	if !madeByLean("A.«_aux_x»") || madeByLean("A.«x_y»") || madeByLean("A.«b»._c") == false {
		t.Error("madeByLean misreads quoted components")
	}
}

// Lean records the whole command as the selection range of a declaration that the command makes without writing
// its name: one a command macro makes (Init.Data.UInt.Decidable), a simproc (Iris.HeapLang.ProofMode), a computed
// field (Init.Prelude's Lean.Name.hash, from its doc comment on). The source and the build agree, so the module is
// read, and each declaration gets its statement hash.
func TestASelectionOfTheWholeCommandHolds(t *testing.T) {
	src := "declare_decidable_uint_quantifiers UInt8 8\n" +
		"simproc [wp_expr_simp] String.reduceEq_copy (( _ : String) = _) := String.reduceEq\n" +
		"/-- The hash of a name. -/\n" +
		"structure N where\n"
	ilean := `{"version":5,"module":"M","decls":{` +
		`"UInt8.forall_iff_forall_ofBitVec":[0,0,0,42,0,0,0,42],` +
		`"Iris.ProofMode.String.reduceEq_copy":[1,0,1,82,1,0,1,82],` +
		`"Lean.Name.hash":[2,0,3,17,2,0,3,17]},"references":{}}`
	md, err := ParseModule("M", []byte(ilean), []byte(src), Roots{"M": true})
	if err != nil {
		t.Fatal(err)
	}
	if md.Stale != 0 {
		t.Fatalf("%d declarations taken for moved: a command's own selection is not evidence that the source moved", md.Stale)
	}
	if len(md.Decls) != 3 {
		t.Fatalf("%d declarations read, want 3", len(md.Decls))
	}
	for _, d := range md.Decls {
		if d.StmtHash == "" {
			t.Errorf("%s has no statement hash", d.Name)
		}
	}
}

// A declaration whose recorded range lies past the end of a source that lost its lines is stale, and reading it
// slices no text outside the source; nor does a name Lean made, which passes without a written name to compare.
func TestADeclarationPastTheEndOfANewerSourceIsStale(t *testing.T) {
	ilean := []byte(`{"version":5,"module":"M","decls":{"T.t":[0,4,0,32,0,12,0,15],` +
		`"M._aux_M___elabRules_1":[0,4,0,32,0,4,0,32]},"references":{}}`)
	built, err := ParseModule("M", ilean, []byte("    theorem T.t : True := trivial\n"), Roots{"M": true})
	if err != nil || built.Stale != 0 {
		t.Fatalf("the built source is stale: %d %v", built.Stale, err)
	}
	md, err := ParseModule("M", ilean, []byte("\n"), Roots{"M": true})
	if err != nil || md.Stale != 1 {
		t.Fatalf("a declaration past the end of the source: stale %d, %v; want 1", md.Stale, err)
	}
	for _, d := range md.Decls {
		if d.Name == "T.t" && d.StmtHash != "" {
			t.Errorf("T.t has hash %s from a source that lost its line", d.StmtHash)
		}
	}
}

// A whole-command selection says nothing of the current source, since both of its ranges come from the reference
// file: a source newer than its build still fails where the recorded range no longer holds a command, also when no
// declaration that writes its name sits at or below the edit.
func TestAWholeCommandSelectionInASourceNewerThanItsBuildFails(t *testing.T) {
	command := "declare_decidable_uint_quantifiers UInt8 8\n"
	theorem := "theorem A.a : True := trivial\n"
	ilean := func(decls string) []byte {
		return []byte(`{"version":5,"module":"M","decls":{` + decls + `},"references":{}}`)
	}
	onlyCommand := ilean(`"UInt8.forall_iff_forall_ofBitVec":[0,0,0,42,0,0,0,42]`)
	withTheorem := ilean(`"A.a":[0,0,0,29,0,8,0,11],"UInt8.forall_iff_forall_ofBitVec":[1,0,1,42,1,0,1,42]`)
	for _, c := range []struct {
		name  string
		ilean []byte
		built string
		now   string
	}{
		{"a line added above a module of one command", onlyCommand, command, "-- a line added after the build\n" + command},
		{"a code line added above a module of one command", onlyCommand, command, "def A.b : Nat := 1\n" + command},
		{"the command changed below an unmoved theorem", withTheorem, theorem + command,
			theorem + "declare_decidable_uint_quantifiers UInt16 16\n"},
		{"a comment added between the theorem and the command", withTheorem, theorem + command,
			theorem + "-- a comment added after the build\n" + command},
		{"a blank line added between the theorem and the command", withTheorem, theorem + command,
			theorem + "\n" + command},
		{"the command removed", withTheorem, theorem + command, theorem},
	} {
		t.Run(c.name, func(t *testing.T) {
			built, err := ParseModule("M", c.ilean, []byte(c.built), Roots{"M": true})
			if err != nil || built.Stale != 0 {
				t.Fatalf("the built source is stale: %d %v", built.Stale, err)
			}
			md, err := ParseModule("M", c.ilean, []byte(c.now), Roots{"M": true})
			if err != nil {
				t.Fatal(err)
			}
			if md.Stale == 0 {
				t.Fatal("a source newer than its build was read as current")
			}
			for _, d := range md.Decls {
				if d.Name == "UInt8.forall_iff_forall_ofBitVec" && d.StmtHash != "" {
					t.Errorf("the command's declaration has hash %s from a source newer than its build", d.StmtHash)
				}
			}
		})
	}
}

func TestCommandAt(t *testing.T) {
	s := newSource("  cmd x -- note\n/-- doc -/\nstructure N where\nlet 𝔸 := 1\n")
	for _, c := range []struct {
		l0, c0, l1, c1 int
		want           bool
	}{
		{0, 2, 0, 7, true},   // a trailing `--` comment
		{0, 0, 0, 7, false},  // starts before the first non-blank character
		{0, 2, 0, 5, false},  // ends inside the line
		{1, 0, 2, 17, true},  // over lines, from the doc comment on
		{1, 0, 2, 18, false}, // past the line's end
		{3, 0, 3, 11, true},  // UTF-16 columns: 𝔸 is two units
		{3, 0, 3, 10, false}, // ends inside the line
		{3, 0, 4, 1, false},  // the line is gone
	} {
		if got := s.commandAt(c.l0, c.c0, c.l1, c.c1); got != c.want {
			t.Errorf("commandAt(%d, %d, %d, %d) = %v, want %v", c.l0, c.c0, c.l1, c.c1, got, c.want)
		}
	}
}

// commandAt cannot tell a command from another of the same length at the same place. For a module that has such a
// declaration, the refresh reads Lake's trace as Fresh does: a build trace of another source hash, or a fetch trace
// (an artifact-cache hit, bound to the source by age alone) older than the source, makes the module unreadable. A
// tree without traces keeps the text check alone.
func TestALakeTraceDecidesAWholeCommandSelection(t *testing.T) {
	built := "declare_decidable_uint_quantifiers UInt8 8\n"
	changed := "declare_decidable_uint_quantifiers UInt8 9\n"
	ilean := `{"version":5,"module":"M","decls":{"UInt8.forall_iff_forall_ofBitVec":[0,0,0,42,0,0,0,42]},"references":{}}`
	buildTrace := `{"schemaVersion":"2025-09-10","inputs":[["M.lean","` + LakeTextHash([]byte(built)) + `"]],` +
		`"outputs":{"i":"` + LakeTextHash([]byte(ilean)) + `.ilean"},"synthetic":false}`
	fetchTrace := `{"schemaVersion":"2025-09-10","inputs":[],"outputs":{"i":"` + LakeTextHash([]byte(ilean)) +
		`.ilean"},"synthetic":true}`
	for _, c := range []struct {
		name, src, trace string
		sourceAge        time.Duration // the source's modification time after the trace's
		unreadable       bool
	}{
		{"the built source", built, buildTrace, time.Hour, false},
		{"a changed command of the same length", changed, buildTrace, -time.Hour, true},
		{"a fetch trace newer than the source", built, fetchTrace, -time.Hour, false},
		{"the change after a fetch", changed, fetchTrace, time.Hour, true},
		{"the change without a trace", changed, "", 0, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			writeModule(t, dir, "M", ilean, c.src)
			if c.trace != "" {
				tracePath := filepath.Join(dir, "M.trace")
				if err := os.WriteFile(tracePath, []byte(c.trace), 0o644); err != nil {
					t.Fatal(err)
				}
				at := time.Now().Add(-2 * time.Hour)
				if err := os.Chtimes(tracePath, at, at); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(filepath.Join(dir, "M.lean"), at.Add(c.sourceAge), at.Add(c.sourceAge)); err != nil {
					t.Fatal(err)
				}
			}
			md, err := readModule(Tree{Source: dir, Lib: dir, Roots: Roots{"M": true}}, Roots{"M": true}, "M")
			if md.Commands != 1 {
				t.Fatalf("%d declarations held by their command, want 1", md.Commands)
			}
			if (err != nil) != c.unreadable {
				t.Fatalf("readModule: %v; want unreadable %v", err, c.unreadable)
			}
		})
	}
}
