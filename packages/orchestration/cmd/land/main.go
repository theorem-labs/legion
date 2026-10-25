package main

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/legion/orchestration/landing"
	"github.com/sjawhar/legion/orchestration/landing/blame"
	"github.com/sjawhar/legion/orchestration/landing/gitx"
	"github.com/sjawhar/legion/orchestration/landing/ledgerstore"
	"github.com/sjawhar/legion/orchestration/ledger"
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "land:", err)
		var usage usageError
		if errors.As(err, &usage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

// usageError is a command line that names no known command or lacks a
// required flag; land exits 2 for it.
type usageError struct{ error }

func (e usageError) Unwrap() error { return e.error }

func run(ctx context.Context, args []string, output io.Writer) error {
	if err := requireGit(ctx); err != nil {
		return err
	}
	if len(args) == 0 {
		return usageError{fmt.Errorf("usage: land <run|serve|status|blame|check|replay>")}
	}
	switch args[0] {
	case "blame":
		return runBlame(ctx, args[1:], output)
	case "check":
		return runCheck(ctx, args[1:], output)
	case "replay":
		return runReplayCommand(ctx, args[1:], output)
	case "run", "serve", "status":
		return runBatchCommand(ctx, args[0], args[1:], output)
	default:
		return usageError{fmt.Errorf("unknown command %q", args[0])}
	}
}

// minimumGitMajor and minimumGitMinor are the oldest git the program runs on. Its merge-tree writes the tree of a
// merge, which is how the program confirms an upstream sync's head.
const (
	minimumGitMajor = 2
	minimumGitMinor = 38
)

// requireGit refuses a git older than 2.38 before any command reads a repository.
func requireGit(ctx context.Context) error {
	output, err := exec.CommandContext(ctx, "git", "--version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("git --version: %w: %s", err, strings.TrimSpace(string(output)))
	}
	major, minor, err := gitVersion(string(output))
	if err != nil {
		return err
	}
	if major < minimumGitMajor || (major == minimumGitMajor && minor < minimumGitMinor) {
		return fmt.Errorf("git %d.%d is too old: land needs git %d.%d or later, whose merge-tree writes the tree of a merge",
			major, minor, minimumGitMajor, minimumGitMinor)
	}
	return nil
}

// gitVersion reads the major and minor version from the line `git --version` prints.
func gitVersion(line string) (int, int, error) {
	fields := strings.Fields(line)
	if len(fields) < 3 || fields[0] != "git" || fields[1] != "version" {
		return 0, 0, fmt.Errorf("git --version printed %q, which names no version", strings.TrimSpace(line))
	}
	parts := strings.SplitN(fields[2], ".", 3)
	if len(parts) < 2 {
		return 0, 0, fmt.Errorf("git --version printed version %q, which has no major and minor part", fields[2])
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, fmt.Errorf("git --version printed version %q: %w", fields[2], err)
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, fmt.Errorf("git --version printed version %q: %w", fields[2], err)
	}
	return major, minor, nil
}

func runBatchCommand(ctx context.Context, command string, args []string, output io.Writer) error {
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "", "path to the repository configuration")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *configPath == "" {
		return fmt.Errorf("--config is required")
	}
	config, err := readConfig(*configPath)
	if err != nil {
		return err
	}
	if command == "serve" && strings.TrimSpace(config.Escalations.Owner) == "" {
		return fmt.Errorf("escalations.owner is required to serve: an escalation enqueued for an empty owner wakes nobody")
	}
	if command == "run" && (config.BatchID == "" || config.Base == "") {
		return fmt.Errorf("batchId and base are required to run a single batch; land serve forms its batches from the ledger's ready migrations")
	}
	program, err := config.program(ctx)
	if err != nil {
		return err
	}
	defer program.close()
	runner := program.runner
	if command != "status" {
		// Before any command runs on the VM: refuse a missing path and make the clone. serve also removes the
		// landed trees and build directories that an earlier process left behind.
		if err := program.containers.docker.Start(ctx, program.containers.declared, command == "serve"); err != nil {
			return err
		}
	}
	switch command {
	case "run":
		batch, err := runner.Run(ctx, program.input)
		if err != nil {
			return err
		}
		if batch.State != landing.Green && batch.State != landing.Landed && batch.State != landing.Failed {
			batches, err := runner.Reconcile(ctx)
			if err != nil {
				return err
			}
			for _, resumed := range batches {
				if resumed.ID == batch.ID {
					batch = resumed
					break
				}
			}
		}
		if (batch.State == landing.Green || batch.State == landing.Publishing) && runner.GitHub != nil {
			batch, err = runner.Publish(ctx, batch)
			if err != nil {
				return err
			}
		}
		return writeJSON(output, batch)
	case "status":
		batches, err := program.list(ctx)
		if err != nil {
			return err
		}
		return writeJSON(output, batches)
	case "serve":
		// Only serve listens: run and status never wait for a ready migration.
		wakes, err := ledgerstore.ReadyWakes(ctx, program.dsn, program.ledger, config.Repository.Name)
		if err != nil {
			return fmt.Errorf("listen for ready migrations: %w", err)
		}
		return serve(ctx, runner, serving{
			repository: config.Repository.Name,
			baseBranch: config.BaseBranch,
			maxMembers: config.Batch.MaxMembers,
			interval:   config.Interval,
			wakes:      wakes,
		}, output)
	default:
		return fmt.Errorf("unknown batch command %q", command)
	}
}

