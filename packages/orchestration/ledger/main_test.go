package ledger_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/sjawhar/legion/orchestration/internal/testpg"
	"github.com/sjawhar/legion/orchestration/ledger"
)

// newStore opens a ledger on a fresh database with one repository, "r", allowing three landing rounds.
func newStore(t *testing.T) *ledger.Store {
	t.Helper()
	s, _ := newStoreDSN(t)
	return s
}

func newStoreDSN(t *testing.T) (*ledger.Store, string) {
	t.Helper()
	ctx := context.Background()
	dsn := testpg.Database(t, "ledger")
	s, err := ledger.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	if _, err := s.EnsureRepo(ctx, "r", 3); err != nil {
		t.Fatal(err)
	}
	return s, dsn
}

func wantRefusal(t *testing.T, err error, reason string) *ledger.Refusal {
	t.Helper()
	var r *ledger.Refusal
	if !errors.As(err, &r) {
		t.Fatalf("want refusal %q, got %v", reason, err)
	}
	if r.Reason != reason {
		t.Fatalf("want refusal %q, got %q: %v", reason, r.Reason, r)
	}
	return r
}

// must returns v, failing the test (by panic, so it works in any expression) when err is set.
func must[T any](v T, err error) T {
	if err != nil {
		panic(fmt.Sprintf("unexpected error: %v", err))
	}
	return v
}
