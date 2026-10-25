package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/legion/orchestration/declindex"
	"github.com/sjawhar/legion/orchestration/internal/indextest"
)

// fixture copies the golden tree of the declindex package and writes a configuration for it.
func fixture(t *testing.T) (config string) {
	t.Helper()
	dir := t.TempDir()
	tree := filepath.Join(dir, "tree")
	src := filepath.Join("..", "..", "declindex", "testdata", "golden")
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(tree, rel), 0o755)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(tree, rel), data, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	config = filepath.Join(dir, "index.yaml")
	writeIndexConfig(t, config, "tree: tree\nlib: .\nindex: idx\nstate: state\nroots: [Golden, Sub]\nfile_kinds: [Spec, Link, Proof]\n")
	return config
}

// indexDSNFile is where a fixture keeps the connection string of its index. The index itself is a schema of a
// Postgres server, named by `index:`; nothing in the tree says how to reach that server, so every command gets
// the connection string with --index-dsn-file, as an operator's hook does.
func indexDSNFile(config string) string { return filepath.Join(filepath.Dir(config), "index.dsn") }

// writeIndexConfig writes a fixture's configuration and, beside it, the connection string of a database of its
// own. It writes the connection string once, so that a test that rewrites its configuration keeps its index.
func writeIndexConfig(t *testing.T, config, body string) {
	t.Helper()
	writeTestFile(t, config, body)
	if _, err := os.Stat(indexDSNFile(config)); err == nil {
		return
	}
	writeTestFile(t, indexDSNFile(config), indextest.DSN(t))
}

// withIndexDSN gives the command the connection string of the index that its --config names, when the fixture
// wrote one. The flag comes first, where the global flags are.
func withIndexDSN(args []string) []string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] != "--config" {
			continue
		}
		if _, err := os.Stat(indexDSNFile(args[i+1])); err == nil {
			return append([]string{"--index-dsn-file", indexDSNFile(args[i+1])}, args...)
		}
		break
	}
	return args
}

// indexConfig loads a fixture's configuration with the connection string that invoke passes with it, so that a
// test opens the index that the commands open: declindex.Open(ctx, cfg.Target()).
func indexConfig(t *testing.T, config string) declindex.Config {
	t.Helper()
	cfg, err := declindex.LoadConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(indexDSNFile(config))
	if err != nil {
		t.Fatal(err)
	}
	cfg.DSN = strings.TrimSpace(string(raw))
	return cfg
}

