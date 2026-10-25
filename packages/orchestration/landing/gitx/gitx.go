// Package gitx builds candidate commits without changing a worktree.
package gitx

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
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

// Rebuild creates the candidate without the members whose identifiers are in without.
func Rebuild(ctx context.Context, repo, base string, members []Member, without map[string]struct{}) (Result, error) {
	kept := make([]Member, 0, len(members))
	excluded := make([]Member, 0, len(without))
	for _, member := range members {
		if _, found := without[member.ID]; found {
			excluded = append(excluded, member)
			continue
		}
		kept = append(kept, member)
	}
	return candidate(ctx, repo, base, kept, excluded)
}

// candidate merges with one --stdin merge-tree process where git answers it
// line by line, and with one merge-tree process per member where it does not.
func candidate(ctx context.Context, repo, base string, members, excluded []Member) (Result, error) {
	streams, err := mergeTreeStreams()
	if err != nil {
		return Result{}, err
	}
	start := processMerges
	if streams {
		start = streamMerges
	}
	return chain(ctx, repo, base, members, excluded, start)
}

// chain forms the candidate: in the members' order, it drops a member whose
// head an earlier member already has (duplicate), that carries an excluded or
// conflicting head (carrier) or that base already holds (contained), and
// merges each other member onto the chain so far, dropping it on a conflict.
// Every head resolves in one process, one listing of their history answers
// every ancestor question, and the merges run in the merger start makes.
func chain(ctx context.Context, repo, base string, members, excluded []Member, start startMerger) (Result, error) {
	base, err := revision(ctx, repo, base)
	if err != nil {
		return Result{}, fmt.Errorf("resolve base: %w", err)
	}
	tree, err := git(ctx, repo, "rev-parse", base+"^{tree}")
	if err != nil {
		return Result{}, fmt.Errorf("read base tree: %w", err)
	}
	heads, err := resolveHeads(ctx, repo, append(append([]Member(nil), excluded...), members...))
	if err != nil {
		return Result{}, err
	}
	excludedHeads, memberHeads := heads[:len(excluded)], heads[len(excluded):]
	history, err := readHistory(ctx, repo, append([]string{base}, heads...))
	if err != nil {
		return Result{}, err
	}
	inBase := history.ancestors(base)
	carried := make([]bool, len(history.parents))
	for _, head := range excludedHeads {
		history.markDescendants(head, carried)
	}
	merger, err := start(ctx, repo)
	if err != nil {
		return Result{}, err
	}
	result := Result{Tree: tree, Commit: base}
	seen := make(map[string]struct{}, len(members))
	for index, member := range members {
		head := memberHeads[index]
		if _, ok := seen[head]; ok {
			result.Dropped = append(result.Dropped, Drop{ID: member.ID, Reason: "duplicate"})
			continue
		}
		seen[head] = struct{}{}
		if carried[history.index[head]] {
			result.Dropped = append(result.Dropped, Drop{ID: member.ID, Reason: "carrier"})
			continue
		}
		if inBase[history.index[head]] {
			result.Dropped = append(result.Dropped, Drop{ID: member.ID, Reason: "contained"})
			continue
		}
		mergedTree, clean, err := merger.merge(result.Commit, head)
		if err != nil {
			merger.close()
			return Result{}, fmt.Errorf("merge member %q: %w", member.ID, err)
		}
		if !clean {
			result.Dropped = append(result.Dropped, Drop{ID: member.ID, Reason: "conflict"})
			history.markDescendants(head, carried)
			continue
		}
		commit, err := gitAs(ctx, repo, candidateIdentity, "commit-tree", mergedTree, "-p", result.Commit, "-p", head, "-m", "candidate merge")
		if err != nil {
			merger.close()
			return Result{}, fmt.Errorf("commit member %q: %w", member.ID, err)
		}
		result.Tree = mergedTree
		result.Commit = commit
	}
	if err := merger.close(); err != nil {
		return Result{}, err
	}
	return result, nil
}

