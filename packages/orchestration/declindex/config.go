package declindex

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is one project's index configuration, a YAML file that the project keeps beside its proofs. `tree` and
// `state` are relative to the file's directory; `lib`, `registry` and `owners` are relative to the tree.
type Config struct {
	Tree string `yaml:"tree"`
	// LibRel is the configured library path below Tree; Lib is the resolved path.
	LibRel string `yaml:"-"`
	Lib    string `yaml:"lib"`
	// Index is the Postgres schema that holds this index (ValidIndexName). Every command that opens the index
	// needs it.
	Index string `yaml:"index"`
	// State is the local directory of the exporter build and the forms module, relative to the configuration
	// file's directory. It must resolve outside the tree; StateDir gives it, and its default.
	State string `yaml:"state"`
	// Reader is the read-only role that a refresh grants USAGE on the schema and SELECT on its tables; empty
	// grants nothing.
	Reader string `yaml:"reader"`
	// DSN is the index's connection string. No configuration file names a secret: it comes from DECLINDEX_DSN or
	// from the file that --index-dsn-file names.
	DSN       string   `yaml:"-"`
	Roots     []string `yaml:"roots"`
	FileKinds []string `yaml:"file_kinds"`
	Jobs      int      `yaml:"jobs"`
	// Registry is the concept registry directory (see package conceptgate), relative to Tree.
	Registry string `yaml:"registry"`
	// Build runs in Tree after a landed refresh (Sync) has moved it to LAND_COMMIT.
	Build []string `yaml:"build"`
	// Remote is the sole remote Sync fetches LAND_COMMIT from when Tree lacks it.
	Remote string `yaml:"remote"`
	// BuildTimeout bounds Build (a duration such as 90s); LoadConfig defaults it to six hours.
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

// defaultBuildTimeout bounds a build that the configuration does not bound.
const defaultBuildTimeout = 6 * time.Hour

// defaultJobs is the parallelism that the configuration does not set: one job for each CPU, at most 48.
func defaultJobs() int { return min(48, runtime.NumCPU()) }

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
	// db: named the SQLite file the index was. Its own message names what replaced it, because KnownFields would
	// report it as an unknown key and leave its owner to guess.
	var keys map[string]yaml.Node
	if err := yaml.Unmarshal(raw, &keys); err == nil {
		if _, ok := keys["db"]; ok {
			return Config{}, fmt.Errorf("%s: %w", path, ErrDBRemoved)
		}
	}
	// A build_timeout the file does not set keeps the default; one that it sets is checked below.
	c := Config{BuildTimeout: defaultBuildTimeout}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	// index: is checked where a command resolves the index it opens, not here: --index overrides the key, so a
	// name this file gets wrong must not refuse a right one on the command line (P-1).
	if c.Tree == "" {
		return Config{}, fmt.Errorf("%s: tree is required", path)
	}
	if len(c.Roots) == 0 {
		return Config{}, fmt.Errorf("%s: roots are required: the first components of the project's module names", path)
	}
	if c.Registry != "" {
		// IsLocal accepts "." (the tree itself), which is no registry directory below it.
		if !filepath.IsLocal(c.Registry) || filepath.Clean(c.Registry) == "." {
			return Config{}, fmt.Errorf("%s: registry must be a non-empty relative path below the tree", path)
		}
	}
	if c.BuildTimeout <= 0 {
		return Config{}, fmt.Errorf("%s: build_timeout %s: must be greater than zero", path, c.BuildTimeout)
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
	c.Tree = rel(dir, c.Tree)
	c.State = rel(dir, c.State)
	c.LibRel = cmp.Or(c.Lib, filepath.Join(".lake", "build", "lib", "lean"))
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

// Target is the index this configuration names: its schema, its connection string, its reader role and the file
// whose lock guards the indexed tree on this machine. The lock file is in the state directory, which is outside
// the tree; a configuration whose state cannot be resolved names no lock, and a command that writes the tree
// resolves the state anyway (StateDir).
func (c Config) Target() Target {
	t := Target{DSN: c.DSN, Index: c.Index, Reader: c.Reader}
	if state, err := c.StateDir(); err == nil {
		t.TreeLock = filepath.Join(state, "tree.lock")
	}
	return t
}

// StateDir resolves the local directory that holds this index's exporter build and forms module: the configured
// state, or the user's cache directory under the index's name. It must resolve outside the tree, so that neither
// a refresh nor the gate can dirty the tree with its scratch work.
func (c Config) StateDir() (string, error) {
	dir := c.State
	if dir == "" {
		if c.Index == "" {
			return "", errors.New("no index state: set state in the configuration, or index, whose name names the default below the cache directory")
		}
		cache, err := os.UserCacheDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(cache, "declindex", c.Index)
	}
	tree, err := canonicalPath(c.Tree)
	if err != nil {
		return "", err
	}
	state, err := canonicalStatePath(dir)
	if err != nil {
		return "", err
	}
	// The state holds the exporter's build and the forms module, which a gate writes: the tree itself is as
	// wrong a place for them as a directory inside it, so equality is refused too (isInside allows it, which
	// the registry, a strict subdirectory of the tree, relies on).
	if state == tree || isInside(tree, state) {
		return "", fmt.Errorf("the index state %s must be outside the tree %s", state, tree)
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

// canonicalStatePath resolves the symlinks of the existing part of a state directory, which need not exist yet,
// and keeps the rest of the path as it is, so that a state directory is compared with the tree by where it will
// be, not by the link that names it.
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
	return err == nil && filepath.IsLocal(rel) && rel != "."
}

// ErrDBRemoved refuses the key db: and the flag --db, which named the SQLite file the index was.
var ErrDBRemoved = errors.New("db: was removed: the index is a Postgres schema; set index: and pass DECLINDEX_DSN or --index-dsn-file")

// TreeSpec is the configuration's Tree.
func (c Config) TreeSpec() Tree {
	roots := Roots{}
	for _, r := range c.Roots {
		roots[r] = true
	}
	return Tree{Source: c.Tree, Lib: c.Lib, Roots: roots}
}

// Compiled gives the options of the compiled part for this configuration: the tree's toolchain, the exporter
// built with it below the index's state directory, and the library directories of the tree and its Lake
// packages. The state directory is resolved (StateDir), so that every path that names it builds one exporter and
// one forms module.
func (c Config) Compiled(ctx context.Context, forms FormsModule, log io.Writer) (*CompiledOptions, error) {
	l, err := FindLean(ctx, c.Tree, c.Lean.Toolchain)
	if err != nil {
		return nil, err
	}
	state, err := c.StateDir()
	if err != nil {
		return nil, err
	}
	bin, err := BuildExporter(ctx, l, c.Lean.CC, filepath.Join(state, "exporter"))
	if err != nil {
		return nil, err
	}
	version, _, err := exporterVersion(l)
	if err != nil {
		return nil, err
	}
	search := append([]string{c.Lib}, c.Lean.SearchPath...)
	for _, pattern := range []string{"*", "*/*"} {
		deps, _ := filepath.Glob(filepath.Join(c.Tree, ".lake", "packages", pattern, ".lake", "build", "lib", "lean"))
		search = append(search, deps...)
	}
	jobs := cmp.Or(c.Lean.Jobs, c.Jobs, defaultJobs())
	per := cmp.Or(c.Lean.ModulesPerProcess, 400)
	return &CompiledOptions{
		Exporter: &Exporter{Lean: l, Bin: bin, Version: version, SearchPath: search, Jobs: jobs, ModulesPerProcess: per, Log: log},
		Forms:    forms,
		Dir:      filepath.Join(state, "forms"),
	}, nil
}
