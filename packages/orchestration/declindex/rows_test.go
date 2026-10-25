package declindex

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"slices"
	"testing"
)

func TestDottedPrefixesYieldTheNameThenEachShorterPrefix(t *testing.T) {
	for name, want := range map[string][]string{
		"A.B.c": {"A.B.c", "A.B", "A"},
		"A":     {"A"},
		"":      nil,
		".x":    {".x"},
		"a..b":  {"a..b", "a.", "a"},
	} {
		if got := slices.Collect(dottedPrefixes(name)); !reflect.DeepEqual(got, want) {
			t.Errorf("dottedPrefixes(%q) = %q, want %q", name, got, want)
		}
	}
}

type countingQuerier struct {
	q     querier
	sizes []int
}

func (c *countingQuerier) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	c.sizes = append(c.sizes, len(args))
	return c.q.QueryContext(ctx, query, args...)
}

func TestEachRowInRunsOneQueryPerChunkAndNoneWithoutValues(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	for _, n := range []int{0, 1, maxInParams, maxInParams + 1, 2*maxInParams + 1} {
		names := make([]string, n)
		for i := range n {
			names[i] = fmt.Sprintf("N%d", i)
		}
		c := &countingQuerier{q: s.db}
		rows := 0
		err := eachRowIn(ctx, c, `SELECT name FROM decls WHERE name IN`, names, func(*sql.Rows) error {
			rows++
			return nil
		})
		if err != nil || rows != 0 {
			t.Fatalf("%d names: %d rows, %v", n, rows, err)
		}
		var want []int
		for left := n; left > 0; left -= maxInParams {
			want = append(want, min(left, maxInParams))
		}
		if !reflect.DeepEqual(c.sizes, want) {
			t.Errorf("%d names ran queries with %v parameters, want %v", n, c.sizes, want)
		}
	}
}
