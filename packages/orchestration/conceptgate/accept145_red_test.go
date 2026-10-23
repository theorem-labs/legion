package conceptgate

import (
	"os"
	"testing"
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
