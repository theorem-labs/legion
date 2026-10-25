// Package graphchecktest builds the graphcheck fixture for tests: a git repository holding the Lean project of
// graphcheck/testdata/lean as its base commit, that project's build, the declaration index of the build, and a
// graphcheck configuration naming both. Only tests import it.
//
// The checks judge Lean by its compiled output, so the fixture is a real build: the base's .olean files are what
// a candidate module compiles against, and the index's compiled part is what the base's statements and types
// come from. The build runs once for each toolchain and set of fixture sources, in a cache directory, and every
// fixture copies it.
package graphchecktest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/sjawhar/legion/orchestration/declindex"
	"github.com/sjawhar/legion/orchestration/graphcheck"
)

// Fixture is one fixture repository and its index.
type Fixture struct {
	// Repo is the git repository; its work tree is the base, and the index's tree.
	Repo string
	// Base is the base commit.
	Base string
	// ConfigPath is the graphcheck configuration file; Cfg is what LoadConfig read from it.
	ConfigPath string
	Cfg        graphcheck.Config
	heads      map[string]string
}

// The default vocabulary of the fixture's configuration: the slice 3 rules read it.
const configBody = `index: declindex.yaml
repository: repo
roots: [Lib]
build_roots: [Lib]
root_new_modules: true
statements: {}
links:
  link_files: ['Lib/**/Link*.lean']
  contract_name: '_*[A-Z][A-Z0-9_]{3,}'
  exclude_files: ['Lib/Skip/**']
  open_marker: 'open ask:'
  open_marker_min_reason: 20
producers:
  interface_names: ['Sized', 'Port']
  binder_files: ['Lib/Drv/**/*.lean']
`

// New builds the fixture.
func New(t testing.TB) *Fixture {
	t.Helper()
	return build(t, nil, true)
}

// NewWithoutCompiledPart builds the fixture with an index read from the reference files alone (lean.export off),
// as a corpus index is: it holds no compiled constant, no type and no compiled statement hash.
func NewWithoutCompiledPart(t testing.TB) *Fixture {
	t.Helper()
	return build(t, nil, false)
}

// NewWithUnreadableModule builds the fixture with Lib/Make.ilean truncated before the refresh, so the index
// records an unreadable module and is incomplete.
func NewWithUnreadableModule(t testing.TB) *Fixture {
	t.Helper()
	return build(t, func(lib string) {
		path := filepath.Join(lib, "Lib", "Make.ilean")
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, raw[:len(raw)/2], 0o644); err != nil {
			t.Fatal(err)
		}
	}, true)
}

// RequireLean gives the Lean toolchain the fixture builds with: the `lean` on PATH. The rule tests compile the
// candidate, so without a toolchain they skip; with GRAPHCHECK_REQUIRE_LEAN set, as the orchestration-lean
// workflow sets it, a missing toolchain fails them instead.
func RequireLean(t testing.TB) declindex.Lean {
	t.Helper()
	if _, err := exec.LookPath("lean"); err != nil {
		if os.Getenv("GRAPHCHECK_REQUIRE_LEAN") != "" {
			t.Fatal("GRAPHCHECK_REQUIRE_LEAN is set and no lean is on PATH")
		}
		t.Skip("no lean on PATH: the graphcheck rule tests build the fixture and compile the candidate")
	}
	lean, err := declindex.FindLean(context.Background(), Testdata(t), "")
	if err != nil {
		t.Fatal(err)
	}
	return lean
}

func build(t testing.TB, damage func(lib string), compiled bool) *Fixture {
	t.Helper()
	lean := RequireLean(t)
	built := baseLib(t, lean)
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	if err := copyTree(Testdata(t), repo, skipBuildFiles); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "init", "-q")
	git(t, repo, "config", "user.email", "fixture@example.invalid")
	git(t, repo, "config", "user.name", "Fixture")
	appendFile(t, filepath.Join(repo, ".git", "info", "exclude"), ".lake/\n")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-qm", "base")
	base := git(t, repo, "rev-parse", "HEAD")

	lib := filepath.Join(repo, ".lake", "build", "lib", "lean")
	if err := copyTree(built, lib, nil); err != nil {
		t.Fatal(err)
	}
	if damage != nil {
		damage(lib)
	}
	db := filepath.Join(dir, "index", "index.sqlite")
	store, err := declindex.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	options := declindex.RefreshOptions{Commit: base, Jobs: 2}
	if compiled {
		options.Compiled = compiledPart(t, lean, lib, dir)
	}
	if _, err := store.Refresh(context.Background(), declindex.Tree{Source: repo, Lib: lib, Roots: declindex.Roots{"Lib": true}},
		options); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "declindex.yaml"), "tree: repo\ndb: index/index.sqlite\nroots: [Lib]\n")
	configPath := filepath.Join(dir, "graphcheck.yaml")
	write(t, configPath, configBody)
	cfg, err := graphcheck.LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	return &Fixture{Repo: repo, Base: base, ConfigPath: configPath, Cfg: cfg, heads: map[string]string{}}
}

