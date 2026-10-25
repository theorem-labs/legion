package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The post-landing registry check, without Lean: the cases where the recheck finds no concept to compile, or
// where it cannot run at all (a configured toolchain that does not exist stands for any such cause).

const (
	conceptKeyA = "0123456789abcdef0123456789abcdef"
	conceptKeyB = "fedcba9876543210fedcba9876543210"
	// unavailableLean makes every recheck that needs Lean unable to run.
	unavailableLean = "lean:\n  toolchain: /nonexistent-lean-toolchain\n"
)

func registryConcept(name, hash, proposal, body string) string {
	return "name: " + name + "\nhome: Golden.lean\nmeaning: Register x2 is 8-aligned at every step of the frame walk.\n" +
		"proposal: " + proposal + "\nproposer: function:alloc_mnt_ns\ncompiled_hash: " + hash + "\nsource: |\n" +
		"  import Golden\n  def " + name + " (n : Nat) : Prop := " + body + "\n"
}

func writeTestFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// registryFixture is the golden fixture as a Git tree with a format-2 registry holding concepts, a contract rule
// that selects nothing (so the contract half of `ledger` is clean), and an initial refresh.
func registryFixture(t *testing.T, extra string, concepts map[string]string) (config, tree string) {
	t.Helper()
	config = fixture(t)
	tree = filepath.Join(filepath.Dir(config), "tree")
	body, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	body = append(body, []byte("repo: r\nregistry: registry\ncontracts:\n  paths: [\"Nowhere/**\"]\n  kinds: [def]\n"+extra)...)
	writeTestFile(t, config, string(body))
	writeTestFile(t, filepath.Join(tree, "registry", "registry.yaml"), "version: 2\n")
	for key, c := range concepts {
		writeTestFile(t, filepath.Join(tree, "registry", "concepts", key+".yaml"), c)
	}
	git(t, tree, "init", "-q")
	git(t, tree, "add", "-A")
	git(t, tree, "commit", "-q", "-m", "one")
	if r := invoke(t, "--config", config, "refresh"); r.code != 0 {
		t.Fatalf("initial refresh: %+v", r)
	}
	return config, tree
}

// land commits the tree's changes and refreshes the index, as a landing does; it returns the landed commit.
func land(t *testing.T, config, tree, msg string) string {
	t.Helper()
	git(t, tree, "add", "-A")
	git(t, tree, "commit", "-q", "-m", msg)
	if r := invoke(t, "--config", config, "refresh"); r.code != 0 {
		t.Fatalf("refresh after %s: %+v", msg, r)
	}
	return git(t, tree, "rev-parse", "HEAD")
}

type registryCheck struct {
	code    int
	added   []string
	defects []string
	unable  string
	stderr  string
	ops     []map[string]any
	raw     string
}

// checkRegistry runs the landing hook's `ledger --contracts-file --emit`, with LAND_COMMIT when landed is set.
func checkRegistry(t *testing.T, config, landed string) registryCheck {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "scratch")
	contracts := filepath.Join(dir, "contracts.json")
	writeTestFile(t, contracts, "[]\n")
	emit := filepath.Join(dir, "ops.jsonl")
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"--config", config, "ledger", "--contracts-file", contracts, "--emit", emit},
		func(k string) string {
			if k == "LAND_COMMIT" {
				return landed
			}
			return ""
		}, &out, &errOut)
	var report struct {
		Registry *struct {
			Added   []string `json:"added"`
			Defects []struct {
				Path    string `json:"path"`
				Message string `json:"message"`
			} `json:"defects"`
			Unable string `json:"unable"`
		} `json:"registry"`
	}
	r := registryCheck{code: code, stderr: errOut.String(), raw: out.String()}
	if err := json.Unmarshal(out.Bytes(), &report); err == nil && report.Registry != nil {
		r.added, r.unable = report.Registry.Added, report.Registry.Unable
		for _, d := range report.Registry.Defects {
			r.defects = append(r.defects, d.Path+": "+d.Message)
		}
	}
	if raw, err := os.ReadFile(emit); err == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			if line == "" {
				continue
			}
			var op map[string]any
			if err := json.Unmarshal([]byte(line), &op); err != nil {
				t.Fatalf("emitted line %q: %v", line, err)
			}
			r.ops = append(r.ops, op)
		}
	}
	t.Logf("ledger exit %d, added %v, defects %v, unable %q\nstderr: %s", r.code, r.added, r.defects, r.unable, r.stderr)
	return r
}

