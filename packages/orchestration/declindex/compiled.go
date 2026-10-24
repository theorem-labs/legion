package declindex

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/sjawhar/legion/orchestration/declindex/confine"
)

// CompiledOptions turn on the compiled part of a refresh: types, compiled statement hashes, atoms and the
// registry's facts, read by the exporter from the .olean files.
type CompiledOptions struct {
	// Exporter reads the modules. When it is Confined (the gate's setting, for proposals), the forms module is
	// compiled in the sandbox too, in a fresh scratch directory below its ScratchDir.
	Exporter *Exporter
	// Forms are the registry's concepts and forms.
	Forms FormsModule
	// Dir holds the compiled forms module of an exporter that is not confined.
	Dir string
}

// compiledDigest names what the compiled part was read with: the exporter and the forms. When it changes, every
// module is read again.
func compiledDigest(co *CompiledOptions, m Manifest, formsSource string) (string, error) {
	j, err := json.Marshal(m.complete())
	if err != nil {
		return "", err
	}
	h := sha256.Sum256([]byte(co.Exporter.Bin + "\x00" + formsSource + "\x00" + string(j)))
	return hex.EncodeToString(h[:]), nil
}

// prepare compiles the forms module and gives the exporter and manifest that read with it.
func (co *CompiledOptions) prepare(ctx context.Context, roots Roots) (*Exporter, Manifest, string, error) {
	src, forms := co.Forms.Source()
	x := *co.Exporter
	m := Manifest{Forms: forms, Types: true, Roots: slices.Sorted(maps.Keys(roots))}
	for _, c := range co.Forms.Concepts {
		m.Concepts = append(m.Concepts, ConstRef{Concept: c.Concept, Const: c.Concept})
	}
	if len(co.Forms.Concepts) > 0 {
		if err := co.Forms.Check(); err != nil {
			return nil, m, "", err
		}
		dir := co.Dir
		if x.Confined {
			// A fresh scratch below the caller's scratch directory, which the caller removes.
			if x.ScratchDir == "" {
				return nil, m, "", errors.New("a confined export needs the exporter's ScratchDir")
			}
			var err error
			if dir, err = confine.NewScratch(x.ScratchDir); err != nil {
				return nil, m, "", err
			}
			if err := CompileConfined(ctx, x.Lean, x.SearchPath, dir, FormsModuleName, []byte(src)); err != nil {
				return nil, m, "", fmt.Errorf("compile the registry's forms: %w", err)
			}
		} else {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, m, "", err
			}
			if err := os.WriteFile(filepath.Join(dir, FormsModuleName+".lean"), []byte(src), 0o644); err != nil {
				return nil, m, "", err
			}
			if err := CompileModule(ctx, x.Lean, x.SearchPath, dir, FormsModuleName); err != nil {
				return nil, m, "", fmt.Errorf("compile the registry's forms: %w", err)
			}
		}
		x.SearchPath = slices.Concat(x.SearchPath, []string{dir})
		m.Imports = []string{FormsModuleName}
	}
	return &x, m, src, nil
}

// Export reads the given compiled modules with the registry's forms: the gate's view of a proposal. extra lists
// further library directories (the proposal's) and roots the project's roots plus the proposal's.
func (co *CompiledOptions) Export(ctx context.Context, roots Roots, modules, extra []string) ([]Constant, error) {
	x, m, _, err := co.prepare(ctx, roots)
	if err != nil {
		return nil, err
	}
	x.SearchPath = slices.Concat(x.SearchPath, extra)
	m.Modules = modules
	return x.Run(ctx, m)
}

// SameCompiled lists the declarations (generated constants counted as their declaration) whose compiled
// statement hash is hash: exact restatements.
func (s *Store) SameCompiled(ctx context.Context, hash string) ([]Found, error) {
	return s.distinctOwners(ctx, `SELECT name FROM consts WHERE cstmt = ? ORDER BY name`, hash)
}

