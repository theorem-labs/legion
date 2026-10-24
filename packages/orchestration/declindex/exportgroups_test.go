package declindex

import (
	"context"
	"path/filepath"
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
	plan, err := exportGroups(ctx, s.q, []string{"K", "M", "N"}, []string{"H"})
	if err != nil {
		t.Fatal(err)
	}
	if groups, apart := plan.groups, plan.apart; len(groups) != 1 || !slices.Equal(groups[0], []string{"M", "N"}) || len(apart) != 1 || !slices.Equal(apart[0], []string{"K"}) {
		t.Errorf("forms importing H: read with the forms %v and without them %v; want one group [M N] with them (H imports A's and B's T), and K without", groups, apart)
	}
	plan, err = exportGroups(ctx, s.q, []string{"H"}, []string{"M"})
	if err != nil {
		t.Fatal(err)
	}
	if groups, apart := plan.groups, plan.apart; len(groups) != 1 || !slices.Equal(groups[0], []string{"H"}) || len(apart) != 0 {
		t.Errorf("forms importing M: read with the forms %v and without them %v; want H with them (H's own imports hold A's and B's T)", groups, apart)
	}
}
