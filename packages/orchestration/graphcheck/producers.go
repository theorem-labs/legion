package graphcheck

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/sjawhar/legion/orchestration/declindex"
	"github.com/sjawhar/legion/orchestration/landing"
	"github.com/sjawhar/legion/orchestration/landing/blame"
)

// Producers runs P1 (README.md, "The rules"): in a file whose binders the configuration reads, a binder whose
// type head is an interface that nothing in the candidate produces. A producer is a declaration whose
// conclusion is the interface (an instance, a definition, a theorem) or one that applies the interface's
// constructor. A binder that already had no producer at the base is the base's state, not this change's, and is
// not reported; a binder whose last producer the candidate removes is, and the finding names that producer.
//
// Every head is the exporter's: the candidate's from the modules Lean compiled for it, the base's from the
// index's compiled part. No judgement reads Lean source text.
func Producers(ctx context.Context, c *Candidate, idx *declindex.Store, cfg Config) ([]landing.Finding, error) {
	if len(c.Changes) == 0 {
		return nil, nil
	}
	if err := compiledIndex(ctx, idx); err != nil {
		return nil, err
	}
	candidate, base, err := c.Constants(ctx)
	if err != nil {
		return nil, err
	}
	p := &producers{c: c, idx: idx, cfg: cfg, candidate: candidate, changed: compiledModules(c)}
	var out []landing.Finding
	for _, name := range p.interfaces(base) {
		hits, err := p.check(ctx, name)
		if err != nil {
			return nil, err
		}
		out = append(out, hits...)
	}
	sortFindings(out)
	return out, nil
}

// compiledModules are the added and modified modules that Lean compiled for the candidate, in path order.
func compiledModules(c *Candidate) []*Module {
	var out []*Module
	for _, ch := range c.Changes {
		if m := c.Module(ch.Module); m != nil && m.State == Compiled {
			out = append(out, m)
		}
	}
	return out
}

// producers runs P1 over the candidate. candidate holds the compiled constants of the changed modules Lean
// compiled, by module, and changed those modules.
type producers struct {
	c         *Candidate
	idx       *declindex.Store
	cfg       Config
	candidate map[string][]declindex.Constant
	changed   []*Module
}

// declaration names one declaration: two modules that are never imported together can each declare one name.
type declaration struct{ name, module string }

// producer is a declaration that gives a value of an interface.
type producer declaration

// bound is one binder of an interface: the declaration that binds it, and where its source writes the head.
type bound struct {
	declaration
	path      string
	line, col int
}

