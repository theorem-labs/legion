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

// A name or a root written in guillemets names the module without them.
func TestParseLakefileReadsAQuotedName(t *testing.T) {
	libraries, ok := parseLakefile("[[lean_lib]]\nname = \"«Odd»\"\nroots = [\"«Odd».Inner\"]\n")
	if !ok || len(libraries) != 1 || libraries[0].Name != "Odd" || !slices.Equal(libraries[0].Roots, []string{"Odd.Inner"}) {
		t.Fatalf("parseLakefile = %+v, %v", libraries, ok)
	}
}

// A lakefile the reader cannot read is not guessed at.
func TestParseLakefileRefusesWhatItCannotRead(t *testing.T) {
	for name, text := range map[string]string{
		"a library with no name":   "[[lean_lib]]\nsrcDir = \"src\"\n",
		"an unterminated array":    "[[lean_lib]]\nname = \"Lib\"\nroots = [\"A\",\n",
		"a root that is no string": "[[lean_lib]]\nname = \"Lib\"\nroots = [A]\n",
		"a value that is no pair":  "[[lean_lib]]\nname\n",
		"an inline table":          "[[lean_lib]]\nname = \"Lib\"\nsrcDir = { path = \"src\" }\n",
	} {
		t.Run(name, func(t *testing.T) {
			if libraries, ok := parseLakefile(text); ok {
				t.Fatalf("parseLakefile = %+v, true; want false", libraries)
			}
		})
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

// Only Lake can read a configuration it alone evaluates, so the check is told it does not know; a tree with no
// configuration builds nothing, and every file is free.
func TestLibraryOfKnowsWhenItCannotRead(t *testing.T) {
	for name, test := range map[string]struct {
		file  string
		known bool
	}{
		"a configuration only Lake evaluates": {"lakefile.lean", false},
		"a configuration it cannot read":      {"lakefile.toml", false},
		"no configuration at all":             {"", true},
	} {
		t.Run(name, func(t *testing.T) {
			tree := t.TempDir()
			if test.file != "" {
				if err := os.WriteFile(filepath.Join(tree, test.file), []byte("[[lean_lib]]\nname\n"), 0o644); err != nil {
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
