package declindex

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Tree is one built Lean project.
type Tree struct {
	// Source is the checkout: the .lean files, at the paths of their modules.
	Source string
	// Lib is the build's library directory for the project's own modules (Lake's .lake/build/lib/lean): their
	// .ilean, .olean and Lake's .hash files.
	Lib string
	// Roots are the project's module roots; only uses of their declarations are kept.
	Roots Roots
	// FileKinds are the file-name prefixes that group a user's file in an answer (for example the kinds of file
	// a project's layout names); a file with none of them is "other".
	FileKinds []string
}

// RefreshOptions control one refresh.
type RefreshOptions struct {
	// Commit is the source commit the build is of; it is recorded as the index's commit.
	Commit string
	// Jobs bounds the parallel readers; zero is min(48, CPUs).
	Jobs int
	// Compiled turns on the compiled part (types, compiled hashes, atoms, facts); nil skips it.
	Compiled *CompiledOptions
	// Log receives one line of progress per phase; nil discards it.
	Log io.Writer
}

// RefreshStats reports what a refresh did.
type RefreshStats struct {
	Reread     int `json:"reread"`
	Removed    int `json:"removed"`
	Unreadable int `json:"unreadable"`
	// Stale counts the declarations whose source is newer than their reference file: they have no statement hash.
	Stale int `json:"stale"`
	// Exported counts the compiled modules read; Rematched the further modules read because a definition they
	// apply changed.
	Exported  int           `json:"exported"`
	Rematched int           `json:"rematched"`
	Elapsed   time.Duration `json:"elapsed_ns"`
}

type moduleFile struct {
	rel                string
	ileanKey, oleanKey string
}

// buildKey identifies one build output: Lake's content hash when it wrote one, else size and modification time.
func buildKey(path string) (string, error) {
	if h, err := os.ReadFile(path + ".hash"); err == nil {
		return "h:" + strings.TrimSpace(string(h)), nil
	}
	st, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	return fmt.Sprintf("s:%d:%d", st.Size(), st.ModTime().UnixNano()), nil
}

func (o RefreshOptions) jobs() int {
	if o.Jobs > 0 {
		return o.Jobs
	}
	return min(48, runtime.NumCPU())
}

// scan lists the modules with a reference file below the tree's library directory and their keys, read in
// parallel. A module's reference key covers its .ilean and its source file: an edit that keeps every position
// (a numeral changed in place) can leave the .ilean byte for byte the same, and the statement hash is read from
// the source.
func scan(tree Tree, jobs int) ([]moduleFile, error) {
	lib := tree.Lib
	var rels []string
	err := filepath.WalkDir(lib, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(p, ".ilean") {
			rel, err := filepath.Rel(lib, p)
			if err != nil {
				return err
			}
			rels = append(rels, filepath.ToSlash(strings.TrimSuffix(rel, ".ilean")))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make([]moduleFile, len(rels))
	errs := make([]error, len(rels))
	parallel(len(rels), jobs, func(i int) {
		base := filepath.Join(lib, filepath.FromSlash(rels[i]))
		out[i].rel = rels[i]
		if out[i].ileanKey, errs[i] = buildKey(base + ".ilean"); errs[i] != nil {
			return
		}
		src, err := os.Stat(filepath.Join(tree.Source, filepath.FromSlash(rels[i])+".lean"))
		if err == nil {
			out[i].ileanKey += fmt.Sprintf("|src:%d:%d", src.Size(), src.ModTime().UnixNano())
		} else if !errors.Is(err, fs.ErrNotExist) {
			errs[i] = err
			return
		}
		out[i].oleanKey, errs[i] = buildKey(base + ".olean")
	})
	return out, errors.Join(errs...)
}

// parallel runs f(0..n-1) on at most jobs goroutines.
func parallel(n, jobs int, f func(int)) {
	next := make(chan int)
	var wg sync.WaitGroup
	for range min(jobs, max(n, 1)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				f(i)
			}
		}()
	}
	for i := range n {
		next <- i
	}
	close(next)
	wg.Wait()
}

