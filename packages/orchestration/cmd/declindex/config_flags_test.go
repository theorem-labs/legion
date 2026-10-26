package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/orchestration/declindex"
)

// flagsConfig writes a configuration and the tree it names, and gives the configuration's path.
func flagsConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "tree"), 0o755); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "index.yaml")
	if err := os.WriteFile(config, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return config
}

func noEnvironment(string) string { return "" }

// flagsRun runs the command line and gives its exit code and what it wrote to stderr.
func flagsRun(t *testing.T, getenv func(string) string, args ...string) (int, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := run(context.Background(), args, getenv, &out, &errOut)
	return code, errOut.String()
}

func isUsageError(err error) bool {
	var u *usageError
	return errors.As(err, &u)
}

// The index's connection string is never in a configuration file: --index-dsn-file gives it, and it wins over
// DECLINDEX_DSN, so that the landing driver can hand one command the write login and the next the read-only one
// (P-1).
func TestTheIndexDSNFileWinsOverTheEnvironment(t *testing.T) {
	config := flagsConfig(t, "tree: tree\nindex: idx\nstate: state\nroots: [A]\n")
	file := filepath.Join(filepath.Dir(config), "index.dsn")
	if err := os.WriteFile(file, []byte(" postgres://file.invalid/declindex \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	environment := func(key string) string {
		if key == "DECLINDEX_DSN" {
			return "postgres://environment.invalid/declindex"
		}
		return ""
	}
	cfg, err := settings(globals{config: config, dsnFile: file}, environment)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DSN != "postgres://file.invalid/declindex" {
		t.Errorf("--index-dsn-file gave %q, want the file's trimmed contents", cfg.DSN)
	}
	if cfg, err = settings(globals{config: config}, environment); err != nil {
		t.Fatal(err)
	}
	if cfg.DSN != "postgres://environment.invalid/declindex" {
		t.Errorf("without the flag the connection string is %q, want DECLINDEX_DSN", cfg.DSN)
	}
	if cfg, err = settings(globals{config: config}, noEnvironment); err != nil {
		t.Fatal(err)
	}
	if cfg.DSN != "" {
		t.Errorf("with neither the flag nor the variable the connection string is %q", cfg.DSN)
	}
	empty := filepath.Join(filepath.Dir(config), "empty.dsn")
	if err := os.WriteFile(empty, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := settings(globals{config: config, dsnFile: empty}, environment); !isUsageError(err) {
		t.Errorf("an empty --index-dsn-file gave %v, want a usage error", err)
	}
	if _, err := settings(globals{config: config, dsnFile: filepath.Join(filepath.Dir(config), "none")}, environment); !isUsageError(err) {
		t.Errorf("an unreadable --index-dsn-file gave %v, want a usage error", err)
	}
}

// --index and --state override their keys, so that one configuration in a tree serves the landing driver's main
// and test instances (P-1, P-2, P-8).
func TestTheIndexAndStateFlagsOverrideTheConfigurationKeys(t *testing.T) {
	config := flagsConfig(t, "tree: tree\nindex: idx\nstate: state\nreader: declindex_ro\nroots: [A]\n")
	cfg, err := settings(globals{config: config}, noEnvironment)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Index != "idx" || cfg.State != filepath.Join(filepath.Dir(config), "state") {
		t.Fatalf("the keys gave index %q and state %q", cfg.Index, cfg.State)
	}
	cwd := t.TempDir()
	t.Chdir(cwd)
	if cfg, err = settings(globals{config: config, index: "other_idx", state: "elsewhere"}, noEnvironment); err != nil {
		t.Fatal(err)
	}
	if cfg.Index != "other_idx" {
		t.Errorf("--index gave %q", cfg.Index)
	}
	if cfg.State != filepath.Join(cwd, "elsewhere") {
		t.Errorf("--state gave %q, want it resolved against the working directory", cfg.State)
	}
	// --state also moves the file whose lock guards the tree, which lives in the state directory.
	got, err := cfg.TreeTarget()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(got.TreeLock, string(filepath.Separator)+filepath.Join("elsewhere", "tree.lock")) {
		t.Errorf("Target().TreeLock = %q, want tree.lock in the --state directory", got.TreeLock)
	}
	got.TreeLock = ""
	if want := (declindex.Target{Index: "other_idx", Reader: "declindex_ro"}); got != want {
		t.Errorf("Target() = %+v, want %+v", got, want)
	}
	if _, err := settings(globals{config: config, index: "Idx"}, noEnvironment); !isUsageError(err) {
		t.Errorf("--index Idx gave %v, want a usage error", err)
	}
}

// db: and --db named the SQLite file. Both are refused, with the keys and the flag that replaced them (P-1).
func TestTheRemovedDBFlagAndKeyAreRefused(t *testing.T) {
	current := flagsConfig(t, "tree: tree\nindex: idx\nstate: state\nroots: [A]\n")
	old := flagsConfig(t, "tree: tree\ndb: state/index.sqlite\nroots: [A]\n")
	for _, args := range [][]string{
		{"--config", current, "--db", "state/index.sqlite", "status"},
		{"--config", old, "status"},
	} {
		code, errOut := flagsRun(t, noEnvironment, args...)
		if code != 2 {
			t.Errorf("%v exited %d, want 2: %s", args, code, errOut)
		}
		for _, want := range []string{"db: was removed", "the index is a Postgres schema", "set index:", "DECLINDEX_DSN", "--index-dsn-file"} {
			if !strings.Contains(errOut, want) {
				t.Errorf("%v: the refusal does not name %q: %s", args, want, errOut)
			}
		}
	}
}

// A command that opens the index needs the schema's name and a connection string; without either it is a usage
// error, as a missing db: was (P-1).
func TestACommandThatOpensTheIndexWithoutAConnectionStringExitsTwo(t *testing.T) {
	withIndex := flagsConfig(t, "tree: tree\nindex: idx\nstate: state\nroots: [A]\n")
	withoutIndex := flagsConfig(t, "tree: tree\nstate: state\nroots: [A]\n")
	dsn := func(string) string { return "postgres://example.invalid:1/declindex" }
	for _, test := range []struct {
		name    string
		config  string
		getenv  func(string) string
		command []string
		want    string
	}{
		{"status without a connection string", withIndex, noEnvironment, []string{"status"}, "DECLINDEX_DSN"},
		{"refresh without a connection string", withIndex, noEnvironment, []string{"refresh"}, "--index-dsn-file"},
		{"a query without a connection string", withIndex, noEnvironment, []string{"hash", "Gold.Frame"}, "no connection string for the index"},
		{"a connection string without an index name", withoutIndex, dsn, []string{"status"}, "index:"},
	} {
		t.Run(test.name, func(t *testing.T) {
			code, errOut := flagsRun(t, test.getenv, append([]string{"--config", test.config}, test.command...)...)
			if code != 2 {
				t.Fatalf("exited %d, want 2: %s", code, errOut)
			}
			if !strings.Contains(errOut, test.want) {
				t.Errorf("the usage message does not name %q: %s", test.want, errOut)
			}
		})
	}
}
