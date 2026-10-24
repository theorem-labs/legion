package landing

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"
	"runtime"
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
// streams blobs through one git cat-file --batch process and scans them in
// parallel.
func (g RepositoryGit) TreeDeclarations(ctx context.Context, repository string, tree Tree) ([]blame.Declaration, error) {
	return g.scanTree(ctx, g.path(repository), string(tree), nil)
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

type blobEntry struct {
	path string
	id   string
}

// scanTree lists Lean blobs with ls-tree -z, streams them through one
// cat-file --batch process, and scans them in parallel. When only is non-nil,
// only the named files are scanned.
func (g RepositoryGit) scanTree(ctx context.Context, repository, revision string, only map[string]struct{}) ([]blame.Declaration, error) {
	listing, err := repositoryGit(ctx, repository, "ls-tree", "-r", "-z", revision)
	if err != nil {
		return nil, fmt.Errorf("list files at %q: %w", revision, err)
	}
	var entries []blobEntry
	for _, raw := range strings.Split(listing, "\x00") {
		meta, path, found := strings.Cut(raw, "\t")
		if !found {
			continue
		}
		fields := strings.Fields(meta)
		if len(fields) != 3 || fields[1] != "blob" || !strings.HasSuffix(path, ".lean") {
			continue
		}
		if only != nil {
			if _, ok := only[path]; !ok {
				continue
			}
		}
		entries = append(entries, blobEntry{path: path, id: fields[2]})
	}
	if len(entries) == 0 {
		return nil, nil
	}
	// Stream blobs through one cat-file --batch process.
	ids := make([]string, len(entries))
	for i, e := range entries {
		ids[i] = e.id
	}
	command := exec.CommandContext(ctx, "git", "-C", repository, "cat-file", "--batch")
	command.Stdin = strings.NewReader(strings.Join(ids, "\n") + "\n")
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("cat-file pipe: %w", err)
	}
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("cat-file start: %w", err)
	}
	reader := bufio.NewReaderSize(stdout, 1<<20)
	type scanJob struct {
		path   string
		source string
	}
	jobs := make([]scanJob, 0, len(entries))
	for _, e := range entries {
		header, err := reader.ReadString('\n')
		if err != nil {
			_ = command.Wait()
			return nil, fmt.Errorf("read blob header for %q: %w", e.path, err)
		}
		headerFields := strings.Fields(header)
		if len(headerFields) < 3 {
			_ = command.Wait()
			return nil, fmt.Errorf("unexpected blob header for %q: %q", e.path, header)
		}
		size, err := strconv.Atoi(headerFields[2])
		if err != nil {
			_ = command.Wait()
			return nil, fmt.Errorf("parse blob size for %q: %w", e.path, err)
		}
		body := make([]byte, size+1) // +1 for trailing newline from cat-file
		if _, err := io.ReadFull(reader, body); err != nil {
			_ = command.Wait()
			return nil, fmt.Errorf("read blob body for %q: %w", e.path, err)
		}
		jobs = append(jobs, scanJob{path: e.path, source: string(body[:size])})
	}
	if err := command.Wait(); err != nil {
		return nil, fmt.Errorf("cat-file: %w", err)
	}
	// Scan in parallel.
	type scanResult struct {
		declarations []blame.Declaration
	}
	results := make([]scanResult, len(jobs))
	work := make(chan int)
	var group sync.WaitGroup
	workers := runtime.GOMAXPROCS(0)
	if workers > len(jobs) {
		workers = len(jobs)
	}
	for range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			for index := range work {
				results[index] = scanResult{
					declarations: blame.ScanDeclarations(jobs[index].path, jobs[index].source),
				}
			}
		}()
	}
	for i := range jobs {
		work <- i
	}
	close(work)
	group.Wait()
	var all []blame.Declaration
	for _, r := range results {
		all = append(all, r.declarations...)
	}
	return all, nil
}

// readBlobs batch-reads a set of blob IDs through one git cat-file --batch
// process and returns their contents keyed by blob ID.
func readBlobs(ctx context.Context, repository string, ids map[string]struct{}) (map[string]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	idList := make([]string, 0, len(ids))
	for id := range ids {
		idList = append(idList, id)
	}
	command := exec.CommandContext(ctx, "git", "-C", repository, "cat-file", "--batch")
	command.Stdin = strings.NewReader(strings.Join(idList, "\n") + "\n")
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("cat-file pipe: %w", err)
	}
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("cat-file start: %w", err)
	}
	reader := bufio.NewReaderSize(stdout, 1<<20)
	blobs := make(map[string]string, len(idList))
	for _, id := range idList {
		header, err := reader.ReadString('\n')
		if err != nil {
			_ = command.Wait()
			return nil, fmt.Errorf("read blob header for %q: %w", id, err)
		}
		headerFields := strings.Fields(header)
		if len(headerFields) < 3 {
			_ = command.Wait()
			return nil, fmt.Errorf("unexpected blob header for %q: %q", id, header)
		}
		size, err := strconv.Atoi(headerFields[2])
		if err != nil {
			_ = command.Wait()
			return nil, fmt.Errorf("parse blob size for %q: %w", id, err)
		}
		body := make([]byte, size+1)
		if _, err := io.ReadFull(reader, body); err != nil {
			_ = command.Wait()
			return nil, fmt.Errorf("read blob body for %q: %w", id, err)
		}
		blobs[id] = string(body[:size])
	}
	if err := command.Wait(); err != nil {
		return nil, fmt.Errorf("cat-file: %w", err)
	}
	return blobs, nil
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
