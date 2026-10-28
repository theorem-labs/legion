package declindex

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

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

// A refresh from nothing writes every edge with its owner already resolved, and no edge of an older module exists
// to move, so it reads no edge back: on the indexed project (4.4 million edges) the read of every changed name over
// the edges ran for more than ten minutes and stalled the refresh. The table's read counters, read after the store
// closes (a backend reports its counters when it ends), say that no query read a row of `edges`. The delete of the
// rows of the modules about to be written scans the table, but it finds none.
func TestARefreshFromNothingReadsNoEdgeBack(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	tree := leanTree(t, dir,
		leanModule{name: "A", decls: []string{"A.X"}, src: "def A.X : Prop := True\n"},
		leanModule{name: "B", imports: []string{"A"}, decls: []string{"B.b"},
			src:  "theorem B.b (h : A.X.wp) : True := trivial\n",
			uses: []leanUse{{"A.X.wp", "A", "A.X.wp"}}},
		leanModule{name: "C", imports: []string{"B"}, decls: []string{"C.c"},
			src:  "theorem C.c (h : B.b) : True := trivial\n",
			uses: []leanUse{{"B.b", "B", "B.b"}}},
	)
	tg := indexTarget(t)
	s, err := Open(ctx, tg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "one"}); err != nil {
		_ = s.Close()
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, tg.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var seqRead, idxFetched, written int64
	// The counters of the closed backends are in the database's statistics a moment after they end.
	for range 50 {
		if err := conn.QueryRow(ctx, `SELECT coalesce(seq_tup_read, 0), coalesce(idx_tup_fetch, 0), coalesce(n_tup_ins, 0)
			FROM pg_stat_user_tables WHERE schemaname = $1 AND relname = 'edges'`, tg.Index).Scan(&seqRead, &idxFetched, &written); err == nil && written > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if written == 0 {
		t.Fatal("the statistics of the table edges show no row written: the test cannot tell")
	}
	if seqRead+idxFetched != 0 {
		t.Errorf("a refresh from nothing read %d rows of the table edges (%d by scan, %d by index); want none",
			seqRead+idxFetched, seqRead, idxFetched)
	}
}

// A landing that adds many declaration names (a new root's thousands of modules) reads the edges of the other modules
// at or below each name by a probe of edges_target for that name, not by a join filter over every row of edges
// (edgesAtOrBelowNames). With 20,000 names against 220,000 edges, Postgres 17 planned that join filter for the query
// as it was before, a plain join under UNION, and the query ran for 277 s. Postgres 16 probed edges_target for that
// query, so only a run on Postgres 17, the version the index runs on and orchestration-lean.yaml tests with, fails
// on it. The plan is read first, so that a join filter fails the test before the query runs. Then the query must
// find the rows of the other modules at and below each name, none of the rewritten module's and none merely next to
// a name, on either side of the range below it.
func TestAManyNamesLandingProbesTheEdgesOfEachName(t *testing.T) {
	ctx := context.Background()
	tg := indexTarget(t)
	s := openAt(t, tg)
	const names, filler = 20000, 200000
	var changed []string
	var rows [][]any
	want := map[constOwner]bool{}
	// edge adds a row of module that uses target, a constant of tmodule that is its own owner.
	edge := func(target, tmodule, module string) {
		rows = append(rows, []any{target, tmodule, "Old.d", module, true, "theorem", target, tmodule, false})
	}
	found := func(target string) {
		edge(target, "New", "Old")
		want[constOwner{declRef{"New", target}, owned{declRef{"New", target}, false}}] = true
	}
	for i := range filler {
		edge(fmt.Sprintf("Old.c%06d", i), "Old", "Old")
	}
	for j := range names {
		n := fmt.Sprintf("New.n%05d", j)
		changed = append(changed, n)
		switch j % 5 {
		case 0:
			found(n) // the name itself
		case 1:
			found(n + ".eq_1") // below the name
		case 2:
			edge(n+".eq_1", "New", "New") // below it, in the module the landing rewrote
		case 3:
			edge(n+"-z", "New", "Old") // next to the name, not below it: '-' sorts before '.'
		case 4:
			edge(n+"0.eq_1", "New", "Old") // past the range below it: '0' sorts after '/'
		}
	}
	conn, err := pgx.Connect(ctx, tg.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.CopyFrom(ctx, pgx.Identifier{tg.Index, "edges"}, edgeColumns, pgx.CopyFromRows(rows)); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, "ANALYZE "+pgx.Identifier{tg.Index, "edges"}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	rewritten := []string{"New"}
	var plan string
	if err := s.q.QueryRow(ctx, "EXPLAIN (FORMAT JSON) "+edgesAtOrBelowNames, changed, rewritten).Scan(&plan); err != nil {
		t.Fatal(err)
	}
	// planNode is one node of the plan, with the fields the test reads.
	type planNode struct {
		NodeType           string     `json:"Node Type"`
		ParentRelationship string     `json:"Parent Relationship"`
		IndexName          string     `json:"Index Name"`
		IndexCond          string     `json:"Index Cond"`
		JoinFilter         string     `json:"Join Filter"`
		Plans              []planNode `json:"Plans"`
	}
	var nodes []struct{ Plan planNode }
	if err := json.Unmarshal([]byte(plan), &nodes); err != nil {
		t.Fatalf("read the plan %q: %v", plan, err)
	}
	if len(nodes) != 1 {
		t.Fatalf("the plan holds %d statements, want 1:\n%s", len(nodes), plan)
	}
	// The inner side of a nested loop runs once for each row of its outer side, here once for each name. A
	// sequential scan there reads every row of a table for each name, as a join filter does, so the plan must read
	// edges_target there instead.
	probed := false
	var walk func(p planNode, inLoop bool)
	walk = func(p planNode, inLoop bool) {
		if p.JoinFilter != "" {
			t.Fatalf("the plan's %s joins rows under the join filter %s instead of probing edges_target:\n%s",
				p.NodeType, p.JoinFilter, plan)
		}
		if inLoop && p.NodeType == "Seq Scan" {
			t.Fatalf("the plan scans a whole table for each name instead of probing edges_target:\n%s", plan)
		}
		if inLoop && p.IndexName == "edges_target" && strings.Contains(p.IndexCond, ">=") {
			probed = true
		}
		for _, c := range p.Plans {
			walk(c, inLoop || p.NodeType == "Nested Loop" && c.ParentRelationship == "Inner")
		}
	}
	walk(nodes[0].Plan, false)
	if !probed {
		t.Fatalf("the plan probes edges_target with the range below a name for no name:\n%s", plan)
	}
	got, err := ownersToCheck(ctx, s.q, changed, nil, rewritten)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the edges at or below %d names: got %d constants, want %d", names, len(got), len(want))
	}
}

// An index that landings built answers reach as one built from nothing, whatever a landing changed about the
// declaration a constant belongs to: a declaration added where the use names it, one added where the use cannot
// see it, the imports of a module in the user's import closure, which no declaration change accompanies, and a
// declaration added above the constant's name and removed again.
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

	t.Run("a landing declares, then removes, a declaration above the constant the use names", func(t *testing.T) {
		dir := t.TempDir()
		use := leanModule{name: "U", imports: []string{"A"}, decls: []string{"U.u"},
			src:  "theorem U.u (h : A.X.Y.eq_1) : True := trivial\n",
			uses: []leanUse{{"A.X.Y.eq_1", "A", "A.X.Y.eq_1"}}}
		a := leanModule{name: "A", decls: []string{"A.X"}, src: "def A.X : Prop := True\n"}
		tree := leanTree(t, dir, a, use)
		s := openStore(t)
		if _, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "one"}); err != nil {
			t.Fatal(err)
		}
		if got := reachOf(t, s, declRef{"A", "A.X"}); got != 1 {
			t.Fatalf("a change to A.X changes %d statements; want U.u, whose A.X.Y.eq_1 belongs to it", got)
		}
		// A declares A.X.Y: U's A.X.Y.eq_1 lies below it, and belongs to it, though U was not read again.
		leanModule{name: "A", decls: []string{"A.X", "A.X.Y"},
			src: "def A.X : Prop := True\ntheorem A.X.Y : True := trivial\n"}.write(t, dir)
		if _, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "two"}); err != nil {
			t.Fatal(err)
		}
		if got := reachOf(t, s, declRef{"A", "A.X.Y"}); got != 1 {
			t.Errorf("after A declares A.X.Y, a change to it changes %d statements; want U.u", got)
		}
		sameAsFullRefresh(t, s, tree, declRef{"A", "A.X"}, declRef{"A", "A.X.Y"}, declRef{"U", "U.u"})
		// A no longer declares A.X.Y: the constant belongs to A.X again.
		a.write(t, dir)
		if _, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "three"}); err != nil {
			t.Fatal(err)
		}
		if got := reachOf(t, s, declRef{"A", "A.X"}); got != 1 {
			t.Errorf("after A.X.Y is gone, a change to A.X changes %d statements; want U.u", got)
		}
		sameAsFullRefresh(t, s, tree, declRef{"A", "A.X"}, declRef{"U", "U.u"})
	})
}