// interfaces are the interface heads this change can have made producerless, sorted: those that a binder of a
// changed binder file keeps, and those that a declaration of the base's changed modules concluded, whose
// producer the candidate may have removed.
func (p *producers) interfaces(base map[string][]declindex.Constant) []string {
	found := map[string]bool{}
	for _, m := range p.changed {
		if !p.cfg.Producers.IsBinderFile(m.Path) {
			continue
		}
		for _, k := range p.candidate[m.Module] {
			if !declared(k, "") {
				continue
			}
			for _, head := range k.Binders {
				if head != "" && p.cfg.Producers.IsInterface(lastComponent(head)) {
					found[head] = true
				}
			}
		}
	}
	for _, constants := range base {
		for _, k := range constants {
			if declared(k, "") && k.Conclusion != "" && p.cfg.Producers.IsInterface(lastComponent(k.Conclusion)) {
				found[k.Conclusion] = true
			}
		}
	}
	out := make([]string, 0, len(found))
	for name := range found {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// check reports the binders of one interface that the candidate leaves without a producer.
func (p *producers) check(ctx context.Context, name string) ([]landing.Finding, error) {
	uses, err := p.idx.UsesOf(ctx, []string{name})
	if err != nil {
		return nil, fmt.Errorf("read the base's uses of %s: %w", name, err)
	}
	base, err := p.baseProducers(ctx, name, uses)
	if err != nil {
		return nil, err
	}
	if len(p.candidateProducers(name, base)) > 0 {
		return nil, nil
	}
	baseBinders, err := p.baseBinders(ctx, name, uses)
	if err != nil {
		return nil, err
	}
	var out []landing.Finding
	for _, b := range p.binders(name, baseBinders) {
		if len(base) == 0 && baseBinders[b.declaration].name != "" {
			continue // The base already had this binder and no producer: the finding is not this change's.
		}
		finding := landing.Finding{Path: b.path, Line: b.line, Col: b.col, Verdict: landing.VerdictFlag,
			Kind: blame.Changed, Declaration: b.name}
		if len(base) > 0 {
			finding.Declaration = base[0].name
			finding.Reason = fmt.Sprintf("P1: %s produced %s in the base and the candidate produces it nowhere, and %s binds it",
				base[0].name, name, b.name)
		} else {
			finding.Reason = fmt.Sprintf("P1: %s binds %s, declared in %s, which nothing in the candidate produces",
				b.name, name, p.declaredIn(ctx, name, uses))
		}
		out = append(out, finding)
	}
	return out, nil
}

// baseProducers are the declarations of the base that produce the interface, sorted by name: those whose
// conclusion is the interface, and those that apply its constructor, which uses records.
func (p *producers) baseProducers(ctx context.Context, name string, uses []declindex.Use) ([]producer, error) {
	constants, err := p.idx.ConstantsConcluding(ctx, []string{name})
	if err != nil {
		return nil, fmt.Errorf("read the base's producers of %s: %w", name, err)
	}
	seen := map[declaration]bool{}
	var out []producer
	add := func(d declaration) {
		if d.name == "" || partOf(d.name, name) || seen[d] {
			return
		}
		seen[d] = true
		out = append(out, producer(d))
	}
	for _, k := range constants {
		if declared(k, name) {
			add(declaration{name: k.Name, module: k.Module})
		}
	}
	for _, u := range uses {
		if u.Target == name+".mk" {
			add(declaration{name: u.Parent, module: u.Module})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].name != out[j].name {
			return out[i].name < out[j].name
		}
		return out[i].module < out[j].module
	})
	return out, nil
}

// candidateProducers are the producers of the interface the candidate has: those of the base in a module it
// leaves alone, those of a changed module it did not compile, whose content no check can read, and those of
// the modules it compiled.
func (p *producers) candidateProducers(name string, base []producer) []producer {
	var out []producer
	for _, pr := range base {
		ch := p.c.ChangeOfModule(pr.module)
		switch {
		case ch == nil:
			out = append(out, pr)
		case ch.Status == Deleted:
		default:
			if m := p.c.Module(pr.module); m == nil || m.State != Compiled {
				out = append(out, pr)
			}
		}
	}
	for _, m := range p.changed {
		for _, k := range p.candidate[m.Module] {
			if declared(k, name) && k.Conclusion == name {
				out = append(out, producer{name: k.Name, module: k.Module})
			}
		}
		for _, u := range m.Data.Uses {
			if u.Target == name+".mk" && u.Parent != "" && !partOf(u.Parent, name) {
				out = append(out, producer{name: u.Parent, module: m.Module})
			}
		}
	}
	return out
}

// baseBinders are the declarations of the base that bind the interface in a file whose binders P1 reads, by
// declaration, each with the position its source writes the head at.
func (p *producers) baseBinders(ctx context.Context, name string, uses []declindex.Use) (map[declaration]bound, error) {
	at := map[declaration]declindex.Use{}
	var modules []string
	for _, u := range uses {
		if u.Target != name || u.Parent == "" || !p.cfg.Producers.IsBinderFile(p.c.PathOf(u.Module)) {
			continue
		}
		d := declaration{name: u.Parent, module: u.Module}
		if _, ok := at[d]; !ok {
			at[d] = u
			modules = append(modules, u.Module)
		}
	}
	out := map[declaration]bound{}
	if len(at) == 0 {
		return out, nil
	}
	constants, err := p.idx.ConstantsIn(ctx, distinctStrings(modules))
	if err != nil {
		return nil, fmt.Errorf("read the base's binders of %s: %w", name, err)
	}
	for _, k := range constants {
		d := declaration{name: k.Name, module: k.Module}
		u, ok := at[d]
		if !ok || !declared(k, "") || !binds(k, name) {
			continue
		}
		out[d] = bound{declaration: d, path: p.c.PathOf(k.Module), line: u.Line, col: u.Col}
	}
	return out, nil
}

// binders are the declarations that bind the interface in the candidate, in path order: those of the base's
// modules that the candidate leaves alone, and those of the changed binder files it compiled.
func (p *producers) binders(name string, base map[declaration]bound) []bound {
	var out []bound
	for d, b := range base {
		if p.c.ChangeOfModule(d.module) == nil {
			out = append(out, b)
		}
	}
	for _, m := range p.changed {
		if !p.cfg.Producers.IsBinderFile(m.Path) {
			continue
		}
		byName := constantsByName(p.candidate[m.Module])
		for _, d := range m.Data.Decls {
			k, ok := byName[d.Name]
			if !ok || !declared(k, "") || !binds(k, name) {
				continue
			}
			line, col := binderAt(p.c, m, d.Name, name, d.Line)
			out = append(out, bound{declaration: declaration{name: d.Name, module: m.Module}, path: m.Path, line: line, col: col})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		switch {
		case out[i].path != out[j].path:
			return out[i].path < out[j].path
		case out[i].line != out[j].line:
			return out[i].line < out[j].line
		}
		return out[i].name < out[j].name
	})
	return out
}

// declaredIn is the file that declares the interface, for the message: the module the base's uses record for it,
// else the changed module that declares it, else the file its name gives.
func (p *producers) declaredIn(ctx context.Context, name string, uses []declindex.Use) string {
	for _, u := range uses {
		if u.Target == name && u.TargetModule != "" {
			return p.c.PathOf(u.TargetModule)
		}
	}
	if decls, err := p.idx.DeclsNamed(ctx, []string{name}); err == nil && len(decls) > 0 {
		return p.c.PathOf(decls[0].Module)
	}
	for _, m := range p.changed {
		for _, d := range m.Data.Decls {
			if d.Name == name {
				return m.Path
			}
		}
	}
	if cut := strings.LastIndexByte(name, '.'); cut > 0 {
		return p.c.PathOf(name[:cut])
	}
	return p.c.PathOf(name)
}

// distinctStrings is values without repeats, in their first order.
func distinctStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, v := range values {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}