// refreshCompiled re-reads the compiled modules whose .olean changed (all of them when the exporter or the
// forms changed), then the statements that a changed definition reaches, since a definition's facts pass to
// every statement that applies it. It records commit as the commit whose compiled part the index holds.
func (s *Store) refreshCompiled(ctx context.Context, tree Tree, co *CompiledOptions, files []moduleFile, commit string, log io.Writer) (int, int, error) {
	t0 := time.Now()
	x, m, src, err := co.prepare(ctx, tree.Roots)
	if err != nil {
		return 0, 0, err
	}
	bodies, err := s.conceptBodies(ctx, co.Forms.Concepts)
	if err != nil {
		return 0, 0, err
	}
	digest, err := compiledDigest(co, m, src+"\x00"+bodies)
	if err != nil {
		return 0, 0, err
	}
	prev, err := s.Meta(ctx, "compiled")
	if err != nil {
		return 0, 0, err
	}
	full := prev != digest
	old, err := s.storedModules(ctx)
	if err != nil {
		return 0, 0, err
	}
	keys := map[string]string{} // module -> its .olean key now
	var todo []string
	for _, f := range files {
		o, ok := old[f.rel]
		if !ok || f.oleanKey == "" {
			continue
		}
		keys[o.module] = f.oleanKey
		if full || o.oleanKey != f.oleanKey {
			todo = append(todo, o.module)
		}
	}
	if len(todo) == 0 {
		if full {
			// Nothing was read with the new exporter or forms: the compiled part is not complete for commit.
			return 0, 0, nil
		}
		return 0, 0, s.write(ctx, log, func() error { return s.setMeta(ctx, s.db, "compiled_commit", commit) })
	}
	sort.Strings(todo)
	before, err := s.defHashes(ctx, todo)
	if err != nil {
		return 0, 0, err
	}
	m.Modules = todo
	consts, err := x.Run(ctx, m)
	if err != nil {
		return 0, 0, err
	}
	logf(log, "index: %d compiled modules read, %d constants, %s", len(todo), len(consts), time.Since(t0).Round(time.Millisecond))
	var again []string
	if !full {
		again, err = s.reachedModules(changedDefs(before, consts), todo)
		if err != nil {
			return 0, 0, err
		}
		if len(again) > 0 {
			m.Modules = again
			more, err := x.Run(ctx, m)
			if err != nil {
				return 0, 0, err
			}
			consts = append(consts, more...)
			logf(log, "index: %d modules re-matched, reached from changed definitions", len(again))
		}
	}
	// The exporter ran without the write lock; only the write of what it read holds it.
	if err := s.writeTx(ctx, log, func(tx *sql.Tx) error {
		return s.writeCompiled(ctx, tx, todo, again, keys, consts, digest, commit)
	}); err != nil {
		return 0, 0, err
	}
	return len(todo), len(again), nil
}

// conceptBodies names the bodies of the concepts with no written form, which are matched by their declaration's
// body: so the body is part of what the compiled part was read with, by its statement hash (a definition's covers
// its whole text).
func (s *Store) conceptBodies(ctx context.Context, concepts []ConceptForms) (string, error) {
	var bodies strings.Builder
	for _, c := range concepts {
		if len(c.Forms) > 0 {
			continue
		}
		found, err := s.Resolve(ctx, c.Concept)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&bodies, "%s=", c.Concept)
		for _, f := range found {
			if f.Name == c.Concept {
				fmt.Fprintf(&bodies, "%s:%s;", f.Module, f.StmtHash)
			}
		}
		bodies.WriteString("\n")
	}
	return bodies.String(), nil
}

// writeCompiled replaces the compiled rows of the modules read (todo, and again, re-matched) with consts, records
// the .olean key now of each module of todo (keys), and the digest and the commit that the compiled part is of.
func (s *Store) writeCompiled(ctx context.Context, tx *sql.Tx, todo, again []string, keys map[string]string, consts []Constant, digest, commit string) error {
	for _, mod := range slices.Concat(todo, again) {
		if err := deleteCompiled(ctx, tx, mod); err != nil {
			return err
		}
	}
	cb := newBatch(tx, "INSERT OR REPLACE", "consts", 5)
	ab, fb := newBatch(tx, "INSERT", "atoms", 3), newBatch(tx, "INSERT", "facts", 4)
	for _, c := range consts {
		if err := cb.add(ctx, c.Name, c.Module, c.Kind, c.Type, c.Hash); err != nil {
			return err
		}
		for _, a := range c.Atoms {
			if err := ab.add(ctx, c.Name, c.Module, a); err != nil {
				return err
			}
		}
		for _, f := range c.Facts {
			if err := fb.add(ctx, c.Name, c.Module, f.Concept, f.How); err != nil {
				return err
			}
		}
	}
	for _, b := range []*batch{cb, ab, fb} {
		if err := b.flush(ctx); err != nil {
			return err
		}
	}
	for _, mod := range todo {
		if _, err := tx.ExecContext(ctx, `UPDATE modules SET olean_key = ? WHERE module = ?`, keys[mod], mod); err != nil {
			return err
		}
	}
	if err := s.setMeta(ctx, tx, "compiled", digest); err != nil {
		return err
	}
	return s.setMeta(ctx, tx, "compiled_commit", commit)
}

