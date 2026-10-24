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
	s := openStore(t)
	ctx := context.Background()
	if _, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "base"}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := s.LandSnapshot(ctx, []string{"Gold.frame_lo", "Gold2.FrameAt", "Gold.Missing"})
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
