package blame

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Declaration is one Lean declaration. It is identified by its file and its
// fully qualified name; Text is its normalised declaration line.
type Declaration struct {
	File      string
	Name      string
	Text      string
	Private   bool
	Protected bool
}

// DeclarationID identifies a declaration in one Lean source file.
type DeclarationID struct {
	File string
	Name string
}

// ID returns the declaration's file-qualified identity.
func (d Declaration) ID() DeclarationID {
	return DeclarationID{File: d.File, Name: d.Name}
}

var (
	declarationRE = regexp.MustCompile(`^\s*(?:@\[[^\]]*\]\s*)*((?:(?:private|protected|noncomputable|scoped|local|unsafe|partial|nonrec|public|meta)\s+)*)(?:class\s+inductive|class\s+abbrev|def|theorem|lemma|structure|class|inductive|abbrev|instance|axiom|opaque|notation|macro|syntax|macro_rules|elab|alias)\s+(?:\(priority\s*:=\s*[^)]*\)\s*)?([^\s(:{\[⟨]+)`)
	namespaceRE   = regexp.MustCompile(`^\s*namespace\s+(\S+)\s*$`)
	sectionRE     = regexp.MustCompile(`^\s*(?:@\[[^\]]*\]\s*)*(?:(?:noncomputable|public|private|meta)\s+)*section(?:\s+(\S+))?\s*$`)
	mutualRE      = regexp.MustCompile(`^\s*mutual\s*$`)
	endRE         = regexp.MustCompile(`^\s*end(?:\s+(\S+))?\s*$`)
	// modifierLineRE is a line of only attributes and modifiers, which apply
	// to the declaration below it.
	modifierLineRE = regexp.MustCompile(`^\s*(?:(?:@\[[^\]]*\]|private|protected|noncomputable|scoped|local|unsafe|partial|nonrec|public|meta)(?:\s+|$))+$`)
	attributeRE    = regexp.MustCompile(`@\[[^\]]*\]`)
	// commandRE is an unindented command that ends the previous declaration's
	// body without declaring a name.
	commandRE = regexp.MustCompile(`^(?:#|@\[|(?:open|variable|universe|set_option|attribute|export|example|instance|deriving|initialize|import|omit|include)\b)`)
)

type scopeKind int

const (
	namespaceScope scopeKind = iota
	sectionScope
	mutualScope
)

type scope struct {
	kind scopeKind
	name string
}

type lineKind int

const (
	otherLine lineKind = iota
	declarationLine
	namespaceLine
)

// scanned is what one source line did to the scanner.
type scanned struct {
	kind        lineKind
	code        string
	declaration Declaration
	// namespace is the fully qualified namespace that a `namespace` line
	// opens or an `end` line closes.
	namespace string
}

// scanner reads Lean source one line at a time. It tracks the namespaces,
// sections and mutual blocks that qualify declarations, the block comments and
// string literals that span lines, modifiers written on their own line above a
// declaration, and the declaration whose body it is in.
type scanner struct {
	file    string
	scopes  []scope
	depth   int
	current Declaration
	// inString is set inside a string literal. rawHashes is the number of #
	// that close a raw string literal, or -1 in an ordinary one.
	inString  bool
	rawHashes int
	// pendingPrivate, pendingProtected, and pendingText come from lines that
	// hold only attributes and modifiers; they apply to the next declaration.
	pendingPrivate   bool
	pendingProtected bool
	pendingText      string
	// currentIsStructure is true when current was declared by structure or class.
	currentIsStructure bool
}

// ScanDeclarations returns every declaration in one Lean source file, each with
// its fully qualified name.
func ScanDeclarations(file, source string) []Declaration {
	s := scanner{file: file}
	var declarations []Declaration
	for _, line := range strings.Split(source, "\n") {
		if result := s.scan(line); result.kind == declarationLine {
			declarations = append(declarations, result.declaration)
		}
	}
	return declarations
}

