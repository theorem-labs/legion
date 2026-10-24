package main

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/sjawhar/legion/orchestration/conceptgate"
	"github.com/sjawhar/legion/orchestration/declindex"
	"github.com/sjawhar/legion/orchestration/declindex/confine"
)

// compiledOptions are refresh's compiled step: the registry's forms module, if the configuration names a registry,
// and the exporter. A registry defect is a note; the forms leave its concept out.
func compiledOptions(ctx context.Context, e *env) (*declindex.CompiledOptions, error) {
	var forms declindex.FormsModule
	if e.cfg.Registry != "" {
		r, defects, err := conceptgate.Load(e.cfg.Registry)
		if err != nil {
			return nil, err
		}
		for _, defect := range defects {
			fmt.Fprintf(e.stderr, "declindex: registry defect: %s\n", defect)
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
	if _, err := e.staleNote(ctx); err != nil {
		return fail(e.stderr, &unableError{err})
	}
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
		return e.printJSONOr(out, rc)
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
	if _, err := e.staleNote(ctx); err != nil {
		return fail(e.stderr, &unableError{err})
	}
	out := map[string][]string{}
	rc := 0
	for _, q := range qs {
		found, code := e.resolve(ctx, q, &rc)
		if code != 0 {
			return code
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
		return e.printJSONOr(out, rc)
	}
	for _, name := range slices.Sorted(maps.Keys(out)) {
		same := out[name]
		fmt.Fprintf(e.stdout, "%s: %d declarations share a relation atom\n", name, len(same))
		for _, s := range same {
			fmt.Fprintln(e.stdout, "  "+s)
		}
	}
	return rc
}

// gateSandboxAvailable is the gate's cheap check that this machine can build the sandbox
// (conceptgate.Options.SandboxAvailable); a test replaces it.
var gateSandboxAvailable = confine.Available

// cmdGate decides one proposal. Exit codes: 0 added, 1 refused (an exact duplicate), 4 near (sent to the
// librarian), 5 the name is taken, 3 unable to decide.
func cmdGate(ctx context.Context, e *env, args []string) int {
	var proposer, acceptedBy string
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		if len(args) < 2 {
			return fail(e.stderr, &usageError{args[0] + " needs a value"})
		}
		switch args[0] {
		case "--proposer":
			proposer = args[1]
		case "--accepted-by":
			acceptedBy = args[1]
		default:
			return fail(e.stderr, &usageError{"unknown gate option " + args[0]})
		}
		args = args[2:]
	}
	if proposer == "" {
		return fail(e.stderr, &usageError{"gate requires --proposer <owner id>"})
	}
	if err := conceptgate.ValidateOwnerID(proposer); err != nil {
		return fail(e.stderr, &usageError{err.Error()})
	}
	if acceptedBy != "" {
		if err := conceptgate.ValidateOwnerID(acceptedBy); err != nil {
			return fail(e.stderr, &usageError{err.Error()})
		}
	}
	if len(args) != 1 {
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
	g, err := conceptgate.OpenGate(ctx, e.cfg, conceptgate.Options{Log: e.stderr, SandboxAvailable: gateSandboxAvailable})
	if err != nil {
		return fail(e.stderr, &unableError{err})
	}
	defer g.Close()
	v, err := g.Decide(ctx, p, conceptgate.DecideOptions{Proposer: proposer, AcceptedBy: acceptedBy, ProposalBytes: raw})
	for _, defect := range v.Defects {
		fmt.Fprintf(e.stderr, "declindex: registry defect: %s\n", defect)
	}
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
