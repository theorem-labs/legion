package landing

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/orchestration/landing/blame"
)

// memberEvidence commits base, then one member commit that writes each file
// in writes and deletes each file in deletes, and indexes the member through
// the real Git adapter.
func memberEvidence(t *testing.T, base, writes map[string]string, deletes ...string) blame.Evidence {
	t.Helper()
	repo, baseCommit, head := memberRepository(t, base, writes, deletes...)
	evidence, err := (RepositoryGit{}).DeclarationEvidence(context.Background(), repo, baseCommit, Member{Migration: "member", Head: head})
	if err != nil {
		t.Fatalf("DeclarationEvidence() error = %v", err)
	}
	return evidence
}

func memberRepository(t *testing.T, base, writes map[string]string, deletes ...string) (string, string, string) {
	t.Helper()
	repo, _ := commandRepository(t, false)
	for path, source := range base {
		writeSource(t, repo, path, source)
	}
	commandReviewGit(t, repo, "add", "-A")
	commandReviewGit(t, repo, "commit", "-qm", "base")
	baseCommit := commandReviewGit(t, repo, "rev-parse", "HEAD")
	for path, source := range writes {
		writeSource(t, repo, path, source)
	}
	for _, path := range deletes {
		if err := os.Remove(filepath.Join(repo, path)); err != nil {
			t.Fatal(err)
		}
	}
	commandReviewGit(t, repo, "add", "-A")
	commandReviewGit(t, repo, "commit", "-qm", "member")
	return repo, baseCommit, commandReviewGit(t, repo, "rev-parse", "HEAD")
}

func wantEvidence(t *testing.T, evidence blame.Evidence, name blame.Name, strength int, label string) {
	t.Helper()
	if gotStrength, gotLabel := blame.EvidenceFor(name, evidence); gotStrength != strength || gotLabel != label {
		t.Errorf("EvidenceFor(%s %s) = (%d, %q), want (%d, %q)", name.Kind, name.Value, gotStrength, gotLabel, strength, label)
	}
}

// A change to a declaration's body is an edit inside that declaration, and
// only that one: not the declaration above or below it, and not a declaration
// of the same name in another namespace.
func TestDeclarationEvidenceAttributesBodyEditToItsDeclaration(t *testing.T) {
	base := "namespace Alpha\ndef Before : Nat :=\n  0\n\n/-- The changed one. -/\ndef Changed : Nat :=\n  1\n\ndef After : Nat :=\n  2\nend Alpha\n\nnamespace Beta\ndef Changed : Nat :=\n  3\nend Beta\n"
	member := "namespace Alpha\ndef Before : Nat :=\n  0\n\n/-- The changed one. -/\ndef Changed : Nat :=\n  4\n\ndef After : Nat :=\n  2\nend Alpha\n\nnamespace Beta\ndef Changed : Nat :=\n  3\nend Beta\n"
	evidence := memberEvidence(t, map[string]string{"Alpha.lean": base}, map[string]string{"Alpha.lean": member})

	wantEvidence(t, evidence, blame.Name{Value: "Alpha.Changed", Kind: blame.Changed}, 2, "edited")
	wantEvidence(t, evidence, blame.Name{Value: "Before", Kind: blame.Changed}, 0, "")
	wantEvidence(t, evidence, blame.Name{Value: "After", Kind: blame.Changed}, 0, "")
	wantEvidence(t, evidence, blame.Name{Value: "Beta.Changed", Kind: blame.Changed}, 0, "")
}

// A comment-only change edits no declaration: the doc comment of one
// declaration and the comment between two declarations are not code.
func TestDeclarationEvidenceIgnoresCommentOnlyEdits(t *testing.T) {
	base := "def First : Nat :=\n  0\n\n-- between\n/-- Second's doc. -/\ndef Second : Nat :=\n  1\n"
	member := "def First : Nat :=\n  0\n\n-- between, reworded\n/-- Second's doc, reworded. -/\ndef Second : Nat :=\n  1\n"
	evidence := memberEvidence(t, map[string]string{"Alpha.lean": base}, map[string]string{"Alpha.lean": member})

	wantEvidence(t, evidence, blame.Name{Value: "First", Kind: blame.Changed}, 0, "")
	wantEvidence(t, evidence, blame.Name{Value: "Second", Kind: blame.Changed}, 0, "")
}

