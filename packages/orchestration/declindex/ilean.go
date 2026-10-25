package declindex

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// IleanVersion is the version of Lean's reference-file format (`Lean/Data/Lsp/Internal.lean`) that this reader
// understands. A file of another version is refused, never guessed at.
const IleanVersion = 5

// Roots are the first components of the module names whose declarations the index follows: the project's own
// modules, not the toolchain's or a dependency's.
type Roots map[string]bool

// Has reports whether module belongs to the roots.
func (r Roots) Has(module string) bool {
	root, _, _ := strings.Cut(module, ".")
	return r[root]
}

// Decl is one declaration that a module's reference file records.
type Decl struct {
	Name   string
	Short  string
	Module string
	// Kind is the command that declares it (theorem, def, structure, ...), "other" for one the reader does not
	// recognise, and empty when the source file is missing.
	Kind string
	Line int // 1-based
	// StmtHash is StatementHash of its statement, empty when the source file is missing.
	StmtHash string
	// Shape is the declaration's SourceShapes entry, which a renamed copy shares; empty without a statement hash.
	Shape string
}

// Use is one reference that elaboration recorded to a declaration of the roots.
type Use struct {
	Target string
	// TargetModule is the module that Lean recorded for Target: the one that declares it, or for a constant that
	// Lean made where it is used (an equation lemma), that using module.
	TargetModule string
	Module       string
	Line         int // 1-based
	Col          int // 1-based, in code points
	// Parent is the declaration that holds the use; empty at top level (open, attribute, variable).
	Parent string
	// Class says where in Parent the use is: "stmt" (its statement: type, binders, hypotheses, or anywhere in a
	// definition-like declaration's text before its body), "proof" (its proof or definition body), "top" (no
	// parent), or "unk" (the parent's statement end is unknown).
	Class string
}

// ModuleData is what one module's reference file and source give the index.
type ModuleData struct {
	Rel    string // path of the module below the library directory, without suffix
	Module string
	// Imports are the modules that the module imports directly.
	Imports []string
	Decls   []Decl
	Uses    []Use
	// heads are the source heads of the declarations that have a statement hash (SourceHeads), each with its
	// declaration's name: a head is joined to a declaration by its line.
	heads []declHead
	// Stale counts the declarations whose recorded position does not hold their name in the source: the source is
	// newer than the reference file. Their statement hashes are left empty.
	Stale int
}

// declHead is one source head of the declaration name.
type declHead struct {
	name string
	head SourceHead
}

type ileanFile struct {
	Version       int                       `json:"version"`
	Module        string                    `json:"module"`
	DirectImports []ileanImport             `json:"directImports"`
	Decls         map[string][]int          `json:"decls"`
	References    map[string]ileanReference `json:"references"`
}

// ileanImport is `[module, isExported, isMeta, importAll]`, one direct import.
type ileanImport struct{ Module string }

func (i *ileanImport) UnmarshalJSON(b []byte) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	if len(raw) == 0 {
		return fmt.Errorf("import %s names no module", b)
	}
	return json.Unmarshal(raw[0], &i.Module)
}

type ileanReference struct {
	Usages []ileanUsage `json:"usages"`
}

// ileanUsage is `[line, col, endLine, endCol, parentDecl?]`, 0-based, columns in UTF-16 code units.
type ileanUsage struct {
	Line, Col int
	Parent    string
}

func (u *ileanUsage) UnmarshalJSON(b []byte) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	if len(raw) < 2 {
		return fmt.Errorf("usage %s has fewer than two positions", b)
	}
	if err := json.Unmarshal(raw[0], &u.Line); err != nil {
		return err
	}
	if err := json.Unmarshal(raw[1], &u.Col); err != nil {
		return err
	}
	if len(raw) > 4 && string(raw[4]) != "null" {
		if err := json.Unmarshal(raw[4], &u.Parent); err != nil {
			return err
		}
	}
	return nil
}

