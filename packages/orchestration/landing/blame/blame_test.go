package blame

import (
	"context"
	"strings"
	"testing"
)

func TestParseFindsRedModulesAndErrorBlocks(t *testing.T) {
	log := strings.Join([]string{
		"✖ [1/4] Building Alpha.Beta",
		"error: Alpha/Beta.lean:8:4: Unknown identifier `missing`",
		"context line",
		"✖ [2/4] Building Beta.Gamma",
		"error: Beta/Gamma.lean:3:1: import Alpha.Beta failed",
		"✔ [3/4] Built Other.Module",
	}, "\n")

	report := Parse(log, []string{"Alpha", "Beta"})
	if got, want := report.RedModules, []string{"Alpha.Beta", "Beta.Gamma"}; !sameStrings(got, want) {
		t.Fatalf("Parse() red modules = %#v, want %#v", got, want)
	}
	if got, want := len(report.Blocks), 2; got != want {
		t.Fatalf("Parse() blocks = %d, want %d", got, want)
	}
	if got, want := report.Blocks[0].Module, "Alpha.Beta"; got != want {
		t.Fatalf("first block module = %q, want %q", got, want)
	}
}

func TestNamesRecognizeBuildErrorShapes(t *testing.T) {
	block := strings.Join([]string{
		"Unknown identifier `gone`",
		"environment already contains 'duplicate'",
		"`field` is not a field of structure `Record`",
		"failed to synthesize Instance",
		"type expected, got",
		"  Function.head argument",
		"Fields missing: `left`, `right`",
	}, "\n")

	got := Names(block)
	want := []Name{
		{Value: "gone", Kind: Unknown},
		{Value: "duplicate", Kind: Added},
		{Value: "Record", Kind: Changed},
		{Value: "Instance", Kind: Changed},
		{Value: "Function.head", Kind: Changed},
		{Value: "Record.left", Kind: Unknown},
		{Value: "Record.right", Kind: Unknown},
		{Value: "Function", Kind: Weak},
	}
	if !sameNames(got, want) {
		t.Fatalf("Names() = %#v, want %#v", got, want)
	}
}

func TestIndexDiffRecordsDeclarationEvidence(t *testing.T) {
	diff := strings.Join([]string{
		"@@ -1 +1 @@ def Changed",
		"-def Removed := 0",
		"+private def Hidden := 0",
		"-structure Record where",
		"-  left : Nat",
		"+structure Record where",
		"+  right : Nat",
		"-namespace Closed",
	}, "\n")

	evidence := IndexDiff(diff)
	if strength, label := EvidenceFor(Name{Value: "Removed", Kind: Unknown}, evidence); strength != 3 || label != "removed" {
		t.Fatalf("removed evidence = (%d, %q), want (3, removed)", strength, label)
	}
	if strength, label := EvidenceFor(Name{Value: "Hidden", Kind: Unknown}, evidence); strength != 3 || label != "privatised" {
		t.Fatalf("private evidence = (%d, %q), want (3, privatised)", strength, label)
	}
	if strength, label := EvidenceFor(Name{Value: "Record.left", Kind: Changed}, evidence); strength != 3 || label != "fields changed" {
		t.Fatalf("field evidence = (%d, %q), want (3, fields changed)", strength, label)
	}
	if strength, label := EvidenceFor(Name{Value: "Closed.member", Kind: Unknown}, evidence); strength != 2 || label != "namespace closed" {
		t.Fatalf("namespace evidence = (%d, %q), want (2, namespace closed)", strength, label)
	}
}

func TestDossierCase9NormalizesQuotedNames(t *testing.T) {
	log := "✖ [1/1] Building Alpha.«quoted-name».Part\nerror: Alpha/«quoted-name»/Part.lean:1:1: Unknown identifier `gone`\n"

	report := Parse(log, []string{"Alpha"})
	if got, want := report.RedModules, []string{"Alpha.quoted-name.Part"}; !sameStrings(got, want) {
		t.Fatalf("Parse() red modules = %#v, want %#v", got, want)
	}
}

