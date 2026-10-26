package graphcheck

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// lakeSetup is the part of a module's setup file (`<build>/ir/<Module>.setup.json`) that Lake hands Lean: the
// package the module belongs to, the options of its package and library, and the shared libraries Lean loads as
// plugins (the toolchain's, the dependencies', and the precompiled modules' of its import closure).
type lakeSetup struct {
	Package string                     `json:"package"`
	Options map[string]json.RawMessage `json:"options"`
	Plugins []struct {
		Path string `json:"path"`
	} `json:"plugins"`
}

// lakeLibrary is one `[[lean_lib]]` of the root package, as `lakefile.toml` declares it: where its sources are,
// and which modules it builds.
type lakeLibrary struct {
	Name string
	// SrcDir is where the library's modules live: the package's source directory joined with the library's own.
	SrcDir string
	// Roots are the module names the library builds; Lake defaults them to the library's name. A module is the
	// library's when a root is a prefix of its name (Lake's isLocalModule), whatever the globs say.
	Roots []string
	// Globs are the library's globs, each a root (`Extra`), its submodules (`Extra.*`) or both (`Extra.+`).
	Globs []string
}

// owns reports whether a library builds a module: one of its roots is the module or a prefix of it, or one of
// its globs matches it.
func (l lakeLibrary) owns(module string) bool {
	for _, root := range l.Roots {
		if module == root || strings.HasPrefix(module, root+".") {
			return true
		}
	}
	for _, glob := range l.Globs {
		switch {
		case strings.HasSuffix(glob, ".+"):
			root := strings.TrimSuffix(glob, ".+")
			if module == root || strings.HasPrefix(module, root+".") {
				return true
			}
		case strings.HasSuffix(glob, ".*"):
			if strings.HasPrefix(module, strings.TrimSuffix(glob, ".*")+".") {
				return true
			}
		case module == glob:
			return true
		}
	}
	return false
}

// libraryOf is the library of the root package that builds a file, and the module name it builds it under. A
// file counts only when a library's source directory holds it, the library owns the module, and the base build
// has an output for it or for a module above it: a tree no library builds (a script, an archived copy) belongs
// to no library, whatever its path looks like.
//
// It reads `lakefile.toml`, Lake's own declaration of the build, never Lean source. known is false where only
// Lake could answer, and the caller is then unable rather than guessing.
func (c *Candidate) libraryOf(file string) (library, module string, known bool) {
	libraries, known := c.lakeLibraries()
	if !known {
		return "", "", false
	}
	file = filepath.ToSlash(file)
	for _, lib := range libraries {
		rel, err := filepath.Rel(cmp.Or(lib.SrcDir, "."), file)
		if err != nil || strings.HasPrefix(rel, "..") {
			continue
		}
		module := strings.ReplaceAll(strings.TrimSuffix(filepath.ToSlash(rel), ".lean"), "/", ".")
		if lib.owns(module) && c.builtModule(module) {
			return lib.Name, module, true
		}
	}
	return "", "", true
}

// builtModule reports whether the base build has an output for a module or for one above it: an object file, or
// a directory holding submodules.
func (c *Candidate) builtModule(module string) bool {
	for name := module; ; {
		base := filepath.Join(c.icfg.Lib, filepath.Join(strings.Split(name, ".")...))
		for _, path := range []string{base + ".olean", base} {
			if _, err := os.Stat(path); err == nil {
				return true
			}
		}
		cut := strings.LastIndexByte(name, '.')
		if cut < 0 {
			return false
		}
		name = name[:cut]
	}
}