type config struct {
	BatchID      string              `json:"batchId"`
	Repository   repositoryConfig    `json:"repository"`
	Base         string              `json:"base"`
	BaseBranch   string              `json:"baseBranch"`
	Members      []landing.Member    `json:"members"`
	Build        []string            `json:"build"`
	Hasher       []string            `json:"hasher"`
	Checks       []checkConfig       `json:"checks"`
	Ledger       ledgerConfig        `json:"ledger"`
	Publishing   publishingConfig    `json:"publishing"`
	Toolchain    []string            `json:"toolchain"`
	Event        eventConfig         `json:"event"`
	Container    containerConfig     `json:"container"`
	PostLanding  landing.PostLanding `json:"post_landing"`
	Librarian    librarianConfig     `json:"librarian"`
	Publish      publishConfig       `json:"publish"`
	ModuleRoots  []string            `json:"moduleRoots"`
	MaxRounds    int                 `json:"maxRounds"`
	InfraRetries int                 `json:"infraRetries"`
	Interval     time.Duration       `json:"interval"`
	GitHub       githubConfig        `json:"github"`
	Escalations  escalationsConfig   `json:"escalations"`
	Effects      effectsConfig       `json:"effects"`
	Batch        batchConfig         `json:"batch"`
	Digest       string              `json:"-"`
}

// batchConfig caps how many ready migrations one batch takes. Zero is the runner's default.
type batchConfig struct {
	MaxMembers int `json:"max_members"`
}

// escalationsConfig names the owner for whom every escalation of the program is
// enqueued.
type escalationsConfig struct {
	Owner string `json:"owner"`
}

type checkConfig struct {
	Name    string   `json:"name"`
	Command []string `json:"command"`
	Format  string   `json:"format"` // findings (the default) or graph
}

// ledgerConfig names the file that holds the ledger database's connection string. The program keeps its batches in
// the ledger and makes every ledger call through its Go API on that database.
type ledgerConfig struct {
	DSNFile string `json:"dsn_file"`
}

// publishingConfig bounds the failed enqueues and waits of an admitted batch before it is withdrawn. Zero keeps the
// runner's default.
type publishingConfig struct {
	RetryLimit int `json:"retry_limit"`
}

// UnmarshalJSON refuses the removed ledger commands, with the key to set instead.
func (l *ledgerConfig) UnmarshalJSON(body []byte) error {
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(body, &keys); err != nil {
		return err
	}
	for _, removed := range []string{"check", "land", "return", "command", "status", "migrationState"} {
		if _, found := keys[removed]; found {
			return fmt.Errorf("ledger.%s is removed; set ledger.dsn_file, and the program calls the ledger directly", removed)
		}
	}
	type plain ledgerConfig
	return json.Unmarshal(body, (*plain)(l))
}

// repositoryConfig names the repository the program lands into: Name is its
// name in the ledger, Path the local checkout or bare clone the program
// reads, and GitDir the program's bare clone on the build VM (decision 14).
type repositoryConfig struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	GitDir string `json:"git_dir"`
	// FetchURL is where owners push their heads; each ready head is fetched from it by its SHA. Empty means Path.
	FetchURL string `json:"fetch_url"`
}

// UnmarshalJSON refuses the old string form of the repository, whose path was
// the separate key repositoryPath, with the shape to write instead.
func (r *repositoryConfig) UnmarshalJSON(body []byte) error {
	if trimmed := bytes.TrimSpace(body); len(trimmed) > 0 && trimmed[0] == '"' {
		return fmt.Errorf(`repository must be an object {"name", "path", "git_dir"}, not a string`)
	}
	type plain repositoryConfig
	return json.Unmarshal(body, (*plain)(r))
}

type githubConfig struct {
	Remote       string        `json:"remote"`
	Executable   string        `json:"executable"`
	PollInterval time.Duration `json:"pollInterval"`
}