type stored struct{ module, ileanKey, oleanKey string }

func (s *Store) storedModules(ctx context.Context) (map[string]stored, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT rel, module, ilean_key, olean_key FROM modules`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]stored{}
	for rows.Next() {
		var rel string
		var st stored
		if err := rows.Scan(&rel, &st.module, &st.ileanKey, &st.oleanKey); err != nil {
			return nil, err
		}
		out[rel] = st
	}
	return out, rows.Err()
}

func logf(w io.Writer, format string, args ...any) {
	if w != nil {
		fmt.Fprintf(w, format+"\n", args...)
	}
}

// Refresh brings the index to the tree's current build. It is RefreshAt without a post-landing synchronisation.
func (s *Store) Refresh(ctx context.Context, tree Tree, opt RefreshOptions) (RefreshStats, error) {
	return s.RefreshAt(ctx, tree, opt, nil)
}

// RefreshAt holds the writer lock across sync and the refresh: a post-landing fetch, checkout and build cannot
// race a second index refresh, and a query observes only the result after both complete.
func (s *Store) RefreshAt(ctx context.Context, tree Tree, opt RefreshOptions, sync func(context.Context) error) (RefreshStats, error) {
	unlock, err := s.lock()
	if err != nil {
		return RefreshStats{}, err
	}
	defer unlock()
	if sync != nil {
		if err := sync(ctx); err != nil {
			return RefreshStats{}, err
		}
	}
	return s.refreshLocked(ctx, tree, opt)
}

// refreshLocked is Refresh's body. Its caller holds the writer lock.
func (s *Store) refreshLocked(ctx context.Context, tree Tree, opt RefreshOptions) (RefreshStats, error) {
	t0 := time.Now()
	var st RefreshStats
	files, err := scan(tree, opt.jobs())
	if err != nil {
		return st, fmt.Errorf("scan %s: %w", tree.Lib, err)
	}
	old, err := s.storedModules(ctx)
	if err != nil {
		return st, err
	}
	seen := map[string]bool{}
	var changed []moduleFile
	for _, f := range files {
		seen[f.rel] = true
		if o, ok := old[f.rel]; !ok || o.ileanKey != f.ileanKey {
			changed = append(changed, f)
		}
	}
	var gone []string
	for rel := range old {
		if !seen[rel] {
			gone = append(gone, rel)
		}
	}
	results := make([]ModuleData, len(changed))
	errs := make([]error, len(changed))
	parallel(len(changed), opt.jobs(), func(i int) {
		results[i], errs[i] = readModule(tree, changed[i].rel)
	})
	// An unchanged module's heads were resolved against the declarations of an earlier refresh. The unchanged
	// modules whose resolution the changed declaration names can move are re-read with the changed ones.
	resolve, err := s.headModulesToResolve(ctx, files, old, changed, gone, results, errs)
	if err != nil {
		return st, err
	}
	if len(resolve) > 0 {
		more := make([]ModuleData, len(resolve))
		moreErrs := make([]error, len(resolve))
		parallel(len(resolve), opt.jobs(), func(i int) {
			more[i], moreErrs[i] = readModule(tree, resolve[i].rel)
		})
		changed = append(changed, resolve...)
		results = append(results, more...)
		errs = append(errs, moreErrs...)
	}
	logf(opt.Log, "index: %d modules, %d reference files changed, %d re-read to resolve heads, %d gone, read in %s",
		len(files), len(changed)-len(resolve), len(resolve), len(gone), time.Since(t0).Round(time.Millisecond))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return st, err
	}
	defer func() { _ = tx.Rollback() }()
	for _, rel := range append(append([]string(nil), gone...), relsOf(changed)...) {
		if o, ok := old[rel]; ok {
			for _, t := range []string{"decls", "heads", "uses", "edges", "imports"} {
				if _, err := tx.ExecContext(ctx, "DELETE FROM "+t+" WHERE module = ?", o.module); err != nil {
					return st, err
				}
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM modules WHERE rel = ?`, rel); err != nil {
			return st, err
		}
	}
	for _, rel := range gone {
		if err := deleteCompiled(ctx, tx, old[rel].module); err != nil {
			return st, err
		}
	}
	type pendingHead struct {
		name, module string
		source       SourceHead
	}
	decls := newBatch(tx, "decls", 7)
	uses := newBatch(tx, "uses", 6)
	imports := newBatch(tx, "imports", 2)
	imports.verb = "INSERT OR IGNORE"
	var pendingHeads []pendingHead
	for i, md := range results {
		if errs[i] != nil {
			st.Unreadable++
			logf(opt.Log, "index: %v", errs[i])
			continue
		}
		st.Reread++
		st.Stale += md.Stale
		// Shapes and heads join declarations by source line. A declaration without a statement hash has no
		// trusted source position (see ModuleData.Stale), so the line may hold another declaration.
		shapes := make(map[int]string, len(md.Shapes))
		for _, shape := range md.Shapes {
			shapes[shape.Line] = shape.Shape
		}
		declsByLine := make(map[int][]Decl, len(md.Decls))
		for _, d := range md.Decls {
			shape := ""
			if d.StmtHash != "" {
				shape = shapes[d.Line]
				declsByLine[d.Line] = append(declsByLine[d.Line], d)
			}
			if err := decls.add(ctx, d.Name, d.Short, d.Module, d.Kind, d.Line, d.StmtHash, shape); err != nil {
				return st, err
			}
		}
		for _, head := range md.Heads {
			for _, d := range declsByLine[head.Line] {
				pendingHeads = append(pendingHeads, pendingHead{name: d.Name, module: d.Module, source: head})
			}
		}
		for _, u := range md.Uses {
			if err := uses.add(ctx, u.Target, u.Module, u.Line, u.Col, u.Parent, u.Class); err != nil {
				return st, err
			}
		}
		for _, imported := range md.Imports {
			if err := imports.add(ctx, md.Module, imported); err != nil {
				return st, err
			}
		}
		// The olean key says which compiled module the index's types and facts were read from; re-reading the
		// reference file does not change it.
		if _, err := tx.ExecContext(ctx, `INSERT INTO modules (rel, module, ilean_key, olean_key) VALUES (?, ?, ?, ?)`,
			md.Rel, md.Module, changed[i].ileanKey, old[md.Rel].oleanKey); err != nil {
			return st, err
		}
	}
	if err := decls.flush(ctx); err != nil {
		return st, err
	}
	if len(pendingHeads) > 0 {
		declared, err := declaredNames(ctx, tx)
		if err != nil {
			return st, err
		}
		isDeclared := func(name string) bool { return declared[name] }
		heads := newBatch(tx, "heads", 5)
		for _, pending := range pendingHeads {
			resolved := ResolveHead(pending.source, isDeclared)
			if err := heads.add(ctx, pending.name, pending.module, pending.source.Role, resolved, pending.source.Line); err != nil {
				return st, err
			}
		}
		if err := heads.flush(ctx); err != nil {
			return st, err
		}
	}
	if err := uses.flush(ctx); err != nil {
		return st, err
	}
	if err := imports.flush(ctx); err != nil {
		return st, err
	}
	if err := insertEdges(ctx, tx, results, errs); err != nil {
		return st, err
	}
	st.Removed = len(gone)
	meta := map[string]string{
		"commit": opt.Commit,
		"built":  time.Now().UTC().Format("2006-01-02T15:04Z"),
		"errors": fmt.Sprint(st.Unreadable),
	}
	// The graph file is written after the commit. A new generation token marks it out of date until a graph of
	// this generation is written, so a failed write is repaired by the next refresh and never read meanwhile.
	token, err := s.Meta(ctx, "graph")
	if err != nil {
		return st, err
	}
	if len(changed) > 0 || len(gone) > 0 || token == "" {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			return st, err
		}
		token = hex.EncodeToString(b[:])
		meta["graph"] = token
	}
	for k, v := range meta {
		if err := s.setMeta(ctx, tx, k, v); err != nil {
			return st, err
		}
	}
	if err := tx.Commit(); err != nil {
		return st, err
	}
	if graphToken(s.path+".graph") != token {
		t1 := time.Now()
		g, err := buildGraph(ctx, s.db)
		if err != nil {
			return st, fmt.Errorf("build the declaration graph: %w", err)
		}
		g.token = token
		if err := g.write(s.path + ".graph"); err != nil {
			return st, fmt.Errorf("write the declaration graph: %w", err)
		}
		s.graphOnce, s.g, s.graphErr = sync.Once{}, nil, nil
		logf(opt.Log, "index: declaration graph of %d declarations and %d edges written in %s", len(g.names), len(g.dst),
			time.Since(t1).Round(time.Millisecond))
	}
	if opt.Compiled != nil {
		if st.Exported, st.Rematched, err = s.refreshCompiled(ctx, tree, opt.Compiled, files, opt.Log); err != nil {
			return st, err
		}
	}
	st.Elapsed = time.Since(t0)
	logf(opt.Log, "index: %d re-read, %d removed, %d unreadable, %d declarations newer than their build, %d compiled modules read, %d re-matched, %s",
		st.Reread, st.Removed, st.Unreadable, st.Stale, st.Exported, st.Rematched, st.Elapsed.Round(time.Millisecond))
	return st, nil
}

