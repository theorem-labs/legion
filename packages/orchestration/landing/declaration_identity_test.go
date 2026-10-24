package landing

import (
	"context"
	"testing"

	"github.com/sjawhar/legion/orchestration/landing/blame"
)

// identityRepository commits base, then commits each member's version of
// Names.lean on its own branch from base. It returns the base commit and each
// member's head.
func identityRepository(t *testing.T, base string, members map[string]string) (string, string, map[string]string) {
	t.Helper()
	repo, _ := commandRepository(t, false)
	writeSource(t, repo, "Names.lean", base)
	commandReviewGit(t, repo, "add", "Names.lean")
	commandReviewGit(t, repo, "commit", "-qm", "base names")
	baseCommit := commandReviewGit(t, repo, "rev-parse", "HEAD")
	heads := make(map[string]string, len(members))
	for name, source := range members {
		commandReviewGit(t, repo, "checkout", "-q", "-b", "member-"+name, baseCommit)
		writeSource(t, repo, "Names.lean", source)
		commandReviewGit(t, repo, "commit", "-qam", name)
		heads[name] = commandReviewGit(t, repo, "rev-parse", "HEAD")
	}
	return repo, baseCommit, heads
}

// identityRepositoryFiles commits a base tree, then creates one branch for
// each member's file changes from that base.
func identityRepositoryFiles(t *testing.T, base map[string]string, members map[string]map[string]string) (string, string, map[string]string) {
	t.Helper()
	repo, _ := commandRepository(t, false)
	for path, source := range base {
		writeSource(t, repo, path, source)
	}
	commandReviewGit(t, repo, "add", ".")
	commandReviewGit(t, repo, "commit", "-qm", "base names")
	baseCommit := commandReviewGit(t, repo, "rev-parse", "HEAD")
	heads := make(map[string]string, len(members))
	for name, changes := range members {
		commandReviewGit(t, repo, "checkout", "-q", "-b", "member-"+name, baseCommit)
		for path, source := range changes {
			writeSource(t, repo, path, source)
		}
		commandReviewGit(t, repo, "commit", "-am", name)
		heads[name] = commandReviewGit(t, repo, "rev-parse", "HEAD")
	}
	return repo, baseCommit, heads
}

func declarationLines(t *testing.T, git RepositoryGit, repo string, tree Tree, name string) []blame.Declaration {
	t.Helper()
	declarations, err := git.Declarations(context.Background(), repo, tree, name)
	if err != nil {
		t.Fatal(err)
	}
	return declarations
}

// attribute runs the evidence step and the tree comparison through the real Git
// adapter: each member's diff is indexed against its sources, and the
// diagnostic's declarations are read from the base and from candidate.
func attribute(t *testing.T, repo, base string, heads map[string]string, candidate Tree, report blame.Report) []blame.Attribution {
	t.Helper()
	git := RepositoryGit{}
	evidence := make(map[string]blame.Evidence, len(heads))
	for name, head := range heads {
		member := Member{Migration: name, Head: head}
		memberEvidence, err := git.DeclarationEvidence(context.Background(), repo, base, member)
		if err != nil {
			t.Fatalf("DeclarationEvidence(%s) error = %v", name, err)
		}
		evidence[name] = memberEvidence
	}
	lines := make(map[string]blame.TreeDeclarations)
	for _, block := range report.Blocks {
		for _, name := range blame.Names(block.Message) {
			lines[name.Value] = blame.TreeDeclarations{
				Base:      declarationLines(t, git, repo, Tree(base), name.Value),
				Candidate: declarationLines(t, git, repo, candidate, name.Value),
			}
		}
	}
	return blame.IdentifyDetailed(report, evidence, lines)
}

func attributedMembers(attributions []blame.Attribution) []string {
	var members []string
	for _, attribution := range attributions {
		if !contains(members, attribution.Member) {
			members = append(members, attribution.Member)
		}
	}
	return members
}

