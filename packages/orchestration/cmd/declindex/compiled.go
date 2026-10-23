package main

import (
	"context"
	"fmt"

	"github.com/sjawhar/legion/orchestration/conceptgate"
	"github.com/sjawhar/legion/orchestration/declindex"
)

// compiledOptions reads the registry, if the configuration names one, and prepares the exporter.
func compiledOptions(ctx context.Context, e *env) (*declindex.CompiledOptions, error) {
	var forms declindex.FormsModule
	if e.cfg.Registry != "" {
		r, err := conceptgate.Load(e.cfg.Registry)
		if err != nil {
			return nil, err
		}
		forms = r.FormsModule()
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
