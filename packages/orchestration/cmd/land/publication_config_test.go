package main

import (
	"testing"
)

func TestReviewConfiguredPublicationRequiresAndWiresScoreEvents(t *testing.T) {
	cfg := config{
		BatchID:        "batch",
		Repository:     "owner/repo",
		RepositoryPath: t.TempDir(),
		Base:           "base",
		BaseBranch:     "trunk",
		StatePath:      t.TempDir() + "/state.json",
		Build:          []string{"sh", "-c", "true"},
		Hasher:         []string{"sh", "-c", "true"},
		Toolchain:      []string{"sh", "-c", "printf tool"},
		Ledger:         ledgerConfig{Check: []string{"sh", "-c", "printf '{\"pass\":true}'"}, Land: []string{"sh", "-c", "true"}, Return: []string{"sh", "-c", "true"}, Status: []string{"sh", "-c", "printf '{\"complete\":false}'"}},
		GitHub:         githubConfig{Remote: "origin"},
		Score:          scoreConfig{Command: []string{"sh", "-c", "printf score"}, Commit: []string{"sh", "-c", "true"}, Status: []string{"sh", "-c", "printf '{\"complete\":false}'"}, Path: "scores/out"},
		Event:          eventConfig{Command: []string{"sh", "-c", "true"}, Status: []string{"sh", "-c", "printf '{\"complete\":false}'"}},
	}
	runner, _, _, err := cfg.runner()
	if err != nil {
		t.Fatalf("runner() error = %v", err)
	}
	if runner.GitHub == nil || runner.Score == nil || runner.Events == nil {
		t.Fatalf("publication dependencies = GitHub:%T Score:%T Events:%T, want all configured", runner.GitHub, runner.Score, runner.Events)
	}
}
