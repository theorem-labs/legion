package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/sjawhar/legion/orchestration/conceptgate"
	"github.com/sjawhar/legion/orchestration/declindex"
	"github.com/sjawhar/legion/orchestration/ledger"
)

// registerOperation is one registration that `ledger --emit` writes for the landing program to apply.
type registerOperation struct {
	Op            string `json:"op"`
	Name          string `json:"name"`
	Owner         string `json:"owner"`
	StatementHash string `json:"statement_hash"`
	VersionRead   int64  `json:"version_read"`
}

// librarianOperation is one near pair that `ledger --emit` writes for the landing program to deliver to the
// librarian: the envelope is the exact text that the librarian command would read from its --proposal file.
type librarianOperation struct {
	Op       string `json:"op"`
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Proposer string `json:"proposer"`
	Envelope string `json:"envelope"`
}

type contractsFileLedger struct {
	contracts  []ledger.Contract
	operations []registerOperation
}

func (l *contractsFileLedger) Contracts(context.Context, string) ([]ledger.Contract, error) {
	return l.contracts, nil
}

func (l *contractsFileLedger) RegisterContract(_ context.Context, _ string, name, owner, hash string, _ bool) (ledger.Contract, error) {
	l.operations = append(l.operations, registerOperation{
		Op: "register", Name: name, Owner: owner, StatementHash: hash,
	})
	return ledger.Contract{Name: name, Owner: owner, StatementHash: hash, Version: 1}, nil
}

// emittedLibrarian keeps each review request as an operation: the landing hook has no network, so the landing
// program delivers them.
type emittedLibrarian struct {
	operations []librarianOperation
}

func (l *emittedLibrarian) Review(_ context.Context, r conceptgate.ReviewRequest) error {
	envelope, err := conceptgate.Envelope(r)
	if err != nil {
		return err
	}
	l.operations = append(l.operations, librarianOperation{Op: "librarian", ID: r.ID, Kind: r.Kind, Proposer: r.Proposer, Envelope: envelope})
	return nil
}

type ledgerCommandReport struct {
	Commit    string                     `json:"commit"`
	Contracts declindex.LedgerReport     `json:"contracts"`
	Registry  *conceptgate.LandingReport `json:"registry"`
}

