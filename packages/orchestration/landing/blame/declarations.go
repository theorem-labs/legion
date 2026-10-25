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
	Kind      string   // the command keyword: def, theorem, structure, class, …
	Line      int      // 1-based line of the keyword
	Fields    []string // structure or class: its own field names, in order
	Extends   bool     // structure or class: it has an `extends` clause
	// Generated is set when Name is the name Lean generates for an
	// anonymous instance, computed from its signature alone. Lean numbers a
	// repeated name by what the whole namespace holds, so such a name is
	// Lean's only while no other file of the namespace generates it, which
	// AmbiguousInstanceNames decides over a whole tree.
	Generated bool
}

// InstanceNames is a set of generated instance names.
type InstanceNames map[string]struct{}

// AmbiguousInstanceNames returns the generated instance names that more than
// one file of declarations generates. Lean gives such a name to one of those
// instances and numbers the rest, and a scan of one file cannot tell which,
// so no declaration may keep it. Build it once per round over the candidate
// tree, and use that one set for every scan of the round, so that the base
// tree, the candidate tree and each member's evidence name a declaration
// alike.
func AmbiguousInstanceNames(declarations []Declaration) InstanceNames {
	files := make(map[string]string, len(declarations))
	ambiguous := InstanceNames{}
	for _, declaration := range declarations {
		if !declaration.Generated {
			continue
		}
		switch first, found := files[declaration.Name]; {
		case !found:
			files[declaration.Name] = declaration.File
		case first != declaration.File:
			ambiguous[declaration.Name] = struct{}{}
		}
	}
	return ambiguous
}

// ResolveInstanceNames renames in place every declaration whose generated
// name is ambiguous, giving it the positional name its file's scan would have
// given it. Declarations of one file must keep their source order.
func ResolveInstanceNames(declarations []Declaration, ambiguous InstanceNames) {
	positions := map[string]int{}
	for index, declaration := range declarations {
		if !declaration.Generated && !isPositionalInstanceName(declaration.Name) {
			continue
		}
		positions[declaration.File]++
		if _, found := ambiguous[declaration.Name]; !found || !declaration.Generated {
			continue
		}
		namespace, _ := splitLastComponent(declaration.Name)
		declarations[index].Name = positionalInstanceName(namespace, positions[declaration.File])
		declarations[index].Generated = false
	}
}

// positionalInstanceName is the name of the nth anonymous instance of a file,
// in a namespace. `<anon-instance>` is not a Lean identifier, so the name
// never equals one Lean generates.
func positionalInstanceName(namespace string, position int) string {
	return qualifyIn(namespace, anonymousInstanceMarker+strconv.Itoa(position))
}

func isPositionalInstanceName(name string) bool {
	return strings.Contains(name, anonymousInstanceMarker)
}

// splitLastComponent splits a qualified name into its namespace and its last
// component.
func splitLastComponent(name string) (string, string) {
	if dot := strings.LastIndex(name, "."); dot >= 0 {
		return name[:dot], name[dot+1:]
	}
	return "", name
}

