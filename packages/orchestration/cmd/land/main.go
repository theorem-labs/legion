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
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, output io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: land <run|serve|status|blame>")
	}
	switch args[0] {
	case "blame":
		return runBlame(ctx, args[1:], output)
	case "run", "serve", "status":
		return runBatchCommand(ctx, args[0], args[1:], output)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
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
	program, err := config.program(ctx)
	if err != nil {
		return err
	}
	defer program.close()
	runner := program.runner
	if command != "status" && program.queue != nil {
		attestCtx, stopAttesting := context.WithCancel(ctx)
		defer stopAttesting()
		go attestMergeGroups(attestCtx, runner, program.queue, program.input.Repository, program.input.BaseBranch, config.GitHub.PollInterval)
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
		return serve(ctx, runner, program.input, config.Interval, output)
	default:
		return fmt.Errorf("unknown batch command %q", command)
	}
}

// attestMergeGroups sets the merge-group status on the base branch's merge groups until ctx ends. It runs beside the
// publisher, which waits for the queue's outcome, because the queue merges a group only after its status is set. A
// failed round is tried again at the next tick.
func attestMergeGroups(ctx context.Context, runner landing.Runner, queue landing.MergeQueue, repository, branch string, interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	posted := make(map[string]string)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := runner.AttestMergeGroups(ctx, queue, repository, branch, posted); err != nil && ctx.Err() == nil {
			fmt.Fprintln(os.Stderr, "land: merge-group status:", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

type config struct {
	BatchID      string              `json:"batchId"`
	Repository   repositoryConfig    `json:"repository"`
	Base         string              `json:"base"`
	BaseBranch   string              `json:"baseBranch"`
	StatePath    string              `json:"statePath"`
	Members      []landing.Member    `json:"members"`
	Build        []string            `json:"build"`
	Hasher       []string            `json:"hasher"`
	Checks       []checkConfig       `json:"checks"`
	Ledger       ledgerConfig        `json:"ledger"`
	Escalations  escalationsConfig   `json:"escalations"`
	Publishing   publishingConfig    `json:"publishing"`
	Toolchain    []string            `json:"toolchain"`
	Event        eventConfig         `json:"event"`
	Container    containerConfig     `json:"container"`
	PostLanding  landing.PostLanding `json:"post_landing"`
	Librarian    librarianConfig     `json:"librarian"`
	Publish      publishConfig       `json:"publish"`
	Effects      effectsConfig       `json:"effects"`
	ModuleRoots  []string            `json:"moduleRoots"`
	MaxRounds    int                 `json:"maxRounds"`
	InfraRetries int                 `json:"infraRetries"`
	Interval     time.Duration       `json:"interval"`
	GitHub       githubConfig        `json:"github"`
	Digest       string              `json:"-"`
}

type checkConfig struct {
	Name    string   `json:"name"`
	Command []string `json:"command"`
}
type ledgerConfig struct {
	// DSNFile is a file that holds the ledger database's connection string. When it is set, the batches are kept in
	// the ledger and the batch calls go to it, in place of the state file.
	DSNFile        string   `json:"dsn_file"`
	Check          []string `json:"check"`
	Land           []string `json:"land"`
	Return         []string `json:"return"`
	Status         []string `json:"status"`
	MigrationState []string `json:"migrationState"`
}

// escalationsConfig names the owner for whom every escalation of this program is enqueued.
type escalationsConfig struct {
	Owner string `json:"owner"`
}

// publishingConfig bounds the failed enqueues and waits of an admitted batch before it is withdrawn. Zero keeps the
// runner's default.
type publishingConfig struct {
	RetryLimit int `json:"retry_limit"`
}

// repositoryConfig names the repository the program lands into: Name is its
// name in the ledger, Path the local checkout or bare clone the program
// reads, and GitDir the program's bare clone on the build VM (decision 14).
type repositoryConfig struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	GitDir string `json:"git_dir"`
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
// image pinned by digest, the uid:gid of the VM user that owns the mounted paths, and the named mounts a hook, a
// build or the hasher may be given.
type containerConfig struct {
	Image  string        `json:"image"`
	User   string        `json:"user"`
	Mounts []mountConfig `json:"mounts"`
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
	digest := sha256.Sum256(body)
	cfg.Digest = hex.EncodeToString(digest[:])
	return cfg, nil
}

// program is the wiring of one configuration: the runner, the configured batch, how the status command lists
// batches, the merge queue whose groups get the merge-group status (nil without GitHub), and what to release at the
// end.
type program struct {
	runner landing.Runner
	input  landing.Input
	list   func(context.Context) ([]landing.Batch, error)
	queue  landing.MergeQueue
	close  func()
}

func (c config) program(ctx context.Context) (program, error) {
	if c.BatchID == "" {
		return program{}, fmt.Errorf("batchId is required")
	}
	if c.Repository.Name == "" || c.Repository.Path == "" || c.Repository.GitDir == "" || c.Base == "" || c.BaseBranch == "" {
		return program{}, fmt.Errorf("repository.name, repository.path, repository.git_dir, base, and baseBranch are required")
	}
	if c.Ledger.DSNFile == "" && c.StatePath == "" {
		return program{}, fmt.Errorf("ledger.dsn_file or statePath is required")
	}
	if len(c.Build) == 0 || len(c.Hasher) == 0 || len(c.Toolchain) == 0 || len(c.Ledger.Check) == 0 || len(c.Ledger.Land) == 0 || len(c.Ledger.Return) == 0 || len(c.Ledger.Status) == 0 || len(c.Ledger.MigrationState) == 0 {
		return program{}, fmt.Errorf("build, hasher, toolchain, and every ledger command including state are required")
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
	runner := landing.Runner{
		Ledger:               landing.CommandLedger{Directory: path, CheckCommand: c.Ledger.Check, LandCommand: c.Ledger.Land, ReturnCommand: c.Ledger.Return, StatusCommand: c.Ledger.Status, MigrationStateCommand: c.Ledger.MigrationState},
		Builder:              landing.CommandBuilder{RepositoryPath: path, Command: c.Build},
		Hasher:               landing.CommandHasher{Command: c.Hasher, Directory: path},
		Git:                  landing.RepositoryGit{Path: path},
		ModuleRoots:          append([]string(nil), c.ModuleRoots...),
		MaxRounds:            c.MaxRounds,
		InfraRetries:         c.InfraRetries,
		ConfigDigest:         c.Digest,
		PublishingRetryLimit: c.Publishing.RetryLimit,
		EscalationsOwner:     strings.TrimSpace(c.Escalations.Owner),
		EscalateAfter:        c.Effects.EscalateAfter,
		RetryBackoff:         c.Effects.Backoff,
		PostLanding:          c.PostLanding,
		Container:            container,
		Librarian:            c.Librarian.Command,
		Scores:               landing.RepositoryScore{Path: path},
		Toolchain:            commandToolchain(path, c.Toolchain),
	}
	for _, check := range c.Checks {
		runner.Checks = append(runner.Checks, landing.CommandChecker{CheckName: check.Name, Command: check.Command, Directory: path})
	}
	if len(c.Publish.Command) != 0 {
		runner.Defects = landing.CommandDefects{Directory: path, Command: c.Publish.Command}
	}
	if publishing {
		github := landing.GitHubCLI{RepositoryPath: path, Remote: c.GitHub.Remote, Executable: c.GitHub.Executable, PollInterval: c.GitHub.PollInterval}
		runner.GitHub = github
		out.queue = github
		// The member event is optional: without it no member event runs, and the ledger's migration.landed is the
		// record. A partial one is refused. The score is a post-landing hook like any other.
		if event := c.Event; len(event.Command) != 0 || len(event.Status) != 0 {
			if len(event.Command) == 0 || len(event.Status) == 0 {
				return program{}, fmt.Errorf("event needs command and status")
			}
			runner.Events = landing.CommandEvents{Directory: path, Command: event.Command, StatusCommand: event.Status}
		}
	}
	if publishing && c.Ledger.DSNFile == "" {
		return program{}, fmt.Errorf("github needs ledger.dsn_file: a batch is admitted in the ledger before its pull request is queued")
	}
	if c.Ledger.DSNFile != "" {
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
		runner.Admission = calls
		runner.Effects = calls
		out.list = batches.ListActive
		out.close = store.Close
	} else {
		state := &landing.JSONStore{Path: c.StatePath}
		runner.Store = state
		out.list = state.List
	}
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

// serve reconciles and publishes until ctx ends. A StopError (an unclassified ledger refusal) stops it, and the batch
// stays for an operator.
func serve(ctx context.Context, runner landing.Runner, input landing.Input, interval time.Duration, output io.Writer) error {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	for {
		batches, err := runner.Reconcile(ctx)
		if err != nil {
			return err
		}
		if len(batches) == 0 {
			batch, err := runner.Run(ctx, input)
			if err != nil {
				return err
			}
			batches = []landing.Batch{batch}
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
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
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
	confirm := flags.String("confirm-with", "", "builder command")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *repository == "" || *base == "" || *membersFile == "" || *logFile == "" {
		return fmt.Errorf("--repo, --base, --members, and --log are required")
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
	evidence, err := collectEvidence(ctx, *repository, *base, members)
	if err != nil {
		return err
	}
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
	lines, err := collectTreeLines(ctx, *repository, blame.Tree(*base), candidateTree, report)
	if err != nil {
		return err
	}
	attributions := blame.IdentifyDetailed(report, evidence, lines)
	if *confirm != "" {
		attributions, err = confirmAttributions(ctx, *repository, *base, members, report.RedModules, attributions, *confirm)
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

func collectEvidence(ctx context.Context, repository, base string, members []blame.Member) (map[string]blame.Evidence, error) {
	evidence := make(map[string]blame.Evidence, len(members))
	git := landing.RepositoryGit{}
	for _, member := range members {
		memberEvidence, err := git.DeclarationEvidence(ctx, repository, base, landing.Member{Migration: member.ID, Head: member.Head})
		if err != nil {
			return nil, fmt.Errorf("read declaration evidence for %q: %w", member.ID, err)
		}
		evidence[member.ID] = memberEvidence
	}
	return evidence, nil
}

func collectTreeLines(ctx context.Context, repository string, base, candidate blame.Tree, report blame.Report) (map[string]blame.TreeDeclarations, error) {
	git := landing.RepositoryGit{}
	baseDeclarations, err := git.TreeDeclarations(ctx, repository, landing.Tree(base))
	if err != nil {
		return nil, fmt.Errorf("read declarations at base: %w", err)
	}
	candidateDeclarations, err := git.TreeDeclarations(ctx, repository, landing.Tree(candidate))
	if err != nil {
		return nil, fmt.Errorf("read declarations at candidate: %w", err)
	}
	names := make(map[string]struct{})
	for _, block := range report.Blocks {
		for _, name := range blame.Names(block.Message) {
			names[name.Value] = struct{}{}
		}
	}
	declarations := make(map[string]blame.TreeDeclarations, len(names))
	for name := range names {
		declarations[name] = blame.TreeDeclarations{
			Base:      blame.DeclarationsFor(name, baseDeclarations),
			Candidate: blame.DeclarationsFor(name, candidateDeclarations),
		}
	}
	return declarations, nil
}

func confirmAttributions(ctx context.Context, repository, base string, members []blame.Member, modules []string, attributions []blame.Attribution, command string) ([]blame.Attribution, error) {
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
	confirmation, err := blame.ConfirmModules(ctx, base, modules, members, suspects, candidate, archiveBuilder{repository: repository, command: command})
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

func confirmCulprits(ctx context.Context, repository, base string, members []blame.Member, modules, named []string, command string) ([]string, error) {
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
	result, err := blame.Confirm(ctx, base, modules, members, named, candidate, archiveBuilder{repository: repository, command: command})
	if err != nil {
		return nil, err
	}
	return append(result.Confirmed, result.Bisected...), nil
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
