package blame

import (
	"regexp"
	"strconv"
	"strings"
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
	declarationRE = regexp.MustCompile(`^\s*(?:@\[[^\]]*\]\s*)*((?:(?:private|protected|noncomputable|scoped|local|unsafe|partial|nonrec|public|meta)\s+)*)(?:def|theorem|lemma|structure|class|inductive|abbrev|instance|axiom|opaque|notation|macro|syntax|macro_rules|elab|alias)\s+([^\s(:{\[⟨]+)`)
	namespaceRE   = regexp.MustCompile(`^\s*namespace\s+(\S+)\s*$`)
	sectionRE     = regexp.MustCompile(`^\s*(?:(?:noncomputable|public|private|meta)\s+)*section(?:\s+(\S+))?\s*$`)
	mutualRE      = regexp.MustCompile(`^\s*mutual\s*$`)
	endRE         = regexp.MustCompile(`^\s*end(?:\s+(\S+))?\s*$`)
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
// sections and mutual blocks that qualify declarations, block comments, and the
// declaration whose body it is in.
type scanner struct {
	file    string
	scopes  []scope
	depth   int
	current Declaration
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

// Refers reports whether a diagnostic name can refer to the declaration. A
// qualified diagnostic name must be exact; an unqualified name can refer to a
// declaration below an open namespace. A protected declaration is never
// reachable by its last component alone.
func Refers(reference string, declaration Declaration) bool {
	return refers(reference, declaration.Name, declaration.Protected)
}

func refers(reference, name string, protected bool) bool {
	reference, name = normaliseModule(reference), normaliseModule(name)
	if reference == name {
		return true
	}
	if strings.Contains(reference, ".") {
		return false
	}
	return strings.HasSuffix(name, "."+reference) && !protected
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
	code := s.uncomment(line)
	if strings.TrimSpace(code) == "" {
		return scanned{code: code}
	}
	if match := namespaceRE.FindStringSubmatch(code); match != nil {
		s.current = Declaration{}
		for _, part := range nameParts(match[1]) {
			s.scopes = append(s.scopes, scope{kind: namespaceScope, name: part})
		}
		return scanned{kind: namespaceLine, code: code, namespace: s.prefix()}
	}
	if match := endRE.FindStringSubmatch(code); match != nil {
		s.current = Declaration{}
		parts := nameParts(match[1])
		closed := s.prefix()
		// An `end N` outside every scope the scanner has seen (a diff whose
		// context lacks the opening line) still closes namespace N.
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
		s.scopes = append(s.scopes, scope{kind: mutualScope})
		return scanned{code: code}
	}
	if match := declarationRE.FindStringSubmatch(code); match != nil {
		modifiers := strings.Fields(match[1])
		declaration := Declaration{
			File:      s.file,
			Name:      s.qualify(match[2]),
			Text:      strings.Join(strings.Fields(code), " "),
			Private:   containsString(modifiers, "private"),
			Protected: containsString(modifiers, "protected"),
		}
		s.current = declaration
		return scanned{kind: declarationLine, code: code, declaration: declaration}
	}
	if commandRE.MatchString(code) {
		s.current = Declaration{}
	}
	return scanned{code: code}
}

// declared returns the fully qualified name that a line, such as a hunk
// header's context, declares in the scanner's current scope, or "". It does
// not change the scanner.
func (s *scanner) declared(line string) string {
	match := declarationRE.FindStringSubmatch((&scanner{}).uncomment(line))
	if match == nil {
		return ""
	}
	return s.qualify(match[2])
}

// uncomment removes block comments, which may span lines and nest, and a
// trailing line comment, outside string literals.
func (s *scanner) uncomment(line string) string {
	var out strings.Builder
	inString := false
	for index := 0; index < len(line); index++ {
		next := byte(0)
		if index+1 < len(line) {
			next = line[index+1]
		}
		switch {
		case s.depth > 0 && line[index] == '-' && next == '/':
			s.depth--
			index++
		case s.depth > 0 && line[index] == '/' && next == '-':
			s.depth++
			index++
		case s.depth > 0:
		case inString && line[index] == '\\' && next != 0:
			out.WriteByte(line[index])
			out.WriteByte(next)
			index++
		case line[index] == '"':
			inString = !inString
			out.WriteByte(line[index])
		case !inString && line[index] == '/' && next == '-':
			s.depth++
			index++
		case !inString && line[index] == '-' && next == '-':
			return out.String()
		default:
			out.WriteByte(line[index])
		}
	}
	return out.String()
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
