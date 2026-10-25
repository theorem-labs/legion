package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// publicationConfig is a complete configuration with GitHub publication and
// neither a score nor an event command.
func publicationConfig(t *testing.T) config {
	return config{
		BatchID:    "batch",
		Repository: repositoryConfig{Name: "owner/repo", Path: t.TempDir(), GitDir: "/theorem/land/repo.git"},
		Base:       "base",
		BaseBranch: "trunk",
		StatePath:  t.TempDir() + "/state.json",
		Build:      []string{"sh", "-c", "true"},
		Hasher:     []string{"sh", "-c", "true"},
		Toolchain:  []string{"sh", "-c", "printf tool"},
		Ledger:     ledgerConfig{Check: []string{"sh", "-c", "printf '{\"pass\":true}'"}, Land: []string{"sh", "-c", "true"}, Return: []string{"sh", "-c", "true"}, Status: []string{"sh", "-c", "printf '{\"complete\":false}'"}, MigrationState: []string{"sh", "-c", "printf '{\"state\":\"open\"}'"}},
		GitHub:     githubConfig{Remote: "origin"},
	}
}

func TestReviewConfiguredPublicationWiresScoreAndEvents(t *testing.T) {
	cfg := publicationConfig(t)
	cfg.Score = scoreConfig{Command: []string{"sh", "-c", "printf score"}, Commit: []string{"sh", "-c", "true"}, Status: []string{"sh", "-c", "printf '{\"complete\":false}'"}, Path: "scores/out"}
	cfg.Event = eventConfig{Command: []string{"sh", "-c", "true"}, Status: []string{"sh", "-c", "printf '{\"complete\":false}'"}}
	runner, _, _, err := cfg.runner()
	if err != nil {
		t.Fatalf("runner() error = %v", err)
	}
	if runner.GitHub == nil || runner.Score == nil || runner.Events == nil {
		t.Fatalf("publication dependencies = GitHub:%T Score:%T Events:%T, want all configured", runner.GitHub, runner.Score, runner.Events)
	}
}

// With GitHub publication configured, the score and the event are optional:
// without them the program publishes no score status and runs no member
// event, and the ledger's migration.landed is the record.
func TestConfiguredPublicationStartsWithoutScoreOrEvent(t *testing.T) {
	runner, _, _, err := publicationConfig(t).runner()
	if err != nil {
		t.Fatalf("runner() error = %v", err)
	}
	if runner.GitHub == nil || runner.Score != nil || runner.Events != nil {
		t.Fatalf("publication dependencies = GitHub:%T Score:%T Events:%T, want GitHub only", runner.GitHub, runner.Score, runner.Events)
	}
}

// A partly configured score or event is refused rather than run half-wired.
func TestConfiguredPublicationRefusesPartialScoreOrEvent(t *testing.T) {
	score := publicationConfig(t)
	score.Score = scoreConfig{Command: []string{"sh", "-c", "printf score"}}
	if _, _, _, err := score.runner(); err == nil || !strings.Contains(err.Error(), "score") {
		t.Fatalf("runner() error = %v, want the incomplete score refused", err)
	}
	event := publicationConfig(t)
	event.Event = eventConfig{Command: []string{"sh", "-c", "true"}}
	if _, _, _, err := event.runner(); err == nil || !strings.Contains(err.Error(), "event") {
		t.Fatalf("runner() error = %v, want the incomplete event refused", err)
	}
}

// The repository is an object: its ledger name, the local checkout and the
// program's bare clone on the build VM. The old string form is refused with
// an error that says what to write instead.
func TestReadConfigRepositoryObject(t *testing.T) {
	dir := t.TempDir()
	object := filepath.Join(dir, "object.json")
	write(t, object, `{"repository": {"name": "owner/repo", "path": "/srv/repo.git", "fetch_url": "https://example.invalid/repo.git", "git_dir": "/theorem/land/repo.git"}}`)
	cfg, err := readConfig(object)
	if err != nil {
		t.Fatalf("readConfig(object) error = %v", err)
	}
	if want := (repositoryConfig{Name: "owner/repo", Path: "/srv/repo.git", GitDir: "/theorem/land/repo.git"}); cfg.Repository != want {
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
		if _, _, _, err := cfg.runner(); err == nil || !strings.Contains(err.Error(), "repository."+missing) {
			t.Errorf("runner() without repository.%s error = %v, want it named", missing, err)
		}
	}
}
