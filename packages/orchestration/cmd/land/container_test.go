package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/orchestration/internal/testdocker"
	"github.com/sjawhar/legion/orchestration/landing"
	"github.com/sjawhar/legion/orchestration/replay"
)

// containerSection is a container configuration whose "VM" is this host: the stand-in ssh runs docker here, the
// clone, run root and build root are temporary directories, and the directories in reads are declared as mounts
// that every check and build gets read-only, so their scripts run in the container.
func containerSection(t *testing.T, reads ...string) map[string]any {
	t.Helper()
	user := testdocker.Require(t)
	root := t.TempDir()
	clone, runs, cache := filepath.Join(root, "clone.git"), filepath.Join(root, "runs"), filepath.Join(root, "lakecache")
	for _, dir := range []string{clone, runs, cache} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mounts := []map[string]any{{"name": "lake-cache", "host_path": cache}}
	shared := map[string]any{}
	for index, dir := range reads {
		name := "read-" + string(rune('a'+index))
		mounts = append(mounts, map[string]any{"name": name, "host_path": dir})
		shared[name] = "read-only"
	}
	build := map[string]any{"lake-cache": "read-write"}
	for name, mode := range shared {
		build[name] = mode
	}
	return map[string]any{
		"ssh": testdocker.SSH(t), "image": testdocker.Image, "user": user,
		"run_root": runs, "build_root": "lake-cache", "mounts": mounts,
		"build_mounts": build, "check_mounts": shared, "clone": clone,
	}
}

// withContainer adds the section to a configuration map, with the clone as repository.git_dir.
func withContainer(values map[string]any, section map[string]any) map[string]any {
	clone := section["clone"]
	delete(section, "clone")
	repository := values["repository"].(map[string]any)
	repository["git_dir"] = clone
	values["container"] = section
	return values
}

// The container section must name the SSH command, the image, the user, the run root and the build root; a mount
// map may name only declared mounts with one of the two modes; the build root must be read-write for the builds;
// and the hasher's network is "default" or "none".
func TestContainerConfigurationIsChecked(t *testing.T) {
	valid := func() config {
		return config{
			Repository: repositoryConfig{Name: "r", Path: "/srv/repo", GitDir: "/srv/land/repo.git"},
			Container: containerConfig{
				SSH: []string{"/usr/bin/ssh", "vm"}, Image: "sha256:one", User: "1002:1003", RunRoot: "/srv/land/runs", BuildRoot: "lake-cache",
				Mounts:      []mountConfig{{Name: "lake-cache", HostPath: "/srv/lakecache"}, {Name: "index-reader-dsn", HostPath: "/srv/land/index-reader.dsn"}},
				BuildMounts: map[string]landing.MountMode{"lake-cache": landing.ReadWrite},
			},
		}
	}
	if _, err := valid().docker(); err != nil {
		t.Fatalf("a valid section is refused: %v", err)
	}
	for name, change := range map[string]func(*config){
		"container.ssh":      func(c *config) { c.Container.SSH = nil },
		"container.run_root": func(c *config) { c.Container.RunRoot = "" },
		"does not declare":   func(c *config) { c.Container.HasherMounts = map[string]landing.MountMode{"index": landing.ReadOnly} },
		"not \"read-only\"":  func(c *config) { c.Container.CheckMounts = map[string]landing.MountMode{"lake-cache": "rw"} },
		"must make the build": func(c *config) {
			c.Container.BuildMounts = map[string]landing.MountMode{"lake-cache": landing.ReadOnly}
		},
		"hasher_network":             func(c *config) { c.Container.HasherNetwork = "bridge" },
		"check_network":              func(c *config) { c.Container.CheckNetwork = "bridge" },
		"container.build_root names": func(c *config) { c.Container.BuildRoot = "cache" },
	} {
		c := valid()
		change(&c)
		if _, err := c.docker(); err == nil || !strings.Contains(err.Error(), name) {
			t.Fatalf("%s: docker() error = %v", name, err)
		}
	}
	c := valid()
	c.Container.HasherNetwork = landing.NetworkHost
	c.Container.CheckNetwork = landing.NetworkHost
	c.Container.HasherMounts = map[string]landing.MountMode{"index-reader-dsn": landing.ReadOnly}
	c.Container.CheckMounts = map[string]landing.MountMode{"index-reader-dsn": landing.ReadOnly}
	k, err := c.docker()
	if err != nil || len(k.hasher) != 1 || k.hasher[0].HostPath != "/srv/land/index-reader.dsn" || k.hasher[0].Mode != landing.ReadOnly {
		t.Fatalf("docker() = %+v, %v", k.hasher, err)
	}
	if k.checkNetwork != landing.NetworkHost || len(k.check) != 1 || k.check[0].Name != "index-reader-dsn" {
		t.Fatalf("the checks get network %q and mounts %+v", k.checkNetwork, k.check)
	}
}

