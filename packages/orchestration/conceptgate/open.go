package conceptgate

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sjawhar/legion/orchestration/declindex"
	"github.com/sjawhar/legion/orchestration/declindex/confine"
)

// Options are what OpenGate and RecheckLanding take besides the configuration.
type Options struct {
	// Librarian receives each review request; nil is the configured librarian command.
	Librarian Librarian
	// Log receives notes and the exporter's progress; nil discards them.
	Log io.Writer
	// SandboxAvailable is OpenGate's first check, that this machine can build the sandbox at all (confine's
	// kernel and architecture check). It starts no process, so a machine without the sandbox fails before any
	// toolchain is looked up or exporter built; nil is confine.Available. It is not the authoritative check: Decide
	// and Recheck run the toolchain in the sandbox (Gate.CheckSandbox) before they compile anything.
	SandboxAvailable func() error
}

func (o Options) log() io.Writer {
	if o.Log == nil {
		return io.Discard
	}
	return o.Log
}

// beforeRegistryCheck is a test hook: it runs after OpenGate's snapshot, before its check that the registry has no
// local changes.
var beforeRegistryCheck func()

// project is a configuration's tree, resolved through symlinks, and its registry: rel is the registry's
// slash-separated path in the tree, and prefix the tree's path in its repository as refresh recorded it in the
// index (empty at the repository's root, else ending in a slash). prefix+rel is the registry's path in the
// repository, the path that additions and the post-landing check name.
type project struct {
	cfg    declindex.Config
	tree   string
	rel    string
	prefix string
}

func resolveProject(ctx context.Context, cfg declindex.Config, index *declindex.Store) (project, error) {
	tree, err := filepath.EvalSymlinks(cfg.Tree)
	if err != nil {
		return project{}, fmt.Errorf("resolve tree: %w", err)
	}
	registry, err := cfg.GateRegistryDir()
	if err != nil {
		return project{}, err
	}
	rel, err := filepath.Rel(tree, registry)
	if err != nil {
		return project{}, fmt.Errorf("find registry relative to tree: %w", err)
	}
	// IsLocal refuses a path that leaves the tree; "." is the tree itself, no registry below it.
	if !filepath.IsLocal(rel) || rel == "." {
		return project{}, fmt.Errorf("registry %s is outside tree %s", registry, tree)
	}
	prefix, err := index.Meta(ctx, "source_prefix")
	if err != nil {
		return project{}, err
	}
	return project{cfg: cfg, tree: tree, rel: filepath.ToSlash(rel), prefix: prefix}, nil
}

// newGate is the one way that OpenGate and RecheckLanding build a gate: over index, reading the registry from the
// snapshot directory registry, with the exporter built by the configured toolchain, and with its proposals and
// librarian requests in work. The gate's forms come from the registry at each decision.
func newGate(ctx context.Context, p project, index *declindex.Store, registry, work string, opts Options) (*Gate, error) {
	compiled, err := p.cfg.Compiled(ctx, declindex.FormsModule{}, opts.log())
	if err != nil {
		return nil, err
	}
	proposals := filepath.Join(work, "proposals")
	librarian := opts.Librarian
	if librarian == nil {
		librarian = CommandLibrarian{Command: p.cfg.Librarian, WorkDir: proposals}
	}
	tree := p.cfg.TreeSpec()
	tree.Source = p.tree
	return &Gate{
		Index:        index,
		Tree:         tree,
		Registry:     registry,
		AdditionRoot: p.prefix + p.rel,
		Compiled:     compiled,
		Librarian:    librarian,
		WorkDir:      proposals,
	}, nil
}

// OpenGate builds the similarity gate of the project that cfg configures, after its preconditions: this machine
// can build the sandbox (Options.SandboxAvailable, before any toolchain is looked up); the index is outside the
// tree, where the gate opens it read-only, so that nothing the gate does can dirty the tree; the index is complete;
// the registry resolves inside the tree; and the tree's HEAD is the index's commit. It then snapshots the registry
// as committed at that commit, and the gate reads the registry only from that snapshot; and it checks, once, after
// the snapshot, that the registry has no local changes, so that what the caller sees in its tree is what the gate
// reads. Close releases the index and removes the snapshot and the gate's work directory.
func OpenGate(ctx context.Context, cfg declindex.Config, opts Options) (_ *Gate, err error) {
	available := opts.SandboxAvailable
	if available == nil {
		available = confine.Available
	}
	if err := available(); err != nil {
		return nil, fmt.Errorf("the gate compiles a proposal only in the sandbox: %w", err)
	}
	state, err := cfg.GateStatePath()
	if err != nil {
		return nil, err
	}
	// The gate also reads the tree: its HEAD, its registry's commit and its build.
	index, err := declindex.OpenReadOnly(ctx, state, declindex.IndexAndTree)
	if err != nil {
		return nil, err
	}
	var work string
	release := func() {
		_ = index.Close()
		if work != "" {
			_ = os.RemoveAll(work)
		}
	}
	defer func() {
		if err != nil {
			release()
		}
	}()
	if err := index.CheckComplete(ctx); err != nil {
		return nil, err
	}
	p, err := resolveProject(ctx, cfg, index)
	if err != nil {
		return nil, err
	}
	commit, err := index.Meta(ctx, "commit")
	if err != nil {
		return nil, err
	}
	head, err := declindex.TreeHead(ctx, p.tree)
	if err != nil {
		return nil, err
	}
	if head != commit {
		return nil, fmt.Errorf("the tree HEAD %s differs from the index commit %s", head, commit)
	}
	if work, err = os.MkdirTemp("", "declindex-gate-"); err != nil {
		return nil, err
	}
	registry, err := snapshotRegistry(ctx, p.tree, commit, p.rel, work)
	if err != nil {
		return nil, err
	}
	if beforeRegistryCheck != nil {
		beforeRegistryCheck()
	}
	if err := registryClean(ctx, p.tree, p.rel); err != nil {
		return nil, err
	}
	g, err := newGate(ctx, p, index, registry, work, opts)
	if err != nil {
		return nil, err
	}
	g.release = release
	return g, nil
}

