// Command declindex answers questions about a built Lean project from its index of declarations: where a name is
// declared, who uses it, how far a change to it reaches, its statement hash, and who states a fact. `refresh`
// brings the index to the build after a landing.
package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"

	"github.com/sjawhar/legion/orchestration/declindex"
)

const usage = "usage: declindex --config FILE [--index NAME] [--index-dsn-file FILE] [--state DIR] [--json] <refresh|status|where|users|hash|states|same|gate|ledger|land-hash> ..."

const help = usage + `

The index is one schema of a Postgres database: index: in the configuration (or --index) names the schema, and
DECLINDEX_DSN or --index-dsn-file gives the connection string. No configuration file holds one.

Global options, before the command:
  --config FILE                the project's index configuration (or DECLINDEX_CONFIG)
  --index NAME                 the index's schema, overriding the configuration's index:
  --index-dsn-file FILE        the index's connection string, read from FILE; it wins over DECLINDEX_DSN
  --state DIR                  the local directory of the exporter build and the forms module, overriding state:
  --json                       answer as JSON, where the command answers in JSON

Commands:
  refresh                      bring the index to the tree's build; with LAND_COMMIT, fetch, check out and build
                               that landed commit before reading it
  status                       the index's commit, build time and size, and the tree's commit
  where NAME...                where each declaration is: name, kind, module, path:line
  users [--lines] [--why] [--limit N] NAME...
                               direct users of each declaration, grouped by directory and file kind, each use
                               marked stmt or proof; and the reach of a change to its statement
  hash NAME...                 name, statement hash and path:line, the hash a change lock records as a read
  states CONCEPT...            the statements that state a registered concept's fact: directly, through a
                               definition they apply (via:<definition>), or by using the concept (uses)
  same NAME...                 the declarations whose statements share a relation atom with NAME's statement
  gate --proposer OWNER [--accepted-by OWNER] PROPOSAL.json
                               the similarity gate: refuse an exact duplicate, send a near match to the librarian,
                               refuse a taken name, or print an addition; the proposal is JSON
                               {"id", "concept": {name, home, meaning, extends}, "source": <Lean module>}
  ledger [--dsn-file PATH | --contracts-file FILE --emit FILE]
                               check the ledger and newly landed registry concepts; file mode emits registrations
                               and near pairs for the landing program and never connects to the ledger
  land-hash                    the landing program's hasher: LAND_TREE, LAND_GIT_DIR, LAND_NAMES or LAND_NAMES_FILE
                               (a matching JSON array), and LAND_BUILD_DIR for changed sources in;
                               name<TAB>hash<TAB>path out

NAME is a full name or any dotted suffix of one. A name declared twice is answered once per declaration.
Exit codes:
  refresh: 0 read; 2 usage; 3 unable; 75 retry
  status: 0 answered; 2 usage; 3 unable; 75 retry
  where: 0 answered; 1 name not found; 2 usage; 3 unable; 75 retry
  users: 0 answered; 1 name not found; 2 usage; 3 unable; 75 retry
  hash: 0 answered; 1 name not found; 2 usage; 3 unable; 75 retry
  states: 0 answered; 1 name not found; 2 usage; 3 unable; 75 retry
  same: 0 answered; 1 name not found; 2 usage; 3 unable; 75 retry
  gate: 0 added; 1 duplicate; 2 usage; 3 unable; 4 near; 5 name taken; 75 retry
  ledger: 0 clean; 1 defect; 2 usage; 3 unable; 75 retry (also when the registry recheck of a landing cannot run)
  land-hash: 0 answered; 2 usage; 3 unable; 75 retry
75 is a failed connection to the index's database, or a run that a signal stopped: the command can be run again.`

type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

// unableError is a failure that leaves the question unanswered: never a pass.
type unableError struct{ err error }

func (e *unableError) Error() string { return e.err.Error() }
func (e *unableError) Unwrap() error { return e.err }

// stoppedBy holds the signal that stopped this run, once one has. fail then says that the run was stopped, not that
// the database is unreachable, which is how the end of the command's context reaches its database connection.
var stoppedBy atomic.Value

