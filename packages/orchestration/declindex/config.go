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
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is one project's index configuration, a YAML file that the project keeps beside its proofs. `tree` and
// `db` are relative to the file's directory; `lib`, `registry` and `owners` are relative to the tree.
type Config struct {
	Tree string `yaml:"tree"`
	// LibRel is the configured library path below Tree; Lib is the resolved path.
	LibRel    string   `yaml:"-"`
	Lib       string   `yaml:"lib"`
	DB        string   `yaml:"db"`
	Roots     []string `yaml:"roots"`
	FileKinds []string `yaml:"file_kinds"`
	Jobs      int      `yaml:"jobs"`
	// Registry is the concept registry directory (see package conceptgate), relative to Tree.
	Registry string `yaml:"registry"`
	// Build runs in Tree after RefreshAt has moved it to LAND_COMMIT.
	Build []string `yaml:"build"`
	// Remote is the sole remote RefreshAt fetches LAND_COMMIT from when Tree lacks it.
	Remote string `yaml:"remote"`
	// BuildTimeout bounds Build; LoadConfig defaults it to six hours.
	BuildTimeout time.Duration `yaml:"build_timeout"`
	// Lean configures the exporter of compiled modules. With Lean.Export off, the index reads only the reference
	// files (uses, positions, statement hashes).
	Lean LeanConfig `yaml:"lean"`
	// Contracts selects the declarations that the ledger tracks, and Owners maps them to their owners.
	Contracts ContractRule `yaml:"contracts"`
	Owners    string       `yaml:"owners"`
	// OwnersFormat is the "format" value the owner map must carry (its producer's format name); empty accepts any.
	OwnersFormat string `yaml:"owners_format"`
	// Repo is the repository's name in the ledger.
	Repo string `yaml:"repo"`
	// Librarian is the command that receives each near match with --id, --proposer and --proposal arguments.
	Librarian []string `yaml:"librarian"`
}

// configYAML retains YAML's strict field check while Config keeps resolved paths and a parsed duration.
type configYAML struct {
	Tree         string       `yaml:"tree"`
	Lib          string       `yaml:"lib"`
	DB           string       `yaml:"db"`
	Roots        []string     `yaml:"roots"`
	FileKinds    []string     `yaml:"file_kinds"`
	Jobs         int          `yaml:"jobs"`
	Registry     string       `yaml:"registry"`
	Build        []string     `yaml:"build"`
	Remote       string       `yaml:"remote"`
	BuildTimeout string       `yaml:"build_timeout"`
	Lean         LeanConfig   `yaml:"lean"`
	Contracts    ContractRule `yaml:"contracts"`
	Owners       string       `yaml:"owners"`
	OwnersFormat string       `yaml:"owners_format"`
	Repo         string       `yaml:"repo"`
	Librarian    []string     `yaml:"librarian"`
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
	var y configYAML
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&y); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	if y.Tree == "" {
		return Config{}, fmt.Errorf("%s: tree is required", path)
	}
	if len(y.Roots) == 0 {
		return Config{}, fmt.Errorf("%s: roots are required: the first components of the project's module names", path)
	}
	if y.Registry != "" {
		clean := filepath.Clean(y.Registry)
		if filepath.IsAbs(y.Registry) || clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return Config{}, fmt.Errorf("%s: registry must be a non-empty relative path below the tree", path)
		}
	}
	timeout := 6 * time.Hour
	if y.BuildTimeout != "" {
		timeout, err = time.ParseDuration(y.BuildTimeout)
		if err != nil || timeout <= 0 {
			if err == nil {
				err = errors.New("must be greater than zero")
			}
			return Config{}, fmt.Errorf("%s: build_timeout %q: %w", path, y.BuildTimeout, err)
		}
	}
	// Every path is absolute, so that a relative --config gives paths that filepath.Rel can compare.
	dir, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return Config{}, err
	}
	rel := func(base, p string) string {
		if p == "" || filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(base, p)
	}
	libRel := y.Lib
	if libRel == "" {
		libRel = filepath.Join(".lake", "build", "lib", "lean")
	}
	c := Config{
		Tree:         rel(dir, y.Tree),
		LibRel:       libRel,
		DB:           rel(dir, y.DB),
		Roots:        y.Roots,
		FileKinds:    y.FileKinds,
		Jobs:         y.Jobs,
		Registry:     y.Registry,
		Build:        y.Build,
		Remote:       y.Remote,
		BuildTimeout: timeout,
		Lean:         y.Lean,
		Contracts:    y.Contracts,
		Owners:       y.Owners,
		OwnersFormat: y.OwnersFormat,
		Repo:         y.Repo,
		Librarian:    y.Librarian,
	}
	c.Lib = rel(c.Tree, c.LibRel)
	c.Registry = rel(c.Tree, c.Registry)
	c.Owners = rel(c.Tree, c.Owners)
	for i, p := range c.Lean.SearchPath {
		c.Lean.SearchPath[i] = rel(c.Tree, p)
	}
	return c, nil
}

// GateRegistryDir resolves the configured registry through symlinks and requires it to remain a subdirectory of
// the tree. The gate reads only this resolved directory.
func (c Config) GateRegistryDir() (string, error) {
	if c.Registry == "" {
		return "", errors.New("the configuration names no registry")
	}
	return pathInside(c.Tree, c.Registry)
}

// GateStatePath resolves the configured index path through symlinks and requires it to remain outside the tree.
// A gate opens this existing file read-only, so neither the index nor its scratch work can dirty the tree.
func (c Config) GateStatePath() (string, error) {
	tree, err := canonicalPath(c.Tree)
	if err != nil {
		return "", err
	}
	state, err := canonicalStatePath(c.DB)
	if err != nil {
		return "", err
	}
	if isInside(tree, state) {
		return "", fmt.Errorf("the gate index state %s must be outside the tree %s", state, tree)
	}
	return state, nil
}

func pathInside(tree, path string) (string, error) {
	root, err := canonicalPath(tree)
	if err != nil {
		return "", err
	}
	child, err := canonicalPath(path)
	if err != nil {
		return "", err
	}
	if !isInside(root, child) {
		return "", fmt.Errorf("registry %s must resolve inside the tree %s", child, root)
	}
	return child, nil
}

func canonicalPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(absolute)
}

func canonicalStatePath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	current := absolute
	var missing []string
	for {
		if _, err := os.Lstat(current); err == nil {
			resolved, err := filepath.EvalSymlinks(current)
			if err != nil {
				return "", err
			}
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return resolved, nil
		} else if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("no existing parent for index state %s", absolute)
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func isInside(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
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
	for _, pattern := range []string{"*", "*/*"} {
		deps, _ := filepath.Glob(filepath.Join(c.Tree, ".lake", "packages", pattern, ".lake", "build", "lib", "lean"))
		search = append(search, deps...)
	}
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
