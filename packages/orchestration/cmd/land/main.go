package main

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

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
		return runBatchCommand(args[0], args[1:])
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func runBatchCommand(command string, args []string) error {
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	config := flags.String("config", "", "path to the repository configuration")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *config == "" {
		return fmt.Errorf("--config is required")
	}
	if _, err := os.ReadFile(*config); err != nil {
		return fmt.Errorf("read configuration: %w", err)
	}
	return fmt.Errorf("ledger integration is not configured")
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
