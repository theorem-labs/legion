package graphcheck

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/sjawhar/legion/orchestration/declindex"
)

// UnableError is a check that cannot decide; the command exits 3 with its message.
type UnableError struct{ Reason string }

func (e *UnableError) Error() string { return e.Reason }

func unable(format string, args ...any) error {
	return &UnableError{Reason: fmt.Sprintf(format, args...)}
}

// Status is how the candidate changed a file.
type Status byte

const (
	Added    Status = 'A'
	Modified Status = 'M'
	Deleted  Status = 'D'
)

// Change is one changed .lean file of the roots.
type Change struct {
	Path   string
	Module string
	Status Status
}

// CompileState is what compiling a changed module gave.
type CompileState int

const (
	// NotBuilt is a changed module that the build does not build: it is not compiled.
	NotBuilt CompileState = iota
	// Compiled is a module that Lean compiled; Data holds its reference file.
	Compiled
	// Failed is a module whose compile Lean refused; Errors holds Lean's errors.
	Failed
	// Skipped is a module that imports a changed module that did not compile.
	Skipped
)

// LeanError is one error that Lean reported compiling a changed module.
type LeanError struct {
	// File is the file Lean named, relative to the directory the module was compiled in.
	File      string
	Line, Col int // 1-based; the column in code points
	Message   string
}

// Module is one added or modified module of the candidate, as Lean compiled it against the base's build.
type Module struct {
	Change
	// Imports are the modules its header imports, as Lean reads the header.
	Imports []string
	State   CompileState
	// Data is the compiled module's reference file, read by the index's own reader.
	Data declindex.ModuleData
	// Errors are the errors of a failed compile, and Output the whole compile output that blame.Parse reads.
	Errors []LeanError
	Output string
}

// Candidate is the candidate tree against its base. The base's facts come from the declaration index; the
// candidate's added and modified modules that the build builds are compiled with Lean against the base's build,
// in a scratch directory, and read from Lean's output. Close removes the scratch directory.
type Candidate struct {
	// Base is LAND_BASE's commit, BaseTree its tree, and Tree the candidate tree.
	Base, BaseTree, Tree string
	// Changes are sorted by path.
	Changes []Change

	repo     string
	cfg      Config
	icfg     declindex.Config
	lean     declindex.Lean
	scratch  string
	byPath   map[string]*Change
	byModule map[string]*Change
	text     map[string]string // path -> candidate text, for added and modified files
	modules  map[string]*Module // added and modified modules
	graph    map[string][]string
	built    map[string]bool
	paths    map[string]string // module -> path, of the index and the changed files

	exportOnce              sync.Once
	constants, baseConstant map[string][]declindex.Constant // module -> constants
	exportErr               error
}

// Open checks the index against the base (D5), reads the candidate's changes, and compiles the changed modules that
// the build builds. The caller closes both.
func Open(ctx context.Context, cfg Config, base, tree string) (*Candidate, *declindex.Store, error) {
	if base == "" || tree == "" {
		return nil, nil, unable("LAND_BASE and LAND_TREE must both be set")
	}
	g := gitDir(cfg.Repository)
	baseCommit, err := g.revParse(ctx, base+"^{commit}")
	if err != nil {
		return nil, nil, unable("LAND_BASE %s is not a commit of %s: %v", base, cfg.Repository, err)
	}
	baseTree, err := g.revParse(ctx, baseCommit+"^{tree}")
	if err != nil {
		return nil, nil, unable("LAND_BASE %s has no tree: %v", base, err)
	}
	if kind, err := g.output(ctx, "cat-file", "-t", tree); err != nil || strings.TrimSpace(kind) != "tree" {
		return nil, nil, unable("LAND_TREE %s is not a tree of %s", tree, cfg.Repository)
	}
	treeID, err := g.revParse(ctx, tree+"^{tree}")
	if err != nil {
		return nil, nil, unable("LAND_TREE %s: %v", tree, err)
	}
	idx, icfg, err := openIndex(ctx, cfg, g, baseTree)
	if err != nil {
		return nil, nil, err
	}
	c := &Candidate{Base: baseCommit, BaseTree: baseTree, Tree: treeID, repo: cfg.Repository, cfg: cfg, icfg: icfg,
		byPath: map[string]*Change{}, byModule: map[string]*Change{}, text: map[string]string{},
		modules: map[string]*Module{}}
	if err := c.read(ctx, g, idx); err != nil {
		_ = c.Close()
		_ = idx.Close()
		return nil, nil, err
	}
	return c, idx, nil
}

