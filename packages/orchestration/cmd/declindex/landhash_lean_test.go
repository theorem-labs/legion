package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/orchestration/declindex"
)

func requireLake(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("lake"); err != nil {
		if os.Getenv("DECLINDEX_REQUIRE_LEAN") == "1" {
			t.Fatalf("lake is required: %v", err)
		}
		t.Skip("lake is not on PATH")
	}
}

func copyLandTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		to := filepath.Join(dst, rel)
		if entry.IsDir() {
			return os.MkdirAll(to, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(to, data, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func runLake(t *testing.T, dir, cache string) {
	t.Helper()
	cmd := exec.Command("lake", "build")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "LAKE_ARTIFACT_CACHE=true", "LAKE_CACHE_DIR="+cache)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("lake build: %v\n%s", err, out)
	}
}

func copyOutput(t *testing.T, from, to string) {
	t.Helper()
	data, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(to); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func traceSourceHash(raw json.RawMessage, sourcePath string) (string, bool) {
	var entries []json.RawMessage
	if json.Unmarshal(raw, &entries) != nil {
		return "", false
	}
	for _, entry := range entries {
		var pair []json.RawMessage
		if json.Unmarshal(entry, &pair) != nil || len(pair) != 2 {
			continue
		}
		var caption string
		if json.Unmarshal(pair[0], &caption) != nil {
			continue
		}
		if sameTraceSourceCaption(caption, sourcePath) {
			var hash string
			if json.Unmarshal(pair[1], &hash) == nil {
				return hash, true
			}
			return "", false
		}
		if hash, ok := traceSourceHash(pair[1], sourcePath); ok {
			return hash, true
		}
	}
	return "", false
}

func sameTraceSourceCaption(caption, sourcePath string) bool {
	if filepath.Clean(caption) == filepath.Clean(sourcePath) {
		return true
	}
	return !filepath.IsAbs(caption) && strings.HasSuffix(filepath.ToSlash(filepath.Clean(sourcePath)),
		"/"+filepath.ToSlash(filepath.Clean(caption)))
}

// TestLandHashWithLakeBuild drives a real Lake package through the base and candidate calls. It proves both the
// trace caption/hash port and artifact-cache fetch traces against the CI toolchain.
func TestLandHashWithLakeBuild(t *testing.T) {
	requireLake(t)
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	copyLandTree(t, filepath.Join("..", "..", "declindex", "testdata", "lean"), filepath.Join(repo, "LandHashFixture"))
	const lakefile = `name = "landhashfixture"
defaultTargets = ["LandHashFixture"]

[[lean_lib]]
name = "LandHashFixture"
`
	if err := os.WriteFile(filepath.Join(repo, "lakefile.toml"), []byte(lakefile), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "LandHashFixture.lean"), []byte("import LandHashFixture.Fixture\nimport LandHashFixture.Copies\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte(".lake/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "init", "-q")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-q", "-m", "A")
	cache := filepath.Join(dir, "lake-cache")
	runLake(t, repo, cache)
	config := filepath.Join(dir, "index.yaml")
	configText := "tree: " + repo + "\nlib: .lake/build/lib/lean\ndb: " + filepath.Join(dir, "index.sqlite") + "\nroots: [LandHashFixture]\n"
	if err := os.WriteFile(config, []byte(configText), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := invoke(t, "--config", config, "refresh"); r.code != 0 {
		t.Fatalf("refresh A: %+v", r)
	}
	baseTree := git(t, repo, "rev-parse", "HEAD^{tree}")

	fixturePath := filepath.Join(repo, "LandHashFixture", "Fixture.lean")
	fixture, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	candidateSource := strings.Replace(string(fixture), "&&& 7#64 = 0#64", "&&& 3#64 = 0#64", 1)
	if candidateSource == string(fixture) {
		t.Fatal("Lean fixture no longer contains the changed statement")
	}
	if err := os.WriteFile(fixturePath, []byte(candidateSource), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "LandHashFixture", "New.lean"), []byte("namespace Fx\ndef New : Prop := True\nend Fx\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-q", "-m", "B")
	candidateTree := git(t, repo, "rev-parse", "HEAD^{tree}")

	build := filepath.Join(dir, "candidate")
	if err := os.MkdirAll(build, 0o755); err != nil {
		t.Fatal(err)
	}
	archive, err := exec.Command("git", "-C", repo, "archive", candidateTree).Output()
	if err != nil {
		t.Fatal(err)
	}
	untar := exec.Command("tar", "-x", "-C", build)
	untar.Stdin = bytes.NewReader(archive)
	if out, err := untar.CombinedOutput(); err != nil {
		t.Fatalf("git archive | tar -x: %v\n%s", err, out)
	}
	runLake(t, build, cache)

	names := namesJSON(t, "Fx.SpAligned")
	base := invokeLandHash(t, config, landValues(repo, baseTree, names, ""))
	if base.code != 0 {
		t.Fatalf("base land-hash: %+v", base)
	}
	candidate := invokeLandHash(t, config, landValues(repo, candidateTree, names, build))
	if candidate.code != 0 {
		t.Fatalf("candidate land-hash: %+v", candidate)
	}
	baseColumns := strings.Split(strings.TrimSpace(base.out), "\t")
	candidateColumns := strings.Split(strings.TrimSpace(candidate.out), "\t")
	if len(baseColumns) != 3 || len(candidateColumns) != 3 || baseColumns[0] != "Fx.SpAligned" ||
		candidateColumns[0] != "Fx.SpAligned" || baseColumns[2] != "LandHashFixture/Fixture.lean" ||
		candidateColumns[2] != "LandHashFixture/Fixture.lean" || baseColumns[1] == candidateColumns[1] {
		t.Fatalf("base %q, candidate %q: want changed hash and LandHashFixture/Fixture.lean path", base.out, candidate.out)
	}

	lib := filepath.Join(build, ".lake", "build", "lib", "lean")
	sawFetch := false
	err = filepath.WalkDir(lib, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".trace") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var trace struct {
			Synthetic bool            `json:"synthetic"`
			Inputs    json.RawMessage `json:"inputs"`
		}
		if err := json.Unmarshal(raw, &trace); err != nil {
			return err
		}
		rel, err := filepath.Rel(lib, strings.TrimSuffix(path, ".trace"))
		if err != nil {
			return err
		}
		source, err := os.ReadFile(filepath.Join(build, rel+".lean"))
		if err != nil {
			return err
		}
		if trace.Synthetic {
			sawFetch = true
			freshness, _, err := declindex.Fresh(build, filepath.Join(".lake", "build", "lib", "lean"), filepath.ToSlash(rel), source)
			if err != nil {
				return err
			}
			if freshness.State != declindex.FreshnessFresh {
				return fmt.Errorf("%s fetch trace is %+v", path, freshness)
			}
			return nil
		}
		hash, ok := traceSourceHash(trace.Inputs, filepath.Join(build, rel+".lean"))
		if !ok || hash != declindex.LakeTextHash(source) {
			return fmt.Errorf("%s does not carry LakeTextHash for its source: %s", path, raw)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !sawFetch {
		t.Fatal("candidate build restored no unchanged module through the artifact cache")
	}

	for _, suffix := range []string{".ilean", ".trace"} {
		copyOutput(t, filepath.Join(repo, ".lake", "build", "lib", "lean", "LandHashFixture", "Fixture"+suffix),
			filepath.Join(lib, "LandHashFixture", "Fixture"+suffix))
	}
	stale := invokeLandHash(t, config, landValues(repo, candidateTree, names, build))
	if stale.code != 3 || stale.out != "" {
		t.Fatalf("A output copied over B = %+v, want exit 3 and no stdout", stale)
	}
}