// lakeLibraries reads the root package's `[[lean_lib]]` declarations once. known is false when the package's
// configuration is one only Lake can evaluate: a `lakefile.lean`, which Lake prefers wherever it is beside a
// `lakefile.toml`, or a `lakefile.toml` this reader does not understand. A tree with no configuration at all
// builds nothing and declares no library.
func (c *Candidate) lakeLibraries() ([]lakeLibrary, bool) {
	c.librariesOnce.Do(func() {
		if _, err := os.Stat(filepath.Join(c.icfg.Tree, "lakefile.lean")); err == nil {
			return
		}
		raw, err := os.ReadFile(filepath.Join(c.icfg.Tree, "lakefile.toml"))
		if errors.Is(err, fs.ErrNotExist) {
			c.librariesKnown = true
			return
		} else if err != nil {
			return
		}
		c.libraries, c.librariesKnown = parseLakefile(string(raw))
	})
	return c.libraries, c.librariesKnown
}

// parseLakefile reads the package's source directory and its `[[lean_lib]]` tables out of a `lakefile.toml`. It
// understands what Lake's own configuration format holds — tables, array-of-table headers, and string, boolean,
// numeric and string-array values over one or more lines — and reports false for anything else, so a
// configuration it cannot read makes the caller unable rather than guess what the build builds.
func parseLakefile(text string) ([]lakeLibrary, bool) {
	var libraries []lakeLibrary
	var packageSrcDir string
	table, lines := "", strings.Split(text, "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(stripTOMLComment(lines[i]))
		if line == "" {
			continue
		}
		if open := len(line) - len(strings.TrimLeft(line, "[")); open > 0 {
			name, ok := strings.CutSuffix(line[open:], strings.Repeat("]", open))
			if !ok || open > 2 {
				return nil, false
			}
			table = strings.TrimSpace(name)
			if open == 2 && table == "lean_lib" {
				libraries = append(libraries, lakeLibrary{})
			}
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, false
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		// A quoted or dotted key, and a library written anywhere but under its own header (an array of inline
		// tables, say), are shapes this reader does not take: Lake reads them, so only it can answer.
		if strings.ContainsAny(key, `".'`) || key == "" {
			return nil, false
		}
		if table != "lean_lib" && (key == "lean_lib" || key == "lean_libs") {
			return nil, false
		}
		// An array over several lines runs to the line that closes it.
		for strings.HasPrefix(value, "[") && !strings.HasSuffix(value, "]") && i+1 < len(lines) {
			i++
			value = strings.TrimSpace(value + " " + strings.TrimSpace(stripTOMLComment(lines[i])))
		}
		switch {
		case table == "" && key == "srcDir":
			if packageSrcDir, ok = tomlString(value); !ok {
				return nil, false
			}
		case table != "lean_lib":
			continue
		case key == "name", key == "srcDir":
			text, ok := tomlString(value)
			if !ok {
				return nil, false
			}
			if key == "name" {
				libraries[len(libraries)-1].Name = text
			} else {
				libraries[len(libraries)-1].SrcDir = text
			}
		case key == "roots", key == "globs":
			items, ok := tomlStrings(value)
			if !ok {
				return nil, false
			}
			if key == "roots" {
				libraries[len(libraries)-1].Roots = items
			} else {
				libraries[len(libraries)-1].Globs = items
			}
		}
	}
	for i := range libraries {
		if libraries[i].Name == "" {
			return nil, false
		}
		// Lake joins the package's source directory with the library's own, and defaults the library's roots to
		// its name. A name or a root may be written in guillemets, which are no part of the module's name.
		libraries[i].SrcDir = filepath.Join(packageSrcDir, libraries[i].SrcDir)
		libraries[i].Name = unquoteName(libraries[i].Name)
		if len(libraries[i].Roots) == 0 {
			libraries[i].Roots = []string{libraries[i].Name}
		}
		for j, root := range libraries[i].Roots {
			libraries[i].Roots[j] = unquoteName(root)
		}
		for j, glob := range libraries[i].Globs {
			libraries[i].Globs[j] = unquoteName(glob)
		}
	}
	return libraries, true
}

// unquoteName drops the guillemets Lean allows around a name's components: «Lib».«Odd name» is Lib.Odd name.
func unquoteName(name string) string {
	return strings.NewReplacer("«", "", "»", "").Replace(name)
}

// stripTOMLComment drops a trailing comment, leaving a `#` inside a quoted value alone.
func stripTOMLComment(line string) string {
	var quote rune
	for i, r := range line {
		switch {
		case r == quote:
			quote = 0
		case quote != 0:
		case r == '"' || r == '\'':
			quote = r
		case r == '#':
			return line[:i]
		}
	}
	return line
}

// tomlString reads one string: a literal string, whose content is its own, or a basic string, which may escape a
// quote or a backslash. A multi-line string of either kind is not read, and makes the caller refuse the file.
func tomlString(value string) (string, bool) {
	if literal, ok := strings.CutPrefix(value, "'"); ok {
		body, ok := strings.CutSuffix(literal, "'")
		return body, ok && !strings.Contains(body, "'")
	}
	if len(value) < 2 || value[0] != '"' || !strings.HasSuffix(value, `"`) {
		return "", false
	}
	var b strings.Builder
	body := value[1 : len(value)-1]
	for i := 0; i < len(body); i++ {
		switch {
		case body[i] == '"':
			return "", false
		case body[i] == '\\' && i+1 < len(body) && (body[i+1] == '"' || body[i+1] == '\\'):
			i++
			b.WriteByte(body[i])
		case body[i] == '\\':
			return "", false
		default:
			b.WriteByte(body[i])
		}
	}
	return b.String(), true
}

// tomlStrings reads an array of basic strings.
func tomlStrings(value string) ([]string, bool) {
	body, ok := strings.CutPrefix(value, "[")
	if !ok {
		return nil, false
	}
	body, ok = strings.CutSuffix(strings.TrimSpace(body), "]")
	if !ok {
		return nil, false
	}
	var out []string
	for _, item := range strings.Split(body, ",") {
		if item = strings.TrimSpace(item); item == "" {
			continue
		}
		text, ok := tomlString(item)
		if !ok {
			return nil, false
		}
		out = append(out, text)
	}
	return out, true
}

// lakeState is the candidate's view of the base build's setup files: each file read once, and each module's plugin
// list worked out once.
type lakeState struct {
	// mu guards every map below: the compiles that ask for their arguments run in parallel.
	mu      sync.Mutex
	c       *Candidate
	dir     string                // the root package's setup directory
	setups  map[string]*lakeSetup // module -> its base setup in whichever package builds it, nil when none has one
	plugins map[string][]string   // module without a setup of its own -> the plugin list worked out for it
	pending map[string]bool       // modules whose plugin list is being worked out
	stale   map[string]string     // shared library path -> the changed precompiled module it belongs to
}

// leanArgs gives the arguments Lake gives a module's compile, read from the base build's setup files:
// `-D<name>=<value>` for each option of its package and library, sorted, then `--plugin=<path>` for each shared
// library Lake loads, in Lake's load order, each once.
//
// A module with a setup of its own takes its options and plugins from it. One without (an added module) takes the
// options of its own library, which Lake gives every module of the library alike, and the plugins its imports
// bring: each import's own list (found in the build of the package that builds it, a dependency's included) and the
// import's own precompiled shared library, merged into one order that keeps each list's order. Where Lake's answer
// cannot be read off the base build (a library with no module built, two imports that order two plugins
// differently, a changed module whose shared library would be stale), the check is unable, never wrong. A tree
// that Lake did not build has no setup directory and gives no arguments.
func (c *Candidate) leanArgs(m *Module) ([]string, error) {
	dir := filepath.Join(filepath.Dir(filepath.Dir(c.icfg.Lib)), "ir")
	if info, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) || err == nil && !info.IsDir() {
		return nil, nil
	} else if err != nil {
		return nil, unable("the base build's setup directory %s cannot be read: %v", dir, err)
	}
	c.lakeOnce.Do(func() {
		s := &lakeState{c: c, dir: dir, setups: map[string]*lakeSetup{}, plugins: map[string][]string{},
			pending: map[string]bool{}, stale: map[string]string{}}
		c.lakeErr = s.findStale()
		c.lake = s
	})
	if c.lakeErr != nil {
		return nil, c.lakeErr
	}
	s := c.lake
	s.mu.Lock()
	defer s.mu.Unlock()
	own, err := s.setup(m.Module)
	if err != nil {
		return nil, err
	}
	opts := own
	if opts == nil {
		if opts, err = s.libraryOptions(m.Module); err != nil {
			return nil, err
		}
	}
	plugins, err := s.pluginsOf(m)
	if err != nil {
		return nil, err
	}
	var options []string
	for name, raw := range opts.Options {
		value, err := optionValue(raw)
		if err != nil {
			return nil, unable("the base build's setup of %s has option %s: %v", m.Module, name, err)
		}
		options = append(options, "-D"+name+"="+value)
	}
	sort.Strings(options)
	args := slices.Compact(options)
	// The plugins keep Lake's order: one plugin's symbols resolve against those loaded before it, so a sorted list
	// fails to load.
	for _, p := range plugins {
		if module, ok := s.stale[p]; ok {
			return nil, staleLibrary(module, m.Module)
		}
		args = append(args, "--plugin="+p)
	}
	return args, nil
}

