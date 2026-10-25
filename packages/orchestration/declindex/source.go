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

func isWord(r rune) bool { return isAlnum(r) || r == '_' || r == '\'' }

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

// SourceShape is the shape of one declaration as its source writes it.
type SourceShape struct {
	Line  int
	Kind  string
	Shape string
}

// SourceHead is one type head written in a declaration's explicit binder or conclusion. Namespace and Opens are
// the lexical scope at Line, so ResolveHead can resolve it against another declaration set.
type SourceHead struct {
	Line      int
	Role      string
	Written   string
	Opens     []string
	Namespace string
}

type sourceLine struct {
	line       int
	start, end int
}

type sourceDeclaration struct {
	line                           int
	kind                           string
	start, end                     int
	keywordEnd, nameStart, nameEnd int
	commandLine                    int
}

// SourceShapes returns one normalized statement shape for every declaration in src.
func SourceShapes(src []byte) []SourceShape {
	text := string(src)
	decls := sourceDeclarations(text)
	out := make([]SourceShape, 0, len(decls))
	for _, d := range decls {
		statement := text[d.start:d.end]
		sigFrom := d.keywordEnd - d.start
		if d.nameStart < d.nameEnd {
			prefix := d.nameStart - d.start
			statement = statement[:prefix] + "_" + statement[d.nameEnd-d.start:]
			sigFrom = prefix + 1
		}
		out = append(out, SourceShape{
			Line:  d.line,
			Kind:  d.kind,
			Shape: StatementHash(StatementText(statement, 0, len(statement), sigFrom, d.kind)),
		})
	}
	return out
}

// SourceHeads returns the binder and conclusion type heads written by theorem-like source declarations.
func SourceHeads(src []byte) []SourceHead {
	text := string(src)
	decls := sourceDeclarations(text)
	byLine := make(map[int][]sourceDeclaration, len(decls))
	for _, d := range decls {
		byLine[d.commandLine] = append(byLine[d.commandLine], d)
	}
	s := newSource(text)
	lines := sourceLines(s)
	scopes := []sourceScope{{}}
	var out []SourceHead
	var pendingOpen []string
	var mutualEnds []int
	for i, line := range lines {
		word, end := sourceWord(text, line.start, line.end)
		if pendingOpen != nil {
			if decls := byLine[line.line]; len(decls) > 0 {
				out = appendSourceHeads(out, text, decls, sourceScopeWithOpens(scopes[len(scopes)-1], pendingOpen))
				pendingOpen = nil
				continue
			}
			if sourceAttributeLine(text, line) {
				continue
			}
			if word == "mutual" {
				if end := sourceMutualEnd(text, lines, i); end >= 0 {
					if len(mutualEnds) == 0 {
						scope := sourceScopeWithOpens(scopes[len(scopes)-1], pendingOpen)
						out = appendSourceHeadsInRange(out, text, decls, line.line, lines[end].line, scope)
					}
					mutualEnds = append(mutualEnds, end)
				}
				pendingOpen = nil
				continue
			}
			pendingOpen = nil
		}
		switch word {
		case "namespace":
			nameStart, nameEnd := sourceName(text, end, line.end)
			if nameStart == nameEnd {
				continue
			}
			parent := scopes[len(scopes)-1]
			name := text[nameStart:nameEnd]
			if strings.HasPrefix(name, "_root_.") {
				name = strings.TrimPrefix(name, "_root_.")
			} else if parent.namespace != "" {
				name = parent.namespace + "." + name
			}
			scopes = append(scopes, sourceScope{namespace: name, opens: append([]string(nil), parent.opens...)})
		case "section":
			parent := scopes[len(scopes)-1]
			scopes = append(scopes, sourceScope{namespace: parent.namespace, opens: append([]string(nil), parent.opens...)})
		case "mutual":
			if end := sourceMutualEnd(text, lines, i); end >= 0 {
				if len(mutualEnds) == 0 {
					out = appendSourceHeadsInRange(out, text, decls, line.line, lines[end].line, scopes[len(scopes)-1])
				}
				mutualEnds = append(mutualEnds, end)
			}
		case "end":
			if len(mutualEnds) > 0 && mutualEnds[len(mutualEnds)-1] == i {
				mutualEnds = mutualEnds[:len(mutualEnds)-1]
				continue
			}
			if len(scopes) > 1 {
				scopes = scopes[:len(scopes)-1]
			}
		case "open":
			if len(mutualEnds) > 0 {
				continue
			}
			names, local := sourceOpenNames(text, end, line.end)
			current := scopes[len(scopes)-1]
			names = sourceOpenNamesInScope(current, names)
			if local {
				if decls := byLine[line.line]; len(decls) > 0 {
					out = appendSourceHeads(out, text, decls, sourceScopeWithOpens(current, names))
				} else if len(names) > 0 {
					pendingOpen = names
				}
				continue
			}
			scope := &scopes[len(scopes)-1]
			scope.opens = append(scope.opens, names...)
		default:
			if len(mutualEnds) == 0 {
				out = appendSourceHeads(out, text, byLine[line.line], scopes[len(scopes)-1])
			}
		}
	}
	return out
}

