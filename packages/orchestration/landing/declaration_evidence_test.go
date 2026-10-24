package landing

import (
	"context"
	"os"
	"path/filepath"
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
