package graphcheck

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/sjawhar/legion/orchestration/declindex"
)

// The reader takes each library's name, source directory, roots and globs out of a lakefile, joining the
// package's source directory with the library's and defaulting the roots to the library's name.
func TestParseLakefileReadsTheLibraries(t *testing.T) {
	libraries, ok := parseLakefile(`name = "proj"          # the package
srcDir = "code"
defaultTargets = ["Top"]

[[lean_lib]]
name = "Lib"

[[lean_lib]]
name = "Extra"
srcDir = "src"
precompileModules = true
roots = [
  "Extra",
  "Extra.Other",   # a second root
]
globs = ["Extra.+"]

[[lean_exe]]
name = "tool"
root = "Tool"
`)
	want := []lakeLibrary{
		{Name: "Lib", SrcDir: "code", Roots: []string{"Lib"}},
		{Name: "Extra", SrcDir: "code/src", Roots: []string{"Extra", "Extra.Other"}, Globs: []string{"Extra.+"}},
	}
	if !ok || !slices.EqualFunc(libraries, want, func(a, b lakeLibrary) bool {
		return a.Name == b.Name && a.SrcDir == b.SrcDir && slices.Equal(a.Roots, b.Roots) && slices.Equal(a.Globs, b.Globs)
	}) {
		t.Fatalf("parseLakefile = %+v, %v; want %+v", libraries, ok, want)
	}
}

// A name or a root written in guillemets names the module without them, and a literal string is a string.
func TestParseLakefileReadsAQuotedName(t *testing.T) {
	libraries, ok := parseLakefile("[[lean_lib]]\nname = \"«Odd»\"\nroots = [\"«Odd».Inner\"]\n\n[[lean_lib]]\nname = 'Lit'\nsrcDir = 'src#1'\n")
	want := []lakeLibrary{
		{Name: "Odd", Roots: []string{"Odd.Inner"}},
		{Name: "Lit", SrcDir: "src#1", Roots: []string{"Lit"}},
	}
	if !ok || !slices.EqualFunc(libraries, want, func(a, b lakeLibrary) bool {
		return a.Name == b.Name && a.SrcDir == b.SrcDir && slices.Equal(a.Roots, b.Roots)
	}) {
		t.Fatalf("parseLakefile = %+v, %v; want %+v", libraries, ok, want)
	}
}

// A lakefile the reader cannot read is not guessed at: it says so, and the check is then unable.
func TestParseLakefileRefusesWhatItCannotRead(t *testing.T) {
	for name, text := range map[string]string{
		"a library with no name":         "[[lean_lib]]\nsrcDir = \"src\"\n",
		"an unterminated array":          "[[lean_lib]]\nname = \"Lib\"\nroots = [\"A\",\n",
		"a root that is no string":       "[[lean_lib]]\nname = \"Lib\"\nroots = [A]\n",
		"a value that is no pair":        "[[lean_lib]]\nname\n",
		"an inline table":                "[[lean_lib]]\nname = \"Lib\"\nsrcDir = { path = \"src\" }\n",
		"an array of inline tables":      "lean_lib = [{ name = \"Lib\" }, { name = \"Extra\", srcDir = \"src\" }]\n",
		"a quoted key":                   "[[lean_lib]]\nname = \"Lib\"\n\"srcDir\" = \"src\"\n",
		"a dotted key":                   "lean_lib.name = \"Lib\"\n",
		"a multi-line basic string":      "[[lean_lib]]\nname = \"\"\"Lib\n\"\"\"\n",
		"a multi-line literal string":    "[[lean_lib]]\nname = '''Lib\n'''\n",
		"a quoted header":                "[[\"lean_lib\"]]\nname = \"Lib\"\n",
		"a quoted header after one":      "[[lean_lib]]\nname = \"Lib\"\n\n[[\"lean_lib\"]]\nname = \"Extra\"\nsrcDir = \"src\"\n",
		"a dotted library key on top":    "lean_lib.name = \"Lib\"\n\n[[lean_lib]]\nname = \"Extra\"\n",
		"a quoted package source":        "\"srcDir\" = \"src\"\n\n[[lean_lib]]\nname = \"Lib\"\n",
		"a single-bracket library":       "[lean_lib]\nname = \"Lib\"\n",
		"a single-bracket library after": "[[lean_lib]]\nname = \"Lib\"\n\n[lean_lib]\nname = \"Extra\"\n",
		"a library subtable":             "[[lean_lib]]\nname = \"Lib\"\n\n[lean_lib.extra]\nname = \"Extra\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			if libraries, ok := parseLakefile(text); ok {
				t.Fatalf("parseLakefile = %+v, true; want false", libraries)
			}
		})
	}
}

