package conceptgate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/orchestration/declindex"
)

func TestRefreshNeverCompilesStoredAddedSource(t *testing.T) {
	g, _ := gateForTest(t)
	witness := filepath.Join(t.TempDir(), "stored-source-ran")
	source := "#eval IO.FS.writeFile \"" + witness + "\" \"\"\n\ndef Fx.Stored : Prop := True\n"
	concept := Concept{Name: "Fx.Stored", Home: "Stored.lean", Meaning: "A stored pending fact.", Proposal: "p", Proposer: "role", CompiledHash: "0123456789abcdef0123456789abcdef", Source: source}
	body, err := Render(concept)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(g.Registry, "concepts", Key(concept)+".yaml"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	reg, defects, err := Load(g.Registry)
	if err != nil || len(defects) != 0 {
		t.Fatalf("registry: %v, defects: %v", err, defects)
	}
	text, _ := reg.FormsModule().Source()
	if strings.Contains(text, source) || strings.Contains(text, "IO.FS.writeFile") {
		t.Fatalf("FormsModule.Source contains stored source:\n%s", text)
	}
	forms, err := Forms(context.Background(), g.Index, reg)
	if err != nil {
		t.Fatal(err)
	}
	text, _ = forms.Source()
	if strings.Contains(text, source) || strings.Contains(text, "IO.FS.writeFile") {
		t.Fatalf("refresh forms contain stored source:\n%s", text)
	}
	compiled := *g.Compiled
	compiled.Forms = forms
	compiled.Dir = t.TempDir()
	if _, err := g.Index.Refresh(context.Background(), g.Tree, declindex.RefreshOptions{Commit: gitHead(t, g.Tree.Source), Compiled: &compiled}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(witness); err == nil {
		t.Fatal("refresh executed stored source")
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
}