// resolveHeads resolves every member's head to its commit in one
// cat-file --batch-check, in the members' order.
func resolveHeads(ctx context.Context, repo string, members []Member) ([]string, error) {
	var input strings.Builder
	for _, member := range members {
		if strings.ContainsAny(member.Head, "\n\x00") {
			return nil, fmt.Errorf("resolve member %q: head %q holds a newline", member.ID, member.Head)
		}
		input.WriteString(member.Head + "^{commit}\n")
	}
	command := exec.CommandContext(ctx, "git", "-C", repo, "cat-file", "--batch-check=%(objectname)")
	command.Stdin = strings.NewReader(input.String())
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("git cat-file --batch-check: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	lines := strings.Split(strings.TrimSuffix(string(output), "\n"), "\n")
	if len(members) == 0 {
		lines = nil
	}
	if len(lines) != len(members) {
		return nil, fmt.Errorf("git cat-file --batch-check answered %d of %d heads", len(lines), len(members))
	}
	heads := make([]string, len(members))
	for index, line := range lines {
		if !isObjectID(line) {
			return nil, fmt.Errorf("resolve member %q: %s", members[index].ID, line)
		}
		heads[index] = line
	}
	return heads, nil
}

func isObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, character := range value {
		if !strings.ContainsRune("0123456789abcdef", character) {
			return false
		}
	}
	return true
}

// history is the commit graph reachable from a set of commits, read in one
// rev-list. It answers ancestor questions exactly, as merge-base --is-ancestor
// does, by walking the graph rather than by commit dates.
type history struct {
	index    map[string]int32
	parents  [][]int32
	children [][]int32
}

func readHistory(ctx context.Context, repo string, tips []string) (history, error) {
	command := exec.CommandContext(ctx, "git", "-C", repo, "rev-list", "--parents", "--stdin")
	command.Stdin = strings.NewReader(strings.Join(tips, "\n") + "\n")
	var stderr bytes.Buffer
	command.Stderr = &stderr
	stdout, err := command.StdoutPipe()
	if err != nil {
		return history{}, err
	}
	if err := command.Start(); err != nil {
		return history{}, fmt.Errorf("git rev-list: %w", err)
	}
	h := history{index: make(map[string]int32)}
	id := func(commit string) int32 {
		if index, found := h.index[commit]; found {
			return index
		}
		index := int32(len(h.parents))
		h.index[commit] = index
		h.parents = append(h.parents, nil)
		h.children = append(h.children, nil)
		return index
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 {
			continue
		}
		commit := id(fields[0])
		for _, parent := range fields[1:] {
			parentIndex := id(parent)
			h.parents[commit] = append(h.parents[commit], parentIndex)
			h.children[parentIndex] = append(h.children[parentIndex], commit)
		}
	}
	if err := scanner.Err(); err != nil {
		command.Wait()
		return history{}, fmt.Errorf("read git rev-list: %w", err)
	}
	if err := command.Wait(); err != nil {
		return history{}, fmt.Errorf("git rev-list --parents: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	for _, tip := range tips {
		if _, found := h.index[tip]; !found {
			return history{}, fmt.Errorf("git rev-list did not list %s", tip)
		}
	}
	return h, nil
}

// ancestors marks commit and every commit it descends from.
func (h history) ancestors(commit string) []bool {
	marked := make([]bool, len(h.parents))
	stack := []int32{h.index[commit]}
	for len(stack) > 0 {
		next := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if marked[next] {
			continue
		}
		marked[next] = true
		stack = append(stack, h.parents[next]...)
	}
	return marked
}

// markDescendants marks commit and every commit that descends from it.
func (h history) markDescendants(commit string, marked []bool) {
	stack := []int32{h.index[commit]}
	for len(stack) > 0 {
		next := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if marked[next] {
			continue
		}
		marked[next] = true
		stack = append(stack, h.children[next]...)
	}
}

// merger merges two commits into a tree without touching a worktree.
type merger interface {
	// merge returns the merged tree and whether the merge was clean.
	merge(ours, theirs string) (tree string, clean bool, err error)
	close() error
}

type startMerger func(ctx context.Context, repo string) (merger, error)

// processMerges runs one git merge-tree per merge.
func processMerges(ctx context.Context, repo string) (merger, error) {
	return processMerger{ctx: ctx, repo: repo}, nil
}

type processMerger struct {
	ctx  context.Context
	repo string
}

func (m processMerger) merge(ours, theirs string) (string, bool, error) {
	tree, err := git(m.ctx, m.repo, "merge-tree", "--write-tree", "--no-messages", ours, theirs)
	if err != nil {
		if exitCode(err) == 1 {
			return "", false, nil
		}
		return "", false, err
	}
	return tree, true, nil
}

func (processMerger) close() error { return nil }

// streamMerges runs every merge in one git merge-tree --stdin, one input line
// per merge, reading each answer before it writes the next line: each merge
// is onto the commit the previous one made.
func streamMerges(ctx context.Context, repo string) (merger, error) {
	command := exec.CommandContext(ctx, "git", "-C", repo, "merge-tree", "--stdin", "-z", "--name-only", "--no-messages")
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, err
	}
	m := &streamMerger{command: command, stdin: stdin, stdout: bufio.NewReader(stdout)}
	command.Stderr = &m.stderr
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("git merge-tree --stdin: %w", err)
	}
	return m, nil
}

