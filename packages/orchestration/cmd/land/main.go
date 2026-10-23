package main

import (
	"archive/tar"
	"bytes"
	"context"
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
	runner, input, store, err := config.runner()
	if err != nil {
		return err
	}
	switch command {
	case "run":
		batch, err := runner.Run(ctx, input)
		if err != nil {
			return err
		}
		return writeJSON(output, batch)
	case "status":
		batches, err := store.List(ctx)
		if err != nil {
			return err
		}
		return writeJSON(output, batches)
	case "serve":
		return serve(ctx, runner, input, store, config.Interval, output)
	default:
		return fmt.Errorf("unknown batch command %q", command)
	}
}

type config struct {
	BatchID        string           `json:"batchId"`
	Repository     string           `json:"repository"`
	RepositoryPath string           `json:"repositoryPath"`
	Base           string           `json:"base"`
	BaseBranch     string           `json:"baseBranch"`
	StatePath      string           `json:"statePath"`
	Members        []landing.Member `json:"members"`
	Build          []string         `json:"build"`
	Checks         []checkConfig    `json:"checks"`
	Ledger         ledgerConfig     `json:"ledger"`
	Toolchain      []string         `json:"toolchain"`
	ModuleRoots    []string         `json:"moduleRoots"`
	MaxRounds      int              `json:"maxRounds"`
	InfraRetries   int              `json:"infraRetries"`
	Interval       time.Duration    `json:"interval"`
	GitHub         githubConfig     `json:"github"`
}

type checkConfig struct {
	Name    string   `json:"name"`
	Command []string `json:"command"`
}

type ledgerConfig struct {
	Check  []string `json:"check"`
	Land   []string `json:"land"`
	Return []string `json:"return"`
}

type githubConfig struct {
	Remote       string        `json:"remote"`
	Executable   string        `json:"executable"`
	PollInterval time.Duration `json:"pollInterval"`
}

func readConfig(path string) (config, error) {
	file, err := os.Open(path)
	if err != nil {
		return config{}, fmt.Errorf("open configuration: %w", err)
	}
	defer file.Close()
	var cfg config
	if err := json.NewDecoder(file).Decode(&cfg); err != nil {
		return config{}, fmt.Errorf("decode configuration: %w", err)
	}
	return cfg, nil
}

func (c config) runner() (landing.Runner, landing.Input, *landing.JSONStore, error) {
	if c.BatchID == "" {
		c.BatchID = "batch"
	}
	if c.Repository == "" || c.RepositoryPath == "" || c.Base == "" || c.BaseBranch == "" || c.StatePath == "" {
		return landing.Runner{}, landing.Input{}, nil, fmt.Errorf("repository, repositoryPath, base, baseBranch, and statePath are required")
	}
	if len(c.Build) == 0 || len(c.Ledger.Check) == 0 || len(c.Ledger.Land) == 0 || len(c.Ledger.Return) == 0 {
		return landing.Runner{}, landing.Input{}, nil, fmt.Errorf("build and every ledger command are required")
	}
	store := &landing.JSONStore{Path: c.StatePath}
	runner := landing.Runner{
		Store:        store,
		Ledger:       landing.CommandLedger{Directory: c.RepositoryPath, CheckCommand: c.Ledger.Check, LandCommand: c.Ledger.Land, ReturnCommand: c.Ledger.Return},
		Builder:      landing.CommandBuilder{RepositoryPath: c.RepositoryPath, Command: c.Build},
		Hasher:       landing.CommandHasher{RepositoryPath: c.RepositoryPath},
		Git:          landing.RepositoryGit{Path: c.RepositoryPath},
		ModuleRoots:  append([]string(nil), c.ModuleRoots...),
		MaxRounds:    c.MaxRounds,
		InfraRetries: c.InfraRetries,
		Toolchain:    commandToolchain(c.RepositoryPath, c.Toolchain),
	}
	for _, check := range c.Checks {
		runner.Checks = append(runner.Checks, landing.CommandChecker{CheckName: check.Name, Command: check.Command, Directory: c.RepositoryPath})
	}
	if c.GitHub.Remote != "" || c.GitHub.Executable != "" {
		runner.GitHub = landing.GitHubCLI{RepositoryPath: c.RepositoryPath, Remote: c.GitHub.Remote, Executable: c.GitHub.Executable, PollInterval: c.GitHub.PollInterval}
	}
	return runner, landing.Input{ID: c.BatchID, Repository: c.Repository, Base: c.Base, BaseBranch: c.BaseBranch, Members: c.Members}, store, nil
}

func commandToolchain(directory string, command []string) func(context.Context) (string, error) {
	if len(command) == 0 {
		return nil
	}
	return func(ctx context.Context) (string, error) {
		process := exec.CommandContext(ctx, command[0], command[1:]...)
		process.Dir = directory
		output, err := process.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("run toolchain command: %w: %s", err, strings.TrimSpace(string(output)))
		}
		return string(output), nil
	}
}

func serve(ctx context.Context, runner landing.Runner, input landing.Input, store *landing.JSONStore, interval time.Duration, output io.Writer) error {
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
			if batches[index].State == landing.Green && runner.GitHub != nil {
				batch, err := runner.Publish(ctx, batches[index])
				if err != nil {
					return err
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
	culprits := blame.Identify(report, evidence, nil, nil)
	reasons := make(map[string]string, len(culprits))
	for _, member := range culprits {
		reasons[member] = firstReason(report)
	}
	if *confirm != "" && len(culprits) != 0 {
		confirmed, err := confirmCulprits(ctx, *repository, *base, members, report.RedModules, culprits, *confirm)
		if err != nil {
			return err
		}
		culprits = confirmed
		for _, member := range culprits {
			if strings.HasPrefix(reasons[member], "ident:") {
				continue
			}
			reasons[member] = "bisect"
		}
	}
	sort.Strings(culprits)
	for _, member := range culprits {
		if _, err := fmt.Fprintf(output, "%s\t%s\n", member, reasons[member]); err != nil {
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
	for _, member := range members {
		mergeBase, err := git(ctx, repository, "merge-base", base, member.Head)
		if err != nil {
			return nil, fmt.Errorf("find merge base for %q: %w", member.ID, err)
		}
		diff, err := git(ctx, repository, "diff", "-U0", "--no-color", mergeBase, member.Head)
		if err != nil {
			return nil, fmt.Errorf("read diff for %q: %w", member.ID, err)
		}
		evidence[member.ID] = blame.IndexDiff(diff)
	}
	return evidence, nil
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
		return blame.BuildResult{Green: false, Output: string(output)}, nil
	}
	return blame.BuildResult{Green: true, Output: string(output)}, nil
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
