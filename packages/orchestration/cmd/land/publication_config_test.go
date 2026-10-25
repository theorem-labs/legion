package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/orchestration/internal/testpg"
)

// publicationConfig is a complete configuration with GitHub publication and neither a score nor an event command.
// Its ledger DSN names no database: a test that opens the ledger gives it one with withLedger.
func publicationConfig(t *testing.T) config {
	dsnFile := filepath.Join(t.TempDir(), "ledger.dsn")
	write(t, dsnFile, "postgres://unopened.invalid/ledger\n")
	return config{
		BatchID:     "batch",
		Repository:  repositoryConfig{Name: "owner/repo", Path: t.TempDir(), GitDir: "/srv/land/repo.git"},
		Base:        "base",
		BaseBranch:  "trunk",
		Build:       []string{"sh", "-c", "true"},
		Hasher:      []string{"sh", "-c", "true"},
		Toolchain:   []string{"sh", "-c", "printf tool"},
		Escalations: escalations{Owner: "landing-operator"},
		Ledger:      ledgerConfig{DSNFile: dsnFile},
		GitHub:      githubConfig{Remote: "origin"},
	}
}

// withLedger points cfg's ledger.dsn_file at a fresh ledger database.
func withLedger(t *testing.T, cfg config) config {
	t.Helper()
	write(t, cfg.Ledger.DSNFile, testpg.Database(t, "land_config")+"\n")
	return cfg
}

// buildProgram wires cfg and releases it at the test's end.
func buildProgram(t *testing.T, cfg config) (program, error) {
	t.Helper()
	built, err := cfg.program(context.Background())
	if err == nil {
		t.Cleanup(built.close)
	}
	return built, err
}

func TestReviewConfiguredPublicationWiresScoreAndEvents(t *testing.T) {
	cfg := publicationConfig(t)
	cfg.Score = scoreConfig{Command: []string{"sh", "-c", "printf score"}, Commit: []string{"sh", "-c", "true"}, Status: []string{"sh", "-c", "printf '{\"complete\":false}'"}, Path: "scores/out"}
	cfg.Event = eventConfig{Command: []string{"sh", "-c", "true"}, Status: []string{"sh", "-c", "printf '{\"complete\":false}'"}}
	built, err := buildProgram(t, withLedger(t, cfg))
	if err != nil {
		t.Fatalf("program() error = %v", err)
	}
	runner := built.runner
	if runner.GitHub == nil || runner.Score == nil || runner.Events == nil {
		t.Fatalf("publication dependencies = GitHub:%T Score:%T Events:%T, want all configured", runner.GitHub, runner.Score, runner.Events)
	}
}

// With GitHub publication configured, the score and the event are optional:
// without them the program publishes no score status and runs no member
// event, and the ledger's migration.landed is the record.
func TestConfiguredPublicationStartsWithoutScoreOrEvent(t *testing.T) {
	built, err := buildProgram(t, withLedger(t, publicationConfig(t)))
	if err != nil {
		t.Fatalf("program() error = %v", err)
	}
	runner := built.runner
	if runner.GitHub == nil || runner.Score != nil || runner.Events != nil {
		t.Fatalf("publication dependencies = GitHub:%T Score:%T Events:%T, want GitHub only", runner.GitHub, runner.Score, runner.Events)
	}
}

// A partly configured score or event is refused rather than run half-wired.
func TestConfiguredPublicationRefusesPartialScoreOrEvent(t *testing.T) {
	score := publicationConfig(t)
	score.Score = scoreConfig{Command: []string{"sh", "-c", "printf score"}}
	if _, err := buildProgram(t, score); err == nil || !strings.Contains(err.Error(), "score") {
		t.Fatalf("runner() error = %v, want the incomplete score refused", err)
	}
	event := publicationConfig(t)
	event.Event = eventConfig{Command: []string{"sh", "-c", "true"}}
	if _, err := buildProgram(t, event); err == nil || !strings.Contains(err.Error(), "event") {
		t.Fatalf("runner() error = %v, want the incomplete event refused", err)
	}
}

// The repository is an object: its ledger name, the local checkout and the
// program's bare clone on the build VM. The old string form is refused with
// an error that says what to write instead.
func TestReadConfigRepositoryObject(t *testing.T) {
	dir := t.TempDir()
	object := filepath.Join(dir, "object.json")
	write(t, object, `{"repository": {"name": "owner/repo", "path": "/srv/repo.git", "fetch_url": "https://example.invalid/repo.git", "git_dir": "/srv/land/repo.git"}}`)
	cfg, err := readConfig(object)
	if err != nil {
		t.Fatalf("readConfig(object) error = %v", err)
	}
	if want := (repositoryConfig{Name: "owner/repo", Path: "/srv/repo.git", GitDir: "/srv/land/repo.git"}); cfg.Repository != want {
		t.Fatalf("repository = %+v, want %+v", cfg.Repository, want)
	}

	legacy := filepath.Join(dir, "string.json")
	write(t, legacy, `{"repository": "owner/repo", "repositoryPath": "/srv/repo.git"}`)
	_, err = readConfig(legacy)
	if err == nil || !strings.Contains(err.Error(), `repository must be an object {"name", "path", "git_dir"}`) {
		t.Fatalf("readConfig(string) error = %v, want the string form refused with the object's shape", err)
	}
}

// Every key of the repository object that the program needs is required.
func TestRunnerRequiresRepositoryNamePathAndGitDir(t *testing.T) {
	for _, missing := range []string{"name", "path", "git_dir"} {
		cfg := publicationConfig(t)
		switch missing {
		case "name":
			cfg.Repository.Name = ""
		case "path":
			cfg.Repository.Path = ""
		case "git_dir":
			cfg.Repository.GitDir = ""
		}
		if _, err := buildProgram(t, cfg); err == nil || !strings.Contains(err.Error(), "repository."+missing) {
			t.Errorf("runner() without repository.%s error = %v, want it named", missing, err)
		}
	}
}