// compiledPart reads the base build's types and compiled statement hashes into the index, as a production
// refresh does: S2, L1 and P1 compare the candidate's compiled constants with them.
func compiledPart(t testing.TB, lean declindex.Lean, lib, scratch string) func(context.Context) (*declindex.CompiledOptions, error) {
	return func(ctx context.Context) (*declindex.CompiledOptions, error) {
		bin, err := declindex.BuildExporter(ctx, lean, "", filepath.Join(cacheDir(t), "exporter"))
		if err != nil {
			return nil, err
		}
		return &declindex.CompiledOptions{
			Exporter: &declindex.Exporter{Lean: lean, Bin: bin, SearchPath: []string{lib}, Jobs: 2, ScratchDir: scratch},
		}, nil
	}
}

// skipBuildFiles drops the entries of testdata/lean that are not the Lean project: the committed reference files
// of the golden test, and the script that writes them.
func skipBuildFiles(rel string) bool { return rel == "golden" || rel == "build.sh" }

// baseLib builds the fixture project once for each toolchain and set of fixture sources, and gives the built
// library directory (.lake/build/lib/lean) that every fixture copies. The build lands in its cache directory by
// one rename, so parallel test binaries never read a half-written one.
func baseLib(t testing.TB, lean declindex.Lean) string {
	t.Helper()
	src := Testdata(t)
	dir := filepath.Join(cacheDir(t), "base", fixtureDigest(t, src, lean.Prefix))
	lib := filepath.Join(dir, "lean")
	if _, err := os.Stat(lib); err == nil {
		return lib
	}
	work, err := os.MkdirTemp(filepath.Dir(dir), "build-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(work)
	project := filepath.Join(work, "project")
	if err := copyTree(src, project, skipBuildFiles); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join(lean.Prefix, "bin", "lake"), "build")
	cmd.Dir = project
	cmd.Env = []string{
		"PATH=" + filepath.Join(lean.Prefix, "bin") + string(os.PathListSeparator) + "/usr/bin:/bin",
		"LEAN_SYSROOT=" + lean.Prefix, "HOME=" + work, "TMPDIR=" + work, "ELAN_HOME=" + filepath.Join(work, "elan"),
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("lake build in %s: %v\n%s", project, err, out)
	}
	if err := os.Rename(filepath.Join(project, ".lake", "build", "lib", "lean"), filepath.Join(work, "lean")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(work, dir); err != nil && !os.IsExist(err) {
		if _, statErr := os.Stat(lib); statErr != nil {
			t.Fatalf("install the fixture build: %v", err)
		}
	}
	return lib
}

// cacheDir holds what the fixture builds once: the base build and the exporter.
func cacheDir(t testing.TB) string {
	t.Helper()
	cache, err := os.UserCacheDir()
	if err != nil {
		cache = os.TempDir()
	}
	dir := filepath.Join(cache, "graphcheck", "fixture")
	if err := os.MkdirAll(filepath.Join(dir, "base"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// fixtureDigest names one build: the toolchain and every source file of the project.
func fixtureDigest(t testing.TB, src, prefix string) string {
	t.Helper()
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00", prefix)
	var files []string
	err := filepath.WalkDir(src, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if top, _, _ := strings.Cut(rel, string(filepath.Separator)); skipBuildFiles(top) {
			return nil
		}
		files = append(files, rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	for _, rel := range files {
		raw, err := os.ReadFile(filepath.Join(src, rel))
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(h, "%s\x00%s\x00", rel, raw)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// Testdata is the fixture project's directory, graphcheck/testdata/lean.
func Testdata(t testing.TB) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("no caller information")
	}
	return filepath.Join(filepath.Dir(file), "..", "testdata", "lean")
}

// Edit commits one replacement on top of the base and returns the commit's tree. An empty old creates the file
// with new; an empty new with a non-empty old deletes that text; an empty old and new deletes the file.
func (f *Fixture) Edit(t testing.TB, path, old, new string) string {
	t.Helper()
	return f.EditMany(t, map[string][2]string{path: {old, new}})
}

// EditMany commits several replacements, one per file, on top of the base and returns the commit's tree.
func (f *Fixture) EditMany(t testing.TB, edits map[string][2]string) string {
	t.Helper()
	commit := f.commit(t, edits)
	tree := git(t, f.Repo, "rev-parse", commit+"^{tree}")
	f.heads[tree] = commit
	return tree
}

// HeadOf returns the commit that Edit or EditMany made for tree.
func (f *Fixture) HeadOf(t testing.TB, tree string) string {
	t.Helper()
	commit, ok := f.heads[tree]
	if !ok {
		t.Fatalf("no commit of tree %s", tree)
	}
	return commit
}

// BaseTree is the base commit's tree.
func (f *Fixture) BaseTree(t testing.TB) string {
	t.Helper()
	return git(t, f.Repo, "rev-parse", f.Base+"^{tree}")
}

// SameTreeCommit makes an empty commit on top of the base: another commit of the base's tree.
func (f *Fixture) SameTreeCommit(t testing.TB) string {
	t.Helper()
	return git(t, f.Repo, "commit-tree", "-p", f.Base, "-m", "same tree", f.BaseTree(t))
}

// Branch commits the edits on top of the base as branch name and returns the branch's head commit.
func (f *Fixture) Branch(t testing.TB, name string, edits map[string][2]string) string {
	t.Helper()
	commit := f.commit(t, edits)
	git(t, f.Repo, "update-ref", "refs/heads/"+name, commit)
	return commit
}

// commit writes the edited files into a scratch index read from the base, and commits its tree on top of the
// base, leaving the work tree and the repository's own index at the base.
func (f *Fixture) commit(t testing.TB, edits map[string][2]string) string {
	t.Helper()
	index := filepath.Join(t.TempDir(), "index")
	env := []string{"GIT_INDEX_FILE=" + index}
	gitEnv(t, f.Repo, env, "read-tree", f.Base)
	paths := make([]string, 0, len(edits))
	for path := range edits {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		old, new := edits[path][0], edits[path][1]
		if old == "" && new == "" {
			gitEnv(t, f.Repo, env, "rm", "-q", "--cached", "--", path)
			continue
		}
		var text string
		if old == "" {
			text = new
		} else {
			current := blobText(t, f.Repo, env, path)
			if !strings.Contains(current, old) {
				t.Fatalf("%s does not hold %q", path, old)
			}
			text = strings.Replace(current, old, new, 1)
		}
		blob := hashObject(t, f.Repo, text)
		gitEnv(t, f.Repo, env, "update-index", "--add", "--cacheinfo", "100644,"+blob+","+path)
	}
	tree := gitEnv(t, f.Repo, env, "write-tree")
	return git(t, f.Repo, "commit-tree", "-p", f.Base, "-m", "edit", tree)
}

func blobText(t testing.TB, repo string, env []string, path string) string {
	t.Helper()
	cmd := exec.Command("git", "cat-file", "blob", ":"+path)
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git cat-file blob :%s: %v", path, err)
	}
	return string(out)
}

func hashObject(t testing.TB, repo, text string) string {
	t.Helper()
	cmd := exec.Command("git", "hash-object", "-w", "--stdin")
	cmd.Dir = repo
	cmd.Stdin = strings.NewReader(text)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git hash-object: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func git(t testing.TB, dir string, args ...string) string {
	t.Helper()
	return gitEnv(t, dir, nil, args...)
}

func gitEnv(t testing.TB, dir string, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(string(out))
}

func write(t testing.TB, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func appendFile(t testing.TB, path, text string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(text); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

// copyTree copies the regular files below src to dst, skipping the top-level entries skip accepts.
func copyTree(src, dst string, skip func(rel string) bool) error {
	return filepath.WalkDir(src, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel != "." && skip != nil && !strings.Contains(rel, string(filepath.Separator)) && skip(rel) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("%s is not a regular file", path)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, raw, 0o644)
	})
}
