package declindex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeModule writes a hand-made reference file and its source below dir.
func writeModule(t *testing.T, dir, rel, ilean, src string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p+".ilean", []byte(ilean), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p+".lean", []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A use of `A.X.wp` belongs to A.X while nothing declares `A.X.wp`. When a later landing adds a module that
// declares it, the use belongs to A.X.wp, though the module that uses it was not read again: an incremental
// refresh answers as a full one does.
func TestEdgeOwnersFollowDeclarationsOfLaterModules(t *testing.T) {
	dir := t.TempDir()
	writeModule(t, dir, "A", `{"version":5,"module":"A","decls":{"A.X":[0,0,0,19,0,4,0,7]},"references":{}}`,
		"def A.X : Prop := True\n")
	writeModule(t, dir, "U", `{"version":5,"module":"U","decls":{"U.u":[0,0,0,42,0,8,0,11]},`+
		`"references":{"{\"c\":{\"m\":\"A\",\"n\":\"A.X.wp\"}}":{"definition":null,"usages":[[0,17,0,23,"U.u"]]}}}`,
		"theorem U.u (h : A.X.wp) : True := trivial\n")
	tree := Tree{Source: dir, Lib: dir, Roots: Roots{"A": true, "U": true, "B": true}}
	path := filepath.Join(t.TempDir(), "index.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if _, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "one"}); err != nil {
		t.Fatal(err)
	}
	if a, err := s.Answer(ctx, "A.X", AnswerOptions{}); err != nil || a.Reach.StatementsChanged != 1 {
		t.Fatalf("before: %+v %v", a.Reach, err)
	}
	writeModule(t, dir, "B", `{"version":5,"module":"B","decls":{"A.X.wp":[0,0,0,24,0,8,0,14]},"references":{}}`,
		"theorem A.X.wp : True := trivial\n")
	if _, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "two"}); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if a, err := s2.Answer(ctx, "A.X", AnswerOptions{}); err != nil || a.Reach.StatementsChanged != 0 {
		t.Fatalf("A.X after A.X.wp is declared: %+v %v", a.Reach, err)
	}
	if a, err := s2.Answer(ctx, "A.X.wp", AnswerOptions{}); err != nil || a.Reach.StatementsChanged != 1 {
		t.Fatalf("A.X.wp: %+v %v", a.Reach, err)
	}
}

// A graph file that failed to be written after the index committed must not be read as current: queries refuse
// until a refresh writes it, and that refresh writes it though no module changed.
func TestAFailedGraphWriteIsRepairedAndNeverAnswered(t *testing.T) {
	tree := goldenTree(t)
	path := filepath.Join(t.TempDir(), "index.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "one"}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(tree.Lib, "Sub", "Wrap.ilean")); err != nil {
		t.Fatal(err)
	}
	// The graph's temporary file cannot be created: the write fails after the commit.
	if err := os.Mkdir(path+".graph.tmp", 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "two"}); err == nil {
		t.Fatal("a refresh whose graph write failed reported success")
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if a, err := s.Answer(ctx, "Gold.Frame", AnswerOptions{}); err == nil {
		t.Fatalf("answered from a stale graph: %+v", a.Reach)
	}
	if err := os.Remove(path + ".graph.tmp"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "two"}); err != nil {
		t.Fatal(err)
	}
	a, err := s.Answer(ctx, "Gold.Frame", AnswerOptions{})
	if err != nil || a.Reach.StatementsChanged != 2 {
		t.Fatalf("after the repair: %+v %v", a.Reach, err)
	}
	s.Close()
}