// Alpha.Shared and Beta.Shared have the same short name and the candidate's
// Alpha text matches Beta's base text. The real Git adapter must select only
// the member that changed Alpha.Shared.
func TestDeclarationIdentitySeparatesSameNamesInDifferentFiles(t *testing.T) {
	repo, baseCommit, heads := identityRepositoryFiles(t,
		map[string]string{
			"Alpha.lean": "namespace Alpha\ndef Shared := 0\nend Alpha\n",
			"Beta.lean":  "namespace Beta\ndef Shared := 1\nend Beta\n",
		},
		map[string]map[string]string{
			"alpha": {"Alpha.lean": "namespace Alpha\ndef Shared := 1\nend Alpha\n"},
			"beta":  {"Beta.lean": "namespace Beta\ndef Shared := 2\nend Beta\n"},
		},
	)
	report := blame.Report{Blocks: []blame.Block{{Module: "Alpha.Mod", Message: "type expected, got\n  Alpha.Shared argument"}}}
	got := attribute(t, repo, baseCommit, map[string]string{"alpha": heads["alpha"], "beta": heads["beta"]}, Tree(heads["alpha"]), report)
	if members := attributedMembers(got); !sameStrings(members, []string{"alpha"}) {
		t.Fatalf("attributed %v (%+v), want only the member that changed Alpha.Shared", members, got)
	}

	declarations := declarationLines(t, RepositoryGit{}, repo, Tree(heads["alpha"]), "Alpha.Shared")
	if len(declarations) != 1 || declarations[0].File != "Alpha.lean" || declarations[0].Name != "Alpha.Shared" || declarations[0].Text != "def Shared := 1" {
		t.Fatalf("Declarations(Alpha.Shared) = %+v, want Alpha's one declaration in Alpha.lean", declarations)
	}
}

// Nested namespaces compose, and a section opened inside one is closed by its
// own `end` without closing the namespace around it.
func TestDeclarationIdentityResolvesNestedNamespacesAndSections(t *testing.T) {
	base := "namespace Outer\nnamespace Inner\ndef value := 0\nend Inner\nnamespace A.B\ndef dotted := 1\nend A.B\nsection S\ndef after := 0\nend S\ndef later := 0\nend Outer\nnamespace Inner\ndef value := 5\nend Inner\n"
	repo, baseCommit, heads := identityRepository(t, base, map[string]string{
		"outer": "namespace Outer\nnamespace Inner\nend Inner\nnamespace A.B\ndef dotted := 1\nend A.B\nsection S\ndef after := 0\nend S\ndef later := 0\nend Outer\nnamespace Inner\ndef value := 5\nend Inner\n",
		"root":  "namespace Outer\nnamespace Inner\ndef value := 0\nend Inner\nnamespace A.B\ndef dotted := 1\nend A.B\nsection S\ndef after := 0\nend S\ndef later := 0\nend Outer\nnamespace Inner\nend Inner\n",
	})
	report := blame.Report{Blocks: []blame.Block{{Module: "Alpha.Mod", Message: "unknown constant 'Outer.Inner.value'"}}}
	got := attribute(t, repo, baseCommit, heads, Tree(baseCommit), report)
	if members := attributedMembers(got); !sameStrings(members, []string{"outer"}) {
		t.Fatalf("attributed %v (%+v), want only the member that removed Outer.Inner.value", members, got)
	}

	scanned, err := (RepositoryGit{}).TreeDeclarations(context.Background(), repo, Tree(baseCommit))
	if err != nil {
		t.Fatal(err)
	}
	byName := make(map[string]string, len(scanned))
	for _, declaration := range scanned {
		if declaration.File == "Names.lean" {
			byName[declaration.Name] = declaration.Text
		}
	}
	want := map[string]string{
		"Outer.Inner.value": "def value := 0",
		"Outer.A.B.dotted":  "def dotted := 1",
		"Outer.after":       "def after := 0",
		"Outer.later":       "def later := 0",
		"Inner.value":       "def value := 5",
	}
	if len(byName) != len(want) {
		t.Fatalf("Names.lean declarations = %v, want exactly %v", byName, want)
	}
	for name, text := range want {
		if byName[name] != text {
			t.Fatalf("declaration %s = %q, want %q (all: %v)", name, byName[name], text, byName)
		}
	}
}

