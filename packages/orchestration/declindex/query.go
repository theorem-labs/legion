package declindex

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
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
	rows, err := s.q.Query(ctx, foundSelect+` WHERE d.short = $1`, short)
	err = eachRow(rows, err, func(r pgx.Rows) error {
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

// declarationsNamed gives the declarations of each of the full names in names, by name.
func (s *Store) declarationsNamed(ctx context.Context, names []string) (map[string][]Found, error) {
	out := make(map[string][]Found, len(names))
	rows, err := s.q.Query(ctx, foundSelect+` WHERE d.name = ANY($1)`, names)
	err = eachRow(rows, err, func(r pgx.Rows) error {
		f, err := scanFound(r)
		if err != nil {
			return err
		}
		out[f.Name] = append(out[f.Name], f)
		return nil
	})
	if err != nil {
		return nil, err
	}
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
	Module      string      `json:"module"`
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
	// FileKinds are the file-name prefixes that group a user's file in an answer (for example the kinds of file a
	// project's layout names); a file with none of them is "other".
	FileKinds []string
}

type useRow struct {
	module        string
	line, col     int
	parent, class string
}

// usersOf gives the uses of the declaration d and of its generated parts: the constants Lean made from it that its
// module never declares (a structure's fields and constructor, equation lemmas). A use of `h.wp` is recorded as
// `X.wp`; it is a use of X. Each recorded constant counts for the declaration it belongs to (ownerResolver.owner),
// so a use of another module's declaration of d's name is not d's.
func (s *Store) usersOf(ctx context.Context, d declRef) ([]useRow, error) {
	type recorded struct {
		target declRef
		u      useRow
	}
	var rs []recorded
	rows, err := s.q.Query(ctx, `SELECT target, tmodule, module, line, col, parent, cls FROM uses
		WHERE target = $1 OR (target >= $2 AND target < $3)`, d.name, d.name+".", d.name+"/")
	err = eachRow(rows, err, func(r pgx.Rows) error {
		var x recorded
		if err := r.Scan(&x.target.name, &x.target.module, &x.u.module, &x.u.line, &x.u.col, &x.u.parent, &x.u.class); err != nil {
			return err
		}
		rs = append(rs, x)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(rs) == 0 {
		return nil, nil
	}
	targets := map[string]bool{}
	for _, x := range rs {
		targets[x.target.name] = true
	}
	declaring, err := declaringPrefixes(ctx, s.q, slices.Sorted(maps.Keys(targets)))
	if err != nil {
		return nil, err
	}
	owners := newOwnerResolver(ctx, s.q, declaring)
	var out []useRow
	for _, x := range rs {
		owner, err := owners.owner(x.target)
		if err != nil {
			return nil, err
		}
		if owner == d {
			out = append(out, x.u)
		}
	}
	return out, nil
}

// modulePaths maps every module the index read to its source path. A wide declaration's answer touches tens of
// thousands of modules, so the map is read in one query, once for the store (derived); a failed read is not kept.
func (s *Store) modulePaths(ctx context.Context) (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.derived.paths != nil {
		return s.derived.paths, nil
	}
	out := map[string]string{}
	rows, err := s.q.Query(ctx, `SELECT module, rel FROM modules`)
	err = eachRow(rows, err, func(r pgx.Rows) error {
		var m, rel string
		if err := r.Scan(&m, &rel); err != nil {
			return err
		}
		out[m] = sourcePath(rel, m)
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.derived.paths = out
	return out, nil
}

// ModuleAt is the module whose source file is at file, a slash-separated path relative to the tree (a registered
// concept's home file), or empty when the index read no module there.
func (s *Store) ModuleAt(ctx context.Context, file string) (string, error) {
	rel, ok := strings.CutSuffix(path.Clean(file), ".lean")
	if !ok {
		return "", nil
	}
	var m string
	err := s.q.QueryRow(ctx, `SELECT module FROM modules WHERE rel = $1`, rel).Scan(&m)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return m, err
}

// pathOf is the source path of module in paths (modulePaths), or the path of its name for a module the index did
// not read.
func pathOf(paths map[string]string, module string) string {
	if p, ok := paths[module]; ok {
		return p
	}
	return sourcePath("", module)
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

// Answer answers about the declaration d, one that Resolve gave.
func (s *Store) Answer(ctx context.Context, d Found, opt AnswerOptions) (Answer, error) {
	paths, err := s.modulePaths(ctx)
	if err != nil {
		return Answer{}, err
	}
	ref := declRef{d.Module, d.Name}
	a := Answer{Declaration: d.Name, Module: d.Module, Kind: d.Kind, At: d.At(), StmtHash: d.StmtHash}
	uses, err := s.usersOf(ctx, ref)
	if err != nil {
		return a, err
	}
	a.Direct.ByDirectory = map[string]map[string]int{}
	parents := map[declRef]bool{}
	type line struct {
		path string
		u    useRow
	}
	var lines []line
	for _, u := range uses {
		if (declRef{u.module, u.parent}) == ref {
			continue
		}
		p := pathOf(paths, u.module)
		a.Direct.Uses++
		if u.parent != "" {
			parents[declRef{u.module, u.parent}] = true
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
		slices.SortStableFunc(lines, func(x, y line) int {
			return cmp.Or(strings.Compare(x.path, y.path), cmp.Compare(x.u.line, y.u.line))
		})
		for _, l := range lines {
			parent := l.u.parent
			if parent == "" {
				parent = "(top level)"
			}
			a.Lines = append(a.Lines, fmt.Sprintf("%s:%d:%d in %s [%s]", l.path, l.u.line, l.u.col, parent, l.u.class))
		}
	}
	a.Reach, err = s.reach(ctx, ref, opt.Why)
	return a, err
}

// NotFoundError says that no declaration has the name.
type NotFoundError struct{ Name string }

func (e *NotFoundError) Error() string { return "no declaration " + e.Name + " in the index" }