// A dotted key in an options table names an option, not a library: the reader passes over it and still gives
// the libraries. A project whose options it refused would be unable on every batch.
func TestParseLakefileReadsPastOptionTables(t *testing.T) {
	for name, options := range map[string]string{
		"a linter option":      "linter.unusedVariables = false",
		"a backward option":    "backward.isDefEq.respectTransparency.types = false",
		"a numeric option":     "maxRecDepth = 8192",
		"an option that is 0":  "maxHeartbeats = 0",
		"a quoted option name": "\"weak.option\" = true",
	} {
		t.Run(name, func(t *testing.T) {
			libraries, ok := parseLakefile("name = \"proj\"\n\n[leanOptions]\n" + options +
				"\n\n[[lean_lib]]\nname = \"Lib\"\n\n[[lean_lib]]\nname = \"Extra\"\nsrcDir = \"src\"\n")
			want := []lakeLibrary{{Name: "Lib", Roots: []string{"Lib"}}, {Name: "Extra", SrcDir: "src", Roots: []string{"Extra"}}}
			if !ok || !slices.EqualFunc(libraries, want, func(a, b lakeLibrary) bool {
				return a.Name == b.Name && a.SrcDir == b.SrcDir && slices.Equal(a.Roots, b.Roots)
			}) {
				t.Fatalf("parseLakefile = %+v, %v; want %+v", libraries, ok, want)
			}
		})
	}
}

// A dotted key of a table that is neither a library nor the package's own is a value the reader does not need.
func TestParseLakefileSkipsADottedKeyElsewhere(t *testing.T) {
	libraries, ok := parseLakefile("[[require]]\nname = \"dep\"\nsource.rev = \"v1\"\n\n[[lean_lib]]\nname = \"Lib\"\n")
	if !ok || len(libraries) != 1 || libraries[0].Name != "Lib" {
		t.Fatalf("parseLakefile = %+v, %v; want the library Lib", libraries, ok)
	}
}

// The shape of a real project's configuration — a package with its options, required packages, several
// libraries and an executable — reads as its libraries. This is the regression guard for a reader that refuses
// a file it should read: such a project would be unable on every batch.
func TestParseLakefileReadsAWholeProject(t *testing.T) {
	libraries, ok := parseLakefile(`name = "proj"
defaultTargets = ["Top"]
testDriver = "test"

[leanOptions]
maxRecDepth = 8192
maxHeartbeats = 64000000
linter.unusedVariables = false
linter.unusedSectionVars = false
linter.unusedTactic = false
backward.isDefEq.respectTransparency.types = false

[[require]]
name = "batteries"
scope = "example"
rev = "v4.36.0"

[[require]]
name = "iris"
path = "vendor/iris"

[[lean_lib]]
name = "Meta"
precompileModules = true

[[lean_lib]]
name = "Core"

[[lean_lib]]
name = "First"

[[lean_lib]]
name = "Second"

[[lean_lib]]
name = "Both"

[[lean_lib]]
name = "Top"

[[lean_exe]]
name = "tool"
root = "Tool"
`)
	want := []string{"Meta", "Core", "First", "Second", "Both", "Top"}
	var got []string
	for _, lib := range libraries {
		got = append(got, lib.Name)
	}
	if !ok || !slices.Equal(got, want) {
		t.Fatalf("parseLakefile = %+v, %v; want the libraries %v", libraries, ok, want)
	}
	for _, lib := range libraries {
		if lib.SrcDir != "" || !slices.Equal(lib.Roots, []string{lib.Name}) {
			t.Fatalf("library %+v, want no source directory of its own and its name as its root", lib)
		}
	}
}

