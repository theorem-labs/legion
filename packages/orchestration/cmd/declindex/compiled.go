package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sjawhar/legion/orchestration/conceptgate"
	"github.com/sjawhar/legion/orchestration/declindex"
	"github.com/sjawhar/legion/orchestration/declindex/confine"
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
// librarian), 5 the name is taken, 3 unable to decide.
func cmdGate(ctx context.Context, e *env, args []string) int {
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		return fail(e.stderr, &usageError{"gate takes one PROPOSAL.json"})
	}
	if e.cfg.Registry == "" {
		return fail(e.stderr, &usageError{"the configuration names no registry"})
	}
	// The gate compiles untrusted text only in the sandbox: where there is none, it starts nothing (no toolchain
	// lookup, no exporter build) and cannot decide.
	if err := confine.Available(); err != nil {
		return fail(e.stderr, &unableError{fmt.Errorf("the gate compiles a proposal only in the sandbox: %w", err)})
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
	case conceptgate.NameTaken:
		return 5
	}
	return 0
}