// cmdLedger runs after a landing. With a contracts file, it emits the registrations that the landing program
// applies in its LAND_COMMIT transaction, and the near pairs for it to deliver to the librarian; a DSN is reserved
// for an operator's manual check, which registers and calls the librarian itself.
func cmdLedger(ctx context.Context, e *env, args []string) int {
	var dsnFile, contractsFile, emit string
	for i := 0; i < len(args); i++ {
		if i+1 >= len(args) {
			return fail(e.stderr, &usageError{"ledger options need a path"})
		}
		switch args[i] {
		case "--dsn-file":
			dsnFile = args[i+1]
		case "--contracts-file":
			contractsFile = args[i+1]
		case "--emit":
			emit = args[i+1]
		default:
			return fail(e.stderr, &usageError{"ledger takes only --dsn-file, --contracts-file and --emit"})
		}
		i++
	}
	if (contractsFile == "") != (emit == "") {
		return fail(e.stderr, &usageError{"--contracts-file and --emit must be used together"})
	}
	if dsnFile != "" && contractsFile != "" {
		return fail(e.stderr, &usageError{"--dsn-file is the operator's ledger check; --contracts-file and --emit are the landing hook's: use one"})
	}
	dsn := e.getenv("LEDGER_DSN")
	if dsnFile != "" {
		raw, err := os.ReadFile(dsnFile)
		if err != nil {
			return fail(e.stderr, &unableError{err})
		}
		dsn = strings.TrimSpace(string(raw))
	}
	if contractsFile == "" && dsn == "" {
		return fail(e.stderr, &usageError{"LEDGER_DSN or --dsn-file is required for an operator ledger check"})
	}
	landed := e.getenv("LAND_COMMIT")
	if landed != "" && !declindex.IsObjectID(landed) {
		return fail(e.stderr, &usageError{"LAND_COMMIT must be 40 lowercase hexadecimal characters"})
	}
	if e.cfg.Repo == "" {
		return fail(e.stderr, &usageError{"the configuration names no repo"})
	}
	owners := declindex.Owners{}
	if e.cfg.Owners != "" {
		var err error
		if owners, err = declindex.LoadOwners(e.cfg.Owners, e.cfg.OwnersFormat); err != nil {
			return fail(e.stderr, &unableError{err})
		}
	}
	// The landing hook mounts the index directory read-only.
	if err := e.openReadOnly(ctx, true); err != nil {
		return fail(e.stderr, err)
	}
	defer e.close()
	indexCommit, err := e.store.Meta(ctx, "commit")
	if err != nil {
		return fail(e.stderr, &unableError{err})
	}
	head, err := declindex.TreeHead(ctx, e.cfg.Tree)
	if err != nil {
		return fail(e.stderr, &unableError{err})
	}
	if indexCommit != head {
		return fail(e.stderr, &unableError{fmt.Errorf("the index is at %q, not the tree's commit %s: refresh it after the landing first", indexCommit, head)})
	}
	if landed != "" {
		covers, err := declindex.CommitIsAncestor(ctx, e.cfg.Tree, landed, indexCommit)
		if err != nil {
			return fail(e.stderr, &unableError{err})
		}
		if !covers {
			return fail(e.stderr, &unableError{fmt.Errorf("LAND_COMMIT %s is neither the index's commit nor its ancestor", landed)})
		}
	}

	var l declindex.ContractLedger
	var emitted *contractsFileLedger
	// The recheck's librarian: in emit mode the landing program delivers each request; else (nil) the configured
	// command receives it.
	var librarian conceptgate.Librarian
	var emittedReviews *emittedLibrarian
	if contractsFile != "" {
		raw, err := os.ReadFile(contractsFile)
		if err != nil {
			return fail(e.stderr, &unableError{err})
		}
		var contracts []ledger.Contract
		if err := json.Unmarshal(raw, &contracts); err != nil || contracts == nil {
			if err == nil {
				err = errors.New("the contracts file must contain a JSON array")
			}
			return fail(e.stderr, &unableError{fmt.Errorf("read contracts file: %w", err)})
		}
		emitted = &contractsFileLedger{contracts: contracts}
		l = emitted
		emittedReviews = &emittedLibrarian{}
		librarian = emittedReviews
	} else {
		existing, err := ledger.OpenExisting(ctx, dsn)
		if err != nil {
			return fail(e.stderr, &unableError{temporaryLedgerError(err)})
		}
		defer existing.Close()
		l = existing
	}
	r, err := e.store.CheckLedger(ctx, l, e.cfg.Repo, e.cfg.Contracts, owners)
	if err != nil {
		var refusal *ledger.Refusal
		if errors.As(err, &refusal) {
			fmt.Fprintf(e.stderr, "declindex: the ledger refused: %v\n", refusal)
		}
		return fail(e.stderr, &unableError{err})
	}
	registry := conceptgate.RecheckLanding(ctx, e.cfg, e.store, r.Commit, landed, conceptgate.Options{Librarian: librarian, Log: e.stderr})
	if emitted != nil {
		operations := make([]any, 0, len(emitted.operations)+len(emittedReviews.operations))
		for _, op := range emitted.operations {
			operations = append(operations, op)
		}
		for _, op := range emittedReviews.operations {
			operations = append(operations, op)
		}
		if err := writeLedgerOperations(emit, operations); err != nil {
			return fail(e.stderr, &unableError{err})
		}
	}
	if c := e.printJSON(ledgerCommandReport{Commit: r.Commit, Contracts: r, Registry: registry}); c != 0 {
		return c
	}
	if registry != nil && registry.Unable != "" {
		fmt.Fprintf(e.stderr, "declindex: registry recheck unavailable: %s\n", registry.Unable)
	}
	added, near, defects := 0, 0, 0
	if registry != nil {
		added, near, defects = len(registry.Added), len(registry.Near), len(registry.Defects)
	}
	fmt.Fprintf(e.stderr, "declindex ledger at %s: %d contracts, %d registered, %d defects, %d gone, %d unowned, %d ambiguous; registry: %d added, %d near, %d defects\n",
		r.Commit, r.Contracts, len(r.Registered), len(r.Defects), len(r.Gone), len(r.Unowned), len(r.Ambiguous), added, near, defects)
	switch {
	case registry != nil && registry.Unable != "":
		// The landing program runs this landing's check again until its recheck runs (or an operator skips it).
		return 75
	case r.Defective() || registry != nil && registry.Defective():
		return 1
	}
	return 0
}

func temporaryLedgerError(err error) error {
	var networkErr net.Error
	if errors.As(err, &networkErr) || errors.Is(err, context.DeadlineExceeded) {
		return &declindex.TempError{Err: err}
	}
	return err
}

func writeLedgerOperations(path string, operations []any) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	for _, operation := range operations {
		if err := enc.Encode(operation); err != nil {
			_ = f.Close()
			return err
		}
	}
	return f.Close()
}
