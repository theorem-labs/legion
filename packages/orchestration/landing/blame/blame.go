// Package blame attributes build failures to candidate members from build output and diffs.
package blame

import (
	"context"
	"fmt"
	"regexp"
	"sort"
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

// Block is one diagnostic block associated with a module.
type Block struct {
	Module  string
	Path    string
	Message string
}

// Report is the red modules and diagnostics parsed from a build log.
type Report struct {
	RedModules []string
	Blocks     []Block
}

var (
	buildingRE     = regexp.MustCompile(`^✖ \[[0-9]+/[0-9]+\] (?:Building|Running|Compiling) (\S+)$`)
	errorRE        = regexp.MustCompile(`^error: (?:\./)?([^:\s]+\.lean):[0-9]+:[0-9]+: ?(.*)$`)
	importRE       = regexp.MustCompile(`\bimport (\S+) failed`)
	blockEndRE     = regexp.MustCompile(`^(?:error:|warning:|info:|trace:|✔|✖|⚠|\[|-- build round|Build completed|Some required|Some builds)`)
	identifier     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.'!?-]*$`)
	declarationRE  = regexp.MustCompile(`^\s*(?:(?:private|protected|noncomputable|scoped|local|unsafe|partial|nonrec|public|meta)\s+)*(?:def|theorem|lemma|structure|class|inductive|abbrev|instance|axiom|opaque|notation|macro|syntax|macro_rules|elab|alias)\s+([^\s(:{\[⟨]+)`)
	namespaceRE    = regexp.MustCompile(`^\s*(?:namespace|end)\s+([A-Za-z_][A-Za-z0-9_.'«»-]*)\s*$`)
	fieldRE        = regexp.MustCompile(`^\s+([A-Za-z_][A-Za-z0-9_']*)\s*:`)
	hunkRE         = regexp.MustCompile(`^@@ [^@]*@@ ?(.*)$`)
	unknownRE      = regexp.MustCompile("(?i)(?:unknown identifier|unknown constant|the environment does not contain|unknown namespace)\\s*[`']([^`']+)[`']")
	addedRE        = regexp.MustCompile(`environment already contains '([^']+)'`)
	fieldOfRE      = regexp.MustCompile("is not a field of structure `([^`]+)`")
	invalidFieldRE = regexp.MustCompile("(?s)Invalid field `[^`]+`.*?of type `([^`\\s]+)")
	classRE        = regexp.MustCompile("(?s)failed to synthesize(?: instance of type class)?\\s*[`']?([A-Z][A-Za-z0-9_.'!?-]*)")
	applicationRE  = regexp.MustCompile("(?s)(?:in the application|type expected, got)\\s*\\n\\s*\\(?@?([A-Za-z_][A-Za-z0-9_.'!?-]*)")
	weakTypeRE     = regexp.MustCompile("(?s)(?:has type|expected to have type)\\s*\\n\\s*\\(?@?([A-Z][A-Za-z0-9_.'!?-]*)")
	fieldsMissing  = regexp.MustCompile("Fields missing: ((?:`[^`]+`(?:, )?)+)")
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
			if underRoots(module, rootSet) {
				current = &Block{Module: module, Path: match[1], Message: match[2]}
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

// Evidence describes declaration-level changes in a unified zero-context diff.
type Evidence struct {
	Removed       map[string]struct{}
	Added         map[string]struct{}
	Private       map[string]struct{}
	Inside        map[string]struct{}
	Namespaces    map[string]struct{}
	FieldsAdded   map[string]struct{}
	FieldsRemoved map[string]struct{}
	Lines         map[string][]Line
}

// Line is one normalised declaration line added to or removed from a diff.
type Line struct {
	Sign string
	Text string
}

// IndexDiff extracts evidence from a unified zero-context diff.
func IndexDiff(diff string) Evidence {
	evidence := newEvidence()
	scope := ""
	for _, line := range strings.Split(diff, "\n") {
		if match := hunkRE.FindStringSubmatch(line); match != nil {
			scope = declarationName(match[1])
			continue
		}
		if len(line) == 0 || (line[0] != '+' && line[0] != '-') || strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---") {
			continue
		}
		sign, body := line[:1], stripComment(line[1:])
		if declaration := declarationName(body); declaration != "" {
			evidence.Lines[declaration] = append(evidence.Lines[declaration], Line{Sign: sign, Text: normaliseLine(body)})
			if sign == "+" {
				evidence.Added[declaration] = struct{}{}
				if strings.Contains(strings.Fields(body)[0], "private") || strings.HasPrefix(strings.TrimSpace(body), "private ") {
					evidence.Private[declaration] = struct{}{}
				}
			} else {
				evidence.Removed[declaration] = struct{}{}
			}
			scope = declaration
			continue
		}
		if match := namespaceRE.FindStringSubmatch(body); match != nil && sign == "-" {
			evidence.Namespaces[match[1]] = struct{}{}
			continue
		}
		if match := fieldRE.FindStringSubmatch(body); match != nil && scope != "" {
			field := scope + "." + match[1]
			if sign == "+" {
				evidence.FieldsAdded[field] = struct{}{}
			} else {
				evidence.FieldsRemoved[field] = struct{}{}
			}
		}
	}
	return evidence
}

func newEvidence() Evidence {
	return Evidence{
		Removed: map[string]struct{}{}, Added: map[string]struct{}{}, Private: map[string]struct{}{}, Inside: map[string]struct{}{}, Namespaces: map[string]struct{}{}, FieldsAdded: map[string]struct{}{}, FieldsRemoved: map[string]struct{}{}, Lines: map[string][]Line{},
	}
}

func declarationName(line string) string {
	match := declarationRE.FindStringSubmatch(stripComment(line))
	if match == nil {
		return ""
	}
	return normaliseModule(match[1])
}

// EvidenceFor scores one member's evidence for a diagnostic name.
func EvidenceFor(name Name, evidence Evidence) (int, string) {
	removed := containsMatch(evidence.Removed, name.Value)
	added := containsMatch(evidence.Added, name.Value)
	private := containsMatch(evidence.Private, name.Value)
	inside := containsMatch(evidence.Inside, name.Value)
	namespace := false
	for value := range evidence.Namespaces {
		if nameMatches(name.Value, value) || strings.HasPrefix(name.Value, value+".") {
			namespace = true
			break
		}
	}
	field := containsMatch(evidence.FieldsAdded, name.Value) || containsMatch(evidence.FieldsRemoved, name.Value)
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
		for value := range evidence.FieldsAdded {
			if last(value) == name.Value {
				return 2, "field"
			}
		}
		for value := range evidence.FieldsRemoved {
			if last(value) == name.Value {
				return 2, "field"
			}
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

func containsMatch(values map[string]struct{}, name string) bool {
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
	if len(rows) > 3 {
		return Selection{Note: "too many weak matches"}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Strength != rows[j].Strength {
			return rows[i].Strength > rows[j].Strength
		}
		return rows[i].Member < rows[j].Member
	})
	return Selection{Strength: rows[0].Strength, Members: rows}
}

// AgainstTree removes stale restatements and promotes additions that changed the
// candidate relative to its base.
func AgainstTree(name string, rows []Row, evidence map[string]Evidence, baseLines, candidateLines []string) []Row {
	newLines := difference(candidateLines, baseLines)
	base := lineSet(baseLines)
	out := make([]Row, 0, len(rows))
	for _, row := range rows {
		memberEvidence, found := evidence[row.Member]
		if !found {
			out = append(out, row)
			continue
		}
		added, removed := declarationLines(memberEvidence, name)
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

func declarationLines(evidence Evidence, name string) (map[string]struct{}, map[string]struct{}) {
	added, removed := map[string]struct{}{}, map[string]struct{}{}
	for declaration, lines := range evidence.Lines {
		if !nameMatches(name, declaration) {
			continue
		}
		for _, line := range lines {
			if line.Sign == "+" {
				added[line.Text] = struct{}{}
			} else {
				removed[line.Text] = struct{}{}
			}
		}
	}
	return added, removed
}

func isRestatement(label string) bool {
	return label == "added" || label == "restated" || label == "signature changed" || label == "renamed or restated"
}

func lineSet(lines []string) map[string]struct{} {
	set := make(map[string]struct{}, len(lines))
	for _, line := range lines {
		set[normaliseLine(line)] = struct{}{}
	}
	return set
}

func difference(lines []string, base []string) map[string]struct{} {
	all := lineSet(lines)
	for line := range lineSet(base) {
		delete(all, line)
	}
	return all
}

func hasIntersection(left, right map[string]struct{}) bool {
	for value := range left {
		if _, found := right[value]; found {
			return true
		}
	}
	return false
}

func subset(left, right map[string]struct{}) bool {
	for value := range left {
		if _, found := right[value]; !found {
			return false
		}
	}
	return true
}

func anyOutside(left, right map[string]struct{}) bool {
	for value := range left {
		if _, found := right[value]; !found {
			return true
		}
	}
	return false
}

// Identify returns evidence-backed members only. It deliberately has no import
// closure input: touching a failed module's closure is never enough to blame a member.
func Identify(report Report, evidence map[string]Evidence, baseLines, candidateLines []string) []string {
	culprits := map[string]struct{}{}
	for _, block := range report.Blocks {
		names := Names(block.Message)
		selections := make([]Selection, 0, len(names))
		hasPrimary := false
		for _, name := range names {
			rows := make([]Row, 0, len(evidence))
			for member, memberEvidence := range evidence {
				strength, label := EvidenceFor(name, memberEvidence)
				if strength != 0 {
					rows = append(rows, Row{Member: member, Strength: strength, Label: label})
				}
			}
			rows = AgainstTree(name.Value, rows, evidence, baseLines, candidateLines)
			selection := Pick(rows)
			if selection.Strength != 0 && name.Kind != Weak && name.Kind != Field {
				hasPrimary = true
			}
			selections = append(selections, selection)
		}
		for index, selection := range selections {
			if hasPrimary && (names[index].Kind == Weak || names[index].Kind == Field) {
				continue
			}
			for _, row := range selection.Members {
				culprits[row.Member] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(culprits))
	for member := range culprits {
		out = append(out, member)
	}
	sort.Strings(out)
	return out
}

// Confirmation is the result of building named members alone or isolating a
// prefix failure when none of the named members fails alone.
type Confirmation struct {
	Confirmed []string
	Bisected  []string
}

// Confirm builds each named member on the base and confirms only red members.
// If no member is confirmed, it locates the first red prefix member.
func Confirm(ctx context.Context, base string, modules []string, members []Member, named []string, candidate Candidate, builder Builder) (Confirmation, error) {
	byID := make(map[string]Member, len(members))
	for _, member := range members {
		byID[member.ID] = member
	}
	confirmation := Confirmation{}
	for _, id := range named {
		member, found := byID[id]
		if !found {
			continue
		}
		tree, err := candidate(ctx, base, []Member{member})
		if err != nil {
			return Confirmation{}, fmt.Errorf("build candidate for %q: %w", id, err)
		}
		red, err := isRed(ctx, builder, tree, modules)
		if err != nil {
			return Confirmation{}, fmt.Errorf("confirm %q: %w", id, err)
		}
		if red {
			confirmation.Confirmed = append(confirmation.Confirmed, id)
		}
	}
	if len(confirmation.Confirmed) != 0 {
		sort.Strings(confirmation.Confirmed)
		return confirmation, nil
	}
	allTree, err := candidate(ctx, base, members)
	if err != nil {
		return Confirmation{}, fmt.Errorf("build full candidate: %w", err)
	}
	red, err := isRed(ctx, builder, allTree, modules)
	if err != nil {
		return Confirmation{}, fmt.Errorf("check full candidate: %w", err)
	}
	if !red || len(members) == 0 {
		return confirmation, nil
	}
	lower, upper := 0, len(members)
	for upper-lower > 1 {
		middle := (lower + upper) / 2
		tree, err := candidate(ctx, base, members[:middle])
		if err != nil {
			return Confirmation{}, fmt.Errorf("build prefix %d: %w", middle, err)
		}
		red, err := isRed(ctx, builder, tree, modules)
		if err != nil {
			return Confirmation{}, fmt.Errorf("check prefix %d: %w", middle, err)
		}
		if red {
			upper = middle
		} else {
			lower = middle
		}
	}
	confirmation.Bisected = []string{members[upper-1].ID}
	return confirmation, nil
}

func isRed(ctx context.Context, builder Builder, tree Tree, modules []string) (bool, error) {
	result, err := builder.Build(ctx, tree, modules)
	if err != nil {
		return false, err
	}
	if result.InfraClass != "" {
		return false, fmt.Errorf("infrastructure failure: %s", result.InfraClass)
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

func stripComment(line string) string {
	if index := strings.Index(line, "--"); index >= 0 {
		return line[:index]
	}
	return line
}

func normaliseLine(line string) string {
	return strings.Join(strings.Fields(stripComment(line)), " ")
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
