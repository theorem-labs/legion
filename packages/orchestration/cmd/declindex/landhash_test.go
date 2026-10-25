package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/orchestration/declindex"
)

// TestLandHashBaseSnapshotPrintsModulePath catches an implementation that emits the former two-column
// protocol rather than identifying the declaration's repository-relative source path.
func TestLandHashBaseSnapshotPrintsModulePath(t *testing.T) {
	config := fixture(t)
	tree := filepath.Join(filepath.Dir(config), "tree")
	git(t, tree, "init", "-q")
	git(t, tree, "add", "-A")
	git(t, tree, "commit", "-q", "-m", "base")
	if r := invoke(t, "--config", config, "refresh"); r.code != 0 {
		t.Fatalf("refresh: %+v", r)
	}

	names, err := json.Marshal([]string{"Gold.frame_lo", "Gold2.FrameAt"})
	if err != nil {
		t.Fatal(err)
	}
	values := envValues{
		"LAND_TREE":    git(t, tree, "rev-parse", "HEAD^{tree}"),
		"LAND_GIT_DIR": filepath.Join(tree, ".git"),
		"LAND_NAMES":   string(names),
	}
	var out, errOut bytes.Buffer
	code := run(context.Background(), withIndexDSN([]string{"--config", config, "land-hash"}), values.get, &out, &errOut)
	const want = "Gold.frame_lo\t9d9fba31f41fc258\tGolden.lean\nGold2.FrameAt\t7f52be71a38bde18\tSub/Wrap.lean\n"
	if code != 0 || out.String() != want {
		t.Fatalf("base land-hash = (%d, %q, %q), want (0, %q, any stderr)", code, out.String(), errOut.String(), want)
	}
}

func invokeLandHash(t *testing.T, config string, values envValues, args ...string) result {
	t.Helper()
	var out, errOut bytes.Buffer
	code := run(context.Background(), withIndexDSN(append([]string{"--config", config, "land-hash"}, args...)), values.get, &out, &errOut)
	return result{code: code, out: out.String(), errOut: errOut.String()}
}