func TestDossierCase11DoesNotBlameImportClosure(t *testing.T) {
	report := Report{Blocks: []Block{{Module: "Alpha.Beta", Message: "Unknown identifier `gone`"}}}
	evidence := map[string]Evidence{
		"closure-only": IndexDiff("+def unrelated := 0"),
	}

	got := Identify(report, evidence, nil, nil)
	if len(got) != 0 {
		t.Fatalf("Identify() = %#v, want no culprit for a closure-only change", got)
	}
}

func TestDossierCase12CapsBroadEvidenceAndIgnoresBaseRestatements(t *testing.T) {
	strong := []Row{
		{Member: "one", Strength: 3, Label: "removed"},
		{Member: "two", Strength: 3, Label: "removed"},
		{Member: "three", Strength: 3, Label: "removed"},
		{Member: "four", Strength: 3, Label: "removed"},
		{Member: "five", Strength: 3, Label: "removed"},
		{Member: "six", Strength: 3, Label: "removed"},
	}
	if got := Pick(strong); len(got.Members) != 0 {
		t.Fatalf("Pick() broad strong evidence = %#v, want no members", got)
	}
	weak := []Row{
		{Member: "one", Strength: 2, Label: "edited"},
		{Member: "two", Strength: 2, Label: "edited"},
		{Member: "three", Strength: 1, Label: "added"},
		{Member: "four", Strength: 1, Label: "added"},
	}
	if got, want := Pick(weak).Members, []Row{{Member: "one", Strength: 2, Label: "edited"}, {Member: "two", Strength: 2, Label: "edited"}, {Member: "four", Strength: 1, Label: "added"}}; !sameRows(got, want) {
		t.Fatalf("Pick() weak evidence = %#v, want %#v", got, want)
	}

	rows := []Row{{Member: "stale", Strength: 3, Label: "added"}, {Member: "changed", Strength: 3, Label: "added"}}
	evidence := map[string]Evidence{
		"stale":   IndexDiff("+def Shared := 0"),
		"changed": IndexDiff("+def Shared := 1"),
	}
	got := AgainstTree("Shared", rows, evidence, []string{"def Shared := 0"}, []string{"def Shared := 1"})
	if want := []Row{{Member: "changed", Strength: 3, Label: "changed in candidate"}}; !sameRows(got, want) {
		t.Fatalf("AgainstTree() = %#v, want %#v", got, want)
	}
}

func TestConfirmRequiresSingleMemberFailure(t *testing.T) {
	builder := fakeBuilder{red: map[string]bool{"base+bad": true}}
	candidate := candidateForTests

	result, err := Confirm(context.Background(), "base", []string{"Alpha.Beta"}, []Member{{ID: "good"}, {ID: "bad"}}, []string{"good", "bad"}, candidate, builder)
	if err != nil {
		t.Fatalf("Confirm() error = %v", err)
	}
	if got, want := result.Confirmed, []string{"bad"}; !sameStrings(got, want) {
		t.Fatalf("Confirm() confirmed = %#v, want %#v", got, want)
	}
}

func TestConfirmBisectsWhenNoNamedMemberFailsAlone(t *testing.T) {
	builder := fakeBuilder{red: map[string]bool{"base+one+two": true, "base+one+two+three": true}}
	members := []Member{{ID: "one"}, {ID: "two"}, {ID: "three"}}

	result, err := Confirm(context.Background(), "base", []string{"Alpha.Beta"}, members, nil, candidateForTests, builder)
	if err != nil {
		t.Fatalf("Confirm() error = %v", err)
	}
	if got, want := result.Bisected, []string{"two"}; !sameStrings(got, want) {
		t.Fatalf("Confirm() bisected = %#v, want %#v", got, want)
	}
}

type fakeBuilder struct {
	red map[string]bool
}

func (b fakeBuilder) Build(_ context.Context, tree Tree, _ []string) (BuildResult, error) {
	return BuildResult{Green: !b.red[string(tree)]}, nil
}

func candidateForTests(_ context.Context, base string, members []Member) (Tree, error) {
	parts := []string{base}
	for _, member := range members {
		parts = append(parts, member.ID)
	}
	return Tree(strings.Join(parts, "+")), nil
}

func sameStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func sameNames(got, want []Name) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func sameRows(got, want []Row) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
