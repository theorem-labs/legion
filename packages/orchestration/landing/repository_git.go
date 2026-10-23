package landing

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/sjawhar/legion/orchestration/landing/blame"
	"github.com/sjawhar/legion/orchestration/landing/gitx"
)

// RepositoryGit implements Git by invoking the repository's git binary without
// creating or changing a working copy. Path, when set, is the local repository
// path while callers may keep a distinct repository identity in batch records.
type RepositoryGit struct {
	Path string
}

// Resolve freezes a ref as an immutable commit ID.
func (g RepositoryGit) Resolve(ctx context.Context, repository, rev string) (string, error) {
	resolved, err := repositoryGit(ctx, g.path(repository), "rev-parse", "--verify", rev+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", rev, err)
	}
	return resolved, nil
}

// Candidate builds an in-memory candidate commit.
func (g RepositoryGit) Candidate(ctx context.Context, repository, base string, members []Member) (gitx.Result, error) {
	inputs := make([]gitx.Member, len(members))
	for index, member := range members {
		inputs[index] = gitx.Member{ID: member.Migration, Head: member.Head}
	}
	result, err := gitx.Candidate(ctx, g.path(repository), base, inputs)
	if err != nil {
		return gitx.Result{}, fmt.Errorf("build candidate: %w", err)
	}
	return result, nil
}

// Rebuild recreates a candidate from every frozen member while retaining the
// excluded heads required to reject carriers.
func (g RepositoryGit) Rebuild(ctx context.Context, repository, base string, members []Member, without map[string]struct{}) (gitx.Result, error) {
	inputs := make([]gitx.Member, len(members))
	for index, member := range members {
		inputs[index] = gitx.Member{ID: member.Migration, Head: member.Head}
	}
	result, err := gitx.Rebuild(ctx, g.path(repository), base, inputs, without)
	if err != nil {
		return gitx.Result{}, fmt.Errorf("rebuild candidate: %w", err)
	}
	return result, nil
}

// ChangeSet returns a member's merge-base diff against the candidate base.
func (g RepositoryGit) ChangeSet(ctx context.Context, repository, base string, member Member) (gitx.Change, error) {
	change, err := gitx.ChangeSet(ctx, g.path(repository), base, gitx.Member{ID: member.Migration, Head: member.Head})
	if err != nil {
		return gitx.Change{}, fmt.Errorf("read change set: %w", err)
	}
	return change, nil
}

// DeclarationEvidence indexes a member's changed Lean declarations from its
// merge base to its head. Both sides are scanned from complete source files so
// each change carries its file and fully qualified Lean name.
func (g RepositoryGit) DeclarationEvidence(ctx context.Context, repository, base string, member Member) (blame.Evidence, error) {
	repository = g.path(repository)
	mergeBase, err := repositoryGit(ctx, repository, "merge-base", base, member.Head)
	if err != nil {
		return blame.Evidence{}, fmt.Errorf("find merge base for %q: %w", member.Migration, err)
	}
	paths, err := repositoryGitLines(ctx, repository, "diff", "--name-only", mergeBase, member.Head)
	if err != nil {
		return blame.Evidence{}, fmt.Errorf("list changed files for %q: %w", member.Migration, err)
	}
	changed := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		if strings.HasSuffix(path, ".lean") {
			changed[path] = struct{}{}
		}
	}
	before, err := g.declarationsAt(ctx, repository, mergeBase, changed)
	if err != nil {
		return blame.Evidence{}, fmt.Errorf("read base declarations for %q: %w", member.Migration, err)
	}
	after, err := g.declarationsAt(ctx, repository, member.Head, changed)
	if err != nil {
		return blame.Evidence{}, fmt.Errorf("read member declarations for %q: %w", member.Migration, err)
	}
	return blame.IndexDeclarations(before, after), nil
}

// TreeDeclarations scans every Lean declaration in one immutable tree. Callers
// with several diagnostic names scan once and filter the resulting identities
// in memory.
func (g RepositoryGit) TreeDeclarations(ctx context.Context, repository string, tree Tree) ([]blame.Declaration, error) {
	return g.declarationsAt(ctx, g.path(repository), string(tree), nil)
}

// Declarations finds the declarations a diagnostic can name in one immutable
// tree. It is a convenience for one-name callers; evidence paths use
// TreeDeclarations to avoid rescanning the tree.
func (g RepositoryGit) Declarations(ctx context.Context, repository string, tree Tree, name string) ([]blame.Declaration, error) {
	declarations, err := g.TreeDeclarations(ctx, repository, tree)
	if err != nil {
		return nil, err
	}
	return blame.DeclarationsFor(name, declarations), nil
}

func (g RepositoryGit) declarationsAt(ctx context.Context, repository, revision string, only map[string]struct{}) ([]blame.Declaration, error) {
	paths, err := repositoryGitLines(ctx, repository, "ls-tree", "-r", "--name-only", revision)
	if err != nil {
		return nil, fmt.Errorf("list files at %q: %w", revision, err)
	}
	var declarations []blame.Declaration
	for _, path := range paths {
		if !strings.HasSuffix(path, ".lean") {
			continue
		}
		if only != nil {
			if _, changed := only[path]; !changed {
				continue
			}
		}
		source, err := repositoryGitSource(ctx, repository, revision, path)
		if err != nil {
			return nil, err
		}
		declarations = append(declarations, blame.ScanDeclarations(path, source)...)
	}
	return declarations, nil
}

func (g RepositoryGit) path(repository string) string {
	if g.Path != "" {
		return g.Path
	}
	return repository
}

func repositoryGit(ctx context.Context, repository string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", append([]string{"-C", repository}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}

func repositoryGitLines(ctx context.Context, repository string, args ...string) ([]string, error) {
	output, err := repositoryGit(ctx, repository, args...)
	if err != nil || output == "" {
		return nil, err
	}
	return strings.Split(output, "\n"), nil
}

func repositoryGitSource(ctx context.Context, repository, revision, path string) (string, error) {
	command := exec.CommandContext(ctx, "git", "-C", repository, "show", revision+":"+path)
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("read %q at %q: %w: %s", path, revision, err, strings.TrimSpace(string(output)))
	}
	return string(output), nil
}
