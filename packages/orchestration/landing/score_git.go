package landing

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// RepositoryScore is the score ref on a remote, read and written with git plumbing in the program's own clone and
// its own credential. The score hook never commits or pushes: this is the apply step.
type RepositoryScore struct {
	// Path is the clone the plumbing runs in.
	Path string
}

// scoreRef is where a fetch of the target ref lands in the clone, so one fetch serves the read that follows it.
const scoreRef = "refs/land/score-target"

// Latest is the newest score of part on remote's ref: the file that the newest commit touching that part holds. A
// ref or a part that is not there yet gives no score.
func (s RepositoryScore) Latest(ctx context.Context, remote, ref, part string) ([]byte, error) {
	fetched, err := s.fetch(ctx, remote, ref)
	if err != nil || !fetched {
		return nil, err
	}
	commit, err := s.git(ctx, "log", "-1", "--format=%H", scoreRef, "--", part+"/")
	if err != nil || commit == "" {
		return nil, err
	}
	listed, err := s.git(ctx, "show", "--name-only", "--format=", commit, "--", part+"/")
	if err != nil {
		return nil, err
	}
	path := ""
	for _, line := range strings.Split(listed, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			path = line
			break
		}
	}
	if path == "" {
		return nil, nil
	}
	body, err := s.output(ctx, nil, "show", commit+":"+path)
	if err != nil {
		return nil, err
	}
	return body, nil
}

// Present is the blob that path holds on remote's ref, and empty when the path is not there: the score of that
// landing was pushed by a run whose done a crash lost, so the apply pushes nothing more.
func (s RepositoryScore) Present(ctx context.Context, remote, ref, path string) (string, error) {
	fetched, err := s.fetch(ctx, remote, ref)
	if err != nil || !fetched {
		return "", err
	}
	blob, err := s.git(ctx, "rev-parse", "--verify", "--quiet", scoreRef+":"+path)
	if err != nil {
		// rev-parse --quiet exits 1 for a path the tree does not hold, which is not a failure of the apply.
		return "", nil
	}
	return blob, nil
}

// Add commits body at path on remote's ref as a fast-forward and returns the commit it pushed. The commit's parent
// is the fetched tip, so a push that another writer raced is rejected and the apply fetches and tries again.
func (s RepositoryScore) Add(ctx context.Context, remote, ref, path string, body []byte) (string, error) {
	fetched, err := s.fetch(ctx, remote, ref)
	if err != nil {
		return "", err
	}
	blob, err := s.write(ctx, body)
	if err != nil {
		return "", err
	}
	index, err := os.CreateTemp("", "land-score-index")
	if err != nil {
		return "", err
	}
	indexPath := index.Name()
	_ = index.Close()
	_ = os.Remove(indexPath)
	defer func() { _ = os.Remove(indexPath) }()
	environment := []string{"GIT_INDEX_FILE=" + indexPath}
	if fetched {
		if _, err := s.run(ctx, environment, "read-tree", scoreRef); err != nil {
			return "", err
		}
	}
	if _, err := s.run(ctx, environment, "update-index", "--add", "--cacheinfo", "100644,"+blob+","+path); err != nil {
		return "", err
	}
	tree, err := s.run(ctx, environment, "write-tree")
	if err != nil {
		return "", err
	}
	arguments := []string{"commit-tree", tree, "-m", "score: " + path}
	if fetched {
		arguments = append(arguments, "-p", scoreRef)
	}
	commit, err := s.git(ctx, arguments...)
	if err != nil {
		return "", err
	}
	if _, err := s.git(ctx, "push", remote, commit+":"+ref); err != nil {
		return "", err
	}
	return commit, nil
}

// fetch puts remote's ref at scoreRef in the clone and reports whether the remote has it. A remote without the ref
// is not a failure: the first score of a repository creates it.
func (s RepositoryScore) fetch(ctx context.Context, remote, ref string) (bool, error) {
	_, err := s.git(ctx, "fetch", "--no-tags", "--force", remote, ref+":"+scoreRef)
	if err == nil {
		return true, nil
	}
	if strings.Contains(err.Error(), "couldn't find remote ref") || strings.Contains(err.Error(), "not our ref") {
		return false, nil
	}
	return false, err
}

// write stores body as a blob in the clone and returns its id.
func (s RepositoryScore) write(ctx context.Context, body []byte) (string, error) {
	process := exec.CommandContext(ctx, "git", "-C", s.Path, "hash-object", "-w", "--stdin")
	process.Stdin = bytes.NewReader(body)
	var out, errors bytes.Buffer
	process.Stdout, process.Stderr = &out, &errors
	if err := process.Run(); err != nil {
		return "", fmt.Errorf("git hash-object: %w: %s", err, strings.TrimSpace(errors.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

func (s RepositoryScore) git(ctx context.Context, arguments ...string) (string, error) {
	return s.run(ctx, nil, arguments...)
}

func (s RepositoryScore) run(ctx context.Context, environment []string, arguments ...string) (string, error) {
	output, err := s.output(ctx, environment, arguments...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

// output runs one git command in the clone with the given variables added to the program's environment.
func (s RepositoryScore) output(ctx context.Context, environment []string, arguments ...string) ([]byte, error) {
	process := exec.CommandContext(ctx, "git", append([]string{"-C", s.Path}, arguments...)...)
	if len(environment) != 0 {
		process.Env = append(os.Environ(), environment...)
	}
	var out, errors bytes.Buffer
	process.Stdout, process.Stderr = &out, &errors
	if err := process.Run(); err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(arguments, " "), err, strings.TrimSpace(errors.String()))
	}
	return out.Bytes(), nil
}

// scorePath is the path one landing's score of a part takes on the target ref.
func scorePath(part, commit string) string {
	return filepath.Join(part, commit+".json")
}