// An attribute written on its own line above a declaration belongs to that
// declaration's signature, not to the declaration above it.
func TestDeclarationEvidenceAttributesAttributeLineToNextDeclaration(t *testing.T) {
	base := "def First : Nat :=\n  0\n\ntheorem Second : True := trivial\n"
	member := "def First : Nat :=\n  0\n\n@[simp]\ntheorem Second : True := trivial\n"
	evidence := memberEvidence(t, map[string]string{"Alpha.lean": base}, map[string]string{"Alpha.lean": member})

	wantEvidence(t, evidence, blame.Name{Value: "Second", Kind: blame.Changed}, 3, "signature changed")
	wantEvidence(t, evidence, blame.Name{Value: "First", Kind: blame.Changed}, 0, "")
}

// The signature runs past the first line: a change on a continuation line
// changes the signature.
func TestDeclarationEvidenceComparesWholeSignature(t *testing.T) {
	base := "theorem Target (n : Nat)\n    (h : n = n) :\n    True := by\n  trivial\n"
	member := "theorem Target (n : Nat)\n    (h : n = 0) :\n    True := by\n  trivial\n"
	evidence := memberEvidence(t, map[string]string{"Alpha.lean": base}, map[string]string{"Alpha.lean": member})

	wantEvidence(t, evidence, blame.Name{Value: "Target", Kind: blame.Changed}, 3, "signature changed")
}

// Lean continues a term on an unindented line that starts with an
// identifier: the binder `{A :` stays open into the next line. A change to a
// later signature line changes the declaration, and a change to its body is
// an edit inside it.
func TestDeclarationEvidenceContinuesSignatureOnUnindentedLine(t *testing.T) {
	const file = "Link.lean"
	base := "private theorem Table.noLink_of [Inst] [Group G] {A :\n" +
		"Table.Args G} (O : Table.Open A)\n" +
		"    (F : Table.Fits A) : Table.NoLink A where\n" +
		"  read := O.read\n" +
		"  write := O.write\n"
	name := blame.Name{Value: "Table.noLink_of", Kind: blame.Changed}

	signature := memberEvidence(t, map[string]string{file: base}, map[string]string{file: strings.Replace(base, "Fits A", "Fits' A", 1)})
	wantEvidence(t, signature, name, 3, "signature changed")

	body := memberEvidence(t, map[string]string{file: base}, map[string]string{file: strings.Replace(base, "O.read", "O.read2", 1)})
	wantEvidence(t, body, name, 2, "edited")
}

// candidateTree builds the candidate of base and members through the real Git
// adapter, and fails the test when a member is dropped.
func candidateTree(t *testing.T, repo, base string, members ...Member) Tree {
	t.Helper()
	result, err := (RepositoryGit{}).Candidate(context.Background(), repo, base, members)
	if err != nil {
		t.Fatalf("Candidate() error = %v", err)
	}
	if len(result.Dropped) != 0 {
		t.Fatalf("Candidate() dropped %+v", result.Dropped)
	}
	return Tree(result.Tree)
}

// A stale member, branched before the base added a declaration, re-adds the
// same declaration with a signature that spans lines. It restates the base,
// so it is not blamed for the duplicate.
func TestDeclarationEvidenceSuppressesMultiLineRestatement(t *testing.T) {
	const restated = "theorem T (n : Nat)\n    (h : n = 0) : True := trivial\n"
	repo, root := commandRepository(t, false)
	writeSource(t, repo, "T.lean", restated)
	commandReviewGit(t, repo, "add", "T.lean")
	commandReviewGit(t, repo, "commit", "-qm", "base")
	base := commandReviewGit(t, repo, "rev-parse", "HEAD")
	commandReviewGit(t, repo, "checkout", "-q", "-b", "stale", root)
	writeSource(t, repo, "T.lean", restated)
	commandReviewGit(t, repo, "add", "T.lean")
	commandReviewGit(t, repo, "commit", "-qm", "stale")
	stale := Member{Migration: "stale", Head: commandReviewGit(t, repo, "rev-parse", "HEAD")}

	report := blame.Report{Blocks: []blame.Block{{Module: "T", Message: "environment already contains 'T'"}}}
	got := attribute(t, repo, base, map[string]string{stale.Migration: stale.Head}, candidateTree(t, repo, base, stale), report)
	if len(got) != 0 {
		t.Fatalf("attributions = %+v, want the stale restatement suppressed", got)
	}
}