// pluginsOf is a module's plugin list in load order: its own setup's, else the merge of what its imports bring.
func (s *lakeState) pluginsOf(m *Module) ([]string, error) {
	own, err := s.setup(m.Module)
	if err != nil {
		return nil, err
	}
	if own != nil {
		return own.pluginPaths(), nil
	}
	if plugins, ok := s.plugins[m.Module]; ok {
		return plugins, nil
	}
	if s.pending[m.Module] {
		return nil, unable("the candidate's imports of %s form a cycle", m.Module)
	}
	s.pending[m.Module] = true
	defer delete(s.pending, m.Module)
	var lists [][]string
	for _, imp := range m.Imports {
		var list []string
		if changed := s.c.modules[imp]; changed != nil {
			if list, err = s.pluginsOf(changed); err != nil {
				return nil, err
			}
		} else {
			setup, err := s.setup(imp)
			if err != nil {
				return nil, err
			}
			if setup == nil {
				if !s.ofRoots(imp) {
					// A module of the toolchain or of a package Lake builds outside the project: it brings no
					// plugin of its own, as the setups of the modules that do import it show.
					continue
				}
				// A module of the project that the base build did not build: the check cannot read what Lake
				// loads for it.
				return nil, unable("%s imports %s, which the base build did not build, so the plugins Lake gives "+
					"the compile cannot be read off it", m.Module, imp)
			}
			list = setup.pluginPaths()
		}
		// An import that is precompiled loads its own shared library, whether the batch changes it or not; a
		// changed one's is stale, which the caller refuses.
		if pkg, ok := s.packageOf(imp); ok {
			if lib, ok := s.sharedLibrary(imp, pkg); ok {
				list = append(list, lib)
			}
		}
		lists = append(lists, list)
	}
	plugins, ok := mergeOrders(lists)
	if !ok {
		return nil, unable("the imports of %s load their plugins in orders that disagree, so Lake's order for it cannot be read off the base build", m.Module)
	}
	s.plugins[m.Module] = plugins
	return plugins, nil
}

