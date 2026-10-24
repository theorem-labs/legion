package declindex

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The text rules of this file follow Lean's surface syntax only as far as a statement needs: comments, strings,
// brackets and the tokens that end a declaration's statement. Offsets are byte offsets into the source.

const (
	openBrackets  = "([{⟨⦃"
	closeBrackets = ")]}⟩⦄"
)

// defKinds are the declaration kinds whose whole text is their meaning; a theorem's meaning is its signature. A
// definition of one of these kinds passes a change to every statement that applies it (propagates).
var defKinds = []string{"def", "abbrev", "instance", "structure", "class", "inductive", "opaque"}

// keywords are the commands whose last occurrence before a declaration's name gives its kind.
var keywords = []string{"theorem", "lemma", "def", "abbrev", "instance", "structure", "class", "inductive", "opaque",
	"axiom", "example"}

// isSpace is Python's str.isspace, so that hashes match the reference reader's: Unicode white space and the four
// ASCII separators.
func isSpace(r rune) bool { return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f) }

func isAlnum(r rune) bool { return unicode.IsLetter(r) || unicode.IsNumber(r) }

func isWord(r rune) bool { return isAlnum(r) || r == '_' }

// source is one file's text with the byte offset of each line's start.
type source struct {
	text   string
	starts []int
}

func newSource(text string) *source {
	starts := []int{0}
	for i := range len(text) {
		if text[i] == '\n' {
			starts = append(starts, i+1)
		}
	}
	return &source{text: text, starts: starts}
}

func (s *source) line(l int) string {
	end := len(s.text)
	if l+1 < len(s.starts) {
		end = s.starts[l+1] - 1
	}
	return s.text[s.starts[l]:end]
}

