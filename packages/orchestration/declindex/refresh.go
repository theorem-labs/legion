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
	"sort"
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
	// PreviousCommit is the index commit that this refresh replaces after a landed synchronisation.
	PreviousCommit string
	// CheckLandedCoverage makes RefreshAt compare Commit with the persisted index before it runs a landed sync.
	CheckLandedCoverage bool
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

func (s *Store) storedSources(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT path, blob FROM sources`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var path, blob string
		if err := rows.Scan(&path, &blob); err != nil {
			return nil, err
		}
		out[path] = blob
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
	if s.beforeRefreshLock != nil {
		s.beforeRefreshLock()
	}
	unlock, err := s.lock()
	if err != nil {
		return RefreshStats{}, err
	}
	defer unlock()
	if sync != nil && opt.CheckLandedCoverage {
		dirty, err := trackedChanges(ctx, tree.Source)
		if err != nil {
			return RefreshStats{}, err
		}
		if dirty {
			return RefreshStats{}, errors.New("the tree has tracked changes")
		}
		covered, err := s.indexCoversLandedCommit(ctx, tree.Source, opt.Commit)
		if err != nil {
			return RefreshStats{}, err
		}
		if covered {
			return RefreshStats{}, ErrAlreadyPast
		}
	}
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
	sourceBlobs, err := readSources(tree)
	if err != nil {
		return st, err
	}
	previousCommit, err := s.Meta(ctx, "commit")
	if err != nil {
		return st, err
	}
	files, err := scan(tree, opt.jobs())
	if err != nil {
		return st, fmt.Errorf("scan %s: %w", tree.Lib, err)
	}
	old, err := s.storedModules(ctx)
	if err != nil {
		return st, err
	}
	storedSourceBlobs, err := s.storedSources(ctx)
	if err != nil {
		return st, err
	}
	seen := map[string]bool{}
	var changed []moduleFile
	for _, f := range files {
		seen[f.rel] = true
		sourceChanged := sourceBlobs != nil && sourceBlobs[f.rel+".lean"] != storedSourceBlobs[f.rel+".lean"]
		if o, ok := old[f.rel]; !ok || o.ileanKey != f.ileanKey || sourceChanged {
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
	if _, err := tx.ExecContext(ctx, `DELETE FROM sources`); err != nil {
		return st, err
	}
	sourcePaths := make([]string, 0, len(sourceBlobs))
	for path := range sourceBlobs {
		sourcePaths = append(sourcePaths, path)
	}
	sort.Strings(sourcePaths)
	sources := newBatch(tx, "sources", 2)
	for _, path := range sourcePaths {
		if err := sources.add(ctx, path, sourceBlobs[path]); err != nil {
			return st, err
		}
	}
	if err := sources.flush(ctx); err != nil {
		return st, err
	}
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
		st.Stale += md.Stale
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
	meta := map[string]string{
		"commit":  opt.Commit,
		"built":   time.Now().UTC().Format("2006-01-02T15:04Z"),
		"errors":  fmt.Sprint(st.Unreadable),
		"sources": "",
	}
	if sourceBlobs != nil {
		meta["sources"] = "git"
	}
	if opt.Commit != previousCommit {
		meta["prev_commit"] = previousCommit
	}
	if opt.PreviousCommit != "" {
		meta["prev_commit"] = opt.PreviousCommit
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
