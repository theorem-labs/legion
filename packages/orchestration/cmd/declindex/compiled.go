package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sjawhar/legion/orchestration/conceptgate"
	"github.com/sjawhar/legion/orchestration/declindex"
	"github.com/sjawhar/legion/orchestration/ledger"
)

// compiledOptions reads the registry, if the configuration names one, and prepares the exporter.
func compiledOptions(ctx context.Context, e *env) (*declindex.CompiledOptions, error) {
	var forms declindex.FormsModule
	if e.cfg.Registry != "" {
		r, err := conceptgate.Load(e.cfg.Registry)
		if err != nil {
			return nil, err
		}
		if forms, err = conceptgate.Forms(ctx, e.store, r); err != nil {
			return nil, err
		}
	}
	return e.cfg.Compiled(ctx, forms, e.stderr)
}

func cmdStates(ctx context.Context, e *env, args []string) int {
	concepts, code := names(e, "states", args)
	if code >= 0 {
		return code
	}
	if err := e.open(true); err != nil {
		return fail(e.stderr, err)
	}
	defer e.close()
	e.staleNote(ctx)
	out := map[string][]declindex.Stating{}
	rc := 0
	for _, c := range concepts {
		rows, err := e.store.States(ctx, c)
		if err != nil {
			return fail(e.stderr, &unableError{err})
		}
		if len(rows) == 0 {
			fmt.Fprintf(e.stderr, "declindex: no statement states %s (is it registered, and the index read with the registry?)\n", c)
			rc = 1
		}
		out[c] = rows
	}
	if e.g.json {
		if c := e.printJSON(out); c != 0 {
			return c
		}
		return rc
	}
	for _, c := range concepts {
		decls := map[string]bool{}
		for _, r := range out[c] {
			decls[r.Decl] = true
		}
		fmt.Fprintf(e.stdout, "%s: %d declarations\n", c, len(decls))
		for _, r := range out[c] {
			fmt.Fprintf(e.stdout, "  %s\t%s\t%s\n", r.Decl, r.How, r.At)
		}
	}
	return rc
}

func cmdSame(ctx context.Context, e *env, args []string) int {
	qs, code := names(e, "same", args)
	if code >= 0 {
		return code
	}
	if err := e.open(true); err != nil {
		return fail(e.stderr, err)
	}
	defer e.close()
	e.staleNote(ctx)
	out := map[string][]string{}
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
		for _, f := range found {
			same, err := e.store.SameAtoms(ctx, f.Name)
			if err != nil {
				return fail(e.stderr, &unableError{err})
			}
			out[f.Name] = same
		}
	}
	if e.g.json {
		if c := e.printJSON(out); c != 0 {
			return c
		}
		return rc
	}
	for name, same := range out {
		fmt.Fprintf(e.stdout, "%s: %d declarations share a relation atom\n", name, len(same))
		for _, s := range same {
			fmt.Fprintln(e.stdout, "  "+s)
		}
	}
	return rc
}

// cmdGate decides one proposal. Exit codes: 0 added, 1 refused (an exact duplicate), 4 near (sent to the
// librarian), 3 unable to decide.
func cmdGate(ctx context.Context, e *env, args []string) int {
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		return fail(e.stderr, &usageError{"gate takes one PROPOSAL.json"})
	}
	if e.cfg.Registry == "" {
		return fail(e.stderr, &usageError{"the configuration names no registry"})
	}
	raw, err := os.ReadFile(args[0])
	if err != nil {
		return fail(e.stderr, &unableError{err})
	}
	var p conceptgate.Proposal
	if err := json.Unmarshal(raw, &p); err != nil {
		return fail(e.stderr, &usageError{fmt.Sprintf("%s: %v", args[0], err)})
	}
	if err := e.open(true); err != nil {
		return fail(e.stderr, err)
	}
	defer e.close()
	e.staleNote(ctx)
	co, err := compiledOptions(ctx, e)
	if err != nil {
		return fail(e.stderr, &unableError{err})
	}
	g := &conceptgate.Gate{Index: e.store, Tree: e.cfg.TreeSpec(), Registry: e.cfg.Registry, Compiled: co,
		Librarian: conceptgate.CommandLibrarian{Command: e.cfg.Librarian}, WorkDir: filepath.Join(filepath.Dir(e.cfg.DB), "proposals")}
	v, err := g.Decide(ctx, p)
	if err != nil {
		return fail(e.stderr, &unableError{err})
	}
	if c := e.printJSON(v); c != 0 {
		return c
	}
	switch v.Outcome {
	case conceptgate.Refused:
		return 1
	case conceptgate.Near:
		return 4
	}
	return 0
}

// cmdLedger runs after a landing: it registers new contracts in the ledger and reports every contract whose
// ledger hash is not the index's. Exit code 1 when it finds a defect.
func cmdLedger(ctx context.Context, e *env, args []string) int {
	dsn := e.getenv("LEDGER_DSN")
	for i := 0; i < len(args); i++ {
		if args[i] != "--dsn-file" || i+1 >= len(args) {
			return fail(e.stderr, &usageError{"ledger takes only --dsn-file PATH"})
		}
		raw, err := os.ReadFile(args[i+1])
		if err != nil {
			return fail(e.stderr, &unableError{err})
		}
		dsn = strings.TrimSpace(string(raw))
		i++
	}
	if dsn == "" {
		return fail(e.stderr, &usageError{"LEDGER_DSN or --dsn-file is required"})
	}
	if e.cfg.Repo == "" {
		return fail(e.stderr, &usageError{"the configuration names no repo"})
	}
	owners := declindex.Owners{}
	if e.cfg.Owners != "" {
		var err error
		if owners, err = declindex.LoadOwners(e.cfg.Owners); err != nil {
			return fail(e.stderr, &unableError{err})
		}
	}
	if err := e.open(true); err != nil {
		return fail(e.stderr, err)
	}
	defer e.close()
	e.staleNote(ctx)
	l, err := ledger.Open(ctx, dsn)
	if err != nil {
		return fail(e.stderr, &unableError{err})
	}
	defer l.Close()
	r, err := e.store.CheckLedger(ctx, l, e.cfg.Repo, e.cfg.Contracts, owners)
	if err != nil {
		var refusal *ledger.Refusal
		if errors.As(err, &refusal) {
			fmt.Fprintf(e.stderr, "declindex: the ledger refused: %v\n", refusal)
		}
		return fail(e.stderr, &unableError{err})
	}
	if c := e.printJSON(r); c != 0 {
		return c
	}
	if len(r.Defects) > 0 {
		return 1
	}
	return 0
}
