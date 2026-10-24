package declindex

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// Found is one declaration that a name resolves to.
type Found struct {
	Name     string `json:"name"`
	Module   string `json:"module"`
	Kind     string `json:"kind"`
	Line     int    `json:"line"`
	Path     string `json:"path"`
	StmtHash string `json:"stmt_hash"`
}

// At is the declaration's place as path:line.
func (f Found) At() string { return f.Path + ":" + strconv.Itoa(f.Line) }

// Resolve finds the declarations named q: its full name, or any dotted suffix of one (Frame, Gold.Frame). A name
// declared in two namespaces gives both.
func (s *Store) Resolve(ctx context.Context, q string) ([]Found, error) {
	short := q[strings.LastIndexByte(q, '.')+1:]
	var out []Found
	rows, err := s.q.QueryContext(ctx, foundSelect+` WHERE d.short = ?`, short)
	err = eachRow(rows, err, func(r *sql.Rows) error {
		f, err := scanFound(r)
		if err != nil {
			return err
		}
		if f.Name != q && !strings.HasSuffix(f.Name, "."+q) {
			return nil
		}
		out = append(out, f)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Module < out[j].Module
	})
	return out, nil
}

func sourcePath(rel, module string) string {
	if rel == "" {
		rel = strings.ReplaceAll(module, ".", "/")
	}
	return rel + ".lean"
}

// DirectUsers are the declarations that mention a declaration or one of its generated parts.
type DirectUsers struct {
	Uses         int `json:"uses"`
	Declarations int `json:"declarations"`
	Directories  int `json:"directories"`
	StmtUses     int `json:"stmt_uses"`
	ProofUses    int `json:"proof_uses"`
	// ByDirectory counts uses by the user's directory and "<file kind> <class>".
	ByDirectory map[string]map[string]int `json:"by_directory"`
}

// ReachCounts size what a change to a declaration's statement reaches: the statements whose meaning changes with
// it (users that mention it in their statement, and definitions that use it anywhere, followed transitively),
// and the proofs at the edge of that set.
type ReachCounts struct {
	StatementsChanged    int `json:"statements_changed"`
	StatementDirectories int `json:"statement_directories"`
	ProofsToRedo         int `json:"proofs_to_redo"`
	ProofDirectories     int `json:"proof_directories"`
	AllDirectories       int `json:"all_directories"`
}

// Reach is ReachCounts and, when asked, one chain of declarations to each directory reached.
type Reach struct {
	ReachCounts
	Why map[string]string `json:"why,omitempty"`
}

// Answer is the index's answer about one declaration: who uses it and how far a change to it reaches.
type Answer struct {
	Declaration string      `json:"declaration"`
	Kind        string      `json:"kind"`
	At          string      `json:"at"`
	StmtHash    string      `json:"stmt_hash"`
	Direct      DirectUsers `json:"direct"`
	Reach       Reach       `json:"reach"`
	// Lines lists every direct use as `path:line:col in <declaration> [class]`.
	Lines []string `json:"lines,omitempty"`
}

// AnswerOptions select the optional parts of an Answer.
type AnswerOptions struct {
	Why   bool
	Lines bool
	// FileKinds group users' files (see Tree.FileKinds).
	FileKinds []string
}

type useRow struct {
	target, module string
	line, col      int
	parent, class  string
}

