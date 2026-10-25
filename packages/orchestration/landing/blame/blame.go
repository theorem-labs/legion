// Package blame attributes build failures to candidate members from build output and diffs.
package blame

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Tree identifies an immutable source tree supplied to a Builder.
type Tree string

// BuildResult is the answer from one build attempt. InfraClass is empty when the
// build reached a source verdict.
type BuildResult struct {
	Green      bool
	Output     string
	InfraClass string
	Absent     bool
}

// Builder builds the requested modules from a source tree.
type Builder interface {
	Build(context.Context, Tree, []string) (BuildResult, error)
}

// Member is a candidate member available for confirmation.
type Member struct {
	ID   string
	Head string
}

// Candidate builds a tree from a base and an ordered set of members.
type Candidate func(context.Context, string, []Member) (Tree, error)

// Kind classifies how an error mentions a declaration.
type Kind string

const (
	Unknown Kind = "unknown"
	Added   Kind = "added"
	Changed Kind = "changed"
	Weak    Kind = "weak"
	Field   Kind = "field"
)

// Name is one declaration name inferred from an error block.
type Name struct {
	Value string
	Kind  Kind
}

// Block is one diagnostic block associated with a module. Line is the
// 1-based line of Path that the error names.
type Block struct {
	Module  string
	Path    string
	Line    int
	Message string
}

// Report is the red modules and diagnostics parsed from a build log.
type Report struct {
	RedModules []string
	Blocks     []Block
}

var (
	buildingRE     = regexp.MustCompile(`^✖ \[[0-9]+/[0-9]+\] (?:Building|Running|Compiling) (\S+)`)
	errorRE        = regexp.MustCompile(`^error: (?:\./)?([^:\s]+\.lean):([0-9]+):[0-9]+: ?(.*)$`)
	importRE       = regexp.MustCompile(`\bimport (\S+) failed`)
	blockEndRE     = regexp.MustCompile(`^(?:error:|warning:|info:|trace:|✔|✖|⚠|\[|-- build round|Build completed|Some required|Some builds)`)
	identifier     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.'!?-]*$`)
	unknownRE      = regexp.MustCompile("(?i)(?:unknown identifier|unknown constant|the environment does not contain|unknown namespace)\\s*[`']([^`']+)[`']")
	addedRE        = regexp.MustCompile(`environment already contains '([^']+)'`)
	fieldOfRE      = regexp.MustCompile("is not a field of structure `([^`]+)`")
	invalidFieldRE = regexp.MustCompile("(?s)Invalid field `[^`]+`.*?of type `([^`\\s]+)")
	classRE        = regexp.MustCompile("(?s)failed to synthesize(?: instance of type class)?\\s*[`']?([A-Z][A-Za-z0-9_.'!?-]*)")
	applicationRE  = regexp.MustCompile("(?s)(?:in the application|type expected, got)\\s*\\n\\s*\\(?@?([A-Za-z_][A-Za-z0-9_.'!?-]*)")
	weakTypeRE     = regexp.MustCompile("(?s)(?:has type|expected to have type)\\s*\\n\\s*\\(?@?([A-Z][A-Za-z0-9_.'!?-]*)")
	fieldsMissing  = regexp.MustCompile("Fields missing: ((?:`[^`]+`(?:, )?)+)")
	privateCtorRE  = regexp.MustCompile("(?i)Constructor for `_private\\.[^`]*?\\.0\\.([^`]+)` is marked as private|from structure `_private\\.[^`]*?\\.0\\.([^`]+)` is private")
	privateBareRE  = regexp.MustCompile("(?i)`_private\\.[^`]*?\\.0\\.([^`]+)` is private")
)

var noise = map[string]struct{}{
	"fun": {}, "let": {}, "match": {}, "if": {}, "do": {}, "Prop": {}, "Type": {}, "Sort": {}, "Nat": {}, "Bool": {}, "List": {}, "Option": {}, "True": {}, "False": {}, "Eq": {}, "And": {}, "Or": {}, "Not": {}, "Exists": {},
}

