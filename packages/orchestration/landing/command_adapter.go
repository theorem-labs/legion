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
	"sync"

	"github.com/sjawhar/legion/orchestration/landing/blame"
)

// CommandLedger runs the ledger CLI. Command is its argv before the command,
// for example ["ledger", "--dsn-file", "/path/to/dsn"]. Check, Land and
// ReturnMember write their request as JSON to a fresh 0600 file and run check,
// land or round with --request-file; the ledger decodes it with unknown fields
// refused, so a renamed field fails rather than being dropped. Contracts runs
// contract list. StatusCommand and MigrationStateCommand are configured
// commands that receive LAND_* variables.
type CommandLedger struct {
	Directory             string
	Command               []string
	StatusCommand         []string
	MigrationStateCommand []string
}

// Check runs the ledger's check. Exit 0 is a pass, exit 1 a refusal and exit 5
// an incomplete deferred check, each with the result on stdout; any other exit
// is an error.
func (l CommandLedger) Check(ctx context.Context, request CheckRequest) (CheckResult, error) {
	stdout, code, err := l.requestCommand(ctx, "check", request)
	if err != nil && code != 1 && code != 5 {
		return CheckResult{}, fmt.Errorf("run ledger check: %w", err)
	}
	var result CheckResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		return CheckResult{}, fmt.Errorf("decode ledger check: %w", err)
	}
	want := map[int]string{0: CheckPass, 1: CheckRefused, 5: CheckIncomplete}[code]
	if result.Outcome != want {
		return CheckResult{}, fmt.Errorf("ledger check exited %d with outcome %q", code, result.Outcome)
	}
	return result, nil
}

// Land runs the ledger's land with the full check's request.
func (l CommandLedger) Land(ctx context.Context, request LandRequest) error {
	if _, _, err := l.requestCommand(ctx, "land", request); err != nil {
		return fmt.Errorf("run ledger land: %w", err)
	}
	return nil
}

// ReturnMember records the member's failed landing round.
func (l CommandLedger) ReturnMember(ctx context.Context, request RoundRequest) error {
	if _, _, err := l.requestCommand(ctx, "round", request); err != nil {
		return fmt.Errorf("run ledger round: %w", err)
	}
	return nil
}

// Contracts lists the repository's registered contracts. contract list is a
// read command, so it prints JSON without --json.
func (l CommandLedger) Contracts(ctx context.Context, repository string) ([]Contract, error) {
	stdout, _, err := l.run(ctx, "contract", "list", repository)
	if err != nil {
		return nil, fmt.Errorf("run ledger contract list: %w", err)
	}
	var contracts []Contract
	if err := json.Unmarshal([]byte(stdout), &contracts); err != nil {
		return nil, fmt.Errorf("decode ledger contract list: %w", err)
	}
	return contracts, nil
}

// Escalate enqueues the item for the owner with the ledger's enqueue.
func (l CommandLedger) Escalate(ctx context.Context, repository, item, owner string) error {
	if _, _, err := l.run(ctx, "enqueue", repository, item, owner); err != nil {
		return fmt.Errorf("run ledger enqueue: %w", err)
	}
	return nil
}

