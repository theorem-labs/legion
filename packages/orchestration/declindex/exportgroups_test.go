package declindex

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
)

// A module whose imports declare a name twice in a way that the forms' imports already hold is not a clash: the
// forms' own environment imports both declarations, so adding the module adds nothing that Lean has not accepted.
// Lean imports two theorems of one name and one statement together, and a project can hold such a pair: on the
// project of the acceptance run, a module that the registry's forms import imports two modules that each declare
// one such theorem. exportGroups records one module per name for a closure (claims), so every module that imports
// the other one is read without the forms: 27,215 of 52,548 modules, and `states` finds the written form in 772 of
// the 1,824 files that state it, where an index without this rule found 1,789. Here H (the forms' import) imports A
// and B, which both declare the theorem T; M imports A and N imports B, so both go with the forms. C's declaration
// of T, a definition that the forms' imports do not hold, is a clash, and K, which imports it, stays apart.
func TestExportGroupsKeepsAModuleWhoseDuplicateTheFormsImportsAlreadyHold(t *testing.T) {
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
	groups, apart, _, err := exportGroups(ctx, s.q, []string{"K", "M", "N"}, []string{"H"})
	if err != nil {
		t.Fatal(err)
	}
	with := slices.Concat(groups...)
	slices.Sort(with)
	without := slices.Concat(apart...)
	if !slices.Equal(with, []string{"M", "N"}) || !slices.Equal(without, []string{"K"}) {
		t.Fatalf("read with the forms %v and without them %v; want M and N with them (H already imports A and B), and K without", with, without)
	}
}