// Parse extracts red modules and error blocks from a build log. roots contains
// configured module-root names, not values hard-coded by this package.
func Parse(log string, roots []string) Report {
	rootSet := make(map[string]struct{}, len(roots))
	for _, root := range roots {
		rootSet[normaliseModule(root)] = struct{}{}
	}
	report := Report{}
	red := make(map[string]struct{})
	addRed := func(module string) {
		module = normaliseModule(module)
		if module == "" || !underRoots(module, rootSet) {
			return
		}
		if _, seen := red[module]; seen {
			return
		}
		red[module] = struct{}{}
		report.RedModules = append(report.RedModules, module)
	}

	var current *Block
	flush := func() {
		if current == nil {
			return
		}
		report.Blocks = append(report.Blocks, *current)
		current = nil
	}
	for _, line := range strings.Split(log, "\n") {
		if match := buildingRE.FindStringSubmatch(line); match != nil {
			flush()
			addRed(match[1])
			continue
		}
		if match := errorRE.FindStringSubmatch(line); match != nil {
			flush()
			module := moduleOfPath(match[1])
			addRed(module)
			for _, importMatch := range importRE.FindAllStringSubmatch(match[3], -1) {
				addRed(importMatch[1])
			}
			if underRoots(module, rootSet) {
				line, _ := strconv.Atoi(match[2])
				current = &Block{Module: module, Path: match[1], Line: line, Message: match[3]}
			}
			continue
		}
		for _, match := range importRE.FindAllStringSubmatch(line, -1) {
			addRed(match[1])
		}
		if current == nil {
			continue
		}
		if blockEndRE.MatchString(line) || len(strings.Split(current.Message, "\n")) > 60 {
			flush()
			continue
		}
		current.Message += "\n" + line
	}
	flush()
	sort.Strings(report.RedModules)
	return report
}

// Names extracts the declarations named in a diagnostic block, ordered by their
// appearance class and then by the source diagnostic order.
func Names(block string) []Name {
	var found []Name
	addAll := func(matches [][]string, kind Kind) {
		for _, match := range matches {
			addName(&found, match[1], kind)
		}
	}
	addAll(unknownRE.FindAllStringSubmatch(block, -1), Unknown)
	for _, match := range privateCtorRE.FindAllStringSubmatch(block, -1) {
		for _, value := range match[1:] {
			if value != "" {
				addName(&found, value, Unknown)
			}
		}
	}
	for _, match := range privateBareRE.FindAllStringSubmatch(block, -1) {
		addName(&found, match[1], Unknown)
	}
	addAll(addedRE.FindAllStringSubmatch(block, -1), Added)
	addAll(fieldOfRE.FindAllStringSubmatch(block, -1), Changed)
	addAll(invalidFieldRE.FindAllStringSubmatch(block, -1), Changed)
	addAll(classRE.FindAllStringSubmatch(block, -1), Changed)
	addAll(applicationRE.FindAllStringSubmatch(block, -1), Changed)
	addAll(weakTypeRE.FindAllStringSubmatch(block, -1), Weak)
	if match := fieldsMissing.FindStringSubmatch(block); match != nil {
		structure := fieldOfRE.FindStringSubmatch(block)
		for _, field := range regexp.MustCompile("`([^`]+)`").FindAllStringSubmatch(match[1], -1) {
			value := field[1]
			kind := Field
			if structure != nil {
				value = structure[1] + "." + value
				kind = Unknown
			}
			addName(&found, value, kind)
		}
	}
	for _, name := range append([]Name(nil), found...) {
		if index := strings.LastIndex(name.Value, "."); index > 0 && name.Kind != Field {
			addName(&found, name.Value[:index], Weak)
		}
	}
	return found
}

func addName(names *[]Name, value string, kind Kind) {
	value = strings.Trim(normaliseModule(value), ".`")
	if value == "" || !identifier.MatchString(value) {
		return
	}
	if _, ignored := noise[value]; ignored {
		return
	}
	for _, name := range *names {
		if name.Value == value {
			return
		}
	}
	*names = append(*names, Name{Value: value, Kind: kind})
}

// declarationSet retains each declaration's complete source identity.
type declarationSet map[DeclarationID]Declaration

// Evidence describes declaration-level changes in a member's sources.
type Evidence struct {
	Removed       declarationSet
	Added         declarationSet
	Private       declarationSet
	Inside        declarationSet
	Namespaces    declarationSet
	FieldsAdded   map[string]struct{}
	FieldsRemoved map[string]struct{}
	Lines         map[DeclarationID][]Line
	Declarations  declarationSet
}

// Line is one declaration's normalised signature, removed from or added to a
// member's sources.
type Line struct {
	Sign string
	Text string
}