func conceptPath(key string) string { return "registry/concepts/" + key + ".yaml" }

// A landing whose registry recheck cannot run at all exits 75, so the landing program runs that landing's check
// again; the check of that landing compares the same two commits even after later landings moved the index. The
// later landing's own check, and a check without LAND_COMMIT (the index's commit, that later landing), do not include
// it. A LAND_COMMIT that the tree does not hold cannot be compared with the index's commit: exit 3.
func TestARegistryRecheckThatCannotRunIsRetriedForItsLanding(t *testing.T) {
	config, tree := registryFixture(t, unavailableLean, nil)
	writeTestFile(t, filepath.Join(tree, "registry", "concepts", conceptKeyB+".yaml"), registryConcept("P.New", conceptKeyB, "p2", "n < 4"))
	second := land(t, config, tree, "add B")
	want := []string{conceptPath(conceptKeyB)}
	if r := checkRegistry(t, config, second); r.code != 75 || !slices.Equal(r.added, want) || r.unable == "" {
		t.Fatalf("the check of the landing that added B: want exit 75 with B added and a cause, got %+v", r)
	}
	writeTestFile(t, filepath.Join(tree, "notes.txt"), "unrelated\n")
	third := land(t, config, tree, "unrelated landing")
	if r := checkRegistry(t, config, second); r.code != 75 || !slices.Equal(r.added, want) {
		t.Fatalf("the retried check of the landing that added B, after a later landing: want exit 75 with B added, got %+v", r)
	}
	if r := checkRegistry(t, config, third); r.code != 0 || r.added == nil || len(r.added) != 0 {
		t.Fatalf("the check of the unrelated landing: want exit 0 and no addition, got %+v", r)
	}
	if r := checkRegistry(t, config, ""); r.code != 0 || r.added == nil || len(r.added) != 0 {
		t.Errorf("the check without LAND_COMMIT, of the index's commit (the unrelated landing): want exit 0 and no addition, got %+v", r)
	}
	if r := checkRegistry(t, config, strings.Repeat("0", 40)); r.code != 3 || !strings.Contains(r.stderr, "compare commits") {
		t.Errorf("the check of a LAND_COMMIT that the tree does not hold: want exit 3, unable to compare commits, got %+v", r)
	}
}

// A refused concept file is the defect of that file alone: a good concept file that the same landing adds is still
// listed and rechecked, the ledger reports only the refused file, and its exit is the defect's or the recheck's,
// never "unable to check the contracts" (3). The index and its queries go on.
func TestARefusedConceptFileIsTheDefectOfThatFileAlone(t *testing.T) {
	config, tree := registryFixture(t, unavailableLean, nil)
	writeTestFile(t, filepath.Join(tree, "registry", "concepts", "P.Bad.yaml"), "name: P.Bad\nhome: Golden.lean\nmeaning: Register x2 is aligned. It stays so.\n")
	writeTestFile(t, filepath.Join(tree, "registry", "concepts", conceptKeyB+".yaml"), registryConcept("P.New", conceptKeyB, "p2", "n < 4"))
	landed := land(t, config, tree, "a good and a refused concept file")
	r := checkRegistry(t, config, landed)
	if want := []string{conceptPath("P.Bad"), conceptPath(conceptKeyB)}; !slices.Equal(r.added, want) {
		t.Errorf("added %v, want both files of the landing %v", r.added, want)
	}
	if len(r.defects) != 1 || !strings.Contains(r.defects[0], "concepts/P.Bad.yaml") {
		t.Errorf("defects %v, want the refused file P.Bad.yaml alone", r.defects)
	}
	// The good concept's recheck cannot run here (no toolchain), and a recheck that cannot run outranks a defect.
	if r.code != 75 || r.unable == "" {
		t.Errorf("want exit 75 with a cause (the good concept's recheck cannot run), got %+v", r)
	}
	if s := invoke(t, "--config", config, "--json", "status"); s.code != 0 {
		t.Errorf("status after the refused file: %+v", s)
	}
}