// A `_root_.` name inside a namespace declares a root name, not one in the
// namespace; private and protected modifiers are recorded on the declaration.
func TestDeclarationIdentityResolvesRootNamesAndModifiers(t *testing.T) {
	base := "namespace Alpha\ndef _root_.helper := 0\nprotected def helper := 1\nprivate def hidden := 2\nend Alpha\n"
	repo, baseCommit, heads := identityRepository(t, base, map[string]string{
		"rooted": "namespace Alpha\nprotected def helper := 1\nprivate def hidden := 2\nend Alpha\n",
		"inner":  "namespace Alpha\ndef _root_.helper := 0\nprivate def hidden := 2\nend Alpha\n",
		"hidden": "namespace Alpha\ndef _root_.helper := 0\nprotected def helper := 1\nend Alpha\n",
	})
	report := blame.Report{Blocks: []blame.Block{{Module: "Alpha.Mod", Message: "unknown constant 'Alpha.helper'"}}}
	got := attribute(t, repo, baseCommit, heads, Tree(baseCommit), report)
	if members := attributedMembers(got); !sameStrings(members, []string{"inner"}) {
		t.Fatalf("attributed %v (%+v), want only the member that removed Alpha.helper", members, got)
	}

	declarations := declarationLines(t, RepositoryGit{}, repo, Tree(baseCommit), "helper")
	if len(declarations) != 1 || declarations[0].Name != "helper" || declarations[0].Text != "def _root_.helper := 0" || declarations[0].Protected {
		t.Fatalf("Declarations(helper) = %+v, want the root helper only", declarations)
	}
	protected := declarationLines(t, RepositoryGit{}, repo, Tree(baseCommit), "Alpha.helper")
	if len(protected) != 1 || !protected[0].Protected || protected[0].Name != "Alpha.helper" {
		t.Fatalf("Declarations(Alpha.helper) = %+v, want the protected Alpha helper", protected)
	}
	hidden, err := (RepositoryGit{}).Declarations(context.Background(), repo, Tree(baseCommit), "Alpha.hidden")
	if err != nil {
		t.Fatal(err)
	}
	if len(hidden) != 1 || !hidden[0].Private {
		t.Fatalf("Declarations(Alpha.hidden) = %+v, want one private declaration", hidden)
	}
	privateReport := blame.Report{Blocks: []blame.Block{{Module: "Alpha.Mod", Message: "unknown constant 'Alpha.hidden'"}}}
	privateAttributions := attribute(t, repo, baseCommit, map[string]string{"hidden": heads["hidden"]}, Tree(baseCommit), privateReport)
	if members := attributedMembers(privateAttributions); !sameStrings(members, []string{"hidden"}) {
		t.Fatalf("private attributions %v (%+v), want only the member that removed Alpha.hidden", members, privateAttributions)
	}
}

// Comments and strings can contain namespace syntax without changing the
// scope used by the real Git declaration adapter.
func TestDeclarationIdentityIgnoresScopesInCommentsAndStrings(t *testing.T) {
	source := "namespace Alpha\n-- namespace Beta\n/- namespace Gamma\n  /- end Alpha -/\n-/\ndef first := \"end Alpha\"\ndef second := 0\nend Alpha\n"
	repo, baseCommit, _ := identityRepository(t, source, nil)
	for _, name := range []string{"Alpha.first", "Alpha.second"} {
		declarations := declarationLines(t, RepositoryGit{}, repo, Tree(baseCommit), name)
		if len(declarations) != 1 || declarations[0].Name != name {
			t.Fatalf("Declarations(%s) = %+v, want one declaration in Alpha", name, declarations)
		}
	}
}