// IndexDeclarations records how the declarations in one member's changed
// files differ between its merge base and head.
func IndexDeclarations(before, after []Declaration) Evidence {
	evidence := newEvidence()
	beforeByID := make(declarationSet, len(before))
	for _, declaration := range before {
		beforeByID[declaration.ID()] = declaration
	}
	afterByID := make(declarationSet, len(after))
	for _, declaration := range after {
		afterByID[declaration.ID()] = declaration
	}
	for id, declaration := range beforeByID {
		candidate, found := afterByID[id]
		if !found || normaliseLine(declaration.Text) != normaliseLine(candidate.Text) {
			evidence.addDeclaration(declaration, "-")
		}
	}
	for id, declaration := range afterByID {
		base, found := beforeByID[id]
		if !found || normaliseLine(base.Text) != normaliseLine(declaration.Text) {
			evidence.addDeclaration(declaration, "+")
		}
	}
	return evidence
}

// NewEvidence returns a zero-value Evidence with initialised maps.
func NewEvidence() Evidence {
	return newEvidence()
}

func newEvidence() Evidence {
	return Evidence{
		Removed:       declarationSet{},
		Added:         declarationSet{},
		Private:       declarationSet{},
		Inside:        declarationSet{},
		Namespaces:    declarationSet{},
		FieldsAdded:   map[string]struct{}{},
		FieldsRemoved: map[string]struct{}{},
		Lines:         map[DeclarationID][]Line{},
		Declarations:  declarationSet{},
	}
}

func (e Evidence) addDeclaration(declaration Declaration, sign string) {
	id := declaration.ID()
	e.Declarations[id] = declaration
	e.Lines[id] = append(e.Lines[id], Line{Sign: sign, Text: normaliseLine(declaration.Text)})
	if sign == "+" {
		e.Added[id] = declaration
		if declaration.Private {
			e.Private[id] = declaration
		}
		return
	}
	e.Removed[id] = declaration
}

// EvidenceFor scores one member's evidence for a diagnostic name.
func EvidenceFor(name Name, evidence Evidence) (int, string) {
	removed := matchesDeclarations(evidence.Removed, name.Value)
	added := matchesDeclarations(evidence.Added, name.Value)
	private := matchesDeclarations(evidence.Private, name.Value)
	inside := matchesDeclarations(evidence.Inside, name.Value)
	namespace := false
	for _, value := range evidence.Namespaces {
		if refersToNamespace(name.Value, value.Name) {
			namespace = true
			break
		}
	}
	field := containsName(evidence.FieldsAdded, name.Value) || containsName(evidence.FieldsRemoved, name.Value)
	switch name.Kind {
	case Unknown:
		switch {
		case private:
			return 3, "privatised"
		case removed && !added:
			return 3, "removed"
		case field:
			return 3, "field removed"
		case removed && added:
			return 3, "renamed or restated"
		case namespace:
			return 2, "namespace closed"
		case inside:
			return 1, "edited"
		}
	case Added:
		switch {
		case added && !removed:
			return 3, "added"
		case added:
			return 2, "restated"
		case inside:
			return 1, "edited"
		}
	case Field:
		// Evidence records a field by its qualified name; build blame names
		// it bare and a graph finding names it qualified.
		if containsName(evidence.FieldsAdded, name.Value) || containsName(evidence.FieldsRemoved, name.Value) {
			return 2, "field"
		}
	default:
		switch {
		case removed && added:
			return 3, "signature changed"
		case private:
			return 3, "privatised"
		case removed:
			return 3, "removed"
		case field:
			return 3, "fields changed"
		case inside:
			return 2, "edited"
		case added:
			return 1, "added"
		}
	}
	return 0, ""
}

func matchesDeclarations(values declarationSet, name string) bool {
	for _, declaration := range values {
		if Refers(name, declaration) {
			return true
		}
	}
	return false
}

func containsName(values map[string]struct{}, name string) bool {
	for value := range values {
		if nameMatches(name, value) {
			return true
		}
	}
	return false
}

// Row is one member's evidence for a name.
type Row struct {
	Member   string
	Strength int
	Label    string
}

// Selection is the evidence selected for one name.
type Selection struct {
	Strength int
	Members  []Row
	Note     string
}