// containerConfig is the container boundary every command that reads candidate or landed content runs behind: the
// SSH argv that reaches the build VM (its last word is the host), the image pinned by digest, the uid:gid of the VM
// user that owns the mounted paths, the VM directory that holds each run's inputs and scratch, and the named mounts
// a hook, a build, the hasher or a check may be given. A mount's host path may be a file (a DSN file) or a
// directory. build_root names the mount that holds the build directories and the package store, and
// packages_source the mount whose .lake/packages fills the store.
type containerConfig struct {
	SSH            []string                     `json:"ssh"`
	Image          string                       `json:"image"`
	User           string                       `json:"user"`
	RunRoot        string                       `json:"run_root"`
	BuildRoot      string                       `json:"build_root"`
	PackagesSource string                       `json:"packages_source"`
	Mounts         []mountConfig                `json:"mounts"`
	BuildMounts    map[string]landing.MountMode `json:"build_mounts"`
	HasherMounts   map[string]landing.MountMode `json:"hasher_mounts"`
	HasherNetwork  landing.Network              `json:"hasher_network"`
	CheckMounts    map[string]landing.MountMode `json:"check_mounts"`
	CheckNetwork   landing.Network              `json:"check_network"`
}

// mountConfig declares one named path of the build VM, mounted at that same path inside a container.
type mountConfig struct {
	Name     string `json:"name"`
	HostPath string `json:"host_path"`
}

// publishConfig is the argv that a hook defect is published with, for a hook that names a topic.
type publishConfig struct {
	Command []string `json:"command"`
}

// librarianConfig is the argv that each near pair of a contracts apply is delivered with.
type librarianConfig struct {
	Command []string `json:"command"`
}

// effectsConfig is the attempt threshold at which a retried effect is escalated (default 10) and the first wait
// between two of its attempts (default 10 seconds).
type effectsConfig struct {
	EscalateAfter int           `json:"escalate_after"`
	Backoff       time.Duration `json:"backoff"`
}

// container is the part of the container configuration that a frozen hook definition copies, with the declared
// mounts by name.
func (c config) container() landing.Container {
	mounts := make(map[string]landing.Mount, len(c.Container.Mounts))
	for _, mount := range c.Container.Mounts {
		mounts[mount.Name] = landing.Mount{Name: mount.Name, HostPath: mount.HostPath}
	}
	return landing.Container{Image: c.Container.Image, User: c.Container.User, GitDir: c.Repository.GitDir, Mounts: mounts}
}

// docker is the executor of every container on the build VM, and the mounts that the builds, the hasher and the
// checks get. It refuses a container section that lacks a key, a mode or network that is not one of the two, and a
// mount map that names an undeclared mount.
func (c config) docker() (containers, error) {
	k := c.Container
	if len(k.SSH) == 0 || k.Image == "" || k.User == "" || k.RunRoot == "" || k.BuildRoot == "" {
		return containers{}, fmt.Errorf("container.ssh, container.image, container.user, container.run_root, and container.build_root are required: every build, check, hasher and hook runs in a container on the build VM")
	}
	declared := c.container().Mounts
	resolve := func(key string, modes map[string]landing.MountMode) ([]landing.Mount, error) {
		var mounts []landing.Mount
		for _, name := range slices.Sorted(maps.Keys(modes)) {
			mount, found := declared[name]
			if !found {
				return nil, fmt.Errorf("container.%s names %q, which container.mounts does not declare", key, name)
			}
			if mode := modes[name]; mode != landing.ReadOnly && mode != landing.ReadWrite {
				return nil, fmt.Errorf("container.%s.%s is %q, not %q or %q", key, name, mode, landing.ReadOnly, landing.ReadWrite)
			}
			mount.Mode = modes[name]
			mounts = append(mounts, mount)
		}
		return mounts, nil
	}
	var out containers
	var err error
	if out.build, err = resolve("build_mounts", k.BuildMounts); err != nil {
		return containers{}, err
	}
	if out.hasher, err = resolve("hasher_mounts", k.HasherMounts); err != nil {
		return containers{}, err
	}
	if out.check, err = resolve("check_mounts", k.CheckMounts); err != nil {
		return containers{}, err
	}
	for key, network := range map[string]landing.Network{"hasher_network": k.HasherNetwork, "check_network": k.CheckNetwork} {
		switch network {
		case "", landing.NetworkNone, landing.NetworkHost:
		default:
			return containers{}, fmt.Errorf("container.%s %q is not %q or %q", key, network, landing.NetworkHost, landing.NetworkNone)
		}
	}
	out.checkNetwork = k.CheckNetwork
	buildRoot, found := declared[k.BuildRoot]
	if !found {
		return containers{}, fmt.Errorf("container.build_root names %q, which container.mounts does not declare", k.BuildRoot)
	}
	if mode, built := k.BuildMounts[k.BuildRoot]; !built || mode != landing.ReadWrite {
		return containers{}, fmt.Errorf("container.build_mounts must make the build root %q read-write: the builds are made inside it", k.BuildRoot)
	}
	source := ""
	if k.PackagesSource != "" {
		mount, found := declared[k.PackagesSource]
		if !found {
			return containers{}, fmt.Errorf("container.packages_source names %q, which container.mounts does not declare", k.PackagesSource)
		}
		source = mount.HostPath
	}
	out.docker = landing.Docker{
		SSH:            append([]string(nil), k.SSH...),
		Image:          k.Image,
		User:           k.User,
		GitDir:         c.Repository.GitDir,
		RepositoryPath: c.Repository.Path,
		Repository:     c.Repository.Name,
		RunRoot:        k.RunRoot,
		BuildRoot:      buildRoot.HostPath,
		PackagesSource: source,
	}
	for _, mount := range c.Container.Mounts {
		out.declared = append(out.declared, landing.Mount{Name: mount.Name, HostPath: mount.HostPath})
	}
	return out, nil
}