const anonymousInstanceMarker = "<anon-instance>#"

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
	// commandRE is an unindented command that ends the previous declaration
	// without declaring a name. Each word is a Lean keyword, so no term
	// continuing a signature can start with it. A syntax line that names a
	// declaration is read by declarationRE first, and an instance by
	// declarationRE or anonymousInstanceRE.
	commandRE = regexp.MustCompile(`^(?:#|@\[|(?:open|variable|universe|set_option|attribute|export|example|deriving|initialize|import|omit|include)\b|(?:(?:local|scoped)\s+)?(?:macro_rules|elab_rules|syntax|declare_syntax_cat)\b)`)
	// anonymousInstanceRE is the line of an instance declared without a
	// name, and instancePrefixRE the words before such an instance's binders
	// in its signature.
	anonymousInstanceRE = regexp.MustCompile(`^\s*(?:@\[[^\]]*\]\s*)*((?:(?:private|protected|noncomputable|scoped|local|unsafe|partial|nonrec|public|meta)\s+)*)instance\b\s*(?:\(priority\s*:=\s*[^)]*\)\s*)?(?:[:(\[{⦃]|$)`)
	instancePrefixRE    = regexp.MustCompile(`^(?:@\[[^\]]*\]\s*)*(?:(?:private|protected|noncomputable|scoped|local|unsafe|partial|nonrec|public|meta)\s+)*instance\b\s*(?:\(priority\s*:=\s*[^)]*\)\s*)?`)
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
	// ambiguous holds the generated instance names that no declaration may
	// keep, because more than one file of the round's candidate tree
	// generates them.
	ambiguous InstanceNames
	file      string
	scopes    []scope
	depth     int
	// line is the 1-based number of the line being scanned.
	line int
	// fieldColumn is the indentation of the current structure or class
	// body's first line, or -1 until that line is read.
	fieldColumn int
	// declarations holds the declarations read so far, in source order.
	// While inDeclaration is set the scanner is in the signature or body of
	// the last one, and while inSignature is also set its signature is still
	// open: signature holds its normalised text so far.
	declarations  []Declaration
	inDeclaration bool
	inSignature   bool
	signature     []byte
	// brackets is the depth of the brackets the current declaration's lines
	// leave open, column the indentation of its declaration line, and
	// lastCode its last code line. blockColumn is the indentation of the
	// block that its last line ending in `where` or `do` opened, or -1, and
	// blockPending is set while that block's first line is still to come.
	brackets     int
	column       int
	lastCode     string
	blockColumn  int
	blockPending bool
	// currentIsStructure is set when the current declaration was declared by
	// structure or class, currentMatchArm when a line that starts with `|`
	// begins its body, and currentArrow when `=>` begins its body, as in a
	// macro, an elab or a notation, whose expansion follows the arrow.
	currentIsStructure bool
	currentMatchArm    bool
	currentArrow       bool
	// anonymous is set while the current declaration is an instance without
	// a name; anonymousInstances holds the file's anonymous instances, which
	// nameAnonymousInstances names once the whole file is read.
	anonymous          bool
	anonymousInstances []anonymousInstance
	// variables holds the names a `variable` command binds. Lean includes a
	// section variable in no generated name. The scanner keeps them for the
	// whole file: a name that a section no longer binds still contributes
	// nothing, which is the safe direction.
	variables map[string]bool
	// inString is set inside a string literal. rawHashes is the number of #
	// that close a raw string literal, or -1 in an ordinary one. literals
	// holds the byte spans of the current line's code that are string or
	// character literals.
	inString  bool
	rawHashes int
	literals  [][2]int
	// pendingPrivate, pendingProtected, and pendingText come from lines that
	// hold only attributes and modifiers; they apply to the next declaration.
	pendingPrivate   bool
	pendingProtected bool
	pendingText      string
}

// ScanDeclarations returns every declaration in one Lean source file, each with
// its fully qualified name and its whole signature. ambiguous holds the
// generated instance names that no declaration may keep, from
// AmbiguousInstanceNames over the round's candidate tree; it is nil for a
// scan that has no tree to compare with, such as a single file's.
func ScanDeclarations(file, source string, ambiguous InstanceNames) []Declaration {
	s := scanner{file: file, ambiguous: ambiguous}
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
	s.line++
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
		modifiers := strings.Fields(match[1])
		s.startDeclaration(code, pendingText, s.qualify(strings.TrimSuffix(match[3], ".")), strings.Join(strings.Fields(match[2]), " "), pendingPrivate || containsString(modifiers, "private"), pendingProtected || containsString(modifiers, "protected"))
		s.currentIsStructure = isStructureDecl(code)
		switch match[2] {
		case "def", "theorem", "lemma", "instance", "abbrev":
			s.currentMatchArm = true
		case "macro", "elab", "notation":
			s.currentArrow = true
		}
		s.signatureLine(code)
		return scanned{kind: declarationLine, owned: true, signature: true}
	}
	if match := anonymousInstanceRE.FindStringSubmatch(code); match != nil {
		modifiers := strings.Fields(match[1])
		s.startDeclaration(code, pendingText, "", "instance", pendingPrivate || containsString(modifiers, "private"), pendingProtected || containsString(modifiers, "protected"))
		s.currentMatchArm, s.anonymous = true, true
		s.signatureLine(code)
		return scanned{kind: declarationLine, owned: true, signature: true}
	}
	if commandRE.MatchString(code) || s.inDeclaration && s.startsCommand(code) {
		s.endDeclaration()
		s.recordVariables(code)
		return scanned{}
	}
	return s.continuation(code, true)
}

