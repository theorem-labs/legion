package graphcheck

import (
	"context"
	"fmt"
	"path"
	"sort"

	"github.com/sjawhar/legion/orchestration/declindex"
	"github.com/sjawhar/legion/orchestration/landing"
	"github.com/sjawhar/legion/orchestration/landing/blame"
)

// Links runs L1 (README.md, "The rules"): in a link file of the candidate, an explicit binder of a public
// theorem whose type head is a contract that a link of another directory concludes. The binder keeps as a
// hypothesis what that link proves, so the theorem stands on an assumption the tree already discharges. A
// binder is left alone when the declaration's doc comment, the binder's line or the line above holds the open
// marker and a reason of at least the configured length.
//
// Every head is the exporter's: the candidate's from the modules Lean compiled for it, the base's from the
// index's compiled part. No judgement reads Lean source text.
func Links(ctx context.Context, c *Candidate, idx *declindex.Store, cfg Config) ([]landing.Finding, error) {
	changed := changedLinkModules(c, cfg)
	if len(changed) == 0 {
		return nil, nil
	}
	if err := compiledIndex(ctx, idx); err != nil {
		return nil, err
	}
	candidate, _, err := c.Constants(ctx)
	if err != nil {
		return nil, err
	}
	l := &links{c: c, idx: idx, cfg: cfg, concluded: map[string][]link{}, found: map[string][]link{}}
	for _, m := range changed {
		l.read(m, candidate[m.Module])
	}
	var out []landing.Finding
	for _, m := range changed {
		hits, err := l.check(ctx, m, candidate[m.Module])
		if err != nil {
			return nil, err
		}
		out = append(out, hits...)
	}
	sortFindings(out)
	return out, nil
}

// changedLinkModules are the added and modified link files of the candidate that Lean compiled, in path order.
func changedLinkModules(c *Candidate, cfg Config) []*Module {
	var out []*Module
	for _, ch := range c.Changes {
		if ch.Status == Deleted || !cfg.Links.IsLinkFile(ch.Path) {
			continue
		}
		if m := c.Module(ch.Module); m != nil && m.State == Compiled {
			out = append(out, m)
		}
	}
	return out
}

// link is one public theorem whose conclusion is a contract: a link that a hypothesis of that contract keeps.
type link struct {
	name, path string
	line       int
}

// links runs L1 over the candidate's changed link files. concluded holds the links of those files by the head
// each concludes, and found caches the links of one head, the base's included.
type links struct {
	c         *Candidate
	idx       *declindex.Store
	cfg       Config
	concluded map[string][]link
	found     map[string][]link
}

// read records the public links that one changed link file holds.
func (l *links) read(m *Module, constants []declindex.Constant) {
	byName := constantsByName(constants)
	for _, d := range m.Data.Decls {
		k, ok := byName[d.Name]
		if !ok || !publicTheorem(k) || k.Conclusion == "" {
			continue
		}
		l.concluded[k.Conclusion] = append(l.concluded[k.Conclusion], link{name: d.Name, path: m.Path, line: d.Line})
	}
}

// check flags each explicit binder of one changed link file whose head is a contract that a link of another
// directory concludes.
func (l *links) check(ctx context.Context, m *Module, constants []declindex.Constant) ([]landing.Finding, error) {
	byName := constantsByName(constants)
	dir := path.Dir(m.Path)
	var out []landing.Finding
	for _, d := range m.Data.Decls {
		k, ok := byName[d.Name]
		if !ok || !publicTheorem(k) {
			continue
		}
		seen := map[string]bool{}
		for _, head := range k.Binders {
			if head == "" || seen[head] || !l.cfg.Links.IsContract(lastComponent(head)) {
				continue
			}
			seen[head] = true
			conclusions, err := l.concluding(ctx, head)
			if err != nil {
				return nil, err
			}
			kept, ok := elsewhere(conclusions, dir)
			if !ok {
				continue
			}
			line, col := binderAt(l.c, m, d.Name, head, d.Line)
			if openMarked(l.c.Text(m.Path), l.cfg.Links.OpenMarker, l.cfg.Links.OpenMarkerMinReason, min(d.Line, line-1), line) {
				continue
			}
			out = append(out, landing.Finding{Path: m.Path, Line: line, Col: col, Verdict: landing.VerdictFlag,
				Kind: blame.Changed, Declaration: d.Name,
				Reason: fmt.Sprintf("L1: %s keeps the hypothesis %s, which %s concludes in %s:%d", d.Name, head,
					kept.name, kept.path, kept.line)})
		}
	}
	return out, nil
}

// elsewhere is the first link that another directory holds.
func elsewhere(found []link, dir string) (link, bool) {
	for _, l := range found {
		if path.Dir(l.path) != dir {
			return l, true
		}
	}
	return link{}, false
}

// concluding gives the links that conclude the head, in path order: those of the candidate's changed link
// files, and those of the base's link files that the candidate leaves alone.
func (l *links) concluding(ctx context.Context, head string) ([]link, error) {
	if cached, ok := l.found[head]; ok {
		return cached, nil
	}
	out := append([]link(nil), l.concluded[head]...)
	base, err := l.idx.ConstantsConcluding(ctx, []string{head})
	if err != nil {
		return nil, fmt.Errorf("read the base's links of %s: %w", head, err)
	}
	modules := map[string]string{} // name -> module, of the base's links
	var names []string
	for _, k := range base {
		if !publicTheorem(k) || l.c.ChangeOfModule(k.Module) != nil {
			continue
		}
		if !l.cfg.Links.IsLinkFile(l.c.PathOf(k.Module)) {
			continue
		}
		modules[k.Name] = k.Module
		names = append(names, k.Name)
	}
	if len(names) > 0 {
		decls, err := l.idx.DeclsNamed(ctx, names)
		if err != nil {
			return nil, fmt.Errorf("read the base's links of %s: %w", head, err)
		}
		for _, d := range decls {
			if modules[d.Name] == d.Module {
				out = append(out, link{name: d.Name, path: l.c.PathOf(d.Module), line: d.Line})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].path != out[j].path {
			return out[i].path < out[j].path
		}
		return out[i].name < out[j].name
	})
	l.found[head] = out
	return out, nil
}

// constantsByName indexes one module's compiled constants by name.
func constantsByName(constants []declindex.Constant) map[string]declindex.Constant {
	out := make(map[string]declindex.Constant, len(constants))
	for _, k := range constants {
		out[k.Name] = k
	}
	return out
}

// compiledIndex refuses an index that cannot answer about the base's compiled statements: a check that judges
// Lean by its compiled output has nothing to judge by without one.
func compiledIndex(ctx context.Context, idx *declindex.Store) error {
	if _, err := idx.CompiledCommit(ctx, true); err != nil {
		return unable("the graph checks read the index's compiled part: %v", err)
	}
	return nil
}

// binderAt is the position to report for the binder of head in a declaration of a changed module: where the
// module's reference file records the head inside the statement, moved to the bracket that opens the binder,
// and the declaration's own position when it records no such use.
func binderAt(c *Candidate, m *Module, declaration, head string, declLine int) (int, int) {
	line, col, ok := headUse(m.Data.Uses, declaration, head)
	if !ok {
		return declLine, declarationColumn(c.Text(m.Path), declLine)
	}
	return line, binderColumn(c.Text(m.Path), line, col)
}
