package messagetest

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"

	ownerpkg "github.com/sjawhar/legion/orchestration/owner"
	"gopkg.in/yaml.v3"
)

func branchRef(owner string) string { return "refs/heads/" + ownerpkg.Branch(owner) }

func git(ctx context.Context, bare string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", append([]string{"--git-dir=" + bare}, args...)...)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		return stdout.String(), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// branchCommits reads the commits of an owner's branch, oldest first, with their trailers. A branch the owner has not
// made yet has none.
func branchCommits(ctx context.Context, bare, owner string) ([]Commit, error) {
	if _, err := git(ctx, bare, "rev-parse", "--verify", "--quiet", branchRef(owner)); err != nil {
		return nil, nil
	}
	out, err := git(ctx, bare, "log", "--reverse", "--format=%H%x1f%(trailers:only,unfold)%x1e", branchRef(owner))
	if err != nil {
		return nil, err
	}
	var commits []Commit
	for _, entry := range strings.Split(out, "\x1e") {
		sha, trailers, ok := strings.Cut(strings.TrimSpace(entry), "\x1f")
		if !ok {
			continue
		}
		commits = append(commits, Commit{Owner: owner, SHA: sha, Trailers: ownerpkg.ParseTrailers(trailers)})
	}
	return commits, nil
}

// readRecord decodes a record at the head of an owner's branch. found is false while the branch or the record does
// not exist.
func readRecord(ctx context.Context, bare, owner, path string, into any) (bool, error) {
	if _, err := git(ctx, bare, "rev-parse", "--verify", "--quiet", branchRef(owner)+":"+path); err != nil {
		return false, nil
	}
	data, err := git(ctx, bare, "show", branchRef(owner)+":"+path)
	if err != nil {
		return false, err
	}
	if err := yaml.Unmarshal([]byte(data), into); err != nil {
		return false, fmt.Errorf("decode %s on the branch of %s: %w", path, owner, err)
	}
	return true, nil
}