func isASCII(s string) bool {
	for i := range len(s) {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// u16Col converts a column counted in UTF-16 code units (as .ilean files count them) to a byte index and a code
// point index within line. Past the end it gives the line's length.
func u16Col(line string, c int) (byteIdx, cp int) {
	if isASCII(line) {
		return c, c
	}
	n := 0
	for i, r := range line {
		if n >= c {
			return i, cp
		}
		if r > 0xFFFF {
			n += 2
		} else {
			n++
		}
		cp++
	}
	return len(line), cp
}

// offset is the byte offset of a 0-based line and UTF-16 column; false when the line does not exist.
func (s *source) offset(l, c int) (int, bool) {
	if l < 0 || l >= len(s.starts) {
		return 0, false
	}
	b, _ := u16Col(s.line(l), c)
	return s.starts[l] + b, true
}

// position is the 0-based line and code point column of a byte offset.
func (s *source) position(off int) (int, int) {
	lo, hi := 0, len(s.starts)-1
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if s.starts[mid] <= off {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo, utf8.RuneCountInString(s.text[s.starts[lo]:min(off, len(s.text))])
}

// next is the offset after the rune at i.
func next(text string, i int) int {
	if i >= len(text) {
		return i + 1
	}
	_, size := utf8.DecodeRuneInString(text[i:])
	return i + size
}

// skipBlockComment returns the offset after the nested block comment that starts at i, scanning no further than n.
func skipBlockComment(text string, i, n int) int {
	d := 1
	i += 2
	for i < n && d > 0 {
		switch {
		case strings.HasPrefix(text[i:], "/-"):
			d++
			i += 2
		case strings.HasPrefix(text[i:], "-/"):
			d--
			i += 2
		default:
			i = next(text, i)
		}
	}
	return i
}

// skipString returns the offset after the string literal that starts at i.
func skipString(text string, i, n int) int {
	i++
	for i < n && text[i] != '"' {
		if text[i] == '\\' {
			i = next(text, i+1)
		} else {
			i = next(text, i)
		}
	}
	return i + 1
}

// bodyStart is the offset of the first `:=`, line-initial `|` or `where` at bracket depth 0 in text[i:end],
// skipping comments and strings: where a declaration's statement ends and its body begins.
func bodyStart(text string, i, end int) (int, bool) {
	depth, n := 0, min(len(text), end)
	for i < n {
		rest := text[i:]
		switch {
		case strings.HasPrefix(rest, "--"):
			j := strings.IndexByte(rest, '\n')
			if j < 0 {
				i = n
			} else {
				i += j
			}
			continue
		case strings.HasPrefix(rest, "/-"):
			i = skipBlockComment(text, i, n)
			continue
		case text[i] == '"':
			i = skipString(text, i, n)
			continue
		}
		r, size := utf8.DecodeRuneInString(rest)
		switch {
		case strings.ContainsRune(openBrackets, r):
			depth++
		case strings.ContainsRune(closeBrackets, r):
			depth = max(0, depth-1)
		case depth == 0:
			if strings.HasPrefix(rest, ":=") {
				return i, true
			}
			if r == '|' && strings.TrimFunc(text[strings.LastIndexByte(text[:i], '\n')+1:i], isSpace) == "" {
				return i, true
			}
			if strings.HasPrefix(rest, "where") && wordBoundaryBefore(text, i, true) && wordBoundaryAfter(text, i+5, len(text), false) {
				return i, true
			}
		}
		i += size
	}
	return 0, false
}

// wordBoundaryBefore: the rune before i is not a word character (nor a dot, when dot is true).
func wordBoundaryBefore(text string, i int, dot bool) bool {
	if i == 0 {
		return true
	}
	r, _ := utf8.DecodeLastRuneInString(text[:i])
	return !(isWord(r) || (dot && r == '.'))
}

// wordBoundaryAfter: the rune at i is not a word character (nor a dot, when dot is true), or i is at end.
func wordBoundaryAfter(text string, i, end int, dot bool) bool {
	if i >= end {
		return true
	}
	r, _ := utf8.DecodeRuneInString(text[i:end])
	return !(isWord(r) || (dot && r == '.'))
}

// stripComments drops `--` line comments and nested `/- -/` block comments (docstrings included) and keeps strings.
func stripComments(s string) string {
	var out strings.Builder
	out.Grow(len(s))
	n := len(s)
	for i := 0; i < n; {
		switch {
		case strings.HasPrefix(s[i:], "--"):
			j := strings.IndexByte(s[i:], '\n')
			if j < 0 {
				i = n
			} else {
				i += j
			}
		case strings.HasPrefix(s[i:], "/-"):
			i = skipBlockComment(s, i, n)
		case s[i] == '"':
			j := skipString(s, i, n)
			out.WriteString(s[i:min(j, n)])
			i = j
		default:
			out.WriteByte(s[i])
			i++
		}
	}
	return out.String()
}

// declKind is the last command keyword in text[a:end], each keyword not part of a longer name.
func declKind(text string, a, end int) string {
	kind := "other"
	for i := a; i < end; i = next(text, i) {
		for _, kw := range keywords {
			j := i + len(kw)
			if j <= end && strings.HasPrefix(text[i:], kw) && wordBoundaryBefore(text, i, true) &&
				wordBoundaryAfter(text, j, end, true) {
				kind = kw
				i = j - 1
				break
			}
		}
	}
	return kind
}

// StatementText is a declaration's statement as written, comments dropped and white space collapsed: its signature
// for a theorem-like declaration, its whole text for a definition-like one (whose body is its meaning).
func StatementText(text string, start, end, sigFrom int, kind string) string {
	b := end
	if !slices.Contains(defKinds, kind) {
		if s, ok := bodyStart(text, sigFrom, next(text, end)); ok {
			b = s
		}
	}
	if b < start {
		return ""
	}
	return strings.Join(strings.FieldsFunc(stripComments(text[start:min(b, len(text))]), isSpace), " ")
}

// StatementHash is the hash a change lock records for a declaration it read: the first 16 hex digits of the
// SHA-256 of its StatementText.
func StatementHash(statement string) string {
	sum := sha256.Sum256([]byte(statement))
	return hex.EncodeToString(sum[:])[:16]
}
