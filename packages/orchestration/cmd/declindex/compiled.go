package main

import (
	"archive/tar"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
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

var (
	gateSandboxAvailable    = confine.Available
	gateAfterRegistryStatus func()
)

func gateRegistryClean(tree, rel string) error {
	status, err := exec.Command("git", "-C", tree, "status", "--porcelain", "--untracked-files=all", "--", rel).Output()
	if err != nil {
		return fmt.Errorf("check registry changes: %w", err)
	}
	if len(status) != 0 {
		return fmt.Errorf("the registry has local changes")
	}
	return nil
}

// snapshotGateRegistry extracts the registry from the verified index commit outside the tree. All gate reads use
// this immutable snapshot, never the caller's mutable working-tree directory.
func snapshotGateRegistry(ctx context.Context, tree, commit, rel, base string) (string, func(), error) {
	root, err := os.MkdirTemp(base, "registry-snapshot-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(root) }
	cmd := exec.CommandContext(ctx, "git", "-C", tree, "archive", "--format=tar", commit, "--", rel)
	out, err := cmd.StdoutPipe()
	if err != nil {
		cleanup()
		return "", nil, err
	}
	if err := cmd.Start(); err != nil {
		cleanup()
		return "", nil, err
	}
	fail := func(err error) (string, func(), error) {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		cleanup()
		return "", nil, err
	}
	if err := extractRegistryArchive(out, root, rel); err != nil {
		return fail(err)
	}
	if _, err := io.Copy(io.Discard, out); err != nil {
		return fail(err)
	}
	if err := cmd.Wait(); err != nil {
		cleanup()
		return "", nil, err
	}
	registry := filepath.Join(root, rel)
	if info, err := os.Stat(registry); err != nil || !info.IsDir() {
		cleanup()
		if err != nil {
			return "", nil, err
		}
		return "", nil, fmt.Errorf("the index commit has no registry directory %s", rel)
	}
	return registry, cleanup, nil
}

// extractRegistryArchive writes the directories and regular files of a `git archive` tar of rel below root. An
// entry whose name is not local (absolute, or leaving the root through `..`) or lies outside rel is refused before
// anything is written for it, and links and other entry types are refused, so no entry writes outside root.
func extractRegistryArchive(archive io.Reader, root, rel string) error {
	prefix := filepath.ToSlash(filepath.Clean(rel))
	reader := tar.NewReader(archive)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if header.Typeflag == tar.TypeXGlobalHeader || header.Typeflag == tar.TypeXHeader {
			continue
		}
		name := filepath.Clean(filepath.FromSlash(header.Name))
		if !filepath.IsLocal(name) {
			return fmt.Errorf("git archive returned unsafe registry path: %s", header.Name)
		}
		if slash := filepath.ToSlash(name); slash != prefix && !strings.HasPrefix(slash, prefix+"/") {
			return fmt.Errorf("git archive returned path outside registry: %s", header.Name)
		}
		target := filepath.Join(root, name)
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(file, reader)
			closeErr := file.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		default:
			return fmt.Errorf("git archive returned unsupported registry entry %s", header.Name)
		}
	}
}

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
	if err := gateSandboxAvailable(); err != nil {
		return fail(e.stderr, &unableError{fmt.Errorf("the gate compiles a proposal only in the sandbox: %w", err)})
	}
	registry, err := e.cfg.GateRegistryDir()
	if err != nil {
		return fail(e.stderr, &unableError{err})
	}
	state, err := e.cfg.GateStatePath()
	if err != nil {
		return fail(e.stderr, &unableError{err})
	}
	tree, err := filepath.EvalSymlinks(e.cfg.Tree)
	if err != nil {
		return fail(e.stderr, &unableError{err})
	}
	e.cfg.Tree, e.cfg.Registry, e.cfg.DB = tree, registry, state
	if err := e.openReadOnly(true); err != nil {
		return fail(e.stderr, err)
	}
	defer e.close()
	commit, err := e.store.Meta(ctx, "commit")
	if err != nil {
		return fail(e.stderr, &unableError{err})
	}
	if head := treeHead(e.cfg.Tree); head != commit {
		return fail(e.stderr, &unableError{fmt.Errorf("the tree HEAD %s differs from the index commit %s", head, commit)})
	}
	rel, err := filepath.Rel(e.cfg.Tree, registry)
	if err != nil {
		return fail(e.stderr, &unableError{err})
	}
	if err := gateRegistryClean(e.cfg.Tree, rel); err != nil {
		return fail(e.stderr, &unableError{err})
	}
	if gateAfterRegistryStatus != nil {
		gateAfterRegistryStatus()
	}
	snapshot, cleanup, err := snapshotGateRegistry(ctx, e.cfg.Tree, commit, rel, filepath.Dir(state))
	if err != nil {
		return fail(e.stderr, &unableError{err})
	}
	defer cleanup()
	if err := gateRegistryClean(e.cfg.Tree, rel); err != nil {
		return fail(e.stderr, &unableError{err})
	}
	e.cfg.Registry = snapshot
	raw, err := os.ReadFile(args[0])
	if err != nil {
		return fail(e.stderr, &unableError{err})
	}
	var p conceptgate.Proposal
	if err := json.Unmarshal(raw, &p); err != nil {
		return fail(e.stderr, &usageError{fmt.Sprintf("%s: %v", args[0], err)})
	}
	e.staleNote(ctx)
	co, err := compiledOptions(ctx, e)
	if err != nil {
		return fail(e.stderr, &unableError{err})
	}
	work := filepath.Join(filepath.Dir(state), "proposals")
	g := &conceptgate.Gate{Index: e.store, Tree: e.cfg.TreeSpec(), Registry: e.cfg.Registry, AdditionRoot: filepath.ToSlash(rel), Compiled: co,
		Librarian: conceptgate.CommandLibrarian{Command: e.cfg.Librarian, WorkDir: work}, WorkDir: work}
	v, err := g.Decide(ctx, p, conceptgate.DecideOptions{Proposer: proposer, AcceptedBy: acceptedBy, ProposalBytes: raw})
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
