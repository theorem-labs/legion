package landing

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"maps"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"

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

// ReadCommit reads a commit's tree and its parents, in their order, from the commit object itself.
func (g RepositoryGit) ReadCommit(ctx context.Context, repository, commit string) (Commit, error) {
	body, err := repositoryGit(ctx, g.path(repository), "cat-file", "commit", commit)
	if err != nil {
		return Commit{}, fmt.Errorf("read commit %q: %w", commit, err)
	}
	var read Commit
	for _, line := range strings.Split(body, "\n") {
		name, value, found := strings.Cut(line, " ")
		if !found {
			break
		}
		switch name {
		case "tree":
			read.Tree = value
		case "parent":
			read.Parents = append(read.Parents, value)
		}
	}
	if read.Tree == "" {
		return Commit{}, fmt.Errorf("read commit %q: it names no tree", commit)
	}
	return read, nil
}

// MergeTree merges two commits without a working copy and is the tree the merge writes. A merge that conflicts is
// an error, as is a merge git cannot make.
func (g RepositoryGit) MergeTree(ctx context.Context, repository, left, right string) (string, error) {
	tree, err := repositoryGit(ctx, g.path(repository), "merge-tree", "--write-tree", "--no-messages", left, right)
	if err != nil {
		return "", fmt.Errorf("merge %q with %q: %w", left, right, err)
	}
	return tree, nil
}