// TestLandHashUsesAFreshCandidateBuild catches the pre-build implementation that rejected every source blob
// changed by the candidate instead of reading its verified .ilean.
func TestLandHashUsesAFreshCandidateBuild(t *testing.T) {
	config := fixture(t)
	tree := filepath.Join(filepath.Dir(config), "tree")
	git(t, tree, "init", "-q")
	git(t, tree, "add", "-A")
	git(t, tree, "commit", "-q", "-m", "base")
	if r := invoke(t, "--config", config, "refresh"); r.code != 0 {
		t.Fatalf("refresh: %+v", r)
	}
	sourcePath := filepath.Join(tree, "Golden.lean")
	source, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	candidate := strings.Replace(string(source), "sp.toNat ≥ 16 := by", "sp.toNat ≥ 32 := by", 1)
	if candidate == string(source) {
		t.Fatal("fixture no longer contains the statement to change")
	}
	if err := os.WriteFile(sourcePath, []byte(candidate), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, tree, "commit", "-q", "-am", "candidate")
	build := t.TempDir()
	if err := os.WriteFile(filepath.Join(build, "Golden.lean"), []byte(candidate), 0o644); err != nil {
		t.Fatal(err)
	}
	ilean, err := os.ReadFile(filepath.Join(tree, "Golden.ilean"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(build, "Golden.ilean"), ilean, 0o644); err != nil {
		t.Fatal(err)
	}
	trace := fmt.Sprintf(`{"schemaVersion":"2025-09-10","inputs":[[%q,%q]],"outputs":{"i":%q},"synthetic":false}`,
		filepath.Join(build, "Golden.lean"), declindex.LakeTextHash([]byte(candidate)), declindex.LakeTextHash(ilean)+".ilean")
	if err := os.WriteFile(filepath.Join(build, "Golden.trace"), []byte(trace), 0o644); err != nil {
		t.Fatal(err)
	}
	names, err := json.Marshal([]string{"Gold.frame_lo"})
	if err != nil {
		t.Fatal(err)
	}
	r := invokeLandHash(t, config, envValues{
		"LAND_TREE":      git(t, tree, "rev-parse", "HEAD^{tree}"),
		"LAND_GIT_DIR":   filepath.Join(tree, ".git"),
		"LAND_BUILD_DIR": build,
		"LAND_NAMES":     string(names),
	})
	const want = "Gold.frame_lo\tdd801c6f96a0e63c\tGolden.lean\n"
	if r.code != 0 || r.out != want {
		t.Fatalf("candidate land-hash = (%d, %q, %q), want (0, %q, any stderr)", r.code, r.out, r.errOut, want)
	}
}

type envValues map[string]string

func (e envValues) get(k string) string { return e[k] }

func landBase(t *testing.T) (string, string, string) {
	t.Helper()
	config := fixture(t)
	tree := filepath.Join(filepath.Dir(config), "tree")
	git(t, tree, "init", "-q")
	git(t, tree, "add", "-A")
	git(t, tree, "commit", "-q", "-m", "base")
	if r := invoke(t, "--config", config, "refresh"); r.code != 0 {
		t.Fatalf("refresh: %+v", r)
	}
	return config, tree, git(t, tree, "rev-parse", "HEAD^{tree}")
}

func namesJSON(t *testing.T, names ...string) string {
	t.Helper()
	raw, err := json.Marshal(names)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func landValues(tree, treeID, names, build string) envValues {
	values := envValues{
		"LAND_TREE":    treeID,
		"LAND_GIT_DIR": filepath.Join(tree, ".git"),
		"LAND_NAMES":   names,
	}
	if build != "" {
		values["LAND_BUILD_DIR"] = build
	}
	return values
}

func writeLandFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeLandBuild(t *testing.T, build, rel string, source, ilean []byte, synthetic bool, hash string) string {
	t.Helper()
	sourcePath := filepath.Join(build, filepath.FromSlash(rel)+".lean")
	writeLandFile(t, sourcePath, source)
	writeLandFile(t, filepath.Join(build, filepath.FromSlash(rel)+".ilean"), ilean)
	var inputs any = [][]string{{sourcePath, hash}}
	if synthetic {
		inputs = [][]string{}
	}
	trace, err := json.Marshal(map[string]any{
		"schemaVersion": "2025-09-10",
		"inputs":        inputs,
		"outputs":       map[string]any{"i": declindex.LakeTextHash(ilean) + ".ilean"},
		"synthetic":     synthetic,
	})
	if err != nil {
		t.Fatal(err)
	}
	tracePath := filepath.Join(build, filepath.FromSlash(rel)+".trace")
	writeLandFile(t, tracePath, trace)
	return tracePath
}

func changeGolden(t *testing.T, tree string) ([]byte, []byte, string) {
	t.Helper()
	sourcePath := filepath.Join(tree, "Golden.lean")
	source, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	candidate := strings.Replace(string(source), "sp.toNat ≥ 16 := by", "sp.toNat ≥ 32 := by", 1)
	if candidate == string(source) {
		t.Fatal("fixture no longer contains the statement to change")
	}
	if err := os.WriteFile(sourcePath, []byte(candidate), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, tree, "commit", "-q", "-am", "candidate")
	ilean, err := os.ReadFile(filepath.Join(tree, "Golden.ilean"))
	if err != nil {
		t.Fatal(err)
	}
	return []byte(candidate), ilean, git(t, tree, "rev-parse", "HEAD^{tree}")
}

// TestLandHashRejectsChangedSourceWithoutABuild catches an implementation that silently reuses the index when
// LAND_TREE has a changed source and no candidate build was provided.
func TestLandHashRejectsChangedSourceWithoutABuild(t *testing.T) {
	config, tree, _ := landBase(t)
	_, _, candidate := changeGolden(t, tree)
	r := invokeLandHash(t, config, landValues(tree, candidate, namesJSON(t, "Gold.frame_lo"), ""))
	if r.code != 3 || r.out != "" {
		t.Fatalf("changed source without build = %+v, want exit 3 and no stdout", r)
	}
}

func TestLandHashTreatsChangedSourceWithoutOutputsAsAbsent(t *testing.T) {
	config, tree, _ := landBase(t)
	_, _, candidate := changeGolden(t, tree)
	r := invokeLandHash(t, config, landValues(tree, candidate, namesJSON(t, "Gold.frame_lo"), t.TempDir()))
	if r.code != 0 || r.out != "Gold.frame_lo\tabsent\t-\n" {
		t.Fatalf("changed source without outputs = %+v", r)
	}
}

func TestLandHashRejectsStaleCandidateBuilds(t *testing.T) {
	t.Run("wrong build source hash", func(t *testing.T) {
		config, tree, _ := landBase(t)
		source, ilean, candidate := changeGolden(t, tree)
		build := t.TempDir()
		writeLandBuild(t, build, "Golden", source, ilean, false, "0000000000000000")
		r := invokeLandHash(t, config, landValues(tree, candidate, namesJSON(t, "Gold.frame_lo"), build))
		if r.code != 3 || r.out != "" || !strings.Contains(r.errOut, "Golden") {
			t.Fatalf("wrong build source hash = %+v, want exit 3 naming Golden", r)
		}
	})
	t.Run("old fetch trace", func(t *testing.T) {
		config, tree, _ := landBase(t)
		source, ilean, candidate := changeGolden(t, tree)
		build := t.TempDir()
		trace := writeLandBuild(t, build, "Golden", source, ilean, true, "")
		fresh := invokeLandHash(t, config, landValues(tree, candidate, namesJSON(t, "Gold.frame_lo"), build))
		const want = "Gold.frame_lo\tdd801c6f96a0e63c\tGolden.lean\n"
		if fresh.code != 0 || fresh.out != want {
			t.Fatalf("fresh fetch trace = %+v, want %q", fresh, want)
		}
		now := time.Now()
		if err := os.Chtimes(trace, now, now); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(filepath.Join(build, "Golden.lean"), now.Add(time.Second), now.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		stale := invokeLandHash(t, config, landValues(tree, candidate, namesJSON(t, "Gold.frame_lo"), build))
		if stale.code != 3 || stale.out != "" || !strings.Contains(stale.errOut, "Golden") {
			t.Fatalf("old fetch trace = %+v, want exit 3 naming Golden", stale)
		}
	})
}

func TestLandHashUsesTheCandidatePathAfterAMove(t *testing.T) {
	config, tree, _ := landBase(t)
	source, err := os.ReadFile(filepath.Join(tree, "Golden.lean"))
	if err != nil {
		t.Fatal(err)
	}
	ilean, err := os.ReadFile(filepath.Join(tree, "Golden.ilean"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tree, "Sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, tree, "mv", "Golden.lean", "Sub/Moved.lean")
	git(t, tree, "commit", "-q", "-m", "move declaration")
	build := t.TempDir()
	writeLandBuild(t, build, "Sub/Moved", source, ilean, false, declindex.LakeTextHash(source))
	r := invokeLandHash(t, config, landValues(tree, git(t, tree, "rev-parse", "HEAD^{tree}"), namesJSON(t, "Gold.frame_lo"), build))
	const want = "Gold.frame_lo\t9d9fba31f41fc258\tSub/Moved.lean\n"
	if r.code != 0 || r.out != want {
		t.Fatalf("moved declaration = %+v, want %q", r, want)
	}
}

func TestLandHashPrintsAbsentAndAmbiguous(t *testing.T) {
	t.Run("deleted declaration", func(t *testing.T) {
		config, tree, _ := landBase(t)
		if err := os.Remove(filepath.Join(tree, "Golden.lean")); err != nil {
			t.Fatal(err)
		}
		git(t, tree, "add", "-A")
		git(t, tree, "commit", "-q", "-m", "delete declaration")
		r := invokeLandHash(t, config, landValues(tree, git(t, tree, "rev-parse", "HEAD^{tree}"), namesJSON(t, "Gold.frame_lo"), ""))
		if r.code != 0 || r.out != "Gold.frame_lo\tabsent\t-\n" {
			t.Fatalf("deleted declaration = %+v", r)
		}
	})
	t.Run("declaration in two paths", func(t *testing.T) {
		config, tree, _ := landBase(t)
		source, err := os.ReadFile(filepath.Join(tree, "Golden.lean"))
		if err != nil {
			t.Fatal(err)
		}
		ilean, err := os.ReadFile(filepath.Join(tree, "Golden.ilean"))
		if err != nil {
			t.Fatal(err)
		}
		writeLandFile(t, filepath.Join(tree, "Sub", "Duplicate.lean"), source)
		git(t, tree, "add", "-A")
		git(t, tree, "commit", "-q", "-m", "duplicate declaration")
		build := t.TempDir()
		writeLandBuild(t, build, "Sub/Duplicate", source, ilean, false, declindex.LakeTextHash(source))
		r := invokeLandHash(t, config, landValues(tree, git(t, tree, "rev-parse", "HEAD^{tree}"), namesJSON(t, "Gold.frame_lo"), build))
		const want = "Gold.frame_lo\tambiguous\tGolden.lean,Sub/Duplicate.lean\n"
		if r.code != 0 || r.out != want {
			t.Fatalf("ambiguous declaration = %+v, want %q", r, want)
		}
	})
}

func TestLandHashNamesFileAndInputValidation(t *testing.T) {
	config, tree, treeID := landBase(t)
	const want = "Gold.frame_lo\t9d9fba31f41fc258\tGolden.lean\n"
	file := filepath.Join(t.TempDir(), "names.json")
	if err := os.WriteFile(file, []byte("[\n  \"Gold.frame_lo\"\n]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fromFile := landValues(tree, treeID, "", "")
	delete(fromFile, "LAND_NAMES")
	fromFile["LAND_NAMES_FILE"] = file
	if r := invokeLandHash(t, config, fromFile); r.code != 0 || r.out != want {
		t.Fatalf("names file = %+v, want %q", r, want)
	}
	both := landValues(tree, treeID, namesJSON(t, "Gold.frame_lo"), "")
	both["LAND_NAMES_FILE"] = file
	if r := invokeLandHash(t, config, both); r.code != 0 || r.out != want {
		t.Fatalf("equal names inputs = %+v, want %q", r, want)
	}
	whitespace := landValues(tree, treeID, " ", "")
	whitespace["LAND_NAMES_FILE"] = file
	if r := invokeLandHash(t, config, whitespace); r.code != 3 || r.out != "" {
		t.Fatalf("whitespace LAND_NAMES plus file = %+v, want exit 3 and no stdout", r)
	}
	missingFile := landValues(tree, treeID, namesJSON(t, "Gold.frame_lo"), "")
	missingFile["LAND_NAMES_FILE"] = filepath.Join(t.TempDir(), "missing.json")
	if r := invokeLandHash(t, config, missingFile); r.code != 3 || r.out != "" {
		t.Fatalf("unreadable names file = %+v, want exit 3 and no stdout", r)
	}
	different := landValues(tree, treeID, namesJSON(t, "Gold.Aligned"), "")
	different["LAND_NAMES_FILE"] = file
	if r := invokeLandHash(t, config, different); r.code != 3 || r.out != "" {
		t.Fatalf("different names inputs = %+v, want exit 3 and no stdout", r)
	}
	neither := landValues(tree, treeID, "", "")
	delete(neither, "LAND_NAMES")
	if r := invokeLandHash(t, config, neither); r.code != 3 || r.out != "" {
		t.Fatalf("missing names input = %+v, want exit 3 and no stdout", r)
	}
	for _, raw := range []string{`["Gold.frame_lo","Gold.frame_lo"]`, `[""]`, `{}`} {
		values := landValues(tree, treeID, raw, "")
		if r := invokeLandHash(t, config, values); r.code != 3 || r.out != "" {
			t.Errorf("LAND_NAMES=%s: %+v, want exit 3 and no stdout", raw, r)
		}
	}
	withoutTree := landValues(tree, treeID, namesJSON(t, "Gold.frame_lo"), "")
	delete(withoutTree, "LAND_TREE")
	if r := invokeLandHash(t, config, withoutTree); r.code != 3 || r.out != "" {
		t.Fatalf("LAND_GIT_DIR without LAND_TREE = %+v, want exit 3 and no stdout", r)
	}
	if r := invokeLandHash(t, config, envValues{}, "unexpected"); r.code != 2 {
		t.Fatalf("argument = %+v, want exit 2", r)
	}
}

// TestLandHashAnswersADotlessName catches a request check that refuses a name without a dot, although the full
// name of a top-level declaration has none.
func TestLandHashAnswersADotlessName(t *testing.T) {
	config, tree, treeID := landBase(t)
	r := invokeLandHash(t, config, landValues(tree, treeID, namesJSON(t, "frame_lo", "Gold.frame_lo"), ""))
	const want = "frame_lo\tabsent\t-\nGold.frame_lo\t9d9fba31f41fc258\tGolden.lean\n"
	if r.code != 0 || r.out != want {
		t.Fatalf("dotless name = %+v, want %q", r, want)
	}
}

// TestLandHashAnswersFromTheIndexOnlyForASourceLandTreeHas catches a hasher that answers a declaration from the
// index when neither the index's sources nor LAND_TREE have its source file: two missing blobs compare equal.
func TestLandHashAnswersFromTheIndexOnlyForASourceLandTreeHas(t *testing.T) {
	config := fixture(t)
	tree := filepath.Join(filepath.Dir(config), "tree")
	writeLandFile(t, filepath.Join(tree, ".gitignore"), []byte("/Golden.lean\n"))
	git(t, tree, "init", "-q")
	git(t, tree, "add", "-A")
	git(t, tree, "commit", "-q", "-m", "base without Golden.lean")
	if r := invoke(t, "--config", config, "refresh"); r.code != 0 {
		t.Fatalf("refresh: %+v", r)
	}
	cfg := indexConfig(t, config)
	store, err := declindex.OpenReadOnly(context.Background(), cfg.Target(), declindex.IndexOnly)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.LandSnapshot(context.Background(), []string{"Gold.frame_lo"})
	store.Close()
	if err != nil {
		t.Fatal(err)
	}
	if _, indexed := snapshot.Modules["Golden"]; !indexed || len(snapshot.Declarations["Gold.frame_lo"]) != 1 {
		t.Fatalf("test setup: the index did not read Golden: %+v", snapshot)
	}
	if blob, ok := snapshot.Sources["Golden.lean"]; ok {
		t.Fatalf("test setup: the index's sources have the ignored Golden.lean as %s", blob)
	}
	treeID := git(t, tree, "rev-parse", "HEAD^{tree}")
	r := invokeLandHash(t, config, landValues(tree, treeID, namesJSON(t, "Gold.frame_lo", "Gold2.FrameAt"), ""))
	const want = "Gold.frame_lo\tabsent\t-\nGold2.FrameAt\t7f52be71a38bde18\tSub/Wrap.lean\n"
	if r.code != 0 || r.out != want {
		t.Fatalf("source missing from the index and LAND_TREE = %+v, want %q", r, want)
	}
}

// TestLandHashRefusesOnlyAPrintedPathThatCannotBePrinted catches a hasher that fails the whole call for any
// LAND_TREE source path with a tab, carriage return, newline or comma, rather than only for a path that an answer
// line would print.
func TestLandHashRefusesOnlyAPrintedPathThatCannotBePrinted(t *testing.T) {
	t.Run("unrelated paths", func(t *testing.T) {
		config := fixture(t)
		tree := filepath.Join(filepath.Dir(config), "tree")
		for _, rel := range []string{"Sub/Comma,Name.lean", "Sub/Tab\tName.lean"} {
			writeLandFile(t, filepath.Join(tree, filepath.FromSlash(rel)), []byte("-- not a requested module\n"))
		}
		git(t, tree, "init", "-q")
		git(t, tree, "add", "-A")
		git(t, tree, "commit", "-q", "-m", "base")
		if r := invoke(t, "--config", config, "refresh"); r.code != 0 {
			t.Fatalf("refresh: %+v", r)
		}
		for _, rel := range []string{"Sub/Line\nName.lean", "Sub/Return\rName.lean"} {
			writeLandFile(t, filepath.Join(tree, filepath.FromSlash(rel)), []byte("-- not a requested module\n"))
		}
		git(t, tree, "add", "-A")
		git(t, tree, "commit", "-q", "-m", "candidate")
		treeID := git(t, tree, "rev-parse", "HEAD^{tree}")
		r := invokeLandHash(t, config, landValues(tree, treeID, namesJSON(t, "Gold.frame_lo", "Gold2.FrameAt"), t.TempDir()))
		const want = "Gold.frame_lo\t9d9fba31f41fc258\tGolden.lean\nGold2.FrameAt\t7f52be71a38bde18\tSub/Wrap.lean\n"
		if r.code != 0 || r.out != want {
			t.Fatalf("unrelated unprintable paths = %+v, want %q", r, want)
		}
	})
	t.Run("printed path", func(t *testing.T) {
		config, tree, _ := landBase(t)
		source, err := os.ReadFile(filepath.Join(tree, "Golden.lean"))
		if err != nil {
			t.Fatal(err)
		}
		ilean, err := os.ReadFile(filepath.Join(tree, "Golden.ilean"))
		if err != nil {
			t.Fatal(err)
		}
		git(t, tree, "mv", "Golden.lean", "Sub/Gold,en.lean")
		git(t, tree, "commit", "-q", "-m", "move declaration to a path with a comma")
		build := t.TempDir()
		writeLandBuild(t, build, "Sub/Gold,en", source, ilean, false, declindex.LakeTextHash(source))
		treeID := git(t, tree, "rev-parse", "HEAD^{tree}")
		r := invokeLandHash(t, config, landValues(tree, treeID, namesJSON(t, "Gold.frame_lo"), build))
		if r.code != 3 || r.out != "" || !strings.Contains(r.errOut, "Sub/Gold,en.lean") {
			t.Fatalf("printed path with a comma = %+v, want exit 3, no stdout, and stderr naming the path", r)
		}
	})
}

// rewriteConfig replaces old with new in the fixture's configuration.
func rewriteConfig(t *testing.T, config, old, new string) {
	t.Helper()
	raw, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	body := strings.Replace(string(raw), old, new, 1)
	if body == string(raw) {
		t.Fatalf("configuration %q has no %q", raw, old)
	}
	writeIndexConfig(t, config, body)
}

// TestLandHashListsTheSourcesRefreshRecorded catches a hasher whose LAND_TREE pathspecs differ from the ones
// refresh records sources with: refresh drops an empty root, and land-hash asked Git for the pathspec "/".
func TestLandHashListsTheSourcesRefreshRecorded(t *testing.T) {
	config := fixture(t)
	rewriteConfig(t, config, "roots: [Golden, Sub]", `roots: [Golden, Sub, Sub, ""]`)
	tree := filepath.Join(filepath.Dir(config), "tree")
	git(t, tree, "init", "-q")
	git(t, tree, "add", "-A")
	git(t, tree, "commit", "-q", "-m", "base")
	if r := invoke(t, "--config", config, "refresh"); r.code != 0 {
		t.Fatalf("refresh: %+v", r)
	}
	treeID := git(t, tree, "rev-parse", "HEAD^{tree}")
	r := invokeLandHash(t, config, landValues(tree, treeID, namesJSON(t, "Gold.frame_lo", "Gold2.FrameAt"), ""))
	const want = "Gold.frame_lo\t9d9fba31f41fc258\tGolden.lean\nGold2.FrameAt\t7f52be71a38bde18\tSub/Wrap.lean\n"
	if r.code != 0 || r.out != want {
		t.Fatalf("empty and duplicate roots = %+v, want %q", r, want)
	}
}

// TestLandHashAnswersATreeBelowTheRepositoryRoot catches a hasher that lists the configured roots at the root of
// LAND_TREE when the configured tree is a directory below its repository's root, or that prints a tree-relative
// path where the contract gives the repository-relative one.
func TestLandHashAnswersATreeBelowTheRepositoryRoot(t *testing.T) {
	config := fixture(t)
	dir := filepath.Dir(config)
	repo := filepath.Join(dir, "repo")
	proj := filepath.Join(repo, "proj")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "tree"), proj); err != nil {
		t.Fatal(err)
	}
	rewriteConfig(t, config, "tree: tree\n", "tree: repo/proj\n")
	git(t, repo, "init", "-q")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-q", "-m", "base")
	if r := invoke(t, "--config", config, "refresh"); r.code != 0 {
		t.Fatalf("refresh: %+v", r)
	}

	base := invokeLandHash(t, config, landValues(repo, git(t, repo, "rev-parse", "HEAD^{tree}"),
		namesJSON(t, "Gold.frame_lo", "Gold2.FrameAt"), ""))
	const wantBase = "Gold.frame_lo\t9d9fba31f41fc258\tproj/Golden.lean\nGold2.FrameAt\t7f52be71a38bde18\tproj/Sub/Wrap.lean\n"
	if base.code != 0 || base.out != wantBase {
		t.Fatalf("base land-hash of a subdirectory tree = %+v, want %q", base, wantBase)
	}

	source, ilean, candidate := changeGolden(t, proj)
	build := t.TempDir()
	writeLandBuild(t, build, "Golden", source, ilean, false, declindex.LakeTextHash(source))
	changed := invokeLandHash(t, config, landValues(repo, candidate, namesJSON(t, "Gold.frame_lo"), build))
	const wantChanged = "Gold.frame_lo\tdd801c6f96a0e63c\tproj/Golden.lean\n"
	if changed.code != 0 || changed.out != wantChanged {
		t.Fatalf("candidate land-hash of a subdirectory tree = %+v, want %q", changed, wantChanged)
	}
}

func TestLandHashRefusesAnIndexWithoutGitSources(t *testing.T) {
	config := fixture(t)
	if r := invoke(t, "--config", config, "refresh"); r.code != 0 {
		t.Fatalf("refresh: %+v", r)
	}
	repo := t.TempDir()
	git(t, repo, "init", "-q")
	git(t, repo, "commit", "--allow-empty", "-q", "-m", "empty")
	r := invokeLandHash(t, config, envValues{
		"LAND_TREE":    git(t, repo, "rev-parse", "HEAD^{tree}"),
		"LAND_GIT_DIR": filepath.Join(repo, ".git"),
		"LAND_NAMES":   namesJSON(t, "Gold.frame_lo"),
	})
	if r.code != 3 || r.out != "" {
		t.Fatalf("index without sources = %+v, want exit 3 and no stdout", r)
	}
}

// TestLandHashReadsOneSnapshotWhileRefreshWaits catches a hasher that queries sources and declarations in separate
// transactions, which could combine an old source blob with a later refresh's declarations.
func TestLandHashReadsOneSnapshotWhileRefreshWaits(t *testing.T) {
	config, tree, baseTree := landBase(t)
	cfg := indexConfig(t, config)
	store, err := declindex.Open(context.Background(), cfg.Target())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	source := filepath.Join(tree, "Golden.lean")
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, append(data, []byte("\n-- third refresh\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, tree, "commit", "-q", "-am", "third")
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := store.Refresh(context.Background(), cfg.TreeSpec(), declindex.RefreshOptions{
			Commit: git(t, tree, "rev-parse", "HEAD"),
			Landed: func(context.Context) error {
				close(entered)
				<-release
				return nil
			},
		})
		done <- err
	}()
	<-entered
	r := invokeLandHash(t, config, landValues(tree, baseTree, namesJSON(t, "Gold.frame_lo"), ""))
	const want = "Gold.frame_lo\t9d9fba31f41fc258\tGolden.lean\n"
	if r.code != 0 || r.out != want {
		close(release)
		<-done
		t.Fatalf("land-hash during refresh = %+v, want %q", r, want)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// TestLandHashRefreshReparsesAChangedBlobWithRestoredMtime catches a refresh that records a new source blob next
// to declaration rows parsed from the old bytes, making land-hash return a stale index hash as unchanged.
func TestLandHashRefreshReparsesAChangedBlobWithRestoredMtime(t *testing.T) {
	config, tree, _ := landBase(t)
	sourcePath := filepath.Join(tree, "Golden.lean")
	before, err := os.Stat(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	candidate := strings.Replace(string(source), "sp.toNat ≥ 16 := by", "sp.toNat ≥ 32 := by", 1)
	if candidate == string(source) || len(candidate) != len(source) {
		t.Fatal("fixture no longer has a same-length statement change")
	}
	if err := os.WriteFile(sourcePath, []byte(candidate), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(sourcePath, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if before.Size() != after.Size() || before.ModTime().UnixNano() != after.ModTime().UnixNano() {
		t.Fatalf("test setup changed source key: before=%d/%d after=%d/%d", before.Size(), before.ModTime().UnixNano(),
			after.Size(), after.ModTime().UnixNano())
	}
	git(t, tree, "commit", "-q", "-am", "same source key, changed blob")
	if r := invoke(t, "--config", config, "refresh"); r.code != 0 {
		t.Fatalf("refresh changed blob: %+v", r)
	}
	r := invokeLandHash(t, config, landValues(tree, git(t, tree, "rev-parse", "HEAD^{tree}"), namesJSON(t, "Gold.frame_lo"), ""))
	const want = "Gold.frame_lo\tdd801c6f96a0e63c\tGolden.lean\n"
	if r.code != 0 || r.out != want {
		t.Fatalf("land-hash after changed blob refresh = %+v, want %q", r, want)
	}
}