// openIndex opens the configured index read-only and refuses one that cannot describe the base: incomplete, not
// of a checkout root, or of another tree than the base's.
func openIndex(ctx context.Context, cfg Config, g gitDir, baseTree string) (*declindex.Store, declindex.Config, error) {
	icfg, err := declindex.LoadConfig(cfg.Index)
	if err != nil {
		return nil, icfg, unable("index configuration: %v", err)
	}
	prefix, err := gitDir(icfg.Tree).output(ctx, "rev-parse", "--show-prefix")
	if err != nil {
		return nil, icfg, unable("the index's tree %s is not a git checkout: %v", icfg.Tree, err)
	}
	if p := strings.TrimSpace(prefix); p != "" {
		return nil, icfg, unable("the index's tree %s is %s below its checkout's root, so its module paths are not repository paths", icfg.Tree, p)
	}
	idx, err := declindex.OpenReadOnlyConfig(ctx, icfg, declindex.IndexOnly)
	if err != nil {
		return nil, icfg, unable("open the index: %v", err)
	}
	fail := func(err error) (*declindex.Store, declindex.Config, error) {
		_ = idx.Close()
		return nil, icfg, err
	}
	if err := idx.CheckComplete(ctx); err != nil {
		return fail(unable("%v", err))
	}
	commit, err := idx.Commit(ctx)
	if err != nil {
		return fail(err)
	}
	if commit == "" {
		return fail(unable("the index records no commit"))
	}
	indexTree, err := g.revParse(ctx, commit+"^{tree}")
	if err != nil {
		return fail(unable("the index's commit %s is not in %s", commit, cfg.Repository))
	}
	if indexTree != baseTree {
		return fail(unable("the index is of commit %s, tree %s, and LAND_BASE has tree %s", commit, indexTree, baseTree))
	}
	return idx, icfg, nil
}

// read lists the changed .lean files of the roots, reads the candidate's texts and -U0 hunks, and compiles.
func (c *Candidate) read(ctx context.Context, g gitDir, idx *declindex.Store) error {
	out, err := g.output(ctx, "diff-tree", "-r", "-z", "--no-renames", "--name-status", c.BaseTree, c.Tree)
	if err != nil {
		return err
	}
	fields := strings.Split(strings.TrimSuffix(out, "\x00"), "\x00")
	for i := 0; i+1 < len(fields); i += 2 {
		status, file := fields[i], fields[i+1]
		module, ok := strings.CutSuffix(file, ".lean")
		if !ok {
			continue
		}
		module = strings.ReplaceAll(module, "/", ".")
		if !c.cfg.inRoots(module) {
			continue
		}
		if strings.ContainsAny(file, "\n\t") {
			return unable("changed file %q has a tab or a newline in its path", file)
		}
		var s Status
		switch status {
		case "A":
			s = Added
		case "M", "T":
			s = Modified
		case "D":
			s = Deleted
		default:
			return unable("changed file %s has status %s", file, status)
		}
		c.Changes = append(c.Changes, Change{Path: file, Module: module, Status: s})
	}
	sort.Slice(c.Changes, func(i, j int) bool { return c.Changes[i].Path < c.Changes[j].Path })
	var specs, patched []string
	for i := range c.Changes {
		ch := &c.Changes[i]
		c.byPath[ch.Path] = ch
		c.byModule[ch.Module] = ch
		if ch.Status != Deleted {
			specs = append(specs, c.Tree+":"+ch.Path)
			patched = append(patched, ch.Path)
		}
	}
	paths, err := idx.ModulePaths(ctx)
	if err != nil {
		return err
	}
	c.paths = map[string]string{}
	for module, file := range paths {
		c.paths[normalName(module)] = file
	}
	if len(specs) != 0 {
		cat, err := startCatFile(c.repo)
		if err != nil {
			return err
		}
		blobs, err := cat.read(specs)
		_ = cat.close()
		if err != nil {
			return err
		}
		for i, file := range patched {
			c.text[file] = blobs[i]
		}
	}
	return c.compile(ctx, idx)
}