// Ancestor reports whether commit is an ancestor of descendant.
func (g RepositoryGit) Ancestor(ctx context.Context, repository, commit, descendant string) (bool, error) {
	command := exec.CommandContext(ctx, "git", "-C", g.path(repository), "merge-base", "--is-ancestor", commit, descendant)
	output, err := command.CombinedOutput()
	if err == nil {
		return true, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("check whether %q is behind %q: %w: %s", commit, descendant, err, strings.TrimSpace(string(output)))
}

// DeclarationEvidence indexes a member's changed Lean declarations from its
// merge base to its head. Both sides are scanned from complete source files so
// each change carries its file and fully qualified Lean name. The unified diff
// fills Inside (body edits), FieldsAdded, FieldsRemoved and Namespaces.
//
// Every blob is read through a single git cat-file --batch process keyed by
// blob ID, not one git show per file.
func (g RepositoryGit) DeclarationEvidence(ctx context.Context, repository, base string, member Member) (blame.Evidence, error) {
	repository = g.path(repository)
	mergeBase, err := repositoryGit(ctx, repository, "merge-base", base, member.Head)
	if err != nil {
		return blame.Evidence{}, fmt.Errorf("find merge base for %q: %w", member.Migration, err)
	}
	// git diff --raw gives us changed files with their old and new blob IDs.
	rawDiff, err := repositoryGit(ctx, repository, "diff", "--raw", "--no-renames", "-z", "--no-abbrev", "--full-index", mergeBase, member.Head, "--", "*.lean")
	if err != nil {
		return blame.Evidence{}, fmt.Errorf("list changed files for %q: %w", member.Migration, err)
	}
	type fileBlobs struct {
		path    string
		oldBlob string // "0000…" for additions
		newBlob string // "0000…" for deletions
	}
	var files []fileBlobs
	blobIDs := make(map[string]struct{})
	// Raw format with -z: fields are separated by NUL. Each entry is
	// ":oldmode newmode oldblob newblob status\0path\0".
	parts := strings.Split(rawDiff, "\x00")
	for i := 0; i+1 < len(parts); i += 2 {
		meta := parts[i]
		path := parts[i+1]
		if path == "" || !strings.HasSuffix(path, ".lean") {
			continue
		}
		// meta starts with optional ":" from previous entry's close or the
		// first entry. Find the colon-prefixed fields.
		idx := strings.LastIndex(meta, ":")
		if idx < 0 {
			continue
		}
		fields := strings.Fields(meta[idx+1:])
		if len(fields) < 5 {
			continue
		}
		oldBlob, newBlob := fields[2], fields[3]
		files = append(files, fileBlobs{path: path, oldBlob: oldBlob, newBlob: newBlob})
		const zeroes = "0000000000000000000000000000000000000000"
		if oldBlob != zeroes {
			blobIDs[oldBlob] = struct{}{}
		}
		if newBlob != zeroes {
			blobIDs[newBlob] = struct{}{}
		}
	}
	if len(files) == 0 {
		return blame.NewEvidence(), nil
	}
	// Batch-read every unique blob through one cat-file --batch.
	blobs, err := readBlobs(ctx, repository, blobIDs)
	if err != nil {
		return blame.Evidence{}, fmt.Errorf("read blobs for %q: %w", member.Migration, err)
	}
	// Scan declarations and line owners from both sides.
	var before, after []blame.Declaration
	beforeOwners := make(map[string][]blame.LineOwner, len(files))
	afterOwners := make(map[string][]blame.LineOwner, len(files))
	for _, f := range files {
		if src, ok := blobs[f.oldBlob]; ok {
			before = append(before, blame.ScanDeclarations(f.path, src)...)
			beforeOwners[f.path] = blame.ScanLineOwners(f.path, src)
		}
		if src, ok := blobs[f.newBlob]; ok {
			after = append(after, blame.ScanDeclarations(f.path, src)...)
			afterOwners[f.path] = blame.ScanLineOwners(f.path, src)
		}
	}
	evidence := blame.IndexDeclarations(before, after)
	// Enrich with Inside, Fields and Namespaces from the unified diff.
	diffOutput, err := repositoryGit(ctx, repository, "diff", "-U0", "--no-renames", "--no-color", "--no-ext-diff", "--no-textconv", mergeBase, member.Head, "--", "*.lean")
	if err != nil {
		return blame.Evidence{}, fmt.Errorf("read diff for %q: %w", member.Migration, err)
	}
	if diffOutput != "" {
		blame.EnrichFromDiff(&evidence, diffOutput, beforeOwners, afterOwners)
	}
	return evidence, nil
}

// TreeDeclarations scans every Lean declaration in one immutable tree. It
// streams the tree's Lean blobs through one git cat-file --batch process and
// hands each blob to a pool of scanning workers as it is read, so only the
// blobs in flight are held in memory.
func (g RepositoryGit) TreeDeclarations(ctx context.Context, repository string, tree Tree) ([]blame.Declaration, error) {
	repository = g.path(repository)
	listing, err := repositoryGit(ctx, repository, "ls-tree", "-r", "-z", string(tree))
	if err != nil {
		return nil, fmt.Errorf("list files at %q: %w", tree, err)
	}
	var paths, ids []string
	for entry := range strings.SplitSeq(listing, "\x00") {
		meta, path, found := strings.Cut(entry, "\t")
		if !found {
			continue
		}
		fields := strings.Fields(meta)
		if len(fields) != 3 || fields[1] != "blob" || !strings.HasSuffix(path, ".lean") {
			continue
		}
		paths = append(paths, path)
		ids = append(ids, fields[2])
	}
	type scanJob struct {
		index  int
		source string
	}
	workers := min(runtime.GOMAXPROCS(0), len(paths))
	jobs := make(chan scanJob, workers)
	results := make([][]blame.Declaration, len(paths))
	var group sync.WaitGroup
	for range workers {
		group.Go(func() {
			for job := range jobs {
				results[job.index] = blame.ScanDeclarations(paths[job.index], job.source)
			}
		})
	}
	err = catFileBatch(ctx, repository, ids, func(index int, source string) {
		jobs <- scanJob{index: index, source: source}
	})
	close(jobs)
	group.Wait()
	if err != nil {
		return nil, fmt.Errorf("read files at %q: %w", tree, err)
	}
	total := 0
	for _, declarations := range results {
		total += len(declarations)
	}
	all := make([]blame.Declaration, 0, total)
	for _, declarations := range results {
		all = append(all, declarations...)
	}
	return all, nil
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

// readBlobs batch-reads a set of blob IDs through one git cat-file --batch
// process and returns their contents keyed by blob ID.
func readBlobs(ctx context.Context, repository string, ids map[string]struct{}) (map[string]string, error) {
	list := slices.Collect(maps.Keys(ids))
	blobs := make(map[string]string, len(list))
	if err := catFileBatch(ctx, repository, list, func(index int, source string) {
		blobs[list[index]] = source
	}); err != nil {
		return nil, err
	}
	return blobs, nil
}

// catFileBatch reads each blob in ids, in order, through one git cat-file
// --batch process, and passes each blob's index in ids and its contents to
// each as soon as it is read.
func catFileBatch(ctx context.Context, repository string, ids []string, each func(index int, source string)) error {
	if len(ids) == 0 {
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	command := exec.CommandContext(ctx, "git", "-C", repository, "cat-file", "--batch")
	command.Stdin = strings.NewReader(strings.Join(ids, "\n") + "\n")
	stdout, err := command.StdoutPipe()
	if err != nil {
		return fmt.Errorf("cat-file pipe: %w", err)
	}
	if err := command.Start(); err != nil {
		return fmt.Errorf("cat-file start: %w", err)
	}
	reader := bufio.NewReaderSize(stdout, 1<<20)
	for index, id := range ids {
		source, err := readBatchBlob(reader)
		if err != nil {
			// Stop git before waiting, since it may be blocked writing.
			cancel()
			_ = command.Wait()
			return fmt.Errorf("read blob %s: %w", id, err)
		}
		each(index, source)
	}
	if err := command.Wait(); err != nil {
		return fmt.Errorf("cat-file: %w", err)
	}
	return nil
}

// readBatchBlob reads one blob from git cat-file --batch output: a header
// `<id> blob <size>`, the contents and a newline.
func readBatchBlob(reader *bufio.Reader) (string, error) {
	header, err := reader.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("read header: %w", err)
	}
	fields := strings.Fields(header)
	if len(fields) != 3 || fields[1] != "blob" {
		return "", fmt.Errorf("unexpected header %q", header)
	}
	size, err := strconv.Atoi(fields[2])
	if err != nil {
		return "", fmt.Errorf("parse size in header %q: %w", header, err)
	}
	// Copy the contents straight from the reader's buffer, so each blob is
	// held once.
	var body strings.Builder
	body.Grow(size)
	for body.Len() < size {
		chunk, err := reader.Peek(min(size-body.Len(), reader.Size()))
		body.Write(chunk)
		if _, discardErr := reader.Discard(len(chunk)); discardErr != nil {
			return "", fmt.Errorf("read contents: %w", discardErr)
		}
		if err != nil {
			return "", fmt.Errorf("read contents: %w", err)
		}
	}
	if _, err := reader.Discard(1); err != nil {
		return "", fmt.Errorf("read contents: %w", err)
	}
	return body.String(), nil
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
