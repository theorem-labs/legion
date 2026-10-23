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
	trace := func(synthetic bool, sourceHash string) string {
		return fmt.Sprintf(`{"schemaVersion":"2025-09-10","inputs":[[%q,%q]],"synthetic":%t}`,
			sourcePath, sourceHash, synthetic)
	}
	writeLakeFile(t, base+".trace", trace(false, hash))
	fresh, err := Fresh(root, libRel, rel, []byte(source))
	if err != nil {
		t.Fatal(err)
	}
	if fresh.State != FreshnessFresh {
		t.Fatalf("matching build trace = %+v, want fresh", fresh)
	}

	writeLakeFile(t, base+".trace", trace(false, "0000000000000000"))
	wrongHash, err := Fresh(root, libRel, rel, []byte(source))
	if err != nil {
		t.Fatal(err)
	}
	if wrongHash.State != FreshnessStale || wrongHash.Reason == "" {
		t.Fatalf("wrong build hash = %+v, want stale with reason", wrongHash)
	}

	writeLakeFile(t, base+".trace", `{"schemaVersion":"next","inputs":[],"synthetic":false}`)
	unknownSchema, err := Fresh(root, libRel, rel, []byte(source))
	if err != nil {
		t.Fatal(err)
	}
	if unknownSchema.State != FreshnessStale || unknownSchema.Reason == "" {
		t.Fatalf("unknown schema = %+v, want stale with reason", unknownSchema)
	}
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
	writeLakeFile(t, tracePath, `{"schemaVersion":"2025-09-10","inputs":[],"synthetic":true}`)

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
}
