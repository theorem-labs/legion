package landing

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

// The apply kinds of a hook: what the program does with the hook's first output after it exits.
const (
	// ApplyNone applies nothing: the hook's outputs are stored and read by nobody.
	ApplyNone = "none"
	// ApplyScore commits the output on the score ref of the hook's target.
	ApplyScore = "score"
	// ApplyContracts applies the output's operations through the ledger in one transaction.
	ApplyContracts = "contracts"
)

// The two inputs that the program writes itself, through its own connections, before each run. A configured input
// may not take either name.
const (
	// PreviousInput is the newest score of the part on the score hook's target ref, or empty when there is none.
	PreviousInput = "previous.json"
	// ContractsInput is the ledger's contract list of the repository, as a contracts hook reads it.
	ContractsInput = "contracts.json"
)

// defaultMaxOutputBytes caps each output a hook leaves in its scratch directory.
const defaultMaxOutputBytes = 8 << 20

// defaultHookTimeout is how long a hook may run when the configuration sets no timeout.
const defaultHookTimeout = 15 * time.Minute

// forbiddenInputEnv are the variables that no host-side input command may be given: the program's credentials.
var forbiddenInputEnv = []string{"SSH_AUTH_SOCK"}

// PostLanding is the configured, ordered list of hooks, and the parts each one runs for. A hook runs once for each
// part, so a configuration of two hooks and two parts is four effects of a landing.
type PostLanding struct {
	Parts          []string     `json:"parts"`
	TimeoutSeconds int          `json:"timeout_seconds"`
	MaxOutputBytes int64        `json:"max_output_bytes"`
	Hooks          []HookConfig `json:"hooks"`
}

// HookConfig is one configured hook. In the configuration it is either a bare argv array, which is short for
// {command: <argv>} with every other key at its default, or an object with these keys.
type HookConfig struct {
	Name    string               `json:"name"`
	Command []string             `json:"command"`
	Inputs  []HookInput          `json:"inputs"`
	Mounts  map[string]MountMode `json:"mounts"`
	Network Network              `json:"network"`
	Outputs []string             `json:"outputs"`
	// Apply is none (the default), score or contracts.
	Apply string `json:"apply"`
	// Target is the remote and ref a score apply pushes to.
	Target *HookTarget `json:"target"`
	// Publish is the Envoy topic a defect (exit 1) is published to, and empty when the hook has none.
	Publish string `json:"publish"`
}

// UnmarshalJSON reads a hook written as a bare argv array as the command of a hook with every other key at its
// default.
func (h *HookConfig) UnmarshalJSON(body []byte) error {
	if trimmed := strings.TrimSpace(string(body)); strings.HasPrefix(trimmed, "[") {
		var argv []string
		if err := json.Unmarshal(body, &argv); err != nil {
			return err
		}
		*h = HookConfig{Command: argv}
		return nil
	}
	type plain HookConfig
	return json.Unmarshal(body, (*plain)(h))
}

// HookTarget is where a score apply pushes: a remote URL and a ref on it.
type HookTarget struct {
	Remote string `json:"remote"`
	Ref    string `json:"ref"`
}

// HookInput is one host-side input of a hook: a command of the operator's configuration, run on the program's host
// with only the variables Env names, whose stdout becomes the file Name under /land/inputs.
type HookInput struct {
	Name    string   `json:"name"`
	Command []string `json:"command"`
	Env     []string `json:"env"`
}

// HookDefinition is everything the executors use, resolved at admission and frozen in the batch's record, so a
// configuration change after admission changes nothing for that batch.
type HookDefinition struct {
	Name    string      `json:"name,omitempty"`
	Command []string    `json:"command"`
	Inputs  []HookInput `json:"inputs,omitempty"`
	// Mounts are the named mounts the hook gets, each at its host path on the build VM with the mode it was given.
	Mounts  []Mount     `json:"mounts,omitempty"`
	Network Network     `json:"network,omitempty"`
	Outputs []string    `json:"outputs,omitempty"`
	Apply   string      `json:"apply"`
	Target  *HookTarget `json:"target,omitempty"`
	// Publish is the defect topic, and empty when the hook has none.
	Publish string `json:"publish,omitempty"`
	// Image is the container image, pinned by digest, and User the uid:gid of the VM user that owns the mounts.
	Image string `json:"image"`
	User  string `json:"user"`
	// GitDir is the program's clone on the build VM, read-only in the container as LAND_GIT_DIR.
	GitDir string `json:"git_dir"`
	// TimeoutSeconds is how long one run may take, and MaxOutputBytes the cap on each output it leaves.
	TimeoutSeconds int   `json:"timeout_seconds"`
	MaxOutputBytes int64 `json:"max_output_bytes"`
	// EscalateTo is the owner of every escalation of this effect, and EscalateAfter the attempt count that
	// enqueues one.
	EscalateTo    string `json:"escalate_to"`
	EscalateAfter int    `json:"escalate_after"`
	// Librarian is the argv of the librarian command that a contracts apply's deliveries run.
	Librarian []string `json:"librarian_command,omitempty"`
	// PartPosition is the part's position in post_landing.parts at admission, which orders the effects of one hook.
	PartPosition int `json:"part_position"`
}

// timeout is how long one run of the hook may take.
func (d HookDefinition) timeout() time.Duration {
	if d.TimeoutSeconds > 0 {
		return time.Duration(d.TimeoutSeconds) * time.Second
	}
	return defaultHookTimeout
}

// outputCap is the cap on each output the hook leaves in its scratch directory.
func (d HookDefinition) outputCap() int64 {
	if d.MaxOutputBytes > 0 {
		return d.MaxOutputBytes
	}
	return defaultMaxOutputBytes
}

