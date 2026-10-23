package landing

import (
	"context"
	"testing"
)

func TestCommandCheckerParsesCandidateFindings(t *testing.T) {
	checker := CommandChecker{
		CheckName: "policy",
		Command:   []string{"sh", "-c", "test \"$LAND_TREE\" = tree && printf 'path\\treason\\n'"},
	}

	findings, err := checker.Check(context.Background(), Tree("tree"))
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if got, want := findings, []Finding{{Path: "path", Reason: "reason"}}; !sameFindings(got, want) {
		t.Fatalf("Check() findings = %#v, want %#v", got, want)
	}
}

func TestCommandCheckerRejectsMalformedOutput(t *testing.T) {
	checker := CommandChecker{CheckName: "policy", Command: []string{"sh", "-c", "printf 'not-a-finding\\n'"}}

	if _, err := checker.Check(context.Background(), Tree("tree")); err == nil {
		t.Fatal("Check() error = nil, want malformed output error")
	}
}

func sameFindings(got, want []Finding) bool {
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
