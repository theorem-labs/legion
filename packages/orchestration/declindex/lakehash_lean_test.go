package declindex

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

var lakeHashInputs = []string{"", "a", "abcdefgh", "one\ntwo\n", "one\r\ntwo\r\n", "λ and 😀"}

// TestLakeHashFixtureMatchesLean pins the Lake implementation that produced the committed vectors. Set
// DECLINDEX_WRITE_LAKE_VECTORS=1 to regenerate the fixture after deliberately upgrading that implementation.
func TestLakeHashFixtureMatchesLean(t *testing.T) {
	if _, err := exec.LookPath("lean"); err != nil {
		if os.Getenv("DECLINDEX_REQUIRE_LEAN") == "1" {
			t.Fatalf("lean is required: %v", err)
		}
		t.Skip("lean is not on PATH")
	}
	version, err := exec.Command("lean", "--version").Output()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	program := filepath.Join(dir, "HashVectors.lean")
	const source = `import Lake

open Lake

def main : IO Unit := do
  IO.println (Hash.ofText "").hex
  IO.println (Hash.ofText "a").hex
  IO.println (Hash.ofText "abcdefgh").hex
  IO.println (Hash.ofText "one\ntwo\n").hex
  IO.println (Hash.ofText "one\r\ntwo\r\n").hex
  IO.println (Hash.ofText "λ and 😀").hex
`
	if err := os.WriteFile(program, []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("lean", "--run", program).Output()
	if err != nil {
		t.Fatal(err)
	}
	hashes := strings.Fields(string(out))
	if len(hashes) != len(lakeHashInputs) {
		t.Fatalf("lean emitted %d hashes, want %d: %q", len(hashes), len(lakeHashInputs), out)
	}
	generated := lakeHashVectors{Toolchain: strings.TrimSpace(string(version))}
	for i, text := range lakeHashInputs {
		generated.Vectors = append(generated.Vectors, struct {
			Text string `json:"text"`
			Hash string `json:"hash"`
		}{Text: text, Hash: hashes[i]})
	}
	path := filepath.Join("testdata", "lakehash", "vectors.json")
	if os.Getenv("DECLINDEX_WRITE_LAKE_VECTORS") == "1" {
		raw, err := json.MarshalIndent(generated, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(raw, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	got := loadLakeHashVectors(t)
	if !reflect.DeepEqual(got.Vectors, generated.Vectors) {
		t.Fatalf("Lake vectors from %s = %+v, want hashes from %s: %+v", got.Toolchain, got.Vectors, generated.Toolchain, generated.Vectors)
	}
}