// pluginPaths is a setup's plugin list, each path once, in its order.
func (setup *lakeSetup) pluginPaths() []string {
	var out []string
	seen := map[string]bool{}
	for _, p := range setup.Plugins {
		if !seen[p.Path] {
			seen[p.Path] = true
			out = append(out, p.Path)
		}
	}
	return out
}

// mergeOrders merges lists that each give a load order into one order that keeps every list's order, the earliest
// seen first where they leave a choice. It reports false when two lists order two entries differently.
func mergeOrders(lists [][]string) ([]string, bool) {
	var order []string
	first := map[string]int{}
	after := map[string]map[string]bool{}
	waits := map[string]int{}
	for _, list := range lists {
		for i, p := range list {
			if _, ok := first[p]; !ok {
				first[p] = len(order)
				order = append(order, p)
			}
			if i == 0 || list[i-1] == p {
				continue
			}
			prev := list[i-1]
			if after[prev] == nil {
				after[prev] = map[string]bool{}
			}
			if !after[prev][p] {
				after[prev][p] = true
				waits[p]++
			}
		}
	}
	out := make([]string, 0, len(order))
	done := map[string]bool{}
	for len(out) < len(order) {
		next := ""
		for _, p := range order {
			if !done[p] && waits[p] == 0 {
				next = p
				break
			}
		}
		if next == "" {
			return nil, false
		}
		done[next] = true
		out = append(out, next)
		for p := range after[next] {
			waits[p]--
		}
	}
	return out, true
}

