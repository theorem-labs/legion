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
// reference_roots. Its edge has the constant as its owner, outside the import closures, also after a landing gives
// the project a declaration that the constant's name lies below, and a change of reference_roots re-reads every
// module though no reference file changed.
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
	if st, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "one"}); err != nil || st.Reread != 1 {
		t.Fatalf("adding reference_roots re-read %d modules, %v; want App", st.Reread, err)
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

	// The project declares Dep.T, which Dep.T.mk lies below: the landing re-resolves no owner of the dependency's.
	leanModule{name: "App.Ext", decls: []string{"Dep.T"}, src: "def Dep.T : Nat := 0\n"}.write(t, dir)
	if st, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "two"}); err != nil || st.Reread != 1 {
		t.Fatalf("the landing of App.Ext re-read %d modules, %v; want App.Ext", st.Reread, err)
	}
	if got := edgeOwners(t, s); !slices.Contains(got, want) {
		t.Fatalf("edges after App.Ext declared Dep.T:\n%s\nwant the line %q", strings.Join(got, "\n"), want)
	}

	if st, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "two"}); err != nil || st.Reread != 0 {
		t.Fatalf("a refresh with the same reference_roots re-read %d modules, %v; want none", st.Reread, err)
	}

	tree.ReferenceRoots = nil
	if st, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "two"}); err != nil || st.Reread != 2 {
		t.Fatalf("removing reference_roots re-read %d modules, %v; want App and App.Ext", st.Reread, err)
	}
	if got := usesOfTarget(t, s, "Dep.T.mk"); len(got) != 0 {
		t.Fatalf("after reference_roots was removed the index keeps uses of Dep.T.mk: %v", got)
	}
}

// A module whose reference file is unchanged keeps the uses its last read kept, so a change of roots re-reads every
// module: Lib's module is read while Lib is no root, and App's use of Lib.c is kept once Lib is one.
func TestRootsChangeRereadsEveryModule(t *testing.T) {
	dir := t.TempDir()
	tree := leanTree(t, dir,
		leanModule{name: "Lib", decls: []string{"Lib.c"}, src: "def Lib.c : Nat := 0\n"},
		leanModule{name: "App", imports: []string{"Lib"}, decls: []string{"App.k"},
			src:  "theorem App.k (h : Lib.c = Lib.c) : True := trivial\n",
			uses: []leanUse{{"Lib.c", "Lib", "Lib.c"}}},
	)
	delete(tree.Roots, "Lib")
	ctx := context.Background()
	s := openStore(t)
	if _, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "one"}); err != nil {
		t.Fatal(err)
	}
	if got := usesOfTarget(t, s, "Lib.c"); len(got) != 0 {
		t.Fatalf("with roots [App] the index keeps uses of Lib.c: %v", got)
	}
	tree.Roots["Lib"] = true
	if st, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "one"}); err != nil || st.Reread != 2 {
		t.Fatalf("adding the root Lib re-read %d modules, %v; want App and Lib", st.Reread, err)
	}
	if got := usesOfTarget(t, s, "Lib.c"); !slices.Equal(got, []string{"App.k of App: stmt"}) {
		t.Fatalf("uses of Lib.c with roots [App, Lib]: %v", got)
	}
	if v, err := s.Meta(ctx, "roots"); err != nil || v != "App,Lib" {
		t.Fatalf("meta roots = %q, %v; want App,Lib", v, err)
	}
	if st, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "one"}); err != nil || st.Reread != 0 {
		t.Fatalf("a refresh with the same roots re-read %d modules, %v; want none", st.Reread, err)
	}
}

// Several reference roots are recorded sorted, so a refresh with the same set re-reads nothing; the project's own
// uses stay kept and resolved through the imports beside the dependency's.
func TestReferenceRootsKeepProjectUsesAndRecordASortedSet(t *testing.T) {
	dir := t.TempDir()
	tree := leanTree(t, dir,
		leanModule{name: "App", decls: []string{"App.f", "App.g"},
			src:  "theorem App.f (h : Dep.T.mk = Lib.x) : True := trivial\ndef App.g : Nat := App.h\n",
			uses: []leanUse{{"Dep.T.mk", "Dep.Defs", "Dep.T.mk"}, {"Lib.x", "Lib.A", "Lib.x"}, {"App.h", "App", "App.h"}}},
	)
	tree.ReferenceRoots = Roots{"Lib": true, "Dep": true}
	ctx := context.Background()
	s := openStore(t)
	if _, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "one"}); err != nil {
		t.Fatal(err)
	}
	if v, err := s.Meta(ctx, "reference_roots"); err != nil || v != "Dep,Lib" {
		t.Fatalf("meta reference_roots = %q, %v; want Dep,Lib", v, err)
	}
	if got := usesOfTarget(t, s, "App.h"); !slices.Equal(got, []string{"App.g of App: proof"}) {
		t.Fatalf("uses of the project's App.h with reference_roots: %v", got)
	}
	got := edgeOwners(t, s)
	for _, want := range []string{
		"App.h of App used by App.g of App: App.h of App, by the imports",
		"Lib.x of Lib.A used by App.f of App: Lib.x of Lib.A",
	} {
		if !slices.Contains(got, want) {
			t.Fatalf("edges:\n%s\nwant the line %q", strings.Join(got, "\n"), want)
		}
	}
	if st, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "one"}); err != nil || st.Reread != 0 {
		t.Fatalf("a refresh with the same two reference roots re-read %d modules, %v; want none", st.Reread, err)
	}
}

