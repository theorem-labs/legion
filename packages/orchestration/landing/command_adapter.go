package landing

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/sjawhar/legion/orchestration/landing/blame"
)

// CommandLedger adapts configured commands to the ledger interface. Commands
// receive their input in LAND_* environment variables and write a CheckResult
// JSON object for Check.
type CommandLedger struct {
	Directory             string
	CheckCommand          []string
	LandCommand           []string
	ReturnCommand         []string
	StatusCommand         []string
	MigrationStateCommand []string
}

func (l CommandLedger) Check(ctx context.Context, repository, migration string, generation int64, changed []string, hashes map[string]string) (CheckResult, error) {
	output, err := runLandingCommand(ctx, l.Directory, l.CheckCommand, ledgerEnvironment(repository, migration, generation, changed, hashes))
	if err != nil {
		return CheckResult{}, fmt.Errorf("run ledger check: %w", err)
	}
	var wire struct {
		Pass   *bool  `json:"pass"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(output), &wire); err != nil || wire.Pass == nil {
		if err != nil {
			return CheckResult{}, fmt.Errorf("decode ledger check: %w", err)
		}
		return CheckResult{}, fmt.Errorf("decode ledger check: missing pass decision")
	}
	return CheckResult{Refused: !*wire.Pass, Reason: wire.Reason}, nil
}

func (l CommandLedger) Land(ctx context.Context, request LandRequest) error {
	env := ledgerEnvironment(request.Repo, request.Migration, request.Generation, request.ChangedPaths, request.Current)
	env = append(env, "LAND_COMMIT="+request.Commit)
	changedContracts, err := json.Marshal(request.ChangedContracts)
	if err != nil {
		return fmt.Errorf("encode changed contracts: %w", err)
	}
	approvedJSON, err := json.Marshal(request.ApprovedPinned)
	if err != nil {
		return fmt.Errorf("encode approvals: %w", err)
	}
	env = append(env, "LAND_CHANGED_CONTRACTS="+string(changedContracts), "LAND_APPROVED_PINNED="+string(approvedJSON))
	if _, err := runLandingCommand(ctx, l.Directory, l.LandCommand, env); err != nil {
		return fmt.Errorf("run ledger land: %w", err)
	}
	return nil
}

func (l CommandLedger) ReturnMember(ctx context.Context, repository, migration string, generation int64, reason string) error {
	env := ledgerEnvironment(repository, migration, generation, nil, nil)
	env = append(env, "LAND_REASON="+reason)
	if _, err := runLandingCommand(ctx, l.Directory, l.ReturnCommand, env); err != nil {
		return fmt.Errorf("run ledger return: %w", err)
	}
	return nil
}

func (l CommandLedger) OperationComplete(ctx context.Context, operation Operation) (bool, error) {
	if len(l.StatusCommand) == 0 {
		return false, nil
	}
	output, err := runLandingCommand(ctx, l.Directory, l.StatusCommand, []string{
		"LAND_OPERATION=" + operation.Key,
		"LAND_OPERATION_KIND=" + operation.Kind,
		"LAND_BATCH=" + operation.Batch,
		"LAND_REPOSITORY=" + operation.Repository,
		"LAND_MEMBER=" + operation.Member,
		fmt.Sprintf("LAND_GENERATION=%d", operation.Generation),
		"LAND_COMMIT=" + operation.Commit,
	})
	if err != nil {
		return false, fmt.Errorf("read operation status: %w", err)
	}
	var wire struct {
		Complete *bool `json:"complete"`
	}
	if err := json.Unmarshal([]byte(output), &wire); err != nil || wire.Complete == nil {
		if err != nil {
			return false, fmt.Errorf("decode operation status: %w", err)
		}
		return false, fmt.Errorf("decode operation status: missing complete decision")
	}
	return *wire.Complete, nil
}

func (l CommandLedger) MigrationState(ctx context.Context, repository, migration string) (string, string, error) {
	output, err := runLandingCommand(ctx, l.Directory, l.MigrationStateCommand, []string{
		"LAND_REPOSITORY=" + repository,
		"LAND_MIGRATION=" + migration,
	})
	if err != nil {
		return "", "", fmt.Errorf("read migration state: %w", err)
	}
	var wire struct {
		State        string `json:"state"`
		LandedCommit string `json:"landedCommit"`
	}
	if err := json.Unmarshal([]byte(output), &wire); err != nil || wire.State == "" {
		if err != nil {
			return "", "", fmt.Errorf("decode migration state: %w", err)
		}
		return "", "", fmt.Errorf("decode migration state: missing state")
	}
	return wire.State, wire.LandedCommit, nil
}
func ledgerEnvironment(repository, migration string, generation int64, changed []string, hashes map[string]string) []string {
	changedJSON, _ := json.Marshal(changed)
	hashesJSON, _ := json.Marshal(hashes)
	return []string{
		"LAND_REPOSITORY=" + repository,
		"LAND_MIGRATION=" + migration,
		fmt.Sprintf("LAND_GENERATION=%d", generation),
		"LAND_CHANGED=" + string(changedJSON),
		"LAND_HASHES=" + string(hashesJSON),
	}
}

// CommandBuilder archives an immutable tree into a temporary directory and runs
// the configured build command there.
type CommandBuilder struct {
	RepositoryPath string
	Command        []string
}

func (b CommandBuilder) Build(ctx context.Context, tree Tree, modules []string) (blame.BuildResult, error) {
	if len(b.Command) == 0 {
		return blame.BuildResult{}, fmt.Errorf("build command is empty")
	}
	archive, err := exec.CommandContext(ctx, "git", "-C", b.RepositoryPath, "archive", "--format=tar", string(tree)).Output()
	if err != nil {
		return blame.BuildResult{}, fmt.Errorf("archive candidate tree: %w", err)
	}
	directory, err := os.MkdirTemp("", "landing-build-")
	if err != nil {
		return blame.BuildResult{}, fmt.Errorf("create build directory: %w", err)
	}
	defer os.RemoveAll(directory)
	if err := extractCandidateTar(directory, archive); err != nil {
		return blame.BuildResult{}, err
	}
	output, err := runLandingCommand(ctx, directory, b.Command, []string{
		"LAND_TREE=" + string(tree),
		"LAND_MODULES=" + strings.Join(modules, " "),
	})
	if err != nil {
		result := blame.BuildResult{Green: false, Output: commandOutput(err)}
		if strings.Contains(result.Output, "LAND_TARGET_ABSENT") {
			result.Absent = true
			return result, nil
		}
		result.InfraClass = classifyInfrastructure(ctx, err)
		return result, nil
	}
	return blame.BuildResult{Green: true, Output: output}, nil
}

// CommandHasher delegates exact statement hashing to a configured repository command.
// The command receives LAND_TREE and LAND_NAMES JSON and prints name<TAB>hash lines.
type CommandHasher struct {
	Command   []string
	Directory string
}

func (h CommandHasher) Hashes(ctx context.Context, tree Tree, names []string) (map[string]string, error) {
	namesJSON, err := json.Marshal(names)
	if err != nil {
		return nil, fmt.Errorf("encode declaration names: %w", err)
	}
	output, err := runLandingCommand(ctx, h.Directory, h.Command, []string{
		"LAND_TREE=" + string(tree),
		"LAND_NAMES=" + string(namesJSON),
	})
	if err != nil {
		return nil, fmt.Errorf("run statement hasher: %w", err)
	}
	wanted := make(map[string]struct{}, len(names))
	for _, name := range names {
		wanted[name] = struct{}{}
	}
	hashes := make(map[string]string, len(names))
	for lineNumber, line := range strings.Split(strings.TrimSuffix(output, "\n"), "\n") {
		if line == "" {
			continue
		}
		name, hash, found := strings.Cut(line, "\t")
		if !found || name == "" || hash == "" {
			return nil, fmt.Errorf("statement hasher line %d is not name and hash", lineNumber+1)
		}
		if _, wanted := wanted[name]; !wanted {
			return nil, fmt.Errorf("statement hasher returned undeclared name %q", name)
		}
		if _, duplicate := hashes[name]; duplicate {
			return nil, fmt.Errorf("statement hasher returned %q more than once", name)
		}
		hashes[name] = hash
	}
	for _, name := range names {
		if hashes[name] == "" {
			return nil, fmt.Errorf("statement hasher omitted %q", name)
		}
	}
	return hashes, nil
}

// CommandScorer runs configured score and score-commit commands.
type CommandScorer struct {
	Directory     string
	ScoreCommand  []string
	CommitCommand []string
	StatusCommand []string
}

func (s CommandScorer) Score(ctx context.Context, commit, part string) (string, error) {
	output, err := runLandingCommand(ctx, s.Directory, s.ScoreCommand, []string{
		"LAND_COMMIT=" + commit,
		"LAND_PART=" + part,
	})
	if err != nil {
		return "", fmt.Errorf("run score: %w", err)
	}
	return strings.TrimSpace(output), nil
}

func (s CommandScorer) Commit(ctx context.Context, repository, commit, path, output string) error {
	_, err := runLandingCommand(ctx, s.Directory, s.CommitCommand, []string{
		"LAND_REPOSITORY=" + repository,
		"LAND_COMMIT=" + commit,
		"LAND_SCORE_PATH=" + path,
		"LAND_SCORE_OUTPUT=" + output,
	})
	if err != nil {
		return fmt.Errorf("commit score: %w", err)
	}
	return nil
}

func (s CommandScorer) OperationComplete(ctx context.Context, operation Operation) (bool, error) {
	return commandOperationComplete(ctx, s.Directory, s.StatusCommand, operation)
}

// CommandEvents publishes configured member events after a durable merge.
type CommandEvents struct {
	Directory     string
	Command       []string
	StatusCommand []string
}

func (e CommandEvents) Publish(ctx context.Context, event LandingEvent) error {
	_, err := runLandingCommand(ctx, e.Directory, e.Command, []string{
		"LAND_BATCH=" + event.Batch,
		"LAND_MEMBER=" + event.Member,
		"LAND_COMMIT=" + event.Commit,
	})
	if err != nil {
		return fmt.Errorf("publish event: %w", err)
	}
	return nil
}

func (e CommandEvents) OperationComplete(ctx context.Context, operation Operation) (bool, error) {
	return commandOperationComplete(ctx, e.Directory, e.StatusCommand, operation)
}

func commandOperationComplete(ctx context.Context, directory string, command []string, operation Operation) (bool, error) {
	if len(command) == 0 {
		return false, nil
	}
	output, err := runLandingCommand(ctx, directory, command, []string{
		"LAND_OPERATION=" + operation.Key,
		"LAND_OPERATION_KIND=" + operation.Kind,
		"LAND_BATCH=" + operation.Batch,
		"LAND_REPOSITORY=" + operation.Repository,
		"LAND_MEMBER=" + operation.Member,
		fmt.Sprintf("LAND_GENERATION=%d", operation.Generation),
		"LAND_COMMIT=" + operation.Commit,
	})
	if err != nil {
		return false, err
	}
	var wire struct {
		Complete *bool `json:"complete"`
	}
	if err := json.Unmarshal([]byte(output), &wire); err != nil || wire.Complete == nil {
		if err != nil {
			return false, err
		}
		return false, fmt.Errorf("operation status has no complete decision")
	}
	return *wire.Complete, nil
}

func runLandingCommand(ctx context.Context, directory string, command, environment []string) (string, error) {
	if len(command) == 0 {
		return "", fmt.Errorf("command is empty")
	}
	process := exec.CommandContext(ctx, command[0], command[1:]...)
	process.Dir = directory
	process.Env = append(os.Environ(), environment...)
	output, err := process.CombinedOutput()
	if err != nil {
		return "", commandFailure{err: err, output: string(output)}
	}
	return string(output), nil
}

type commandFailure struct {
	err    error
	output string
}

func (e commandFailure) Error() string {
	return fmt.Sprintf("%v: %s", e.err, strings.TrimSpace(e.output))
}

func (e commandFailure) Unwrap() error { return e.err }

func commandOutput(err error) string {
	var failure commandFailure
	if errors.As(err, &failure) {
		return failure.output
	}
	return err.Error()
}

func classifyInfrastructure(ctx context.Context, err error) string {
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

func extractCandidateTar(directory string, archive []byte) error {
	reader := tar.NewReader(bytes.NewReader(archive))
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read candidate archive: %w", err)
		}
		path := filepath.Join(directory, header.Name)
		if !strings.HasPrefix(path, directory+string(os.PathSeparator)) {
			return fmt.Errorf("candidate archive path escapes destination")
		}
		switch header.Typeflag {
		case tar.TypeXGlobalHeader, tar.TypeXHeader:
			continue
		case tar.TypeDir:
			if err := os.MkdirAll(path, os.FileMode(header.Mode)); err != nil {
				return fmt.Errorf("create candidate directory: %w", err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return fmt.Errorf("create candidate parent: %w", err)
			}
			file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(header.Mode))
			if err != nil {
				return fmt.Errorf("create candidate file: %w", err)
			}
			_, copyErr := io.Copy(file, reader)
			closeErr := file.Close()
			if copyErr != nil {
				return fmt.Errorf("write candidate file: %w", copyErr)
			}
			if closeErr != nil {
				return fmt.Errorf("close candidate file: %w", closeErr)
			}
		case tar.TypeSymlink:
			if filepath.IsAbs(header.Linkname) {
				return fmt.Errorf("candidate archive has absolute symlink %q", header.Name)
			}
			target := filepath.Clean(filepath.Join(filepath.Dir(path), header.Linkname))
			if !strings.HasPrefix(target, directory+string(os.PathSeparator)) {
				return fmt.Errorf("candidate archive symlink escapes destination")
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return fmt.Errorf("create candidate symlink parent: %w", err)
			}
			if err := os.Symlink(header.Linkname, path); err != nil {
				return fmt.Errorf("create candidate symlink: %w", err)
			}
		default:
			return fmt.Errorf("candidate archive has unsupported entry %q", header.Name)
		}
	}
}