// DeclarationsFor returns the declarations that a diagnostic reference can
// name. A qualified reference is exact; a bare reference may be relative to an
// open namespace.
func DeclarationsFor(reference string, declarations []Declaration) []Declaration {
	out := make([]Declaration, 0, len(declarations))
	for _, declaration := range declarations {
		if Refers(reference, declaration) {
			out = append(out, declaration)
		}
	}
	return out
}

// Refers reports whether a diagnostic name can refer to the declaration. Lean
// prints a name relative to the namespaces open where it reports the error, so
// a reference matches a declaration whose fully qualified name ends in it at a
// component boundary. A protected declaration is never reachable by its last
// component alone.
func Refers(reference string, declaration Declaration) bool {
	return refers(reference, declaration.Name, declaration.Protected)
}

func refers(reference, name string, protected bool) bool {
	reference, name = normaliseModule(reference), normaliseModule(name)
	if reference == name {
		return true
	}
	if !strings.HasSuffix(name, "."+reference) {
		return false
	}
	return strings.Contains(reference, ".") || !protected
}

// refersToNamespace reports whether a name lies in the namespace: some leading
// part of the name, as written, refers to the namespace's qualified name.
func refersToNamespace(reference, namespace string) bool {
	prefix := normaliseModule(reference)
	for prefix != "" {
		if refers(prefix, namespace, false) {
			return true
		}
		index := strings.LastIndex(prefix, ".")
		if index < 0 {
			break
		}
		prefix = prefix[:index]
	}
	return false
}

func (s *scanner) scan(line string) scanned {
	startedInString := s.inString
	code := s.uncomment(line)
	if strings.TrimSpace(code) == "" {
		return scanned{code: code}
	}
	if startedInString {
		// The line continues a string literal, so nothing on it is a command.
		s.pendingPrivate, s.pendingProtected = false, false
		s.pendingText = ""
		return scanned{code: code}
	}
	if modifierLineRE.MatchString(code) && !sectionRE.MatchString(code) {
		words := strings.Fields(attributeRE.ReplaceAllString(code, " "))
		s.pendingPrivate = s.pendingPrivate || containsString(words, "private")
		s.pendingProtected = s.pendingProtected || containsString(words, "protected")
		s.pendingText += strings.Join(strings.Fields(code), " ") + " "
		return scanned{code: code}
	}
	pendingPrivate, pendingProtected, pendingText := s.pendingPrivate, s.pendingProtected, s.pendingText
	s.pendingPrivate, s.pendingProtected, s.pendingText = false, false, ""
	if match := namespaceRE.FindStringSubmatch(code); match != nil {
		s.current = Declaration{}
		s.currentIsStructure = false
		for _, part := range nameParts(match[1]) {
			s.scopes = append(s.scopes, scope{kind: namespaceScope, name: part})
		}
		return scanned{kind: namespaceLine, code: code, namespace: s.prefix()}
	}
	if match := endRE.FindStringSubmatch(code); match != nil {
		s.current = Declaration{}
		s.currentIsStructure = false
		parts := nameParts(match[1])
		closed := s.prefix()
		closesNamespace := len(s.scopes) == 0 && len(parts) != 0
		if closesNamespace {
			closed = strings.Join(parts, ".")
		}
		for range max(len(parts), 1) {
			if len(s.scopes) == 0 {
				break
			}
			closesNamespace = closesNamespace || s.scopes[len(s.scopes)-1].kind == namespaceScope
			s.scopes = s.scopes[:len(s.scopes)-1]
		}
		if closesNamespace {
			return scanned{kind: namespaceLine, code: code, namespace: closed}
		}
		return scanned{code: code}
	}
	if match := sectionRE.FindStringSubmatch(code); match != nil {
		s.current = Declaration{}
		s.currentIsStructure = false
		parts := nameParts(match[1])
		if len(parts) == 0 {
			parts = []string{""}
		}
		for _, part := range parts {
			s.scopes = append(s.scopes, scope{kind: sectionScope, name: part})
		}
		return scanned{code: code}
	}
	if mutualRE.MatchString(code) {
		s.current = Declaration{}
		s.currentIsStructure = false
		s.scopes = append(s.scopes, scope{kind: mutualScope})
		return scanned{code: code}
	}
	if match := declarationRE.FindStringSubmatch(code); match != nil {
		modifiers := strings.Fields(match[1])
		text := strings.TrimSpace(pendingText + strings.Join(strings.Fields(code), " "))
		declaration := Declaration{
			File:      s.file,
			Name:      s.qualify(strings.TrimSuffix(match[2], ".")),
			Text:      text,
			Private:   pendingPrivate || containsString(modifiers, "private"),
			Protected: pendingProtected || containsString(modifiers, "protected"),
		}
		s.current = declaration
		s.currentIsStructure = isStructureDecl(code)
		return scanned{kind: declarationLine, code: code, declaration: declaration}
	}
	if commandRE.MatchString(code) {
		s.current = Declaration{}
		s.currentIsStructure = false
	}
	return scanned{code: code}
}