// compile writes the added and modified files into a scratch directory, reads their imports with Lean, finds the
// modules the build builds, and compiles those of them in import order against the base's build.
func (c *Candidate) compile(ctx context.Context, idx *declindex.Store) error {
	base, err := idx.Imports(ctx)
	if err != nil {
		return err
	}
	c.graph = make(map[string][]string, len(base)+len(c.Changes))
	for module, imported := range base {
		module = normalName(module)
		if c.byModule[module] == nil {
			c.graph[module] = normalNames(imported)
		}
	}
	var changed []*Module
	for _, ch := range c.Changes {
		if ch.Status == Deleted {
			continue
		}
		m := &Module{Change: ch}
		c.modules[ch.Module] = m
		c.paths[ch.Module] = ch.Path
		changed = append(changed, m)
	}
	if len(changed) == 0 {
		c.closure()
		return nil
	}
	c.lean, err = declindex.FindLean(ctx, c.icfg.Tree, c.icfg.Lean.Toolchain)
	if err != nil {
		return unable("the index's tree selects no Lean toolchain: %v", err)
	}
	if c.scratch, err = os.MkdirTemp("", "graphcheck-"); err != nil {
		return err
	}
	files := make([]string, 0, len(changed))
	for _, m := range changed {
		target := filepath.Join(c.scratch, filepath.FromSlash(m.Path))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(target, []byte(c.text[m.Path]), 0o644); err != nil {
			return err
		}
		files = append(files, m.Path)
	}
	imports, err := c.headerImports(ctx, files)
	if err != nil {
		return err
	}
	for i, m := range changed {
		h := imports[i]
		m.Imports = h.imports
		c.graph[m.Module] = m.Imports
		if h.err != "" {
			m.State, m.Output = Failed, h.err
			m.Errors = []LeanError{{File: m.Path, Line: 1, Col: 1, Message: h.err}}
		}
	}
	c.closure()
	if err := c.linkBase(); err != nil {
		return err
	}
	return c.compileBuilt(ctx, changed)
}

// linkBase fills the scratch with the base build's outputs, so that it is a complete library of the candidate.
// Lean resolves every module of a root in the first search-path directory that holds that root and looks no
// further, so the changed modules and the unchanged ones have to sit together. The outputs of a changed module
// are left out: its own compile writes them, and a deleted module's are gone from the candidate. The links are
// symbolic, and no compile writes through one.
func (c *Candidate) linkBase() error {
	return filepath.WalkDir(c.icfg.Lib, func(p string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		rel, err := filepath.Rel(c.icfg.Lib, p)
		if err != nil {
			return err
		}
		if c.byModule[moduleOfOutput(rel)] != nil {
			return nil
		}
		target := filepath.Join(c.scratch, rel)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.Symlink(p, target); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
		return nil
	})
}

// moduleOfOutput is the module one build output belongs to: its path without the first suffix of its last
// component, so that Lib/Core.olean, Lib/Core.olean.hash and Lib/Core.trace all give Lib.Core.
func moduleOfOutput(rel string) string {
	rel = filepath.ToSlash(rel)
	slash := strings.LastIndexByte(rel, '/')
	if dot := strings.IndexByte(rel[slash+1:], '.'); dot >= 0 {
		rel = rel[:slash+1+dot]
	}
	return strings.ReplaceAll(rel, "/", ".")
}

// closure computes the modules the build builds: the closure of build_roots over the candidate's import graph,
// with each added module a root too when root_new_modules is set.
func (c *Candidate) closure() {
	roots := normalNames(c.cfg.BuildRoots)
	for _, ch := range c.Changes {
		if ch.Status == Added && c.cfg.RootNewModules {
			roots = append(roots, ch.Module)
		}
	}
	c.built = map[string]bool{}
	stack := roots
	for len(stack) > 0 {
		module := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		imported, ok := c.graph[module]
		if !ok || c.built[module] {
			continue
		}
		c.built[module] = true
		stack = append(stack, imported...)
	}
}

