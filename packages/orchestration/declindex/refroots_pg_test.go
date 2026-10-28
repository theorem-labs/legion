package declindex

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// usesOfTarget is every use row of the index whose constant is target, as "user of module: class".
func usesOfTarget(t *testing.T, s *Store, target string) []string {
	t.Helper()
	uses, err := s.UsesOf(context.Background(), []string{target})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, u := range uses {
		out = append(out, u.Parent+" of "+u.Module+": "+u.Class)
	}
	slices.Sort(out)
	return out
}

// A use of a dependency's constant is kept only when the configuration names the dependency's root in
// reference_roots. Its edge has the constant as its owner, outside the import closures, and a change of
// reference_roots re-reads every module though no reference file changed.
func TestReferenceRootsKeepUsesOfADependency(t *testing.T) {
	dir := t.TempDir()
	tree := leanTree(t, dir,
		leanModule{name: "App", decls: []string{"App.f", "App.g"},
			src:  "theorem App.f (h : Dep.T.mk = Dep.T.mk) : True := trivial\ndef App.g : Nat := App.h\n",
			uses: []leanUse{{"Dep.T.mk", "Dep.Defs", "Dep.T.mk"}, {"App.h", "App", "App.h"}}},
	)
	ctx := context.Background()
	s := openStore(t)
	if _, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "one"}); err != nil {
		t.Fatal(err)
	}
	if got := usesOfTarget(t, s, "Dep.T.mk"); len(got) != 0 {
		t.Fatalf("without reference_roots the index keeps uses of Dep.T.mk: %v", got)
	}

	tree.ReferenceRoots = Roots{"Dep": true}
	st, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "one"})
	if err != nil {
		t.Fatal(err)
	}
	if st.Reread != 1 {
		t.Fatalf("adding reference_roots re-read %d modules; want App", st.Reread)
	}
	if got := usesOfTarget(t, s, "Dep.T.mk"); !slices.Equal(got, []string{"App.f of App: stmt"}) {
		t.Fatalf("uses of Dep.T.mk with reference_roots [Dep]: %v", got)
	}
	want := "Dep.T.mk of Dep.Defs used by App.f of App: Dep.T.mk of Dep.Defs"
	if got := edgeOwners(t, s); !slices.Contains(got, want) {
		t.Fatalf("edges:\n%s\nwant the line %q (its own owner, not by the imports)", strings.Join(got, "\n"), want)
	}
	if v, err := s.Meta(ctx, "reference_roots"); err != nil || v != "Dep" {
		t.Fatalf("meta reference_roots = %q, %v; want Dep", v, err)
	}

	if st, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "one"}); err != nil || st.Reread != 0 {
		t.Fatalf("a refresh with the same reference_roots re-read %d modules, %v; want none", st.Reread, err)
	}

	tree.ReferenceRoots = nil
	if st, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "one"}); err != nil || st.Reread != 1 {
		t.Fatalf("removing reference_roots re-read %d modules, %v; want App", st.Reread, err)
	}
	if got := usesOfTarget(t, s, "Dep.T.mk"); len(got) != 0 {
		t.Fatalf("after reference_roots was removed the index keeps uses of Dep.T.mk: %v", got)
	}
}