// Pick chooses bounded evidence. Ambiguous broad evidence blames no member.
func Pick(rows []Row) Selection {
	if len(rows) == 0 {
		return Selection{}
	}
	strong := make([]Row, 0, len(rows))
	for _, row := range rows {
		if row.Strength >= 3 {
			strong = append(strong, row)
		}
	}
	if len(strong) != 0 {
		if len(strong) > 5 {
			return Selection{Strength: 3, Note: "too many strong matches"}
		}
		sort.Slice(strong, func(i, j int) bool { return strong[i].Member < strong[j].Member })
		return Selection{Strength: 3, Members: strong}
	}
	if len(rows) > 5 {
		return Selection{Note: "too many weak matches"}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Strength != rows[j].Strength {
			return rows[i].Strength > rows[j].Strength
		}
		return rows[i].Member < rows[j].Member
	})
	if len(rows) > 3 {
		rows = rows[:3]
	}
	return Selection{Strength: rows[0].Strength, Members: rows}
}

// AgainstTree removes stale restatements and promotes additions that changed the
// candidate relative to its base. Each line includes its source file and fully
// qualified declaration name, so equal text in another declaration cannot
// suppress a real change.
func AgainstTree(name string, rows []Row, evidence map[string]Evidence, baseDeclarations, candidateDeclarations []Declaration) []Row {
	newLines := difference(declarationLineSet(name, candidateDeclarations), declarationLineSet(name, baseDeclarations))
	base := declarationLineSet(name, baseDeclarations)
	out := make([]Row, 0, len(rows))
	for _, row := range rows {
		memberEvidence, found := evidence[row.Member]
		if !found {
			out = append(out, row)
			continue
		}
		added, removed := evidenceDeclarationLines(memberEvidence, name)
		if hasIntersection(added, newLines) {
			out = append(out, Row{Member: row.Member, Strength: 3, Label: "changed in candidate"})
			continue
		}
		if isRestatement(row.Label) && len(added) != 0 && subset(added, base) && !anyOutside(removed, base) {
			continue
		}
		out = append(out, row)
	}
	return out
}

type treeDeclarationLine struct {
	ID   DeclarationID
	Text string
}

func evidenceDeclarationLines(evidence Evidence, name string) (map[treeDeclarationLine]struct{}, map[treeDeclarationLine]struct{}) {
	added, removed := map[treeDeclarationLine]struct{}{}, map[treeDeclarationLine]struct{}{}
	for id, lines := range evidence.Lines {
		declaration, found := evidence.Declarations[id]
		if !found {
			declaration = Declaration{File: id.File, Name: id.Name}
		}
		if !Refers(name, declaration) {
			continue
		}
		for _, line := range lines {
			key := treeDeclarationLine{ID: id, Text: line.Text}
			if line.Sign == "+" {
				added[key] = struct{}{}
			} else {
				removed[key] = struct{}{}
			}
		}
	}
	return added, removed
}

func isRestatement(label string) bool {
	return label == "added" || label == "restated" || label == "signature changed" || label == "renamed or restated"
}

func declarationLineSet(name string, declarations []Declaration) map[treeDeclarationLine]struct{} {
	set := make(map[treeDeclarationLine]struct{}, len(declarations))
	for _, declaration := range declarations {
		if Refers(name, declaration) {
			set[treeDeclarationLine{ID: declaration.ID(), Text: normaliseLine(declaration.Text)}] = struct{}{}
		}
	}
	return set
}

func difference(lines, base map[treeDeclarationLine]struct{}) map[treeDeclarationLine]struct{} {
	out := make(map[treeDeclarationLine]struct{}, len(lines))
	for line := range lines {
		if _, found := base[line]; !found {
			out[line] = struct{}{}
		}
	}
	return out
}

func hasIntersection(left, right map[treeDeclarationLine]struct{}) bool {
	for value := range left {
		if _, found := right[value]; found {
			return true
		}
	}
	return false
}

func subset(left, right map[treeDeclarationLine]struct{}) bool {
	for value := range left {
		if _, found := right[value]; !found {
			return false
		}
	}
	return true
}

func anyOutside(left, right map[treeDeclarationLine]struct{}) bool {
	for value := range left {
		if _, found := right[value]; !found {
			return true
		}
	}
	return false
}

// TreeLines gives the declarations that one name can refer to at the frozen
// base and in the candidate tree.
type TreeLines struct {
	Base      []Declaration
	Candidate []Declaration
}

// Attribution is one evidence-backed selection for a module diagnostic.
type Attribution struct {
	Module   string
	Name     string
	Kind     Kind
	Member   string
	Strength int
	Evidence string
}