// A concept file added in the landing that deletes a similar one is an addition, not a rename.
func TestARegistryAdditionBehindADeletionIsRechecked(t *testing.T) {
	t.Run("a pure addition", func(t *testing.T) {
		config, tree := registryFixture(t, unavailableLean, map[string]string{conceptKeyA: registryConcept("P.Old", conceptKeyA, "p1", "n < 3")})
		writeTestFile(t, filepath.Join(tree, "registry", "concepts", conceptKeyB+".yaml"), registryConcept("P.New", conceptKeyB, "p2", "n < 4"))
		landed := land(t, config, tree, "add B")
		if r := checkRegistry(t, config, landed); !slices.Equal(r.added, []string{conceptPath(conceptKeyB)}) || r.code != 75 {
			t.Fatalf("want B added and the recheck unable, got %+v", r)
		}
	})
	t.Run("A deleted and B added in one landing", func(t *testing.T) {
		config, tree := registryFixture(t, unavailableLean, map[string]string{conceptKeyA: registryConcept("P.Old", conceptKeyA, "p1", "n < 3")})
		git(t, tree, "rm", "-q", filepath.Join("registry", "concepts", conceptKeyA+".yaml"))
		writeTestFile(t, filepath.Join(tree, "registry", "concepts", conceptKeyB+".yaml"), registryConcept("P.New", conceptKeyB, "p2", "n < 4"))
		landed := land(t, config, tree, "replace A by B")
		if r := checkRegistry(t, config, landed); !slices.Equal(r.added, []string{conceptPath(conceptKeyB)}) || r.code != 75 {
			t.Fatalf("B was added in this landing: want it rechecked, got %+v", r)
		}
	})
}

// A tree below its repository's root: the landing hook's check lists an added concept file by its
// repository-relative path, the path the gate printed as its addition, and rechecks it.
func TestARegistryAdditionInATreeBelowTheRepositoryRootIsRechecked(t *testing.T) {
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
	body, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, config, string(body)+"repo: r\nregistry: registry\ncontracts:\n  paths: [\"Nowhere/**\"]\n  kinds: [def]\n"+unavailableLean)
	writeTestFile(t, filepath.Join(proj, "registry", "registry.yaml"), "version: 2\n")
	git(t, repo, "init", "-q")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-q", "-m", "one")
	if r := invoke(t, "--config", config, "refresh"); r.code != 0 {
		t.Fatalf("initial refresh: %+v", r)
	}
	writeTestFile(t, filepath.Join(proj, "registry", "concepts", conceptKeyB+".yaml"), registryConcept("P.New", conceptKeyB, "p2", "n < 4"))
	landed := land(t, config, repo, "add B")
	want := []string{"proj/" + conceptPath(conceptKeyB)}
	if r := checkRegistry(t, config, landed); r.code != 75 || !slices.Equal(r.added, want) || r.unable == "" {
		t.Fatalf("the check of the landing that added B below the repository root: want exit 75 with %v added and a cause, got %+v", want, r)
	}
}

// A concept file that the loader refuses (a two-sentence meaning, an unknown key) is a registry defect of that
// file: the ledger exits 1 and names it; it is not unable to check.
func TestAMalformedConceptFileIsALedgerDefect(t *testing.T) {
	config, tree := registryFixture(t, unavailableLean, nil)
	writeTestFile(t, filepath.Join(tree, "registry", "concepts", "P.Bad.yaml"), "name: P.Bad\nhome: Golden.lean\nmeaning: Register x2 is aligned. It stays so.\n")
	writeTestFile(t, filepath.Join(tree, "registry", "concepts", "P.Odd.yaml"), "name: P.Odd\nhome: Golden.lean\nmeaning: A fact with an unknown key.\ncolour: red\n")
	landed := land(t, config, tree, "a librarian adds two concepts by hand")
	r := checkRegistry(t, config, landed)
	if r.code != 1 || r.unable != "" {
		t.Fatalf("want exit 1 with no inability, got %+v", r)
	}
	joined := strings.Join(r.defects, "\n")
	for _, file := range []string{"concepts/P.Bad.yaml", "concepts/P.Odd.yaml"} {
		if !strings.Contains(joined, file) {
			t.Errorf("defects %v do not name %s", r.defects, file)
		}
	}
}

// A landing that adds no concept reports `"added": []`, as the first landing does, never null.
func TestALandingWithoutRegistryAdditionsReportsAnEmptyList(t *testing.T) {
	config, tree := registryFixture(t, "", map[string]string{conceptKeyA: registryConcept("P.Old", conceptKeyA, "p1", "n < 3")})
	writeTestFile(t, filepath.Join(tree, "notes.txt"), "unrelated\n")
	landed := land(t, config, tree, "unrelated landing")
	r := checkRegistry(t, config, landed)
	if r.code != 0 || !strings.Contains(r.raw, "\"added\": []") {
		t.Fatalf("want exit 0 and an empty added list, got %+v\n%s", r, r.raw)
	}
}

