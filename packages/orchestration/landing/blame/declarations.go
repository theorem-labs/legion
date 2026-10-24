package blame

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Declaration is one Lean declaration. It is identified by its file and its
// fully qualified name. Text is its normalised signature: the modifier lines
// above it, its declaration line and every continuation line up to the start
// of its body, joined with single spaces.
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
	declarationRE = regexp.MustCompile(`^\s*(?:@\[[^\]]*\]\s*)*((?:(?:private|protected|noncomputable|scoped|local|unsafe|partial|nonrec|public|meta)\s+)*)(class\s+inductive|class\s+abbrev|def|theorem|lemma|structure|class|inductive|abbrev|instance|axiom|opaque|notation|macro|syntax|macro_rules|elab|alias)\s+(?:\(priority\s*:=\s*[^)]*\)\s*)?([^\s(:{\[⟨]+)`)
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
	kind lineKind
	code string
	// namespace is the fully qualified namespace that a `namespace` line
	// opens or an `end` line closes.
	namespace string
	// owned is set on a line of the current declaration's signature or body,
	// and signature on a line of its signature.
	owned, signature bool
}

// scanner reads Lean source one line at a time. It tracks the namespaces,
// sections and mutual blocks that qualify declarations, the block comments and
// string literals that span lines, modifiers written on their own line above a
// declaration, and the declaration whose signature or body it is in.
type scanner struct {
	file   string
	scopes []scope
	depth  int
	// declarations holds the declarations read so far, in source order.
	// While inDeclaration is set the scanner is in the signature or body of
	// the last one, and while inSignature is also set its signature is still
	// open: signature holds its normalised text so far.
	declarations  []Declaration
	inDeclaration bool
	inSignature   bool
	signature     []byte
	// currentIsStructure is set when the current declaration was declared by
	// structure or class, and currentMatchArm when a line that starts with
	// `|` begins its body.
	currentIsStructure bool
	currentMatchArm    bool
	// inString is set inside a string literal. rawHashes is the number of #
	// that close a raw string literal, or -1 in an ordinary one.
	inString  bool
	rawHashes int
	// pendingPrivate, pendingProtected, and pendingText come from lines that
	// hold only attributes and modifiers; they apply to the next declaration.
	pendingPrivate   bool
	pendingProtected bool
	pendingText      string
}

// ScanDeclarations returns every declaration in one Lean source file, each with
// its fully qualified name and its whole signature.
func ScanDeclarations(file, source string) []Declaration {
	s := scanner{file: file}
	for line := range strings.SplitSeq(source, "\n") {
		s.scan(line)
	}
	return s.finish()
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

// scan reads one source line.
func (s *scanner) scan(line string) scanned {
	startedInString := s.inString
	code := s.uncomment(line)
	result := s.command(code, startedInString)
	result.code = code
	return result
}

// command reads the code of one source line; startedInString is set when the
// line starts inside a string literal.
func (s *scanner) command(code string, startedInString bool) scanned {
	if strings.TrimSpace(code) == "" {
		return scanned{}
	}
	if startedInString {
		// The line continues a string literal, so nothing on it is a command.
		s.pendingPrivate, s.pendingProtected = false, false
		s.pendingText = ""
		return s.continuation(code, false)
	}
	if modifierLineRE.MatchString(code) && !sectionRE.MatchString(code) {
		words := strings.Fields(attributeRE.ReplaceAllString(code, " "))
		s.pendingPrivate = s.pendingPrivate || containsString(words, "private")
		s.pendingProtected = s.pendingProtected || containsString(words, "protected")
		s.pendingText += strings.Join(strings.Fields(code), " ") + " "
		return scanned{}
	}
	pendingPrivate, pendingProtected, pendingText := s.pendingPrivate, s.pendingProtected, s.pendingText
	s.pendingPrivate, s.pendingProtected, s.pendingText = false, false, ""
	if match := namespaceRE.FindStringSubmatch(code); match != nil {
		s.endDeclaration()
		for _, part := range nameParts(match[1]) {
			s.scopes = append(s.scopes, scope{kind: namespaceScope, name: part})
		}
		return scanned{kind: namespaceLine, namespace: s.prefix()}
	}
	if match := endRE.FindStringSubmatch(code); match != nil {
		s.endDeclaration()
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
			return scanned{kind: namespaceLine, namespace: closed}
		}
		return scanned{}
	}
	if match := sectionRE.FindStringSubmatch(code); match != nil {
		s.endDeclaration()
		parts := nameParts(match[1])
		if len(parts) == 0 {
			parts = []string{""}
		}
		for _, part := range parts {
			s.scopes = append(s.scopes, scope{kind: sectionScope, name: part})
		}
		return scanned{}
	}
	if mutualRE.MatchString(code) {
		s.endDeclaration()
		s.scopes = append(s.scopes, scope{kind: mutualScope})
		return scanned{}
	}
	if match := declarationRE.FindStringSubmatch(code); match != nil {
		s.endDeclaration()
		modifiers := strings.Fields(match[1])
		s.declarations = append(s.declarations, Declaration{
			File:      s.file,
			Name:      s.qualify(strings.TrimSuffix(match[3], ".")),
			Private:   pendingPrivate || containsString(modifiers, "private"),
			Protected: pendingProtected || containsString(modifiers, "protected"),
		})
		s.inDeclaration, s.inSignature = true, true
		s.currentIsStructure = isStructureDecl(code)
		switch match[2] {
		case "def", "theorem", "lemma", "instance", "abbrev":
			s.currentMatchArm = true
		}
		s.signature = appendFields(appendFields(s.signature[:0], pendingText), code)
		if containsBodyStart(code) {
			s.endSignature()
		}
		return scanned{kind: declarationLine, owned: true, signature: true}
	}
	if commandRE.MatchString(code) {
		s.endDeclaration()
		return scanned{}
	}
	if s.inSignature && startsCommand(code) && !containsBodyStart(code) {
		// A signature continues on indented lines and on lines that start
		// with a bracket or an operator. An unindented word that does not
		// begin the body starts a command the scanner does not name.
		s.endDeclaration()
		return scanned{}
	}
	return s.continuation(code, true)
}

