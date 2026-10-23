package declindex

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// CompiledOptions turn on the compiled part of a refresh: types, compiled statement hashes, atoms and the
// registry's facts, read by the exporter from the .olean files.
type CompiledOptions struct {
	Exporter *Exporter
	// Forms are the registry's concepts and forms.
	Forms FormsModule
	// Dir holds the compiled forms module.
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
	m := Manifest{Forms: forms, Types: true}
	for r := range roots {
		m.Roots = append(m.Roots, r)
	}
	sort.Strings(m.Roots)
	for _, c := range co.Forms.Concepts {
		m.Concepts = append(m.Concepts, ConstRef{Concept: c.Concept, Const: c.Concept})
	}
	if len(co.Forms.Concepts) > 0 {
		if err := co.Forms.Check(); err != nil {
			return nil, m, "", err
		}
		if err := os.MkdirAll(co.Dir, 0o755); err != nil {
			return nil, m, "", err
		}
		if err := os.WriteFile(filepath.Join(co.Dir, FormsModuleName+".lean"), []byte(src), 0o644); err != nil {
			return nil, m, "", err
		}
		if err := CompileModule(ctx, x.Lean, x.SearchPath, co.Dir, FormsModuleName); err != nil {
			return nil, m, "", fmt.Errorf("compile the registry's forms: %w", err)
		}
		x.SearchPath = append(append([]string(nil), x.SearchPath...), co.Dir)
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
	x.SearchPath = append(append([]string(nil), x.SearchPath...), extra...)
	m.Modules = modules
	return x.Run(ctx, m)
}

// SameCompiled lists the declarations (generated constants counted as their declaration) whose compiled
// statement hash is hash: exact restatements.
func (s *Store) SameCompiled(ctx context.Context, hash string) ([]Found, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM consts WHERE cstmt = ? ORDER BY name`, hash)
	if err != nil {
		return nil, err
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return nil, err
		}
		names = append(names, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	o := &owners{s: s, cache: map[string]Found{}}
	if err := o.preload(ctx, names); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []Found
	for _, n := range names {
		f, err := o.of(ctx, n)
		if err != nil {
			return nil, err
		}
		if !seen[f.Name] {
			seen[f.Name] = true
			out = append(out, f)
		}
	}
	return out, nil
}

// refreshCompiled re-reads the compiled modules whose .olean changed (all of them when the exporter or the
// forms changed), then the statements that a changed definition reaches, since a definition's facts pass to
// every statement that applies it.
func (s *Store) refreshCompiled(ctx context.Context, tree Tree, co *CompiledOptions, files []moduleFile, log io.Writer) (int, int, error) {
	t0 := time.Now()
	x, m, src, err := co.prepare(ctx, tree.Roots)
	if err != nil {
		return 0, 0, err
	}
	digest, err := compiledDigest(co, m, src)
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
		return 0, 0, nil
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = tx.Rollback() }()
	for _, mod := range append(append([]string(nil), todo...), again...) {
		if err := deleteCompiled(ctx, tx, mod); err != nil {
			return 0, 0, err
		}
	}
	cb, ab, fb := newBatch(tx, "consts", 5), newBatch(tx, "atoms", 3), newBatch(tx, "facts", 4)
	cb.verb = "INSERT OR REPLACE"
	for _, c := range consts {
		if err := cb.add(ctx, c.Name, c.Module, c.Kind, c.Type, c.Hash); err != nil {
			return 0, 0, err
		}
		for _, a := range c.Atoms {
			if err := ab.add(ctx, c.Name, c.Module, a); err != nil {
				return 0, 0, err
			}
		}
		for _, f := range c.Facts {
			if err := fb.add(ctx, c.Name, c.Module, f.Concept, f.How); err != nil {
				return 0, 0, err
			}
		}
	}
	for _, b := range []*batch{cb, ab, fb} {
		if err := b.flush(ctx); err != nil {
			return 0, 0, err
		}
	}
	for _, mod := range todo {
		if _, err := tx.ExecContext(ctx, `UPDATE modules SET olean_key = ? WHERE module = ?`, keys[mod], mod); err != nil {
			return 0, 0, err
		}
	}
	if err := s.setMeta(ctx, tx, "compiled", digest); err != nil {
		return 0, 0, err
	}
	return len(todo), len(again), tx.Commit()
}

// defHashes are the compiled hashes of the definitions of the modules, before they are read again.
func (s *Store) defHashes(ctx context.Context, modules []string) (map[string]string, error) {
	out := map[string]string{}
	for i := 0; i < len(modules); i += 500 {
		chunk := modules[i:min(i+500, len(modules))]
		rows, err := s.db.QueryContext(ctx, `SELECT name, cstmt FROM consts WHERE kind = 'def' AND module IN (`+
			placeholders(len(chunk))+`)`, anys(chunk)...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var n, h string
			if err := rows.Scan(&n, &h); err != nil {
				rows.Close()
				return nil, err
			}
			out[n] = h
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
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
	err := s.db.QueryRowContext(ctx, `SELECT module, kind, type, cstmt FROM consts WHERE name = ?`, name).
		Scan(&c.Module, &c.Kind, &c.Type, &c.Hash)
	if err == sql.ErrNoRows {
		return c, &NotFoundError{Name: name}
	}
	if err != nil {
		return c, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT atom FROM atoms WHERE name = ? ORDER BY atom`, name)
	if err != nil {
		return c, err
	}
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			rows.Close()
			return c, err
		}
		c.Atoms = append(c.Atoms, a)
	}
	rows.Close()
	rows, err = s.db.QueryContext(ctx, `SELECT concept, how FROM facts WHERE name = ? ORDER BY concept, how`, name)
	if err != nil {
		return c, err
	}
	defer rows.Close()
	for rows.Next() {
		var f Fact
		if err := rows.Scan(&f.Concept, &f.How); err != nil {
			return c, err
		}
		c.Facts = append(c.Facts, f)
	}
	return c, rows.Err()
}

