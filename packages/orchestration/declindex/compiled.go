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

// compiledDigest names what the compiled part was read with: the exporter's version (not its path, which differs
// between the paths that name one index) and the forms, their text and their import closure (formsClosure). When it
// changes, every module is read again.
func compiledDigest(co *CompiledOptions, m Manifest, formsSource string) (string, error) {
	j, err := json.Marshal(m.complete())
	if err != nil {
		return "", err
	}
	h := sha256.Sum256([]byte(co.Exporter.Version + "\x00" + formsSource + "\x00" + string(j)))
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

// Export reads the given compiled modules with the registry's forms: the gate's view of a proposal or of a landed
// module. extra lists further library directories (the proposal's), roots the project's roots plus the proposal's,
// and home, when set, the module that the hashes count the modules' constants as declared in (Manifest.Home).
func (co *CompiledOptions) Export(ctx context.Context, roots Roots, modules, extra []string, home string) ([]Constant, error) {
	x, m, _, err := co.prepare(ctx, roots)
	if err != nil {
		return nil, err
	}
	x.SearchPath = slices.Concat(x.SearchPath, extra)
	m.Modules, m.Home = modules, home
	return x.Run(ctx, m)
}

// SameCompiled lists the declarations (generated constants counted as their declaration) whose compiled
// statement hash is hash: exact restatements.
func (s *Store) SameCompiled(ctx context.Context, hash string) ([]Found, error) {
	return s.distinctOwners(ctx, `SELECT name, module FROM consts WHERE cstmt = ? ORDER BY name, module`, hash)
}

// FormsClash is why the compiled step reads Module without the registry's forms: its imports declare Name in
// DeclaredIn, and the forms' imports declare it in FormsDeclaredIn, and the two sets may not load together in one
// environment (exportGroups): they share no declaration, or they hold two axioms between them and differ. Lean
// imports a second declaration of a name only when one of the two subsumes the other (two theorems with one
// statement), which the index cannot know before it reads them, so the module states no fact of a written form. It
// is a defect of the tree, which declares one name in two modules.
type FormsClash struct {
	Module          string   `json:"module"`
	Name            string   `json:"name"`
	DeclaredIn      []string `json:"declared_in"`
	FormsDeclaredIn []string `json:"forms_declared_in"`
}

// WithoutForms lists, by module and name, the clashes of the modules that the index's compiled part read without the
// registry's forms: none when every module was read with them.
func (s *Store) WithoutForms(ctx context.Context) ([]FormsClash, error) {
	out := []FormsClash{}
	rows, err := s.q.QueryContext(ctx, `SELECT module, name, declarer, forms FROM without_forms ORDER BY module, name, declarer`)
	err = eachRow(rows, err, func(r *sql.Rows) error {
		var module, name, declarer string
		var forms bool
		if err := r.Scan(&module, &name, &declarer, &forms); err != nil {
			return err
		}
		if n := len(out); n == 0 || out[n-1].Module != module || out[n-1].Name != name {
			out = append(out, FormsClash{Module: module, Name: name})
		}
		c := &out[len(out)-1]
		if forms {
			c.FormsDeclaredIn = append(c.FormsDeclaredIn, declarer)
		} else {
			c.DeclaredIn = append(c.DeclaredIn, declarer)
		}
		return nil
	})
	return out, err
}

// ClashModules counts the modules of clashes, which are sorted by module.
func ClashModules(clashes []FormsClash) int {
	n := 0
	for i, c := range clashes {
		if i == 0 || clashes[i-1].Module != c.Module {
			n++
		}
	}
	return n
}

// refreshCompiled re-reads the compiled modules whose .olean changed (all of them when the exporter, the forms or
// the build of a module in the forms' import closure changed), then the statements that a changed definition
// reaches, since a definition's facts pass to every statement that applies it. It records commit as the commit
// whose compiled part the index holds.
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
	old, err := s.storedModules(ctx)
	if err != nil {
		return 0, 0, err
	}
	keys := map[string]string{} // module -> its .olean key now
	for _, f := range files {
		if o, ok := old[f.rel]; ok && f.oleanKey != "" {
			keys[o.module] = f.oleanKey
		}
	}
	var closure string
	if len(m.Imports) > 0 {
		if closure, err = s.formsClosure(ctx, co.Forms.Imports, keys); err != nil {
			return 0, 0, err
		}
	}
	digest, err := compiledDigest(co, m, src+"\x00"+bodies+"\x00"+closure)
	if err != nil {
		return 0, 0, err
	}
	prev, err := s.Meta(ctx, "compiled")
	if err != nil {
		return 0, 0, err
	}
	full := prev != digest
	var todo []string
	reading := map[string]bool{}
	for _, f := range files {
		o, ok := old[f.rel]
		if !ok || f.oleanKey == "" {
			continue
		}
		if full || o.oleanKey != f.oleanKey {
			todo = append(todo, o.module)
			reading[o.module] = true
		}
	}
	// A module read without the forms is read again each time: the clash that kept it from them can leave the
	// forms' imports while the module itself does not change.
	clashed, err := s.WithoutForms(ctx)
	if err != nil {
		return 0, 0, err
	}
	for _, c := range clashed {
		if _, ok := keys[c.Module]; ok && !reading[c.Module] {
			todo = append(todo, c.Module)
			reading[c.Module] = true
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
	runs, withoutForms, err := s.exportRuns(ctx, x, m, co.Forms.Imports, todo, log)
	if err != nil {
		return 0, 0, err
	}
	consts, err := x.RunEach(ctx, runs)
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
			runs, more, err := s.exportRuns(ctx, x, m, co.Forms.Imports, again, log)
			if err != nil {
				return 0, 0, err
			}
			withoutForms = append(withoutForms, more...)
			read, err := x.RunEach(ctx, runs)
			if err != nil {
				return 0, 0, err
			}
			consts = append(consts, read...)
			logf(log, "index: %d modules re-matched, reached from changed definitions", len(again))
		}
	}
	// The exporter ran without the write lock; only the write of what it read holds it.
	if err := s.writeTx(ctx, log, func(tx *sql.Tx) error {
		return s.writeCompiled(ctx, tx, compiledRead{read: todo, rematched: again, oleanKeys: keys, consts: consts,
			withoutForms: withoutForms, digest: digest, commit: commit})
	}); err != nil {
		return 0, 0, err
	}
	return len(todo), len(again), nil
}