// uncomment returns a line's code: it removes block comments, which may span
// lines and nest, and a trailing line comment. Comment markers inside string
// and character literals are code. A string literal, including a raw string
// `r#"…"#`, may span lines, so the scanner carries it to the next line.
func (s *scanner) uncomment(line string) string {
	var out strings.Builder
	for index := 0; index < len(line); index++ {
		current := line[index]
		next := byte(0)
		if index+1 < len(line) {
			next = line[index+1]
		}
		switch {
		case s.depth > 0 && current == '-' && next == '/':
			s.depth--
			index++
		case s.depth > 0 && current == '/' && next == '-':
			s.depth++
			index++
		case s.depth > 0:
		case s.inString && s.rawHashes >= 0:
			out.WriteByte(current)
			if current == '"' && strings.HasPrefix(line[index+1:], strings.Repeat("#", s.rawHashes)) {
				out.WriteString(line[index+1 : index+1+s.rawHashes])
				index += s.rawHashes
				s.inString = false
			}
		case s.inString && current == '\\' && next != 0:
			out.WriteByte(current)
			out.WriteByte(next)
			index++
		case s.inString:
			out.WriteByte(current)
			if current == '"' {
				s.inString = false
			}
		case current == '"':
			s.inString, s.rawHashes = true, -1
			out.WriteByte(current)
		case current == 'r' && !identifierBefore(line, index):
			hashes := 0
			for index+1+hashes < len(line) && line[index+1+hashes] == '#' {
				hashes++
			}
			if index+1+hashes < len(line) && line[index+1+hashes] == '"' {
				out.WriteString(line[index : index+2+hashes])
				index += 1 + hashes
				s.inString, s.rawHashes = true, hashes
				continue
			}
			out.WriteByte(current)
		case current == '\'' && !identifierBefore(line, index):
			end := charLiteralEnd(line, index)
			if end < 0 {
				out.WriteByte(current)
				continue
			}
			out.WriteString(line[index : end+1])
			index = end
		case current == '/' && next == '-':
			s.depth++
			index++
		case current == '-' && next == '-':
			return out.String()
		default:
			out.WriteByte(current)
		}
	}
	return out.String()
}

// identifierBefore reports whether the byte before index continues an
// identifier, so a quote or `r` there is part of a name, not a literal.
func identifierBefore(line string, index int) bool {
	if index == 0 {
		return false
	}
	previous := line[index-1]
	if previous >= utf8.RuneSelf {
		return true
	}
	return previous == '_' || previous == '\'' || previous == '!' || previous == '?' || unicode.IsLetter(rune(previous)) || unicode.IsDigit(rune(previous))
}

// charLiteralEnd returns the index of the closing quote of a character
// literal that opens at start, such as 'a', '"' or '\x41', or -1 when the quote
// does not open one (a name literal such as 'Nat.succ).
func charLiteralEnd(line string, start int) int {
	index := start + 1
	if index >= len(line) {
		return -1
	}
	if line[index] == '\\' {
		index++
		if index >= len(line) {
			return -1
		}
		switch line[index] {
		case 'x':
			index += 3
		case 'u':
			index += 5
		default:
			index++
		}
	} else {
		character, size := utf8.DecodeRuneInString(line[index:])
		if character == '\'' {
			return -1
		}
		index += size
	}
	if index < len(line) && line[index] == '\'' {
		return index
	}
	return -1
}