// libraryOptions is the setup an added module takes its options from: any base setup of a module of its own
// library (its first name component), as Lake gives all of them the package's and the library's options. With no
// module of the library built, there is nothing to read them from.
func (s *lakeState) libraryOptions(module string) (*lakeSetup, error) {
	library := nameComponents(module)[0]
	var found string
	for _, start := range []string{filepath.Join(s.dir, library+".setup.json"), filepath.Join(s.dir, library)} {
		_ = filepath.WalkDir(start, func(p string, entry fs.DirEntry, err error) error {
			if err != nil || found != "" {
				return filepath.SkipAll
			}
			if !entry.IsDir() && strings.HasSuffix(p, ".setup.json") {
				found = p
				return filepath.SkipAll
			}
			return nil
		})
		if found != "" {
			break
		}
	}
	if found == "" {
		return nil, unable("no module of library %s is in the base build, so the options Lake gives its new module %s cannot be read", library, module)
	}
	return readSetup(found)
}

func staleLibrary(module, importer string) error {
	return unable("%s is a precompiled module that the candidate changes, and compiling %s loads its shared library; "+
		"the check cannot rebuild a precompiled module's shared library", module, importer)
}

// findStale records the base shared library of every changed module that has one.
func (s *lakeState) findStale() error {
	for name := range s.c.modules {
		setup, err := s.setup(name)
		if err != nil {
			return err
		}
		if setup == nil {
			continue
		}
		if lib, ok := s.sharedLibrary(name, setup.Package); ok {
			s.stale[lib] = name
		}
	}
	return nil
}

// setup reads a module's base setup file from the build of the package that builds it, or gives nil when none of
// them has one. Lake keeps each package's setups beside its library directory, `<build>/ir`, so each directory on
// the search path names one; the root package's comes first.
func (s *lakeState) setup(module string) (*lakeSetup, error) {
	if setup, ok := s.setups[module]; ok {
		return setup, nil
	}
	rel := filepath.Join(nameComponents(module)...) + ".setup.json"
	dirs := []string{s.dir}
	for _, lib := range s.c.leanPath {
		dirs = append(dirs, filepath.Join(filepath.Dir(filepath.Dir(lib)), "ir"))
	}
	for _, dir := range dirs {
		path := filepath.Join(dir, rel)
		if _, err := os.Stat(path); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, unable("the base build's setup of %s cannot be read: %v", module, err)
		}
		setup, err := readSetup(path)
		if err != nil {
			return nil, err
		}
		s.setups[module] = setup
		return setup, nil
	}
	s.setups[module] = nil
	return nil, nil
}

// readSetup reads one setup file.
func readSetup(path string) (*lakeSetup, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, unable("the base build's setup %s cannot be read: %v", path, err)
	}
	var setup lakeSetup
	if err := json.Unmarshal(raw, &setup); err != nil {
		return nil, unable("the base build's setup %s is not Lake's: %v", path, err)
	}
	return &setup, nil
}

// ofRoots reports whether a module belongs to the project the index reads, by its first name component. A module
// of the toolchain or of a package outside the project is not one the base build builds.
func (s *lakeState) ofRoots(module string) bool {
	return slices.Contains(s.c.icfg.Roots, nameComponents(module)[0])
}