// main cancels the command on SIGTERM, SIGINT or SIGHUP, so that the build and the toolchain processes it started
// are killed with their process groups before it exits, rather than left running without it. A second signal ends
// the process at once.
func main() {
	ctx, cancel := context.WithCancel(context.Background())
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	go func() {
		s := <-signals
		stoppedBy.Store(s)
		signal.Stop(signals)
		cancel()
	}()
	code := run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr)
	signal.Stop(signals)
	cancel()
	os.Exit(code)
}

type globals struct {
	config, index, state, dsnFile string
	json                          bool
}

func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	var g globals
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		switch args[0] {
		case "--help", "-h":
			fmt.Fprintln(stdout, help)
			return 0
		case "--json":
			g.json = true
			args = args[1:]
			continue
		case "--config", "--index", "--index-dsn-file", "--state":
			if len(args) < 2 {
				return fail(stderr, &usageError{args[0] + " needs a value"})
			}
			switch args[0] {
			case "--config":
				g.config = args[1]
			case "--index":
				g.index = args[1]
			case "--index-dsn-file":
				g.dsnFile = args[1]
			case "--state":
				g.state = args[1]
			}
			args = args[2:]
			continue
		case "--db":
			return fail(stderr, &usageError{declindex.ErrDBRemoved.Error()})
		}
		return fail(stderr, &usageError{"unknown option " + args[0]})
	}
	if g.config == "" {
		g.config = getenv("DECLINDEX_CONFIG")
	}
	if len(args) == 0 {
		return fail(stderr, &usageError{"missing command"})
	}
	if g.config == "" {
		return fail(stderr, &usageError{"--config (or DECLINDEX_CONFIG) is required"})
	}
	cmd, rest := args[0], args[1:]
	if _, ok := commands[cmd]; !ok {
		return fail(stderr, &usageError{fmt.Sprintf("unknown command %q", cmd)})
	}
	cfg, err := settings(g, getenv)
	if err != nil {
		return fail(stderr, err)
	}
	// A command writes its output here and returns, closing the index, before anything reads the output: a
	// command's snapshot of the index holds up a refresh's write (declindex.OpenReadOnly), so a slow reader of the
	// output must not hold the snapshot.
	var out bytes.Buffer
	code := commands[cmd](ctx, &env{g: g, cfg: cfg, stdout: &out, stderr: stderr, getenv: getenv}, rest)
	if _, err := out.WriteTo(stdout); err != nil && code == 0 {
		return fail(stderr, &unableError{err})
	}
	return code
}

type env struct {
	g      globals
	cfg    declindex.Config
	stdout io.Writer
	stderr io.Writer
	getenv func(string) string
	store  *declindex.Store
}