// A reference file older than its source records positions that no longer hold the declarations. Their
// statement hashes are unknown, never the hash of whatever text now sits at those positions.
func TestStaleReferenceFileGivesNoStatementHash(t *testing.T) {
	ilean, src, _ := readGolden(t)
	shifted := append([]byte("-- a line added after the build\n"), src...)
	got, err := ParseModule("Golden", ilean, shifted, Roots{"Golden": true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Stale == 0 {
		t.Fatal("no stale declaration reported")
	}
	for _, d := range got.Decls {
		if d.StmtHash != "" {
			t.Errorf("%s has hash %s from a source newer than its reference file", d.Name, d.StmtHash)
		}
	}
	fresh, err := ParseModule("Golden", ilean, src, Roots{"Golden": true})
	if err != nil || fresh.Stale != 0 {
		t.Fatalf("the built source is stale: %d %v", fresh.Stale, err)
	}
}

func TestLoadOwnersChecksTheConfiguredFormat(t *testing.T) {
	p := filepath.Join(t.TempDir(), "owners.json")
	body := `{"format":"graph/owners/v3","owners":[{"id":"function:frame_at","contract":"Sub"}]}`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if o, err := LoadOwners(p, "graph/owners/v3"); err != nil || o["Sub"] != "function:frame_at" {
		t.Fatalf("the configured format: %v %v", o, err)
	}
	if _, err := LoadOwners(p, ""); err != nil {
		t.Fatalf("no configured format: %v", err)
	}
	if _, err := LoadOwners(p, "graph/owners/v4"); err == nil || !strings.Contains(err.Error(), "graph/owners/v4") {
		t.Fatalf("another format was accepted: %v", err)
	}
}

// A use inside a declaration that Lean records but does not declare (an auxiliary such as `A.f.go`), of that
// same name, is a use of the declaration it belongs to: an edge from A.f to A.f.go.
func TestAUseOfItsOwnUndeclaredParentIsAnEdgeFromItsOwner(t *testing.T) {
	dir := t.TempDir()
	writeModule(t, dir, "A", `{"version":5,"module":"A","decls":{"A.f":[0,0,0,15,0,4,0,7]},`+
		`"references":{"{\"c\":{\"m\":\"A\",\"n\":\"A.f.go\"}}":{"definition":null,"usages":[[0,11,0,13,"A.f.go"]]}}}`,
		"def A.f := go\n")
	s := refreshed(t, Tree{Source: dir, Lib: dir, Roots: Roots{"A": true}})
	a, err := s.Answer(context.Background(), "A.f", AnswerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if a.Reach.StatementsChanged+a.Reach.ProofsToRedo != 1 {
		t.Fatalf("reach %+v, want the auxiliary", a.Reach)
	}
}

// Lean names the declarations of some commands itself (an elaborator's auxiliary, an option's initializer); their
// recorded selection is the command. They are not evidence that the source moved, and a `_root_.` name is its own.
func TestSelectionHoldsForNamesLeanMade(t *testing.T) {
	text := "/-- doc -/\nelab_rules : tactic | `(tactic| t) => pure ()\ndef _root_.A.b : Nat := 1\nregister_option x : Bool := {}\n"
	if !selectionHolds(text, "M._aux_M___elabRules_M_tacticT_1", 0, 10) {
		t.Error("an elaborator's auxiliary (internal name) was taken for a moved declaration")
	}
	at := strings.Index(text, "_root_")
	if !selectionHolds(text, "A.b", at, at+len("_root_.A.b")) {
		t.Error("a `_root_.` name was taken for a moved declaration")
	}
	if !selectionHolds(text, "_private.M.0.initFn._@.M.1._hyg.4", strings.Index(text, "register"), len(text)) {
		t.Error("an option's initializer (private hygienic name) was taken for a moved declaration")
	}
	if selectionHolds(text, "_private.M.0.A.c", at, at+len("_root_.A.b")) {
		t.Error("a private user declaration at another name's position was taken as current")
	}
	if selectionHolds(text, "A.z", at, at+len("_root_.A.b")) {
		t.Error("a declaration at another name's position was taken as current")
	}
}

// A module whose source moved since its build is unreadable: the refresh says so (exit 3 in the command), and
// the index refuses to answer until a build brings it back.
func TestRefreshCountsAModuleNewerThanItsBuildAsUnreadable(t *testing.T) {
	tree := goldenTree(t)
	src := filepath.Join(tree.Source, "Golden.lean")
	data, _ := os.ReadFile(src)
	if err := os.WriteFile(src, append([]byte("-- edited after the build\n"), data...), 0o644); err != nil {
		t.Fatal(err)
	}
	s := openStore(t)
	st, err := s.Refresh(context.Background(), tree, RefreshOptions{Commit: "one"})
	if err != nil {
		t.Fatal(err)
	}
	if st.Unreadable != 1 || st.Reread != 1 {
		t.Fatalf("stats %+v: want the moved module unreadable and the other read", st)
	}
	if n, _ := s.Meta(context.Background(), "errors"); n != "1" {
		t.Fatalf("errors meta %q", n)
	}
}