// packageOf is the package that builds a module, from its base setup; false when the base build has none.
func (s *lakeState) packageOf(module string) (string, bool) {
	setup, err := s.setup(module)
	if err != nil || setup == nil {
		return "", false
	}
	return setup.Package, true
}

// sharedLibrary is a module's precompiled shared library, when the base build has one. Lake names it by Lean's
// module initialization stem, `<package>_<Module>` mangled, and keeps it in the library directory of the package
// that builds it, so every library directory on the search path is looked in, the root's first.
func (s *lakeState) sharedLibrary(module, pkg string) (string, bool) {
	name := initializationStem(module, pkg) + ".so"
	for _, lib := range append([]string{s.c.icfg.Lib}, s.c.leanPath...) {
		for _, path := range []string{filepath.Join(lib, name), filepath.Join(filepath.Dir(lib), name)} {
			if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
				return path, true
			}
		}
	}
	return "", false
}

// optionValue writes a setup option's JSON value as `-D` takes it: a Boolean or a number as written, a string
// unquoted.
func optionValue(raw json.RawMessage) (string, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) > 0 && raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", err
		}
		return s, nil
	}
	text := string(raw)
	if text == "true" || text == "false" {
		return text, nil
	}
	if _, err := strconv.ParseFloat(text, 64); err == nil {
		return text, nil
	}
	return "", fmt.Errorf("value %s is not a Boolean, a number or a string", text)
}

// initializationStem is Lean's mkModuleInitializationStem (Lean/Compiler/NameMangling.lean): the mangled package
// name, `_`, and the mangled module name, one name component at a time, a «»-quoted component (which may hold a
// dot) as one.
func initializationStem(module, pkg string) string {
	var b strings.Builder
	if pkg != "" {
		b.WriteString(mangleString(pkg))
		b.WriteByte('_')
	}
	prev := ""
	for i, part := range nameComponents(module) {
		m := mangleString(part)
		switch {
		case i == 0 && needsDisambiguation(m):
			b.WriteString("00")
		case i > 0 && (strings.HasSuffix(prev, "_") || needsDisambiguation(m)):
			b.WriteString("_00")
		case i > 0:
			b.WriteByte('_')
		}
		b.WriteString(m)
		prev = part
	}
	return b.String()
}

// nameComponents splits a Lean name into its components: at each dot outside «», with the «» taken off.
func nameComponents(name string) []string {
	var out []string
	var b strings.Builder
	quoted := false
	for _, r := range name {
		switch {
		case r == '«' && !quoted:
			quoted = true
		case r == '»' && quoted:
			quoted = false
		case r == '.' && !quoted:
			out = append(out, b.String())
			b.Reset()
		default:
			b.WriteRune(r)
		}
	}
	return append(out, b.String())
}

// mangleString is the escaping Lean applies to one name component when it builds a symbol.
func mangleString(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '_':
			b.WriteString("__")
		case r < 0x100:
			fmt.Fprintf(&b, "_x%02x", r)
		case r < 0x10000:
			fmt.Fprintf(&b, "_u%04x", r)
		default:
			fmt.Fprintf(&b, "_U%08x", r)
		}
	}
	return b.String()
}

// needsDisambiguation is Lean's checkDisambiguation: whether a mangled component could be read as an escape.
func needsDisambiguation(m string) bool {
	for i := range len(m) {
		switch c := m[i]; {
		case c == '_':
			continue
		case c == 'x':
			return lowerHex(m[i+1:], 2)
		case c == 'u':
			return lowerHex(m[i+1:], 4)
		case c == 'U':
			return lowerHex(m[i+1:], 8)
		default:
			return c >= '0' && c <= '9'
		}
	}
	return true
}

func lowerHex(s string, n int) bool {
	if len(s) < n {
		return false
	}
	for _, c := range []byte(s[:n]) {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
