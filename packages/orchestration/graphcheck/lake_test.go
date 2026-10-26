package graphcheck

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/sjawhar/legion/orchestration/declindex"
)

// lakeTree is a synthetic base build: a library directory and Lake's setup directory beside it, and the builds of
// the dependency packages it requires, each laid out the same way.
type lakeTree struct {
	t       *testing.T
	root    string
	lib, ir string
	deps    []*lakeTree
}

func newLakeTree(t *testing.T) *lakeTree {
	t.Helper()
	return makeLakeTree(t, t.TempDir())
}

func makeLakeTree(t *testing.T, root string) *lakeTree {
	t.Helper()
	tree := &lakeTree{t: t, root: root, lib: filepath.Join(root, ".lake", "build", "lib", "lean"), ir: filepath.Join(root, ".lake", "build", "ir")}
	for _, dir := range []string{tree.lib, tree.ir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return tree
}

// dependency adds the build of a required package, which Lake puts on the search path after the root's.
func (l *lakeTree) dependency(name string) *lakeTree {
	l.t.Helper()
	dep := makeLakeTree(l.t, filepath.Join(l.root, ".lake", "packages", name))
	l.deps = append(l.deps, dep)
	return dep
}

// olean writes a module's object file, which says which package builds the module.
func (l *lakeTree) olean(module string) {
	l.t.Helper()
	l.write(filepath.Join(l.lib, strings.ReplaceAll(module, ".", "/")+".olean"), []byte("olean"))
}

// setup writes a module's setup file as Lake writes it, importArts included.
func (l *lakeTree) setup(module string, options map[string]any, plugins ...string) {
	l.t.Helper()
	l.setupWith(module, options, plugins, nil)
}

// setupWith writes a module's setup file with its dynamic libraries as well as its plugins.
func (l *lakeTree) setupWith(module string, options map[string]any, plugins, dynlibs []string) {
	l.t.Helper()
	type path struct {
		Path string `json:"path"`
	}
	ps, ds := []path{}, []path{}
	for _, p := range plugins {
		ps = append(ps, path{p})
	}
	for _, d := range dynlibs {
		ds = append(ds, path{d})
	}
	raw, err := json.Marshal(map[string]any{"name": module, "package": "lib", "isModule": false, "options": options,
		"plugins": ps, "dynlibs": ds, "importArts": map[string]any{"Init": map[string]any{"olean": "/x/Init.olean"}}})
	if err != nil {
		l.t.Fatal(err)
	}
	l.write(filepath.Join(l.ir, strings.ReplaceAll(module, ".", "/")+".setup.json"), raw)
}

// lakefile writes the package's configuration, which gives the libraries their extra arguments.
func (l *lakeTree) lakefile(text string) {
	l.t.Helper()
	l.write(filepath.Join(l.root, "lakefile.toml"), []byte(text))
}

// precompiled writes a module's shared library and gives its path.
func (l *lakeTree) precompiled(module string) string {
	l.t.Helper()
	path := filepath.Join(l.lib, "lib_"+strings.ReplaceAll(module, ".", "_")+".so")
	l.write(path, []byte("ELF"))
	return path
}

func (l *lakeTree) write(path string, raw []byte) {
	l.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		l.t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		l.t.Fatal(err)
	}
}

// candidate is a candidate over the tree whose changed modules are changed, each with its imports.
func (l *lakeTree) candidate(changed map[string][]string) *Candidate {
	c := &Candidate{icfg: declindex.Config{Tree: l.root, Lib: l.lib, Roots: []string{"Lib", "Fresh"}},
		modules: map[string]*Module{}, leanPath: []string{l.lib}}
	for _, dep := range l.deps {
		c.leanPath = append(c.leanPath, dep.lib)
	}
	for name, imports := range changed {
		c.modules[name] = &Module{Change: Change{Module: name, Status: Modified}, Imports: imports}
	}
	return c
}

