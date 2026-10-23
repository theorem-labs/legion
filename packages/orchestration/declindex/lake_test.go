package declindex

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type lakeHashVectors struct {
	Toolchain string `json:"toolchain"`
	Vectors   []struct {
		Text string `json:"text"`
		Hash string `json:"hash"`
	} `json:"vectors"`
}

func loadLakeHashVectors(t *testing.T) lakeHashVectors {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "lakehash", "vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var vectors lakeHashVectors
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatal(err)
	}
	if vectors.Toolchain == "" || len(vectors.Vectors) == 0 {
		t.Fatalf("invalid Lake hash vectors: %+v", vectors)
	}
	return vectors
}

// TestLakeTextHashMatchesLakeVectors catches a different string hash, seed, mix operation, or hex rendering than
// Lake uses in build traces.
func TestLakeTextHashMatchesLakeVectors(t *testing.T) {
	for _, vector := range loadLakeHashVectors(t).Vectors {
		if got := LakeTextHash([]byte(vector.Text)); got != vector.Hash {
			t.Errorf("LakeTextHash(%q) = %s, want %s", vector.Text, got, vector.Hash)
		}
	}
}

func TestLakeTextHashNormalizesCRLF(t *testing.T) {
	if got, want := LakeTextHash([]byte("one\r\ntwo\r\n")), LakeTextHash([]byte("one\ntwo\n")); got != want {
		t.Fatalf("CRLF hash %s, LF hash %s", got, want)
	}
}

func writeLakeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFreshClassifiesLakeBuildOutputs(t *testing.T) {
	const (
		libRel = "build/lib"
		rel    = "Root/Module"
		source = "one\ntwo\n"
		hash   = "3a02e8fdb34ea3d4"
	)
	root := t.TempDir()
	base := filepath.Join(root, libRel, filepath.FromSlash(rel))
	sourcePath := filepath.Join(root, filepath.FromSlash(rel)+".lean")
	writeLakeFile(t, sourcePath, source)

	notBuilt, err := Fresh(root, libRel, rel, []byte(source))
	if err != nil {
		t.Fatal(err)
	}
	if notBuilt.State != FreshnessNotBuilt {
		t.Fatalf("no outputs = %+v, want not built", notBuilt)
	}

	writeLakeFile(t, base+".ilean", "{}")
	ileanOut := LakeTextHash([]byte("{}")) + ".ilean"
	trace := func(synthetic bool, sourceHash string) string {
		return fmt.Sprintf(`{"schemaVersion":"2025-09-10","inputs":[[%q,%q]],"outputs":{"i":%q},"synthetic":%t}`,
			sourcePath, sourceHash, ileanOut, synthetic)
	}
	writeLakeFile(t, base+".trace", trace(false, hash))
	fresh, err := Fresh(root, libRel, rel, []byte(source))
	if err != nil {
		t.Fatal(err)
	}
	if fresh.State != FreshnessFresh {
		t.Fatalf("matching build trace = %+v, want fresh", fresh)
	}

	stale := func(what string) {
		t.Helper()
		got, err := Fresh(root, libRel, rel, []byte(source))
		if err != nil {
			t.Fatal(err)
		}
		if got.State != FreshnessStale || got.Reason == "" {
			t.Fatalf("%s = %+v, want stale with reason", what, got)
		}
	}
	writeLakeFile(t, base+".trace", trace(false, "0000000000000000"))
	stale("wrong build hash")

	writeLakeFile(t, base+".trace", `{"schemaVersion":"next","inputs":[],"synthetic":false}`)
	stale("unknown schema")

	// The plan's E5 control: the build directory's source rewritten after the build, its trace unchanged.
	writeLakeFile(t, base+".trace", trace(false, hash))
	writeLakeFile(t, sourcePath, "one\ntwo rewritten\n")
	stale("rewritten built source")
	if err := os.Remove(sourcePath); err != nil {
		t.Fatal(err)
	}
	stale("missing built source")
	writeLakeFile(t, sourcePath, source)

	// Another build's .ilean copied over this one: the trace records a different output.
	writeLakeFile(t, base+".ilean", `{"decls":{}}`)
	stale("copied .ilean")
	writeLakeFile(t, base+".ilean", "{}")
	writeLakeFile(t, base+".trace", fmt.Sprintf(`{"schemaVersion":"2025-09-10","inputs":[[%q,%q]],"synthetic":false}`, sourcePath, hash))
	stale("trace without outputs")
}

func TestFreshChecksFetchedSourceAgeAndBytes(t *testing.T) {
	const (
		libRel = "build/lib"
		rel    = "Root/Module"
		source = "one\ntwo\n"
	)
	root := t.TempDir()
	base := filepath.Join(root, libRel, filepath.FromSlash(rel))
	sourcePath := filepath.Join(root, filepath.FromSlash(rel)+".lean")
	writeLakeFile(t, sourcePath, source)
	writeLakeFile(t, base+".ilean", "{}")
	tracePath := base + ".trace"
	fetch := fmt.Sprintf(`{"schemaVersion":"2025-09-10","inputs":[],"outputs":{"i":%q},"synthetic":true}`, LakeTextHash([]byte("{}"))+".ilean")
	writeLakeFile(t, tracePath, fetch)

	fresh, err := Fresh(root, libRel, rel, []byte(source))
	if err != nil {
		t.Fatal(err)
	}
	if fresh.State != FreshnessFresh {
		t.Fatalf("fresh fetch trace = %+v, want fresh", fresh)
	}

	now := time.Now()
	if err := os.Chtimes(tracePath, now, now); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(sourcePath, now.Add(time.Second), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	oldFetch, err := Fresh(root, libRel, rel, []byte(source))
	if err != nil {
		t.Fatal(err)
	}
	if oldFetch.State != FreshnessStale || oldFetch.Reason == "" {
		t.Fatalf("old fetch trace = %+v, want stale with reason", oldFetch)
	}

	writeLakeFile(t, sourcePath, "one\ntwo changed\n")
	differentSource, err := Fresh(root, libRel, rel, []byte(source))
	if err != nil {
		t.Fatal(err)
	}
	if differentSource.State != FreshnessStale || differentSource.Reason == "" {
		t.Fatalf("different fetched source = %+v, want stale with reason", differentSource)
	}

	// A fetch trace binds the outputs: an .ilean that is not the recorded output is stale.
	writeLakeFile(t, sourcePath, source)
	writeLakeFile(t, tracePath, fetch)
	writeLakeFile(t, base+".ilean", `{"decls":{}}`)
	copied, err := Fresh(root, libRel, rel, []byte(source))
	if err != nil {
		t.Fatal(err)
	}
	if copied.State != FreshnessStale || copied.Reason == "" {
		t.Fatalf("fetch trace with another .ilean = %+v, want stale with reason", copied)
	}
}