// settings is the configuration a command runs with: the file, the overrides of --index and --state, and the
// index's connection string, which no configuration file holds (P-1).
func settings(g globals, getenv func(string) string) (declindex.Config, error) {
	cfg, err := declindex.LoadConfig(g.config)
	if err != nil {
		// An old configuration names the index file: its owner must change the file, as for a wrong flag.
		if errors.Is(err, declindex.ErrDBRemoved) {
			return cfg, &usageError{err.Error()}
		}
		return cfg, &unableError{err}
	}
	if g.index != "" {
		if err := declindex.ValidIndexName(g.index); err != nil {
			return cfg, &usageError{"--index: " + err.Error()}
		}
		cfg.Index = g.index
	}
	if g.state != "" {
		state, err := filepath.Abs(g.state)
		if err != nil {
			return cfg, &usageError{"--state " + g.state + ": " + err.Error()}
		}
		cfg.State = state
	}
	if cfg.DSN, err = indexDSN(g.dsnFile, getenv); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// indexDSN is the index's connection string: the contents of the file that --index-dsn-file names, which wins,
// else DECLINDEX_DSN. So one configuration serves a hook that writes the index with one login and a query that
// reads it with another.
func indexDSN(file string, getenv func(string) string) (string, error) {
	if file == "" {
		return strings.TrimSpace(getenv("DECLINDEX_DSN")), nil
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return "", &usageError{"--index-dsn-file: " + err.Error()}
	}
	dsn := strings.TrimSpace(string(raw))
	if dsn == "" {
		return "", &usageError{"--index-dsn-file " + file + ": the file is empty"}
	}
	return dsn, nil
}

// target is the index a command opens. A command that opens the index without a schema name or a connection
// string is a usage error, as a missing db: was before it (P-1).
func (e *env) target() (declindex.Target, error) {
	return e.targetFor(false)
}

// targetFor is the index a command opens; withTree also names the file whose lock guards the indexed tree, for a
// command that reads or writes the tree (refresh, gate, ledger). A command that reads the index alone needs no
// tree on this machine, so it does not resolve the state directory: an owner's pod has the index and no tree.
func (e *env) targetFor(withTree bool) (declindex.Target, error) {
	t := e.cfg.Target()
	if withTree {
		var err error
		if t, err = e.cfg.TreeTarget(); err != nil {
			return declindex.Target{}, &usageError{err.Error()}
		}
	}
	switch {
	case t.Index == "":
		return t, &usageError{"index: is required: the index is a schema of a Postgres database (or pass --index NAME)"}
	case t.DSN == "":
		return t, &usageError{"no connection string for the index: set DECLINDEX_DSN or pass --index-dsn-file FILE"}
	}
	if err := declindex.ValidIndexName(t.Index); err != nil {
		return t, &usageError{"index: " + err.Error()}
	}
	return t, nil
}

// open opens the index to write it: only refresh and the schema rebuild do, and both read and change the tree,
// so the target names the tree's lock.
func (e *env) open(ctx context.Context) error {
	return e.openWith(ctx, true, false, func(t declindex.Target) (*declindex.Store, error) { return declindex.Open(ctx, t) })
}

// openReadOnly opens the index for one snapshot of it, until close, and writes nothing (declindex.OpenReadOnly).
// scope is what the command reads besides the index.
func (e *env) openReadOnly(ctx context.Context, scope declindex.ReadScope, needIndex bool) error {
	return e.openWith(ctx, scope == declindex.IndexAndTree, needIndex, func(t declindex.Target) (*declindex.Store, error) {
		return declindex.OpenReadOnly(ctx, t, scope)
	})
}

func (e *env) openWith(ctx context.Context, withTree, needIndex bool, open func(declindex.Target) (*declindex.Store, error)) error {
	t, err := e.targetFor(withTree)
	if err != nil {
		return err
	}
	s, err := open(t)
	if err != nil {
		return &unableError{err}
	}
	e.store = s
	if !needIndex {
		return nil
	}
	if err := s.CheckComplete(ctx); err != nil {
		return &unableError{err}
	}
	return nil
}

func (e *env) close() {
	if e.store != nil {
		_ = e.store.Close()
	}
}

var commands = map[string]func(context.Context, *env, []string) int{
	"refresh":   cmdRefresh,
	"status":    cmdStatus,
	"where":     cmdWhere,
	"users":     cmdUsers,
	"hash":      cmdHash,
	"states":    cmdStates,
	"same":      cmdSame,
	"gate":      cmdGate,
	"ledger":    cmdLedger,
	"land-hash": cmdLandHash,
}

func fail(w io.Writer, err error) int {
	var u *usageError
	var nf *declindex.NotFoundError
	var temporary *declindex.TempError
	// A run that a signal stopped failed because it was stopped, whatever error the end of its context left (its
	// database connection reports itself unreachable): it says so, and exits 75, as a run can be started again.
	if s, ok := stoppedBy.Load().(os.Signal); ok {
		fmt.Fprintf(w, "declindex: the run was stopped by a signal (%v) before it finished: %v\n", s, err)
		return 75
	}
	// A failed connection to the index's database leaves every question unanswered, and another run can answer
	// it: the landing program retries 75 and escalates 3 (P-11).
	if declindex.Unreachable(err) {
		fmt.Fprintln(w, "declindex: the index's database is unreachable: "+err.Error())
		return 75
	}
	fmt.Fprintln(w, "declindex: "+err.Error())
	switch {
	case errors.As(err, &u):
		fmt.Fprintln(w, usage)
		return 2
	case errors.As(err, &nf):
		return 1
	case errors.As(err, &temporary):
		return 75
	default:
		return 3
	}
}

func (e *env) printJSON(v any) int {
	enc := json.NewEncoder(e.stdout)
	enc.SetIndent("", " ")
	if err := enc.Encode(v); err != nil {
		return fail(e.stderr, &unableError{err})
	}
	return 0
}

// printJSONOr prints v as JSON and gives rc, or the exit code of a failed print.
func (e *env) printJSONOr(v any, rc int) int {
	if c := e.printJSON(v); c != 0 {
		return c
	}
	return rc
}

// resolve gives the declarations that q names, and 0 to go on or the exit code of a failure it has reported when
// the index cannot answer. A name that names none is reported on stderr and sets *rc to 1.
func (e *env) resolve(ctx context.Context, q string, rc *int) ([]declindex.Found, int) {
	found, err := e.store.Resolve(ctx, q)
	if err != nil {
		return nil, fail(e.stderr, &unableError{err})
	}
	if len(found) == 0 {
		*rc = fail(e.stderr, &declindex.NotFoundError{Name: q})
	}
	return found, 0
}

func cmdRefresh(ctx context.Context, e *env, args []string) int {
	if len(args) != 0 {
		return fail(e.stderr, &usageError{"refresh takes no arguments"})
	}
	if err := e.open(ctx); err != nil {
		return fail(e.stderr, err)
	}
	defer e.close()
	tree := e.cfg.TreeSpec()
	opt := declindex.RefreshOptions{Jobs: e.cfg.Jobs, Log: e.stderr}
	if commit := e.getenv("LAND_COMMIT"); commit != "" {
		if !declindex.IsObjectID(commit) {
			return fail(e.stderr, &usageError{"LAND_COMMIT must be 40 lowercase hexadecimal characters"})
		}
		opt.Commit, opt.Landed = commit, declindex.Sync(e.cfg, commit, e.stderr)
	} else if head, err := declindex.TreeHead(ctx, e.cfg.Tree); err != nil {
		// A build outside Git is indexed without a commit, which no ledger or gate check accepts.
		fmt.Fprintf(e.stderr, "note: the index records no commit: %v\n", err)
	} else {
		opt.Commit = head
	}
	if e.cfg.Lean.Export {
		opt.Compiled = func(ctx context.Context) (*declindex.CompiledOptions, error) { return compiledOptions(ctx, e) }
	}
	st, err := e.store.Refresh(ctx, tree, opt)
	if errors.Is(err, declindex.ErrAlreadyPast) {
		if e.cfg.Lean.Export {
			clashes, err := e.store.WithoutForms(ctx)
			if err != nil {
				return fail(e.stderr, &unableError{err})
			}
			writeWithoutForms(e.stderr, declindex.ClashModules(clashes), clashes)
		}
		return 0
	}
	if err != nil {
		return fail(e.stderr, &unableError{err})
	}
	writeWithoutForms(e.stderr, declindex.ClashModules(st.WithoutForms), st.WithoutForms)
	// The refresh has committed; this count is a round trip of its own, so a database that goes away here asks
	// for a retry (75) and says nothing about what the refresh read.
	n, err := e.store.ModuleCount(ctx)
	if err != nil {
		return fail(e.stderr, &unableError{err})
	}
	if n == 0 || st.Unreadable > 0 {
		fmt.Fprintf(e.stderr, "declindex: the index is incomplete: %d modules, %d unreadable reference files\n", n, st.Unreadable)
		return 3
	}
	if e.g.json {
		return e.printJSON(st)
	}
	return 0
}

type status struct {
	Commit   string `json:"commit"`
	Built    string `json:"built"`
	Modules  int    `json:"modules"`
	Errors   string `json:"errors"`
	TreeHead string `json:"tree_head"`
	// WithoutForms are the clashes of the modules that the compiled part read without the registry's forms.
	WithoutForms []declindex.FormsClash `json:"without_forms"`
}

func cmdStatus(ctx context.Context, e *env, args []string) int {
	if len(args) != 0 {
		return fail(e.stderr, &usageError{"status takes no arguments"})
	}
	if err := e.openReadOnly(ctx, declindex.IndexOnly, false); err != nil {
		return fail(e.stderr, err)
	}
	defer e.close()
	var st status
	var err error
	if st.Commit, err = e.store.Meta(ctx, "commit"); err != nil {
		return fail(e.stderr, &unableError{err})
	}
	st.Built, _ = e.store.Meta(ctx, "built")
	st.Errors, _ = e.store.Meta(ctx, "errors")
	if st.Modules, err = e.store.ModuleCount(ctx); err != nil {
		return fail(e.stderr, &unableError{err})
	}
	if st.WithoutForms, err = e.store.WithoutForms(ctx); err != nil {
		return fail(e.stderr, &unableError{err})
	}
	if st.TreeHead, err = declindex.TreeHead(ctx, e.cfg.Tree); err != nil {
		fmt.Fprintf(e.stderr, "note: %v\n", err)
	}
	writeWithoutForms(e.stderr, declindex.ClashModules(st.WithoutForms), st.WithoutForms)
	if e.g.json {
		return e.printJSON(st)
	}
	fmt.Fprintf(e.stdout, "index at %s, built %s, %d modules, %s unreadable; tree at %s\n", st.Commit, st.Built,
		st.Modules, st.Errors, st.TreeHead)
	return 0
}

// staleNote reads the index's commit, and warns when the build the index read is not the tree's current commit, or
// when the tree's commit cannot be read.
func (e *env) staleNote(ctx context.Context) (string, error) {
	c, err := e.store.Meta(ctx, "commit")
	if err != nil {
		return "", err
	}
	switch h, err := declindex.TreeHead(ctx, e.cfg.Tree); {
	case err != nil:
		fmt.Fprintf(e.stderr, "note: cannot compare the index's commit with the tree's: %v\n", err)
	case c != h:
		fmt.Fprintf(e.stderr, "note: the index is at %s, the tree at %s; run declindex refresh after the build\n", short(c), short(h))
	}
	return c, nil
}

// openQuery opens the index read-only for a query and notes a stale commit (staleNote). It gives the index's commit
// and 0 to go on, or the exit code of a failure that it has reported, having closed the index it opened.
func (e *env) openQuery(ctx context.Context) (string, int) {
	if err := e.openReadOnly(ctx, declindex.IndexOnly, true); err != nil {
		return "", fail(e.stderr, err)
	}
	commit, err := e.staleNote(ctx)
	if err != nil {
		code := fail(e.stderr, &unableError{err})
		e.close()
		return "", code
	}
	return commit, 0
}

func short(c string) string { return c[:min(9, len(c))] }

// names gives a command's NAME arguments and 0 to go on, or the exit code of the usage failure it has reported.
func names(e *env, cmd string, args []string) ([]string, int) {
	if len(args) == 0 {
		return nil, fail(e.stderr, &usageError{cmd + " needs at least one NAME"})
	}
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			return nil, fail(e.stderr, &usageError{fmt.Sprintf("unknown option %s for %s", a, cmd)})
		}
	}
	return args, 0
}