// A library owns a module when a root is a prefix of its name, whatever the globs say, and when a glob matches
// it: `Extra` the root alone, `Extra.*` its submodules, `Extra.+` both.
func TestALibraryOwnsAModule(t *testing.T) {
	for name, test := range map[string]struct {
		library lakeLibrary
		owns    []string
		free    []string
	}{
		"a root and its submodules": {
			library: lakeLibrary{Name: "Extra", Roots: []string{"Extra"}},
			owns:    []string{"Extra", "Extra.Util", "Extra.A.B"},
			free:    []string{"Extras", "Other", "Other.Extra"},
		},
		"the glob of a root alone": {
			library: lakeLibrary{Name: "Extra", Globs: []string{"Other"}},
			owns:    []string{"Other"},
			free:    []string{"Other.Util"},
		},
		"the glob of the submodules": {
			library: lakeLibrary{Name: "Extra", Globs: []string{"Other.*"}},
			owns:    []string{"Other.Util", "Other.A.B"},
			free:    []string{"Other"},
		},
		"the glob of both": {
			library: lakeLibrary{Name: "Extra", Globs: []string{"Other.+"}},
			owns:    []string{"Other", "Other.Util"},
			free:    []string{"Others"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			for _, module := range test.owns {
				if !test.library.owns(module) {
					t.Errorf("owns(%q) = false, want true", module)
				}
			}
			for _, module := range test.free {
				if test.library.owns(module) {
					t.Errorf("owns(%q) = true, want false", module)
				}
			}
		})
	}
}

// libraryOf names the library that builds a file, and says nothing of a file no library builds. A library's own
// source directory holds its modules, and the base build must have an output for the module.
func TestLibraryOfReadsTheBuildsOwnLibraries(t *testing.T) {
	tree := t.TempDir()
	lib := filepath.Join(tree, ".lake", "build", "lib", "lean")
	for _, dir := range []string{filepath.Join(lib, "Lib"), filepath.Join(lib, "Extra")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(name, text string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(tree, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("lakefile.toml", "[[lean_lib]]\nname = \"Lib\"\n\n[[lean_lib]]\nname = \"Extra\"\nsrcDir = \"src\"\n")
	write(filepath.Join(".lake", "build", "lib", "lean", "Extra.olean"), "olean")
	c := &Candidate{icfg: declindex.Config{Tree: tree, Lib: lib}}
	for name, test := range map[string]struct{ file, library, module string }{
		"a module of a library with its own source directory": {"src/Extra/Util.lean", "Extra", "Extra.Util"},
		"the root module of such a library":                   {"src/Extra.lean", "Extra", "Extra"},
		"a script no library builds":                          {"tools/Probe.lean", "", ""},
		"an archived copy of a built path":                    {"archive/Extra/Util.lean", "", ""},
		"a file under the library's name but not its source":  {"Extra/Util.lean", "", ""},
	} {
		t.Run(name, func(t *testing.T) {
			library, module, known := c.libraryOf(test.file)
			if !known || library != test.library || module != test.module {
				t.Fatalf("libraryOf(%q) = %q, %q, %v; want %q, %q, true", test.file, library, module, known, test.library, test.module)
			}
		})
	}
}

// Only Lake can read a configuration it alone evaluates, so the check is told it does not know. Lake prefers a
// `lakefile.lean` wherever one sits beside a `lakefile.toml`, so that pair is unknown too. A tree with no
// configuration builds nothing, and every file is free.
func TestLibraryOfKnowsWhenItCannotRead(t *testing.T) {
	for name, test := range map[string]struct {
		files []string
		known bool
	}{
		"a configuration only Lake evaluates": {[]string{"lakefile.lean"}, false},
		"a readable configuration beside it":  {[]string{"lakefile.lean", "lakefile.toml"}, false},
		"a configuration it cannot read":      {[]string{"lakefile.toml"}, false},
		"no configuration at all":             {nil, true},
	} {
		t.Run(name, func(t *testing.T) {
			tree := t.TempDir()
			for _, file := range test.files {
				text := "[[lean_lib]]\nname\n"
				if file == "lakefile.toml" && len(test.files) > 1 {
					text = "[[lean_lib]]\nname = \"Lib\"\n"
				}
				if err := os.WriteFile(filepath.Join(tree, file), []byte(text), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			c := &Candidate{icfg: declindex.Config{Tree: tree, Lib: filepath.Join(tree, "lib")}}
			if _, _, known := c.libraryOf("Lib/Core.lean"); known != test.known {
				t.Fatalf("libraryOf known = %v, want %v", known, test.known)
			}
		})
	}
}
