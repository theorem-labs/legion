package conceptgate

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sjawhar/legion/orchestration/declindex"
	"github.com/sjawhar/legion/orchestration/internal/indextest"
)

// orchestration-lean.yaml sets DECLINDEX_REQUIRE_SANDBOX=1 ("The gate compiles proposals only in the Landlock and
// seccomp sandbox; here its tests must run, not skip"). The gate's Lean tests (this package's requireSandboxTest,
// cmd/declindex's gate and recheck tests) skip unless DECLINDEX_SANDBOX_TEST=1, so wherever the sandbox is
// required, that variable must be set too, or the sandboxed gate tests would pass by skipping.
func TestRequiredSandboxRunsTheGateTests(t *testing.T) {
	if os.Getenv("DECLINDEX_REQUIRE_SANDBOX") == "" {
		t.Skip("the sandbox is not required here")
	}
	if os.Getenv("DECLINDEX_SANDBOX_TEST") != "1" {
		t.Fatal("DECLINDEX_REQUIRE_SANDBOX is set, but the gate's sandboxed Lean tests skip: they run only with DECLINDEX_SANDBOX_TEST=1")
	}
}

// A librarian concept with forms and no stored source states its fact through its forms; the index need not
// declare its name (README: "A concept with no form is matched by its declaration's body", so one with forms is
// not). Forms keeps such a concept's forms, so that `declindex states` finds the statements that state its fact
// even when no module declares the concept.
func TestFormsKeepsUndeclaredConceptsForms(t *testing.T) {
	reg, defects, err := Load(filepath.Join("testdata", "registry"))
	if err != nil || len(defects) != 0 {
		t.Fatalf("load: %v %v", err, defects)
	}
	s, err := declindex.Open(context.Background(), declindex.Target{DSN: indextest.DSN(t), Index: "idx"})
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