// continuation records a line of the current declaration after its
// declaration line: a signature line until the body starts, then a body line.
// A line that starts with `|` begins the body of a definition whose body can be
// a list of match arms; armMayStart is false on a line that starts inside a
// string literal.
func (s *scanner) continuation(code string, armMayStart bool) scanned {
	if !s.inDeclaration {
		return scanned{}
	}
	if !s.inSignature {
		return scanned{owned: true}
	}
	if armMayStart && s.currentMatchArm && startsMatchArm(code) {
		s.endSignature()
		return scanned{owned: true}
	}
	s.signature = appendFields(s.signature, code)
	if containsBodyStart(code) {
		s.endSignature()
	}
	return scanned{owned: true, signature: true}
}

// endSignature stores the open signature as the current declaration's Text.
func (s *scanner) endSignature() {
	if s.inSignature {
		s.declarations[len(s.declarations)-1].Text = string(s.signature)
		s.inSignature = false
	}
}

// endDeclaration leaves the current declaration, if any.
func (s *scanner) endDeclaration() {
	s.endSignature()
	s.inDeclaration = false
	s.currentIsStructure, s.currentMatchArm = false, false
}

// finish ends the source and returns its declarations.
func (s *scanner) finish() []Declaration {
	s.endDeclaration()
	return s.declarations
}

// appendFields appends the whitespace-separated fields of text to a
// normalised text, separated by single spaces.
func appendFields(normalised []byte, text string) []byte {
	for field := range strings.FieldsSeq(text) {
		if len(normalised) != 0 {
			normalised = append(normalised, ' ')
		}
		normalised = append(normalised, field...)
	}
	return normalised
}

// startsMatchArm reports whether a code line starts with the `|` of a match
// arm or a constructor.
func startsMatchArm(code string) bool {
	code = strings.TrimLeft(code, " \t")
	return code == "|" || strings.HasPrefix(code, "| ") || strings.HasPrefix(code, "|\t")
}

// startsCommand reports whether a code line starts in column 0 with a word,
// as a command does.
func startsCommand(code string) bool {
	first := code[0]
	return first == '_' || 'a' <= first && first <= 'z' || 'A' <= first && first <= 'Z'
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

// LineOwner describes one source line as the scan of its whole file found it.
type LineOwner struct {
	// Owner is the declaration whose signature or body holds the line. It is
	// zero for a line outside every declaration, for a blank or comment-only
	// line, and for a line of modifiers, which belong to the declaration below.
	Owner Declaration
	// Kind is what the line is: a declaration line, a line that opens or
	// closes a namespace, or another line.
	Kind        lineKind
	IsSignature bool // the declaration line or a continuation line before the body
	IsStructure bool // the owner was declared by structure or class
	// text is the line as written and code the line without its comments.
	// namespace is the fully qualified namespace that a namespace line opens
	// or closes.
	text, code, namespace string
}

// ScanLineOwners returns one LineOwner per source line, in order.
func ScanLineOwners(file, source string) []LineOwner {
	s := scanner{file: file}
	lines := strings.Split(source, "\n")
	owners := make([]LineOwner, len(lines))
	// owned holds, for each line, 1 + the index of its owner in
	// s.declarations, or 0. A declaration's Text is complete only when its
	// signature ends, so owners are filled in after the scan.
	owned := make([]int, len(lines))
	for index, line := range lines {
		result := s.scan(line)
		owners[index] = LineOwner{Kind: result.kind, text: line, code: result.code, namespace: result.namespace}
		if result.owned {
			owned[index] = len(s.declarations)
			owners[index].IsSignature = result.signature
			owners[index].IsStructure = s.currentIsStructure
		}
	}
	declarations := s.finish()
	for index, owner := range owned {
		if owner != 0 {
			owners[index].Owner = declarations[owner-1]
		}
	}
	return owners
}
