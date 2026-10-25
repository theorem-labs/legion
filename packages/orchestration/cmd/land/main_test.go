package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/orchestration/internal/testpg"
	"github.com/sjawhar/legion/orchestration/ledger"
)

func TestBlameCommandPrintsEvidenceBackedMember(t *testing.T) {
	repo := t.TempDir()
	command(t, repo, "init", "-q")
	command(t, repo, "config", "user.email", "test@example.invalid")
	command(t, repo, "config", "user.name", "Test")
	write(t, filepath.Join(repo, "Alpha.lean"), "def gone := 0\n")
	command(t, repo, "add", "Alpha.lean")
	command(t, repo, "commit", "-qm", "base")
	base := command(t, repo, "rev-parse", "HEAD")
	write(t, filepath.Join(repo, "Alpha.lean"), "def replacement := 0\n")
	command(t, repo, "add", "Alpha.lean")
	command(t, repo, "commit", "-qm", "member")
	head := command(t, repo, "rev-parse", "HEAD")

	members := filepath.Join(t.TempDir(), "members.tsv")
	log := filepath.Join(t.TempDir(), "build.log")
	write(t, members, "one\t"+head+"\n")
	write(t, log, "✖ [1/1] Building Alpha\nerror: Alpha.lean:1:1: Unknown identifier `gone`\n")
	var output bytes.Buffer

	if err := run(context.Background(), []string{"blame", "--repo", repo, "--base", base, "--members", members, "--log", log}, &output); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if got := output.String(); !strings.Contains(got, "one\tAlpha\tgone\tunknown\t3\tremoved") {
		t.Fatalf("blame output = %q, want structured evidence for one", got)
	}
}

func TestBlameCommandExcludesListedMembers(t *testing.T) {
	repo := t.TempDir()
	command(t, repo, "init", "-q")
	command(t, repo, "config", "user.email", "test@example.invalid")
	command(t, repo, "config", "user.name", "Test")
	write(t, filepath.Join(repo, "Alpha.lean"), "def gone := 0\n")
	command(t, repo, "add", "Alpha.lean")
	command(t, repo, "commit", "-qm", "base")
	base := command(t, repo, "rev-parse", "HEAD")
	write(t, filepath.Join(repo, "Alpha.lean"), "def replacement := 0\n")
	command(t, repo, "add", "Alpha.lean")
	command(t, repo, "commit", "-qm", "member")
	head := command(t, repo, "rev-parse", "HEAD")

	members := filepath.Join(t.TempDir(), "members.tsv")
	excluded := filepath.Join(t.TempDir(), "excluded.txt")
	log := filepath.Join(t.TempDir(), "build.log")
	write(t, members, "one\t"+head+"\n")
	write(t, excluded, "one\n")
	write(t, log, "✖ [1/1] Building Alpha\nerror: Alpha.lean:1:1: Unknown identifier `gone`\n")
	var output bytes.Buffer

	if err := run(context.Background(), []string{"blame", "--repo", repo, "--base", base, "--members", members, "--exclude", excluded, "--log", log}, &output); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if got := output.String(); got != "" {
		t.Fatalf("blame output = %q, want excluded member omitted", got)
	}
}

// An old member restates the base's own `def Shared := 0`, and another adds
// a second Shared in another file. `land blame` compares each member's
// additions with the base and candidate trees: the restatement is not
// evidence, and the second Shared changed the candidate.
func TestBlameCommandSuppressesRestatementOfTheBase(t *testing.T) {
	repo := t.TempDir()
	command(t, repo, "init", "-q")
	command(t, repo, "config", "user.email", "test@example.invalid")
	command(t, repo, "config", "user.name", "Test")
	write(t, filepath.Join(repo, "Root.lean"), "def root := 0\n")
	command(t, repo, "add", "Root.lean")
	command(t, repo, "commit", "-qm", "root")
	root := command(t, repo, "rev-parse", "HEAD")
	write(t, filepath.Join(repo, "Alpha.lean"), "def Shared := 0\n")
	command(t, repo, "add", "Alpha.lean")
	command(t, repo, "commit", "-qm", "base")
	base := command(t, repo, "rev-parse", "HEAD")
	command(t, repo, "checkout", "-q", "-b", "stale", root)
	write(t, filepath.Join(repo, "Alpha.lean"), "def Shared := 0\n")
	command(t, repo, "add", "Alpha.lean")
	command(t, repo, "commit", "-qm", "stale")
	stale := command(t, repo, "rev-parse", "HEAD")
	command(t, repo, "checkout", "-q", "-b", "duplicate", base)
	write(t, filepath.Join(repo, "Beta.lean"), "def Shared := 1\n")
	command(t, repo, "add", "Beta.lean")
	command(t, repo, "commit", "-qm", "duplicate")
	duplicate := command(t, repo, "rev-parse", "HEAD")

	members := filepath.Join(t.TempDir(), "members.tsv")
	log := filepath.Join(t.TempDir(), "build.log")
	write(t, members, "stale\t"+stale+"\nduplicate\t"+duplicate+"\n")
	write(t, log, "✖ [1/1] Building Alpha\nerror: Alpha.lean:1:1: environment already contains 'Shared'\n")
	var output bytes.Buffer

	if err := run(context.Background(), []string{"blame", "--repo", repo, "--base", base, "--members", members, "--log", log}, &output); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if got, want := output.String(), "duplicate\tAlpha\tShared\tadded\t3\tchanged in candidate\n"; got != want {
		t.Fatalf("blame output = %q, want %q", got, want)
	}
}

