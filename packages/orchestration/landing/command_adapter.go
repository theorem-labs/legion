package landing

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	Directory     string
	CheckCommand  []string
	LandCommand   []string
	ReturnCommand []string
}

func (l CommandLedger) Check(ctx context.Context, repository, migration string, generation int64, changed []string, hashes map[string]string) (CheckResult, error) {
	output, err := runLandingCommand(ctx, l.Directory, l.CheckCommand, ledgerEnvironment(repository, migration, generation, changed, hashes))
	if err != nil {
		return CheckResult{}, fmt.Errorf("run ledger check: %w", err)
	}
	if strings.TrimSpace(output) == "" {
		return CheckResult{}, nil
	}
	var result CheckResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		return CheckResult{}, fmt.Errorf("decode ledger check: %w", err)
	}
	return result, nil
}

func (l CommandLedger) Land(ctx context.Context, repository, migration string, generation int64, commit string, changed map[string]string, approved []string) error {
	env := ledgerEnvironment(repository, migration, generation, nil, changed)
	env = append(env, "LAND_COMMIT="+commit)
	approvedJSON, err := json.Marshal(approved)
	if err != nil {
		return fmt.Errorf("encode approvals: %w", err)
	}
	env = append(env, "LAND_APPROVED_PINNED="+string(approvedJSON))
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
		return blame.BuildResult{Green: false, Output: commandOutput(err)}, nil
	}
	return blame.BuildResult{Green: true, Output: output}, nil
}

// CommandHasher hashes every configured declaration's matching source lines at
// an immutable tree.
type CommandHasher struct {
	RepositoryPath string
}

func (h CommandHasher) Hashes(ctx context.Context, tree Tree, names []string) (map[string]string, error) {
	hashes := make(map[string]string, len(names))
	for _, name := range names {
		command := exec.CommandContext(ctx, "git", "-C", h.RepositoryPath, "grep", "-h", "-I", "-F", name, string(tree), "--")
		output, err := command.CombinedOutput()
		if err != nil {
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 1 {
				return nil, fmt.Errorf("read declaration %q: %w", name, err)
			}
			output = nil
		}
		digest := sha256.Sum256(output)
		hashes[name] = hex.EncodeToString(digest[:])
	}
	return hashes, nil
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
		}
	}
}