func cmdWhere(ctx context.Context, e *env, args []string) int {
	qs, code := names(e, "where", args)
	if code != 0 {
		return code
	}
	if _, code = e.openQuery(ctx); code != 0 {
		return code
	}
	defer e.close()
	var all []declindex.Found
	rc := 0
	for _, q := range qs {
		found, code := e.resolve(ctx, q, &rc)
		if code != 0 {
			return code
		}
		all = append(all, found...)
	}
	if e.g.json {
		return e.printJSONOr(all, rc)
	}
	for _, f := range all {
		fmt.Fprintf(e.stdout, "%s\t%s\t%s\t%s\n", f.Name, f.Kind, f.Module, f.At())
	}
	return rc
}

func cmdHash(ctx context.Context, e *env, args []string) int {
	qs, code := names(e, "hash", args)
	if code != 0 {
		return code
	}
	if _, code = e.openQuery(ctx); code != 0 {
		return code
	}
	defer e.close()
	rc := 0
	type hashed struct {
		Declaration string `json:"declaration"`
		Hash        string `json:"hash"`
		At          string `json:"at"`
	}
	var out []hashed
	for _, q := range qs {
		found, code := e.resolve(ctx, q, &rc)
		if code != 0 {
			return code
		}
		for _, f := range found {
			out = append(out, hashed{f.Name, f.StmtHash, f.At()})
			if f.StmtHash == "" {
				fmt.Fprintf(e.stderr, "declindex: %s has no statement hash: its source file is missing or newer than its build\n", f.Name)
				rc = 1
			}
		}
	}
	if e.g.json {
		return e.printJSONOr(out, rc)
	}
	for _, h := range out {
		hash := h.Hash
		if hash == "" {
			hash = "-"
		}
		fmt.Fprintf(e.stdout, "%s\t%s\t%s\n", h.Declaration, hash, h.At)
	}
	return rc
}

