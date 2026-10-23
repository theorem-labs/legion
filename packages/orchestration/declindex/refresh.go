package declindex

import (
	"context"
	"database/sql"
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

// scan lists the modules with a reference file below lib and their build keys, reading the keys in parallel.
func scan(lib string, jobs int) ([]moduleFile, error) {
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

// Refresh brings the index to the tree's current build: it re-reads the modules whose reference file changed,
// drops the ones that are gone, recomputes their edges, and (with an exporter) re-reads the compiled modules
// that changed and re-matches the statements that reach a changed definition.
func (s *Store) Refresh(ctx context.Context, tree Tree, opt RefreshOptions) (RefreshStats, error) {
	unlock, err := s.lock()
	if err != nil {
		return RefreshStats{}, err
	}
	defer unlock()
	t0 := time.Now()
	var st RefreshStats
	files, err := scan(tree.Lib, opt.jobs())
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
	logf(opt.Log, "index: %d modules, %d reference files changed, %d gone, read in %s", len(files), len(changed),
		len(gone), time.Since(t0).Round(time.Millisecond))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return st, err
	}
	defer func() { _ = tx.Rollback() }()
	for _, rel := range append(append([]string(nil), gone...), relsOf(changed)...) {
		if o, ok := old[rel]; ok {
			for _, t := range []string{"decls", "uses", "edges"} {
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
	decls := newBatch(tx, "decls", 6)
	uses := newBatch(tx, "uses", 6)
	for i, md := range results {
		if errs[i] != nil {
			st.Unreadable++
			logf(opt.Log, "index: %v", errs[i])
			continue
		}
		st.Reread++
		for _, d := range md.Decls {
			if err := decls.add(ctx, d.Name, d.Short, d.Module, d.Kind, d.Line, d.StmtHash); err != nil {
				return st, err
			}
		}
		for _, u := range md.Uses {
			if err := uses.add(ctx, u.Target, u.Module, u.Line, u.Col, u.Parent, u.Class); err != nil {
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
	if err := uses.flush(ctx); err != nil {
		return st, err
	}
	if err := insertEdges(ctx, tx, results, errs); err != nil {
		return st, err
	}
	st.Removed = len(gone)
	for k, v := range map[string]string{
		"commit": opt.Commit,
		"built":  time.Now().UTC().Format("2006-01-02T15:04Z"),
		"errors": fmt.Sprint(st.Unreadable),
	} {
		if err := s.setMeta(ctx, tx, k, v); err != nil {
			return st, err
		}
	}
	if err := tx.Commit(); err != nil {
		return st, err
	}
	if _, err := os.Stat(s.path + ".graph"); st.Reread > 0 || len(gone) > 0 || err != nil {
		t1 := time.Now()
		g, err := buildGraph(ctx, s.db)
		if err != nil {
			return st, fmt.Errorf("build the declaration graph: %w", err)
		}
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
	logf(opt.Log, "index: %d re-read, %d removed, %d unreadable, %d compiled modules read, %d re-matched, %s",
		st.Reread, st.Removed, st.Unreadable, st.Exported, st.Rematched, st.Elapsed.Round(time.Millisecond))
	return st, nil
}

func relsOf(fs []moduleFile) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.rel
	}
	return out
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
	return ParseModule(rel, ilean, src, tree.Roots)
}

// insertEdges adds the declaration-level edges of the re-read modules. A use's target is counted as the longest
// declared name it extends (`X.wp` is X's field), so a generated part's use is a use of its declaration.
func insertEdges(ctx context.Context, tx *sql.Tx, results []ModuleData, errs []error) error {
	if len(results) == 0 {
		return nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT name FROM decls`)
	if err != nil {
		return err
	}
	names := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return err
		}
		names[n] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	owner := map[string]string{}
	own := func(t string) string {
		if o, ok := owner[t]; ok {
			return o
		}
		o := t
		for o != "" && !names[o] {
			i := strings.LastIndexByte(o, '.')
			if i < 0 {
				o = ""
				break
			}
			o = o[:i]
		}
		if o == "" {
			o = t
		}
		owner[t] = o
		return o
	}
	edges := newBatch(tx, "edges", 5)
	for i, md := range results {
		if errs[i] != nil {
			continue
		}
		kind := map[string]string{}
		for _, d := range md.Decls {
			kind[d.Name] = d.Kind
		}
		type pair struct{ src, dst string }
		agg := map[pair]bool{}
		var order []pair
		for _, u := range md.Uses {
			if u.Parent == "" {
				continue
			}
			src := own(u.Target)
			if src == u.Parent {
				continue
			}
			p := pair{src, u.Parent}
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
			if err := edges.add(ctx, p.src, p.dst, md.Module, stmt, kind[p.dst]); err != nil {
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