// recordVariables adds the names a `variable` command binds, which Lean
// includes in no generated name. The names are those bound before each
// group's `:`, outside every bracket of the group.
func (s *scanner) recordVariables(code string) {
	trimmed := strings.TrimLeft(code, " \t")
	if word := firstWord(trimmed); word != "variable" && word != "variables" {
		return
	}
	for _, group := range splitBinderGroups(trimmed) {
		names := group[1 : len(group)-1]
		if colon := topLevelIndex(group[1:], ":"); colon >= 0 && !strings.HasPrefix(group[1+colon:], ":=") {
			names = group[1 : 1+colon]
		} else if group[0] == '[' {
			// An instance binder without a name binds none.
			continue
		}
		for _, name := range strings.Fields(names) {
			if s.variables == nil {
				s.variables = map[string]bool{}
			}
			s.variables[name] = true
		}
	}
}

// startDeclaration ends the current declaration and starts one named name,
// declared by the keyword kind, on a declaration line. An anonymous
// instance's name is set by nameAnonymousInstances, once the whole file is
// read.
func (s *scanner) startDeclaration(code, pendingText, name, kind string, private, protected bool) {
	s.endDeclaration()
	s.declarations = append(s.declarations, Declaration{File: s.file, Name: name, Private: private, Protected: protected, Kind: kind, Line: s.line})
	s.inDeclaration, s.inSignature = true, true
	s.brackets, s.column = 0, indentation(code)
	s.blockColumn, s.blockPending = -1, false
	s.fieldColumn = -1
	s.signature = appendFields(appendFields(s.signature[:0], pendingText), code)
}

// signatureLine reads the declaration line, whose code is already in the
// signature.
func (s *scanner) signatureLine(code string) {
	if s.track(code) {
		s.endSignature()
	}
	s.endLine(code)
}

// continuation records a line of the current declaration after its
// declaration line: a signature line until the body starts, then a body line.
// Lean continues a signature on any line, indented or not, until the `:=` or
// `where` that starts the body. A line that starts with `|` begins the body of
// a definition whose body can be a list of match arms; armMayStart is false
// on a line that starts inside a string literal.
func (s *scanner) continuation(code string, armMayStart bool) scanned {
	if !s.inDeclaration {
		return scanned{}
	}
	if s.blockPending {
		s.blockColumn, s.blockPending = indentation(code), false
	}
	defer s.endLine(code)
	arm := armMayStart && s.currentMatchArm && s.brackets == 0 && startsMatchArm(code)
	brackets := s.brackets
	bodyStarts := s.track(code)
	if !s.inSignature {
		if armMayStart && brackets == 0 {
			s.bodyField(code)
		}
		return scanned{owned: true}
	}
	if arm {
		s.endSignature()
		return scanned{owned: true}
	}
	s.signature = appendFields(s.signature, code)
	if bodyStarts {
		s.endSignature()
	}
	return scanned{owned: true, signature: true}
}

// bodyField records the fields that a body line of a structure or class
// declares, when the line is at the body's first indentation.
func (s *scanner) bodyField(code string) {
	declaration := &s.declarations[len(s.declarations)-1]
	if declaration.Kind != "structure" && declaration.Kind != "class" {
		return
	}
	if s.fieldColumn < 0 {
		s.fieldColumn = indentation(code)
	}
	if indentation(code) != s.fieldColumn {
		return
	}
	declaration.Fields = append(declaration.Fields, fieldNames(code)...)
}