// containers is the configured executor and the mounts of each kind of container.
type containers struct {
	docker               landing.Docker
	build, hasher, check []landing.Mount
	checkNetwork         landing.Network
	declared             []landing.Mount
}

// builder is the build adapter of land run, land serve, land blame and land replay.
func (k containers) builder(command []string) landing.ContainerBuilder {
	return landing.ContainerBuilder{Docker: k.docker, Command: command, Mounts: k.build}
}

// checkers are the configured checks, each in a container.
func (k containers) checkers(checks []checkConfig) []landing.Checker {
	checkers := make([]landing.Checker, 0, len(checks))
	for _, check := range checks {
		checkers = append(checkers, landing.ContainerChecker{Docker: k.docker, CheckName: check.Name, Command: check.Command, Format: check.Format, Mounts: k.check, Network: k.checkNetwork})
	}
	return checkers
}

type eventConfig struct {
	Command []string `json:"command"`
	Status  []string `json:"status"`
}

func readConfig(path string) (config, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return config{}, fmt.Errorf("read configuration: %w", err)
	}
	var cfg config
	if err := json.Unmarshal(body, &cfg); err != nil {
		return config{}, fmt.Errorf("decode configuration: %w", err)
	}
	for index, check := range cfg.Checks {
		if check.Format != "" && check.Format != landing.FormatFindings && check.Format != landing.FormatGraph {
			return config{}, fmt.Errorf("checks[%d].format %q is not %q or %q", index, check.Format, landing.FormatFindings, landing.FormatGraph)
		}
	}
	digest := sha256.Sum256(body)
	cfg.Digest = hex.EncodeToString(digest[:])
	return cfg, nil
}

// checkers are the configured checks, each run in the repository path.
func (c config) checkers() []landing.Checker {
	checkers := make([]landing.Checker, 0, len(c.Checks))
	for _, check := range c.Checks {
		checkers = append(checkers, landing.CommandChecker{CheckName: check.Name, Command: check.Command, Directory: c.Repository.Path, Format: check.Format})
	}
	return checkers
}

// program is the wiring of one configuration: the runner, the configured batch, how the status command lists
// batches, and what to release at the end. The merge queue's required landing/merge-group status comes from the
// repository's GitHub Actions workflow, not from this program.
type program struct {
	runner     landing.Runner
	containers containers
	input      landing.Input
	list       func(context.Context) ([]landing.Batch, error)
	close      func()
	// ledger and dsn are the open ledger and its database, from which `land serve` opens its wake source.
	ledger *ledger.Store
	dsn    string
}