// ResolveHead resolves h in the namespace and opened namespaces that its source had in scope. It returns the
// written name unchanged when none is declared.
func ResolveHead(h SourceHead, declared func(string) bool) string {
	written := h.Written
	rootWritten := strings.HasPrefix(written, "_root_.")
	if rootWritten {
		written = strings.TrimPrefix(written, "_root_.")
	}
	if !rootWritten {
		for namespace := strings.TrimPrefix(h.Namespace, "_root_."); namespace != ""; {
			candidate := namespace + "." + written
			if declared(candidate) {
				return candidate
			}
			i := strings.LastIndexByte(namespace, '.')
			if i < 0 {
				break
			}
			namespace = namespace[:i]
		}
	}
	if !rootWritten {
		for _, open := range h.Opens {
			open = strings.TrimPrefix(open, "_root_.")
			if open == "" {
				continue
			}
			candidate := open + "." + written
			if declared(candidate) {
				return candidate
			}
		}
	}
	if declared(written) {
		return written
	}
	return h.Written
}

type sourceScope struct {
	namespace string
	opens     []string
}

func sourceHeadKind(kind string) bool {
	switch kind {
	case "theorem", "lemma", "def", "abbrev", "structure":
		return true
	default:
		return false
	}
}

func appendSourceHeads(out []SourceHead, text string, decls []sourceDeclaration, scope sourceScope) []SourceHead {
	for _, d := range decls {
		if sourceHeadKind(d.kind) {
			out = append(out, declarationHeads(text, d, scope)...)
		}
	}
	return out
}

func appendSourceHeadsInRange(out []SourceHead, text string, decls []sourceDeclaration, firstLine, lastLine int, scope sourceScope) []SourceHead {
	for _, d := range decls {
		if d.commandLine > firstLine && d.commandLine < lastLine && sourceHeadKind(d.kind) {
			out = append(out, declarationHeads(text, d, scope)...)
		}
	}
	return out
}

func sourceScopeWithOpens(scope sourceScope, opens []string) sourceScope {
	return sourceScope{
		namespace: scope.namespace,
		opens:     append(append([]string(nil), scope.opens...), opens...),
	}
}

func sourceOpenNamesInScope(scope sourceScope, names []string) []string {
	out := make([]string, 0, len(names)*2)
	for _, name := range names {
		if strings.HasPrefix(name, "_root_.") {
			out = append(out, strings.TrimPrefix(name, "_root_."))
		} else if scope.namespace != "" {
			out = append(out, scope.namespace+"."+name, name)
		} else {
			out = append(out, name)
		}
	}
	return out
}