// Close releases what OpenGate opened for the gate: its index, its registry snapshot and its work directory. It
// does nothing for a Gate built directly.
func (g *Gate) Close() {
	if g.release != nil {
		g.release()
		g.release = nil
	}
}

// LandingReport is the registry part of the post-landing check of one landing: the concept files that the landing
// added, by their paths from the repository's root (the paths the gate printed as additions); the registry's
// defects; the near pairs sent to the librarian; and the cause that stopped the recheck, if one did.
type LandingReport struct {
	Added   []string      `json:"added"`
	Defects []Defect      `json:"defects"`
	Near    []RecheckNear `json:"near"`
	Unable  string        `json:"unable"`
}

// Defective says whether the report has a defect or a near pair.
func (r *LandingReport) Defective() bool {
	return len(r.Defects) != 0 || len(r.Near) != 0
}

// RecheckLanding rechecks the registry concepts that the landing of landed (of commit when empty) added, as
// committed at commit, the commit of index, which the caller has checked is the tree's HEAD. It is nil when cfg
// names no registry. A cause that stops the recheck is the report's Unable.
func RecheckLanding(ctx context.Context, cfg declindex.Config, index *declindex.Store, commit, landed string, opts Options) *LandingReport {
	if cfg.Registry == "" {
		return nil
	}
	report := &LandingReport{Added: []string{}, Defects: []Defect{}, Near: []RecheckNear{}}
	if err := recheckLanding(ctx, cfg, index, commit, landed, opts, report); err != nil {
		report.Unable = err.Error()
	}
	return report
}

func recheckLanding(ctx context.Context, cfg declindex.Config, index *declindex.Store, commit, landed string, opts Options, report *LandingReport) error {
	p, err := resolveProject(ctx, cfg, index)
	if err != nil {
		return err
	}
	if landed == "" {
		landed = commit
	}
	previous, recorded, err := index.Landing(ctx, landed)
	if err != nil {
		return err
	}
	switch {
	case recorded:
		if report.Added, err = addedRegistryConcepts(ctx, p, previous, landed); err != nil {
			return err
		}
	case landed == commit:
		return fmt.Errorf("the index has no record of the landing of its commit %s: refresh the index", landed)
	default:
		// A later landing's refresh read this commit; the check of that landing rechecks this one's additions.
		fmt.Fprintf(opts.log(), "note: no refresh stopped at %s; the check of the landing that read it rechecks its additions\n", landed[:min(9, len(landed))])
	}
	work, err := os.MkdirTemp("", "declindex-recheck-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	snapshot, err := snapshotRegistry(ctx, p.tree, commit, p.rel, work)
	if err != nil {
		return fmt.Errorf("snapshot registry: %w", err)
	}
	reg, defects, err := Load(snapshot)
	if err != nil {
		return fmt.Errorf("load registry: %w", err)
	}
	report.Defects = append(report.Defects, defects...)
	// An added file that the loader refused or keyed wrongly is already a defect; the rest are rechecked.
	loaded := map[string]bool{}
	for _, c := range reg.Concepts {
		loaded[p.prefix+p.rel+"/"+conceptFile(c)] = true
	}
	var recheck []string
	for _, path := range report.Added {
		if loaded[path] {
			recheck = append(recheck, path)
		}
	}
	if len(recheck) == 0 {
		return nil
	}
	gate, err := newGate(ctx, p, index, snapshot, work, opts)
	if err != nil {
		return fmt.Errorf("prepare registry recheck: %w", err)
	}
	rechecked, err := Recheck(ctx, gate, reg, recheck)
	report.Defects = append(report.Defects, rechecked.Defects...)
	report.Near = append(report.Near, rechecked.Near...)
	return err
}

// addedRegistryConcepts lists the concept files (the .yaml files directly under concepts/, which the loader
// reads) that the commits previous..commit added, by their paths from the repository's root. Rename detection is
// off: a file added in the landing that deleted a similar one is an addition.
func addedRegistryConcepts(ctx context.Context, p project, previous, commit string) ([]string, error) {
	added := []string{}
	if previous == "" || commit == "" {
		return added, nil
	}
	concepts := p.rel + "/concepts"
	// The pathspec is relative to the tree, where git runs; the paths it prints are relative to the repository.
	raw, err := exec.CommandContext(ctx, "git", "-C", p.tree, "diff", "--name-only", "--no-renames", "--diff-filter=A", "-z", previous, commit, "--", concepts).Output()
	if err != nil {
		return nil, fmt.Errorf("list registry additions from %s to %s: %w", previous, commit, err)
	}
	prefix := p.prefix + concepts + "/"
	for _, path := range strings.Split(string(raw), "\x00") {
		name, ok := strings.CutPrefix(path, prefix)
		if !ok || strings.Contains(name, "/") || filepath.Ext(name) != ".yaml" {
			continue
		}
		added = append(added, path)
	}
	sort.Strings(added)
	return added, nil
}