// program wires one configuration. It needs no configured batch: `land serve` has none, and forms each batch from
// the ledger's ready migrations on the current tip of the base branch. `land run`, which keeps its fixed member
// list for a single batch, checks its own keys before it calls this.
func (c config) program(ctx context.Context) (program, error) {
	if c.Repository.Name == "" || c.Repository.Path == "" || c.Repository.GitDir == "" || c.BaseBranch == "" {
		return program{}, fmt.Errorf("repository.name, repository.path, repository.git_dir, and baseBranch are required")
	}
	if c.Ledger.DSNFile == "" {
		return program{}, fmt.Errorf("ledger.dsn_file is required: the batches are kept in the ledger")
	}
	if len(c.Build) == 0 || len(c.Hasher) == 0 || len(c.Toolchain) == 0 {
		return program{}, fmt.Errorf("build, hasher, and toolchain are required")
	}
	if strings.TrimSpace(c.Escalations.Owner) == "" {
		return program{}, fmt.Errorf("escalations.owner is required: the ledger enqueues every escalation for it")
	}
	publishing := c.GitHub.Remote != "" || c.GitHub.Executable != ""
	if c.Publishing.RetryLimit < 0 {
		return program{}, fmt.Errorf("publishing.retry_limit must not be negative")
	}
	path := c.Repository.Path
	out := program{
		input: landing.Input{ID: c.BatchID, Repository: c.Repository.Name, Base: c.Base, BaseBranch: c.BaseBranch, Members: c.Members},
		close: func() {},
	}
	container := c.container()
	if err := c.PostLanding.Validate(container.Mounts); err != nil {
		return program{}, err
	}
	if len(c.PostLanding.Hooks) != 0 && strings.TrimSpace(c.Escalations.Owner) == "" {
		return program{}, fmt.Errorf("escalations.owner is required: the ledger enqueues every escalation for it")
	}
	containers, err := c.docker()
	if err != nil {
		return program{}, err
	}
	out.containers = containers
	git := landing.RepositoryGit{Path: path, FetchURL: c.Repository.FetchURL}
	runner := landing.Runner{
		Builder:              containers.builder(c.Build),
		Hasher:               landing.ContainerHasher{Docker: containers.docker, Command: c.Hasher, Mounts: containers.hasher, Network: c.Container.HasherNetwork},
		Containers:           containers.docker,
		Trees:                containers.docker,
		Git:                  git,
		Heads:                git,
		ModuleRoots:          append([]string(nil), c.ModuleRoots...),
		MaxRounds:            c.MaxRounds,
		InfraRetries:         c.InfraRetries,
		ConfigDigest:         c.Digest,
		PublishingRetryLimit: c.Publishing.RetryLimit,
		EscalateTo:           strings.TrimSpace(c.Escalations.Owner),
		EscalateAfter:        c.Effects.EscalateAfter,
		RetryBackoff:         c.Effects.Backoff,
		PostLanding:          c.PostLanding,
		Container:            container,
		Librarian:            c.Librarian.Command,
		Scores:               landing.RepositoryScore{Path: path},
		Toolchain:            commandToolchain(path, c.Toolchain),
	}
	runner.Checks = containers.checkers(c.Checks)
	if len(c.Publish.Command) != 0 {
		runner.Defects = landing.CommandDefects{Directory: path, Command: c.Publish.Command}
	}
	if publishing {
		github := landing.GitHubCLI{RepositoryPath: path, Remote: c.GitHub.Remote, Executable: c.GitHub.Executable, PollInterval: c.GitHub.PollInterval}
		runner.GitHub = github
		// The member event is optional: without it no member event runs, and the ledger's migration.landed is the
		// record. A partial one is refused. The score is a post-landing hook like any other.
		if event := c.Event; len(event.Command) != 0 || len(event.Status) != 0 {
			if len(event.Command) == 0 || len(event.Status) == 0 {
				return program{}, fmt.Errorf("event needs command and status")
			}
			runner.Events = landing.CommandEvents{Directory: path, Command: event.Command, StatusCommand: event.Status}
		}
	}
	body, err := os.ReadFile(c.Ledger.DSNFile)
	if err != nil {
		return program{}, fmt.Errorf("read ledger.dsn_file: %w", err)
	}
	dsn := strings.TrimSpace(string(body))
	if dsn == "" {
		return program{}, fmt.Errorf("ledger.dsn_file %s is empty", c.Ledger.DSNFile)
	}
	store, err := ledger.Open(ctx, dsn)
	if err != nil {
		return program{}, fmt.Errorf("open the ledger: %w", err)
	}
	batches := ledgerstore.New(store, c.Repository.Name)
	calls := ledgerstore.NewLedger(store, c.Repository.Name)
	runner.Store = batches
	runner.Ledger = calls
	runner.Admission = calls
	runner.Effects = calls
	out.list = batches.ListActive
	out.close = store.Close
	out.ledger, out.dsn = store, dsn
	out.runner = runner
	return out, nil
}

func commandToolchain(directory string, command []string) func(context.Context, landing.Tree) (string, error) {
	if len(command) == 0 {
		return nil
	}
	return func(ctx context.Context, tree landing.Tree) (string, error) {
		process := exec.CommandContext(ctx, command[0], command[1:]...)
		process.Dir = directory
		process.Env = append(os.Environ(), "LAND_TREE="+string(tree))
		output, err := process.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("run toolchain command: %w: %s", err, strings.TrimSpace(string(output)))
		}
		return string(output), nil
	}
}

// serving is what the serve loop forms each batch from: the repository, the branch it lands on and the cap on a
// batch's members; and what starts its next round: a wake when an owner readies a migration, or else the interval,
// so a wake that is lost delays a round by at most one interval.
type serving struct {
	repository string
	baseBranch string
	maxMembers int
	interval   time.Duration
	// wakes delivers a wake for each ready submission (ledgerstore.ReadyWakes). Nil, or closed, leaves the interval.
	wakes <-chan struct{}
}