func relsOf(fs []moduleFile) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.rel
	}
	return out
}

// headModulesToResolve returns the unchanged modules with a stored head whose resolution can differ once the
// changed and gone modules are rewritten. Every candidate ResolveHead tries ends with the written name, so a head
// can resolve differently only when a declaration name with its last component is added or removed.
func (s *Store) headModulesToResolve(ctx context.Context, files []moduleFile, old map[string]stored, changed []moduleFile,
	gone []string, results []ModuleData, errs []error) ([]moduleFile, error) {
	// With nothing rewritten no name moves; with every module changed none is left unchanged.
	if (len(changed) == 0 && len(gone) == 0) || len(changed) == len(files) {
		return nil, nil
	}
	rewritten := map[string]bool{}
	for _, rel := range append(relsOf(changed), gone...) {
		if o, ok := old[rel]; ok {
			rewritten[o.module] = true
		}
	}
	before, err := s.declNamesIn(ctx, rewritten)
	if err != nil {
		return nil, err
	}
	after := map[string]bool{}
	for i, md := range results {
		if errs[i] != nil {
			continue
		}
		for _, d := range md.Decls {
			after[d.Name] = true
		}
	}
	moved := map[string]bool{}
	for name := range before {
		if !after[name] {
			moved[lastComponent(name)] = true
		}
	}
	for name := range after {
		if !before[name] {
			moved[lastComponent(name)] = true
		}
	}
	if len(moved) == 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT module, head FROM heads`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	modules := map[string]bool{}
	for rows.Next() {
		var module, head string
		if err := rows.Scan(&module, &head); err != nil {
			return nil, err
		}
		if !rewritten[module] && moved[lastComponent(head)] {
			modules[module] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	changedRels := make(map[string]bool, len(changed))
	for _, f := range changed {
		changedRels[f.rel] = true
	}
	var out []moduleFile
	for _, f := range files {
		if o, ok := old[f.rel]; ok && !changedRels[f.rel] && modules[o.module] {
			out = append(out, f)
		}
	}
	return out, nil
}

// declNamesIn returns the stored declaration names of the modules.
func (s *Store) declNamesIn(ctx context.Context, modules map[string]bool) (map[string]bool, error) {
	list := make([]string, 0, len(modules))
	for module := range modules {
		list = append(list, module)
	}
	names := map[string]bool{}
	for start := 0; start < len(list); start += 500 {
		chunk := list[start:min(start+500, len(list))]
		rows, err := s.db.QueryContext(ctx, `SELECT name FROM decls WHERE module IN (`+placeholders(len(chunk))+`)`, anys(chunk)...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				rows.Close()
				return nil, err
			}
			names[name] = true
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return names, nil
}

// lastComponent is the part of a dotted name after its last dot, as Decl.Short is.
func lastComponent(name string) string { return name[strings.LastIndexByte(name, '.')+1:] }

func declaredNames(ctx context.Context, tx *sql.Tx) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT name FROM decls`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	names := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names[name] = true
	}
	return names, rows.Err()
}