// The landing hook mounts the index directory read-only: `ledger` writes nothing there (its registry snapshot and
// proposal work go to the temporary directory) and still checks.
func TestLedgerChecksWithAReadOnlyIndexDirectory(t *testing.T) {
	config, tree := registryFixture(t, "", map[string]string{conceptKeyA: registryConcept("P.Old", conceptKeyA, "p1", "n < 3")})
	writeTestFile(t, filepath.Join(tree, "notes.txt"), "unrelated\n")
	landed := land(t, config, tree, "unrelated landing")
	state := filepath.Join(filepath.Dir(config), "state")
	if err := os.Chmod(state, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(state, 0o755) })
	t.Setenv("TMPDIR", t.TempDir())
	if r := checkRegistry(t, config, landed); r.code != 0 {
		t.Fatalf("ledger with a read-only index directory: want exit 0, got %+v", r)
	}
}

// --dsn-file names the operator's ledger; with --contracts-file it would be ignored, so the pair is refused.
func TestLedgerRefusesADSNFileWithAContractsFile(t *testing.T) {
	config, _ := registryFixture(t, "", nil)
	dir := t.TempDir()
	dsn, contracts := filepath.Join(dir, "dsn"), filepath.Join(dir, "contracts.json")
	writeTestFile(t, dsn, "postgres://127.0.0.1:1/unused\n")
	writeTestFile(t, contracts, "[]\n")
	r := invoke(t, "--config", config, "ledger", "--dsn-file", dsn, "--contracts-file", contracts, "--emit", filepath.Join(dir, "ops.jsonl"))
	if r.code != 2 {
		t.Fatalf("ledger with --dsn-file and --contracts-file: %+v", r)
	}
}

// LAND_COMMIT is a full object id as Git prints it: 40 lowercase hexadecimal characters.
func TestLandCommitMustBeLowercaseHex(t *testing.T) {
	config, tree := registryFixture(t, "", nil)
	upper := strings.ToUpper(git(t, tree, "rev-parse", "HEAD"))
	env := func(k string) string {
		if k == "LAND_COMMIT" {
			return upper
		}
		return ""
	}
	for _, args := range [][]string{{"refresh"}, {"ledger", "--contracts-file", filepath.Join(t.TempDir(), "c.json"), "--emit", filepath.Join(t.TempDir(), "o.jsonl")}} {
		var out, errOut bytes.Buffer
		if code := run(context.Background(), append([]string{"--config", config}, args...), env, &out, &errOut); code != 2 {
			t.Errorf("%s with an uppercase LAND_COMMIT exited %d: %s", args[0], code, errOut.String())
		}
	}
}

// `same` prints its answers in a fixed order.
func TestSamePrintsItsAnswersInNameOrder(t *testing.T) {
	config := fixture(t)
	if r := invoke(t, "--config", config, "refresh"); r.code != 0 {
		t.Fatalf("refresh: %+v", r)
	}
	var first string
	for i := range 20 {
		r := invoke(t, "--config", config, "same", "Gold.frame_lo", "Gold.Aligned", "Gold.Frame", "Gold2.FrameAt")
		if r.code != 0 {
			t.Fatalf("same: %+v", r)
		}
		if i == 0 {
			first = r.out
			continue
		}
		if r.out != first {
			t.Fatalf("same printed\n%s\nthen\n%s", first, r.out)
		}
	}
	lines := strings.Split(strings.TrimSpace(first), "\n")
	if !slices.IsSorted(lines) {
		t.Fatalf("same output is not in name order:\n%s", first)
	}
}

// A landed refresh whose compiled step fails leaves the landing uncovered: the retry runs the compiled step
// again rather than exiting 0 with the previous commit's compiled part.
func TestARetryAfterAFailedCompiledStepRunsItAgain(t *testing.T) {
	f := newLandingFixture(t)
	body, err := os.ReadFile(f.config)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, f.config, string(body)+"lean:\n  export: true\n  toolchain: /nonexistent-lean-toolchain\n")
	if r := f.refreshAt(t, f.second); r.code != 3 {
		t.Fatalf("a landed refresh whose compiled step fails: %+v", r)
	}
	if r := f.refreshAt(t, f.second); r.code != 3 || !strings.Contains(r.errOut, "nonexistent-lean-toolchain") {
		t.Fatalf("the retry: want the compiled step to run again and fail, got %+v", r)
	}
}