func (s *Store) declared(ctx context.Context, q querier, names []string) (map[string]bool, error) {
	out := map[string]bool{}
	err := eachRowIn(ctx, q, `SELECT name FROM decls WHERE name IN`, names, func(r *sql.Rows) error {
		var n string
		if err := r.Scan(&n); err != nil {
			return err
		}
		out[n] = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// usersOf gives the uses of name and of its generated parts: names Lean made from the declaration that the
// source never declares (a structure's fields and constructor, equation lemmas). A use of `h.wp` is recorded as
// `X.wp`; it is a use of X.
func (s *Store) usersOf(ctx context.Context, name string) ([]useRow, error) {
	var cands []string
	rows, err := s.q.QueryContext(ctx, `SELECT DISTINCT target FROM uses WHERE target >= ? AND target < ?`, name+".", name+"/")
	err = eachRow(rows, err, func(r *sql.Rows) error {
		var t string
		if err := r.Scan(&t); err != nil {
			return err
		}
		cands = append(cands, t)
		return nil
	})
	if err != nil {
		return nil, err
	}
	declared, err := s.declared(ctx, s.q, cands)
	if err != nil {
		return nil, err
	}
	keys := []string{name}
	for _, c := range cands {
		if !declared[c] {
			keys = append(keys, c)
		}
	}
	var out []useRow
	err = eachRowIn(ctx, s.q, `SELECT module, line, col, parent, cls FROM uses WHERE target IN`, keys, func(r *sql.Rows) error {
		u := useRow{target: name}
		if err := r.Scan(&u.module, &u.line, &u.col, &u.parent, &u.class); err != nil {
			return err
		}
		out = append(out, u)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// paths maps modules to their source paths. A wide declaration's answer touches tens of thousands of modules, so
// the map is read in one query, once per answer.
type paths struct {
	s     *Store
	cache map[string]string
}

func (p *paths) of(ctx context.Context, module string) (string, error) {
	if p.cache == nil {
		cache := map[string]string{}
		rows, err := p.s.q.QueryContext(ctx, `SELECT module, rel FROM modules`)
		err = eachRow(rows, err, func(r *sql.Rows) error {
			var m, rel string
			if err := r.Scan(&m, &rel); err != nil {
				return err
			}
			cache[m] = sourcePath(rel, m)
			return nil
		})
		if err != nil {
			return "", err
		}
		p.cache = cache
	}
	if v, ok := p.cache[module]; ok {
		return v, nil
	}
	return sourcePath("", module), nil
}

func fileKind(p string, kinds []string) string {
	b := path.Base(p)
	for _, k := range kinds {
		if strings.HasPrefix(b, k) {
			return k
		}
	}
	return "other"
}

func dirOf(p string) string {
	d := path.Dir(p)
	if d == "." {
		return ""
	}
	return d
}

// Answer answers about the declaration with exactly this name.
func (s *Store) Answer(ctx context.Context, name string, opt AnswerOptions) (Answer, error) {
	found, err := s.Resolve(ctx, name)
	if err != nil {
		return Answer{}, err
	}
	var decl *Found
	for i := range found {
		if found[i].Name == name {
			decl = &found[i]
			break
		}
	}
	if decl == nil {
		return Answer{}, &NotFoundError{Name: name}
	}
	ps := &paths{s: s}
	a := Answer{Declaration: decl.Name, Kind: decl.Kind, At: decl.At(), StmtHash: decl.StmtHash}
	uses, err := s.usersOf(ctx, name)
	if err != nil {
		return a, err
	}
	a.Direct.ByDirectory = map[string]map[string]int{}
	parents := map[string]bool{}
	type line struct {
		path string
		u    useRow
	}
	var lines []line
	for _, u := range uses {
		if u.parent == name {
			continue
		}
		p, err := ps.of(ctx, u.module)
		if err != nil {
			return a, err
		}
		a.Direct.Uses++
		if u.parent != "" {
			parents[u.parent] = true
		}
		switch u.class {
		case "stmt":
			a.Direct.StmtUses++
		case "proof":
			a.Direct.ProofUses++
		}
		d := dirOf(p)
		if a.Direct.ByDirectory[d] == nil {
			a.Direct.ByDirectory[d] = map[string]int{}
		}
		a.Direct.ByDirectory[d][fileKind(p, opt.FileKinds)+" "+u.class]++
		if opt.Lines {
			lines = append(lines, line{p, u})
		}
	}
	a.Direct.Declarations = len(parents)
	a.Direct.Directories = len(a.Direct.ByDirectory)
	if opt.Lines {
		sort.SliceStable(lines, func(i, j int) bool {
			if lines[i].path != lines[j].path {
				return lines[i].path < lines[j].path
			}
			return lines[i].u.line < lines[j].u.line
		})
		for _, l := range lines {
			parent := l.u.parent
			if parent == "" {
				parent = "(top level)"
			}
			a.Lines = append(a.Lines, fmt.Sprintf("%s:%d:%d in %s [%s]", l.path, l.u.line, l.u.col, parent, l.u.class))
		}
	}
	a.Reach, err = s.reach(ctx, ps, name, opt.Why)
	return a, err
}

// NotFoundError says that no declaration has the name.
type NotFoundError struct{ Name string }

func (e *NotFoundError) Error() string { return "no declaration " + e.Name + " in the index" }

// propagates says whether an edge passes a change on: a use in a statement, or any use by a definition (defKinds).
var propagates = `(e.stmt = 1 OR e.kind IN ('` + strings.Join(defKinds, `', '`) + `'))`

// reach computes the statements a change to root's statement changes, and the proofs at their edge, on the
// declaration graph that the last refresh wrote. A declaration is its name and the module of its use: Lean names
// every `example` `_example`, so one name can stand for a declaration of each of many modules.
func (s *Store) reach(ctx context.Context, ps *paths, root string, why bool) (Reach, error) {
	g, err := s.graph()
	if err != nil {
		return Reach{}, err
	}
	changed, reprove, via := g.walk(root)
	cdirs, rdirs := map[string][]string{}, map[string][]string{}
	for d := range changed {
		p, err := ps.of(ctx, d.module)
		if err != nil {
			return Reach{}, err
		}
		cdirs[dirOf(p)] = append(cdirs[dirOf(p)], d.name)
	}
	for d := range reprove {
		p, err := ps.of(ctx, d.module)
		if err != nil {
			return Reach{}, err
		}
		rdirs[dirOf(p)] = append(rdirs[dirOf(p)], d.name)
	}
	all := map[string]bool{}
	for d := range cdirs {
		all[d] = true
	}
	for d := range rdirs {
		all[d] = true
	}
	r := Reach{ReachCounts: ReachCounts{
		StatementsChanged: len(changed), StatementDirectories: len(cdirs),
		ProofsToRedo: len(reprove), ProofDirectories: len(rdirs), AllDirectories: len(all),
	}}
	if !why {
		return r, nil
	}
	r.Why = map[string]string{}
	short := root[strings.LastIndexByte(root, '.')+1:]
	for d := range all {
		ds := cdirs[d]
		if ds == nil {
			ds = rdirs[d]
		}
		sort.Strings(ds)
		chain := []string{ds[0]}
		for len(chain) < 50 {
			v, ok := via[chain[len(chain)-1]]
			if !ok || v == root {
				break
			}
			chain = append(chain, v)
		}
		slices.Reverse(chain)
		r.Why[d] = short + " -> " + strings.Join(chain, " -> ")
	}
	return r, nil
}

// graph loads the declaration graph once per open index: for a store opened read-only, from the graph file that was
// in place when its snapshot began, which a later refresh's replacement does not change.
func (s *Store) graph() (*graph, error) {
	s.graphOnce.Do(func() {
		var data []byte
		switch {
		case s.snapshot == nil:
			data, s.graphErr = os.ReadFile(s.path + ".graph")
		case s.graphFile == nil:
			s.graphErr = fmt.Errorf("%s.graph: %w", s.path, fs.ErrNotExist)
		default:
			data, s.graphErr = io.ReadAll(io.NewSectionReader(s.graphFile, 0, math.MaxInt64))
		}
		if s.graphErr == nil {
			s.g, s.graphErr = parseGraph(s.path+".graph", data)
		}
		if s.graphErr == nil {
			if want, err := s.Meta(context.Background(), "graph"); err != nil {
				s.graphErr = err
			} else if s.g.token != want {
				s.graphErr = fmt.Errorf("the graph file is of generation %q, the index of %q", s.g.token, want)
			}
		}
		if s.graphErr != nil {
			s.g = nil
			s.graphErr = fmt.Errorf("the declaration graph is missing, unreadable or out of date: run declindex refresh: %w", s.graphErr)
		}
	})
	return s.g, s.graphErr
}