// land run refuses to start when repository.git_dir does not exist on the VM.
func TestLandRunRefusesAMissingClone(t *testing.T) {
	cfg := withLedger(t, publicationConfig(t))
	cfg.Container = containerConfig{SSH: testdocker.SSH(t), Image: testdocker.Image, User: testdocker.Require(t), RunRoot: t.TempDir(), BuildRoot: "lake-cache",
		Mounts: []mountConfig{{Name: "lake-cache", HostPath: t.TempDir()}}, BuildMounts: map[string]landing.MountMode{"lake-cache": landing.ReadWrite}}
	cfg.Repository.GitDir = filepath.Join(t.TempDir(), "absent.git")
	built, err := buildProgram(t, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := built.containers.docker.Start(context.Background(), built.containers.declared, true); err == nil || !strings.Contains(err.Error(), "repository.git_dir") {
		t.Fatalf("Start() = %v, want the missing clone refused", err)
	}
}

// land blame --config confirms with the configured build, in a container: the build sees the tree and the module.
func TestLandBlameConfirmsWithTheConfiguredBuildInAContainer(t *testing.T) {
	repo, base, heads := scratchRepository(t, map[string]string{"culprit": "-def gone := 0"})
	marker := t.TempDir()
	script := writeScript(t, `test -f Alpha/Core.lean && test -n "$LAND_MODULES" && test -d /land/scratch || exit 9
grep -q 'def gone' Alpha/Core.lean && exit 0
exit 1`)
	section := containerSection(t, filepath.Dir(script), marker)
	config := writeConfig(t, withContainer(map[string]any{
		"repository": map[string]any{"name": "r", "path": repo}, "build": []string{script},
	}, section))
	members := filepath.Join(t.TempDir(), "members.tsv")
	write(t, members, "culprit\t"+heads["culprit"]+"\n")
	log := filepath.Join(t.TempDir(), "build.log")
	write(t, log, "✖ [2/2] Building Alpha.Use\nerror: Alpha/Use.lean:2:12: Unknown identifier `Alpha.gone`\n")
	var out bytes.Buffer
	if err := run(context.Background(), []string{"blame", "--repo", repo, "--base", base, "--members", members, "--log", log, "--config", config}, &out); err != nil {
		t.Fatalf("blame --config: %v", err)
	}
	if !strings.HasPrefix(out.String(), "culprit\tAlpha.Use\t") {
		t.Fatalf("blame output %q, want the culprit confirmed", out.String())
	}
	if err := run(context.Background(), []string{"blame", "--repo", repo, "--base", base, "--members", members, "--log", log, "--config", config, "--confirm-with", "true"}, &out); err == nil {
		t.Fatal("blame with --config and --confirm-with was accepted")
	}
}

// land replay --confirm confirms with the configured build in a container, and its checks run in containers too.
func TestLandReplayConfirmsInAContainer(t *testing.T) {
	repo, base, heads := scratchRepository(t, map[string]string{"quiet": "+def extra := 1"})
	dir := t.TempDir()
	writeFile(t, dir, "build-round-1.log", "")
	manifest := writeManifest(t, dir, replayManifest(repo, base, "quiet", heads["quiet"]))
	check := graphCheck(t, `refuse\tadded\tAlpha.extra\tAlpha/Core.lean:5:1\tN2: x\n`)
	build := writeScript(t, `test -f Alpha/Core.lean && test -d /land/scratch`)
	section := containerSection(t, filepath.Dir(check["command"].([]string)[0]), filepath.Dir(build))
	config := writeConfig(t, withContainer(map[string]any{
		"repository": map[string]any{"name": "r", "path": repo}, "moduleRoots": []string{"Alpha"},
		"checks": []map[string]any{check}, "build": []string{build},
	}, section))
	got := runReplay(t, manifest, config, "--confirm")
	if got.err != nil {
		t.Fatalf("replay: %v", got.err)
	}
	if len(got.summary) != 1 || len(got.summary[0].FalseRefusals) != 1 || got.summary[0].FalseRefusals[0].Outcome != "confirmed-green" {
		t.Fatalf("summary = %+v", got.summary)
	}
}

// replayManifest is a one-round replay of one member.
func replayManifest(repo, base, id, head string) replay.Manifest {
	return replay.Manifest{Repository: repo, Base: base,
		Members:     []replay.Member{{ID: id, Head: head}},
		BuildRounds: []replay.BuildRound{{Round: 1, Log: "build-round-1.log"}}}
}

// The bridge is not a network a container may have: a configuration that still says "default" is refused at load,
// for a hook and for each container key, with the two values it may have.
func TestTheBridgeIsRefused(t *testing.T) {
	cfg := publicationConfig(t)
	cfg.PostLanding = landing.PostLanding{Parts: []string{"slice"}, Hooks: []landing.HookConfig{
		{Name: "refresh", Command: []string{"/bin/true"}, Network: "default"},
	}}
	if _, err := buildProgram(t, cfg); err == nil || !strings.Contains(err.Error(), `network "default" is not "host" or "none"`) {
		t.Fatalf("a hook with network default = %v", err)
	}
	for _, key := range []string{"hasher_network", "check_network"} {
		cfg := publicationConfig(t)
		if key == "hasher_network" {
			cfg.Container.HasherNetwork = "default"
		} else {
			cfg.Container.CheckNetwork = "default"
		}
		_, err := cfg.docker()
		if err == nil || !strings.Contains(err.Error(), "container."+key+` "default" is not "host" or "none"`) {
			t.Fatalf("container.%s default = %v", key, err)
		}
	}
}