func readModule(tree Tree, rel string) (ModuleData, error) {
	ilean, err := os.ReadFile(filepath.Join(tree.Lib, filepath.FromSlash(rel)+".ilean"))
	if err != nil {
		return ModuleData{}, err
	}
	src, err := os.ReadFile(filepath.Join(tree.Source, filepath.FromSlash(rel)+".lean"))
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return ModuleData{}, err
		}
		src = nil
	} else if src == nil {
		src = []byte{}
	}
	md, err := ParseModule(rel, ilean, src, tree.Roots)
	if err == nil && md.Stale > 0 {
		return md, fmt.Errorf("%s.lean is newer than its build: %d declarations are not where its reference file records them", rel, md.Stale)
	}
	return md, err
}

// insertEdges adds the declaration-level edges of the re-read modules: for each user declaration, the targets
// it uses as recorded. Which declaration a target belongs to (`X.wp` is X's field while nothing declares `X.wp`)
// depends on every module's declarations, so buildGraph decides it when it writes the graph.
func insertEdges(ctx context.Context, tx *sql.Tx, results []ModuleData, errs []error) error {
	edges := newBatch(tx, "edges", 5)
	for i, md := range results {
		if errs[i] != nil {
			continue
		}
		kind := map[string]string{}
		for _, d := range md.Decls {
			kind[d.Name] = d.Kind
		}
		type pair struct{ target, dst string }
		agg := map[pair]bool{}
		var order []pair
		for _, u := range md.Uses {
			if u.Parent == "" {
				continue
			}
			p := pair{u.Target, u.Parent}
			if _, ok := agg[p]; !ok {
				order = append(order, p)
			}
			agg[p] = agg[p] || u.Class == "stmt"
		}
		for _, p := range order {
			stmt := 0
			if agg[p] {
				stmt = 1
			}
			if err := edges.add(ctx, p.target, p.dst, md.Module, stmt, kind[p.dst]); err != nil {
				return err
			}
		}
	}
	return edges.flush(ctx)
}

// batch inserts rows into one table many at a time.
type batch struct {
	tx    *sql.Tx
	table string
	cols  int
	args  []any
	// verb is the insert statement's verb: INSERT, or INSERT OR REPLACE.
	verb string
}

const batchRows = 400

func newBatch(tx *sql.Tx, table string, cols int) *batch {
	return &batch{tx: tx, table: table, cols: cols, verb: "INSERT"}
}

func (b *batch) add(ctx context.Context, vals ...any) error {
	b.args = append(b.args, vals...)
	if len(b.args) >= batchRows*b.cols {
		return b.flush(ctx)
	}
	return nil
}

func (b *batch) flush(ctx context.Context) error {
	if len(b.args) == 0 {
		return nil
	}
	n := len(b.args) / b.cols
	row := "(" + strings.TrimSuffix(strings.Repeat("?,", b.cols), ",") + ")"
	q := b.verb + " INTO " + b.table + " VALUES " + strings.TrimSuffix(strings.Repeat(row+",", n), ",")
	_, err := b.tx.ExecContext(ctx, q, b.args...)
	b.args = b.args[:0]
	return err
}