func sourceLines(s *source) []sourceLine {
	lines := make([]sourceLine, 0, len(s.starts))
	for line := range s.starts {
		text := s.line(line)
		if text == "" {
			continue
		}
		r, _ := utf8.DecodeRuneInString(text)
		if isSpace(r) || !lineHasCode(text) {
			continue
		}
		start := s.starts[line]
		lines = append(lines, sourceLine{line: line, start: start, end: start + len(text)})
	}
	return lines
}

func lineHasCode(text string) bool {
	i := skipSourceSpaceAndComments(text, 0, len(text))
	return i < len(text)
}

func sourceAttributeLine(text string, line sourceLine) bool {
	start := skipSourceSpaceAndComments(text, line.start, line.end)
	return strings.HasPrefix(text[start:line.end], "@[")
}

func sourceDeclarations(text string) []sourceDeclaration {
	s := newSource(text)
	lines := sourceLines(s)
	out := make([]sourceDeclaration, 0, len(lines))
	var mutualStarts []int
	for i, line := range lines {
		word, _ := sourceWord(text, line.start, line.end)
		switch word {
		case "mutual":
			mutualStarts = append(mutualStarts, line.line+1)
			continue
		case "end":
			if len(mutualStarts) > 0 {
				start := mutualStarts[len(mutualStarts)-1]
				mutualStarts = mutualStarts[:len(mutualStarts)-1]
				if len(mutualStarts) == 0 {
					out = append(out, sourceMutualDeclarations(s, start, line.line)...)
				}
				continue
			}
		}
		if len(mutualStarts) > 0 {
			continue
		}
		end := sourceDeclarationEnd(text, lines, i)
		if d, ok := sourceDeclarationAt(s, line.line, end); ok {
			out = append(out, d)
		}
	}
	return out
}

func sourceMutualDeclarations(s *source, firstLine, lastLine int) []sourceDeclaration {
	var candidates []sourceDeclaration
	indent := -1
	for line := firstLine; line < lastLine; line++ {
		d, ok := sourceDeclarationAt(s, line, s.starts[lastLine])
		if !ok {
			continue
		}
		lineIndent := sourceIndentation(s.line(line))
		if indent < 0 || lineIndent < indent {
			indent = lineIndent
		}
		candidates = append(candidates, d)
	}
	if indent < 0 {
		return nil
	}
	out := make([]sourceDeclaration, 0, len(candidates))
	for _, d := range candidates {
		if sourceIndentation(s.line(d.commandLine)) == indent {
			out = append(out, d)
		}
	}
	for i := range out {
		out[i].end = s.starts[lastLine]
		if i+1 < len(out) {
			out[i].end = out[i+1].start
		}
	}
	return out
}

func sourceDeclarationAt(s *source, line, end int) (sourceDeclaration, bool) {
	lineStart := s.starts[line]
	lineEnd := lineStart + len(s.line(line))
	kind, keywordEnd, ok := sourceDeclKeyword(s.text, lineStart, lineEnd)
	if !ok {
		return sourceDeclaration{}, false
	}
	startLine := decoratedDeclarationLine(s, line)
	nameStart, nameEnd := sourceName(s.text, keywordEnd, lineEnd)
	return sourceDeclaration{
		line:        startLine + 1,
		kind:        kind,
		start:       s.starts[startLine],
		end:         end,
		keywordEnd:  keywordEnd,
		nameStart:   nameStart,
		nameEnd:     nameEnd,
		commandLine: line,
	}, true
}

func sourceDeclarationEnd(text string, lines []sourceLine, current int) int {
	next := current + 1
	for next < len(lines) && sourceContinuationLine(text, lines[next]) {
		next++
	}
	if next < len(lines) {
		return lines[next].start
	}
	return len(text)
}

func sourceContinuationLine(text string, line sourceLine) bool {
	word, _ := sourceWord(text, line.start, line.end)
	switch word {
	case "where", "termination_by", "decreasing_by", "deriving":
		return true
	}
	start := skipSourceSpaceAndComments(text, line.start, line.end)
	return start < line.end && text[start] == '|'
}