// A member that changes a continuation line of a signature changes the
// declaration in the candidate, so its evidence is promoted.
func TestDeclarationEvidencePromotesMultiLineChange(t *testing.T) {
	repo, base, head := memberRepository(t,
		map[string]string{"T.lean": "theorem T (n : Nat)\n    (h : n = 0) : True := trivial\n"},
		map[string]string{"T.lean": "theorem T (n : Nat)\n    (h : n = 1) : True := trivial\n"},
	)
	member := Member{Migration: "member", Head: head}

	report := blame.Report{Blocks: []blame.Block{{Module: "T", Message: "environment already contains 'T'"}}}
	got := attribute(t, repo, base, map[string]string{member.Migration: member.Head}, candidateTree(t, repo, base, member), report)
	want := blame.Attribution{Module: "T", Name: "T", Kind: blame.Added, Member: "member", Strength: 3, Evidence: "changed in candidate"}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("attributions = %+v, want [%+v]", got, want)
	}
}

// A match arm of a definition is its body, not its signature: editing one arm
// is an edit inside the definition.
func TestDeclarationEvidenceMatchArmIsBody(t *testing.T) {
	base := "def f : Nat → Nat\n  | 0 => 1\n  | n + 1 => n\n"
	member := "def f : Nat → Nat\n  | 0 => 2\n  | n + 1 => n\n"
	evidence := memberEvidence(t, map[string]string{"Alpha.lean": base}, map[string]string{"Alpha.lean": member})

	wantEvidence(t, evidence, blame.Name{Value: "f", Kind: blame.Changed}, 2, "edited")
	wantEvidence(t, evidence, blame.Name{Value: "f", Kind: blame.Unknown}, 1, "edited")
}

// Git names a hunk's enclosing declaration by the line it finds above the
// hunk. Two declarations share the short name Changed, and the hunk lies
// inside Beta's: the evidence belongs to Beta.Changed, not to the first
// Changed in the file. A context line that is not a declaration, such as
// `namespace Beta`, names no declaration.
func TestDeclarationEvidenceHunkHeaderResolvesPosition(t *testing.T) {
	base := "namespace Alpha\ndef Changed := 0\nend Alpha\nnamespace Beta\ndef Changed := 1\nend Beta\n"

	// The hunk adds Beta.Extra after Beta.Changed; git's context is
	// `def Changed := 1`.
	added := memberEvidence(t, map[string]string{"Alpha.lean": base}, map[string]string{"Alpha.lean": "namespace Alpha\ndef Changed := 0\nend Alpha\nnamespace Beta\ndef Changed := 1\ndef Extra := 2\nend Beta\n"})
	wantEvidence(t, added, blame.Name{Value: "Beta.Changed", Kind: blame.Changed}, 2, "edited")
	wantEvidence(t, added, blame.Name{Value: "Alpha.Changed", Kind: blame.Changed}, 0, "")

	// The hunk edits Beta.Changed itself; git's context is `namespace Beta`.
	edited := memberEvidence(t, map[string]string{"Alpha.lean": base}, map[string]string{"Alpha.lean": "namespace Alpha\ndef Changed := 0\nend Alpha\nnamespace Beta\ndef Changed := 2\nend Beta\n"})
	wantEvidence(t, edited, blame.Name{Value: "Beta.Changed", Kind: blame.Changed}, 3, "signature changed")
	wantEvidence(t, edited, blame.Name{Value: "Alpha.Changed", Kind: blame.Changed}, 0, "")
}

// A `namespace` line inside a block comment opens no namespace, so removing
// the comment closes none.
func TestDeclarationEvidenceIgnoresNamespaceInComment(t *testing.T) {
	base := "/-\nnamespace Ghost\n-/\ndef kept := 0\n"
	member := "def kept := 0\n"
	evidence := memberEvidence(t, map[string]string{"Alpha.lean": base}, map[string]string{"Alpha.lean": member})

	wantEvidence(t, evidence, blame.Name{Value: "Ghost.x", Kind: blame.Unknown}, 0, "")
	wantEvidence(t, evidence, blame.Name{Value: "kept", Kind: blame.Changed}, 0, "")
}