// A module's compile carries the dynamic libraries its setup lists, after its plugins, as Lake passes them, and
// the extra arguments the package and its library give it, the package's first.
func TestLeanArgsPassesDynamicLibrariesAndTheConfiguredArguments(t *testing.T) {
	l := newLakeTree(t)
	l.lakefile("moreLeanArgs = [\"-DwarningAsError=true\"]\n\n[[lean_lib]]\nname = \"Lib\"\nmoreLeanArgs = [\"-DmaxRecDepth=8192\"]\nweakLeanArgs = [\"--quiet\"]\n")
	l.setupWith("Lib.A", map[string]any{"pp.width": 100}, []string{"/tc/libLake_shared.so"}, []string{"/dyn/libOne.so", "/dyn/libTwo.so", "/dyn/libOne.so"})
	c := l.candidate(map[string][]string{"Lib.A": nil})
	args, err := c.leanArgs(c.modules["Lib.A"])
	want := []string{"-Dpp.width=100", "--plugin=/tc/libLake_shared.so", "--load-dynlib=/dyn/libOne.so",
		"--load-dynlib=/dyn/libTwo.so", "-DwarningAsError=true", "-DmaxRecDepth=8192", "--quiet"}
	if err != nil || !slices.Equal(args, want) {
		t.Fatalf("leanArgs = %q, %v; want %q", args, err, want)
	}
}

// A module of a library the configuration does not declare takes no extra arguments, and a configuration only
// Lake can read makes the check unable rather than compile without the arguments it would pass.
func TestLeanArgsAndTheConfigurationItCannotRead(t *testing.T) {
	for name, test := range map[string]struct {
		lakefile string
		want     []string
		unable   bool
	}{
		"no configuration at all":        {"", nil, false},
		"a library that is not this one": {"[[lean_lib]]\nname = \"Other\"\nmoreLeanArgs = [\"-Dx=1\"]\n", nil, false},
		"a configuration it cannot read": {"[[lean_lib]]\nname\n", nil, true},
	} {
		t.Run(name, func(t *testing.T) {
			l := newLakeTree(t)
			if test.lakefile != "" {
				l.lakefile(test.lakefile)
			}
			l.setup("Lib.A", nil)
			c := l.candidate(map[string][]string{"Lib.A": nil})
			args, err := c.leanArgs(c.modules["Lib.A"])
			var unable *UnableError
			if errors.As(err, &unable) != test.unable {
				t.Fatalf("leanArgs error = %v, want unable = %v", err, test.unable)
			}
			if !test.unable && !slices.Equal(args, test.want) {
				t.Fatalf("leanArgs = %q, want %q", args, test.want)
			}
		})
	}
}

// A library the build precompiles is linked into every module that imports it under one name,
// `lib<package>_<Library>.so`, beside the per-module `<package>_<Module>.so`. A changed module of that library
// makes both stale, so a compile that would load either is unable rather than reading the old code.
func TestLeanArgsIsUnableForAChangedModuleOfAPrecompiledLibrary(t *testing.T) {
	for name, plugin := range map[string]string{
		"the library's own shared library": "/p/.lake/build/lib/liblib_Meta.so",
		"the module's shared library":      "/p/.lake/build/lib/lean/lib_Meta_Fast.so",
	} {
		t.Run(name, func(t *testing.T) {
			l := newLakeTree(t)
			l.lakefile("[[lean_lib]]\nname = \"Meta\"\nprecompileModules = true\n\n[[lean_lib]]\nname = \"Lib\"\n")
			l.setupWith("Meta.Fast", nil, []string{"/tc/libLake_shared.so"}, nil)
			l.setupWith("Lib.A", nil, []string{"/tc/libLake_shared.so", plugin}, nil)
			c := l.candidate(map[string][]string{"Meta.Fast": nil, "Lib.A": {"Meta.Fast"}})
			c.icfg.Roots = []string{"Lib", "Meta"}
			for i := range c.modules {
				c.modules[i].Change.Path = strings.ReplaceAll(i, ".", "/") + ".lean"
			}
			var unable *UnableError
			if _, err := c.leanArgs(c.modules["Lib.A"]); !errors.As(err, &unable) || !strings.Contains(unable.Reason, "Meta.Fast") {
				t.Fatalf("leanArgs error = %v, want *UnableError naming Meta.Fast", err)
			}
		})
	}
}

func sorted(args ...string) []string {
	sort.Strings(args)
	return slices.Compact(args)
}

