package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/orchestration/internal/testpg"
	"github.com/sjawhar/legion/orchestration/ledger"
)

// servingLedger opens a ledger on a fresh database with the repository configured, and returns the store and the
// 0600 file that names its database.
func servingLedger(t *testing.T, repository string) (*ledger.Store, string) {
	t.Helper()
	ctx := context.Background()
	dsn := testpg.Database(t, "land_serve")
	store, err := ledger.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if _, err := store.EnsureRepo(ctx, repository, 6); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "ledger.dsn")
	write(t, path, dsn+"\n")
	return store, path
}

// readyRepository is a repository on branch trunk, with one branch per migration that adds a file of its own, so
// no two of them write the same path.
func readyRepository(t *testing.T, migrations ...string) (repo, base string, heads map[string]string) {
	t.Helper()
	repo = t.TempDir()
	command(t, repo, "init", "-q", "-b", "trunk")
	command(t, repo, "config", "user.email", "test@example.invalid")
	command(t, repo, "config", "user.name", "Test")
	write(t, filepath.Join(repo, "Base.lean"), "def base := 0\n")
	command(t, repo, "add", ".")
	command(t, repo, "commit", "-qm", "base")
	base = command(t, repo, "rev-parse", "HEAD")
	heads = make(map[string]string, len(migrations))
	for _, id := range migrations {
		command(t, repo, "checkout", "-q", "-b", id, base)
		write(t, filepath.Join(repo, id+".lean"), "def "+id+" := 1\n")
		command(t, repo, "add", ".")
		command(t, repo, "commit", "-qm", id)
		heads[id] = command(t, repo, "rev-parse", "HEAD")
	}
	command(t, repo, "checkout", "-q", "trunk")
	return repo, base, heads
}

// servingConfig is a configuration for `land serve`: no batchId, no base and no members, because a serving program
// forms each batch from the ledger's ready migrations.
func servingConfig(t *testing.T, repository, repo, dsnFile string, interval time.Duration) string {
	t.Helper()
	return writeConfig(t, withContainer(map[string]any{
		"repository":  map[string]any{"name": repository, "path": repo},
		"baseBranch":  "trunk",
		"build":       []string{"sh", "-c", "exit 0"},
		"hasher":      []string{"sh", "-c", "exit 0"},
		"toolchain":   []string{"sh", "-c", "printf tool"},
		"ledger":      map[string]any{"dsn_file": dsnFile},
		"escalations": map[string]any{"owner": "landing-operator"},
		"batch":       map[string]any{"max_members": 2},
		"interval":    int64(interval),
	}, containerSection(t)))
}

// land serve starts without a configured batch and forms its first one from the ledger's ready migrations, in
// ready order and capped at batch.max_members, on the current tip of the base branch.
func TestServeFormsItsFirstBatchFromTheReadyMigrations(t *testing.T) {
	ctx := context.Background()
	repo, base, heads := readyRepository(t, "first", "second", "third")
	store, dsnFile := servingLedger(t, "owner/repo")
	for _, id := range []string{"first", "second", "third"} {
		if _, err := store.OpenMigration(ctx, ledger.OpenRequest{
			Repo: "owner/repo", ID: id, Owner: "alice", BaseCommit: base, Branch: id, Writes: []string{id + ".lean"},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Ready(ctx, ledger.ReadyRequest{
			Repo: "owner/repo", ID: id, Generation: 1, Head: heads[id], Key: "ready-" + id,
		}); err != nil {
			t.Fatal(err)
		}
	}

	// One round, then the loop waits and the ended context stops it.
	round, stop := context.WithTimeout(ctx, 5*time.Minute)
	defer stop()
	var output bytes.Buffer
	err := run(round, []string{"serve", "--config", servingConfig(t, "owner/repo", repo, dsnFile, time.Hour)}, &output)
	if err == nil || !strings.Contains(err.Error(), "context") {
		t.Fatalf("serve() error = %v, want the ended context, and never a configured batch", err)
	}
	var batches []struct {
		ID            string `json:"id"`
		Base          string `json:"base"`
		FrozenMembers []struct {
			Migration  string `json:"migration"`
			Head       string `json:"head"`
			ReadyEvent int64  `json:"ready_event"`
		} `json:"frozenMembers"`
	}
	if err := json.Unmarshal(output.Bytes(), &batches); err != nil {
		t.Fatalf("decode the serve output %q: %v", output.String(), err)
	}
	if len(batches) != 1 || batches[0].Base != base || len(batches[0].FrozenMembers) != 2 {
		t.Fatalf("serve formed %+v, want one batch at the branch tip with two members", batches)
	}
	for position, want := range []string{"first", "second"} {
		member := batches[0].FrozenMembers[position]
		if member.Migration != want || member.Head != heads[want] || member.ReadyEvent == 0 {
			t.Fatalf("member %d is %+v, want %s at its ready record", position, member, want)
		}
	}
	// The batch is the ledger's, under the id the ready list named.
	if stored, err := store.LoadBatch(ctx, "owner/repo", batches[0].ID); err != nil || stored.ID != batches[0].ID {
		t.Fatalf("the ledger holds %+v for %q: %v", stored, batches[0].ID, err)
	}
}

// With no ready migration, a round forms nothing and waits; the program still starts and reconciles.
func TestServeWithNoReadyMigrationFormsNothing(t *testing.T) {
	repo, _, _ := readyRepository(t)
	_, dsnFile := servingLedger(t, "owner/repo")

	round, stop := context.WithTimeout(context.Background(), 5*time.Minute)
	defer stop()
	var output bytes.Buffer
	err := run(round, []string{"serve", "--config", servingConfig(t, "owner/repo", repo, dsnFile, time.Hour)}, &output)
	if err == nil || !strings.Contains(err.Error(), "context") {
		t.Fatalf("serve() error = %v, want the ended context", err)
	}
	if strings.TrimSpace(output.String()) != "[]" {
		t.Fatalf("serve wrote %q, want no batch", output.String())
	}
}

// `land run` keeps its fixed member list for a single batch, so it still refuses a configuration with no batchId.
func TestRunStillNeedsItsConfiguredBatch(t *testing.T) {
	repo, _, _ := readyRepository(t)
	_, dsnFile := servingLedger(t, "owner/repo")
	var output bytes.Buffer
	err := run(context.Background(), []string{"run", "--config", servingConfig(t, "owner/repo", repo, dsnFile, time.Hour)}, &output)
	if err == nil || !strings.Contains(err.Error(), "batchId") {
		t.Fatalf("run() error = %v, want the missing batchId refused", err)
	}
}