func TestRunCommandBuildsConfiguredBatch(t *testing.T) {
	repo := t.TempDir()
	command(t, repo, "init", "-q")
	command(t, repo, "config", "user.email", "test@example.invalid")
	command(t, repo, "config", "user.name", "Test")
	write(t, filepath.Join(repo, "Alpha.lean"), "def base := 0\n")
	command(t, repo, "add", "Alpha.lean")
	command(t, repo, "commit", "-qm", "base")
	base := command(t, repo, "rev-parse", "HEAD")
	write(t, filepath.Join(repo, "Alpha.lean"), "def member := 0\n")
	command(t, repo, "add", "Alpha.lean")
	command(t, repo, "commit", "-qm", "member")
	head := command(t, repo, "rev-parse", "HEAD")

	config := filepath.Join(t.TempDir(), "config.json")
	raw, err := json.Marshal(withContainer(map[string]any{
		"batchId":     "batch",
		"repository":  map[string]any{"name": "owner/repo", "path": repo},
		"base":        base,
		"baseBranch":  "trunk",
		"members":     []map[string]any{{"migration": "one", "generation": 1, "head": head}},
		"build":       []string{"sh", "-c", "exit 0"},
		"hasher":      []string{"sh", "-c", "exit 0"},
		"toolchain":   []string{"sh", "-c", "printf tool"},
		"escalations": map[string]any{"owner": "steward"},
		"ledger":      map[string]any{"dsn_file": ledgerDSNFile(t, "owner/repo", "one", head)},
	}, containerSection(t)))
	if err != nil {
		t.Fatal(err)
	}
	write(t, config, string(raw))
	var output bytes.Buffer

	if err := run(context.Background(), []string{"run", "--config", config}, &output); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if got := output.String(); !strings.Contains(got, `"state":"green"`) || strings.Contains(got, `"configurationHash":""`) || strings.Contains(got, `"toolchain":""`) {
		t.Fatalf("run output = %q, want bound green receipt", got)
	}
	output.Reset()
	if err := run(context.Background(), []string{"status", "--config", config}, &output); err != nil {
		t.Fatalf("status error = %v", err)
	}
	if got := output.String(); !strings.Contains(got, `"id":"batch"`) {
		t.Fatalf("status output = %q, want stored batch", got)
	}
}

// ledgerDSNFile is a file naming a fresh ledger database with repository repo and migration id open for head and
// readied, as its owner leaves it before a batch picks it up.
func ledgerDSNFile(t *testing.T, repo, id, head string) string {
	t.Helper()
	ctx := context.Background()
	dsn := testpg.Database(t, "land_run")
	store, err := ledger.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.EnsureRepo(ctx, repo, 3); err != nil {
		t.Fatal(err)
	}
	if _, err := store.OpenMigration(ctx, ledger.OpenRequest{Repo: repo, ID: id, Owner: "alice", BaseCommit: "base", Branch: "branch/" + id, Writes: []string{"Alpha.lean"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Ready(ctx, ledger.ReadyRequest{Repo: repo, ID: id, Generation: 1, Head: head, Key: "ready-" + id}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "ledger.dsn")
	write(t, path, dsn+"\n")
	return path
}

// The escalations owner is required: an escalation enqueued for an empty owner
// wakes nobody.
func TestConfigRequiresEscalateTo(t *testing.T) {
	cfg := publicationConfig(t)
	cfg.Escalations.Owner = ""
	if _, err := cfg.program(context.Background()); err == nil || !strings.Contains(err.Error(), "escalations.owner is required") {
		t.Fatalf("program() error = %v, want escalations.owner required", err)
	}
}

// The ledger commands are removed: the program calls the ledger directly, so a
// configuration that still names one is refused with the key to set instead.
func TestConfigRefusesRemovedLedgerCommands(t *testing.T) {
	for _, key := range []string{"check", "land", "return", "command", "status", "migrationState"} {
		path := filepath.Join(t.TempDir(), "config.json")
		write(t, path, `{"ledger": {"`+key+`": ["true"]}}`)
		if _, err := readConfig(path); err == nil || !strings.Contains(err.Error(), "ledger."+key+" is removed; set ledger.dsn_file") {
			t.Fatalf("readConfig() with ledger.%s error = %v, want it refused", key, err)
		}
	}
}

func write(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func command(t *testing.T, repo string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}
