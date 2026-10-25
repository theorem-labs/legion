package landing

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// hookConfig is a configured hook of the given name, apply kind and mounts.
func hookConfig(name, apply string, outputs []string, mounts map[string]MountMode) HookConfig {
	hook := HookConfig{Name: name, Command: []string{"/bin/" + name}, Outputs: outputs, Apply: apply, Mounts: mounts}
	if apply == ApplyScore {
		hook.Target = &HookTarget{Remote: "https://example.invalid/repo", Ref: "refs/heads/score"}
	}
	return hook
}

// declaredMounts are the mounts a configuration declares, by name.
func declaredMounts() map[string]Mount {
	return map[string]Mount{
		"index": {Name: "index", HostPath: "/srv/land/index"},
		"graph": {Name: "graph", HostPath: "/srv/land/graph"},
	}
}

// hookRunner is a runner whose post-landing configuration is the hooks and parts given.
func hookRunner(hooks []HookConfig, parts []string) Runner {
	return Runner{
		PostLanding:   PostLanding{Parts: parts, Hooks: hooks, TimeoutSeconds: 900, MaxOutputBytes: 1 << 20},
		Container:     Container{Image: "image@sha256:one", User: "1002:1003", GitDir: "/srv/land/repo.git", Mounts: declaredMounts()},
		EscalateTo:    "escalations",
		EscalateAfter: 3,
		Librarian:     []string{"/bin/librarian-notify"},
	}
}

// Admission freezes one effect for each hook and part, in the configured order of the hooks and then of the parts,
// with everything the executors read copied into the effect: the image, the user, the mounts at their host paths,
// the apply kind and its target, the escalations owner and threshold, and the librarian command of a contracts
// hook.
func TestAdmissionFreezesOneEffectForEachHookAndPart(t *testing.T) {
	runner := hookRunner([]HookConfig{
		hookConfig("refresh", "", nil, map[string]MountMode{"index": ReadWrite}),
		hookConfig("ledger", ApplyContracts, []string{"ops.jsonl"}, map[string]MountMode{"index": ReadOnly, "graph": ReadOnly}),
	}, []string{"slice", "all"})
	effects := runner.hookEffects(Batch{ID: "batch"})

	keys := []string{}
	for _, effect := range effects {
		keys = append(keys, effect.Key)
	}
	want := []string{"hook/batch/0/slice", "hook/batch/0/all", "hook/batch/1/slice", "hook/batch/1/all"}
	if !slices.Equal(keys, want) {
		t.Fatalf("frozen effects = %q, want %q", keys, want)
	}
	refresh := *effects[0].Definition
	if refresh.Image != "image@sha256:one" || refresh.User != "1002:1003" || refresh.EscalateTo != "escalations" ||
		refresh.EscalateAfter != 3 || refresh.Apply != ApplyNone || refresh.Network != NetworkNone {
		t.Fatalf("the frozen refresh is %+v", refresh)
	}
	if !slices.Equal(refresh.Mounts, []Mount{{Name: "index", HostPath: "/srv/land/index", Mode: ReadWrite}}) {
		t.Fatalf("the refresh's frozen mounts are %+v", refresh.Mounts)
	}
	if effects[1].Part != "all" || effects[1].Definition.PartPosition != 1 {
		t.Fatalf("the second effect is %+v", effects[1])
	}
	contracts := *effects[2].Definition
	if contracts.Apply != ApplyContracts || !slices.Equal(contracts.Librarian, []string{"/bin/librarian-notify"}) {
		t.Fatalf("the frozen ledger hook is %+v", contracts)
	}
	if len(contracts.Mounts) != 2 || contracts.Mounts[0].Name != "graph" || contracts.Mounts[1].Name != "index" {
		t.Fatalf("the ledger hook's frozen mounts are %+v", contracts.Mounts)
	}
}

// A hook written as a bare argv array is a hook whose every other key is at its default.
func TestABareArgvIsAHookWithEveryOtherKeyAtItsDefault(t *testing.T) {
	var configured PostLanding
	if err := unmarshalPostLanding(`{"parts":["slice"],"hooks":[["declindex","refresh"]]}`, &configured); err != nil {
		t.Fatal(err)
	}
	hook := configured.Hooks[0]
	if !slices.Equal(hook.Command, []string{"declindex", "refresh"}) || hook.Apply != "" || len(hook.Mounts) != 0 {
		t.Fatalf("the bare argv hook is %+v", hook)
	}
	if err := configured.Validate(declaredContainer()); err != nil {
		t.Fatalf("Validate() of a bare argv hook = %v", err)
	}
}