// defHashes are the compiled hashes of the definitions of the modules, before they are read again.
func (s *Store) defHashes(ctx context.Context, modules []string) (map[string]string, error) {
	out := map[string]string{}
	err := eachRowIn(ctx, s.q, `SELECT name, cstmt FROM consts WHERE kind = 'def' AND module IN`, modules, func(r *sql.Rows) error {
		var n, h string
		if err := r.Scan(&n, &h); err != nil {
			return err
		}
		out[n] = h
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// changedDefs are the definitions whose compiled statement is new, gone or different.
func changedDefs(before map[string]string, consts []Constant) []string {
	var out []string
	seen := map[string]bool{}
	for _, c := range consts {
		if c.Kind != "def" {
			continue
		}
		seen[c.Name] = true
		if before[c.Name] != c.Hash {
			out = append(out, c.Name)
		}
	}
	for n := range before {
		if !seen[n] {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// reachedModules are the modules, other than skip, of the statements that a change to defs changes.
func (s *Store) reachedModules(defs, skip []string) ([]string, error) {
	if len(defs) == 0 {
		return nil, nil
	}
	g, err := s.graph()
	if err != nil {
		return nil, err
	}
	done := map[string]bool{}
	for _, m := range skip {
		done[m] = true
	}
	var out []string
	for _, d := range defs {
		changed, _, _ := g.walk(d)
		for r := range changed {
			if !done[r.module] {
				done[r.module] = true
				out = append(out, r.module)
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// Compiled is what the index read about one compiled constant.
type Compiled struct {
	Name   string `json:"name"`
	Module string `json:"module"`
	Kind   string `json:"kind"`
	Type   string `json:"type"`
	// Hash is the compiled statement hash: equal for two declarations whose elaborated statements are equal up to
	// the names of their binders and of the declaration itself.
	Hash  string   `json:"compiled_hash"`
	Atoms []string `json:"atoms,omitempty"`
	Facts []Fact   `json:"facts,omitempty"`
}

// Constant reads one compiled constant.
func (s *Store) Constant(ctx context.Context, name string) (Compiled, error) {
	c := Compiled{Name: name}
	err := s.q.QueryRowContext(ctx, `SELECT module, kind, type, cstmt FROM consts WHERE name = ?`, name).
		Scan(&c.Module, &c.Kind, &c.Type, &c.Hash)
	if err == sql.ErrNoRows {
		return c, &NotFoundError{Name: name}
	}
	if err != nil {
		return c, err
	}
	rows, err := s.q.QueryContext(ctx, `SELECT atom FROM atoms WHERE name = ? ORDER BY atom`, name)
	err = eachRow(rows, err, func(r *sql.Rows) error {
		var a string
		if err := r.Scan(&a); err != nil {
			return err
		}
		c.Atoms = append(c.Atoms, a)
		return nil
	})
	if err != nil {
		return c, err
	}
	rows, err = s.q.QueryContext(ctx, `SELECT concept, how FROM facts WHERE name = ? ORDER BY concept, how`, name)
	err = eachRow(rows, err, func(r *sql.Rows) error {
		var f Fact
		if err := r.Scan(&f.Concept, &f.How); err != nil {
			return err
		}
		c.Facts = append(c.Facts, f)
		return nil
	})
	return c, err
}

// dottedPrefixes yields name, then each shorter prefix that ends before one of its dots, while the prefix is not
// empty: A.B.c, A.B, A.
func dottedPrefixes(name string) iter.Seq[string] {
	return func(yield func(string) bool) {
		for p := name; p != ""; {
			if !yield(p) {
				return
			}
			i := strings.LastIndexByte(p, '.')
			if i < 0 {
				return
			}
			p = p[:i]
		}
	}
}

// owners maps each constant name to the declaration it belongs to: the longest declared name it extends (`X.mk`
// is X's constructor), or the name alone when it extends none. It resolves all the names at once: one query per
// maxInParams candidate names (each name and its dotted prefixes), not one lookup per name. A wide concept has
// tens of thousands of statements.
func (s *Store) owners(ctx context.Context, names []string) (map[string]Found, error) {
	cands := map[string]bool{}
	for _, n := range names {
		for p := range dottedPrefixes(n) {
			cands[p] = true
		}
	}
	found := map[string]Found{}
	err := eachRowIn(ctx, s.q, foundSelect+` WHERE d.name IN`, slices.Sorted(maps.Keys(cands)), func(r *sql.Rows) error {
		f, err := scanFound(r)
		if err != nil {
			return err
		}
		if _, ok := found[f.Name]; !ok {
			found[f.Name] = f
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make(map[string]Found, len(names))
	for _, n := range names {
		out[n] = Found{Name: n}
		for p := range dottedPrefixes(n) {
			if f, ok := found[p]; ok {
				out[n] = f
				break
			}
		}
	}
	return out, nil
}

// distinctOwners lists the declarations that the constants the query names belong to (see owners), each once, in
// the order of its first constant.
func (s *Store) distinctOwners(ctx context.Context, query string, args ...any) ([]Found, error) {
	var names []string
	rows, err := s.q.QueryContext(ctx, query, args...)
	err = eachRow(rows, err, func(r *sql.Rows) error {
		var n string
		if err := r.Scan(&n); err != nil {
			return err
		}
		names = append(names, n)
		return nil
	})
	if err != nil {
		return nil, err
	}
	owner, err := s.owners(ctx, names)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []Found
	for _, n := range names {
		if f := owner[n]; !seen[f.Name] {
			seen[f.Name] = true
			out = append(out, f)
		}
	}
	return out, nil
}

// Stating is one statement that states a registered concept's fact.
type Stating struct {
	Decl   string `json:"declaration"`
	Const  string `json:"constant"`
	Module string `json:"module"`
	How    string `json:"how"`
	At     string `json:"at,omitempty"`
}

// States lists the statements that state a concept's fact, directly, through a definition, or by using it.
func (s *Store) States(ctx context.Context, concept string) ([]Stating, error) {
	type row struct{ name, module, how string }
	var rs []row
	rows, err := s.q.QueryContext(ctx, `SELECT name, module, how FROM facts WHERE concept = ? ORDER BY module, name, how`, concept)
	err = eachRow(rows, err, func(r *sql.Rows) error {
		var x row
		if err := r.Scan(&x.name, &x.module, &x.how); err != nil {
			return err
		}
		rs = append(rs, x)
		return nil
	})
	if err != nil {
		return nil, err
	}
	consts := make([]string, len(rs))
	for i, r := range rs {
		consts[i] = r.name
	}
	owner, err := s.owners(ctx, consts)
	if err != nil {
		return nil, err
	}
	out := make([]Stating, 0, len(rs))
	for _, r := range rs {
		f := owner[r.name]
		st := Stating{Decl: f.Name, Const: r.name, Module: r.module, How: r.how}
		if f.Path != "" {
			st.At = f.At()
		}
		out = append(out, st)
	}
	return out, nil
}

// SameAtoms lists the declarations whose statements share a relation atom with name's statement: who else
// states one of its facts in the same form.
func (s *Store) SameAtoms(ctx context.Context, name string) ([]string, error) {
	found, err := s.distinctOwners(ctx, `SELECT DISTINCT b.name FROM atoms a JOIN atoms b ON b.atom = a.atom
		WHERE a.name = ? AND b.name <> a.name`, name)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, f := range found {
		out = append(out, f.Name)
	}
	sort.Strings(out)
	return out, nil
}
