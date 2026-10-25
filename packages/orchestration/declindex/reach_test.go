package declindex

import (
	"context"
	"encoding/json"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// referenceNames are the declarations of the reference tool's answers (testdata/golden/answers.json), in order.
func referenceNames(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "golden", "answers.json"))
	if err != nil {
		t.Fatal(err)
	}
	var answers map[string]json.RawMessage
	if err := json.Unmarshal(raw, &answers); err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(answers))
	for name := range answers {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// countingQuerier counts the queries that name the table it watches, and passes every query on.
type countingQuerier struct {
	q     querier
	table string
	n     *int
}

func (c countingQuerier) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if strings.Contains(sql, c.table) {
		*c.n++
	}
	return c.q.Query(ctx, sql, args...)
}

func (c countingQuerier) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if strings.Contains(sql, c.table) {
		*c.n++
	}
	return c.q.QueryRow(ctx, sql, args...)
}

// countEdgeQueries counts the queries of the edges table that f makes.
func countEdgeQueries(t *testing.T, s *Store, f func()) int {
	t.Helper()
	n := 0
	was := s.q
	s.q = countingQuerier{q: was, table: "edges", n: &n}
	defer func() { s.q = was }()
	f()
	return n
}

// An answer's reach is the closure of the edges out of a declaration, which the database walks in one query: a
// query for each level of the walk sends every edge of the level over the network, and the acceptance bar of
// `users` (95th percentile under 3 s) fails on the declarations that most of the tree uses. The golden tree
// reaches Gold2.FrameAt through Gold.Frame, so a walk of one query for each level makes more than one.
func TestReachReadsTheEdgesInOneQuery(t *testing.T) {
	s := refreshed(t, goldenTree(t))
	var got Answer
	n := countEdgeQueries(t, s, func() {
		var err error
		got, err = answerFor(t, s, "Gold.Aligned", AnswerOptions{})
		if err != nil {
			t.Fatal(err)
		}
	})
	if n != 1 {
		t.Errorf("an answer without --why read the edges in %d queries, want 1", n)
	}
	if got.Reach.StatementsChanged == 0 || got.Reach.AllDirectories == 0 {
		t.Fatalf("reach of Gold.Aligned is empty: %+v", got.Reach.ReachCounts)
	}
}

// The chains of --why need the walk's own order, so they come from a walk of the edges; its counts must be those
// of the one query, for every declaration the reference answers name.
func TestReachCountsAreTheSameWithAndWithoutWhy(t *testing.T) {
	s := refreshed(t, goldenTree(t))
	for _, name := range referenceNames(t) {
		plain, err := answerFor(t, s, name, AnswerOptions{})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		why, err := answerFor(t, s, name, AnswerOptions{Why: true})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if plain.Reach.ReachCounts != why.Reach.ReachCounts {
			t.Errorf("%s: reach %+v, with --why %+v", name, plain.Reach.ReachCounts, why.Reach.ReachCounts)
		}
	}
}

// Lean puts no bound on a declaration's name, and the index stores two names in one edge row: the user's and the
// owner's. An index of the two names together holds both in one B-tree entry, which Postgres refuses past 2,704
// bytes, so a refresh of a tree whose names are individually storable would fail with SQLSTATE 54000 and the
// whole landing with it. The longest edge of the copy's index takes 474 bytes for its four names, so this is a
// bound on what the index can hold, not on what it holds today.
func TestARefreshStoresAnEdgeOfTwoNamesThatOneIndexEntryCouldNotHold(t *testing.T) {
	// Postgres compresses what it puts in an index, so a name of one letter repeated would fit however long it
	// is. Lean's generated names are not of one letter: these are drawn from a fixed seed.
	rng := rand.New(rand.NewPCG(20261025, 577))
	long := func(prefix string) string {
		const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
		b := make([]byte, 1400)
		for i := range b {
			b[i] = alphabet[rng.IntN(len(alphabet))]
		}
		return prefix + string(b)
	}
	owner, user := long("A.o"), long("B.u")
	tree := leanTree(t, t.TempDir(),
		leanModule{name: "A", decls: []string{owner}, src: "def " + owner + " : Prop := True\n"},
		leanModule{name: "B", imports: []string{"A"}, decls: []string{user},
			src:  "theorem " + user + " (h : " + owner + ") : True := trivial\n",
			uses: []leanUse{{owner, "A", owner}}},
	)
	s := openStore(t)
	if _, err := s.Refresh(context.Background(), tree, RefreshOptions{Commit: "c0ffee"}); err != nil {
		t.Fatalf("a refresh of two names of %d bytes: %v", len(owner), err)
	}
	got, err := answerFor(t, s, owner, AnswerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Reach.StatementsChanged != 1 {
		t.Errorf("the statement that uses it changes %d statements, want 1: %+v", got.Reach.StatementsChanged, got.Reach.ReachCounts)
	}
}