func TestLeanArgsFormatsOptionsAndKeepsThePluginOrder(t *testing.T) {
	l := newLakeTree(t)
	b := l.precompiled("Lib.B")
	l.setup("Lib.A", map[string]any{"maxRecDepth": 8192, "linter.unusedVariables": false, "pp.width": 1.5, "trace.x": "a b"},
		"/tc/libLake_shared.so", "/tc/libLake_shared.so", "/dep/libDep.so", b)
	l.setup("Lib.B", map[string]any{"maxRecDepth": 8192}, "/tc/libLake_shared.so", "/dep/libDep.so")
	c := l.candidate(map[string][]string{"Lib.A": {"Lib.B"}})
	args, err := c.leanArgs(c.modules["Lib.A"])
	if err != nil {
		t.Fatal(err)
	}
	want := append(sorted("-DmaxRecDepth=8192", "-Dlinter.unusedVariables=false", "-Dpp.width=1.5", "-Dtrace.x=a b"),
		"--plugin=/tc/libLake_shared.so", "--plugin=/dep/libDep.so", "--plugin="+b)
	if !slices.Equal(args, want) {
		t.Fatalf("leanArgs = %q, want %q", args, want)
	}
}

// An added module takes its own library's options, as Lake gives every module of a library the same ones, whatever
// it imports.
func TestLeanArgsGivesAnAddedModuleItsLibrarysOptions(t *testing.T) {
	l := newLakeTree(t)
	l.setup("Aux.X", map[string]any{"other": 1})
	l.setup("Lib.B", map[string]any{"maxRecDepth": 8192})
	for _, imports := range [][]string{{"Aux.X", "Lib.B"}, {"Aux.X"}} {
		c := l.candidate(map[string][]string{"Lib.New": imports})
		c.modules["Lib.New"].Status = Added
		args, err := c.leanArgs(c.modules["Lib.New"])
		if err != nil || !slices.Equal(args, []string{"-DmaxRecDepth=8192"}) {
			t.Fatalf("imports %v: leanArgs = %q, %v, want Lib's options", imports, args, err)
		}
	}
}

// A library with no module in the base build gives no options to read: the check cannot match Lake and says so.
func TestLeanArgsIsUnableForAModuleOfANewLibrary(t *testing.T) {
	l := newLakeTree(t)
	l.setup("Lib.B", map[string]any{"maxRecDepth": 8192})
	c := l.candidate(map[string][]string{"Fresh.New": {"Lib.B"}})
	c.modules["Fresh.New"].Status = Added
	var u *UnableError
	if _, err := c.leanArgs(c.modules["Fresh.New"]); !errors.As(err, &u) || !strings.Contains(u.Reason, "Fresh") {
		t.Fatalf("leanArgs error %v, want *UnableError naming the library Fresh", err)
	}
}

// An added module that imports only a dependency package's modules gets the plugins Lake gives them: those modules'
// own setups live in that package's build, not in the root's.
func TestLeanArgsGivesAnAddedModuleThePluginsOfADependencyImport(t *testing.T) {
	l := newLakeTree(t)
	l.setup("Lib.B", map[string]any{"maxRecDepth": 8192})
	dep := l.dependency("dep")
	dep.olean("Dep.X")
	dep.setup("Dep.X", nil, "/tc/libLake_shared.so", "/dep/libdep_Dep.so")
	c := l.candidate(map[string][]string{"Lib.New": {"Dep.X"}})
	c.modules["Lib.New"].Status = Added
	args, err := c.leanArgs(c.modules["Lib.New"])
	want := []string{"-DmaxRecDepth=8192", "--plugin=/tc/libLake_shared.so", "--plugin=/dep/libdep_Dep.so"}
	if err != nil || !slices.Equal(args, want) {
		t.Fatalf("leanArgs = %q, %v, want %q", args, err, want)
	}
}

