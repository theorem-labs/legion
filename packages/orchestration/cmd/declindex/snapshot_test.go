package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/orchestration/declindex"
)

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// A read command reads one snapshot of the index until it closes the index. The command writes its output only after
// it has closed the index, so output that nobody reads (a pager left open) holds no snapshot and does not hold up a
// refresh.
func TestAQueryClosesTheIndexBeforeItWritesItsOutput(t *testing.T) {
	config := fixture(t)
	if r := invoke(t, "--config", config, "refresh"); r.code != 0 {
		t.Fatalf("refresh: %+v", r)
	}
	var once sync.Once
	var refresh result
	out := writerFunc(func(p []byte) (int, error) {
		once.Do(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var o, e bytes.Buffer
			code := run(ctx, withIndexDSN([]string{"--config", config, "refresh"}), func(string) string { return "" }, &o, &e)
			refresh = result{code, o.String(), e.String()}
		})
		return len(p), nil
	})
	var errOut bytes.Buffer
	if code := run(context.Background(), withIndexDSN([]string{"--config", config, "where", "Gold.Frame"}), func(string) string { return "" }, out, &errOut); code != 0 {
		t.Fatalf("where exited %d: %s", code, errOut.String())
	}
	if refresh.code != 0 {
		t.Fatalf("a refresh while the query wrote its output: %+v", refresh)
	}
}

// Between a refresh's write of the reference part and its write of the compiled part, the index's compiled part is
// of the commit before; a compiled step that failed leaves the index so. Every reader of the compiled part refuses
// it rather than join the two commits' rows: the gate exits 3, the ledger's recheck of an addition exits 75, and
// states and same exit 3. A reader of the reference part alone (where) answers from the new commit.
func TestReadersOfTheCompiledPartRefuseAnIndexBetweenItsTwoParts(t *testing.T) {
	requireLake(t)
	ctx := context.Background()
	dir := t.TempDir()
	tree := filepath.Join(dir, "tree")
	registry := filepath.Join(tree, "registry")
	source, err := os.ReadFile(filepath.Join("..", "..", "declindex", "testdata", "lean", "Fixture.lean"))
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(tree, "Fixture.lean"), string(source))
	lean, err := declindex.FindLean(ctx, t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := declindex.CompileModule(ctx, lean, nil, tree, "Fixture"); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(registry, "registry.yaml"), "version: 2\n")
	librarian := filepath.Join(dir, "librarian")
	if err := os.WriteFile(librarian, []byte("#!/bin/sh\nprintf '%s\\n' \"$2\" >> "+strconv.Quote(filepath.Join(dir, "invocations"))+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "index.yaml")
	writeIndexConfig(t, config, "tree: tree\nlib: .\nindex: idx\nstate: state\nroots: [Fixture]\nregistry: registry\nrepo: r\n"+
		"contracts:\n  paths: [NoSuch/**]\n  kinds: [def]\nlibrarian:\n  - "+librarian+"\nlean:\n  export: true\n  jobs: 1\n")
	git(t, tree, "init", "-q")
	git(t, tree, "add", ".")
	git(t, tree, "commit", "-qm", "base")
	base := git(t, tree, "rev-parse", "HEAD")
	if r := invoke(t, "--config", config, "refresh"); r.code != 0 {
		t.Fatalf("refresh base: %+v", r)
	}

	// The second commit adds a concept file, which the ledger's check of its landing rechecks.
	const hash = "00112233445566778899aabbccddeeff"
	writeTestFile(t, filepath.Join(registry, "concepts", hash+".yaml"), "name: Fx.Added\nhome: Fixture.lean\n"+
		"meaning: A concept that the second commit adds.\nproposal: added\nproposer: role\ncompiled_hash: "+hash+"\n"+
		"source: |\n  import Fixture\n\n  def Fx.Added (x : Nat) : Prop := x < 7\n")
	git(t, tree, "add", "registry")
	git(t, tree, "commit", "-qm", "add a concept")
	second := git(t, tree, "rev-parse", "HEAD")
	// The refresh of the second commit writes its reference part and stops before its compiled part.
	cfg := indexConfig(t, config)
	tgt, err := cfg.Target()
	if err != nil {
		t.Fatal(err)
	}
	s, err := declindex.Open(ctx, tgt)
	if err != nil {
		t.Fatal(err)
	}
	stopped := errors.New("the test stops the compiled step")
	_, err = s.Refresh(ctx, cfg.TreeSpec(), declindex.RefreshOptions{Commit: second, Compiled: func(context.Context) (*declindex.CompiledOptions, error) {
		return nil, stopped
	}})
	if closeErr := s.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if !errors.Is(err, stopped) {
		t.Fatalf("the refresh of the second commit: %v", err)
	}

	proposal := filepath.Join(dir, "proposal.json")
	writeTestFile(t, proposal, `{"id":"p","concept":{"name":"Fx.Fresh","home":"Fixture.lean","meaning":"A proposed concept."},`+
		`"source":"import Fixture\n\ndef Fx.Fresh (x : Nat) : Prop := x < 9\n"}`)
	contracts, emit := filepath.Join(dir, "contracts.json"), filepath.Join(dir, "operations.jsonl")
	writeTestFile(t, contracts, "[]\n")
	refusal := "compiled part is of " + base + ", not of its commit " + second
	for _, c := range []struct {
		args []string
		code int
	}{
		{[]string{"gate", "--proposer", "role", proposal}, 3},
		{[]string{"ledger", "--contracts-file", contracts, "--emit", emit}, 75},
		{[]string{"states", "Fx.Added"}, 3},
		{[]string{"same", "Fx.Frame"}, 3},
	} {
		if r := invoke(t, append([]string{"--config", config}, c.args...)...); r.code != c.code || !strings.Contains(r.errOut, refusal) {
			t.Errorf("%s between the two parts: exit %d, want %d naming %q: %s", c.args[0], r.code, c.code, refusal, r.errOut)
		}
	}
	if r := invoke(t, "--config", config, "where", "Fx.Frame"); r.code != 0 {
		t.Errorf("where between the two parts: %+v", r)
	}

	// Once a refresh has read the compiled part of the second commit, those readers answer again.
	if r := invoke(t, "--config", config, "refresh"); r.code != 0 {
		t.Fatalf("refresh the second commit: %+v", r)
	}
	for _, args := range [][]string{{"gate", "--proposer", "role", proposal}, {"ledger", "--contracts-file", contracts, "--emit", emit}, {"states", "Fx.Added"}, {"same", "Fx.Frame"}} {
		if r := invoke(t, append([]string{"--config", config}, args...)...); r.code == 3 || r.code == 75 || strings.Contains(r.errOut, "compiled part is of") {
			t.Errorf("%s after the compiled part of the second commit: %+v", args[0], r)
		}
	}
}