// The program checks its own configuration before it starts, because the ledger stores what it sends and checks
// none of it.
func TestAConfigurationTheExecutorsCouldNotRunIsRefused(t *testing.T) {
	for name, hook := range map[string]HookConfig{
		"an apply that is not one of the three": {Command: []string{"/bin/hook"}, Apply: "publish"},
		"a score hook with no target":           {Command: []string{"/bin/hook"}, Apply: ApplyScore, Outputs: []string{"score.json"}},
		"an apply with no outputs":              {Command: []string{"/bin/hook"}, Apply: ApplyContracts},
		"a mount mode that is neither":          {Command: []string{"/bin/hook"}, Mounts: map[string]MountMode{"index": "rw"}},
		"a mount that is not declared":          {Command: []string{"/bin/hook"}, Mounts: map[string]MountMode{"lake-cache": ReadOnly}},
		"a network that is neither":             {Command: []string{"/bin/hook"}, Network: "bridge"},
		"an input the program writes itself": {Command: []string{"/bin/hook"}, Inputs: []HookInput{
			{Name: ContractsInput, Command: []string{"/bin/contracts"}}}},
		"an input command that is not absolute": {Command: []string{"/bin/hook"}, Inputs: []HookInput{
			{Name: "cost.json", Command: []string{"land-cost"}}}},
		"an input given a credential": {Command: []string{"/bin/hook"}, Inputs: []HookInput{
			{Name: "cost.json", Command: []string{"/bin/land-cost"}, Env: []string{"LEDGER_DSN"}}}},
		"a hook with no command": {Name: "empty"},
	} {
		t.Run(name, func(t *testing.T) {
			err := PostLanding{Parts: []string{"slice"}, Hooks: []HookConfig{hook}}.Validate(declaredContainer())
			if err == nil {
				t.Fatalf("Validate() accepted %s", name)
			}
			if !strings.Contains(err.Error(), "post_landing.hooks[0]") {
				t.Fatalf("Validate() error = %v, want the hook named", err)
			}
		})
	}
}

// A configuration whose hooks the executors can run is accepted.
func TestAConfigurationTheExecutorsCanRunIsAccepted(t *testing.T) {
	configured := PostLanding{Parts: []string{"slice", "all"}, Hooks: []HookConfig{
		hookConfig("refresh", "", nil, map[string]MountMode{"index": ReadWrite}),
		hookConfig("score", ApplyScore, []string{"score.json"}, map[string]MountMode{"graph": ReadOnly}),
	}}
	configured.Hooks[1].Inputs = []HookInput{{Name: "cost.json", Command: []string{"/bin/land-cost", "$LAND_COMMIT"}}}
	if err := configured.Validate(declaredContainer()); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
}

// unmarshalPostLanding reads a post_landing block as the configuration file holds it.
func unmarshalPostLanding(body string, into *PostLanding) error {
	return json.Unmarshal([]byte(body), into)
}

// declaredContainer is the container the hooks are frozen with: the program's clone and the declared mounts.
func declaredContainer() Container {
	return Container{Image: "image@sha256:one", User: "1002:1003", GitDir: "/srv/land/repo.git", Mounts: declaredMounts()}
}

// Every hook gets the program's clone, which holds LAND_TREE, so a configuration with hooks and no
// repository.git_dir is refused at load, naming the key, rather than running hooks that cannot read the tree.
func TestHooksWithoutTheProgramsCloneAreRefused(t *testing.T) {
	configured := PostLanding{Parts: []string{"slice"}, Hooks: []HookConfig{{Name: "graph", Command: []string{"/bin/graph"}}}}
	container := declaredContainer()
	container.GitDir = ""
	err := configured.Validate(container)
	if err == nil || !strings.Contains(err.Error(), "repository.git_dir is required to run hooks") {
		t.Fatalf("Validate() without the clone = %v, want repository.git_dir named", err)
	}
	// With no hook there is nothing to run, so the key is not required here.
	if err := (PostLanding{Parts: []string{"slice"}}).Validate(container); err != nil {
		t.Fatalf("Validate() with no hook = %v", err)
	}
}