// serve reconciles and publishes until ctx ends. With no batch left to do, it forms the next one from the ledger's
// ready migrations, on the current tip of the base branch; a round with no ready migration does nothing and waits.
// The next round starts when an owner readies a migration (a wake) or when the interval passes, whichever is first.
// A wake that arrives during a round is kept for the next one; the round itself reads the ready list, so a wake
// carries no member. A StopError (an unclassified ledger refusal) stops it, and the batch stays for an operator.
func serve(ctx context.Context, runner landing.Runner, configured serving, output io.Writer) error {
	interval := configured.interval
	if interval <= 0 {
		interval = 10 * time.Second
	}
	wakes := configured.wakes
	for {
		// This round reads the ready list as it is now, so every wake already pending is answered by it.
		wakes = drain(wakes)
		batches, err := runner.Reconcile(ctx)
		if err != nil {
			return err
		}
		if len(batches) == 0 {
			input, err := runner.ReadyInput(ctx, configured.repository, configured.baseBranch, configured.maxMembers)
			switch {
			case errors.Is(err, landing.ErrNoReadyMembers):
			case err != nil:
				return err
			default:
				batch, err := runner.Run(ctx, input)
				if err != nil {
					return err
				}
				batches = []landing.Batch{batch}
			}
		}
		for index := range batches {
			if (batches[index].State == landing.Green || batches[index].State == landing.Publishing) && runner.GitHub != nil {
				batch, err := runner.Publish(ctx, batches[index])
				if err != nil {
					var stop *landing.StopError
					retry := batch.State == landing.Publishing || errors.Is(err, landing.ErrEffectsPending)
					if errors.As(err, &stop) || !retry {
						return err
					}
					batches[index] = batch
					continue
				}
				batches[index] = batch
			}
		}
		if err := writeJSON(output, batches); err != nil {
			return err
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		case _, open := <-wakes:
			timer.Stop()
			if !open {
				// The wake source ended (its connection failed): the interval alone starts the rounds from now on.
				wakes = nil
			}
		}
	}
}

// drain takes every wake already pending and returns the wake source, or nil once it has ended.
func drain(wakes <-chan struct{}) <-chan struct{} {
	for {
		select {
		case _, open := <-wakes:
			if !open {
				return nil
			}
		default:
			return wakes
		}
	}
}

func writeJSON(output io.Writer, value any) error {
	if err := json.NewEncoder(output).Encode(value); err != nil {
		return fmt.Errorf("write output: %w", err)
	}
	return nil
}

func runBlame(ctx context.Context, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("blame", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	repository := flags.String("repo", "", "repository path")
	base := flags.String("base", "", "base commit")
	membersFile := flags.String("members", "", "member TSV")
	excludeFile := flags.String("exclude", "", "member identifiers to exclude")
	candidateRef := flags.String("candidate", "", "recorded candidate commit or tree")
	logFile := flags.String("log", "", "build log")
	confirm := flags.String("confirm-with", "", "confirmation build command, run on this host")
	configPath := flags.String("config", "", "land configuration whose build confirms, in a container on the build VM")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *repository == "" || *base == "" || *membersFile == "" || *logFile == "" {
		return fmt.Errorf("--repo, --base, --members, and --log are required")
	}
	if *confirm != "" && *configPath != "" {
		return fmt.Errorf("--confirm-with and --config are two ways to confirm; give one")
	}
	var confirmer blame.Builder
	switch {
	case *confirm != "":
		confirmer = archiveBuilder{repository: *repository, command: *confirm}
	case *configPath != "":
		cfg, err := readConfig(*configPath)
		if err != nil {
			return err
		}
		if len(cfg.Build) == 0 {
			return fmt.Errorf("--config: the configuration has no build")
		}
		// The trees come from --repo, so the pushes to the program's clone do too.
		cfg.Repository.Path = *repository
		containers, err := cfg.docker()
		if err != nil {
			return err
		}
		if err := containers.docker.Start(ctx, containers.declared, false); err != nil {
			return err
		}
		confirmer = containers.builder(cfg.Build)
	}
	members, err := readMembers(*membersFile)
	if err != nil {
		return err
	}
	if *excludeFile != "" {
		excluded, err := readExcluded(*excludeFile)
		if err != nil {
			return err
		}
		members = withoutExcluded(members, excluded)
	}
	log, err := os.ReadFile(*logFile)
	if err != nil {
		return fmt.Errorf("read build log: %w", err)
	}
	report := blame.Parse(string(log), nil)
	var candidateTree blame.Tree
	if *candidateRef != "" {
		tree, err := git(ctx, *repository, "rev-parse", "--verify", *candidateRef+"^{tree}")
		if err != nil {
			return fmt.Errorf("resolve candidate tree: %w", err)
		}
		candidateTree = blame.Tree(tree)
	} else {
		inputs := make([]gitx.Member, len(members))
		for index, member := range members {
			inputs[index] = gitx.Member{ID: member.ID, Head: member.Head}
		}
		candidate, err := gitx.Candidate(ctx, *repository, *base, inputs)
		if err != nil {
			return fmt.Errorf("build blame candidate: %w", err)
		}
		candidateTree = blame.Tree(candidate.Tree)
	}
	// The trees are scanned first: their candidate names the generated
	// instance names that no declaration may keep, and each member's
	// evidence must use the same set as the trees.
	lines, ambiguous, _, err := collectTreeLines(ctx, *repository, blame.Tree(*base), candidateTree, report)
	if err != nil {
		return err
	}
	evidence, err := collectEvidence(ctx, *repository, *base, members, ambiguous)
	if err != nil {
		return err
	}
	attributions := blame.IdentifyDetailed(report, evidence, lines)
	if confirmer != nil {
		attributions, err = confirmAttributions(ctx, *repository, *base, members, report.RedModules, attributions, confirmer)
		if err != nil {
			return err
		}
	}
	for _, attribution := range attributions {
		if _, err := fmt.Fprintf(output, "%s\t%s\t%s\t%s\t%d\t%s\n", attribution.Member, attribution.Module, attribution.Name, attribution.Kind, attribution.Strength, attribution.Evidence); err != nil {
			return fmt.Errorf("write output: %w", err)
		}
	}
	return nil
}

func readMembers(path string) ([]blame.Member, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read members: %w", err)
	}
	var members []blame.Member
	for lineNumber, line := range strings.Split(string(body), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 2 || fields[0] == "" || fields[1] == "" {
			return nil, fmt.Errorf("members line %d is not member and head", lineNumber+1)
		}
		members = append(members, blame.Member{ID: fields[0], Head: fields[1]})
	}
	return members, nil
}