// Several imports' plugin lists merge into one load order that keeps each list's own order.
func TestLeanArgsMergesThePluginOrdersOfSeveralImports(t *testing.T) {
	l := newLakeTree(t)
	l.setup("Lib.B", map[string]any{}, "/a.so", "/b.so", "/d.so")
	l.setup("Lib.C", map[string]any{}, "/a.so", "/c.so", "/d.so")
	c := l.candidate(map[string][]string{"Lib.New": {"Lib.B", "Lib.C"}})
	c.modules["Lib.New"].Status = Added
	args, err := c.leanArgs(c.modules["Lib.New"])
	want := []string{"--plugin=/a.so", "--plugin=/b.so", "--plugin=/c.so", "--plugin=/d.so"}
	if err != nil || !slices.Equal(args, want) {
		t.Fatalf("leanArgs = %q, %v, want %q", args, err, want)
	}
}

// Two imports whose plugin lists order two libraries differently leave no load order Lake would use.
func TestLeanArgsIsUnableWhenImportsDisagreeOnPluginOrder(t *testing.T) {
	l := newLakeTree(t)
	l.setup("Lib.B", map[string]any{}, "/a.so", "/b.so")
	l.setup("Lib.C", map[string]any{}, "/b.so", "/a.so")
	c := l.candidate(map[string][]string{"Lib.New": {"Lib.B", "Lib.C"}})
	c.modules["Lib.New"].Status = Added
	var u *UnableError
	if _, err := c.leanArgs(c.modules["Lib.New"]); !errors.As(err, &u) {
		t.Fatalf("leanArgs error %v, want *UnableError", err)
	}
}

// An added module that imports a changed precompiled module is in the same position as a modified one: the shared
// library Lake would load is the stale base one, so the check is unable rather than compiling without it.
func TestLeanArgsIsUnableForAnAddedModuleImportingAChangedPrecompiledModule(t *testing.T) {
	l := newLakeTree(t)
	l.setup("Lib.Pre", nil, "/tc/libLake_shared.so")
	l.precompiled("Lib.Pre")
	c := l.candidate(map[string][]string{"Lib.New": {"Lib.Pre"}, "Lib.Pre": nil})
	c.modules["Lib.New"].Status = Added
	var u *UnableError
	if _, err := c.leanArgs(c.modules["Lib.New"]); !errors.As(err, &u) || !strings.Contains(u.Reason, "Lib.Pre") {
		t.Fatalf("leanArgs = %v, want *UnableError naming Lib.Pre", err)
	}
}

// A precompiled module of a dependency package has its shared library in that package's build, not the root's.
func TestLeanArgsFindsADependencysSharedLibrary(t *testing.T) {
	l := newLakeTree(t)
	l.setup("Lib.B", map[string]any{})
	dep := l.dependency("dep")
	dep.setup("Dep.X", nil, "/tc/libLake_shared.so")
	lib := dep.precompiled("Dep.X")
	c := l.candidate(map[string][]string{"Lib.New": {"Dep.X"}})
	c.modules["Lib.New"].Status = Added
	args, err := c.leanArgs(c.modules["Lib.New"])
	want := []string{"--plugin=/tc/libLake_shared.so", "--plugin=" + lib}
	if err != nil || !slices.Equal(args, want) {
		t.Fatalf("leanArgs = %q, %v, want %q", args, err, want)
	}
}

// A module the base build never built says nothing about what Lake loads for it: the check is unable rather than
// compiling its importer without the plugins.
func TestLeanArgsIsUnableForAnImportTheBaseBuildLacks(t *testing.T) {
	l := newLakeTree(t)
	l.setup("Lib.B", map[string]any{})
	c := l.candidate(map[string][]string{"Lib.New": {"Lib.Unknown"}})
	c.modules["Lib.New"].Status = Added
	var u *UnableError
	if _, err := c.leanArgs(c.modules["Lib.New"]); !errors.As(err, &u) || !strings.Contains(u.Reason, "Lib.Unknown") {
		t.Fatalf("leanArgs = %v, want *UnableError naming Lib.Unknown", err)
	}
}

// A name component written in «» may hold a dot; Lean mangles it as one component, and so does the library name.
func TestInitializationStemOfAQuotedComponent(t *testing.T) {
	if got, want := initializationStem("Lib.«a.b»", "lib"), "lib_Lib_a_x2eb"; got != want {
		t.Fatalf("initializationStem = %q, want %q", got, want)
	}
	if got := nameComponents("Lib.«a.b».C"); !slices.Equal(got, []string{"Lib", "a.b", "C"}) {
		t.Fatalf("nameComponents = %q", got)
	}
}