type referenceKey struct {
	C *struct {
		M string `json:"m"`
		N string `json:"n"`
	} `json:"c"`
}

type sigEnd struct{ line, col int }

// ParseModule reads one module's reference file (.ilean) and its source text. src is nil when the source file is
// missing: declarations then have no kind and no hash, and uses stay unclassified. Only references to
// declarations of roots modules are kept.
func ParseModule(rel string, ilean, src []byte, roots Roots) (ModuleData, error) {
	var f ileanFile
	if err := json.Unmarshal(ilean, &f); err != nil {
		return ModuleData{}, fmt.Errorf("%s.ilean: %w", rel, err)
	}
	if f.Version != IleanVersion {
		return ModuleData{}, fmt.Errorf("%s.ilean: format version %d, this reader reads version %d", rel, f.Version, IleanVersion)
	}
	md := ModuleData{Rel: rel, Module: f.Module}
	if md.Module == "" {
		md.Module = strings.ReplaceAll(rel, "/", ".")
	}
	for _, i := range f.DirectImports {
		md.Imports = append(md.Imports, i.Module)
	}
	var s *source
	if src != nil {
		s = newSource(string(src))
	}
	names := make([]string, 0, len(f.Decls))
	for name := range f.Decls {
		names = append(names, name)
	}
	sort.Strings(names)
	sigends := map[string]sigEnd{}
	for _, name := range names {
		r := f.Decls[name]
		if len(r) < 8 {
			return ModuleData{}, fmt.Errorf("%s.ilean: declaration %s has a range of %d numbers", rel, name, len(r))
		}
		d := Decl{Name: name, Short: name[strings.LastIndexByte(name, '.')+1:], Module: md.Module, Line: r[0] + 1}
		if s != nil {
			a, okA := s.offset(r[0], r[1])
			sel, okS := s.offset(r[4], r[5])
			selEnd, okSE := s.offset(r[6], r[7])
			e, okE := s.offset(r[2], r[3])
			if okA && okS {
				d.Kind = "other"
				if sel >= a {
					d.Kind = declKind(s.text, a, min(next(s.text, sel), len(s.text)))
				}
				if okSE && okE {
					if b, ok := bodyStart(s.text, selEnd, next(s.text, e)); ok {
						l, c := s.position(b)
						sigends[name] = sigEnd{l, c}
					}
					if selectionHolds(s.text, name, sel, selEnd) {
						d.StmtHash = StatementHash(StatementText(s.text, a, e, selEnd, d.Kind))
					} else {
						md.Stale++
					}
				}
			}
		}
		md.Decls = append(md.Decls, d)
	}
	if src != nil {
		md.joinSourceFacts(src)
	}
	for key, ref := range f.References {
		var k referenceKey
		if err := json.Unmarshal([]byte(key), &k); err != nil {
			return ModuleData{}, fmt.Errorf("%s.ilean: reference key %q: %w", rel, key, err)
		}
		if k.C == nil || !roots.Has(k.C.M) {
			continue
		}
		for _, u := range ref.Usages {
			ci := u.Col
			if s != nil && u.Line < len(s.starts) {
				_, ci = u16Col(s.line(u.Line), u.Col)
			}
			class := "unk"
			switch end, ok := sigends[u.Parent]; {
			case u.Parent == "":
				class = "top"
			case ok:
				class = "proof"
				if u.Line < end.line || (u.Line == end.line && ci < end.col) {
					class = "stmt"
				}
			}
			md.Uses = append(md.Uses, Use{Target: k.C.N, TargetModule: k.C.M, Module: md.Module, Line: u.Line + 1, Col: ci + 1,
				Parent: u.Parent, Class: class})
		}
	}
	sortUses(md.Uses)
	return md, nil
}

