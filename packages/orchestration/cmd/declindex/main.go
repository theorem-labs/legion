// Command declindex answers questions about a built Lean project from its index of declarations: where a name is
// declared, who uses it, how far a change to it reaches, its statement hash, and who states a fact. `refresh`
// brings the index to the build after a landing.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"

	"github.com/sjawhar/legion/orchestration/declindex"
)

const usage = "usage: declindex --config FILE [--db FILE] [--json] <refresh|status|where|users|hash|states|same|gate|ledger|land-hash> ..."

const help = usage + `

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
                               for the landing program and never connects to the ledger
  land-hash                    the landing program's hasher: LAND_TREE, LAND_GIT_DIR, LAND_NAMES or LAND_NAMES_FILE
                               (a matching JSON array) in; name<TAB>hash<TAB>path out

NAME is a full name or any dotted suffix of one. A name declared twice is answered once per declaration.
Exit codes:
  refresh: 0 read; 2 usage; 3 unable; 75 retry
  status: 0 answered; 2 usage; 3 unable
  where: 0 answered; 1 name not found; 2 usage; 3 unable
  users: 0 answered; 1 name not found; 2 usage; 3 unable
  hash: 0 answered; 1 name not found; 2 usage; 3 unable
  states: 0 answered; 1 name not found; 2 usage; 3 unable
  same: 0 answered; 1 name not found; 2 usage; 3 unable
  gate: 0 added; 1 duplicate; 2 usage; 3 unable; 4 near; 5 name taken
  ledger: 0 clean; 1 defect; 2 usage; 3 unable; 75 retry
  land-hash: 0 answered; 2 usage; 3 unable`

type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

// unableError is a failure that leaves the question unanswered: never a pass.
type unableError struct{ err error }

func (e *unableError) Error() string { return e.err.Error() }
func (e *unableError) Unwrap() error { return e.err }

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

type globals struct {
	config, db, tree string
	json             bool
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
		case "--config", "--db", "--tree":
			if len(args) < 2 {
				return fail(stderr, &usageError{args[0] + " needs a value"})
			}
			switch args[0] {
			case "--config":
				g.config = args[1]
			case "--db":
				g.db = args[1]
			default:
				g.tree = args[1]
			}
			args = args[2:]
			continue
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
	cfg, err := declindex.LoadConfig(g.config)
	if err != nil {
		return fail(stderr, &unableError{err})
	}
	if g.db != "" {
		cfg.DB = g.db
	}
	if g.tree != "" {
		cfg.Tree = g.tree
	}
	if cfg.DB == "" {
		return fail(stderr, &usageError{declindex.ErrNoDB.Error()})
	}
	return commands[cmd](ctx, &env{g: g, cfg: cfg, stdout: stdout, stderr: stderr, getenv: getenv}, rest)
}

type env struct {
	g      globals
	cfg    declindex.Config
	stdout io.Writer
	stderr io.Writer
	getenv func(string) string
	store  *declindex.Store
}

func (e *env) open(needIndex bool) error {
	return e.openWith(needIndex, declindex.Open)
}

func (e *env) openReadOnly(needIndex bool) error {
	return e.openWith(needIndex, declindex.OpenReadOnly)
}

func (e *env) openWith(needIndex bool, open func(string) (*declindex.Store, error)) error {
	s, err := open(e.cfg.DB)
	if err != nil {
		return &unableError{err}
	}
	e.store = s
	if !needIndex {
		return nil
	}
	built, err := s.Meta(context.Background(), "built")
	if err != nil {
		return &unableError{err}
	}
	if built == "" {
		return &unableError{fmt.Errorf("the index at %s was never refreshed: run declindex refresh", e.cfg.DB)}
	}
	if n, err := s.Meta(context.Background(), "errors"); err != nil {
		return &unableError{err}
	} else if n != "0" {
		return &unableError{fmt.Errorf("the index at %s is incomplete: %s modules' reference files could not be read", e.cfg.DB, n)}
	}
	return nil
}

func (e *env) close() {
	if e.store != nil {
		_ = e.store.Close()
	}
}

