// Package gitx builds candidate commits without changing a worktree.
package gitx

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
)

// Member is one immutable candidate input.
type Member struct {
	ID   string
	Head string
}

// Drop records a member that cannot be part of a candidate.
type Drop struct {
	ID     string
	Reason string
}

// Result is an in-memory candidate commit and its tree.
type Result struct {
	Tree    string
	Commit  string
	Dropped []Drop
}

// Range is one merge-base-to-head member range.
type Range struct {
	Base string
	Head string
}

// Change describes a member's files and commit range relative to a base.
type Change struct {
	Files  []string
	Ranges []Range
}

// Candidate merges members in order onto base. It records conflicting members and
// members carrying a rejected head without modifying the repository worktree.
func Candidate(ctx context.Context, repo, base string, members []Member) (Result, error) {
	return candidate(ctx, repo, base, members, nil)
}

func candidate(ctx context.Context, repo, base string, members []Member, excludedHeads []string) (Result, error) {
	base, err := revision(ctx, repo, base)
	if err != nil {
		return Result{}, fmt.Errorf("resolve base: %w", err)
	}
	tree, err := git(ctx, repo, "rev-parse", base+"^{tree}")
	if err != nil {
		return Result{}, fmt.Errorf("read base tree: %w", err)
	}
	result := Result{Tree: tree, Commit: base}
	seen := make(map[string]struct{}, len(members))
	rejected := append([]string(nil), excludedHeads...)
	for _, member := range members {
		head, err := revision(ctx, repo, member.Head)
		if err != nil {
			return Result{}, fmt.Errorf("resolve member %q: %w", member.ID, err)
		}
		if _, ok := seen[head]; ok {
			result.Dropped = append(result.Dropped, Drop{ID: member.ID, Reason: "duplicate"})
			continue
		}
		seen[head] = struct{}{}
		if carries(ctx, repo, rejected, head) {
			result.Dropped = append(result.Dropped, Drop{ID: member.ID, Reason: "carrier"})
			continue
		}
		contained, err := ancestor(ctx, repo, head, base)
		if err != nil {
			return Result{}, fmt.Errorf("check member %q against base: %w", member.ID, err)
		}
		if contained {
			result.Dropped = append(result.Dropped, Drop{ID: member.ID, Reason: "contained"})
			continue
		}
		mergedTree, err := git(ctx, repo, "merge-tree", "--write-tree", "--no-messages", result.Commit, head)
		if err != nil {
			if exitCode(err) == 1 {
				result.Dropped = append(result.Dropped, Drop{ID: member.ID, Reason: "conflict"})
				rejected = append(rejected, head)
				continue
			}
			return Result{}, fmt.Errorf("merge member %q: %w", member.ID, err)
		}
		commit, err := git(ctx, repo, "commit-tree", mergedTree, "-p", result.Commit, "-p", head, "-m", "candidate merge")
		if err != nil {
			return Result{}, fmt.Errorf("commit member %q: %w", member.ID, err)
		}
		result.Tree = mergedTree
		result.Commit = commit
	}
	return result, nil
}

// Rebuild creates the candidate without the members whose identifiers are in without.
func Rebuild(ctx context.Context, repo, base string, members []Member, without map[string]struct{}) (Result, error) {
	kept := make([]Member, 0, len(members))
	excludedHeads := make([]string, 0, len(without))
	for _, member := range members {
		if _, excluded := without[member.ID]; excluded {
			head, err := revision(ctx, repo, member.Head)
			if err != nil {
				return Result{}, fmt.Errorf("resolve excluded member %q: %w", member.ID, err)
			}
			excludedHeads = append(excludedHeads, head)
			continue
		}
		kept = append(kept, member)
	}
	return candidate(ctx, repo, base, kept, excludedHeads)
}

// ChangeSet returns the files and merge-base range that a member contributes.
func ChangeSet(ctx context.Context, repo, base string, member Member) (Change, error) {
	base, err := revision(ctx, repo, base)
	if err != nil {
		return Change{}, fmt.Errorf("resolve base: %w", err)
	}
	head, err := revision(ctx, repo, member.Head)
	if err != nil {
		return Change{}, fmt.Errorf("resolve member %q: %w", member.ID, err)
	}
	mergeBase, err := git(ctx, repo, "merge-base", base, head)
	if err != nil {
		return Change{}, fmt.Errorf("find merge base for %q: %w", member.ID, err)
	}
	raw, err := git(ctx, repo, "diff", "--name-only", "--no-renames", "-z", mergeBase, head)
	if err != nil {
		return Change{}, fmt.Errorf("list member %q files: %w", member.ID, err)
	}
	var files []string
	for _, path := range strings.Split(raw, "\x00") {
		path = strings.TrimSpace(path)
		if path != "" {
			files = append(files, path)
		}
	}
	sort.Strings(files)
	return Change{Files: files, Ranges: []Range{{Base: mergeBase, Head: head}}}, nil
}

func carries(ctx context.Context, repo string, rejected []string, head string) bool {
	for _, rejectedHead := range rejected {
		contained, err := ancestor(ctx, repo, rejectedHead, head)
		if err == nil && contained {
			return true
		}
	}
	return false
}

func ancestor(ctx context.Context, repo, ancestor, descendant string) (bool, error) {
	command := exec.CommandContext(ctx, "git", "-C", repo, "merge-base", "--is-ancestor", ancestor, descendant)
	if err := command.Run(); err != nil {
		if exitCode(err) == 1 {
			return false, nil
		}
		return false, fmt.Errorf("git merge-base --is-ancestor: %w", err)
	}
	return true, nil
}

func revision(ctx context.Context, repo, rev string) (string, error) {
	resolved, err := git(ctx, repo, "rev-parse", "--verify", rev+"^{commit}")
	if err != nil {
		return "", err
	}
	return resolved, nil
}

func gitLines(ctx context.Context, repo string, args ...string) ([]string, error) {
	output, err := git(ctx, repo, args...)
	if err != nil {
		return nil, err
	}
	if output == "" {
		return nil, nil
	}
	return strings.Split(output, "\n"), nil
}

func git(ctx context.Context, repo string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", append([]string{"-C", repo}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}

func exitCode(err error) int {
	var commandError *exec.ExitError
	if !errors.As(err, &commandError) {
		return -1
	}
	return commandError.ExitCode()
}