func (l CommandLedger) requestCommand(ctx context.Context, command string, request any) (string, int, error) {
	body, err := json.Marshal(request)
	if err != nil {
		return "", -1, fmt.Errorf("encode %s request: %w", command, err)
	}
	file, err := os.CreateTemp("", "land-"+command+"-*.json")
	if err != nil {
		return "", -1, fmt.Errorf("create %s request file: %w", command, err)
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(body); err != nil {
		file.Close()
		return "", -1, fmt.Errorf("write %s request file: %w", command, err)
	}
	if err := file.Close(); err != nil {
		return "", -1, fmt.Errorf("write %s request file: %w", command, err)
	}
	return l.run(ctx, command, "--request-file", file.Name())
}

// run runs the ledger CLI and returns its stdout and exit code. A nonzero exit
// is an error that carries stderr.
func (l CommandLedger) run(ctx context.Context, args ...string) (string, int, error) {
	if len(l.Command) == 0 {
		return "", -1, fmt.Errorf("ledger command is empty")
	}
	argv := append(append([]string(nil), l.Command[1:]...), args...)
	process := exec.CommandContext(ctx, l.Command[0], argv...)
	process.Dir = l.Directory
	var stdout, stderr bytes.Buffer
	process.Stdout, process.Stderr = &stdout, &stderr
	if err := process.Run(); err != nil {
		code := -1
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			code = exit.ExitCode()
		}
		return stdout.String(), code, commandFailure{err: err, output: stderr.String() + stdout.String()}
	}
	return stdout.String(), 0, nil
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

// CommandBuilder archives an immutable tree into a temporary directory and runs
// the configured build command there.
type CommandBuilder struct {
	RepositoryPath string
	Command        []string
}

// Build builds a tree and removes its directory when it returns.
func (b CommandBuilder) Build(ctx context.Context, tree Tree, modules []string) (blame.BuildResult, error) {
	build, err := b.build(ctx, tree, modules)
	build.Release()
	return build.Result, err
}

// BuildKept builds the candidate tree and keeps its directory until Release.
func (b CommandBuilder) BuildKept(ctx context.Context, tree Tree) (KeptBuild, error) {
	return b.build(ctx, tree, nil)
}

func (b CommandBuilder) build(ctx context.Context, tree Tree, modules []string) (KeptBuild, error) {
	kept := KeptBuild{Release: noRelease}
	if len(b.Command) == 0 {
		return kept, fmt.Errorf("build command is empty")
	}
	archive, err := exec.CommandContext(ctx, "git", "-C", b.RepositoryPath, "archive", "--format=tar", string(tree)).Output()
	if err != nil {
		return kept, fmt.Errorf("archive candidate tree: %w", err)
	}
	directory, err := os.MkdirTemp("", "landing-build-")
	if err != nil {
		return kept, fmt.Errorf("create build directory: %w", err)
	}
	var once sync.Once
	kept.Directory = directory
	kept.Release = func() { once.Do(func() { os.RemoveAll(directory) }) }
	if err := extractCandidateTar(directory, archive); err != nil {
		kept.Release()
		return KeptBuild{Release: noRelease}, err
	}
	output, err := runLandingCommand(ctx, directory, b.Command, []string{
		"LAND_TREE=" + string(tree),
		"LAND_MODULES=" + strings.Join(modules, " "),
	})
	if err != nil {
		kept.Result = blame.BuildResult{Green: false, Output: commandOutput(err)}
		if strings.Contains(kept.Result.Output, "LAND_TARGET_ABSENT") {
			kept.Result.Absent = true
			return kept, nil
		}
		kept.Result.InfraClass = classifyInfrastructure(ctx, err)
		return kept, nil
	}
	kept.Result = blame.BuildResult{Green: true, Output: output}
	return kept, nil
}

// maxNamesEnvironment is the largest names array passed in LAND_NAMES; the
// array is always in the file that LAND_NAMES_FILE names.
const maxNamesEnvironment = 131072

// CommandHasher runs the repository's statement hasher in Directory, the
// repository's checkout. It receives LAND_TREE, LAND_GIT_DIR (the checkout's git
// directory, which holds the tree), LAND_BUILD_DIR (the
// candidate's kept build, empty for the base tree), LAND_NAMES_FILE (a JSON
// array of names) and, when the array fits in 131,072 bytes, LAND_NAMES. It
// prints name<TAB>hash<TAB>path for each name: a path of "-" means the name is
// absent, and comma-joined paths mean it is ambiguous. Exit 3 means it could not
// check the tree, and exit 75 that it could not reach a resource it needs,
// such as the index database, so the step is retried later.
type CommandHasher struct {
	Command   []string
	Directory string
}

func (h CommandHasher) Hashes(ctx context.Context, input HashInput) (map[string]StatementHash, error) {
	if len(h.Command) == 0 {
		return nil, fmt.Errorf("hasher command is empty")
	}
	namesJSON, err := json.Marshal(input.Names)
	if err != nil {
		return nil, fmt.Errorf("encode declaration names: %w", err)
	}
	file, err := os.CreateTemp("", "land-names-*.json")
	if err != nil {
		return nil, fmt.Errorf("create names file: %w", err)
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(namesJSON); err != nil {
		file.Close()
		return nil, fmt.Errorf("write names file: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("write names file: %w", err)
	}
	gitDir, err := exec.CommandContext(ctx, "git", "-C", h.Directory, "rev-parse", "--absolute-git-dir").Output()
	if err != nil {
		return nil, fmt.Errorf("read git directory: %w", err)
	}
	environment := []string{
		"LAND_TREE=" + string(input.Tree),
		"LAND_GIT_DIR=" + strings.TrimSpace(string(gitDir)),
		"LAND_BUILD_DIR=" + input.BuildDirectory,
		"LAND_NAMES_FILE=" + file.Name(),
	}
	if len(namesJSON) <= maxNamesEnvironment {
		environment = append(environment, "LAND_NAMES="+string(namesJSON))
	}
	process := exec.CommandContext(ctx, h.Command[0], h.Command[1:]...)
	process.Dir = h.Directory
	process.Env = append(os.Environ(), environment...)
	var stdout, stderr bytes.Buffer
	process.Stdout, process.Stderr = &stdout, &stderr
	if err := process.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 3 {
			return nil, HashUnableError{Output: strings.TrimSpace(stderr.String())}
		}
		if errors.As(err, &exit) && exit.ExitCode() == 75 {
			return nil, HashRetryError{Output: strings.TrimSpace(stderr.String())}
		}
		return nil, fmt.Errorf("run statement hasher: %w", commandFailure{err: err, output: stderr.String()})
	}
	return parseStatementHashes(stdout.String(), input.Names)
}

func parseStatementHashes(output string, names []string) (map[string]StatementHash, error) {
	wanted := make(map[string]struct{}, len(names))
	for _, name := range names {
		wanted[name] = struct{}{}
	}
	hashes := make(map[string]StatementHash, len(names))
	for lineNumber, line := range strings.Split(strings.TrimSuffix(output, "\n"), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 3 || fields[0] == "" || fields[2] == "" {
			return nil, fmt.Errorf("statement hasher line %d is not name, hash and path", lineNumber+1)
		}
		name, hash, path := fields[0], fields[1], fields[2]
		if _, wanted := wanted[name]; !wanted {
			return nil, fmt.Errorf("statement hasher returned undeclared name %q", name)
		}
		if _, duplicate := hashes[name]; duplicate {
			return nil, fmt.Errorf("statement hasher returned %q more than once", name)
		}
		statement := StatementHash{Hash: hash}
		if path == "-" {
			statement.Hash = ""
		} else {
			statement.Paths = strings.Split(path, ",")
			if hash == "" && len(statement.Paths) == 1 {
				return nil, fmt.Errorf("statement hasher gave %q no hash", name)
			}
		}
		hashes[name] = statement
	}
	for _, name := range names {
		if _, found := hashes[name]; !found {
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

// Publish runs the event command with the member's flags in LAND_FLAGS, a
// JSON array ("[]" when it has none).
func (e CommandEvents) Publish(ctx context.Context, event LandingEvent) error {
	flags := event.Flags
	if flags == nil {
		flags = []Flag{}
	}
	encoded, err := json.Marshal(flags)
	if err != nil {
		return fmt.Errorf("encode event flags: %w", err)
	}
	_, err = runLandingCommand(ctx, e.Directory, e.Command, []string{
		"LAND_BATCH=" + event.Batch,
		"LAND_MEMBER=" + event.Member,
		"LAND_COMMIT=" + event.Commit,
		"LAND_FLAGS=" + string(encoded),
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