func TestLeanArgsTakesThePluginsOfAChangedImport(t *testing.T) {
	l := newLakeTree(t)
	l.setup("Lib.Base", nil, "/tc/libLake_shared.so")
	base := l.precompiled("Lib.Base")
	l.setup("Lib.Mid", nil, "/tc/libLake_shared.so", "/dep/libDep.so", base)
	c := l.candidate(map[string][]string{"Lib.Top": {"Lib.Mid"}, "Lib.Mid": {"Lib.Base"}})
	c.modules["Lib.Top"].Status = Added
	args, err := c.leanArgs(c.modules["Lib.Top"])
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--plugin=/tc/libLake_shared.so", "--plugin=/dep/libDep.so", "--plugin=" + base}
	if !slices.Equal(args, want) {
		t.Fatalf("leanArgs = %q, want %q", args, want)
	}
}

func TestLeanArgsIsUnableForAChangedPrecompiledImport(t *testing.T) {
	l := newLakeTree(t)
	l.setup("Lib.Pre", nil, "/tc/libLake_shared.so")
	pre := l.precompiled("Lib.Pre")
	l.setup("Lib.User", nil, "/tc/libLake_shared.so", pre)
	c := l.candidate(map[string][]string{"Lib.User": {"Lib.Pre"}, "Lib.Pre": nil})
	_, err := c.leanArgs(c.modules["Lib.User"])
	var u *UnableError
	if !errors.As(err, &u) || !strings.Contains(u.Reason, "Lib.Pre") || !strings.Contains(u.Reason, "shared library") {
		t.Fatalf("leanArgs = %v, want *UnableError naming Lib.Pre's shared library", err)
	}
	// The changed precompiled module compiles on its own: nothing it loads is stale.
	if _, err := c.leanArgs(c.modules["Lib.Pre"]); err != nil {
		t.Fatalf("leanArgs(Lib.Pre) = %v", err)
	}
}

func TestLeanArgsIsUnableForAStaleLibraryAnUnchangedImportLoads(t *testing.T) {
	l := newLakeTree(t)
	l.setup("Lib.Pre", nil)
	pre := l.precompiled("Lib.Pre")
	l.setup("Lib.Mid", nil, pre)
	c := l.candidate(map[string][]string{"Lib.User": {"Lib.Mid"}, "Lib.Pre": nil})
	var u *UnableError
	if _, err := c.leanArgs(c.modules["Lib.User"]); !errors.As(err, &u) || !strings.Contains(u.Reason, "Lib.Pre") {
		t.Fatalf("leanArgs = %v, want *UnableError naming Lib.Pre", err)
	}
}

func TestLeanArgsWithoutSetupDirectoryGivesNothing(t *testing.T) {
	root := t.TempDir()
	c := &Candidate{icfg: declindex.Config{Lib: filepath.Join(root, ".lake", "build", "lib", "lean")},
		modules: map[string]*Module{"Lib.A": {Change: Change{Module: "Lib.A"}}}}
	if args, err := c.leanArgs(c.modules["Lib.A"]); args != nil || err != nil {
		t.Fatalf("leanArgs = %q, %v, want nothing", args, err)
	}
}

func TestInitializationStemIsLeans(t *testing.T) {
	for _, c := range []struct{ module, pkg, want string }{
		{"Acme.Meta.StepOpts", "acme", "acme_Acme_Meta_StepOpts"},
		{"Lib.Core", "lib", "lib_Lib_Core"},
		{"Lib.x12", "lib", "lib_Lib_00x12"},
		{"Lib.A_.B", "my_pkg", "my__pkg_Lib_A___00B"},
		{"Lib.2nd", "lib", "lib_Lib_002nd"},
		{"x1f.A", "", "00x1f_A"},
	} {
		if got := initializationStem(c.module, c.pkg); got != c.want {
			t.Errorf("initializationStem(%s, %s) = %s, want %s", c.module, c.pkg, got, c.want)
		}
	}
}