// owners maps constant names to the declarations they belong to: the longest declared name each extends
// (`X.mk` is X's constructor).
type owners struct {
	s     *Store
	cache map[string]Found
}

// preload resolves many constant names at once: one query per 500 candidate names (each name and its dotted
// prefixes), not one lookup per name. A wide concept has tens of thousands of statements.
func (o *owners) preload(ctx context.Context, names []string) error {
	cands := map[string]bool{}
	for _, n := range names {
		for p := n; p != ""; {
			cands[p] = true
			i := strings.LastIndexByte(p, '.')
			if i < 0 {
				break
			}
			p = p[:i]
		}
	}
	list := keys(cands)
	found := map[string]Found{}
	for i := 0; i < len(list); i += 500 {
		chunk := list[i:min(i+500, len(list))]
		rows, err := o.s.db.QueryContext(ctx, `SELECT d.name, d.module, d.kind, d.line, d.stmt_hash, COALESCE(m.rel, '')
			FROM decls d LEFT JOIN modules m ON m.module = d.module WHERE d.name IN (`+placeholders(len(chunk))+`)`, anys(chunk)...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var f Found
			var rel string
			if err := rows.Scan(&f.Name, &f.Module, &f.Kind, &f.Line, &f.StmtHash, &rel); err != nil {
				rows.Close()
				return err
			}
			f.Path = sourcePath(rel, f.Module)
			if _, ok := found[f.Name]; !ok {
				found[f.Name] = f
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	for _, n := range names {
		o.cache[n] = Found{Name: n}
		for p := n; p != ""; {
			if f, ok := found[p]; ok {
				o.cache[n] = f
				break
			}
			i := strings.LastIndexByte(p, '.')
			if i < 0 {
				break
			}
			p = p[:i]
		}
	}
	return nil
}

func (o *owners) of(ctx context.Context, name string) (Found, error) {
	if f, ok := o.cache[name]; ok {
		return f, nil
	}
	for n := name; n != ""; {
		var f Found
		var rel string
		err := o.s.db.QueryRowContext(ctx, `SELECT d.name, d.module, d.kind, d.line, d.stmt_hash, COALESCE(m.rel, '')
			FROM decls d LEFT JOIN modules m ON m.module = d.module WHERE d.name = ? LIMIT 1`, n).
			Scan(&f.Name, &f.Module, &f.Kind, &f.Line, &f.StmtHash, &rel)
		if err == nil {
			f.Path = sourcePath(rel, f.Module)
			o.cache[name] = f
			return f, nil
		}
		if err != sql.ErrNoRows {
			return Found{}, err
		}
		i := strings.LastIndexByte(n, '.')
		if i < 0 {
			break
		}
		n = n[:i]
	}
	f := Found{Name: name}
	o.cache[name] = f
	return f, nil
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
	rows, err := s.db.QueryContext(ctx, `SELECT name, module, how FROM facts WHERE concept = ? ORDER BY module, name, how`, concept)
	if err != nil {
		return nil, err
	}
	type row struct{ name, module, how string }
	var rs []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.name, &r.module, &r.how); err != nil {
			rows.Close()
			return nil, err
		}
		rs = append(rs, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	o := &owners{s: s, cache: map[string]Found{}}
	consts := make([]string, len(rs))
	for i, r := range rs {
		consts[i] = r.name
	}
	if err := o.preload(ctx, consts); err != nil {
		return nil, err
	}
	out := make([]Stating, 0, len(rs))
	for _, r := range rs {
		f, err := o.of(ctx, r.name)
		if err != nil {
			return nil, err
		}
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
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT b.name FROM atoms a JOIN atoms b ON b.atom = a.atom
		WHERE a.name = ? AND b.name <> a.name`, name)
	if err != nil {
		return nil, err
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return nil, err
		}
		names = append(names, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	o := &owners{s: s, cache: map[string]Found{}}
	if err := o.preload(ctx, names); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, n := range names {
		f, err := o.of(ctx, n)
		if err != nil {
			return nil, err
		}
		if !seen[f.Name] {
			seen[f.Name] = true
			out = append(out, f.Name)
		}
	}
	sort.Strings(out)
	return out, nil
}