// fieldNames returns the fields an indented structure line declares, in
// order: attributes and `private` or `protected`, one or more names, binder
// groups, then `:`. A constructor line (`mk ::`), a default for a parent's
// field (`base := 0`) and any other line declare none.
func fieldNames(code string) []string {
	if indentation(code) == 0 {
		return nil
	}
	rest := strings.TrimSpace(code)
	for {
		if strings.HasPrefix(rest, "@[") {
			end := strings.IndexByte(rest, ']')
			if end < 0 {
				return nil
			}
			rest = strings.TrimLeft(rest[end+1:], " \t")
			continue
		}
		if word := firstWord(rest); word == "private" || word == "protected" {
			rest = strings.TrimLeft(rest[len(word):], " \t")
			continue
		}
		break
	}
	var names []string
	binders := false
	for rest != "" {
		first, size := utf8.DecodeRuneInString(rest)
		switch {
		case first == ':':
			if strings.HasPrefix(rest, ":=") || strings.HasPrefix(rest, "::") {
				return nil
			}
			return names
		case strings.ContainsRune("([{⦃", first):
			if len(names) == 0 {
				return nil
			}
			end := closingBracket(rest)
			if end < 0 {
				return nil
			}
			rest, binders = rest[end:], true
		case first == ' ' || first == '\t':
			rest = rest[size:]
		default:
			word := firstWord(rest)
			if word == "" || binders || !startsIdentifier(word) {
				return nil
			}
			names = append(names, word)
			rest = rest[len(word):]
		}
	}
	return nil
}

// closingBracket is the index just after the bracket that closes the one text
// starts with, or -1.
func closingBracket(text string) int {
	depth := 0
	for index, r := range text {
		switch {
		case strings.ContainsRune("([{⦃", r):
			depth++
		case strings.ContainsRune(")]}⦄", r):
			depth--
			if depth == 0 {
				return index + utf8.RuneLen(r)
			}
		}
	}
	return -1
}

// endLine records a code line of the current declaration as its last one. A
// line that ends in `where` or `do` opens a block, whose first line sets the
// block's column.
func (s *scanner) endLine(code string) {
	s.lastCode = code
	word := lastWord(code)
	s.blockPending = word == "where" || word == "do"
}

// track counts the brackets a code line of the current declaration opens
// and closes, skipping its string and character literals, and carries the
// depth to the next line. It reports whether the line starts the body of an
// open signature: a `:=`, or for a macro, an elab or a notation a `=>`,
// outside every bracket, or a `where` that ends the line outside every
// bracket.
func (s *scanner) track(code string) bool {
	bodyStarts := false
	literal := 0
	for index := 0; index < len(code); index++ {
		if literal < len(s.literals) && index == s.literals[literal][0] {
			index = s.literals[literal][1] - 1
			literal++
			continue
		}
		switch code[index] {
		case '(', '[', '{':
			s.brackets++
		case ')', ']', '}':
			if s.brackets > 0 {
				s.brackets--
			}
		case ':':
			bodyStarts = bodyStarts || s.brackets == 0 && strings.HasPrefix(code[index+1:], "=")
		case '=':
			bodyStarts = bodyStarts || s.currentArrow && s.brackets == 0 && strings.HasPrefix(code[index+1:], ">")
		}
	}
	return bodyStarts || s.brackets == 0 && lastWord(code) == "where"
}

// startsCommand reports whether a code line of the current declaration
// starts a new command instead. Lean reads a command from the column it
// starts at, and a term's argument must lie to the right of it, so an
// identifier at or left of the declaration's column starts the next command,
// unless a bracket is open, the line above leaves its term incomplete, the
// line is in a block that `where` or `do` opened at that column, or it starts
// with a keyword that continues a declaration.
func (s *scanner) startsCommand(code string) bool {
	trimmed := strings.TrimLeft(code, " \t")
	if column := len(code) - len(trimmed); column > s.column || column == s.blockColumn || s.brackets > 0 {
		return false
	}
	if !startsIdentifier(trimmed) || continuingKeywords[firstWord(trimmed)] {
		return false
	}
	return termComplete(s.lastCode)
}

