package declindex

import (
	"context"
	"reflect"
	"testing"
)

// TestLandSnapshotCarriesSourcesModulesAndRequestedDeclarations catches a hasher snapshot that mixes rows from
// different refreshes or cannot identify a declaration's source file.
func TestLandSnapshotCarriesSourcesModulesAndRequestedDeclarations(t *testing.T) {
	tree := goldenTree(t)
	gitIn(t, tree.Source, "init", "-q")
	gitIn(t, tree.Source, "add", "-A")
	gitIn(t, tree.Source, "commit", "-q", "-m", "base")
	tg := indexTarget(t)
	s := openAt(t, tg)
	ctx := context.Background()
	if _, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "base"}); err != nil {
		t.Fatal(err)
	}
	reader, err := OpenReadOnly(ctx, tg, IndexOnly)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	snapshot, err := reader.LandSnapshot(ctx, []string{"Gold.frame_lo", "Gold2.FrameAt", "Gold.Missing"})
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.HasSources {
		t.Fatal("snapshot does not mark git sources available")
	}
	if got, want := snapshot.Sources["Golden.lean"], gitIn(t, tree.Source, "hash-object", "--", "Golden.lean"); got != want {
		t.Errorf("Golden.lean blob = %q, want %q", got, want)
	}
	if want := map[string]bool{"Golden": true, "Sub/Wrap": true}; !reflect.DeepEqual(snapshot.Modules, want) {
		t.Errorf("modules = %v, want %v", snapshot.Modules, want)
	}
	decls := snapshot.Declarations["Gold2.FrameAt"]
	if len(decls) != 1 || decls[0].Module != "Sub.Wrap" || decls[0].Path != "Sub/Wrap.lean" || decls[0].StmtHash == "" {
		t.Errorf("Gold2.FrameAt = %+v, want one hashed declaration of module Sub.Wrap in Sub/Wrap.lean", decls)
	}
	if got := snapshot.Declarations["Gold.Missing"]; len(got) != 0 {
		t.Errorf("Gold.Missing = %+v, want none", got)
	}
}

// The ls-tree reader refuses a record it cannot read, as the ls-files reader does, and skips only what is not a
// `.lean` blob below the tree's prefix.
func TestTreeSourcesRefusesAMalformedRecord(t *testing.T) {
	good := "100644 blob 1111111111111111111111111111111111111111\tsub/A.lean\x00" +
		"040000 tree 2222222222222222222222222222222222222222\tsub/Dir\x00" +
		"100644 blob 3333333333333333333333333333333333333333\tsub/README.md\x00" +
		"100644 blob 4444444444444444444444444444444444444444\tother/B.lean\x00"
	got, err := treeSources([]byte(good), "sub/")
	if err != nil || !reflect.DeepEqual(got, map[string]string{"A.lean": "1111111111111111111111111111111111111111"}) {
		t.Fatalf("well-formed records: %v, %v", got, err)
	}
	for _, bad := range []string{
		"100644 blob\tsub/A.lean\x00",
		"100644 blob 1111111111111111111111111111111111111111 extra\tsub/A.lean\x00",
	} {
		if got, err := treeSources([]byte(bad), "sub/"); err == nil {
			t.Errorf("record %q was read as %v; want it refused", bad, got)
		}
	}
}