func sourceIndentation(text string) int {
	indent := 0
	for _, r := range text {
		if !isSpace(r) {
			break
		}
		indent++
	}
	return indent
}

func sourceMutualEnd(text string, lines []sourceLine, start int) int {
	depth := 1
	for i := start + 1; i < len(lines); i++ {
		word, _ := sourceWord(text, lines[i].start, lines[i].end)
		switch word {
		case "mutual":
			depth++
		case "end":
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func decoratedDeclarationLine(s *source, line int) int {
	start := line
	indent := sourceIndentation(s.line(line))
	for line--; line >= 0; line-- {
		text := strings.TrimFunc(s.line(line), isSpace)
		if strings.HasPrefix(text, "@[") || strings.HasPrefix(text, "/-") {
			start = line
			continue
		}
		if decorationStart := sourceMultilineDecorationStart(s, line, indent); decorationStart >= 0 {
			start = decorationStart
			line = decorationStart
			continue
		}
		break
	}
	return start
}

func sourceMultilineDecorationStart(s *source, lastLine, indent int) int {
	last := strings.TrimFunc(s.line(lastLine), isSpace)
	if !strings.HasSuffix(last, "]") && !strings.HasSuffix(last, "-/") {
		return -1
	}
	lastEnd := s.starts[lastLine] + len(s.line(lastLine))
	for line := lastLine; line >= 0; line-- {
		text := s.line(line)
		if strings.TrimFunc(text, isSpace) == "" {
			return -1
		}
		lineIndent := sourceIndentation(text)
		if lineIndent < indent {
			return -1
		}
		if lineIndent > indent {
			continue
		}
		start := s.starts[line]
		lineEnd := start + len(text)
		for start < lineEnd {
			r, size := utf8.DecodeRuneInString(s.text[start:])
			if !isSpace(r) {
				break
			}
			start += size
		}
		switch {
		case strings.HasPrefix(s.text[start:], "@["):
			if close := matchingBracket(s.text, start+1, lastEnd); close >= 0 {
				return line
			}
		case strings.HasPrefix(s.text[start:], "/-"):
			if end := skipBlockComment(s.text, start, lastEnd); end <= lastEnd {
				return line
			}
		}
		return -1
	}
	return -1
}

func sourceDeclKeyword(text string, start, end int) (string, int, bool) {
	depth := 0
	for i := start; i < end; {
		switch {
		case strings.HasPrefix(text[i:], "--"):
			return "", 0, false
		case strings.HasPrefix(text[i:], "/-"):
			i = skipBlockComment(text, i, end)
			continue
		case text[i] == '"':
			i = skipString(text, i, end)
			continue
		}
		r, size := utf8.DecodeRuneInString(text[i:])
		switch {
		case strings.ContainsRune(openBrackets, r):
			depth++
		case strings.ContainsRune(closeBrackets, r):
			depth = max(0, depth-1)
		case depth == 0:
			for _, kind := range keywords {
				keywordEnd := i + len(kind)
				if keywordEnd <= end && strings.HasPrefix(text[i:], kind) && wordBoundaryBefore(text, i, true) &&
					(keywordEnd >= end || wordBoundaryAfterDot(text, keywordEnd, end)) {
					return kind, keywordEnd, true
				}
			}
		}
		i += size
	}
	return "", 0, false
}

func sourceWord(text string, start, end int) (string, int) {
	start = skipSourceSpaceAndComments(text, start, end)
	if start >= end {
		return "", start
	}
	r, _ := utf8.DecodeRuneInString(text[start:])
	if !isWord(r) {
		return "", start
	}
	i := next(text, start)
	for i < end {
		r, _ := utf8.DecodeRuneInString(text[i:])
		if !isWord(r) {
			break
		}
		i = next(text, i)
	}
	return text[start:i], i
}

func sourceName(text string, start, end int) (int, int) {
	start = skipSourceSpaceAndComments(text, start, end)
	if start >= end {
		return start, start
	}
	i := start
	for {
		if strings.HasPrefix(text[i:end], "«") {
			close := strings.Index(text[i:end], "»")
			if close < 0 {
				if i == start {
					return start, start
				}
				return start, i
			}
			i += close + len("»")
		} else {
			r, _ := utf8.DecodeRuneInString(text[i:])
			if !isWord(r) {
				if i == start {
					return start, start
				}
				return start, i
			}
			i = next(text, i)
			for i < end {
				r, _ := utf8.DecodeRuneInString(text[i:])
				if !isWord(r) {
					break
				}
				i = next(text, i)
			}
		}
		if i >= end || text[i] != '.' {
			return start, i
		}
		dot := i
		i++
		if i >= end {
			return start, dot
		}
		if strings.HasPrefix(text[i:end], "«") {
			continue
		}
		r, _ := utf8.DecodeRuneInString(text[i:])
		if !isWord(r) {
			return start, dot
		}
	}
}

func skipSourceSpaceAndComments(text string, start, end int) int {
	for start < end {
		switch {
		case strings.HasPrefix(text[start:], "--"):
			if lineEnd := strings.IndexByte(text[start:end], '\n'); lineEnd >= 0 {
				start += lineEnd + 1
			} else {
				return end
			}
		case strings.HasPrefix(text[start:], "/-"):
			start = skipBlockComment(text, start, end)
		default:
			r, size := utf8.DecodeRuneInString(text[start:])
			if !isSpace(r) {
				return start
			}
			start += size
		}
	}
	return start
}

func sourceOpenNames(text string, start, end int) ([]string, bool) {
	var names []string
	for start < end {
		nameStart, nameEnd := sourceName(text, start, end)
		if nameStart == nameEnd {
			return names, false
		}
		name := text[nameStart:nameEnd]
		if name == "scoped" {
			return names, false
		}
		if name == "in" {
			return names, true
		}
		names = append(names, name)
		start = nameEnd
	}
	return names, false
}

func declarationHeads(text string, d sourceDeclaration, scope sourceScope) []SourceHead {
	end := d.end
	if body, ok := bodyStart(text, d.keywordEnd, d.end); ok {
		end = body
	}
	start := d.nameEnd
	if start == d.nameStart {
		start = d.keywordEnd
	}
	lastColon := lastTopLevelColon(text, start, end)
	telescopeEnd := end
	if lastColon >= 0 {
		telescopeEnd = lastColon
	}
	var out []SourceHead
	depth := 0
	for i := start; i < telescopeEnd; {
		switch {
		case strings.HasPrefix(text[i:], "--"):
			i = skipSourceSpaceAndComments(text, i, telescopeEnd)
			continue
		case strings.HasPrefix(text[i:], "/-"):
			i = skipBlockComment(text, i, telescopeEnd)
			continue
		case text[i] == '"':
			i = skipString(text, i, telescopeEnd)
			continue
		}
		r, size := utf8.DecodeRuneInString(text[i:])
		switch {
		case r == '(' && depth == 0:
			if close := matchingBracket(text, i, telescopeEnd); close >= 0 {
				binder := text[i+size : close]
				if colon := topLevelColon(binder, 0, len(binder)); colon >= 0 {
					if head := sourceTypeHead(binder[colon+1:]); head != "" {
						out = append(out, sourceHead(d, "binder", head, scope))
					}
				}
			}
			depth++
		case strings.ContainsRune(openBrackets, r):
			depth++
		case strings.ContainsRune(closeBrackets, r):
			depth = max(0, depth-1)
		}
		i += size
	}
	if lastColon >= 0 {
		if head := sourceTypeHead(text[lastColon+1 : end]); head != "" {
			out = append(out, sourceHead(d, "conclusion", head, scope))
		}
	}
	return out
}

func sourceHead(d sourceDeclaration, role, written string, scope sourceScope) SourceHead {
	return SourceHead{
		Line:      d.line,
		Role:      role,
		Written:   written,
		Opens:     append([]string(nil), scope.opens...),
		Namespace: scope.namespace,
	}
}

func matchingBracket(text string, start, end int) int {
	depth := 0
	for i := start; i < end; {
		switch {
		case strings.HasPrefix(text[i:], "--"):
			i = skipSourceSpaceAndComments(text, i, end)
			continue
		case strings.HasPrefix(text[i:], "/-"):
			i = skipBlockComment(text, i, end)
			continue
		case text[i] == '"':
			i = skipString(text, i, end)
			continue
		}
		r, size := utf8.DecodeRuneInString(text[i:])
		switch {
		case strings.ContainsRune(openBrackets, r):
			depth++
		case strings.ContainsRune(closeBrackets, r):
			depth--
			if depth == 0 {
				return i
			}
		}
		i += size
	}
	return -1
}

func topLevelColon(text string, start, end int) int {
	depth := 0
	for i := start; i < end; {
		switch {
		case strings.HasPrefix(text[i:], "--"):
			i = skipSourceSpaceAndComments(text, i, end)
			continue
		case strings.HasPrefix(text[i:], "/-"):
			i = skipBlockComment(text, i, end)
			continue
		case text[i] == '"':
			i = skipString(text, i, end)
			continue
		}
		r, size := utf8.DecodeRuneInString(text[i:])
		switch {
		case strings.ContainsRune(openBrackets, r):
			depth++
		case strings.ContainsRune(closeBrackets, r):
			depth = max(0, depth-1)
		case r == ':' && depth == 0 && (i+size >= end || text[i+size] != '='):
			return i
		}
		i += size
	}
	return -1
}

func lastTopLevelColon(text string, start, end int) int {
	last := -1
	depth := 0
	for i := start; i < end; {
		switch {
		case strings.HasPrefix(text[i:], "--"):
			i = skipSourceSpaceAndComments(text, i, end)
			continue
		case strings.HasPrefix(text[i:], "/-"):
			i = skipBlockComment(text, i, end)
			continue
		case text[i] == '"':
			i = skipString(text, i, end)
			continue
		}
		r, size := utf8.DecodeRuneInString(text[i:])
		switch {
		case strings.ContainsRune(openBrackets, r):
			depth++
		case strings.ContainsRune(closeBrackets, r):
			depth = max(0, depth-1)
		case r == ':' && depth == 0 && (i+size >= end || text[i+size] != '='):
			last = i
		}
		i += size
	}
	return last
}

func sourceTypeHead(text string) string {
	text = strings.TrimFunc(stripComments(text), isSpace)
	for {
		if strings.HasPrefix(text, "∀") {
			comma := topLevelComma(text, len("∀"), len(text))
			if comma < 0 {
				return ""
			}
			text = strings.TrimFunc(text[comma+1:], isSpace)
			continue
		}
		if strings.HasPrefix(text, "(") {
			close := matchingBracket(text, 0, len(text))
			if close == len(text)-1 {
				text = strings.TrimFunc(text[1:close], isSpace)
				continue
			}
		}
		if strings.HasPrefix(text, "@") {
			text = strings.TrimFunc(text[1:], isSpace)
			continue
		}
		break
	}
	start, end := sourceName(text, 0, len(text))
	if start == end {
		return ""
	}
	return text[start:end]
}

func topLevelComma(text string, start, end int) int {
	depth := 0
	for i := start; i < end; {
		switch {
		case strings.HasPrefix(text[i:], "--"):
			i = skipSourceSpaceAndComments(text, i, end)
			continue
		case strings.HasPrefix(text[i:], "/-"):
			i = skipBlockComment(text, i, end)
			continue
		case text[i] == '"':
			i = skipString(text, i, end)
			continue
		}
		r, size := utf8.DecodeRuneInString(text[i:])
		switch {
		case strings.ContainsRune(openBrackets, r):
			depth++
		case strings.ContainsRune(closeBrackets, r):
			depth = max(0, depth-1)
		case r == ',' && depth == 0:
			return i
		}
		i += size
	}
	return -1
}