// formsClosure names the forms' resolved import closure: each module that the forms' imports import, directly or
// not, with the key of its build now (keys; none for a module outside the library directory). What a module states
// of a written form, and whether it loads with the forms at all, depends on every one of them, not only on the
// forms' text: a module of that closure that imports another module, or is built anew, changes it.
func (s *Store) formsClosure(ctx context.Context, forms []string, keys map[string]string) (string, error) {
	g, err := loadImports(ctx, s.q)
	if err != nil {
		return "", err
	}
	all := map[string]bool{}
	for _, f := range forms {
		maps.Copy(all, g.closure(f))
	}
	var b strings.Builder
	for _, mod := range slices.Sorted(maps.Keys(all)) {
		fmt.Fprintf(&b, "%s=%s\n", mod, keys[mod])
	}
	return b.String(), nil
}

// exportRuns gives the exporter runs that read modules with the manifest m, whose forms module imports forms: one
// run for each group of modules that one Lean environment can import with those modules (exportGroups). A module
// whose imports and the forms' imports hold declarations of one name that share none is read without the forms
// module, keeping only the concepts matched by their own declaration whose name no two modules declare: no
// environment is known to hold both, so it states no fact of a written form, and its environment's declaration of a
// concept's name can be another module's. clashes are those modules' clashes (FormsClash).
func (s *Store) exportRuns(ctx context.Context, x *Exporter, m Manifest, forms, modules []string, log io.Writer) (runs []Manifest, clashes []FormsClash, err error) {
	var base []string
	if len(m.Imports) > 0 {
		base = forms
	}
	loads := func(ctx context.Context, pair []string) (bool, error) { return x.loadsTogether(ctx, m.Roots, pair) }
	plan, err := exportGroups(ctx, s.q, modules, base, loads)
	if err != nil {
		return nil, nil, err
	}
	if plan.checks > 0 {
		logf(log, "index: %d pairs of modules that declare one name checked with Lean, %d import together", plan.checks, plan.loaded)
	}
	for _, g := range plan.groups {
		mm := m
		mm.Modules = g
		runs = append(runs, mm)
	}
	own := func(refs []ConstRef) []ConstRef {
		var out []ConstRef
		for _, r := range refs {
			if r.Const == r.Concept && !plan.clashing[r.Concept] {
				out = append(out, r)
			}
		}
		return out
	}
	for _, g := range plan.apart {
		mm := m
		mm.Modules, mm.Imports, mm.Forms, mm.Concepts = g, nil, own(m.Forms), own(m.Concepts)
		runs = append(runs, mm)
	}
	return runs, plan.clashes, nil
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

// compiledRead is what a compiled step read, for writeCompiled.
type compiledRead struct {
	read      []string          // the modules read, whose .olean key is recorded
	rematched []string          // the modules read again because a definition they use changed
	oleanKeys map[string]string // the .olean key now of each module of read
	consts    []Constant
	// withoutForms are the clashes of the modules of read and rematched that were read without the forms.
	withoutForms []FormsClash
	digest       string // what the compiled part was read with
	commit       string // the commit the compiled part is of
}

// writeCompiled replaces the compiled rows of the modules read (read, and rematched) with consts and their clashes
// with the forms, records the .olean key now of each module of read, and the digest and the commit that the
// compiled part is of.
func (s *Store) writeCompiled(ctx context.Context, tx *sql.Tx, w compiledRead) error {
	for _, mod := range slices.Concat(w.read, w.rematched) {
		if err := deleteCompiled(ctx, tx, mod); err != nil {
			return err
		}
	}
	cb := newBatch(tx, "INSERT OR REPLACE", "consts", 5)
	ab, fb := newBatch(tx, "INSERT", "atoms", 3), newBatch(tx, "INSERT", "facts", 4)
	for _, c := range w.consts {
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
	wb := newBatch(tx, "INSERT OR REPLACE", "without_forms", 4)
	for _, c := range w.withoutForms {
		for _, d := range c.DeclaredIn {
			if err := wb.add(ctx, c.Module, c.Name, d, false); err != nil {
				return err
			}
		}
		for _, d := range c.FormsDeclaredIn {
			if err := wb.add(ctx, c.Module, c.Name, d, true); err != nil {
				return err
			}
		}
	}
	for _, b := range []*batch{cb, ab, fb, wb} {
		if err := b.flush(ctx); err != nil {
			return err
		}
	}
	for _, mod := range w.read {
		if _, err := tx.ExecContext(ctx, `UPDATE modules SET olean_key = ? WHERE module = ?`, w.oleanKeys[mod], mod); err != nil {
			return err
		}
	}
	if err := s.setMeta(ctx, tx, "compiled", w.digest); err != nil {
		return err
	}
	return s.setMeta(ctx, tx, "compiled_commit", w.commit)
}

// defHashes are the compiled hashes of the definitions of the modules, before they are read again.
func (s *Store) defHashes(ctx context.Context, modules []string) (map[declRef]string, error) {
	out := map[declRef]string{}
	err := eachRowIn(ctx, s.q, `SELECT name, module, cstmt FROM consts WHERE kind = 'def' AND module IN`, modules, func(r *sql.Rows) error {
		var d declRef
		var h string
		if err := r.Scan(&d.name, &d.module, &h); err != nil {
			return err
		}
		out[d] = h
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// changedDefs are the definitions whose compiled statement is new, gone or different.
func changedDefs(before map[declRef]string, consts []Constant) []declRef {
	var out []declRef
	seen := map[declRef]bool{}
	for _, c := range consts {
		if c.Kind != "def" {
			continue
		}
		d := declRef{c.Module, c.Name}
		seen[d] = true
		if before[d] != c.Hash {
			out = append(out, d)
		}
	}
	for d := range before {
		if !seen[d] {
			out = append(out, d)
		}
	}
	slices.SortFunc(out, declRef.compare)
	return out
}

// Compiled is what the index read about one compiled constant.
type Compiled struct {
	Name   string `json:"name"`
	Module string `json:"module"`
	Kind   string `json:"kind"`
	Type   string `json:"type"`
	// Hash is the compiled statement hash: equal for two declarations whose elaborated statements are equal up to
	// the names of their binders and of the declaration itself, and up to how a numeral of core Lean's Nat and
	// BitVec instances is written. Any other numeral counts with its type and instance, which give its value. Each
	// constant that a statement names counts with the module that declares it.
	Hash  string   `json:"compiled_hash"`
	Atoms []string `json:"atoms,omitempty"`
	Facts []Fact   `json:"facts,omitempty"`
}

// Constant reads the compiled constant name of module.
func (s *Store) Constant(ctx context.Context, name, module string) (Compiled, error) {
	c := Compiled{Name: name, Module: module}
	err := s.q.QueryRowContext(ctx, `SELECT kind, type, cstmt FROM consts WHERE name = ? AND module = ?`, name, module).
		Scan(&c.Kind, &c.Type, &c.Hash)
	if err == sql.ErrNoRows {
		return c, &NotFoundError{Name: name}
	}
	if err != nil {
		return c, err
	}
	rows, err := s.q.QueryContext(ctx, `SELECT atom FROM atoms WHERE name = ? AND module = ? ORDER BY atom`, name, module)
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
	rows, err = s.q.QueryContext(ctx, `SELECT concept, how FROM facts WHERE name = ? AND module = ? ORDER BY concept, how`, name, module)
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

// DeclarationParts are the names of the compiled constants of module that belong to one of the declarations of
// names in module: each declaration's own, and those that Lean generated from it (a structure's constructor and
// fields), not another declaration in its namespace. It is empty when the index read module without its compiled
// part.
func (s *Store) DeclarationParts(ctx context.Context, module string, names []string) ([]string, error) {
	decls := map[declRef]bool{}
	seen := map[string]bool{}
	var consts []declRef
	for _, n := range names {
		decls[declRef{module, n}] = true
		rows, err := s.q.QueryContext(ctx, `SELECT name FROM consts WHERE module = ? AND
			(name = ? OR (name >= ? AND name < ?)) ORDER BY name`, module, n, n+".", n+"/")
		err = eachRow(rows, err, func(r *sql.Rows) error {
			c := declRef{module: module}
			if err := r.Scan(&c.name); err != nil {
				return err
			}
			if !seen[c.name] {
				seen[c.name] = true
				consts = append(consts, c)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	owner, err := s.owners(ctx, consts)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, c := range consts {
		if o := owner[c]; decls[declRef{o.Module, o.Name}] {
			out = append(out, c.name)
		}
	}
	return out, nil
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

// owners maps each compiled constant to the declaration it belongs to (ownerResolver.owner: `X.mk` is X's
// constructor), or to the constant itself, with no place, when it belongs to none. It resolves all the constants at
// once: one query per maxInParams candidate names (each name and its dotted prefixes), not one lookup per constant.
// A wide concept has tens of thousands of statements.
func (s *Store) owners(ctx context.Context, consts []declRef) (map[declRef]Found, error) {
	names := make([]string, len(consts))
	for i, c := range consts {
		names[i] = c.name
	}
	declaring, err := declaringPrefixes(ctx, s.q, names)
	if err != nil {
		return nil, err
	}
	resolver := newOwnerResolver(ctx, s.q, declaring)
	ref := make(map[declRef]declRef, len(consts))
	declared := map[string]bool{}
	for _, c := range consts {
		o, err := resolver.owner(c)
		if err != nil {
			return nil, err
		}
		ref[c] = o
		if slices.Contains(declaring[o.name], o.module) {
			declared[o.name] = true
		}
	}
	found := map[declRef]Found{}
	err = eachRowIn(ctx, s.q, foundSelect+` WHERE d.name IN`, slices.Sorted(maps.Keys(declared)), func(r *sql.Rows) error {
		f, err := scanFound(r)
		if err != nil {
			return err
		}
		if d := (declRef{f.Module, f.Name}); found[d].Name == "" {
			found[d] = f
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make(map[declRef]Found, len(consts))
	for _, c := range consts {
		o := ref[c]
		if f, ok := found[o]; ok {
			out[c] = f
		} else {
			out[c] = Found{Name: o.name, Module: o.module}
		}
	}
	return out, nil
}

// distinctOwners lists the declarations that the constants the query names (by name and module) belong to (see
// owners), each once, in the order of its first constant.
func (s *Store) distinctOwners(ctx context.Context, query string, args ...any) ([]Found, error) {
	var consts []declRef
	rows, err := s.q.QueryContext(ctx, query, args...)
	err = eachRow(rows, err, func(r *sql.Rows) error {
		var c declRef
		if err := r.Scan(&c.name, &c.module); err != nil {
			return err
		}
		consts = append(consts, c)
		return nil
	})
	if err != nil {
		return nil, err
	}
	owner, err := s.owners(ctx, consts)
	if err != nil {
		return nil, err
	}
	seen := map[declRef]bool{}
	var out []Found
	for _, c := range consts {
		f := owner[c]
		if d := (declRef{f.Module, f.Name}); !seen[d] {
			seen[d] = true
			out = append(out, f)
		}
	}
	return out, nil
}

// Stating is one statement that states a registered concept's fact: the declaration that its constant belongs to,
// by name and module, and the constant, by name and module (an equation lemma that Lean made in the module that
// uses it is recorded there, and belongs to a declaration of that module's imports).
type Stating struct {
	Decl       string `json:"declaration"`
	DeclModule string `json:"declaration_module"`
	Const      string `json:"constant"`
	Module     string `json:"module"`
	How        string `json:"how"`
	At         string `json:"at,omitempty"`
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
	consts := make([]declRef, len(rs))
	for i, r := range rs {
		consts[i] = declRef{r.module, r.name}
	}
	owner, err := s.owners(ctx, consts)
	if err != nil {
		return nil, err
	}
	out := make([]Stating, 0, len(rs))
	for _, r := range rs {
		f := owner[declRef{r.module, r.name}]
		st := Stating{Decl: f.Name, DeclModule: f.Module, Const: r.name, Module: r.module, How: r.how}
		if f.Path != "" {
			st.At = f.At()
		}
		out = append(out, st)
	}
	return out, nil
}

// SameAtoms lists the declarations whose statements share a relation atom with the statement of d, a declaration
// that Resolve gave: who else states one of its facts in the same form.
func (s *Store) SameAtoms(ctx context.Context, d Found) ([]Found, error) {
	found, err := s.distinctOwners(ctx, `SELECT DISTINCT b.name, b.module FROM atoms a JOIN atoms b ON b.atom = a.atom
		WHERE a.name = ? AND a.module = ? AND NOT (b.name = a.name AND b.module = a.module)`, d.Name, d.Module)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(found, func(x, y Found) int {
		return declRef{x.Module, x.Name}.compare(declRef{y.Module, y.Name})
	})
	return found, nil
}
