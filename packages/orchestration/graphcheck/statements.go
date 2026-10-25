package graphcheck

import (
	"context"
	"fmt"
	"sort"

	"github.com/sjawhar/legion/orchestration/declindex"
	"github.com/sjawhar/legion/orchestration/landing"
	"github.com/sjawhar/legion/orchestration/landing/blame"
)

// Statements runs S2 (README.md, "The rules"): a declaration that the candidate adds, and whose compiled statement
// equals another declaration's of the candidate, is flagged.
func Statements(ctx context.Context, c *Candidate, idx *declindex.Store, _ Config) ([]landing.Finding, error) {
	out, err := restatements(ctx, c, idx)
	if err != nil {
		return nil, err
	}
	sortFindings(out)
	return out, nil
}

// stated is one declaration with its compiled statement hash (declindex.Constant.Hash).
type stated struct {
	path, name string
	line       int
	hash       string
	isNew      bool
}

// restatements runs S2. A declaration of a compiled added or modified module is new when the base's build of that
// module does not declare its name. A new declaration restates another when their compiled statement hashes are
// equal: another declaration of a compiled changed module, from the exporter's constants, or a declaration of an
// unchanged module, from the index's compiled part. The index's declarations of a changed module are the base's,
// which the candidate supersedes, so they do not count.
func restatements(ctx context.Context, c *Candidate, idx *declindex.Store) ([]landing.Finding, error) {
	all, err := compiledStatements(ctx, c)
	if err != nil {
		return nil, err
	}
	var hashes []string
	seen := map[string]bool{}
	for _, x := range all {
		if x.isNew && !seen[x.hash] {
			seen[x.hash] = true
			hashes = append(hashes, x.hash)
		}
	}
	if len(hashes) == 0 {
		return nil, nil
	}
	if _, err := idx.CompiledCommit(ctx, true); err != nil {
		return nil, unable("S2 reads the index's compiled statement hashes: %v", err)
	}
	for _, hash := range hashes {
		found, err := idx.SameCompiled(ctx, hash)
		if err != nil {
			return nil, err
		}
		for _, f := range found {
			// A constant that belongs to no declaration has no place to name.
			if f.Path == "" || c.ChangeOfModule(f.Module) != nil {
				continue
			}
			name, _ := unmangle(f.Name, f.Module)
			all = append(all, stated{path: f.Path, name: name, line: f.Line, hash: hash})
		}
	}
	byHash := map[string][]stated{}
	for _, x := range all {
		byHash[x.hash] = append(byHash[x.hash], x)
	}
	var out []landing.Finding
	for hash, xs := range byHash {
		sort.Slice(xs, func(i, j int) bool {
			if xs[i].path != xs[j].path {
				return xs[i].path < xs[j].path
			}
			if xs[i].line != xs[j].line {
				return xs[i].line < xs[j].line
			}
			return xs[i].name < xs[j].name
		})
		byHash[hash] = xs
	}
	for _, x := range all {
		if !x.isNew {
			continue
		}
		for _, other := range byHash[x.hash] {
			if other == x {
				continue
			}
			out = append(out, landing.Finding{Path: x.path, Line: x.line, Col: declarationColumn(c.Text(x.path), x.line),
				Verdict: landing.VerdictFlag, Kind: blame.Added, Declaration: x.name,
				Reason: fmt.Sprintf("S2: restates %s (%s:%d); a concept proposal goes to the similarity gate first", other.name, other.path, other.line)})
			break
		}
	}
	return out, nil
}

// compiledStatements lists the declarations of every compiled changed module with their compiled statement hashes,
// each marked new when the base's build of its module does not declare its name. A declaration is one that the
// compiled module's reference file records; its constant is the exporter's constant of that name. A constructor is
// part of its inductive, whose hash reads the constructor's type, so it is not a declaration of its own here.
func compiledStatements(ctx context.Context, c *Candidate) ([]stated, error) {
	candidate, base, err := c.Constants(ctx)
	if err != nil {
		return nil, err
	}
	var out []stated
	for _, ch := range c.Changes {
		m := c.Module(ch.Module)
		if m == nil || m.State != Compiled {
			continue
		}
		module := normalName(ch.Module)
		before := map[string]bool{}
		for _, k := range base[module] {
			name, _ := unmangle(k.Name, k.Module)
			before[name] = true
		}
		constants := map[string]declindex.Constant{}
		for _, k := range candidate[module] {
			name, _ := unmangle(k.Name, k.Module)
			constants[name] = k
		}
		for _, d := range m.Data.Decls {
			name, _ := unmangle(d.Name, m.Data.Module)
			k, ok := constants[name]
			if !ok || k.Kind == "ctor" || k.Hash == "" {
				continue
			}
			out = append(out, stated{path: ch.Path, name: name, line: d.Line, hash: k.Hash, isNew: !before[name]})
		}
	}
	return out, nil
}
