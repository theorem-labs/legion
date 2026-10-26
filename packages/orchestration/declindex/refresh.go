package declindex

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
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
}

// RefreshOptions control one refresh.
type RefreshOptions struct {
	// Commit is the source commit the build is of; it is recorded as the index's commit, and a change of commit as
	// a landing (see Store.Landing).
	Commit string
	// Landed is the post-landing synchronisation (Sync) that Refresh runs under its refresh lock before it reads the
	// tree; nil refreshes the tree as it is. With it, Refresh first refuses a tree with tracked changes, and gives
	// ErrAlreadyPast without running it when the persisted index already covers Commit.
	Landed func(context.Context) error
	// Jobs bounds the parallel readers; zero is min(48, CPUs).
	Jobs int
	// Compiled gives the options of the compiled part (types, compiled hashes, atoms, facts); nil skips that part.
	// It runs after the reference files are read and committed, under the same refresh lock, because the registry's
	// forms import the modules that the index then knows. The commit's compiled part is complete only once it ran.
	Compiled func(context.Context) (*CompiledOptions, error)
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
	// WithoutForms are the clashes of the modules that the compiled part read without the registry's forms
	// (Store.WithoutForms), from this refresh and earlier ones: an index defect, which the tree's duplicate
	// declarations cause. Empty without the compiled part.
	WithoutForms []FormsClash `json:"without_forms"`
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
	return defaultJobs()
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
	out := map[string]stored{}
	rows, err := s.q.Query(ctx, `SELECT rel, module, ilean_key, olean_key FROM modules`)
	err = eachRow(rows, err, func(r pgx.Rows) error {
		var rel string
		var st stored
		if err := r.Scan(&rel, &st.module, &st.ileanKey, &st.oleanKey); err != nil {
			return err
		}
		out[rel] = st
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// sourceBlobs reads the sources table: each source's path and the blob the index read.
func (s *Store) sourceBlobs(ctx context.Context) (map[string]string, error) {
	out := map[string]string{}
	rows, err := s.q.Query(ctx, `SELECT path, blob FROM sources`)
	err = eachRow(rows, err, func(r pgx.Rows) error {
		var path, blob string
		if err := r.Scan(&path, &blob); err != nil {
			return err
		}
		out[path] = blob
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func logf(w io.Writer, format string, args ...any) {
	if w != nil {
		fmt.Fprintf(w, format+"\n", args...)
	}
}

// Refresh brings the index to the tree's current build. It holds the index's advisory lock across the landed sync
// (RefreshOptions.Landed) and the refresh: a post-landing fetch, checkout and build cannot race a second index
// refresh, and a reader of the tree waits for the result of both. The lock and the refresh's write transactions
// live on one connection, so a refresh that loses it has written nothing; a reader of the index alone takes no
// lock and reads the state of the refresh before while this one runs.
func (s *Store) Refresh(ctx context.Context, tree Tree, opt RefreshOptions) (RefreshStats, error) {
	unlock, err := s.lockRefresh(ctx)
	if err != nil {
		return RefreshStats{}, err
	}
	defer unlock()
	if s.afterRefreshLock != nil {
		if err := s.afterRefreshLock(ctx); err != nil {
			return RefreshStats{}, err
		}
	}
	// What the index held before this refresh read anything of it: the coverage check below, the reads of the
	// tree and the reads of the index all rest on this state, and no lock of a pooled route can hold it, so
	// every write of this refresh refuses when another writer has moved it since (writeTxFrom).
	generation, err := s.generation(ctx)
	if err != nil {
		return RefreshStats{}, err
	}
	if opt.Landed != nil {
		// The one check that the tree is clean, before the landed sync checks out another commit there.
		if err := requireCleanTree(ctx, tree.Source); err != nil {
			return RefreshStats{}, err
		}
		covering, err := s.indexCoversLandedCommit(ctx, tree.Source, opt.Commit, opt.Compiled != nil)
		if err != nil {
			return RefreshStats{}, err
		}
		if covering != "" {
			logf(opt.Log, "declindex: index at %s covers LAND_COMMIT %s", covering, opt.Commit)
			return RefreshStats{}, ErrAlreadyPast
		}
		if err := opt.Landed(ctx); err != nil {
			return RefreshStats{}, err
		}
	}
	return s.refreshLocked(ctx, tree, generation, opt)
}

// refreshLocked is Refresh's body. Its caller holds the tree's lock and gives the index's generation as it was
// before this refresh read anything; it reads the tree, and then writes the index.
func (s *Store) refreshLocked(ctx context.Context, tree Tree, generation int64, opt RefreshOptions) (RefreshStats, error) {
	t0 := time.Now()
	st := RefreshStats{WithoutForms: []FormsClash{}}
	r, err := s.readReference(ctx, tree, opt.jobs())
	if err != nil {
		return st, err
	}
	logf(opt.Log, "index: %d modules, %d reference files changed, %d gone, read in %s", len(r.files), len(r.changed),
		len(r.gone), time.Since(t0).Round(time.Millisecond))
	for i, err := range r.errs {
		if err != nil {
			st.Unreadable++
			logf(opt.Log, "index: %v", err)
			continue
		}
		st.Reread++
		st.Stale += r.results[i].Stale
	}
	st.Removed = len(r.gone)
	// A name the index cannot hold in one entry of its indexes is a defect of the tree, named before the
	// refresh writes anything (NameTooLongError), never a failure inside the database.
	if err := checkReference(r.results, r.errs); err != nil {
		return st, err
	}
	// sources keys a file by its path, and modules by the path of its module's source.
	if err := checkSources(slices.Sorted(maps.Keys(r.sources))); err != nil {
		return st, err
	}
	if err := s.writeTxFrom(ctx, generation, func(tx pgx.Tx) error {
		if err := writeSources(ctx, tx, r.storedSources, r.sources); err != nil {
			return err
		}
		before, err := readBeforeModules(ctx, tx, r)
		if err != nil {
			return err
		}
		if err := writeModules(ctx, tx, r); err != nil {
			return err
		}
		if err := reresolveOwners(ctx, tx, r, before, opt.Log); err != nil {
			return err
		}
		if err := s.writeMeta(ctx, tx, r, opt.Commit, st.Unreadable); err != nil {
			return err
		}
		// What this refresh owes: with lean.export it reads the compiled part next, in a second
		// transaction. A reader between them, or after a compiled step that failed, must be told that the
		// compiled part is missing, also when the index never had one (compiled_commit is then empty).
		owed := ""
		if opt.Compiled != nil {
			owed = "1"
		}
		return setMeta(ctx, tx, "compiled_owed", owed)
	}); err != nil {
		return st, err
	}
	// The write rewrote the modules: the store's answers read them anew.
	s.forget()
	if opt.Compiled != nil {
		co, err := opt.Compiled(ctx)
		if err != nil {
			return st, err
		}
		if st.Exported, st.Rematched, err = s.refreshCompiled(ctx, tree, co, r.files, opt.Commit, generation+1, opt.Log); err != nil {
			return st, err
		}
		if st.WithoutForms, err = s.WithoutForms(ctx); err != nil {
			return st, err
		}
	}
	if err := s.analyze(ctx); err != nil {
		return st, err
	}
	st.Elapsed = time.Since(t0)
	logf(opt.Log, "index: %d re-read, %d removed, %d unreadable, %d declarations newer than their build, %d compiled modules read, %d re-matched, %s",
		st.Reread, st.Removed, st.Unreadable, st.Stale, st.Exported, st.Rematched, st.Elapsed.Round(time.Millisecond))
	return st, nil
}

// reference is what a refresh read before it writes the reference part: the tree's sources (nil outside Git) and
// the index's, the modules of the build and of the index, which of them changed or are gone, and the re-read
// modules (an error for each that could not be read).
type reference struct {
	sources, storedSources map[string]string
	sourcePrefix           string
	previousCommit         string
	files                  []moduleFile
	old                    map[string]stored
	changed                []moduleFile
	gone                   []string
	results                []ModuleData
	errs                   []error
}

// readReference reads the tree's sources and build, and the index's sources and modules, and re-reads the modules
// whose reference file or source changed, on jobs goroutines.
func (s *Store) readReference(ctx context.Context, tree Tree, jobs int) (reference, error) {
	var r reference
	var err error
	if r.sources, r.sourcePrefix, err = readSources(tree); err != nil {
		return r, err
	}
	if r.previousCommit, err = s.Meta(ctx, "commit"); err != nil {
		return r, err
	}
	if r.files, err = scan(tree, jobs); err != nil {
		return r, fmt.Errorf("scan %s: %w", tree.Lib, err)
	}
	if r.old, err = s.storedModules(ctx); err != nil {
		return r, err
	}
	if r.storedSources, err = s.sourceBlobs(ctx); err != nil {
		return r, err
	}
	seen := map[string]bool{}
	for _, f := range r.files {
		seen[f.rel] = true
		sourceChanged := r.sources != nil && r.sources[f.rel+".lean"] != r.storedSources[f.rel+".lean"]
		if o, ok := r.old[f.rel]; !ok || o.ileanKey != f.ileanKey || sourceChanged {
			r.changed = append(r.changed, f)
		}
	}
	for rel := range r.old {
		if !seen[rel] {
			r.gone = append(r.gone, rel)
		}
	}
	r.results = make([]ModuleData, len(r.changed))
	r.errs = make([]error, len(r.changed))
	parallel(len(r.changed), jobs, func(i int) {
		r.results[i], r.errs[i] = readModule(tree, r.changed[i].rel)
	})
	return r, nil
}

// writeSources makes the sources table current: the rows of sources that stored has and current lacks are
// deleted, and only new or changed rows written. The index's lock, held since stored was read, keeps another
// refresh from changing the table meanwhile, as it does for the modules.
func writeSources(ctx context.Context, tx pgx.Tx, stored, current map[string]string) error {
	// A copy writes new rows only, so every row it replaces is deleted first, with those that are gone.
	var drop []string
	for path := range stored {
		if _, kept := current[path]; !kept {
			drop = append(drop, path)
		}
	}
	var write []string
	for _, path := range slices.Sorted(maps.Keys(current)) {
		if blob, ok := stored[path]; ok {
			if blob == current[path] {
				continue
			}
			drop = append(drop, path)
		}
		write = append(write, path)
	}
	if len(drop) > 0 {
		if _, err := tx.Exec(ctx, `DELETE FROM sources WHERE path = ANY($1)`, drop); err != nil {
			return err
		}
	}
	if len(write) == 0 {
		return nil
	}
	row := make([]any, 2)
	_, err := tx.CopyFrom(ctx, pgx.Identifier{"sources"}, []string{"path", "blob"}, copyFrom(func(yield func([]any) bool) {
		for _, path := range write {
			row[0], row[1] = path, current[path]
			if !yield(row) {
				return
			}
		}
	}))
	return err
}

// writeModules replaces the reference rows of the changed and gone modules (and the compiled rows of the gone ones,
// and of the changed ones that could not be read, which leave the index) with those of the re-read modules: their
// imports, declarations, uses, modules rows and edges.
func writeModules(ctx context.Context, tx pgx.Tx, r reference) error {
	if err := deleteModules(ctx, tx, slices.Concat(r.gone, relsOf(r.changed)), r.old); err != nil {
		return err
	}
	gone := make([]string, 0, len(r.gone))
	for _, rel := range r.gone {
		gone = append(gone, r.old[rel].module)
	}
	for i, err := range r.errs {
		if old, ok := r.old[r.changed[i].rel]; ok && err != nil {
			gone = append(gone, old.module)
		}
	}
	if err := deleteCompiled(ctx, tx, gone); err != nil {
		return err
	}
	// modules has rel as its primary key, and a copy cannot replace a row, so the last row of each rel wins here
	// as INSERT OR REPLACE did. The other three tables have no key: every row is written.
	moduleRows := make([][]any, 0, len(r.results))
	at := make(map[string]int, len(r.results))
	for i, md := range r.results {
		if r.errs[i] != nil {
			continue
		}
		// The olean key says which compiled module the index's types and facts were read from; re-reading the
		// reference file does not change it. Each re-read module's old row was deleted above.
		row := []any{md.Rel, md.Module, r.changed[i].ileanKey, r.old[md.Rel].oleanKey}
		if j, ok := at[md.Rel]; ok {
			moduleRows[j] = row
			continue
		}
		at[md.Rel] = len(moduleRows)
		moduleRows = append(moduleRows, row)
	}
	// Each source below holds one row at a time: a refresh of a whole project writes millions of them.
	imports, decls, uses := make([]any, 2), make([]any, 6), make([]any, 7)
	for _, c := range []struct {
		table string
		cols  []string
		rows  pgx.CopyFromSource
	}{
		{"modules", []string{"rel", "module", "ilean_key", "olean_key"}, pgx.CopyFromRows(moduleRows)},
		{"imports", []string{"module", "imported"}, copyFrom(func(yield func([]any) bool) {
			for i, md := range r.results {
				if r.errs[i] != nil {
					continue
				}
				for _, imported := range md.Imports {
					imports[0], imports[1] = md.Module, imported
					if !yield(imports) {
						return
					}
				}
			}
		})},
		{"decls", []string{"name", "short", "module", "kind", "line", "stmt_hash"}, copyFrom(func(yield func([]any) bool) {
			for i, md := range r.results {
				if r.errs[i] != nil {
					continue
				}
				for _, d := range md.Decls {
					decls[0], decls[1], decls[2], decls[3], decls[4], decls[5] = d.Name, d.Short, d.Module, d.Kind, d.Line, d.StmtHash
					if !yield(decls) {
						return
					}
				}
			}
		})},
		{"uses", []string{"target", "tmodule", "module", "line", "col", "parent", "cls"}, copyFrom(func(yield func([]any) bool) {
			for i, md := range r.results {
				if r.errs[i] != nil {
					continue
				}
				for _, u := range md.Uses {
					uses[0], uses[1], uses[2], uses[3], uses[4], uses[5], uses[6] = u.Target, u.TargetModule, u.Module, u.Line, u.Col, u.Parent, u.Class
					if !yield(uses) {
						return
					}
				}
			}
		})},
	} {
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{c.table}, c.cols, c.rows); err != nil {
			return fmt.Errorf("write the index's %s: %w", c.table, err)
		}
	}
	return insertEdges(ctx, tx, r.results, r.errs)
}

// writeMeta records the refresh in the meta table, and a change of commit as a landing.
func (s *Store) writeMeta(ctx context.Context, tx pgx.Tx, r reference, commit string, unreadable int) error {
	meta := map[string]string{
		"commit":  commit,
		"built":   time.Now().UTC().Format("2006-01-02T15:04Z"),
		"errors":  fmt.Sprint(unreadable),
		"sources": "",
		// The tree's path below the root of its repository: land-hash reads LAND_TREE below it.
		"source_prefix": r.sourcePrefix,
	}
	if r.sources != nil {
		meta["sources"] = "git"
	}
	if commit != "" && commit != r.previousCommit {
		if _, err := tx.Exec(ctx, `INSERT INTO landings (landed, previous) VALUES ($1, $2)
			ON CONFLICT (landed) DO UPDATE SET previous = EXCLUDED.previous`, commit, r.previousCommit); err != nil {
			return err
		}
	}
	for k, v := range meta {
		if err := setMeta(ctx, tx, k, v); err != nil {
			return err
		}
	}
	return nil
}

// deleteModules deletes the reference rows of the modules at rels: the imports, declarations, uses and edges of
// each one's old module name, and its modules row. One statement per table takes every module of the refresh.
func deleteModules(ctx context.Context, tx pgx.Tx, rels []string, old map[string]stored) error {
	if len(rels) == 0 {
		return nil
	}
	modules := make([]string, 0, len(rels))
	for _, rel := range rels {
		if o, ok := old[rel]; ok {
			modules = append(modules, o.module)
		}
	}
	if len(modules) > 0 {
		for _, t := range []string{"imports", "decls", "uses", "edges"} {
			if _, err := tx.Exec(ctx, "DELETE FROM "+t+" WHERE module = ANY($1)", modules); err != nil {
				return err
			}
		}
	}
	_, err := tx.Exec(ctx, `DELETE FROM modules WHERE rel = ANY($1)`, rels)
	return err
}

func relsOf(files []moduleFile) []string {
	out := make([]string, len(files))
	for i, f := range files {
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