func (s *scanner) prefix() string {
	parts := make([]string, 0, len(s.scopes))
	for _, scope := range s.scopes {
		if scope.kind == namespaceScope {
			parts = append(parts, scope.name)
		}
	}
	return strings.Join(parts, ".")
}

func (s *scanner) qualify(name string) string {
	name = normaliseModule(name)
	if root, found := strings.CutPrefix(name, "_root_."); found {
		return root
	}
	if prefix := s.prefix(); prefix != "" {
		return prefix + "." + name
	}
	return name
}

func nameParts(name string) []string {
	name = normaliseModule(name)
	if name == "" {
		return nil
	}
	return strings.Split(name, ".")
}

func containsString(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

// diffPath is a file name from a diff's `---` or `+++` line without its side
// prefix; /dev/null is "".
func diffPath(path, prefix string) string {
	path = strings.TrimSpace(path)
	if strings.HasPrefix(path, `"`) {
		if unquoted, err := strconv.Unquote(path); err == nil {
			path = unquoted
		}
	}
	if path == "/dev/null" {
		return ""
	}
	return strings.TrimPrefix(path, prefix)
}

// isStructureDecl reports whether a code line declares a structure or class.
func isStructureDecl(code string) bool {
	fields := strings.Fields(code)
	for _, word := range fields {
		switch word {
		case "structure", "class":
			return true
		case "def", "theorem", "lemma", "inductive", "abbrev", "instance",
			"axiom", "opaque", "notation", "macro", "syntax", "macro_rules",
			"elab", "alias":
			return false
		}
	}
	return false
}

// containsBodyStart reports whether a code line (already uncommented) begins
// the definition body. It detects `:=` at bracket depth zero and a trailing
// `where`.
func containsBodyStart(code string) bool {
	depth := 0
	for i := 0; i < len(code); i++ {
		switch code[i] {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			if depth > 0 {
				depth--
			}
		case ':':
			if depth == 0 && i+1 < len(code) && code[i+1] == '=' {
				return true
			}
		}
	}
	trimmed := strings.TrimSpace(code)
	return trimmed == "where" || strings.HasSuffix(trimmed, " where")
}

// LineOwner describes which declaration owns a source line and what part of
// the declaration the line belongs to.
type LineOwner struct {
	Owner       Declaration
	IsSignature bool // true for the header and continuation lines before := or where
	IsStructure bool // true when the owner was declared by structure or class
}

// ScanLineOwners returns one LineOwner per source line (0-indexed). Lines
// outside any declaration's body have a zero owner. Comment-only and blank
// lines inside a declaration keep the owner but are marked as non-signature
// body lines.
func ScanLineOwners(file, source string) []LineOwner {
	s := scanner{file: file}
	lines := strings.Split(source, "\n")
	owners := make([]LineOwner, len(lines))
	inSignature := false
	for i, line := range lines {
		wasInString := s.inString
		result := s.scan(line)
		code := strings.TrimSpace(result.code)
		switch {
		case result.kind == declarationLine:
			inSignature = !containsBodyStart(result.code)
			owners[i] = LineOwner{
				Owner:       result.declaration,
				IsSignature: true,
				IsStructure: s.currentIsStructure,
			}
		case s.current.Name == "":
			inSignature = false
		case code == "":
			// Blank or comment-only line: no evidence.
		case !wasInString && modifierLineRE.MatchString(result.code) && !sectionRE.MatchString(result.code):
			// Modifier/attribute line: belongs to the next declaration.
		default:
			if inSignature && containsBodyStart(result.code) {
				owners[i] = LineOwner{Owner: s.current, IsSignature: true, IsStructure: s.currentIsStructure}
				inSignature = false
				continue
			}
			owners[i] = LineOwner{Owner: s.current, IsSignature: inSignature, IsStructure: s.currentIsStructure}
		}
	}
	return owners
}
