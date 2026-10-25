package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/sjawhar/legion/orchestration/declindex"
)

// Two compiled definitions whose string literals differ only in white space have different values, so `hash`
// gives them different statement hashes; a change to the white space between their tokens changes nothing.
func TestHashChangesWithTheWhiteSpaceOfAStringLiteral(t *testing.T) {
	requireLake(t)
	ctx := context.Background()
	lean, err := declindex.FindLean(ctx, t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	tree := filepath.Join(dir, "tree")
	config := filepath.Join(dir, "index.yaml")
	writeTestFile(t, config, "tree: tree\nlib: .\ndb: state/index.sqlite\nroots: [Fixture]\n")
	hashOf := func(source string) string {
		t.Helper()
		writeTestFile(t, filepath.Join(tree, "Fixture.lean"), source)
		if err := declindex.CompileModule(ctx, lean, nil, tree, "Fixture"); err != nil {
			t.Fatal(err)
		}
		if r := invoke(t, "--config", config, "refresh"); r.code != 0 {
			t.Fatalf("refresh: %+v", r)
		}
		r := invoke(t, "--config", config, "--json", "hash", "message")
		var hashed []struct {
			Hash string `json:"hash"`
		}
		if err := json.Unmarshal([]byte(r.out), &hashed); r.code != 0 || err != nil || len(hashed) != 1 || hashed[0].Hash == "" {
			t.Fatalf("hash message: %+v %v", r, err)
		}
		return hashed[0].Hash
	}
	one := hashOf("def message : String := \"a b\"\n")
	if two := hashOf("def message : String := \"a  b\"\n"); two == one {
		t.Errorf("\"a b\" and \"a  b\" have one statement hash, %s", one)
	}
	if spaced := hashOf("def message :  String :=\n    \"a b\"\n"); spaced != one {
		t.Errorf("white space between tokens changed the statement hash: %s, want %s", spaced, one)
	}
}