func readExcluded(path string) (map[string]struct{}, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read excluded members: %w", err)
	}
	excluded := make(map[string]struct{})
	for _, line := range strings.Split(string(body), "\n") {
		member := strings.TrimSpace(line)
		if member != "" && !strings.HasPrefix(member, "#") {
			excluded[member] = struct{}{}
		}
	}
	return excluded, nil
}

func withoutExcluded(members []blame.Member, excluded map[string]struct{}) []blame.Member {
	out := members[:0]
	for _, member := range members {
		if _, found := excluded[member.ID]; !found {
			out = append(out, member)
		}
	}
	return out
}

func collectEvidence(ctx context.Context, repository, base string, members []blame.Member, ambiguous blame.InstanceNames) (map[string]blame.Evidence, error) {
	evidence := make(map[string]blame.Evidence, len(members))
	git := landing.RepositoryGit{}
	for _, member := range members {
		memberEvidence, err := git.DeclarationEvidence(ctx, repository, base, landing.Member{Migration: member.ID, Head: member.Head}, ambiguous)
		if err != nil {
			return nil, fmt.Errorf("read declaration evidence for %q: %w", member.ID, err)
		}
		evidence[member.ID] = memberEvidence
	}
	return evidence, nil
}

// collectTreeLines groups, by each name the report mentions, the declarations
// of the base and candidate trees that carry it. It also returns the round's
// ambiguous generated instance names, which it has resolved in both trees and
// which every scan of the round must use, and the qualified names of the
// fields that a structure or class of either tree declares.
func collectTreeLines(ctx context.Context, repository string, base, candidate blame.Tree, report blame.Report) (map[string]blame.TreeLines, blame.InstanceNames, map[string]struct{}, error) {
	git := landing.RepositoryGit{}
	baseDeclarations, err := git.TreeDeclarations(ctx, repository, landing.Tree(base))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read declarations at base: %w", err)
	}
	candidateDeclarations, err := git.TreeDeclarations(ctx, repository, landing.Tree(candidate))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read declarations at candidate: %w", err)
	}
	ambiguous := blame.AmbiguousInstanceNames(candidateDeclarations)
	blame.ResolveInstanceNames(candidateDeclarations, ambiguous)
	blame.ResolveInstanceNames(baseDeclarations, ambiguous)
	names := make(map[string]struct{})
	for _, block := range report.Blocks {
		for _, name := range blame.Names(block.Message) {
			names[name.Value] = struct{}{}
		}
	}
	declarations := make(map[string]blame.TreeLines, len(names))
	for name := range names {
		declarations[name] = blame.TreeLines{
			Base:      blame.DeclarationsFor(name, baseDeclarations),
			Candidate: blame.DeclarationsFor(name, candidateDeclarations),
		}
	}
	fields := make(map[string]struct{})
	for _, tree := range [][]blame.Declaration{baseDeclarations, candidateDeclarations} {
		for _, declaration := range tree {
			for _, field := range declaration.Fields {
				fields[declaration.Name+"."+field] = struct{}{}
			}
		}
	}
	return declarations, ambiguous, fields, nil
}