type header struct {
	imports []string
	err     string
}

// headerImports reads the files' headers with Lean (`lean --deps-json`), in the scratch directory.
func (c *Candidate) headerImports(ctx context.Context, files []string) ([]header, error) {
	cmd := exec.CommandContext(ctx, filepath.Join(c.lean.Prefix, "bin", "lean"), append([]string{"--deps-json"}, files...)...)
	cmd.Dir = c.scratch
	cmd.Env = []string{"LEAN_SYSROOT=" + c.lean.Prefix, "PATH=" + filepath.Join(c.lean.Prefix, "bin") + ":/usr/bin:/bin", "HOME=" + c.scratch, "TMPDIR=" + c.scratch}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, unable("lean --deps-json: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	var out struct {
		Imports []struct {
			Errors []any `json:"errors"`
			Result *struct {
				Imports []struct {
					Module string `json:"module"`
				} `json:"imports"`
			} `json:"result"`
		} `json:"imports"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		return nil, unable("lean --deps-json printed %q: %v", strings.TrimSpace(stdout.String()), err)
	}
	if len(out.Imports) != len(files) {
		return nil, unable("lean --deps-json read %d headers of %d files", len(out.Imports), len(files))
	}
	headers := make([]header, len(files))
	for i, entry := range out.Imports {
		if len(entry.Errors) != 0 || entry.Result == nil {
			headers[i].err = fmt.Sprintf("Lean cannot read the header of %s: %v", files[i], entry.Errors)
			continue
		}
		seen := map[string]bool{}
		for _, imp := range entry.Result.Imports {
			name := normalName(imp.Module)
			if !seen[name] {
				seen[name] = true
				headers[i].imports = append(headers[i].imports, name)
			}
		}
	}
	return headers, nil
}

// compileBuilt compiles the changed modules that the build builds. Each starts as soon as every changed module in
// its import closure is compiled, at most 48 at a time; a module whose closure holds a changed module that did not
// compile is skipped, and so are the modules waiting for it.
func (c *Candidate) compileBuilt(ctx context.Context, changed []*Module) error {
	pending := map[string]*Module{}
	for _, m := range changed {
		if c.built[m.Module] && m.State == NotBuilt {
			pending[m.Module] = m
		}
	}
	if len(pending) == 0 {
		return nil
	}
	jobs := c.icfg.Jobs
	if jobs <= 0 {
		jobs = runtime.NumCPU()
	}
	jobs = min(jobs, 48)
	searchPath := append([]string{c.icfg.Lib}, c.icfg.Lean.SearchPath...)
	roots := declindex.Roots{}
	for _, r := range c.icfg.Roots {
		roots[r] = true
	}
	// deps are the changed modules in each module's closure, waits how many of them are still pending, and users
	// the pending modules that wait for each one.
	deps := make(map[string][]string, len(pending))
	waits := make(map[string]int, len(pending))
	users := map[string][]*Module{}
	for name, m := range pending {
		deps[name] = c.changedDeps(m)
		for _, d := range deps[name] {
			if pending[d] != nil {
				waits[name]++
				users[d] = append(users[d], m)
			}
		}
	}
	var queue []*Module
	left := len(pending)
	// finish records a module that compiled, failed or was skipped, and makes ready each user it was the last wait
	// of; a ready user with a changed module in its closure that did not compile is skipped, and finished in turn.
	var finish func(m *Module)
	finish = func(m *Module) {
		left--
		for _, u := range users[m.Module] {
			if waits[u.Module]--; waits[u.Module] != 0 {
				continue
			}
			if c.closureFailed(deps[u.Module]) {
				u.State = Skipped
				finish(u)
				continue
			}
			queue = append(queue, u)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(pending)) {
		if m := pending[name]; waits[name] == 0 {
			if c.closureFailed(deps[name]) {
				m.State = Skipped
				finish(m)
			} else {
				queue = append(queue, m)
			}
		}
	}
	type result struct {
		m   *Module
		err error
	}
	results := make(chan result, len(pending))
	running := 0
	var firstErr error
	for left > 0 || running > 0 {
		for firstErr == nil && running < jobs && len(queue) > 0 {
			m := queue[0]
			queue = queue[1:]
			running++
			go func() { results <- result{m, c.compileOne(ctx, m, searchPath, roots)} }()
		}
		if running == 0 {
			if firstErr != nil || left == 0 {
				break
			}
			// Nothing runs and nothing is ready, yet modules wait: they import each other.
			for _, m := range pending {
				if m.State == NotBuilt {
					m.State, m.Errors = Failed, []LeanError{{File: m.Path, Line: 1, Col: 1, Message: "import cycle among the changed modules"}}
				}
			}
			break
		}
		r := <-results
		running--
		if r.err != nil && firstErr == nil {
			firstErr = r.err
		}
		finish(r.m)
	}
	return firstErr
}

// changedDeps gives the changed modules a module imports, directly or through unchanged modules: Lean reads the
// .olean of every module in the import closure, so each of them must be compiled first.
func (c *Candidate) changedDeps(m *Module) []string {
	seen := map[string]bool{m.Module: true}
	var out []string
	stack := slices.Clone(m.Imports)
	for len(stack) > 0 {
		name := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[name] {
			continue
		}
		seen[name] = true
		if c.modules[name] != nil {
			out = append(out, name)
		}
		stack = append(stack, c.graph[name]...)
	}
	return out
}

// closureFailed reports whether one of the changed modules a module needs did not compile.
func (c *Candidate) closureFailed(deps []string) bool {
	for _, d := range deps {
		if s := c.modules[d].State; s == Failed || s == Skipped {
			return true
		}
	}
	return false
}

// leanMessageRE is the first line of one of Lean's messages: `<file>:<line>:<col>: <severity>: <text>`.
var leanMessageRE = regexp.MustCompile(`^(.+?\.lean):(\d+):(\d+): (error|warning|info)(?:\([^)]*\))?: ?(.*)$`)

// compileOne compiles one module with declindex.CompileModule and reads its reference file, or keeps Lean's
// output for C1 (compile.go), which turns it into findings or into the reason the check cannot decide.
func (c *Candidate) compileOne(ctx context.Context, m *Module, searchPath []string, roots declindex.Roots) error {
	rel := strings.TrimSuffix(m.Path, ".lean")
	err := declindex.CompileModule(ctx, c.lean, searchPath, c.scratch, rel)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		m.State, m.Output = Failed, err.Error()
		m.Errors = parseLeanErrors(m.Output, c.scratch)
		return nil
	}
	ilean, err := os.ReadFile(filepath.Join(c.scratch, filepath.FromSlash(rel)+".ilean"))
	if err != nil {
		return unable("compile %s: Lean wrote no reference file: %v", m.Path, err)
	}
	data, err := declindex.ParseModule(rel, ilean, []byte(c.text[m.Path]), roots)
	if err != nil {
		return unable("read the compiled %s: %v", m.Path, err)
	}
	m.State, m.Data = Compiled, data
	return nil
}

// parseLeanErrors reads Lean's error messages from its output: each message is its first line and the indented
// or continuing lines up to the next message. root is the directory the modules were compiled in, below which
// the messages name their files.
func parseLeanErrors(output, root string) []LeanError {
	var out []LeanError
	var current *LeanError
	for _, line := range strings.Split(output, "\n") {
		if m := leanMessageRE.FindStringSubmatch(line); m != nil {
			current = nil
			if m[4] != "error" {
				continue
			}
			l, _ := strconv.Atoi(m[2])
			col, _ := strconv.Atoi(m[3])
			file := filepath.ToSlash(strings.TrimPrefix(strings.TrimPrefix(m[1], root), string(filepath.Separator)))
			out = append(out, LeanError{File: file, Line: max(l, 1), Col: col + 1, Message: m[5]})
			current = &out[len(out)-1]
			continue
		}
		if current != nil && line != "" {
			current.Message += "\n" + line
		}
	}
	return out
}

// Close removes the scratch directory.
func (c *Candidate) Close() error {
	if c == nil || c.scratch == "" {
		return nil
	}
	err := os.RemoveAll(c.scratch)
	c.scratch = ""
	return err
}

// Change returns the change of a file, or nil when the file is unchanged.
func (c *Candidate) Change(file string) *Change { return c.byPath[file] }

// ChangeOfModule returns the change of a module's file, or nil when it is unchanged.
func (c *Candidate) ChangeOfModule(module string) *Change { return c.byModule[normalName(module)] }

// Module returns an added or modified module, or nil.
func (c *Candidate) Module(module string) *Module { return c.modules[normalName(module)] }

// CompiledModules are the changed modules Lean compiled, in name order: those the build builds, and no module
// downstream of them. The check's cost is theirs.
func (c *Candidate) CompiledModules() []string {
	out := make([]string, 0, len(c.modules))
	for module, m := range c.modules {
		if m.State == Compiled {
			out = append(out, module)
		}
	}
	sort.Strings(out)
	return out
}

// Built reports whether the build builds the module in the candidate.
func (c *Candidate) Built(module string) bool { return c.built[normalName(module)] }

// PathOf is a module's file: the index's path, a changed file's, or the path its name gives.
func (c *Candidate) PathOf(module string) string {
	module = normalName(module)
	if file, ok := c.paths[module]; ok {
		return file
	}
	return strings.ReplaceAll(module, ".", "/") + ".lean"
}

// Text is a changed file's text in the candidate ("" for a deleted file).
func (c *Candidate) Text(file string) string { return c.text[file] }

// ImportersOf gives every built module that imports the module, directly or not, the module itself included.
func (c *Candidate) importersOf(module string, reverse map[string][]string) map[string]bool {
	out := map[string]bool{}
	stack := []string{module}
	for len(stack) > 0 {
		m := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if out[m] {
			continue
		}
		out[m] = true
		stack = append(stack, reverse[m]...)
	}
	for m := range out {
		if !c.built[m] {
			delete(out, m)
		}
	}
	return out
}

// reverseGraph maps each module to the modules that import it directly.
func (c *Candidate) reverseGraph() map[string][]string {
	reverse := map[string][]string{}
	for module, imported := range c.graph {
		for _, imp := range imported {
			reverse[imp] = append(reverse[imp], module)
		}
	}
	return reverse
}

// exporter builds (once, cached by its version) the index's exporter for the tree's toolchain.
func (c *Candidate) exporter(ctx context.Context, searchPath []string) (*declindex.Exporter, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		cache = os.TempDir()
	}
	bin, err := declindex.BuildExporter(ctx, c.lean, c.icfg.Lean.CC, filepath.Join(cache, "graphcheck", "exporter"))
	if err != nil {
		return nil, unable("build the exporter: %v", err)
	}
	jobs := c.icfg.Jobs
	if jobs <= 0 {
		jobs = min(48, runtime.NumCPU())
	}
	return &declindex.Exporter{Lean: c.lean, Bin: bin, SearchPath: searchPath, Jobs: jobs, ScratchDir: c.scratch}, nil
}

// Constants gives the compiled constants of every compiled changed module, and BaseConstants those of the base's
// build of every modified and deleted module, each by module, as the index's exporter reads them.
func (c *Candidate) Constants(ctx context.Context) (candidate, base map[string][]declindex.Constant, err error) {
	c.exportOnce.Do(func() { c.exportErr = c.export(ctx) })
	return c.constants, c.baseConstant, c.exportErr
}

func (c *Candidate) export(ctx context.Context) error {
	c.constants, c.baseConstant = map[string][]declindex.Constant{}, map[string][]declindex.Constant{}
	var compiled, before []string
	for _, ch := range c.Changes {
		if m := c.modules[ch.Module]; m != nil && m.State == Compiled {
			compiled = append(compiled, ch.Module)
		}
		if ch.Status != Added {
			if module, ok := c.indexModule(ch); ok {
				before = append(before, module)
			}
		}
	}
	if len(compiled) == 0 && len(before) == 0 {
		return nil
	}
	if c.lean.Prefix == "" {
		l, err := declindex.FindLean(ctx, c.icfg.Tree, c.icfg.Lean.Toolchain)
		if err != nil {
			return unable("the index's tree selects no Lean toolchain: %v", err)
		}
		c.lean = l
	}
	if c.scratch == "" {
		dir, err := os.MkdirTemp("", "graphcheck-")
		if err != nil {
			return err
		}
		c.scratch = dir
	}
	for _, side := range []struct {
		modules    []string
		searchPath []string
		into       map[string][]declindex.Constant
	}{
		{compiled, append([]string{c.scratch, c.icfg.Lib}, c.icfg.Lean.SearchPath...), c.constants},
		{before, append([]string{c.icfg.Lib}, c.icfg.Lean.SearchPath...), c.baseConstant},
	} {
		if len(side.modules) == 0 {
			continue
		}
		x, err := c.exporter(ctx, side.searchPath)
		if err != nil {
			return err
		}
		constants, err := x.Run(ctx, declindex.Manifest{Modules: side.modules, Roots: c.icfg.Roots, Types: true})
		if err != nil {
			return unable("export the compiled constants: %v", err)
		}
		for _, k := range constants {
			side.into[normalName(k.Module)] = append(side.into[normalName(k.Module)], k)
		}
	}
	return nil
}

// indexModule is the index's name of a changed file's module.
func (c *Candidate) indexModule(ch Change) (string, bool) {
	for module, file := range c.paths {
		if file == ch.Path && c.modules[module] == nil {
			return module, true
		}
	}
	if ch.Status != Added {
		return ch.Module, true
	}
	return "", false
}

// gitDir runs git in one directory.
type gitDir string

func (g gitDir) output(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = string(g)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func (g gitDir) revParse(ctx context.Context, rev string) (string, error) {
	out, err := g.output(ctx, "rev-parse", "--verify", "--quiet", "--end-of-options", rev)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// catFile is one `git cat-file --batch` process.
type catFile struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
}

func startCatFile(repo string) (*catFile, error) {
	cmd := exec.Command("git", "cat-file", "--batch")
	cmd.Dir = repo
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &catFile{cmd: cmd, stdin: stdin, stdout: bufio.NewReaderSize(stdout, 1<<16)}, nil
}

// read returns the blobs that specs (`<tree>:<path>`) name, in order; a missing one is an error.
func (c *catFile) read(specs []string) ([]string, error) {
	written := make(chan error, 1)
	go func() {
		w := bufio.NewWriter(c.stdin)
		for _, spec := range specs {
			if _, err := w.WriteString(spec + "\n"); err != nil {
				written <- err
				return
			}
		}
		written <- w.Flush()
	}()
	out := make([]string, len(specs))
	for i, spec := range specs {
		header, err := c.stdout.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("git cat-file: %w", err)
		}
		fields := strings.Fields(header)
		if len(fields) != 3 || fields[1] != "blob" {
			return nil, fmt.Errorf("git cat-file %s: %s", spec, strings.TrimSpace(header))
		}
		size, err := strconv.Atoi(fields[2])
		if err != nil {
			return nil, fmt.Errorf("git cat-file %s: %s", spec, strings.TrimSpace(header))
		}
		body := make([]byte, size+1)
		if _, err := io.ReadFull(c.stdout, body); err != nil {
			return nil, fmt.Errorf("git cat-file %s: %w", spec, err)
		}
		out[i] = string(body[:size])
	}
	if err := <-written; err != nil {
		return nil, fmt.Errorf("git cat-file: %w", err)
	}
	return out, nil
}

func (c *catFile) close() error {
	_ = c.stdin.Close()
	return c.cmd.Wait()
}

// normalName drops the «» quotes of a Lean name.
func normalName(name string) string {
	if !strings.ContainsAny(name, "«»") {
		return name
	}
	return strings.NewReplacer("«", "", "»", "").Replace(name)
}

func normalNames(names []string) []string {
	out := make([]string, len(names))
	for i, name := range names {
		out[i] = normalName(name)
	}
	return out
}
