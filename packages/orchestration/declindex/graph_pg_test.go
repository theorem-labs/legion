package declindex

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// leanModule is a hand-made module of a test tree: its name, its direct imports, its source, the declarations its
// reference file records and the constants its source uses. Each declaration is written on a line of its own, and
// each use on the line of the declaration that holds it.
type leanModule struct {
	name    string
	imports []string
	src     string
	decls   []string
	uses    []leanUse
}

// leanUse is one constant that a module's source uses: its name, the module Lean records for it (the one that
// declares it, or for a constant Lean made where it is used, the using module), and the text that names it.
type leanUse struct {
	target, tmodule, text string
}

// write writes the module's reference file and its source below dir, with the ranges that its source gives.
func (m leanModule) write(t *testing.T, dir string) {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(m.src, "\n"), "\n")
	lineOf := func(text string) (int, int) {
		t.Helper()
		for i, l := range lines {
			if c := strings.Index(l, text); c >= 0 {
				return i, c
			}
		}
		t.Fatalf("module %s: its source holds no %q:\n%s", m.name, text, m.src)
		return 0, 0
	}
	decls := map[string][]int{}
	holder := map[int]string{}
	for _, d := range m.decls {
		i, c := lineOf(d)
		decls[d] = []int{i, 0, i, len(lines[i]), i, c, i, c + len(d)}
		holder[i] = d
	}
	refs := map[string]map[string]any{}
	for _, u := range m.uses {
		i, c := lineOf(u.text)
		key := `{"c":{"m":"` + u.tmodule + `","n":"` + u.target + `"}}`
		if refs[key] == nil {
			refs[key] = map[string]any{"definition": nil, "usages": []any{}}
		}
		refs[key]["usages"] = append(refs[key]["usages"].([]any), []any{i, c, i, c + len(u.text), holder[i]})
	}
	f := map[string]any{"version": IleanVersion, "module": m.name, "decls": decls, "references": refs}
	if len(m.imports) > 0 {
		var imports [][]any
		for _, i := range m.imports {
			imports = append(imports, []any{i, false, false, false})
		}
		f["directImports"] = imports
	}
	ilean, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	writeModule(t, dir, m.name, string(ilean), m.src)
}

// leanTree writes the modules below dir and gives the tree that holds them.
func leanTree(t *testing.T, dir string, ms ...leanModule) Tree {
	t.Helper()
	roots := Roots{}
	for _, m := range ms {
		m.write(t, dir)
		roots[strings.SplitN(m.name, ".", 2)[0]] = true
	}
	return Tree{Source: dir, Lib: dir, Roots: roots}
}

// referenceTables are the tables a refresh's reference part writes, with the column that says which module, or
// which file, each row belongs to. meta and landings record the refresh itself, not a module.
var referenceTables = []struct{ table, owner string }{
	{"sources", "path"}, {"modules", "module"}, {"imports", "module"},
	{"decls", "module"}, {"uses", "module"}, {"edges", "module"},
}

