package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sjawhar/legion/orchestration/conceptgate"
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
	Commit    string                       `json:"commit"`
	Contracts declindex.LedgerReport       `json:"contracts"`
	Registry  *ledgerRegistryCommandReport `json:"registry"`
}

type ledgerRegistryCommandReport struct {
	Added   []string                  `json:"added"`
	Defects []conceptgate.Defect      `json:"defects"`
	Near    []conceptgate.RecheckNear `json:"near"`
	Unable  string                    `json:"unable"`
}

func newLedgerRegistryCommandReport() *ledgerRegistryCommandReport {
	return &ledgerRegistryCommandReport{
		Added:   []string{},
		Defects: []conceptgate.Defect{},
		Near:    []conceptgate.RecheckNear{},
	}
}

func (r *ledgerRegistryCommandReport) Defective() bool {
	return len(r.Defects) != 0 || len(r.Near) != 0
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
	registry := recheckLedgerRegistry(ctx, e, r.Commit)
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
	if r.Defective() || registry != nil && registry.Defective() {
		return 1
	}
	if registry != nil && registry.Unable != "" {
		return 3
	}
	return 0
}

func recheckLedgerRegistry(ctx context.Context, e *env, commit string) *ledgerRegistryCommandReport {
	if e.cfg.Registry == "" {
		return nil
	}
	report := newLedgerRegistryCommandReport()
	tree, err := filepath.EvalSymlinks(e.cfg.Tree)
	if err != nil {
		report.Unable = fmt.Sprintf("resolve tree: %v", err)
		return report
	}
	registry, err := e.cfg.GateRegistryDir()
	if err != nil {
		report.Unable = err.Error()
		return report
	}
	relative, err := registryPathRelativeToTree(tree, registry)
	if err != nil {
		report.Unable = err.Error()
		return report
	}
	previous, err := e.store.Meta(ctx, "prev_commit")
	if err != nil {
		report.Unable = fmt.Sprintf("read prev_commit: %v", err)
		return report
	}
	added, err := addedRegistryConcepts(ctx, tree, relative, previous, commit)
	if err != nil {
		report.Unable = err.Error()
		return report
	}
	report.Added = added
	snapshot, cleanup, err := snapshotGateRegistry(ctx, tree, commit, relative, filepath.Dir(e.cfg.DB))
	if err != nil {
		report.Unable = fmt.Sprintf("snapshot registry: %v", err)
		return report
	}
	defer cleanup()
	reg, defects, err := conceptgate.Load(snapshot)
	if err != nil {
		report.Unable = fmt.Sprintf("load registry: %v", err)
		return report
	}
	report.Defects = append(report.Defects, defects...)
	if len(added) == 0 {
		return report
	}
	recheckEnv := *e
	recheckEnv.cfg.Tree, recheckEnv.cfg.Registry = tree, snapshot
	compiled, err := compiledOptions(ctx, &recheckEnv)
	if err != nil {
		report.Unable = fmt.Sprintf("prepare registry recheck: %v", err)
		return report
	}
	gate := &conceptgate.Gate{
		Index:     e.store,
		Tree:      recheckEnv.cfg.TreeSpec(),
		Registry:  snapshot,
		Compiled:  compiled,
		Librarian: conceptgate.CommandLibrarian{Command: e.cfg.Librarian, WorkDir: filepath.Join(filepath.Dir(e.cfg.DB), "proposals")},
		WorkDir:   filepath.Join(filepath.Dir(e.cfg.DB), "proposals"),
	}
	rechecked, err := conceptgate.Recheck(ctx, gate, reg, added)
	report.Defects = append(report.Defects, rechecked.Defects...)
	report.Near = append(report.Near, rechecked.Near...)
	if err != nil {
		report.Unable = err.Error()
	}
	return report
}

func registryPathRelativeToTree(tree, registry string) (string, error) {
	relative, err := filepath.Rel(tree, registry)
	if err != nil {
		return "", fmt.Errorf("find registry relative to tree: %w", err)
	}
	relative = filepath.Clean(relative)
	if relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("registry %s is outside tree %s", registry, tree)
	}
	return filepath.ToSlash(relative), nil
}

func addedRegistryConcepts(ctx context.Context, tree, registry, previous, commit string) ([]string, error) {
	if previous == "" || commit == "" {
		return []string{}, nil
	}
	concepts := filepath.ToSlash(filepath.Join(registry, "concepts"))
	raw, err := exec.CommandContext(ctx, "git", "-C", tree, "diff", "--name-only", "--diff-filter=A", "-z", previous, commit, "--", concepts).Output()
	if err != nil {
		return nil, fmt.Errorf("list registry additions from %s to %s: %w", previous, commit, err)
	}
	prefix := concepts + "/"
	var added []string
	for _, path := range strings.Split(string(raw), "\x00") {
		if !strings.HasPrefix(path, prefix) {
			continue
		}
		name := strings.TrimPrefix(path, prefix)
		if strings.Contains(name, "/") || filepath.Ext(name) != ".yaml" {
			continue
		}
		added = append(added, path)
	}
	sort.Strings(added)
	return added, nil
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