// An index that an older declindex wrote records no roots: the read that kept its uses may have had other roots, so
// the first refresh by this one re-reads every module, also with the same roots and no reference roots, and records
// them; the next re-reads nothing.
func TestAnIndexWithoutRecordedRootsIsReread(t *testing.T) {
	dir := t.TempDir()
	tree := leanTree(t, dir,
		leanModule{name: "App", decls: []string{"App.g"}, src: "def App.g : Nat := App.h + Dep.c\n",
			uses: []leanUse{{"App.h", "App", "App.h"}, {"Dep.c", "Dep.Defs", "Dep.c"}}},
	)
	ctx := context.Background()
	s := openStore(t)
	if _, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "one"}); err != nil {
		t.Fatal(err)
	}
	execIndex(t, s, `DELETE FROM meta WHERE k IN ('roots', 'reference_roots', 'roots_generation')`)
	if st, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "one"}); err != nil || st.Reread != 1 {
		t.Fatalf("the first refresh of an index without recorded roots re-read %d modules, %v; want App", st.Reread, err)
	}
	if v, err := s.Meta(ctx, "roots"); err != nil || v != "App" {
		t.Fatalf("meta roots = %q, %v; want App", v, err)
	}
	if st, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "one"}); err != nil || st.Reread != 0 {
		t.Fatalf("the refresh after it re-read %d modules, %v; want none", st.Reread, err)
	}

	execIndex(t, s, `DELETE FROM meta WHERE k IN ('roots', 'reference_roots', 'roots_generation')`)
	tree.ReferenceRoots = Roots{"Dep": true}
	if st, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "one"}); err != nil || st.Reread != 1 {
		t.Fatalf("the first refresh with reference_roots of an index without recorded roots re-read %d modules, %v; want App", st.Reread, err)
	}
	if got := usesOfTarget(t, s, "Dep.c"); !slices.Equal(got, []string{"App.g of App: proof"}) {
		t.Fatalf("uses of Dep.c after the upgrade: %v", got)
	}
}

// A declindex older than reference_roots refuses a configuration that names them, but it opens an index of the
// same schema with a configuration that does not, re-reads a changed module keeping only its roots' uses, and
// leaves the meta keys as they were. The next refresh by this one finds that another writer wrote the index since
// its own last write and re-reads every module, so the dependency's uses come back.
func TestARefreshAfterAnOlderWriterRereadsEveryModule(t *testing.T) {
	dir := t.TempDir()
	app := leanModule{name: "App", decls: []string{"App.f"},
		src:  "theorem App.f (h : Dep.T.mk = Dep.T.mk) : True := trivial\n",
		uses: []leanUse{{"Dep.T.mk", "Dep.Defs", "Dep.T.mk"}}}
	tree := leanTree(t, dir, app,
		leanModule{name: "Other", decls: []string{"Other.k"}, src: "theorem Other.k (h : Dep.c = Dep.c) : True := trivial\n",
			uses: []leanUse{{"Dep.c", "Dep.Defs", "Dep.c"}}})
	tree.ReferenceRoots = Roots{"Dep": true}
	ctx := context.Background()
	s := openStore(t)
	if _, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "one"}); err != nil {
		t.Fatal(err)
	}
	if st, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "one"}); err != nil || st.Reread != 0 {
		t.Fatalf("a second refresh by this declindex re-read %d modules, %v; want none", st.Reread, err)
	}
	before := map[string]string{}
	for _, k := range []string{"roots", "reference_roots", "roots_generation"} {
		v, err := s.Meta(ctx, k)
		if err != nil {
			t.Fatal(err)
		}
		before[k] = v
	}

	// The older writer: App's build changed, and its refresh re-reads App alone and keeps no use of Dep's
	// constants. This declindex stands in for it: the reference_roots it would not compare are made its own
	// beforehand, and the meta keys it would not write are put back afterwards, so that only the generation
	// records its write.
	app.src = "theorem App.f (h : Dep.T.mk = Dep.T.mk) : True := True.intro\n"
	app.write(t, dir)
	older := tree
	older.ReferenceRoots = nil
	execIndex(t, s, `UPDATE meta SET v = '' WHERE k = 'reference_roots'`)
	if st, err := s.Refresh(ctx, older, RefreshOptions{Commit: "two"}); err != nil || st.Reread != 1 {
		t.Fatalf("the older writer's refresh re-read %d modules, %v; want App", st.Reread, err)
	}
	for k, v := range before {
		execIndex(t, s, `UPDATE meta SET v = $2 WHERE k = $1`, k, v)
	}
	if got := usesOfTarget(t, s, "Dep.T.mk"); len(got) != 0 {
		t.Fatalf("the older writer kept uses of Dep.T.mk: %v", got)
	}
	if got := usesOfTarget(t, s, "Dep.c"); len(got) != 1 {
		t.Fatalf("the older writer re-read Other: uses of Dep.c %v", got)
	}

	if st, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "two"}); err != nil || st.Reread != 2 {
		t.Fatalf("the refresh after the older writer re-read %d modules, %v; want App and Other", st.Reread, err)
	}
	if got := usesOfTarget(t, s, "Dep.T.mk"); !slices.Equal(got, []string{"App.f of App: stmt"}) {
		t.Fatalf("uses of Dep.T.mk after the older writer: %v", got)
	}
	if got := usesOfTarget(t, s, "Dep.c"); !slices.Equal(got, []string{"Other.k of Other: stmt"}) {
		t.Fatalf("uses of Dep.c after the older writer: %v", got)
	}
	if st, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "two"}); err != nil || st.Reread != 0 {
		t.Fatalf("the refresh after that re-read %d modules, %v; want none", st.Reread, err)
	}
}
