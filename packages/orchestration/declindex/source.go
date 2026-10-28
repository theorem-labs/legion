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

// isSpace is Python's str.isspace, the white space that the reference reader collapses: Unicode white space and the
// four ASCII separators.
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
		c = min(c, len(line))
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

// commandAt reports whether the source has a command at a range recorded in 0-based lines and UTF-16 columns: the
// range starts at its line's first non-blank character and ends after a non-blank one, at its line's end or before
// only blanks and a `--` comment. A line shorter than the recorded end fails, as does a range over lines that are
// gone. Two lines of the same length can still fool it; it is no hash of the source.
func (s *source) commandAt(l0, c0, l1, c1 int) bool {
	if l0 < 0 || l1 < l0 || l1 >= len(s.starts) {
		return false
	}
	first := s.line(l0)
	b0, _ := u16Col(first, c0)
	if b0 >= len(first) || strings.TrimFunc(first[:b0], isSpace) != "" {
		return false
	}
	if r, _ := utf8.DecodeRuneInString(first[b0:]); isSpace(r) {
		return false
	}
	last := s.line(l1)
	if u16Len(last) < c1 {
		return false
	}
	b1, _ := u16Col(last, c1)
	if b1 == 0 {
		return false
	}
	if r, _ := utf8.DecodeLastRuneInString(last[:b1]); isSpace(r) {
		return false
	}
	rest := strings.TrimLeftFunc(last[b1:], isSpace)
	return rest == "" || strings.HasPrefix(rest, "--")
}

// u16Len is the length of line in UTF-16 code units.
func u16Len(line string) int {
	if isASCII(line) {
		return len(line)
	}
	n := 0
	for _, r := range line {
		n++
		if r > 0xFFFF {
			n++
		}
	}
	return n
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

// literalEnd is the offset after the literal that starts at i, and false when none starts there: a string ("…",
// with its escapes), a raw string (r"…", r#"…"#) or a character ('a', '\n', '\x41'). A `'` after a name's rune is
// the name's prime (h'), and one that another `'` follows is notation's (Mathlib's set image): neither starts a
// character.
func literalEnd(text string, i, n int) (int, bool) {
	switch {
	case text[i] == '"':
		return skipString(text, i, n), true
	case text[i] == 'r' && wordBoundaryBefore(text, i, true):
		j := i + 1
		for j < n && text[j] == '#' {
			j++
		}
		if j >= n || text[j] != '"' {
			return 0, false
		}
		closing := `"` + strings.Repeat("#", j-i-1)
		if k := strings.Index(text[j+1:n], closing); k >= 0 {
			return j + 1 + k + len(closing), true
		}
		return n, true
	case text[i] == '\'' && !nameRuneBefore(text, i) && i+1 < n:
		j := i + 1
		if text[j] == '\\' {
			// The escaped rune, then the hex digits of a `\x41` or `\u03B1`.
			j = next(text, j+1)
			for j < n && strings.IndexByte("0123456789abcdefABCDEF", text[j]) >= 0 {
				j++
			}
		} else if r, size := utf8.DecodeRuneInString(text[j:n]); r != '\'' && r != '\n' {
			j += size
		} else {
			return 0, false
		}
		if j < n && text[j] == '\'' {
			return j + 1, true
		}
	}
	return 0, false
}

// nameRuneBefore reports whether the rune before i continues a name: a word rune, or the `'`, `!` or `?` that a name
// can hold.
func nameRuneBefore(text string, i int) bool {
	if i == 0 {
		return false
	}
	r, _ := utf8.DecodeLastRuneInString(text[:i])
	return isWord(r) || r == '\'' || r == '!' || r == '?'
}

// skipLineComment returns the offset of the newline that ends the `--` comment at i, or n when no newline follows.
func skipLineComment(text string, i, n int) int {
	if j := strings.IndexByte(text[i:], '\n'); j >= 0 {
		return i + j
	}
	return n
}

// bodyStart is the offset of the first `:=`, line-initial `|` or `where` at bracket depth 0 in text[i:end],
// skipping comments and literals: where a declaration's statement ends and its body begins.
func bodyStart(text string, i, end int) (int, bool) {
	depth, n := 0, min(len(text), end)
	for i < n {
		rest := text[i:]
		switch {
		case strings.HasPrefix(rest, "--"):
			i = skipLineComment(text, i, n)
			continue
		case strings.HasPrefix(rest, "/-"):
			i = skipBlockComment(text, i, n)
			continue
		}
		if j, ok := literalEnd(text, i, n); ok {
			i = j
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

// normalize drops `--` line comments and nested `/- -/` block comments (docstrings included), keeps each literal as
// written (literalEnd), and collapses each run of white space between tokens to one space, with none at either end.
func normalize(s string) string {
	var out strings.Builder
	out.Grow(len(s))
	n := len(s)
	space := false // white space since the last token written
	for i := 0; i < n; {
		switch {
		case strings.HasPrefix(s[i:], "--"):
			i = skipLineComment(s, i, n)
			continue
		case strings.HasPrefix(s[i:], "/-"):
			i = skipBlockComment(s, i, n)
			continue
		}
		var token string
		if j, ok := literalEnd(s, i, n); ok {
			token, i = s[i:min(j, n)], j
		} else {
			r, size := utf8.DecodeRuneInString(s[i:])
			if isSpace(r) {
				space, i = true, i+size
				continue
			}
			token, i = s[i:i+size], i+size
		}
		if space && out.Len() > 0 {
			out.WriteByte(' ')
		}
		space = false
		out.WriteString(token)
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

// StatementText is a declaration's statement as written, comments dropped and the white space between its tokens
// collapsed (a literal keeps its own): its signature for a theorem-like declaration, its whole text for a
// definition-like one (whose body is its meaning).
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
	return normalize(text[start:min(b, len(text))])
}

// StatementHash is the hash a change lock records for a declaration it read: the first 16 hex digits of the
// SHA-256 of its StatementText.
func StatementHash(statement string) string {
	sum := sha256.Sum256([]byte(statement))
	return hex.EncodeToString(sum[:])[:16]
}