// joinSourceFacts gives each declaration that has a statement hash its shape, and the module the source heads of
// those declarations, joined by line: SourceShapes, SourceHeads and the reference file all give a declaration's first
// line, its doc comment and attributes included. A declaration without a statement hash has no trusted position in
// the source (Stale), so its line may hold another declaration.
func (md *ModuleData) joinSourceFacts(src []byte) {
	shapes := map[int]string{}
	for _, s := range SourceShapes(src) {
		shapes[s.Line] = s.Shape
	}
	byLine := map[int][]string{}
	for i := range md.Decls {
		d := &md.Decls[i]
		if d.StmtHash == "" {
			continue
		}
		d.Shape = shapes[d.Line]
		byLine[d.Line] = append(byLine[d.Line], d.Name)
	}
	for _, h := range SourceHeads(src) {
		for _, name := range byLine[h.Line] {
			md.heads = append(md.heads, declHead{name: name, head: h})
		}
	}
}

func sortUses(us []Use) {
	sort.Slice(us, func(i, j int) bool {
		a, b := us[i], us[j]
		if a.Target != b.Target {
			return a.Target < b.Target
		}
		if a.TargetModule != b.TargetModule {
			return a.TargetModule < b.TargetModule
		}
		if a.Module != b.Module {
			return a.Module < b.Module
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		if a.Col != b.Col {
			return a.Col < b.Col
		}
		if a.Parent != b.Parent {
			return a.Parent < b.Parent
		}
		return a.Class < b.Class
	})
}

// anonymousSelections are the commands whose declarations Lean names itself (an `instance` without a name, the
// auxiliary of a `macro_rules`): their selection range holds the command, not the name.
var anonymousSelections = []string{"instance", "example", "macro_rules", "macro", "syntax", "elab_rules", "elab",
	"notation", "infixl", "infixr", "infix", "prefix", "postfix", "deriving", "@[", "scoped", "local"}

// userName is a declaration's name as its source writes it: a private name without its `_private.<module>.<n>.`
// prefix.
func userName(name string) string {
	if !strings.HasPrefix(name, "_private.") {
		return name
	}
	parts := strings.Split(name, ".")
	for i, p := range parts {
		if i > 0 && p != "" && strings.Trim(p, "0123456789") == "" {
			return strings.Join(parts[i+1:], ".")
		}
	}
	return name
}

// madeByLean reports whether Lean made the name for a command (an elaborator's or macro's auxiliary, an
// initializer, a hygienic name): a component that starts with `_`, quoted (`«_aux_…»`) or not.
func madeByLean(name string) bool {
	inQuote := false
	start := true
	for _, r := range name {
		switch {
		case r == '«' && start:
			inQuote = true
			continue // the component starts after its opening quote
		case r == '«':
			inQuote = true
		case r == '»':
			inQuote = false
		case r == '.' && !inQuote:
			start = true
			continue
		case start && r == '_':
			return true
		}
		start = false
	}
	return false
}

// unquote drops the guillemets that quote a name's components. A Replacer builds its tables once and is safe for
// concurrent use, and ParseModule runs on parallel goroutines.
var unquote = strings.NewReplacer("«", "", "»", "")

// selectionHolds reports whether the source still holds the declaration at the position its reference file
// recorded: the selection range is its name (or a dotted suffix of it, or the name written from `_root_`), or the
// command of a declaration Lean names itself. A name Lean made has no written name to compare and passes. A
// source newer than its build fails it.
func selectionHolds(text, name string, sel, selEnd int) bool {
	want := userName(name)
	if madeByLean(want) {
		return true
	}
	if sel < 0 || sel > len(text) {
		return false
	}
	if selEnd >= sel && selEnd <= len(text) {
		got := strings.TrimPrefix(unquote.Replace(text[sel:selEnd]), "_root_.")
		want := unquote.Replace(want)
		if got != "" && (got == want || strings.HasSuffix(want, "."+got)) {
			return true
		}
	}
	rest := text[sel:]
	for _, kw := range anonymousSelections {
		if strings.HasPrefix(rest, kw) {
			return true
		}
	}
	return false
}