func cmdUsers(ctx context.Context, e *env, args []string) int {
	opt := declindex.AnswerOptions{FileKinds: e.cfg.FileKinds}
	limit := 25
	var qs []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--lines":
			opt.Lines = true
		case "--why":
			opt.Why = true
		case "--limit":
			if i+1 >= len(args) {
				return fail(e.stderr, &usageError{"--limit needs a number"})
			}
			n, err := strconv.Atoi(args[i+1])
			if err != nil || n < 0 {
				return fail(e.stderr, &usageError{"--limit needs a number of 0 or more"})
			}
			limit = n
			i++
		default:
			qs = append(qs, args[i])
		}
	}
	qs, code := names(e, "users", qs)
	if code != 0 {
		return code
	}
	commit, code := e.openQuery(ctx)
	if code != 0 {
		return code
	}
	defer e.close()
	rc := 0
	var answers []declindex.Answer
	for _, q := range qs {
		found, code := e.resolve(ctx, q, &rc)
		if code != 0 {
			return code
		}
		if len(found) > 1 && !e.g.json {
			fmt.Fprintf(e.stdout, "%s: %d declarations share this name; each is answered separately\n", q, len(found))
		}
		for _, f := range found {
			a, err := e.store.Answer(ctx, f, opt)
			if err != nil {
				return fail(e.stderr, &unableError{err})
			}
			answers = append(answers, a)
			if !e.g.json {
				show(e.stdout, a, limit)
			}
		}
	}
	if e.g.json {
		return e.printJSONOr(map[string]any{"index": commit, "answers": answers}, rc)
	}
	return rc
}

