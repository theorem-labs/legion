package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"

	"github.com/sjawhar/legion/orchestration/declindex"
	"github.com/sjawhar/legion/orchestration/ledger"
)

type ledgerOperation struct {
	Op            string `json:"op"`
	Name          string `json:"name"`
	Owner         string `json:"owner"`
	StatementHash string `json:"statement_hash"`
	VersionRead   int64  `json:"version_read"`
}

type contractsFileLedger struct {
	contracts  []ledger.Contract
	operations []ledgerOperation
}

func (l *contractsFileLedger) Contracts(context.Context, string) ([]ledger.Contract, error) {
	return l.contracts, nil
}

func (l *contractsFileLedger) RegisterContract(_ context.Context, _ string, name, owner, hash string, _ bool) (ledger.Contract, error) {
	l.operations = append(l.operations, ledgerOperation{
		Op: "register", Name: name, Owner: owner, StatementHash: hash,
	})
	return ledger.Contract{Name: name, Owner: owner, StatementHash: hash, Version: 1}, nil
}

type ledgerCommandReport struct {
	Commit    string                 `json:"commit"`
	Contracts declindex.LedgerReport `json:"contracts"`
	Registry  any                    `json:"registry"`
}

// cmdLedger runs after a landing. With a contracts file, it emits the registrations that the landing program
// applies in its LAND_COMMIT transaction; a DSN is reserved for an operator's manual check.
func cmdLedger(ctx context.Context, e *env, args []string) int {
	dsn := e.getenv("LEDGER_DSN")
	var contractsFile, emit string
	for i := 0; i < len(args); i++ {
		if i+1 >= len(args) {
			return fail(e.stderr, &usageError{"ledger options need a path"})
		}
		switch args[i] {
		case "--dsn-file":
			raw, err := os.ReadFile(args[i+1])
			if err != nil {
				return fail(e.stderr, &unableError{err})
			}
			dsn = strings.TrimSpace(string(raw))
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
	if contractsFile == "" && dsn == "" {
		return fail(e.stderr, &usageError{"LEDGER_DSN or --dsn-file is required for an operator ledger check"})
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
	if err := e.open(true); err != nil {
		return fail(e.stderr, err)
	}
	defer e.close()
	indexCommit := e.staleNote(ctx)
	if indexCommit != treeHead(e.cfg.Tree) {
		return fail(e.stderr, &unableError{fmt.Errorf("the index is at %q, not the tree's commit: refresh it after the landing first", indexCommit)})
	}
	if commit := e.getenv("LAND_COMMIT"); commit != "" {
		if !isCommitID(commit) {
			return fail(e.stderr, &usageError{"LAND_COMMIT must be 40 hexadecimal characters"})
		}
		covers, err := ledgerCommitCovers(ctx, e.cfg.Tree, commit, indexCommit)
		if err != nil {
			return fail(e.stderr, &unableError{err})
		}
		if !covers {
			return fail(e.stderr, &unableError{fmt.Errorf("LAND_COMMIT %s is neither the index's commit nor its ancestor", commit)})
		}
	}

	var l declindex.ContractLedger
	var emitted *contractsFileLedger
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
	if emitted != nil {
		if err := writeLedgerOperations(emit, emitted.operations); err != nil {
			return fail(e.stderr, &unableError{err})
		}
	}
	if c := e.printJSON(ledgerCommandReport{Commit: r.Commit, Contracts: r, Registry: nil}); c != 0 {
		return c
	}
	fmt.Fprintf(e.stderr, "declindex ledger at %s: %d contracts, %d registered, %d defects, %d gone, %d unowned, %d ambiguous; registry: 0 added, 0 near, 0 defects\n",
		r.Commit, r.Contracts, len(r.Registered), len(r.Defects), len(r.Gone), len(r.Unowned), len(r.Ambiguous))
	if r.Defective() {
		return 1
	}
	return 0
}

func ledgerCommitCovers(ctx context.Context, tree, landed, indexed string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", tree, "merge-base", "--is-ancestor", landed, indexed)
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return false, nil
		}
		return false, fmt.Errorf("compare LAND_COMMIT %s with index commit %s: %w", landed, indexed, err)
	}
	return true, nil
}
func temporaryLedgerError(err error) error {
	var networkErr net.Error
	if errors.As(err, &networkErr) || errors.Is(err, context.DeadlineExceeded) {
		return &declindex.TempError{Err: err}
	}
	return err
}

func writeLedgerOperations(path string, operations []ledgerOperation) error {
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
