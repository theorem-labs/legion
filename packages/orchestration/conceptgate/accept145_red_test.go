package conceptgate

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sjawhar/legion/orchestration/declindex"
)

// Accept145 (INFRA-145 acceptance, E8): orchestration-lean.yaml sets DECLINDEX_REQUIRE_SANDBOX=1 with the comment
// "The gate compiles proposals only in the Landlock and seccomp sandbox; here its tests must run, not skip", but
// the gate's Lean tests (this package's requireSandboxTest, cmd/declindex's gate and recheck tests) skip unless
// DECLINDEX_SANDBOX_TEST=1, which no workflow sets. So CI's `lean` job passes without running them (conceptgate
// took 0.024 s in run 40030886828). Where the sandbox is required, the sandboxed gate tests must not skip.
func TestRequiredSandboxRunsTheGateTests(t *testing.T) {
	if os.Getenv("DECLINDEX_REQUIRE_SANDBOX") == "" {
		t.Skip("the sandbox is not required here")
	}
	if os.Getenv("DECLINDEX_SANDBOX_TEST") != "1" {
		t.Fatal("DECLINDEX_REQUIRE_SANDBOX is set, but the gate's sandboxed Lean tests skip: they run only with DECLINDEX_SANDBOX_TEST=1")
	}
}

// Accept145 (INFRA-145 acceptance, E2): a librarian concept with forms and no stored source states its fact through
// its forms; the index need not declare its name (README: "A concept with no form is matched by its declaration's
// body", so one with forms is not). Forms kept only concepts the index declares, so a librarian concept with 18
// forms that no module declares left an empty forms module, and `declindex states` for it found 0 of the 1,824
// files that state its fact (1,789 with the format-1 registry). Run infra145-e2-20261023T2236Z.
func TestFormsKeepsUndeclaredConceptsForms(t *testing.T) {
	reg, defects, err := Load(filepath.Join("testdata", "registry"))
	if err != nil || len(defects) != 0 {
		t.Fatalf("load: %v %v", err, defects)
	}
	s, err := declindex.Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	fm, err := Forms(context.Background(), s, reg)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range fm.Concepts {
		if c.Concept == "Fx.SpAligned" && len(c.Forms) == 1 {
			return
		}
	}
	t.Fatalf("the forms module lost Fx.SpAligned's form; concepts %+v", fm.Concepts)
}