var (
	// continuingKeywords start a line that continues a declaration.
	continuingKeywords = map[string]bool{"where": true, "termination_by": true, "decreasing_by": true, "then": true, "else": true, "with": true, "extends": true, "from": true, "at": true, "using": true, "in": true, "do": true, "by": true, "deriving": true}
	// openingKeywords end a line whose term continues on the next line.
	openingKeywords = map[string]bool{"where": true, "then": true, "else": true, "do": true, "by": true, "fun": true, "with": true, "from": true, "in": true, "if": true, "let": true, "have": true, "show": true, "match": true, "calc": true, "at": true, "using": true, "return": true, "suffices": true, "extends": true}
)

// termComplete reports whether a code line can end a term: it ends in an
// identifier, a literal or a closing bracket, not in an operator, a
// separator or a keyword that needs what follows.
func termComplete(code string) bool {
	code = strings.TrimRight(code, " \t")
	if code == "" {
		return true
	}
	last, _ := utf8.DecodeLastRuneInString(code)
	if strings.ContainsRune(")]}⟩⦄⟧»⌝\"'", last) {
		return true
	}
	return identifierRune(last) && !openingKeywords[lastWord(code)]
}

// startsIdentifier reports whether code starts with an identifier: a letter
// other than λ, a «, or an _ that is not a hole such as a calc step's `_ = …`.
func startsIdentifier(code string) bool {
	first, size := utf8.DecodeRuneInString(code)
	switch {
	case first == '«':
		return true
	case first == '_':
		next, _ := utf8.DecodeRuneInString(code[size:])
		return next == '_' || unicode.IsLetter(next) || unicode.IsDigit(next)
	}
	return unicode.IsLetter(first) && first != 'λ'
}