func show(w io.Writer, a declindex.Answer, limit int) {
	if limit == 0 {
		limit = math.MaxInt
	}
	d, r := a.Direct, a.Reach
	fmt.Fprintf(w, "%s  (%s)  %s\n", a.Declaration, a.Kind, a.At)
	fmt.Fprintf(w, "  direct users: %d declarations in %d directories (%d uses in statements, %d in proofs)\n",
		d.Declarations, d.Directories, d.StmtUses, d.ProofUses)
	type dir struct {
		name  string
		total int
		kinds map[string]int
	}
	var dirs []dir
	for name, ks := range d.ByDirectory {
		t := 0
		for _, n := range ks {
			t += n
		}
		dirs = append(dirs, dir{name, t, ks})
	}
	slices.SortFunc(dirs, func(a, b dir) int {
		return cmp.Or(cmp.Compare(b.total, a.total), strings.Compare(a.name, b.name))
	})
	for i, dd := range dirs {
		if i == limit {
			fmt.Fprintf(w, "    ... %d more directories (--limit 0 for all)\n", len(dirs)-limit)
			break
		}
		var ks []string
		for k, n := range dd.kinds {
			ks = append(ks, fmt.Sprintf("%s %d", k, n))
		}
		sort.Strings(ks)
		fmt.Fprintf(w, "    %s  %s\n", dd.name, strings.Join(ks, ", "))
	}
	fmt.Fprintf(w, "  reach of a change to its statement: %d statements change (%d directories), %d proofs to redo "+
		"(%d directories); %d directories in all\n", r.StatementsChanged, r.StatementDirectories, r.ProofsToRedo,
		r.ProofDirectories, r.AllDirectories)
	for i, k := range slices.Sorted(maps.Keys(r.Why)) {
		if i == limit {
			fmt.Fprintf(w, "    ... %d more (--limit 0 for all)\n", len(r.Why)-limit)
			break
		}
		fmt.Fprintf(w, "    %s: %s\n", k, r.Why[k])
	}
	for _, l := range a.Lines {
		fmt.Fprintln(w, "    "+l)
	}
}
