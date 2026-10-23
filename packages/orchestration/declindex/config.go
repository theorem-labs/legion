package declindex

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"gopkg.in/yaml.v3"
)

// Config is one project's index configuration, a YAML file that the project keeps beside its proofs. `tree` and
// `db` are relative to the file's directory; `lib`, `registry` and `owners` are relative to the tree.
type Config struct {
	Tree      string   `yaml:"tree"`
	Lib       string   `yaml:"lib"`
	DB        string   `yaml:"db"`
	Roots     []string `yaml:"roots"`
	FileKinds []string `yaml:"file_kinds"`
	Jobs      int      `yaml:"jobs"`
	// Registry is the concept registry file (see package conceptgate).
	Registry string `yaml:"registry"`
	// Lean configures the exporter of compiled modules. With Lean.Export off, the index reads only the reference
	// files (uses, positions, statement hashes).
	Lean LeanConfig `yaml:"lean"`
	// Contracts selects the declarations that the ledger tracks, and Owners maps them to their owners.
	Contracts ContractRule `yaml:"contracts"`
	Owners    string       `yaml:"owners"`
	// Repo is the repository's name in the ledger.
	Repo string `yaml:"repo"`
}

// LeanConfig says how to build and run the exporter.
type LeanConfig struct {
	// Export turns on the compiled part: types, compiled statement hashes, atoms and the registry's facts.
	Export bool `yaml:"export"`
	// Toolchain is the prefix of the tree's Lean toolchain (`lean --print-prefix`); empty asks elan in the tree.
	Toolchain string `yaml:"toolchain"`
	// CC is the C compiler that builds the exporter; empty is LEAN_CC, else the toolchain's clang, else cc.
	CC string `yaml:"cc"`
	// SearchPath lists further library directories (dependencies' builds), relative to the tree.
	SearchPath []string `yaml:"search_path"`
	// Jobs bounds the exporter processes; zero is the index's jobs.
	Jobs int `yaml:"jobs"`
	// ModulesPerProcess bounds the modules one exporter process reads; zero is 400.
	ModulesPerProcess int `yaml:"modules_per_process"`
}

// ContractRule selects contracts: declarations in files that match one of Paths (globs with `**`), of one of
// Kinds, whose last name component matches Name (a regular expression).
type ContractRule struct {
	Paths []string `yaml:"paths"`
	Kinds []string `yaml:"kinds"`
	Name  string   `yaml:"name"`
}

// LoadConfig reads and checks a configuration file.
func LoadConfig(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	if c.Tree == "" {
		return Config{}, fmt.Errorf("%s: tree is required", path)
	}
	if len(c.Roots) == 0 {
		return Config{}, fmt.Errorf("%s: roots are required: the first components of the project's module names", path)
	}
	dir := filepath.Dir(path)
	rel := func(base, p string) string {
		if p == "" || filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(base, p)
	}
	c.Tree = rel(dir, c.Tree)
	if c.Lib == "" {
		c.Lib = filepath.Join(".lake", "build", "lib", "lean")
	}
	c.Lib = rel(c.Tree, c.Lib)
	c.DB = rel(dir, c.DB)
	c.Registry = rel(c.Tree, c.Registry)
	c.Owners = rel(c.Tree, c.Owners)
	for i, p := range c.Lean.SearchPath {
		c.Lean.SearchPath[i] = rel(c.Tree, p)
	}
	return c, nil
}

// ErrNoDB is returned when no index path is configured.
var ErrNoDB = errors.New("no index file: set db in the configuration or pass --db")

// TreeSpec is the configuration's Tree.
func (c Config) TreeSpec() Tree {
	roots := Roots{}
	for _, r := range c.Roots {
		roots[r] = true
	}
	return Tree{Source: c.Tree, Lib: c.Lib, Roots: roots, FileKinds: c.FileKinds}
}

// Compiled gives the options of the compiled part for this configuration: the tree's toolchain, the exporter
// built with it below the index's directory, and the library directories of the tree and its Lake packages.
func (c Config) Compiled(ctx context.Context, forms FormsModule, log io.Writer) (*CompiledOptions, error) {
	l, err := FindLean(ctx, c.Tree, c.Lean.Toolchain)
	if err != nil {
		return nil, err
	}
	state := filepath.Dir(c.DB)
	bin, err := BuildExporter(ctx, l, c.Lean.CC, filepath.Join(state, "exporter"))
	if err != nil {
		return nil, err
	}
	search := append([]string{c.Lib}, c.Lean.SearchPath...)
	deps, _ := filepath.Glob(filepath.Join(c.Tree, ".lake", "packages", "*", ".lake", "build", "lib", "lean"))
	search = append(search, deps...)
	jobs := c.Lean.Jobs
	if jobs == 0 {
		jobs = c.Jobs
	}
	if jobs == 0 {
		jobs = min(48, runtime.NumCPU())
	}
	per := c.Lean.ModulesPerProcess
	if per == 0 {
		per = 400
	}
	return &CompiledOptions{
		Exporter: &Exporter{Lean: l, Bin: bin, SearchPath: search, Jobs: jobs, ModulesPerProcess: per, Log: log},
		Forms:    forms,
		Dir:      filepath.Join(state, "forms"),
	}, nil
}