func confirmAttributions(ctx context.Context, repository, base string, members []blame.Member, modules []string, attributions []blame.Attribution, builder blame.Builder) ([]blame.Attribution, error) {
	suspects := make(map[string][]string)
	for _, attribution := range attributions {
		suspects[attribution.Module] = append(suspects[attribution.Module], attribution.Member)
	}
	candidate := func(ctx context.Context, base string, selected []blame.Member) (blame.Tree, error) {
		inputs := make([]gitx.Member, len(selected))
		for index, member := range selected {
			inputs[index] = gitx.Member{ID: member.ID, Head: member.Head}
		}
		result, err := gitx.Candidate(ctx, repository, base, inputs)
		if err != nil {
			return "", err
		}
		return blame.Tree(result.Tree), nil
	}
	confirmation, err := blame.ConfirmModules(ctx, base, modules, members, suspects, candidate, builder)
	if err != nil {
		return nil, err
	}
	index := make(map[string]blame.Attribution)
	for _, attribution := range attributions {
		index[attribution.Module+"\x00"+attribution.Member] = attribution
	}
	var out []blame.Attribution
	for module, culprits := range confirmation.ByModule {
		for _, culprit := range culprits {
			if attribution, found := index[module+"\x00"+culprit.Member]; found && culprit.Kind == "evidence" {
				out = append(out, attribution)
				continue
			}
			out = append(out, blame.Attribution{Module: module, Name: "<bisect>", Kind: blame.Changed, Member: culprit.Member, Evidence: culprit.Kind})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Member != out[j].Member {
			return out[i].Member < out[j].Member
		}
		return out[i].Module < out[j].Module
	})
	return out, nil
}

func firstReason(report blame.Report) string {
	for _, block := range report.Blocks {
		names := blame.Names(block.Message)
		if len(names) != 0 {
			return "ident:" + names[0].Value
		}
	}
	return "bisect"
}

type archiveBuilder struct {
	repository string
	command    string
}

func (b archiveBuilder) Build(ctx context.Context, tree blame.Tree, modules []string) (blame.BuildResult, error) {
	archive, err := exec.CommandContext(ctx, "git", "-C", b.repository, "archive", "--format=tar", string(tree)).Output()
	if err != nil {
		return blame.BuildResult{}, fmt.Errorf("archive tree: %w", err)
	}
	directory, err := os.MkdirTemp("", "land-build-")
	if err != nil {
		return blame.BuildResult{}, fmt.Errorf("create build directory: %w", err)
	}
	defer os.RemoveAll(directory)
	if err := extractTar(directory, archive); err != nil {
		return blame.BuildResult{}, err
	}
	cmd := exec.CommandContext(ctx, "sh", "-c", b.command)
	cmd.Dir = directory
	cmd.Env = append(os.Environ(), "LAND_TREE="+string(tree), "LAND_MODULES="+strings.Join(modules, " "))
	output, err := cmd.CombinedOutput()
	if err != nil {
		return blame.BuildResult{Green: false, Output: string(output), InfraClass: archiveInfraClass(ctx, err)}, nil
	}
	return blame.BuildResult{Green: true, Output: string(output)}, nil
}

func archiveInfraClass(ctx context.Context, err error) string {
	if ctx.Err() != nil {
		return "timeout"
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return "launch"
	}
	code := exit.ExitCode()
	switch code {
	case -1:
		return "killed"
	case 124:
		return "timeout"
	case 125, 137:
		return "resource-kill"
	default:
		if code > 128 {
			return "signal"
		}
		return ""
	}
}

func extractTar(directory string, archive []byte) error {
	reader := tar.NewReader(bytes.NewReader(archive))
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read archive: %w", err)
		}
		path := filepath.Join(directory, header.Name)
		if !strings.HasPrefix(path, directory+string(os.PathSeparator)) {
			return fmt.Errorf("archive path escapes destination")
		}
		switch header.Typeflag {
		case tar.TypeXGlobalHeader, tar.TypeXHeader:
			continue
		case tar.TypeDir:
			if err := os.MkdirAll(path, os.FileMode(header.Mode)); err != nil {
				return fmt.Errorf("create archive directory: %w", err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return fmt.Errorf("create archive parent: %w", err)
			}
			file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(header.Mode))
			if err != nil {
				return fmt.Errorf("create archive file: %w", err)
			}
			_, copyErr := io.Copy(file, reader)
			closeErr := file.Close()
			if copyErr != nil {
				return fmt.Errorf("write archive file: %w", copyErr)
			}
			if closeErr != nil {
				return fmt.Errorf("close archive file: %w", closeErr)
			}
		case tar.TypeSymlink:
			if filepath.IsAbs(header.Linkname) {
				return fmt.Errorf("archive has absolute symlink %q", header.Name)
			}
			target := filepath.Clean(filepath.Join(filepath.Dir(path), header.Linkname))
			if !strings.HasPrefix(target, directory+string(os.PathSeparator)) {
				return fmt.Errorf("archive symlink escapes destination")
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return fmt.Errorf("create archive symlink parent: %w", err)
			}
			if err := os.Symlink(header.Linkname, path); err != nil {
				return fmt.Errorf("create archive symlink: %w", err)
			}
		default:
			return fmt.Errorf("archive has unsupported entry %q", header.Name)
		}
	}
}

func git(ctx context.Context, repository string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", append([]string{"-C", repository}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}
