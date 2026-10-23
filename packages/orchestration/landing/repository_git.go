package landing

import (
	"context"
	"errors"
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

// Diff returns a zero-context diff for a member's merge-base range.
func (g RepositoryGit) Diff(ctx context.Context, repository, base string, member Member) (string, error) {
	repository = g.path(repository)
	mergeBase, err := repositoryGit(ctx, repository, "merge-base", base, member.Head)
	if err != nil {
		return "", fmt.Errorf("find merge base for %q: %w", member.Migration, err)
	}
	diff, err := repositoryGit(ctx, repository, "diff", "-U0", "--no-color", mergeBase, member.Head)
	if err != nil {
		return "", fmt.Errorf("read diff for %q: %w", member.Migration, err)
	}
	return diff, nil
}

// DeclarationLines resolves a diagnostic name by its final component, then
// filters exact declaration names with the original suffix-match rule.
func (g RepositoryGit) DeclarationLines(ctx context.Context, repository string, tree Tree, name string) ([]string, error) {
	repository = g.path(repository)
	search := name
	if index := strings.LastIndex(search, "."); index >= 0 {
		search = search[index+1:]
	}
	command := exec.CommandContext(ctx, "git", "-C", repository, "grep", "-h", "-I", "-F", search, string(tree), "--")
	output, err := command.CombinedOutput()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return nil, nil
		}
		return nil, fmt.Errorf("git grep %q: %w: %s", name, err, strings.TrimSpace(string(output)))
	}
	text := strings.TrimSpace(string(output))
	if text == "" {
		return nil, nil
	}
	return blame.DeclarationLinesFor(name, strings.Split(text, "\n")), nil
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
