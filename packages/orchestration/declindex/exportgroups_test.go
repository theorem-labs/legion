package declindex

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
)

// Lean imports two declarations of one name together when one subsumes the other (two theorems with one statement),
// so a module whose import closure holds a declaration of a name that the forms' closure also holds adds nothing that
// Lean has not accepted: every declaration of that name in the two closures loads together. A theorem T is declared
// in A and in B, and a definition T in C. H imports A and B; M imports A, N imports B, K imports C.
//   - With the forms importing H, M and N are read with the forms, in one group; K, whose T the forms' closure does
//     not hold, is read without them.
//   - With the forms importing M (A's T only), H is read with them: its own closure already holds A's and B's T.
func TestExportGroupsReadsWithTheFormsADuplicateThatTheFormsImportsHold(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, d := range [][]string{
		{"T", "A", "theorem", "h1"}, {"T", "B", "theorem", "h1"}, {"T", "C", "def", "h2"},
		{"H.x", "H", "def", "h3"}, {"M.y", "M", "def", "h4"}, {"N.y", "N", "def", "h5"}, {"K.y", "K", "def", "h6"},
	} {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO decls (name, short, module, kind, line, stmt_hash) VALUES (?, ?, ?, ?, 1, ?)`,
			d[0], d[0], d[1], d[2], d[3]); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range [][2]string{{"H", "A"}, {"H", "B"}, {"M", "A"}, {"N", "B"}, {"K", "C"}} {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO imports (module, imported) VALUES (?, ?)`, e[0], e[1]); err != nil {
			t.Fatal(err)
		}
	}
	plan, err := exportGroups(ctx, s.q, []string{"K", "M", "N"}, []string{"H"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if groups, apart := plan.groups, plan.apart; len(groups) != 1 || !slices.Equal(groups[0], []string{"M", "N"}) || len(apart) != 1 || !slices.Equal(apart[0], []string{"K"}) {
		t.Errorf("forms importing H: read with the forms %v and without them %v; want one group [M N] with them (H imports A's and B's T), and K without", groups, apart)
	}
	plan, err = exportGroups(ctx, s.q, []string{"H"}, []string{"M"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if groups, apart := plan.groups, plan.apart; len(groups) != 1 || !slices.Equal(groups[0], []string{"H"}) || len(apart) != 0 {
		t.Errorf("forms importing M: read with the forms %v and without them %v; want H with them (H's own imports hold A's and B's T)", groups, apart)
	}
}

// indexWith is an index whose decls are decls (name, module, kind) and whose imports are imports (module, imported),
// in that order.
func indexWith(t *testing.T, decls [][3]string, imports [][2]string) *Store {
	t.Helper()
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	for _, d := range decls {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO decls (name, short, module, kind, line, stmt_hash) VALUES (?, ?, ?, ?, 1, '')`,
			d[0], d[0], d[1], d[2]); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range imports {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO imports (module, imported) VALUES (?, ?)`, e[0], e[1]); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// Two axioms of one name load together only when Lean finds the type a proposition by a cheap check, which it does
// not for `∀ (p : Nat → Prop) (n : Nat), p n → p n`; a theorem of the same type subsumes each of them. So Home, which
// imports the theorem first and then A's and B's axiom `dup`, builds, but an environment that imports A and B before
// the theorem does not, though each of A's and B's declarations is one that Home's imports hold. Where two axioms
// meet, a module joins a group only with the same declarations of the name, unless Lean imports the two axioms'
// modules together, which it does not here (no Lean). One axiom with the theorem is safe in any order.
func TestExportGroupsKeepsApartTwoAxiomsThatATheoremReconciles(t *testing.T) {
	ctx := context.Background()
	decls := [][3]string{{"dup", "A", "axiom"}, {"dup", "B", "axiom"}, {"dup", "Thm", "theorem"}}
	s := indexWith(t, decls, [][2]string{{"Home", "Thm"}, {"Home", "A"}, {"Home", "B"}})
	plan, err := exportGroups(ctx, s.q, []string{"A", "B"}, []string{"Home"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.groups) != 0 || !slices.EqualFunc(plan.apart, [][]string{{"A"}, {"B"}}, slices.Equal) {
		t.Errorf("forms importing Home: read with the forms %v and without them %v; want A and B each alone without them", plan.groups, plan.apart)
	}
	s = indexWith(t, decls, [][2]string{{"Home", "B"}, {"Home", "Thm"}, {"Home", "A"}})
	if plan, err = exportGroups(ctx, s.q, []string{"A", "Home"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if !slices.EqualFunc(plan.groups, [][]string{{"A"}, {"Home"}}, slices.Equal) || len(plan.apart) != 0 {
		t.Errorf("no forms: groups %v, apart %v; want A and Home in two groups", plan.groups, plan.apart)
	}
	s = indexWith(t, [][3]string{{"dup", "A", "axiom"}, {"dup", "Thm", "theorem"}}, [][2]string{{"Home", "Thm"}, {"Home", "A"}})
	if plan, err = exportGroups(ctx, s.q, []string{"A"}, []string{"Home"}, nil); err != nil {
		t.Fatal(err)
	}
	if !slices.EqualFunc(plan.groups, [][]string{{"A"}}, slices.Equal) || len(plan.apart) != 0 {
		t.Errorf("one axiom and the theorem: read with the forms %v and without them %v; want A with them", plan.groups, plan.apart)
	}
}

// Two axioms of one name whose type Lean finds a proposition by its cheap check (`axiom dup : True` in A and in B)
// import together in any order. Where two sets of declarations hold two axioms between them and differ, the planner
// asks Lean whether each pair of the axioms' modules imports together (a module that declares a name imports no other
// declaration of it, so the pair meets exactly those two), and joins the sets when Lean imports every pair. Home, which
// the forms import, imports A; UseB imports B.
func TestExportGroupsAsksLeanWhetherTwoAxiomsImportTogether(t *testing.T) {
	ctx := context.Background()
	s := indexWith(t, [][3]string{{"dup", "A", "axiom"}, {"dup", "B", "axiom"}}, [][2]string{{"Home", "A"}, {"UseB", "B"}})
	for _, c := range []struct {
		lean        bool
		with, apart [][]string
	}{{true, [][]string{{"UseB"}}, nil}, {false, nil, [][]string{{"UseB"}}}} {
		var asked [][]string
		loads := func(_ context.Context, pair []string) (bool, error) {
			asked = append(asked, pair)
			return c.lean, nil
		}
		plan, err := exportGroups(ctx, s.q, []string{"UseB"}, []string{"Home"}, loads)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.EqualFunc(plan.groups, c.with, slices.Equal) || !slices.EqualFunc(plan.apart, c.apart, slices.Equal) {
			t.Errorf("Lean imports A and B together: %v; read with the forms %v and without them %v, want %v and %v",
				c.lean, plan.groups, plan.apart, c.with, c.apart)
		}
		if !slices.ContainsFunc(asked, func(p []string) bool { return slices.Equal(p, []string{"A", "B"}) }) {
			t.Errorf("Lean imports A and B together: %v; the planner asked Lean about %v, want the pair [A B]", c.lean, asked)
		}
	}
}

// A module whose reference file can no longer be read leaves the index with its compiled rows and its clashes with
// the forms, so that no later refresh reports a module that the index no longer holds.
func TestAModuleThatBecomesUnreadableLeavesNoCompiledRows(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	lib := filepath.Join(dir, "lib")
	ilean := filepath.Join(lib, "M.ilean")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ilean, []byte(`{"version":5,"module":"M","directImports":[],"decls":{},"references":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(filepath.Join(dir, "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tree := Tree{Source: dir, Lib: lib, Roots: Roots{"M": true}}
	if st, err := s.Refresh(ctx, tree, RefreshOptions{Jobs: 1}); err != nil || st.Reread != 1 {
		t.Fatalf("first refresh: %+v, %v", st, err)
	}
	for _, q := range []string{
		`INSERT INTO consts (name, module, kind, type, cstmt) VALUES ('M.x', 'M', 'def', 'Nat', 'h')`,
		`INSERT INTO without_forms (module, name, declarer, forms) VALUES ('M', 'dup', 'A', 0), ('M', 'dup', 'B', 1)`,
	} {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(ilean, []byte(`{broken`), 0o644); err != nil {
		t.Fatal(err)
	}
	if st, err := s.Refresh(ctx, tree, RefreshOptions{Jobs: 1}); err != nil || st.Unreadable != 1 {
		t.Fatalf("refresh of the broken reference file: %+v, %v; want one unreadable module", st, err)
	}
	clashes, err := s.WithoutForms(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var consts int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM consts WHERE module = 'M'`).Scan(&consts); err != nil {
		t.Fatal(err)
	}
	if len(clashes) != 0 || consts != 0 {
		t.Errorf("after M became unreadable: clashes %+v and %d compiled constants of M; want none", clashes, consts)
	}
}

// Grouping copies each module's sets of declarers once: when N modules declare one theorem and one module imports
// them all, the bytes that exportGroups allocates grow about linearly in N, not as N².
func TestExportGroupsAllocatesLinearlyInTheDeclarersOfAName(t *testing.T) {
	ctx := context.Background()
	allocated := func(n int) uint64 {
		decls := make([][3]string, n)
		imports := make([][2]string, n)
		for i := range n {
			m := fmt.Sprintf("M%05d", i)
			decls[i], imports[i] = [3]string{"dup", m, "theorem"}, [2]string{"H", m}
		}
		s := indexWith(t, decls, imports)
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		if _, err := exportGroups(ctx, s.q, []string{"H"}, []string{"H"}, nil); err != nil {
			t.Fatal(err)
		}
		runtime.ReadMemStats(&after)
		return after.TotalAlloc - before.TotalAlloc
	}
	small, large := allocated(2048), allocated(4096)
	if large > 3*small {
		t.Errorf("exportGroups allocated %d bytes for 2048 declarers of one name and %d for 4096; want about twice", small, large)
	}
}
