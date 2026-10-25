package graphcheck

import (
	"strings"

	"github.com/sjawhar/legion/orchestration/declindex"
)

// The heads that L1 and P1 judge by are the exporter's: declindex.Constant.Binders holds the head constant of
// each explicit binder's type, in order, and Conclusion the head constant of the type after every binder. The
// candidate's come from Candidate.Constants, the base's from the index (ConstantsIn, ConstantsConcluding), which
// holds them only where its compiled part was read. Nothing here reads Lean source text to decide anything: the
// candidate's text gives a column for a line the compiled reference file records, and nothing else.

// binds reports whether one of the constant's explicit binders has the head.
func binds(k declindex.Constant, head string) bool {
	for _, b := range k.Binders {
		if b == head {
			return true
		}
	}
	return false
}

// declarationKinds are the kinds of constant that someone writes as a declaration: a constructor, a recursor and
// the parts of a quotient are Lean's, not a producer and not a link.
var declarationKinds = map[string]bool{"def": true, "theorem": true, "axiom": true, "opaque": true, "inductive": true}

// declared reports whether the constant is one that someone wrote as a declaration, and not a part that Lean
// generated from the owner declaration (its constructor, its projections, an equation lemma). It says nothing
// about whether the declaration is public: a private instance produces, a private theorem links nothing.
func declared(k declindex.Constant, owner string) bool {
	return declarationKinds[k.Kind] && !partOf(k.Name, owner)
}

// publicTheorem reports whether the constant is a theorem that someone wrote and that another module can name:
// L1 reads the links that a tree can rely on, and a private one is not one.
func publicTheorem(k declindex.Constant) bool {
	return k.Kind == "theorem" && declared(k, "") && !isPrivate(k.Name)
}

// partOf reports whether a name is the owner's or lies below it: Lean generates X.mk and X.rec from X.
func partOf(name, owner string) bool {
	return owner != "" && (name == owner || strings.HasPrefix(name, owner+"."))
}

// isPrivate reports whether a compiled constant's name is the name Lean gives a private declaration.
func isPrivate(name string) bool { return strings.HasPrefix(normalName(name), "_private.") }

// headUse is the position of the first use of head that the module's reference file records inside the
// declaration's statement: where its source writes the binder's or the conclusion's type head. A use outside a
// statement serves when the file records none inside one.
func headUse(uses []declindex.Use, declaration, head string) (line, col int, ok bool) {
	for _, inStatement := range []bool{true, false} {
		for _, u := range uses {
			if u.Parent != declaration || u.Target != head || (inStatement && u.Class != "stmt") {
				continue
			}
			return u.Line, u.Col, true
		}
	}
	return 0, 0, false
}

// binderColumn is the column to report for a binder whose type head stands at line and col: the column of the
// bracket that opens the binder, when the head's line holds one, else the head's own column. The compiled
// reference file gives the head's position; this reads the candidate's text for the column only, and judges
// nothing by it. An unchanged file has no candidate text, and keeps the head's column.
func binderColumn(text string, line, col int) int {
	if text == "" || line < 1 || col < 1 {
		return max(col, 1)
	}
	lines := strings.Split(text, "\n")
	if line > len(lines) {
		return col
	}
	runes := []rune(lines[line-1])
	if col-1 > len(runes) {
		return col
	}
	depth := 0
	for i := col - 2; i >= 0; i-- {
		switch {
		case isCloser(runes[i]):
			depth++
		case isOpener(runes[i]):
			if depth == 0 {
				return i + 1
			}
			depth--
		}
	}
	return col
}

func isOpener(r rune) bool {
	switch r {
	case '(', '{', '[', '⦃', '⟨':
		return true
	}
	return false
}

func isCloser(r rune) bool {
	switch r {
	case ')', '}', ']', '⦄', '⟩':
		return true
	}
	return false
}

// openMarked reports whether the comments on lines first to last of the candidate's text hold the marker
// followed by at least minReason characters of reason: the declaration's doc comment, the binder's line and the
// lines between them. The reason runs from the marker to the end of its comment block, which is the first line
// of the region below it that holds no comment. A marker is a convention of the text, not a fact about Lean.
func openMarked(text, marker string, minReason, first, last int) bool {
	if marker == "" || text == "" {
		return false
	}
	for rest := commentsIn(text, first, last); ; {
		i := strings.Index(rest, marker)
		if i < 0 {
			return false
		}
		rest = rest[i+len(marker):]
		if reasonLength(untilBlankLine(rest)) >= minReason {
			return true
		}
	}
}

// commentsIn is the text of lines first to last with every byte that is not in a comment blanked, so that the
// marker is read in a comment alone and the lines keep their places.
func commentsIn(text string, first, last int) string {
	stripped := stripComments(text)
	if len(stripped) != len(text) {
		return ""
	}
	out := []byte(text)
	for i := range out {
		if out[i] != '\n' && stripped[i] == out[i] {
			out[i] = ' '
		}
	}
	lines := strings.Split(string(out), "\n")
	from, to := max(first, 1), min(last, len(lines))
	if from > to {
		return ""
	}
	return strings.Join(lines[from-1:to], "\n")
}

// untilBlankLine is s up to its first blank line after the first.
func untilBlankLine(s string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines[1:] {
		if strings.TrimSpace(line) == "" {
			return strings.Join(lines[:i+1], "\n")
		}
	}
	return s
}

// reasonLength counts the characters of a reason: a run of white space counts as one, and the end of the comment
// that holds it counts as none.
func reasonLength(reason string) int {
	reason = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(reason), "-/"))
	return len([]rune(strings.Join(strings.Fields(reason), " ")))
}