// Validate refuses a configuration whose hooks the executors could not run, with the mounts that
// container.mounts declares. The program checks its own configuration before it starts, because the ledger stores
// what the program sends and checks none of it.
func (p PostLanding) Validate(mounts map[string]Mount) error {
	for index, hook := range p.Hooks {
		if err := hook.validate(mounts); err != nil {
			return fmt.Errorf("post_landing.hooks[%d]: %w", index, err)
		}
	}
	return nil
}

func (h HookConfig) validate(mounts map[string]Mount) error {
	if len(h.Command) == 0 {
		return fmt.Errorf("command is required")
	}
	apply := h.Apply
	if apply == "" {
		apply = ApplyNone
	}
	if apply != ApplyNone && apply != ApplyScore && apply != ApplyContracts {
		return fmt.Errorf("apply %q is not %q, %q or %q", h.Apply, ApplyNone, ApplyScore, ApplyContracts)
	}
	if apply == ApplyScore && (h.Target == nil || h.Target.Remote == "" || h.Target.Ref == "") {
		return fmt.Errorf("a score hook needs target.remote and target.ref")
	}
	if apply != ApplyNone && len(h.Outputs) == 0 {
		return fmt.Errorf("apply %q needs outputs: its apply reads the first of them", apply)
	}
	switch h.Network {
	case "", NetworkNone, NetworkDefault:
	default:
		return fmt.Errorf("network %q is not %q or %q", h.Network, NetworkDefault, NetworkNone)
	}
	for _, name := range slices.Sorted(maps.Keys(h.Mounts)) {
		mode := h.Mounts[name]
		if mode != ReadWrite && mode != ReadOnly {
			return fmt.Errorf("mount %q has mode %q, which is neither %q nor %q", name, mode, ReadWrite, ReadOnly)
		}
		if _, declared := mounts[name]; !declared {
			return fmt.Errorf("mount %q is not declared in container.mounts", name)
		}
	}
	for _, input := range h.Inputs {
		if input.Name == PreviousInput || input.Name == ContractsInput {
			return fmt.Errorf("input %q is written by the program itself", input.Name)
		}
		if input.Name == "" || strings.ContainsAny(input.Name, `/\`) {
			return fmt.Errorf("input name %q is not a file name", input.Name)
		}
		if len(input.Command) == 0 || !strings.HasPrefix(input.Command[0], "/") {
			return fmt.Errorf("input %q needs a command whose program is an absolute path", input.Name)
		}
		for _, name := range input.Env {
			if slices.Contains(forbiddenInputEnv, name) || strings.HasPrefix(name, "GH_") ||
				strings.HasPrefix(name, "GITHUB_") || strings.HasSuffix(name, "_DSN") || strings.HasSuffix(name, "_TOKEN") {
				return fmt.Errorf("input %q may not be given %s, which is a credential of the program", input.Name, name)
			}
		}
	}
	return nil
}

// hookEffects are the frozen hook effects of a landing: one for each (hook, part), in the hooks' configured order
// and then the parts' configured order. Every value an executor reads is copied into the effect's definition here,
// at admission, so a later configuration change cannot reach this batch.
func (r Runner) hookEffects(batch Batch) []PostLandEffect {
	effects := make([]PostLandEffect, 0, len(r.PostLanding.Hooks)*len(r.PostLanding.Parts))
	for index, hook := range r.PostLanding.Hooks {
		for position, part := range r.PostLanding.Parts {
			definition := r.freeze(hook, position)
			effects = append(effects, PostLandEffect{
				Key:        fmt.Sprintf("hook/%s/%d/%s", batch.ID, index, part),
				Op:         OpHook,
				Index:      index,
				Part:       part,
				Definition: &definition,
			})
		}
	}
	return effects
}

// freeze resolves one hook against the runner's configuration: its named mounts at their host paths, the image and
// user, the timeout and output cap, the escalations owner and threshold, and the librarian command of a contracts
// hook.
func (r Runner) freeze(hook HookConfig, partPosition int) HookDefinition {
	definition := HookDefinition{
		Name:           hook.Name,
		Command:        append([]string(nil), hook.Command...),
		Inputs:         append([]HookInput(nil), hook.Inputs...),
		Network:        hook.Network,
		Outputs:        append([]string(nil), hook.Outputs...),
		Apply:          hook.Apply,
		Target:         hook.Target,
		Publish:        hook.Publish,
		Image:          r.Container.Image,
		User:           r.Container.User,
		GitDir:         r.Container.GitDir,
		TimeoutSeconds: r.PostLanding.TimeoutSeconds,
		MaxOutputBytes: r.PostLanding.MaxOutputBytes,
		EscalateTo:     r.EscalateTo,
		EscalateAfter:  r.escalateAfter(),
		PartPosition:   partPosition,
	}
	if definition.Apply == "" {
		definition.Apply = ApplyNone
	}
	if definition.Network == "" {
		definition.Network = NetworkNone
	}
	for _, name := range slices.Sorted(maps.Keys(hook.Mounts)) {
		mount := r.Container.Mounts[name]
		definition.Mounts = append(definition.Mounts, Mount{Name: name, HostPath: mount.HostPath, Mode: hook.Mounts[name]})
	}
	if definition.Apply == ApplyContracts {
		definition.Librarian = append([]string(nil), r.Librarian...)
	}
	return definition
}

// Container is the part of the container configuration that a frozen hook definition copies: the pinned image, the
// user of every run, and the mounts that container.mounts declares, by name.
type Container struct {
	Image string
	User  string
	// GitDir is the program's bare clone on the build VM, which every hook gets read-only as LAND_GIT_DIR.
	GitDir string
	Mounts map[string]Mount
}
