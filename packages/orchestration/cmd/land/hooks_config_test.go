package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/orchestration/landing"
)

// hookedConfig is a publication configuration with two declared mounts and one configured hook.
func hookedConfig(t *testing.T, hook landing.HookConfig) config {
	cfg := publicationConfig(t)
	cfg.Container = containerConfig{
		Image: "image@sha256:one", User: "1002:1003",
		Mounts: []mountConfig{{Name: "index", HostPath: "/srv/land/index"}, {Name: "graph", HostPath: "/srv/land/graph"}},
	}
	cfg.PostLanding = landing.PostLanding{Parts: []string{"slice", "all"}, TimeoutSeconds: 21600, Hooks: []landing.HookConfig{hook}}
	cfg.Librarian = librarianConfig{Command: []string{"/usr/local/bin/librarian-notify"}}
	cfg.Effects = effectsConfig{EscalateAfter: 4}
	return cfg
}

// The program starts with its hooks wired: the configuration's parts, hooks, container and librarian reach the
// runner, which freezes them into each batch it admits.
func TestTheConfiguredHooksReachTheRunner(t *testing.T) {
	cfg := hookedConfig(t, landing.HookConfig{
		Name: "ledger", Command: []string{"/usr/local/bin/declindex", "ledger"}, Apply: landing.ApplyContracts,
		Outputs: []string{"ops.jsonl"}, Mounts: map[string]landing.MountMode{"index": landing.ReadOnly},
	})
	built, err := buildProgram(t, withLedger(t, cfg))
	if err != nil {
		t.Fatalf("program() error = %v", err)
	}
	runner := built.runner
	if runner.Effects == nil {
		t.Fatal("the runner has no effect log")
	}
	if len(runner.PostLanding.Hooks) != 1 || runner.Container.Image != "image@sha256:one" ||
		runner.Container.Mounts["index"].HostPath != "/srv/land/index" || runner.EscalateAfter != 4 {
		t.Fatalf("the runner's hook configuration is %+v / %+v", runner.PostLanding, runner.Container)
	}
	effects := runner.PostLanding.Hooks
	if effects[0].Apply != landing.ApplyContracts || runner.Librarian[0] != "/usr/local/bin/librarian-notify" {
		t.Fatalf("the configured hook is %+v with librarian %q", effects[0], runner.Librarian)
	}
}

// A hook the executors could not run refuses the program at start, because the ledger stores what the program
// sends and checks none of it.
func TestTheProgramRefusesAHookItCouldNotRun(t *testing.T) {
	for name, hook := range map[string]landing.HookConfig{
		"an apply that is not one of the three": {Command: []string{"/bin/hook"}, Apply: "push"},
		"a score hook with no target": {Command: []string{"/bin/hook"}, Apply: landing.ApplyScore,
			Outputs: []string{"score.json"}},
		"an apply with no outputs":     {Command: []string{"/bin/hook"}, Apply: landing.ApplyContracts},
		"an undeclared mount":          {Command: []string{"/bin/hook"}, Mounts: map[string]landing.MountMode{"elan": landing.ReadOnly}},
		"a mount mode that is neither": {Command: []string{"/bin/hook"}, Mounts: map[string]landing.MountMode{"index": "rw"}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := buildProgram(t, hookedConfig(t, hook)); err == nil ||
				!strings.Contains(err.Error(), "post_landing.hooks[0]") {
				t.Fatalf("program() error = %v, want %s refused", err, name)
			}
		})
	}
}

// A configuration with hooks and no escalations owner is refused: the ledger enqueues every escalation for the
// owner it is sent, and an item with an empty owner wakes nobody.
func TestTheProgramRefusesHooksWithNoEscalationsOwner(t *testing.T) {
	cfg := hookedConfig(t, landing.HookConfig{Command: []string{"/bin/hook"}})
	cfg.Escalations.Owner = "  "
	if _, err := buildProgram(t, cfg); err == nil || !strings.Contains(err.Error(), "escalations.owner") {
		t.Fatalf("program() error = %v, want the empty escalations owner refused", err)
	}
}

// A hook written as a bare argv array in the file is read as a hook with every other key at its default.
func TestReadConfigReadsABareArgvHook(t *testing.T) {
	path := filepath.Join(t.TempDir(), "land.json")
	write(t, path, `{"post_landing": {"parts": ["slice"], "hooks": [["declindex", "refresh"]]}}`)
	cfg, err := readConfig(path)
	if err != nil {
		t.Fatalf("readConfig() error = %v", err)
	}
	body, _ := json.Marshal(cfg.PostLanding.Hooks[0].Command)
	if string(body) != `["declindex","refresh"]` {
		t.Fatalf("the bare argv hook's command is %s", body)
	}
}