// A removed line comment is `--- …` in the diff, and an added line that
// starts with `++` is `+++ …`: inside a hunk they are changed lines, not the
// file headers of the next file.
func TestDeclarationEvidenceReadsChangedLinesThatLookLikeFileHeaders(t *testing.T) {
	base := "-- note\nnamespace Closed\ndef member := 0\nend Closed\n"
	member := "++ added\ndef member := 0\n"
	evidence := memberEvidence(t, map[string]string{"Alpha.lean": base}, map[string]string{"Alpha.lean": member})

	wantEvidence(t, evidence, blame.Name{Value: "Closed.elsewhere", Kind: blame.Unknown}, 2, "namespace closed")
}

// A removed or added structure field is recorded under the structure's fully
// qualified name.
func TestDeclarationEvidenceRecordsStructureFields(t *testing.T) {
	base := "namespace Alpha\nstructure Record where\n  left : Nat\n  right : Nat\nend Alpha\n"
	member := "namespace Alpha\nstructure Record where\n  right : Nat\n  /-- A new field. -/\n  middle : Nat\nend Alpha\n"
	evidence := memberEvidence(t, map[string]string{"Alpha.lean": base}, map[string]string{"Alpha.lean": member})

	wantEvidence(t, evidence, blame.Name{Value: "Alpha.Record.left", Kind: blame.Unknown}, 3, "field removed")
	wantEvidence(t, evidence, blame.Name{Value: "Record.left", Kind: blame.Unknown}, 3, "field removed")
	wantEvidence(t, evidence, blame.Name{Value: "Beta.Record.left", Kind: blame.Unknown}, 0, "")
	wantEvidence(t, evidence, blame.Name{Value: "middle", Kind: blame.Field}, 2, "field")
	wantEvidence(t, evidence, blame.Name{Value: "left", Kind: blame.Field}, 2, "field")
	wantEvidence(t, evidence, blame.Name{Value: "Record", Kind: blame.Changed}, 2, "edited")
}

// Removing a namespace closes it for every name declared in it elsewhere.
func TestDeclarationEvidenceRecordsClosedNamespace(t *testing.T) {
	base := "namespace Closed\ndef member := 0\nend Closed\n"
	member := "def member := 0\n"
	evidence := memberEvidence(t, map[string]string{"Alpha.lean": base}, map[string]string{"Alpha.lean": member})

	wantEvidence(t, evidence, blame.Name{Value: "Closed.elsewhere", Kind: blame.Unknown}, 2, "namespace closed")
	wantEvidence(t, evidence, blame.Name{Value: "Other.elsewhere", Kind: blame.Unknown}, 0, "")
}

// A file moved with a declaration removed: the removal is evidence, and the
// declarations that only moved are not strong evidence of an addition.
func TestDeclarationEvidenceReadsRemovalsFromRenamedFiles(t *testing.T) {
	kept := "def keep1 := 1\ndef keep2 := 2\ndef keep3 := 3\ndef keep4 := 4\ndef keep5 := 5\ndef keep6 := 6\ndef keep7 := 7\ndef keep8 := 8\n"
	evidence := memberEvidence(t,
		map[string]string{"Old.lean": "def gone := 0\n" + kept},
		map[string]string{"New.lean": kept},
		"Old.lean",
	)

	wantEvidence(t, evidence, blame.Name{Value: "gone", Kind: blame.Unknown}, 3, "removed")
	wantEvidence(t, evidence, blame.Name{Value: "keep1", Kind: blame.Added}, 2, "restated")
}

// Git quotes a path with non-ASCII bytes unless it is asked for raw paths;
// the adapter reads such a file in a member's diff and in a tree.
func TestDeclarationEvidenceReadsNonASCIIPaths(t *testing.T) {
	const path = "Alpha é.lean"
	repo, baseCommit, head := memberRepository(t,
		map[string]string{path: "def gone := 0\ndef kept := 1\n"},
		map[string]string{path: "def kept := 1\n"},
	)
	evidence, err := (RepositoryGit{}).DeclarationEvidence(context.Background(), repo, baseCommit, Member{Migration: "member", Head: head})
	if err != nil {
		t.Fatalf("DeclarationEvidence() error = %v", err)
	}
	wantEvidence(t, evidence, blame.Name{Value: "gone", Kind: blame.Unknown}, 3, "removed")

	declarations, err := (RepositoryGit{}).TreeDeclarations(context.Background(), repo, Tree(baseCommit))
	if err != nil {
		t.Fatalf("TreeDeclarations() error = %v", err)
	}
	found := blame.DeclarationsFor("gone", declarations)
	if len(found) != 1 || found[0].File != path {
		t.Fatalf("declarations of gone at base = %+v, want one in %q", found, path)
	}
}
