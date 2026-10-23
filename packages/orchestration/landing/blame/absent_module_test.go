package blame

import (
	"context"
	"testing"
)

func TestReviewAbsentBaseModuleDoesNotBecomeBaseBroken(t *testing.T) {
	builder := moduleVerdictBuilder{results: map[string]BuildResult{
		"base:Alpha.New":     {Absent: true},
		"base+bad:Alpha.New": {Green: false},
	}}
	confirmation, err := ConfirmModules(context.Background(), "base", []string{"Alpha.New"}, []Member{{ID: "bad"}}, map[string][]string{"Alpha.New": {"bad"}}, candidateForTests, builder)
	if err != nil {
		t.Fatalf("ConfirmModules() error = %v", err)
	}
	if len(confirmation.BaseBroken) != 0 {
		t.Fatalf("base broken = %#v, want none", confirmation.BaseBroken)
	}
	if got, want := confirmation.ByModule["Alpha.New"], []Culprit{{Member: "bad", Kind: "evidence"}}; !sameCulprits(got, want) {
		t.Fatalf("culprits = %#v, want %#v", got, want)
	}
}

type moduleVerdictBuilder struct{ results map[string]BuildResult }

func (b moduleVerdictBuilder) Build(_ context.Context, tree Tree, modules []string) (BuildResult, error) {
	return b.results[string(tree)+":"+modules[0]], nil
}
