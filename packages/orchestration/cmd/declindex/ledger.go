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
	if landed != "" && !isCommitID(landed) {
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
	indexCommit := e.staleNote(ctx)
	if indexCommit != treeHead(e.cfg.Tree) {
		return fail(e.stderr, &unableError{fmt.Errorf("the index is at %q, not the tree's commit: refresh it after the landing first", indexCommit)})
	}
	if landed != "" {
		covers, err := ledgerCommitCovers(ctx, e.cfg.Tree, landed, indexCommit)
		if err != nil {
			return fail(e.stderr, &unableError{err})
		}
		if !covers {
			return fail(e.stderr, &unableError{fmt.Errorf("LAND_COMMIT %s is neither the index's commit nor its ancestor", landed)})
		}
	}

	var l declindex.ContractLedger
	var emitted *contractsFileLedger
	var librarian conceptgate.Librarian
	var emittedReviews *emittedLibrarian
	work, err := os.MkdirTemp("", "declindex-ledger-")
	if err != nil {
		return fail(e.stderr, &unableError{err})
	}
	defer os.RemoveAll(work)
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
		librarian = conceptgate.CommandLibrarian{Command: e.cfg.Librarian, WorkDir: filepath.Join(work, "librarian")}
	}
	r, err := e.store.CheckLedger(ctx, l, e.cfg.Repo, e.cfg.Contracts, owners)
	if err != nil {
		var refusal *ledger.Refusal
		if errors.As(err, &refusal) {
			fmt.Fprintf(e.stderr, "declindex: the ledger refused: %v\n", refusal)
		}
		return fail(e.stderr, &unableError{err})
	}
	registry := recheckLedgerRegistry(ctx, e, r.Commit, landed, librarian, work)
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

// recheckLedgerRegistry rechecks the registry concepts that the landing of landed (the index's commit when empty)
// added, as committed at the index's commit. A cause that stops the recheck is the report's Unable.
func recheckLedgerRegistry(ctx context.Context, e *env, indexCommit, landed string, librarian conceptgate.Librarian, work string) *ledgerRegistryCommandReport {
	if e.cfg.Registry == "" {
		return nil
	}
	report := newLedgerRegistryCommandReport()
	if err := fillRegistryRecheck(ctx, e, indexCommit, landed, librarian, work, report); err != nil {
		report.Unable = err.Error()
	}
	return report
}

func fillRegistryRecheck(ctx context.Context, e *env, indexCommit, landed string, librarian conceptgate.Librarian, work string, report *ledgerRegistryCommandReport) error {
	tree, err := filepath.EvalSymlinks(e.cfg.Tree)
	if err != nil {
		return fmt.Errorf("resolve tree: %w", err)
	}
	registry, err := e.cfg.GateRegistryDir()
	if err != nil {
		return err
	}
	relative, err := registryPathRelativeToTree(tree, registry)
	if err != nil {
		return err
	}
	// The tree's path in its repository, which refresh recorded: Git prints the paths of a diff from the
	// repository's root, and the report names an added file by that repository-relative path, as the gate does.
	prefix, err := e.store.Meta(ctx, "source_prefix")
	if err != nil {
		return err
	}
	if landed == "" {
		landed = indexCommit
	}
	previous, recorded, err := e.store.Landing(ctx, landed)
	if err != nil {
		return err
	}
	switch {
	case recorded:
		if report.Added, err = addedRegistryConcepts(ctx, tree, prefix, relative, previous, landed); err != nil {
			return err
		}
	case landed == indexCommit:
		return fmt.Errorf("the index has no record of the landing of its commit %s: refresh the index", landed)
	default:
		// A later landing's refresh read this commit; the check of that landing rechecks this one's additions.
		fmt.Fprintf(e.stderr, "note: no refresh stopped at %s; the check of the landing that read it rechecks its additions\n", short(landed))
	}
	snapshot, cleanup, err := snapshotGateRegistry(ctx, tree, indexCommit, relative, work)
	if err != nil {
		return fmt.Errorf("snapshot registry: %w", err)
	}
	defer cleanup()
	reg, defects, err := conceptgate.Load(snapshot)
	if err != nil {
		return fmt.Errorf("load registry: %w", err)
	}
	report.Defects = append(report.Defects, defects...)
	// An added file that the loader refused or keyed wrongly is already a defect; the rest are rechecked.
	loaded := map[string]bool{}
	for _, c := range reg.Concepts {
		loaded[prefix+filepath.ToSlash(filepath.Join(relative, "concepts", conceptgate.Key(c)+".yaml"))] = true
	}
	var recheck []string
	for _, path := range report.Added {
		if loaded[path] {
			recheck = append(recheck, path)
		}
	}
	if len(recheck) == 0 {
		return nil
	}
	compiled, err := e.cfg.Compiled(ctx, declindex.FormsModule{}, e.stderr)
	if err != nil {
		return fmt.Errorf("prepare registry recheck: %w", err)
	}
	spec := e.cfg.TreeSpec()
	spec.Source = tree
	gate := &conceptgate.Gate{
		Index:     e.store,
		Tree:      spec,
		Registry:  snapshot,
		Compiled:  compiled,
		Librarian: librarian,
		WorkDir:   filepath.Join(work, "proposals"),
	}
	rechecked, err := conceptgate.Recheck(ctx, gate, reg, recheck)
	report.Defects = append(report.Defects, rechecked.Defects...)
	report.Near = append(report.Near, rechecked.Near...)
	return err
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

// addedRegistryConcepts lists the concept files (the .yaml files directly under concepts/, which the loader
// reads) that the commits previous..commit added, by their paths from the repository's root. registry is the
// registry's path in the tree, and prefix the tree's path in the repository (empty at its root, else ending in a
// slash). Rename detection is off: a file added in the landing that deleted a similar one is an addition.
func addedRegistryConcepts(ctx context.Context, tree, prefix, registry, previous, commit string) ([]string, error) {
	added := []string{}
	if previous == "" || commit == "" {
		return added, nil
	}
	concepts := filepath.ToSlash(filepath.Join(registry, "concepts"))
	// The pathspec is relative to the tree, where git runs; the paths it prints are relative to the repository.
	raw, err := exec.CommandContext(ctx, "git", "-C", tree, "diff", "--name-only", "--no-renames", "--diff-filter=A", "-z", previous, commit, "--", concepts).Output()
	if err != nil {
		return nil, fmt.Errorf("list registry additions from %s to %s: %w", previous, commit, err)
	}
	prefix += concepts + "/"
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