// Identify returns the unique members selected by IdentifyDetailed. The
// declaration slices apply to every name for compatibility with callers that
// have one pair.
func Identify(report Report, evidence map[string]Evidence, baseDeclarations, candidateDeclarations []Declaration) []string {
	declarations := make(map[string]TreeLines)
	for _, block := range report.Blocks {
		for _, name := range Names(block.Message) {
			declarations[name.Value] = TreeLines{Base: baseDeclarations, Candidate: candidateDeclarations}
		}
	}
	attributions := IdentifyDetailed(report, evidence, declarations)
	members := make(map[string]struct{}, len(attributions))
	for _, attribution := range attributions {
		members[attribution.Member] = struct{}{}
	}
	out := make([]string, 0, len(members))
	for member := range members {
		out = append(out, member)
	}
	sort.Strings(out)
	return out
}

// IdentifyDetailed attributes each module independently and retains exact
// diagnostic evidence for every selected member.
func IdentifyDetailed(report Report, evidence map[string]Evidence, declarations map[string]TreeLines) []Attribution {
	byModule := make(map[string][]Name)
	for _, block := range report.Blocks {
		seen := make(map[string]struct{}, len(byModule[block.Module]))
		for _, name := range byModule[block.Module] {
			seen[name.Value] = struct{}{}
		}
		for _, name := range Names(block.Message) {
			if _, found := seen[name.Value]; found {
				continue
			}
			seen[name.Value] = struct{}{}
			byModule[block.Module] = append(byModule[block.Module], name)
		}
	}
	modules := make([]string, 0, len(byModule))
	for module := range byModule {
		modules = append(modules, module)
	}
	sort.Strings(modules)
	var out []Attribution
	for _, module := range modules {
		names := byModule[module]
		selections := make([]Selection, len(names))
		hasPrimary := false
		for index, name := range names {
			selection := SelectFor(name, evidence, declarations[name.Value])
			selections[index] = selection
			if selection.Strength != 0 && primary(name.Kind) {
				hasPrimary = true
			}
		}
		for index, selection := range selections {
			if hasPrimary && !primary(names[index].Kind) {
				continue
			}
			for _, row := range selection.Members {
				out = append(out, Attribution{
					Module:   module,
					Name:     names[index].Value,
					Kind:     names[index].Kind,
					Member:   row.Member,
					Strength: row.Strength,
					Evidence: row.Label,
				})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Member != out[j].Member {
			return out[i].Member < out[j].Member
		}
		if out[i].Module != out[j].Module {
			return out[i].Module < out[j].Module
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// SelectFor is the bounded evidence selection for one name: EvidenceFor for
// each member, AgainstTree, then Pick. Build blame and the graph checks both
// attribute a name through it.
func SelectFor(name Name, evidence map[string]Evidence, lines TreeLines) Selection {
	rows := make([]Row, 0, len(evidence))
	for member, memberEvidence := range evidence {
		strength, label := EvidenceFor(name, memberEvidence)
		if strength != 0 {
			rows = append(rows, Row{Member: member, Strength: strength, Label: label})
		}
	}
	return Pick(AgainstTree(name.Value, rows, evidence, lines.Base, lines.Candidate))
}

func primary(kind Kind) bool {
	return kind == Unknown || kind == Added || kind == Changed
}

// Confirmation is the result of building named members alone or isolating a
// prefix failure when none of the named members fails alone.
type Confirmation struct {
	Confirmed  []string
	Bisected   []string
	ByModule   map[string][]Culprit
	BaseBroken []string
}

// Culprit records how confirmation identified a member for one module.
type Culprit struct {
	Member string
	Kind   string
}

// Confirm preserves the original aggregate API. New callers should use
// ConfirmModules so every red module is independently resolved.
func Confirm(ctx context.Context, base string, modules []string, members []Member, named []string, candidate Candidate, builder Builder) (Confirmation, error) {
	suspects := make(map[string][]string, len(modules))
	for _, module := range modules {
		suspects[module] = append([]string(nil), named...)
	}
	return ConfirmModules(ctx, base, modules, members, suspects, candidate, builder)
}

// ConfirmModules checks the base, red-alone suspects, and prefix bisection for
// every red module. A confirmation in one module never hides another module.
func ConfirmModules(ctx context.Context, base string, modules []string, members []Member, suspects map[string][]string, candidate Candidate, builder Builder) (Confirmation, error) {
	byID := make(map[string]Member, len(members))
	for _, member := range members {
		byID[member.ID] = member
	}
	out := Confirmation{ByModule: make(map[string][]Culprit)}
	for _, module := range modules {
		baseTree, err := candidate(ctx, base, nil)
		if err != nil {
			return Confirmation{}, fmt.Errorf("build base for %q: %w", module, err)
		}
		baseResult, err := builder.Build(ctx, baseTree, []string{module})
		if err != nil {
			return Confirmation{}, fmt.Errorf("check base for %q: %w", module, err)
		}
		if baseResult.InfraClass != "" {
			return Confirmation{}, fmt.Errorf("check base for %q: infrastructure failure: %s", module, baseResult.InfraClass)
		}
		if !baseResult.Absent && !baseResult.Green {
			out.BaseBroken = append(out.BaseBroken, module)
			continue
		}
		for _, id := range suspects[module] {
			member, found := byID[id]
			if !found {
				continue
			}
			tree, err := candidate(ctx, base, []Member{member})
			if err != nil {
				return Confirmation{}, fmt.Errorf("build candidate for %q in %q: %w", id, module, err)
			}
			red, err := isRed(ctx, builder, tree, []string{module})
			if err != nil {
				return Confirmation{}, fmt.Errorf("confirm %q in %q: %w", id, module, err)
			}
			if red {
				out.ByModule[module] = append(out.ByModule[module], Culprit{Member: id, Kind: "evidence"})
			}
		}
		if len(out.ByModule[module]) != 0 {
			continue
		}
		culprit, found, err := bisectModule(ctx, base, module, members, candidate, builder)
		if err != nil {
			return Confirmation{}, err
		}
		if found {
			out.ByModule[module] = append(out.ByModule[module], Culprit{Member: culprit, Kind: "bisect"})
		}
	}
	for module, culprits := range out.ByModule {
		sort.Slice(culprits, func(i, j int) bool { return culprits[i].Member < culprits[j].Member })
		out.ByModule[module] = culprits
		for _, culprit := range culprits {
			if culprit.Kind == "evidence" {
				out.Confirmed = append(out.Confirmed, culprit.Member)
			} else {
				out.Bisected = append(out.Bisected, culprit.Member)
			}
		}
	}
	sort.Strings(out.Confirmed)
	sort.Strings(out.Bisected)
	sort.Strings(out.BaseBroken)
	return out, nil
}

func bisectModule(ctx context.Context, base, module string, members []Member, candidate Candidate, builder Builder) (string, bool, error) {
	if len(members) == 0 {
		return "", false, nil
	}
	allTree, err := candidate(ctx, base, members)
	if err != nil {
		return "", false, fmt.Errorf("build full candidate for %q: %w", module, err)
	}
	red, err := isRed(ctx, builder, allTree, []string{module})
	if err != nil {
		return "", false, fmt.Errorf("check full candidate for %q: %w", module, err)
	}
	if !red {
		return "", false, nil
	}
	lower, upper := 0, len(members)
	for upper-lower > 1 {
		middle := (lower + upper) / 2
		tree, err := candidate(ctx, base, members[:middle])
		if err != nil {
			return "", false, fmt.Errorf("build prefix %d for %q: %w", middle, module, err)
		}
		red, err := isRed(ctx, builder, tree, []string{module})
		if err != nil {
			return "", false, fmt.Errorf("check prefix %d for %q: %w", middle, module, err)
		}
		if red {
			upper = middle
		} else {
			lower = middle
		}
	}
	return members[upper-1].ID, true, nil
}

func isRed(ctx context.Context, builder Builder, tree Tree, modules []string) (bool, error) {
	result, err := builder.Build(ctx, tree, modules)
	if err != nil {
		return false, err
	}
	if result.InfraClass != "" {
		return false, fmt.Errorf("infrastructure failure: %s", result.InfraClass)
	}
	if result.Absent {
		return false, nil
	}
	return !result.Green, nil
}

func moduleOfPath(path string) string {
	return normaliseModule(strings.TrimSuffix(path, ".lean"))
}

func underRoots(module string, roots map[string]struct{}) bool {
	if len(roots) == 0 {
		return true
	}
	root := module
	if index := strings.IndexByte(root, '.'); index >= 0 {
		root = root[:index]
	}
	_, found := roots[root]
	return found
}

func normaliseModule(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(value, "«", ""), "»", ""), "/", ".")
}

func normaliseLine(line string) string {
	return strings.Join(strings.Fields(line), " ")
}

func nameMatches(left, right string) bool {
	left, right = normaliseModule(left), normaliseModule(right)
	if last(left) != last(right) {
		return false
	}
	return left == right || strings.HasSuffix(left, "."+right) || strings.HasSuffix(right, "."+left)
}

func last(value string) string {
	if index := strings.LastIndex(value, "."); index >= 0 {
		return value[index+1:]
	}
	return value
}