// indexConn connects to a fixture's index schema, for the rows that a test writes itself: rows that no refresh
// would write, to put the index in a state that the command must report.
func indexConn(t *testing.T, cfg declindex.Config) *pgx.Conn {
	t.Helper()
	c, err := pgx.ParseConfig(cfg.DSN)
	if err != nil {
		t.Fatal(err)
	}
	c.RuntimeParams["search_path"] = pgx.Identifier{cfg.Index}.Sanitize()
	conn, err := pgx.ConnectConfig(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

type result struct {
	code        int
	out, errOut string
}

func invoke(t *testing.T, args ...string) result {
	t.Helper()
	var out, errOut bytes.Buffer
	code := run(context.Background(), withIndexDSN(args), func(string) string { return "" }, &out, &errOut)
	return result{code, out.String(), errOut.String()}
}

func TestHelpStartsWithOneLineUsage(t *testing.T) {
	r := invoke(t, "--help")
	if r.code != 0 || !strings.HasPrefix(r.out, "usage: declindex ") {
		t.Fatalf("help: %+v", r)
	}
}

func TestHelpListsFinalCommandInterfacesAndExitCodes(t *testing.T) {
	r := invoke(t, "--help")
	for _, want := range []string{
		"--index-dsn-file",
		"refresh: 0 read; 2 usage; 3 unable; 75 retry",
		"gate --proposer OWNER [--accepted-by OWNER] PROPOSAL.json",
		"ledger: 0 clean; 1 defect; 2 usage; 3 unable; 75 retry",
		"LAND_NAMES or LAND_NAMES_FILE",
		"LAND_BUILD_DIR for changed sources",
		"name<TAB>hash<TAB>path",
	} {
		if !strings.Contains(r.out, want) {
			t.Errorf("help does not list %q:\n%s", want, r.out)
		}
	}
}

// land-hash exits 3 for a changed source without LAND_BUILD_DIR, so its usage message names it with the rest of
// its environment.
func TestLandHashUsageNamesItsWholeEnvironment(t *testing.T) {
	r := invoke(t, "--config", fixture(t), "land-hash", "unexpected")
	if r.code != 2 {
		t.Fatalf("land-hash with an argument exited %d, want 2: %+v", r.code, r)
	}
	for _, want := range []string{"LAND_TREE", "LAND_GIT_DIR", "LAND_NAMES_FILE", "LAND_BUILD_DIR for changed sources"} {
		if !strings.Contains(r.errOut, want) {
			t.Errorf("the usage message does not name %q: %s", want, r.errOut)
		}
	}
}

func TestUsageErrorsExitTwo(t *testing.T) {
	config := fixture(t)
	for _, args := range [][]string{
		{},
		{"--config", config},
		{"--config", config, "nonsense"},
		{"--config", config, "users"},
		{"--config", config, "users", "--limit"},
		{"users", "X"},
	} {
		if r := invoke(t, args...); r.code != 2 {
			t.Errorf("%v exited %d, want 2: %s", args, r.code, r.errOut)
		}
	}
}

func TestQueriesBeforeAnyRefreshAreUnableToCheck(t *testing.T) {
	config := fixture(t)
	if r := invoke(t, "--config", config, "users", "Gold.Frame"); r.code != 3 {
		t.Fatalf("a query of an empty index exited %d, want 3: %+v", r.code, r)
	}
}

// Two indexes of one Postgres database are two schemas, named by `index:`, and neither sees the other's rows: a
// refresh of one leaves the other without an index to read, and `--index` picks the schema whatever the
// configuration says. The connection string comes from the environment here, as a hook sets it (P-1, P-2).
func TestTwoIndexesOfOneDatabaseStayApart(t *testing.T) {
	config := fixture(t)
	dsn, err := os.ReadFile(indexDSNFile(config))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DECLINDEX_DSN", strings.TrimSpace(string(dsn)))
	// Not invoke: this test gives the connection string in the environment, so no --index-dsn-file is passed.
	command := func(args ...string) result {
		t.Helper()
		var out, errOut bytes.Buffer
		code := run(context.Background(), append([]string{"--config", config}, args...),
			func(name string) string { return os.Getenv(name) }, &out, &errOut)
		return result{code, out.String(), errOut.String()}
	}
	if r := command("refresh"); r.code != 0 {
		t.Fatalf("refresh of the index idx: %+v", r)
	}
	if r := command("--index", "other", "status"); r.code != 3 || !strings.Contains(r.errOut, `the index "other" was never refreshed`) {
		t.Fatalf("status of the index other, which no refresh made: want exit 3 and its name, got %+v", r)
	}
	if r := command("--index", "other", "refresh"); r.code != 0 {
		t.Fatalf("refresh of the index other: %+v", r)
	}
	// Each index answers from its own rows, and dropping one leaves the other whole.
	for _, index := range []string{"idx", "other"} {
		if r := command("--index", index, "users", "Frame"); r.code != 0 || !strings.Contains(r.out, "Gold.Frame") {
			t.Errorf("users in the index %s: %+v", index, r)
		}
	}
	conn := indexConn(t, indexConfig(t, config))
	if _, err := conn.Exec(context.Background(), `DROP SCHEMA other CASCADE`); err != nil {
		t.Fatal(err)
	}
	if r := command("users", "Frame"); r.code != 0 || !strings.Contains(r.out, "Gold.Frame") {
		t.Errorf("users in the index idx after the index other was dropped: %+v", r)
	}
}

func TestRefreshThenAnswerAsTheReferenceTool(t *testing.T) {
	config := fixture(t)
	if r := invoke(t, "--config", config, "refresh"); r.code != 0 {
		t.Fatalf("refresh: %+v", r)
	}
	r := invoke(t, "--config", config, "--json", "users", "--lines", "Frame")
	if r.code != 0 {
		t.Fatalf("users: %+v", r)
	}
	var got struct {
		Answers []struct {
			Declaration string `json:"declaration"`
			Direct      struct {
				Declarations int                       `json:"declarations"`
				ByDirectory  map[string]map[string]int `json:"by_directory"`
			} `json:"direct"`
			Reach struct {
				StatementsChanged int `json:"statements_changed"`
				ProofsToRedo      int `json:"proofs_to_redo"`
			} `json:"reach"`
			Lines []string `json:"lines"`
		} `json:"answers"`
	}
	if err := json.Unmarshal([]byte(r.out), &got); err != nil {
		t.Fatalf("%v: %s", err, r.out)
	}
	if len(got.Answers) != 1 {
		t.Fatalf("answers: %+v", got)
	}
	a := got.Answers[0]
	if a.Declaration != "Gold.Frame" || a.Direct.Declarations != 6 || a.Reach.StatementsChanged != 5 || a.Reach.ProofsToRedo != 1 ||
		len(a.Lines) != 9 || a.Direct.ByDirectory["Sub"]["other proof"] != 5 {
		t.Fatalf("answer: %+v", a)
	}
	text := invoke(t, "--config", config, "users", "Frame")
	if text.code != 0 || !strings.Contains(text.out, "direct users: 6 declarations in 2 directories") {
		t.Fatalf("text answer: %+v", text)
	}
}

func TestHashPrintsNameTabHash(t *testing.T) {
	config := fixture(t)
	invoke(t, "--config", config, "refresh")
	r := invoke(t, "--config", config, "hash", "Gold.frame_lo", "Aligned")
	if r.code != 0 {
		t.Fatalf("hash: %+v", r)
	}
	want := "Gold.frame_lo\t9d9fba31f41fc258\tGolden.lean:17\nGold.Aligned\tcad50378567521cb\tGolden.lean:7\n"
	if r.out != want {
		t.Fatalf("hash output %q, want %q", r.out, want)
	}
}

func TestUnknownNameExitsOne(t *testing.T) {
	config := fixture(t)
	invoke(t, "--config", config, "refresh")
	r := invoke(t, "--config", config, "users", "NoSuch")
	if r.code != 1 || !strings.Contains(r.errOut, "NoSuch") {
		t.Fatalf("unknown name: %+v", r)
	}
}

func TestWhereAndStatus(t *testing.T) {
	config := fixture(t)
	invoke(t, "--config", config, "refresh")
	r := invoke(t, "--config", config, "where", "FrameAt")
	if r.code != 0 || !strings.Contains(r.out, "Gold2.FrameAt\tdef\tSub.Wrap\tSub/Wrap.lean:7") {
		t.Fatalf("where: %+v", r)
	}
	s := invoke(t, "--config", config, "--json", "status")
	var st struct {
		Modules int `json:"modules"`
	}
	if s.code != 0 || json.Unmarshal([]byte(s.out), &st) != nil || st.Modules != 2 {
		t.Fatalf("status: %+v", s)
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}