// rowsOfOtherModules gives every row of the reference tables that is neither module's nor its file's, as the
// table, the row and the transaction that wrote it: a row a refresh rewrites gets another xmin, also when its
// values do not change, so two readings that are equal say that the refresh between them wrote no such row.
func rowsOfOtherModules(t *testing.T, s *Store, module string) []string {
	t.Helper()
	var out []string
	for _, tb := range referenceTables {
		rows, err := s.q.Query(context.Background(), `SELECT `+tb.owner+`, t::text, t.xmin::text FROM `+tb.table+` t`)
		if err := eachRow(rows, err, func(r pgx.Rows) error {
			var owner, row, xmin string
			if err := r.Scan(&owner, &row, &xmin); err != nil {
				return err
			}
			if owner != module && owner != module+".lean" {
				out = append(out, tb.table+" "+row+" written by "+xmin)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	sort.Strings(out)
	return out
}

// edgeOwners is every edge of the index with the declaration its target belongs to, sorted.
func edgeOwners(t *testing.T, s *Store) []string {
	t.Helper()
	var out []string
	rows, err := s.q.Query(context.Background(),
		`SELECT target, tmodule, dst, module, owner, owner_module, by_closure FROM edges`)
	if err := eachRow(rows, err, func(r pgx.Rows) error {
		var target, tmodule, dst, module, owner, ownerModule string
		var byClosure bool
		if err := r.Scan(&target, &tmodule, &dst, &module, &owner, &ownerModule, &byClosure); err != nil {
			return err
		}
		out = append(out, target+" of "+tmodule+" used by "+dst+" of "+module+": "+owner+" of "+ownerModule+
			map[bool]string{true: ", by the imports", false: ""}[byClosure])
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

// foundIn is the declaration ref of the index, which must hold exactly it.
func foundIn(t *testing.T, s *Store, ref declRef) Found {
	t.Helper()
	found, err := s.Resolve(context.Background(), ref.name)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(found, func(f Found) bool { return f.Name == ref.name && f.Module == ref.module })
	if i < 0 {
		t.Fatalf("%s of %s is not in the index: %+v", ref.name, ref.module, found)
	}
	return found[i]
}

// reachOf is how many statements a change to ref's statement changes.
func reachOf(t *testing.T, s *Store, ref declRef) int {
	t.Helper()
	a, err := s.Answer(context.Background(), foundIn(t, s, ref), AnswerOptions{Why: true})
	if err != nil {
		t.Fatal(err)
	}
	return a.Reach.StatementsChanged
}

// sameAsFullRefresh checks that the index the landings left answers as one built from nothing from the same tree:
// every edge's owner, and each declaration's whole answer, its why chains and all.
func sameAsFullRefresh(t *testing.T, s *Store, tree Tree, refs ...declRef) {
	t.Helper()
	ctx := context.Background()
	full := refreshed(t, tree)
	if a, b := edgeOwners(t, s), edgeOwners(t, full); !slices.Equal(a, b) {
		t.Errorf("the edges of the index the landings left:\n%s\nand of one built from nothing:\n%s",
			strings.Join(a, "\n"), strings.Join(b, "\n"))
	}
	for _, ref := range refs {
		a, err := s.Answer(ctx, foundIn(t, s, ref), AnswerOptions{Why: true})
		if err != nil {
			t.Fatal(err)
		}
		b, err := full.Answer(ctx, foundIn(t, full, ref), AnswerOptions{Why: true})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(a, b) {
			t.Errorf("%s of %s: the index the landings left answers\n%+v\nand one built from nothing\n%+v",
				ref.name, ref.module, a, b)
		}
	}
}

// A landing that changes one module and declares nothing new writes no row of any other module: a refresh
// resolves the owner of each edge it writes, and updates another row's owner only where its change moved it. So
// the rows of the modules it did not read keep the transaction that wrote them, and the index answers as one
// built from nothing.
func TestALandingWritesNoRowOfAnotherModule(t *testing.T) {
	dir := t.TempDir()
	tree := leanTree(t, dir,
		leanModule{name: "A", decls: []string{"A.X"}, src: "def A.X : Prop := True\n"},
		leanModule{name: "W", decls: []string{"W.w"}, src: "def W.w : Prop := True\n"},
		leanModule{name: "B", imports: []string{"A"}, decls: []string{"B.b"},
			src:  "theorem B.b (h : A.X.wp) : True := trivial\n",
			uses: []leanUse{{"A.X.wp", "A", "A.X.wp"}}},
		// V's constant is one Lean made where it is used: its owner is whatever V's import closure declares.
		leanModule{name: "V", imports: []string{"W"}, decls: []string{"V.v"},
			src:  "theorem V.v (h : A.X.eq_1) : True := trivial\n",
			uses: []leanUse{{"A.X.eq_1", "V", "A.X.eq_1"}}},
		leanModule{name: "U", imports: []string{"A"}, decls: []string{"U.u"},
			src:  "theorem U.u (h : A.X.wp) : True := trivial\n",
			uses: []leanUse{{"A.X.wp", "A", "A.X.wp"}}},
	)
	ctx := context.Background()
	s := openStore(t)
	if _, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "one"}); err != nil {
		t.Fatal(err)
	}
	// B.b and U.u use A.X.wp, which belongs to A.X; V's A.X.eq_1 belongs to no declaration V imports.
	if got := reachOf(t, s, declRef{"A", "A.X"}); got != 2 {
		t.Fatalf("a change to A.X changes %d statements; want B.b and U.u", got)
	}
	before := rowsOfOtherModules(t, s, "U")
	// U's landing: its proof changes, its statement, its declarations and its imports do not.
	leanModule{name: "U", imports: []string{"A"}, decls: []string{"U.u"},
		src:  "theorem U.u (hh : A.X.wp) : True := by trivial\n",
		uses: []leanUse{{"A.X.wp", "A", "A.X.wp"}}}.write(t, dir)
	if _, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "two"}); err != nil {
		t.Fatal(err)
	}
	after := rowsOfOtherModules(t, s, "U")
	if !slices.Equal(before, after) {
		t.Errorf("the landing rewrote rows of other modules.\nbefore:\n%s\nafter:\n%s",
			strings.Join(before, "\n"), strings.Join(after, "\n"))
	}
	if got := reachOf(t, s, declRef{"A", "A.X"}); got != 2 {
		t.Errorf("after the landing a change to A.X changes %d statements; want B.b and U.u", got)
	}
	sameAsFullRefresh(t, s, tree, declRef{"A", "A.X"}, declRef{"U", "U.u"}, declRef{"V", "V.v"})
}

// An index that landings built answers reach as one built from nothing, whatever a landing changed about the
// declaration a constant belongs to: a declaration added where the use names it, one added where the use cannot
// see it, and the imports of a module in the user's import closure, which no declaration change accompanies.
func TestAnIncrementalRefreshAnswersReachAsAFullOne(t *testing.T) {
	ctx := context.Background()
	t.Run("a landing declares the constant the earlier use names", func(t *testing.T) {
		dir := t.TempDir()
		tree := leanTree(t, dir,
			leanModule{name: "A", decls: []string{"A.X"}, src: "def A.X : Prop := True\n"},
			leanModule{name: "U", imports: []string{"A"}, decls: []string{"U.u"},
				src:  "theorem U.u (h : A.X.wp) : True := trivial\n",
				uses: []leanUse{{"A.X.wp", "A", "A.X.wp"}}})
		s := openStore(t)
		if _, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "one"}); err != nil {
			t.Fatal(err)
		}
		if got := reachOf(t, s, declRef{"A", "A.X"}); got != 1 {
			t.Fatalf("a change to A.X changes %d statements; want U.u, whose A.X.wp belongs to it", got)
		}
		// A now declares A.X.wp itself: U's use is of that declaration, though U was not read again.
		leanModule{name: "A", decls: []string{"A.X", "A.X.wp"},
			src: "def A.X : Prop := True\ntheorem A.X.wp : True := trivial\n"}.write(t, dir)
		if _, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "two"}); err != nil {
			t.Fatal(err)
		}
		if got := reachOf(t, s, declRef{"A", "A.X"}); got != 0 {
			t.Errorf("a change to A.X changes %d statements; want none: A.X.wp is its own declaration now", got)
		}
		if got := reachOf(t, s, declRef{"A", "A.X.wp"}); got != 1 {
			t.Errorf("a change to A.X.wp changes %d statements; want U.u", got)
		}
		sameAsFullRefresh(t, s, tree, declRef{"A", "A.X"}, declRef{"A", "A.X.wp"}, declRef{"U", "U.u"})
	})

	t.Run("a landing declares it where the use cannot see it", func(t *testing.T) {
		dir := t.TempDir()
		tree := leanTree(t, dir,
			leanModule{name: "A", decls: []string{"A.X"}, src: "def A.X : Prop := True\n"},
			leanModule{name: "U", imports: []string{"A"}, decls: []string{"U.u"},
				src:  "theorem U.u (h : A.X.wp) : True := trivial\n",
				uses: []leanUse{{"A.X.wp", "A", "A.X.wp"}}})
		s := openStore(t)
		if _, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "one"}); err != nil {
			t.Fatal(err)
		}
		before := rowsOfOtherModules(t, s, "C")
		// C declares A.X.wp, but Lean recorded U's constant under A, which declares no A.X.wp: U uses A's.
		leanModule{name: "C", decls: []string{"A.X.wp"}, src: "theorem A.X.wp : True := trivial\n"}.write(t, dir)
		if _, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "two"}); err != nil {
			t.Fatal(err)
		}
		if got := reachOf(t, s, declRef{"A", "A.X"}); got != 1 {
			t.Errorf("a change to A.X changes %d statements; want U.u, whose A.X.wp is still A's", got)
		}
		if got := reachOf(t, s, declRef{"C", "A.X.wp"}); got != 0 {
			t.Errorf("a change to C's A.X.wp changes %d statements; want none", got)
		}
		// The landing made the refresh resolve U's edge again, and found the owner where it was: no row written.
		if after := rowsOfOtherModules(t, s, "C"); !slices.Equal(before, after) {
			t.Errorf("a resolution that moved no owner rewrote rows.\nbefore:\n%s\nafter:\n%s",
				strings.Join(before, "\n"), strings.Join(after, "\n"))
		}
		sameAsFullRefresh(t, s, tree, declRef{"A", "A.X"}, declRef{"C", "A.X.wp"}, declRef{"U", "U.u"})
	})

	t.Run("a landing changes the imports of a module in the user's closure", func(t *testing.T) {
		dir := t.TempDir()
		// A and Z both declare A.X. V's constant is one Lean made in V, so it belongs to the A.X of the first
		// module of V's import closure that declares one.
		tree := leanTree(t, dir,
			leanModule{name: "A", decls: []string{"A.X"}, src: "def A.X : Prop := True\n"},
			leanModule{name: "Z", decls: []string{"A.X"}, src: "def A.X : Prop := True\n"},
			leanModule{name: "W", imports: []string{"Z"}, decls: []string{"W.w"},
				src: "import Z\ndef W.w : Prop := True\n"},
			leanModule{name: "V", imports: []string{"W"}, decls: []string{"V.v"},
				src:  "theorem V.v (h : A.X.eq_1) : True := trivial\n",
				uses: []leanUse{{"A.X.eq_1", "V", "A.X.eq_1"}}})
		s := openStore(t)
		if _, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "one"}); err != nil {
			t.Fatal(err)
		}
		if got := reachOf(t, s, declRef{"Z", "A.X"}); got != 1 {
			t.Fatalf("a change to Z's A.X changes %d statements; want V.v", got)
		}
		if got := reachOf(t, s, declRef{"A", "A.X"}); got != 0 {
			t.Fatalf("a change to A's A.X changes %d statements; want none: V does not import A", got)
		}
		// W imports A as well: V's closure now holds A, which comes before Z, and V was not read again.
		leanModule{name: "W", imports: []string{"A", "Z"}, decls: []string{"W.w"},
			src: "import A\nimport Z\ndef W.w : Prop := True\n"}.write(t, dir)
		if _, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "two"}); err != nil {
			t.Fatal(err)
		}
		if got := reachOf(t, s, declRef{"A", "A.X"}); got != 1 {
			t.Errorf("a change to A's A.X changes %d statements; want V.v", got)
		}
		if got := reachOf(t, s, declRef{"Z", "A.X"}); got != 0 {
			t.Errorf("a change to Z's A.X changes %d statements; want none", got)
		}
		sameAsFullRefresh(t, s, tree, declRef{"A", "A.X"}, declRef{"Z", "A.X"}, declRef{"V", "V.v"})
	})

	t.Run("a landing gives a constant that no declaration owned an owner", func(t *testing.T) {
		dir := t.TempDir()
		tree := leanTree(t, dir,
			leanModule{name: "A", decls: []string{"A.X"}, src: "def A.X : Prop := True\n"},
			leanModule{name: "W", decls: []string{"W.w"}, src: "def W.w : Prop := True\n"},
			leanModule{name: "V", imports: []string{"W"}, decls: []string{"V.v"},
				src:  "theorem V.v (h : A.X.eq_1) : True := trivial\n",
				uses: []leanUse{{"A.X.eq_1", "V", "A.X.eq_1"}}})
		s := openStore(t)
		if _, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "one"}); err != nil {
			t.Fatal(err)
		}
		if got := reachOf(t, s, declRef{"A", "A.X"}); got != 0 {
			t.Fatalf("a change to A.X changes %d statements; want none: V's closure holds no A.X", got)
		}
		// W imports A: V's A.X.eq_1, which belonged to no declaration, belongs to A's A.X now.
		leanModule{name: "W", imports: []string{"A"}, decls: []string{"W.w"},
			src: "import A\ndef W.w : Prop := True\n"}.write(t, dir)
		if _, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "two"}); err != nil {
			t.Fatal(err)
		}
		if got := reachOf(t, s, declRef{"A", "A.X"}); got != 1 {
			t.Errorf("a change to A.X changes %d statements; want V.v", got)
		}
		sameAsFullRefresh(t, s, tree, declRef{"A", "A.X"}, declRef{"V", "V.v"})
	})
}
