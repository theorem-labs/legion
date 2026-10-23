package blame

import (
	"context"
	"testing"
)

func TestReviewConfirmModulesBisectsUnexplainedModuleAfterAnotherConfirms(t *testing.T) {
	builder := moduleBuilder{red: map[string]bool{
		"base+first:Alpha.One":               true,
		"base+first+one+two:Alpha.Two":       true,
		"base+first+one+two+three:Alpha.Two": true,
	}}
	members := []Member{{ID: "first"}, {ID: "one"}, {ID: "two"}, {ID: "three"}}

	confirmation, err := ConfirmModules(context.Background(), "base", []string{"Alpha.One", "Alpha.Two"}, members, map[string][]string{"Alpha.One": {"first"}}, candidateForTests, builder)
	if err != nil {
		t.Fatalf("ConfirmModules() error = %v", err)
	}
	if got, want := confirmation.ByModule["Alpha.One"], []Culprit{{Member: "first", Kind: "evidence"}}; !sameCulprits(got, want) {
		t.Fatalf("first module = %#v, want %#v", got, want)
	}
	if got, want := confirmation.ByModule["Alpha.Two"], []Culprit{{Member: "two", Kind: "bisect"}}; !sameCulprits(got, want) {
		t.Fatalf("second module = %#v, want %#v", got, want)
	}
}

func TestReviewConfirmModulesMarksRedBaseWithoutBlaming(t *testing.T) {
	builder := moduleBuilder{red: map[string]bool{"base:Alpha.Base": true}}

	confirmation, err := ConfirmModules(context.Background(), "base", []string{"Alpha.Base"}, []Member{{ID: "one"}}, nil, candidateForTests, builder)
	if err != nil {
		t.Fatalf("ConfirmModules() error = %v", err)
	}
	if got, want := confirmation.BaseBroken, []string{"Alpha.Base"}; !sameStrings(got, want) {
		t.Fatalf("base broken = %#v, want %#v", got, want)
	}
	if len(confirmation.ByModule) != 0 {
		t.Fatalf("culprits = %#v, want none", confirmation.ByModule)
	}
}

type moduleBuilder struct{ red map[string]bool }

func (b moduleBuilder) Build(_ context.Context, tree Tree, modules []string) (BuildResult, error) {
	return BuildResult{Green: !b.red[string(tree)+":"+modules[0]]}, nil
}

func sameCulprits(got, want []Culprit) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}