type streamMerger struct {
	command *exec.Cmd
	stdin   io.WriteCloser
	stdout  *bufio.Reader
	stderr  bytes.Buffer
	closed  bool
}

// merge writes one merge and reads its answer: the status, the tree, the
// conflicted paths and an empty field that ends the answer, each ended by NUL.
func (m *streamMerger) merge(ours, theirs string) (string, bool, error) {
	if _, err := fmt.Fprintf(m.stdin, "%s %s\n", ours, theirs); err != nil {
		return "", false, m.failure(err)
	}
	status, err := m.field()
	if err != nil {
		return "", false, err
	}
	tree, err := m.field()
	if err != nil {
		return "", false, err
	}
	for {
		path, err := m.field()
		if err != nil {
			return "", false, err
		}
		if path == "" {
			break
		}
	}
	switch status {
	case "1":
		return tree, true, nil
	case "0":
		return "", false, nil
	default:
		return "", false, fmt.Errorf("git merge-tree --stdin: merge status %s: %s", status, strings.TrimSpace(m.stderr.String()))
	}
}

func (m *streamMerger) field() (string, error) {
	value, err := m.stdout.ReadString(0)
	if err != nil {
		return "", m.failure(err)
	}
	return strings.TrimSuffix(value, "\x00"), nil
}

// failure ends the process and names why it stopped answering.
func (m *streamMerger) failure(err error) error {
	m.close()
	return fmt.Errorf("git merge-tree --stdin: %w: %s", err, strings.TrimSpace(m.stderr.String()))
}

func (m *streamMerger) close() error {
	if m.closed {
		return nil
	}
	m.closed = true
	m.stdin.Close()
	if err := m.command.Wait(); err != nil {
		return fmt.Errorf("git merge-tree --stdin: %w: %s", err, strings.TrimSpace(m.stderr.String()))
	}
	return nil
}

// mergeTreeStreams reports whether this git answers merge-tree --stdin line
// by line. Git 2.49 made it flush each answer (6a9ae81015, the --stdin
// deadlock fix); an older one holds its answers until end of input.
var mergeTreeStreams = sync.OnceValues(func() (bool, error) {
	output, err := exec.Command("git", "version").Output()
	if err != nil {
		return false, fmt.Errorf("git version: %w", err)
	}
	major, minor, err := parseGitVersion(string(output))
	if err != nil {
		return false, err
	}
	return major > 2 || major == 2 && minor >= 49, nil
})

func parseGitVersion(output string) (int, int, error) {
	fields := strings.Fields(output)
	if len(fields) < 3 || fields[0] != "git" || fields[1] != "version" {
		return 0, 0, fmt.Errorf("read git version from %q", strings.TrimSpace(output))
	}
	parts := strings.SplitN(fields[2], ".", 3)
	if len(parts) < 2 {
		return 0, 0, fmt.Errorf("read git version from %q", strings.TrimSpace(output))
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, fmt.Errorf("read git version from %q: %w", strings.TrimSpace(output), err)
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, fmt.Errorf("read git version from %q: %w", strings.TrimSpace(output), err)
	}
	return major, minor, nil
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
	return gitAs(ctx, repo, nil, args...)
}

// candidateIdentity is the author and committer of a candidate merge: the
// landing program's own object, so it never depends on the git identity of
// the host that builds it, which a build machine often lacks.
var candidateIdentity = []string{
	"GIT_AUTHOR_NAME=land", "GIT_AUTHOR_EMAIL=land@localhost",
	"GIT_COMMITTER_NAME=land", "GIT_COMMITTER_EMAIL=land@localhost",
}

// gitAs runs git with environment added to the process's own.
func gitAs(ctx context.Context, repo string, environment []string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", append([]string{"-C", repo}, args...)...)
	if environment != nil {
		command.Env = append(os.Environ(), environment...)
	}
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