// identifierRune reports whether a rune can be part of an identifier.
func identifierRune(r rune) bool {
	return r == '_' || r == '\'' || r == '!' || r == '?' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

// firstWord returns the identifier that starts code.
func firstWord(code string) string {
	if end := strings.IndexFunc(code, func(r rune) bool { return !identifierRune(r) }); end >= 0 {
		return code[:end]
	}
	return code
}

// lastWord returns the identifier that ends code, after trailing spaces.
func lastWord(code string) string {
	code = strings.TrimRight(code, " \t")
	start := strings.LastIndexFunc(code, func(r rune) bool { return !identifierRune(r) })
	if start < 0 {
		return code
	}
	_, size := utf8.DecodeRuneInString(code[start:])
	return code[start+size:]
}

// indentation is the number of spaces and tabs before a line's code.
func indentation(code string) int {
	return len(code) - len(strings.TrimLeft(code, " \t"))
}

// endSignature stores the open signature as the current declaration's Text.
// An anonymous instance's name is resolved in finish, which needs the whole
// file; its computed name and namespace are recorded here, while the scopes
// are still its own (a line that changes them ends the declaration first).
func (s *scanner) endSignature() {
	if !s.inSignature {
		return
	}
	declaration := &s.declarations[len(s.declarations)-1]
	declaration.Text = string(s.signature)
	s.inSignature = false
	if declaration.Kind == "structure" || declaration.Kind == "class" {
		declaration.Extends = extendsBeforeWhere(declaration.Text)
	}
	if s.anonymous {
		name, computed := instanceName(declaration.Text, s.variables)
		s.anonymousInstances = append(s.anonymousInstances, anonymousInstance{
			index: len(s.declarations) - 1, prefix: s.prefix(), name: name, computed: computed,
		})
	}
}

// extendsBeforeWhere reports whether a structure or class signature has the
// word `extends` before the `where` that opens its body.
func extendsBeforeWhere(text string) bool {
	for _, word := range identifiers(text) {
		switch word {
		case "extends":
			return true
		case "where":
			return false
		}
	}
	return false
}

// anonymousInstance is an instance declared without a name: index is its
// declaration, prefix the namespace it is in, and name the name Lean
// generates for it when computed is set.
type anonymousInstance struct {
	index    int
	prefix   string
	name     string
	computed bool
}

// nameAnonymousInstances names the file's anonymous instances, once the whole
// file is read.
//
// An instance keeps the name Lean generates for it only when the scanner
// computed that name exactly and the name is the file's alone: Lean numbers a
// repeated name `_1`, `_2` by what the namespace already holds, which one
// file does not say, so a guessed number can be the name Lean gives another
// instance. Every other anonymous instance is named by its position among the
// file's anonymous instances. `<anon-instance>` is not a Lean identifier, so
// such a name never equals a name Lean generates, and a diagnostic that names
// a generated instance finds no declaration rather than the wrong one.
//
// A name that more than one file of the round's candidate tree generates is
// in ambiguous, and no declaration keeps it: Lean gives it to one of those
// instances and numbers the rest, and a scan of one file cannot tell which.
// AmbiguousInstanceNames builds that set over a whole tree.
func (s *scanner) nameAnonymousInstances() {
	taken := map[string]int{}
	for index, declaration := range s.declarations {
		if !s.isAnonymous(index) {
			taken[declaration.Name]++
		}
	}
	for _, instance := range s.anonymousInstances {
		if instance.computed {
			taken[qualifyIn(instance.prefix, instance.name)]++
		}
	}
	for position, instance := range s.anonymousInstances {
		name := qualifyIn(instance.prefix, instance.name)
		_, shared := s.ambiguous[name]
		if !instance.computed || shared || taken[name] != 1 {
			s.declarations[instance.index].Name = positionalInstanceName(instance.prefix, position+1)
			continue
		}
		s.declarations[instance.index].Name = name
		s.declarations[instance.index].Generated = true
	}
}

func (s *scanner) isAnonymous(index int) bool {
	for _, instance := range s.anonymousInstances {
		if instance.index == index {
			return true
		}
	}
	return false
}

func qualifyIn(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "." + name
}

// instanceBinder is one binder group of an anonymous instance: the names it
// binds and their type.
type instanceBinder struct {
	names []string
	typ   string
}

// instanceName is the name Lean generates for an anonymous instance from its
// signature (Lean.Elab.Command.NameGen): "inst", the constants of the
// instance's type, then "Of" and the constants of each binder that neither
// the type nor a later binder depends on. Each constant is its last name
// component, capitalised, and appears once. A bound name, a section variable
// in variables, a one-letter name (Lean binds it automatically), a named
// argument and a keyword all add nothing. computed is false when the scanner
// cannot be sure the name is Lean's, which computableType decides.
//
// A residual case computed does not catch: when no constant of the name is
// defined in the project, Lean appends a suffix made from the module
// (mkBaseNameWithSuffix), which the scanner cannot see. The name is then not
// Lean's, and a diagnostic that names the instance finds no declaration. It
// cannot name another instance of the same file, because two instances of one
// file never keep the same computed name.
func instanceName(text string, variables map[string]bool) (string, bool) {
	binders, typ := splitInstance(instancePrefixRE.ReplaceAllString(text, ""))
	bound := map[string]bool{}
	for name := range variables {
		bound[name] = true
	}
	groups := make([]instanceBinder, 0, len(binders))
	for _, group := range binders {
		names, typ := "", group
		if colon := topLevelIndex(group[1:], ":"); colon >= 0 && !strings.HasPrefix(group[1+colon:], ":=") {
			names, typ = group[1:1+colon], group[2+colon:len(group)-1]
		} else {
			typ = group[1 : len(group)-1]
		}
		binder := instanceBinder{typ: strings.TrimSpace(typ)}
		// An instance binder's name is never written in the type.
		if group[0] != '[' {
			binder.names = strings.Fields(names)
		}
		for _, name := range binder.names {
			bound[name] = true
		}
		groups = append(groups, binder)
	}
	seen := map[string]bool{}
	name := "inst" + constantWords(typ, bound, seen)
	// Lean names the type first, then each binder from the innermost out,
	// and writes the binders from the outermost in.
	suffixes := make([]string, len(groups))
	computed := computableType(typ, bound)
	for index := len(groups) - 1; index >= 0; index-- {
		if dependsOn(groups[index].names, typ, groups[index+1:]) {
			continue
		}
		computed = computed && computableType(groups[index].typ, bound)
		if words := constantWords(groups[index].typ, bound, seen); words != "" {
			suffixes[index] = "Of" + words
		}
	}
	return name + strings.Join(suffixes, ""), computed
}

// computableType reports whether the scanner reads a type the way Lean's name
// generator does. It rejects every shape whose name needs elaboration:
// notation such as `→`, `×`, `+` and `∀`, which Lean names by the constants
// behind it; a type ascription or any other `:`, which Lean ignores; a
// numeral, whose `OfNat` Lean names; a projection of a bound name such as
// `st.room`, where Lean names the projected structure's field instead; and
// `inferInstance`, whose value Lean elaborates. Only identifiers, dotted
// constant names, spaces and round brackets remain.
func computableType(typ string, bound map[string]bool) bool {
	for _, word := range identifiers(typ) {
		if word == "inferInstance" {
			return false
		}
		if base, _, dotted := strings.Cut(word, "."); dotted && bound[base] {
			return false
		}
	}
	for index := 0; index < len(typ); {
		r, size := utf8.DecodeRuneInString(typ[index:])
		switch {
		case unicode.IsLetter(r) && r != 'λ', r == '_', r == '\'', r == '.':
		case r == ' ' || r == '\t' || r == '(' || r == ')':
		case unicode.IsDigit(r):
			// A digit is part of an identifier, never a numeral of its own.
			if index == 0 || !identifierRune(rune(typ[index-1])) {
				return false
			}
		default:
			return false
		}
		index += size
	}
	return true
}

// splitInstance splits an anonymous instance's signature after the word
// instance into its bracketed binder groups and its type, without the body.
func splitInstance(text string) ([]string, string) {
	groups := splitBinderGroups(text)
	colon := topLevelIndex(text, ":")
	if colon < 0 || strings.HasPrefix(text[colon:], ":=") {
		return groups, ""
	}
	typ := text[colon+1:]
	if body := topLevelIndex(typ, ":="); body >= 0 {
		typ = typ[:body]
	}
	typ = strings.TrimSpace(typ)
	if lastWord(typ) == "where" {
		typ = strings.TrimSpace(strings.TrimSuffix(typ, "where"))
	}
	return groups, typ
}

// splitBinderGroups returns the bracketed binder groups of text, in order,
// up to the first `:` outside every bracket.
func splitBinderGroups(text string) []string {
	var groups []string
	depth, start := 0, -1
	for index, r := range text {
		switch {
		case strings.ContainsRune("([{⦃", r):
			if depth == 0 {
				start = index
			}
			depth++
		case strings.ContainsRune(")]}⦄", r):
			depth--
			if depth == 0 && start >= 0 {
				groups = append(groups, text[start:index+utf8.RuneLen(r)])
				start = -1
			}
		case r == ':' && depth == 0 && !strings.HasPrefix(text[index:], ":="):
			return groups
		}
	}
	return groups
}

// topLevelIndex is the index of the first token in text outside every
// bracket, or -1.
func topLevelIndex(text, token string) int {
	depth := 0
	for index, r := range text {
		switch {
		case strings.ContainsRune("([{⦃⟨", r):
			depth++
		case strings.ContainsRune(")]}⦄⟩", r):
			depth--
		case depth == 0 && strings.HasPrefix(text[index:], token):
			return index
		}
	}
	return -1
}

// dependsOn reports whether the type or a later binder mentions one of a
// binder group's names.
func dependsOn(names []string, typ string, later []instanceBinder) bool {
	for _, name := range names {
		if mentions(typ, name) {
			return true
		}
		for _, binder := range later {
			if mentions(binder.typ, name) {
				return true
			}
		}
	}
	return false
}

// mentions reports whether a term mentions an identifier.
func mentions(term, name string) bool {
	for _, word := range identifiers(term) {
		if word == name {
			return true
		}
	}
	return false
}

var (
	namedArgumentRE = regexp.MustCompile(`\(\s*[A-Za-z_][A-Za-z0-9_']*\s*:=[^()]*\)`)
	termKeywords    = map[string]bool{"fun": true, "forall": true, "if": true, "then": true, "else": true, "let": true, "have": true, "show": true, "by": true, "do": true, "match": true, "with": true, "Type": true, "Sort": true, "Prop": true}
)

// constantWords renders the constants of a term as Lean's instance names
// do, and adds them to seen.
func constantWords(term string, bound, seen map[string]bool) string {
	var out strings.Builder
	for _, word := range identifiers(namedArgumentRE.ReplaceAllString(term, "")) {
		component := word[strings.LastIndex(word, ".")+1:]
		if bound[word] || seen[word] || termKeywords[word] || utf8.RuneCountInString(component) <= 1 {
			continue
		}
		seen[word] = true
		first, size := utf8.DecodeRuneInString(component)
		out.WriteRune(unicode.ToUpper(first))
		out.WriteString(component[size:])
	}
	return out.String()
}

// identifiers returns the dotted identifiers of a term, in order.
func identifiers(term string) []string {
	var out []string
	for index := 0; index < len(term); {
		r, size := utf8.DecodeRuneInString(term[index:])
		if !unicode.IsLetter(r) && r != '_' || r == 'λ' || index > 0 && identifierBefore(term, index) {
			index += size
			continue
		}
		end := index
		for end < len(term) {
			next, nextSize := utf8.DecodeRuneInString(term[end:])
			if !identifierRune(next) && next != '.' {
				break
			}
			end += nextSize
		}
		if word := strings.Trim(term[index:end], "."); word != "" && word != "_" {
			out = append(out, word)
		}
		index = end
	}
	return out
}

// endDeclaration leaves the current declaration, if any.
func (s *scanner) endDeclaration() {
	s.endSignature()
	s.inDeclaration = false
	s.currentIsStructure, s.currentMatchArm, s.currentArrow, s.anonymous = false, false, false, false
}

// finish ends the source, names its anonymous instances and returns its
// declarations.
func (s *scanner) finish() []Declaration {
	s.endDeclaration()
	s.nameAnonymousInstances()
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

// uncomment returns a line's code: it removes block comments, which may span
// lines and nest, and a trailing line comment. Comment markers inside string
// and character literals are code. A string literal, including a raw string
// `r#"…"#`, may span lines, so the scanner carries it to the next line. It
// records the span of each literal in the code in s.literals.
func (s *scanner) uncomment(line string) string {
	var out strings.Builder
	s.literals = s.literals[:0]
	literalStart := 0
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
				s.literals = append(s.literals, [2]int{literalStart, out.Len()})
			}
		case s.inString && current == '\\' && next != 0:
			out.WriteByte(current)
			out.WriteByte(next)
			index++
		case s.inString:
			out.WriteByte(current)
			if current == '"' {
				s.inString = false
				s.literals = append(s.literals, [2]int{literalStart, out.Len()})
			}
		case current == '"':
			s.inString, s.rawHashes = true, -1
			literalStart = out.Len()
			out.WriteByte(current)
		case current == 'r' && !identifierBefore(line, index):
			hashes := 0
			for index+1+hashes < len(line) && line[index+1+hashes] == '#' {
				hashes++
			}
			if index+1+hashes < len(line) && line[index+1+hashes] == '"' {
				literalStart = out.Len()
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
			s.literals = append(s.literals, [2]int{out.Len(), out.Len() + end + 1 - index})
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
	if s.inString {
		s.literals = append(s.literals, [2]int{literalStart, out.Len()})
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
	// code is the line without its comments, and namespace the fully
	// qualified namespace that a namespace line opens or closes.
	code, namespace string
}

// ScanLineOwners returns one LineOwner per source line, in order. ambiguous
// is the round's set of generated instance names that no declaration may
// keep, as for ScanDeclarations.
func ScanLineOwners(file, source string, ambiguous InstanceNames) []LineOwner {
	s := scanner{file: file, ambiguous: ambiguous}
	lines := strings.Split(source, "\n")
	owners := make([]LineOwner, len(lines))
	// owned holds, for each line, 1 + the index of its owner in
	// s.declarations, or 0. A declaration's Text is complete only when its
	// signature ends, so owners are filled in after the scan.
	owned := make([]int, len(lines))
	for index, line := range lines {
		result := s.scan(line)
		owners[index] = LineOwner{Kind: result.kind, code: result.code, namespace: result.namespace}
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