var commands map[string]func(context.Context, *env, []string) int

func init() {
	commands = map[string]func(context.Context, *env, []string) int{
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
}

func fail(w io.Writer, err error) int {
	fmt.Fprintln(w, "declindex: "+err.Error())
	var u *usageError
	var nf *declindex.NotFoundError
	var temporary *declindex.TempError
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

func treeHead(tree string) string {
	out, err := exec.Command("git", "-C", tree, "rev-parse", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func (e *env) printJSON(v any) int {
	enc := json.NewEncoder(e.stdout)
	enc.SetIndent("", " ")
	if err := enc.Encode(v); err != nil {
		return fail(e.stderr, &unableError{err})
	}
	return 0
}

func cmdRefresh(ctx context.Context, e *env, args []string) int {
	if len(args) != 0 {
		return fail(e.stderr, &usageError{"refresh takes no arguments"})
	}
	if err := e.open(false); err != nil {
		return fail(e.stderr, err)
	}
	defer e.close()
	tree := e.cfg.TreeSpec()
	opt := declindex.RefreshOptions{Commit: treeHead(e.cfg.Tree), Jobs: e.cfg.Jobs, Log: e.stderr}
	var sync func(context.Context) error
	if commit := e.getenv("LAND_COMMIT"); commit != "" {
		if !isCommitID(commit) {
			return fail(e.stderr, &usageError{"LAND_COMMIT must be 40 hexadecimal characters"})
		}
		previous, err := e.store.Meta(ctx, "commit")
		if err != nil {
			return fail(e.stderr, &unableError{err})
		}
		opt.Commit, opt.PreviousCommit, opt.CheckLandedCoverage = commit, previous, true
		sync = declindex.Sync(e.cfg, commit, e.stderr)
	}
	st, err := e.store.RefreshAt(ctx, tree, opt, sync)
	if errors.Is(err, declindex.ErrAlreadyPast) {
		return 0
	}
	if err == nil && e.cfg.Lean.Export {
		// The forms module imports the registered concepts' modules, which the index knows only once the
		// reference files are read: the compiled part runs second.
		var co *declindex.CompiledOptions
		if co, err = compiledOptions(ctx, e); err == nil {
			opt.Compiled = co
			var cst declindex.RefreshStats
			cst, err = e.store.Refresh(ctx, tree, opt)
			st.Exported, st.Rematched, st.Elapsed = cst.Exported, cst.Rematched, st.Elapsed+cst.Elapsed
		}
	}
	if err != nil {
		return fail(e.stderr, &unableError{err})
	}
	n, err := e.store.ModuleCount(ctx)
	if err != nil || n == 0 || st.Unreadable > 0 {
		fmt.Fprintf(e.stderr, "declindex: the index is incomplete: %d modules, %d unreadable reference files\n", n, st.Unreadable)
		return 3
	}
	if e.g.json {
		return e.printJSON(st)
	}
	return 0
}

func isCommitID(commit string) bool {
	if len(commit) != 40 {
		return false
	}
	for _, r := range commit {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return true
}

type status struct {
	Commit   string `json:"commit"`
	Built    string `json:"built"`
	Modules  int    `json:"modules"`
	Errors   string `json:"errors"`
	TreeHead string `json:"tree_head"`
}

func cmdStatus(ctx context.Context, e *env, args []string) int {
	if len(args) != 0 {
		return fail(e.stderr, &usageError{"status takes no arguments"})
	}
	if err := e.open(false); err != nil {
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
	st.TreeHead = treeHead(e.cfg.Tree)
	if e.g.json {
		return e.printJSON(st)
	}
	fmt.Fprintf(e.stdout, "index at %s, built %s, %d modules, %s unreadable; tree at %s\n", st.Commit, st.Built,
		st.Modules, st.Errors, st.TreeHead)
	return 0
}

// staleNote warns when the build the index read is not the tree's current commit.
func (e *env) staleNote(ctx context.Context) string {
	c, _ := e.store.Meta(ctx, "commit")
	if h := treeHead(e.cfg.Tree); h != "" && c != h {
		fmt.Fprintf(e.stderr, "note: the index is at %s, the tree at %s; run declindex refresh after the build\n", short(c), short(h))
	}
	return c
}

func short(c string) string { return c[:min(9, len(c))] }

func names(e *env, cmd string, args []string) ([]string, int) {
	if len(args) == 0 {
		return nil, fail(e.stderr, &usageError{cmd + " needs at least one NAME"})
	}
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			return nil, fail(e.stderr, &usageError{fmt.Sprintf("unknown option %s for %s", a, cmd)})
		}
	}
	return args, -1
}

func cmdWhere(ctx context.Context, e *env, args []string) int {
	qs, code := names(e, "where", args)
	if code >= 0 {
		return code
	}
	if err := e.open(true); err != nil {
		return fail(e.stderr, err)
	}
	defer e.close()
	e.staleNote(ctx)
	var all []declindex.Found
	rc := 0
	for _, q := range qs {
		found, err := e.store.Resolve(ctx, q)
		if err != nil {
			return fail(e.stderr, &unableError{err})
		}
		if len(found) == 0 {
			rc = fail(e.stderr, &declindex.NotFoundError{Name: q})
			continue
		}
		all = append(all, found...)
	}
	if e.g.json {
		if c := e.printJSON(all); c != 0 {
			return c
		}
		return rc
	}
	for _, f := range all {
		fmt.Fprintf(e.stdout, "%s\t%s\t%s\t%s\n", f.Name, f.Kind, f.Module, f.At())
	}
	return rc
}

func cmdHash(ctx context.Context, e *env, args []string) int {
	qs, code := names(e, "hash", args)
	if code >= 0 {
		return code
	}
	if err := e.open(true); err != nil {
		return fail(e.stderr, err)
	}
	defer e.close()
	e.staleNote(ctx)
	rc := 0
	type hashed struct {
		Declaration string `json:"declaration"`
		Hash        string `json:"hash"`
		At          string `json:"at"`
	}
	var out []hashed
	for _, q := range qs {
		found, err := e.store.Resolve(ctx, q)
		if err != nil {
			return fail(e.stderr, &unableError{err})
		}
		if len(found) == 0 {
			rc = fail(e.stderr, &declindex.NotFoundError{Name: q})
			continue
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
		if c := e.printJSON(out); c != 0 {
			return c
		}
		return rc
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
	if code >= 0 {
		return code
	}
	if err := e.open(true); err != nil {
		return fail(e.stderr, err)
	}
	defer e.close()
	commit := e.staleNote(ctx)
	rc := 0
	var answers []declindex.Answer
	for _, q := range qs {
		found, err := e.store.Resolve(ctx, q)
		if err != nil {
			return fail(e.stderr, &unableError{err})
		}
		if len(found) == 0 {
			rc = fail(e.stderr, &declindex.NotFoundError{Name: q})
			continue
		}
		if len(found) > 1 && !e.g.json {
			fmt.Fprintf(e.stdout, "%s: %d declarations share this name; each is answered separately\n", q, len(found))
		}
		for _, f := range found {
			a, err := e.store.Answer(ctx, f.Name, opt)
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
		if c := e.printJSON(map[string]any{"index": commit, "answers": answers}); c != 0 {
			return c
		}
	}
	return rc
}

func show(w io.Writer, a declindex.Answer, limit int) {
	if limit == 0 {
		limit = int(^uint(0) >> 1)
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
	sort.Slice(dirs, func(i, j int) bool {
		if dirs[i].total != dirs[j].total {
			return dirs[i].total > dirs[j].total
		}
		return dirs[i].name < dirs[j].name
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
	if r.Why != nil {
		var ds []string
		for k := range r.Why {
			ds = append(ds, k)
		}
		sort.Strings(ds)
		for i, k := range ds {
			if i == limit {
				fmt.Fprintf(w, "    ... %d more (--limit 0 for all)\n", len(ds)-limit)
				break
			}
			fmt.Fprintf(w, "    %s: %s\n", k, r.Why[k])
		}
	}
	for _, l := range a.Lines {
		fmt.Fprintln(w, "    "+l)
	}
}
